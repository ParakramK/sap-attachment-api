package storage

import (
	"path/filepath"
	"strings"
)

// ObjectPath returns the backend-neutral relative path for committed content:
// <sha-prefix>/<sha256><ext>. The hash prefix spreads files over directories;
// the extension keeps the object recognizable on disk.
func ObjectPath(sha256, filename string) string {
	return sha256[:2] + "/" + sha256 + Ext(filename)
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
