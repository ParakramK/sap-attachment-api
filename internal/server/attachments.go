package server

import (
	"encoding/json"
	"errors"
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
}

func toResponse(a *db.Attachment, store storage.Storage) attachmentResponse {
	return attachmentResponse{
		ID:          a.ID,
		Filename:    a.Filename,
		ContentType: a.ContentType,
		SizeBytes:   a.SizeBytes,
		SHA256:      a.SHA256,
		Status:      a.Status,
		StoragePath: store.Reference(a.ObjectPath),
		CreatedAt:   a.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   a.UpdatedAt.Format(time.RFC3339),
	}
}

// createAttachment accepts either a multipart upload (a "file" part) or a
// JSON body naming a remote source:
//
//	{"source_url": "https://..."} or {"source_uri": "s3://bucket/key"}
type sourceRequest struct {
	SourceURL string `json:"source_url"`
	SourceURI string `json:"source_uri"`
	Filename  string `json:"filename"`
}

func createAttachment(d Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		mediaType, params, err := mime.ParseMediaType(c.Get(fiber.HeaderContentType))
		if err != nil {
			return fiber.NewError(http.StatusUnsupportedMediaType, "unsupported content type")
		}

		var input service.Input
		switch {
		case strings.HasPrefix(mediaType, "multipart/"):
			input, err = multipartInput(c, params["boundary"])
		case mediaType == "application/json":
			input, err = sourceInput(c, d)
		default:
			return fiber.NewError(http.StatusUnsupportedMediaType, "expected multipart/form-data or application/json")
		}
		if err != nil {
			return err // already an API error
		}

		att, deduped, err := d.Uploader.Upload(c.Context(), input)
		if err != nil {
			return uploadError(err)
		}

		status := http.StatusCreated
		if deduped {
			status = http.StatusOK
		}
		return c.Status(status).JSON(toResponse(att, d.Store))
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

func sourceInput(c fiber.Ctx, d Deps) (service.Input, error) {
	var req sourceRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return service.Input{}, fiber.NewError(http.StatusBadRequest, "invalid JSON body")
	}

	switch {
	case req.SourceURL != "" && req.SourceURI == "":
	case req.SourceURI != "" && req.SourceURL == "":
	default:
		return service.Input{}, fiber.NewError(http.StatusBadRequest, "set exactly one of source_url or source_uri")
	}

	uri := req.SourceURL
	if uri == "" {
		uri = req.SourceURI
	}

	res, err := d.Fetcher.Fetch(c.Context(), uri)
	if err != nil {
		return service.Input{}, fetchError(err)
	}
	filename := req.Filename
	if filename == "" {
		filename = res.Filename
	}
	return service.Input{Reader: res.Body, Filename: filename, Size: res.Size}, nil
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
	if errors.Is(err, storage.ErrTooLarge) {
		return fiber.NewError(http.StatusRequestEntityTooLarge, "upload exceeds maximum size")
	}
	return fiber.NewError(http.StatusInternalServerError, "upload failed")
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

func deleteAttachment(d Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		att, err := d.Repo.ByID(c.Context(), c.Params("id"))
		if err != nil {
			return fiber.NewError(http.StatusInternalServerError, "lookup failed")
		}
		if att == nil {
			return fiber.NewError(http.StatusNotFound, "attachment not found")
		}

		// Object removal first: if it fails, the row stays and DELETE can be
		// retried. Removal is idempotent for missing objects.
		if err := d.Store.Delete(c.Context(), att.ObjectPath); err != nil {
			return fiber.NewError(http.StatusInternalServerError, "failed to remove stored object")
		}
		if err := d.Repo.Delete(c.Context(), att.ID); err != nil {
			return fiber.NewError(http.StatusInternalServerError, "failed to delete attachment")
		}
		return c.SendStatus(http.StatusNoContent)
	}
}
