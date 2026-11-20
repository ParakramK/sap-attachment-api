// Package server wires the Fiber application: middleware chain, JSON error
// envelope, health endpoints, and the attachment API.
package server

import (
	"context"
	_ "embed"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/helmet"
	"github.com/gofiber/fiber/v3/middleware/idempotency"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/requestid"

	"github.com/parakram/sap-attachment-api/internal/config"
	"github.com/parakram/sap-attachment-api/internal/db"
	"github.com/parakram/sap-attachment-api/internal/fetch"
	"github.com/parakram/sap-attachment-api/internal/service"
	"github.com/parakram/sap-attachment-api/internal/storage"
)

// Deps carries everything the server needs; constructed in main.
// SAPClient is consumer-defined so tests can fake it.
type SAPClient interface {
	CreateAttachment(ctx context.Context, storagePath string) error
}

type Deps struct {
	Cfg      *config.Config
	Log      *slog.Logger
	Ready    func(context.Context) error
	Repo     *db.Repo
	Store    storage.Storage
	Uploader *service.Uploader
	Fetcher  *fetch.Fetcher
	SAP      SAPClient
}

// Connection timeouts: uploads stream slowly on constrained links, so reads
// get a generous budget.
const (
	readTimeout     = 2 * time.Minute
	writeTimeout    = 2 * time.Minute
	idleTimeout     = 1 * time.Minute
	rateLimitPerMin = 120 // per-IP requests per minute, uploads included
)

//go:embed openapi.yaml
var openapiSpec []byte

// swaggerDocs is served at /docs; the UI assets come from a CDN, so this is
// a developer tool, not an offline one.
const swaggerDocs = `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>sap-attachment-api docs</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js" crossorigin></script>
  <script>
    SwaggerUIBundle({ url: "/openapi.yaml", dom_id: "#swagger" });
  </script>
</body>
</html>`

// New builds the Fiber app. Callers own the instance and its lifecycle.
func New(d Deps) *fiber.App {
	cfg := d.Cfg
	app := fiber.New(fiber.Config{
		AppName:           "sap-attachment-api",
		ErrorHandler:      errorHandler(d.Log),
		StreamRequestBody: true, // upload bodies stream instead of buffering
		BodyLimit:         int(cfg.MaxUploadBytes() + 1<<20),
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	})

	app.Use(recover.New())
	app.Use(requestid.New())
	app.Use(helmet.New())
	app.Use(limiter.New(limiter.Config{Max: rateLimitPerMin, Expiration: time.Minute}))
	app.Use(requestLogger(d.Log))

	app.Get("/healthz", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	app.Get("/readyz", func(c fiber.Ctx) error {
		if err := d.Ready(c.Context()); err != nil {
			return c.Status(http.StatusServiceUnavailable).
				JSON(fiber.Map{"status": "unavailable", "detail": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})

	// API contract + interactive docs (unauthenticated; no secrets here).
	app.Get("/openapi.yaml", func(c fiber.Ctx) error {
		return c.Type("yaml").Send(openapiSpec)
	})
	app.Get("/docs", func(c fiber.Ctx) error {
		return c.Type("html").SendString(swaggerDocs)
	})

	v1 := app.Group("/v1")
	v1.Use(apiKeyAuth(cfg.APIKeys))

	// X-Idempotency-Key is mandatory on POSTs; the guard runs before the
	// middleware so a rejected request is never cached as a replay.
	v1.Post("/attachments",
		requireIdempotencyKey,
		idempotency.New(idempotency.Config{
			KeyScope: func(c fiber.Ctx) string {
				if caller, ok := c.Locals("caller").(string); ok {
					return caller
				}
				return ""
			},
		}),
		createAttachment(d))
	v1.Get("/attachments/:id", getAttachment(d))

	return app
}

// requireIdempotencyKey rejects POSTs without a usable idempotency key.
// Validation reuses the middleware's own rule so there is one source of truth.
func requireIdempotencyKey(c fiber.Ctx) error {
	key := c.Get("X-Idempotency-Key")
	if key == "" {
		return fiber.NewError(http.StatusBadRequest, "X-Idempotency-Key header is required")
	}
	if err := idempotency.ConfigDefault.KeyHeaderValidate(key); err != nil {
		return fiber.NewError(http.StatusBadRequest, "invalid X-Idempotency-Key")
	}
	return c.Next()
}

// requestLogger emits one structured access-log entry per request, correlated
// by request ID. Replaces Fiber's default logger middleware.
func requestLogger(log *slog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()
		err := c.Next()

		// Fiber's error handler runs after the chain unwinds, so an error
		// returned here leaves the response status unset; derive it.
		status := c.Response().StatusCode()
		if err != nil && status == http.StatusOK {
			var fe *fiber.Error
			if errors.As(err, &fe) {
				status = fe.Code
			} else {
				status = http.StatusInternalServerError
			}
		}

		level := slog.LevelInfo
		switch {
		case status >= http.StatusInternalServerError:
			level = slog.LevelError
		case status >= http.StatusBadRequest:
			level = slog.LevelWarn
		}

		log.Log(c.Context(), level, "http_request",
			"request_id", requestid.FromContext(c),
			"method", c.Method(),
			"path", c.Path(),
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_ip", c.IP(),
		)
		return err
	}
}

// errorHandler renders the service-wide error envelope:
// {"code": <int>, "message": <string>, "request_id": <string>}.
// Internal error details are never sent to the client.
func errorHandler(log *slog.Logger) fiber.ErrorHandler {
	return func(c fiber.Ctx, err error) error {
		code := fiber.StatusInternalServerError
		msg := "internal server error"

		var fe *fiber.Error
		if errors.As(err, &fe) {
			code = fe.Code
			msg = fe.Message
		}

		reqID := requestid.FromContext(c)
		if code >= http.StatusInternalServerError {
			log.ErrorContext(c.Context(), "request failed",
				"request_id", reqID, "status", code, "err", err)
			msg = "internal server error"
		}

		return c.Status(code).JSON(fiber.Map{
			"code":       code,
			"message":    msg,
			"request_id": reqID,
		})
	}
}
