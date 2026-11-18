package storage

import "errors"

// ErrTooLarge signals an upload exceeding the configured size limit.
var ErrTooLarge = errors.New("upload exceeds maximum size")
