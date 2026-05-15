package auth

import "errors"

// ErrInvalidPrefix and ErrInvalidToken are distinct for internal telemetry.
// HTTP handlers collapse both to the same opaque 401.
var (
	ErrInvalidPrefix = errors.New("invalid bearer key prefix")
	ErrInvalidToken  = errors.New("invalid bearer key")

	// ErrActiveKeyExists is returned when the installation already has an active key.
	ErrActiveKeyExists = errors.New("installation already has an active api key")
)
