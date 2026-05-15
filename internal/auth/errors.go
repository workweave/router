package auth

import "errors"

// ErrInvalidPrefix and ErrInvalidToken are distinct for internal telemetry.
// HTTP handlers collapse both to the same opaque 401.
var (
	ErrInvalidPrefix = errors.New("invalid bearer key prefix")
	ErrInvalidToken  = errors.New("invalid bearer key")

	// ErrAPIKeyNotFound is returned when a rotate/delete targets a key that is
	// either missing or owned by a different installation.
	ErrAPIKeyNotFound = errors.New("api key not found")
)
