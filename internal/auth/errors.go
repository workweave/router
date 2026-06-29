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

	// ErrInstallationNotFound is returned when an installation update matches no
	// row — a stale, soft-deleted, or cross-tenant id. Without it a zero-row
	// UPDATE looks like success, so the caller would invalidate the cache and
	// report the change as applied when nothing changed.
	ErrInstallationNotFound = errors.New("installation not found")
)
