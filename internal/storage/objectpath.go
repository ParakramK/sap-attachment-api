package storage

import (
	"path/filepath"
	"strings"
)

// ObjectPath returns the backend-neutral relative path for committed content:
// <sha-prefix>/<filename>. The hash prefix spreads files over directories and
// disambiguates an idempotency key reused with different content; the file name
// is the idempotency key plus the original extension (bounded, Windows-safe).
func ObjectPath(sha256, filename string) string {
	return sha256[:2] + "/" + filename
}

// Ext extracts a safe extension (".pdf") from a filename: lowercase
// alphanumeric, at most 9 characters, empty when absent or unsafe.
func Ext(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if len(ext) < 2 || len(ext) > 10 {
		return ""
	}
	for _, r := range ext[1:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return ext
}
