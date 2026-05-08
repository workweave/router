package auth

import "errors"

// ErrInvalidPrefix and ErrInvalidToken are kept distinct for internal telemetry;
// HTTP handlers must collapse both to the same opaque 401 to avoid leaking which
// check failed.
var (
	ErrInvalidPrefix = errors.New("invalid bearer key prefix")
	ErrInvalidToken  = errors.New("invalid bearer key")

	// ErrActiveKeyExists is returned by APIKeyRepository.Create when the
	// installation already has an active (non-soft-deleted) key. Callers
	// should rotate (soft-delete the old key, then create a new one) rather
	// than create a second.
	ErrActiveKeyExists = errors.New("installation already has an active api key")
)
