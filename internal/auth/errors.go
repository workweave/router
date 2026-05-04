package auth

import "errors"

// ErrInvalidPrefix and ErrInvalidToken are kept distinct for internal telemetry;
// HTTP handlers must collapse both to the same opaque 401 to avoid leaking which
// check failed.
var (
	ErrInvalidPrefix = errors.New("invalid bearer key prefix")
	ErrInvalidToken  = errors.New("invalid bearer key")
)
