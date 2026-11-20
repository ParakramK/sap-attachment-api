package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/parakram/sap-attachment-api/internal/db"
	"github.com/parakram/sap-attachment-api/internal/fetch"
	"github.com/parakram/sap-attachment-api/internal/service"
	"github.com/parakram/sap-attachment-api/internal/storage"
)

// attachmentResponse is the public shape of an attachment. StoragePath is the
// absolute local disk path the SAP integration reads.
type attachmentResponse struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
	Status      string `json:"status"`
	StoragePath string `json:"storage_path"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	// SAP attachment entry id (0 when SAP integration is disabled).
	SAPAbsoluteEntry int64 `json:"sap_absolute_entry,omitempty"`
}

func toResponse(a *db.Attachment, store storage.Storage) attachmentResponse {
	// Prefer the persisted full path; older rows (before the column existed)
	// fall back to deriving it from object_path.
	ref := a.StoragePath
	if ref == "" {
		ref = store.Reference(a.ObjectPath)
	}
	return attachmentResponse{
		ID:               a.ID,
		Filename:         a.Filename,
		ContentType:      a.ContentType,
		SizeBytes:        a.SizeBytes,
		SHA256:           a.SHA256,
		Status:           a.Status,
		StoragePath:      ref,
		CreatedAt:        a.CreatedAt.Format(time.RFC3339),
		UpdatedAt:        a.UpdatedAt.Format(time.RFC3339),
		SAPAbsoluteEntry: a.SAPAbsoluteEntry,
	}
}

// createAttachment accepts either a multipart upload (a "file" part) or a
// JSON body naming one or more remote sources. Successful responses are
// always a JSON array of attachment records.
type sourceEntry struct {
	SourceURL string `json:"source_url"`
	SourceURI string `json:"source_uri"`
	Filename  string `json:"filename"`
}

type sourceRequest struct {
	SourceURL string        `json:"source_url"`
	SourceURI string        `json:"source_uri"`
	Filename  string        `json:"filename"`
	Sources   []sourceEntry `json:"sources"`
}

const maxSources = 20

func createAttachment(d Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		mediaType, params, err := mime.ParseMediaType(c.Get(fiber.HeaderContentType))
		if err != nil {
			return fiber.NewError(http.StatusUnsupportedMediaType, "unsupported content type")
		}

		var inputs []service.Input
		switch {
		case strings.HasPrefix(mediaType, "multipart/"):
			var in service.Input
			in, err = multipartInput(c, params["boundary"])
			inputs = []service.Input{in}
		case mediaType == "application/json":
			inputs, err = jsonInputs(c, d)
		default:
			return fiber.NewError(http.StatusUnsupportedMediaType, "expected multipart/form-data or application/json")
		}
		if err != nil {
			return err // already an API error
		}

		// One idempotency key per request; multiple files get -2, -3 suffixes.
		baseKey := c.Get("X-Idempotency-Key") // guaranteed by the middleware
		for i := range inputs {
			inputs[i].IdempotencyKey = baseKey
			if i > 0 {
				inputs[i].IdempotencyKey = fmt.Sprintf("%s-%d", baseKey, i+1)
			}
		}

		// All validation failures happen before any commit (see jsonInputs).
		records := make([]*db.Attachment, 0, len(inputs))
		anyDeduped := false
		for _, input := range inputs {
			att, deduped, err := d.Uploader.Upload(c.Context(), input)
			if err != nil {
				return uploadError(err)
			}
			records = append(records, att)
			anyDeduped = anyDeduped || deduped
		}

		// One Service Layer call with all new/unregistered files as lines.
		if d.SAP != nil {
			pending := make([]*db.Attachment, 0, len(records))
			for _, a := range records {
				if !a.SAPRegistered {
					pending = append(pending, a)
				}
			}
			if len(pending) > 0 {
				paths := make([]string, len(pending))
				for i, a := range pending {
					paths[i] = a.StoragePath
				}
				entries, err := d.SAP.CreateAttachments(c.Context(), paths)
				if err != nil {
					d.Log.WarnContext(c.Context(), "sap attachment failed", "err", err)
					return fiber.NewError(http.StatusBadGateway, "failed to register attachment in SAP")
				}
				for i, a := range pending {
					if err := d.Repo.MarkSAPRegistered(c.Context(), a.ID, entries[i]); err != nil {
						return fiber.NewError(http.StatusInternalServerError, "failed to record SAP attachment entry")
					}
					a.SAPRegistered = true
					a.SAPAbsoluteEntry = entries[i]
				}
			}
		}

		out := make([]attachmentResponse, len(records))
		for i, a := range records {
			out[i] = toResponse(a, d.Store)
		}
		status := http.StatusCreated
		if anyDeduped {
			status = http.StatusOK
		}
		return c.Status(status).JSON(out)
	}
}

// multipartInput streams the first file part; other parts are skipped with a
// bounded read so oversized junk parts cannot exhaust resources.
func multipartInput(c fiber.Ctx, boundary string) (service.Input, error) {
	if boundary == "" {
		return service.Input{}, fiber.NewError(http.StatusBadRequest, "multipart boundary missing")
	}

	mr := multipart.NewReader(c.BodyStream(), boundary)
	for scanned := 0; scanned < 10; scanned++ {
		part, err := mr.NextPart()
		if err != nil {
			return service.Input{}, fiber.NewError(http.StatusBadRequest, "invalid multipart body")
		}
		if fn := part.FileName(); fn != "" {
			return service.Input{Reader: part, Filename: fn, Size: -1}, nil
		}
		// Non-file part: drain bounded, then move on.
		if _, err := io.Copy(io.Discard, io.LimitReader(part, 1<<20)); err != nil {
			return service.Input{}, fiber.NewError(http.StatusBadRequest, "multipart part too large")
		}
	}
	return service.Input{}, fiber.NewError(http.StatusBadRequest, "no file part in multipart body")
}

// jsonInputs fetches and validates every source before anything is committed.
// Bodies are buffered (bounded by MAX_UPLOAD_MB each) so validation of a later
// source can abort the whole request without leaving earlier files behind.
func jsonInputs(c fiber.Ctx, d Deps) ([]service.Input, error) {
	var req sourceRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return nil, fiber.NewError(http.StatusBadRequest, "invalid JSON body")
	}

	var entries []sourceEntry
	switch {
	case len(req.Sources) > 0:
		if req.SourceURL != "" || req.SourceURI != "" {
			return nil, fiber.NewError(http.StatusBadRequest, "use sources[] or singular fields, not both")
		}
		if len(req.Sources) > maxSources {
			return nil, fiber.NewError(http.StatusBadRequest, fmt.Sprintf("at most %d sources per request", maxSources))
		}
		entries = req.Sources
	case req.SourceURL != "" && req.SourceURI == "":
		entries = []sourceEntry{{SourceURL: req.SourceURL, Filename: req.Filename}}
	case req.SourceURI != "" && req.SourceURL == "":
		entries = []sourceEntry{{SourceURI: req.SourceURI, Filename: req.Filename}}
	default:
		return nil, fiber.NewError(http.StatusBadRequest, "set exactly one of source_url or source_uri, or provide sources[]")
	}

	inputs := make([]service.Input, 0, len(entries))
	maxBytes := d.Cfg.MaxUploadBytes()
	for _, entry := range entries {
		if (entry.SourceURL == "") == (entry.SourceURI == "") {
			return nil, fiber.NewError(http.StatusBadRequest, "each source needs exactly one of source_url or source_uri")
		}
		uri := entry.SourceURL
		if uri == "" {
			uri = entry.SourceURI
		}
		res, err := d.Fetcher.Fetch(c.Context(), uri)
		if err != nil {
			return nil, fetchError(err)
		}
		data, readErr := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
		_ = res.Body.Close()
		if readErr != nil {
			return nil, fiber.NewError(http.StatusBadGateway, "failed to read remote source")
		}
		if int64(len(data)) > maxBytes {
			return nil, fiber.NewError(http.StatusRequestEntityTooLarge, "remote source exceeds size limit")
		}
		head := data
		if len(head) > 512 {
			head = head[:512]
		}
		if !service.AllowedType(http.DetectContentType(head)) {
			return nil, fiber.NewError(http.StatusUnsupportedMediaType, "only PDF and image files are accepted")
		}

		filename := entry.Filename
		if filename == "" {
			filename = res.Filename
		}
		inputs = append(inputs, service.Input{
			Reader: bytes.NewReader(data), Filename: filename, Size: int64(len(data)),
		})
	}
	return inputs, nil
}

// fetchError maps fetcher sentinels onto API errors.
func fetchError(err error) error {
	switch {
	case errors.Is(err, fetch.ErrInvalidURI):
		return fiber.NewError(http.StatusBadRequest, err.Error())
	case errors.Is(err, fetch.ErrBucketDenied), errors.Is(err, fetch.ErrDeniedNetwork):
		return fiber.NewError(http.StatusForbidden, err.Error())
	case errors.Is(err, fetch.ErrNotFound):
		return fiber.NewError(http.StatusNotFound, "remote source not found")
	case errors.Is(err, fetch.ErrFetchFailed):
		return fiber.NewError(http.StatusBadGateway, "failed to download remote source")
	default:
		return fiber.NewError(http.StatusBadGateway, "failed to download remote source")
	}
}

// uploadError maps uploader errors onto API errors.
func uploadError(err error) error {
	switch {
	case errors.Is(err, storage.ErrTooLarge):
		return fiber.NewError(http.StatusRequestEntityTooLarge, "upload exceeds maximum size")
	case errors.Is(err, service.ErrUnsupportedType):
		return fiber.NewError(http.StatusUnsupportedMediaType, "only PDF and image files are accepted")
	default:
		return fiber.NewError(http.StatusInternalServerError, "upload failed")
	}
}

func getAttachment(d Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		att, err := d.Repo.ByID(c.Context(), c.Params("id"))
		if err != nil {
			return fiber.NewError(http.StatusInternalServerError, "lookup failed")
		}
		if att == nil {
			return fiber.NewError(http.StatusNotFound, "attachment not found")
		}
		return c.JSON(toResponse(att, d.Store))
	}
}
