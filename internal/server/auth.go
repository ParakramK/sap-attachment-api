package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/gofiber/fiber/v3"
)

// apiKeyAuth enforces the configured API keys on /v1 routes. Empty key list
// disables auth (local development); production validation requires keys.
// Digest comparison hides key material and length from timing analysis.
func apiKeyAuth(keys []string) fiber.Handler {
	digests := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		sum := sha256.Sum256([]byte(k))
		digests[hex.EncodeToString(sum[:])] = struct{}{}
	}

	return func(c fiber.Ctx) error {
		if len(digests) == 0 {
			return c.Next()
		}

		provided := c.Get("X-API-Key") //nolint:gosec // G101: header name, not a credential
		sum := sha256.Sum256([]byte(provided))
		if _, ok := digests[hex.EncodeToString(sum[:])]; !ok {
			return fiber.NewError(http.StatusUnauthorized, "missing or invalid API key")
		}

		// Idempotency keys are scoped per caller so one client cannot replay
		// another client's key.
		c.Locals("caller", hex.EncodeToString(sum[:8]))
		return c.Next()
	}
}
