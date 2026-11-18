// Package server wires the Fiber application: middleware chain, JSON error
// envelope, and health endpoints. Feature routes are added by later
// milestones.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/helmet"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/requestid"

	"github.com/parakram/sap-attachment-api/internal/config"
)

// New builds the Fiber app. ready is checked by /readyz; callers own the
// instance and its lifecycle.
func New(cfg *config.Config, log *slog.Logger, ready func(context.Context) error) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      "sap-attachment-api",
		ErrorHandler: errorHandler(log),
	})

	app.Use(recover.New())
	app.Use(requestid.New())
	app.Use(helmet.New())
	app.Use(requestLogger(log))

	app.Get("/healthz", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	app.Get("/readyz", func(c fiber.Ctx) error {
		if err := ready(c.Context()); err != nil {
			return c.Status(http.StatusServiceUnavailable).
				JSON(fiber.Map{"status": "unavailable", "detail": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})

	return app
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
