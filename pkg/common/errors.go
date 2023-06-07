package common

import "errors"

var (
	ErrFileHeaderMismatch = errors.New("unexpected file header")
	ErrCrcMismatch        = errors.New("crc64 mismatch")
)
