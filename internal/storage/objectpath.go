package storage

import (
	"path/filepath"
	"strings"
)

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
