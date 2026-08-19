package platform

import "errors"

// Sentinel errors returned by repositories. Services translate these into the
// httpx error taxonomy; repositories stay free of HTTP concerns.
var (
	// ErrSentinelNotFound means "no row matched, within the caller's tenant".
	ErrSentinelNotFound = errors.New("record not found")
	// ErrSentinelConflict means the write violated a business invariant.
	ErrSentinelConflict = errors.New("record conflict")
	// ErrSentinelImmutable means the row is append-only (audit, timeline).
	ErrSentinelImmutable = errors.New("record is immutable")
)
