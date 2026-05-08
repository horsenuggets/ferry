// Package server implements the ferry receiver: a tus-1.0.0 compatible
// resumable upload server with bearer-token auth, namespace scoping,
// idempotency-key support, and atomic-rename-on-completion semantics.
package server

import (
	"errors"
	"net/http"
)

// Protocol/handler errors. The HTTP layer maps these to status codes via
// statusFor.
var (
	ErrUnsupportedVersion  = errors.New("unsupported tus version")
	ErrInvalidContentType  = errors.New("invalid content type")
	ErrInvalidUploadLength = errors.New("invalid upload length")
	ErrInvalidOffset       = errors.New("invalid upload offset")
	ErrMismatchOffset      = errors.New("upload offset mismatch")
	ErrNotFound            = errors.New("upload not found")
	ErrFileLocked          = errors.New("upload locked")
	ErrSizeExceeded        = errors.New("size exceeded")
	ErrInsufficientStorage = errors.New("insufficient storage")
	ErrUnauthorized        = errors.New("unauthorized")
	ErrForbidden           = errors.New("forbidden")
	ErrInternal            = errors.New("internal error")
	// ErrChecksumMismatch is returned when the body of a PATCH didn't hash to
	// the value declared in Upload-Checksum. Maps to HTTP 460 per the tus
	// checksum extension - non-standard, but spec-mandated for tus checksum.
	ErrChecksumMismatch = errors.New("checksum mismatch")
	// ErrUnsupportedChecksumAlgo is returned when Upload-Checksum names an
	// algorithm ferry doesn't support (it accepts crc32c and sha256). 400.
	ErrUnsupportedChecksumAlgo = errors.New("unsupported checksum algorithm")
)

// statusChecksumMismatch is the tus checksum-extension status code. Not in
// http.StatusXxx because it's non-standard.
const statusChecksumMismatch = 460

// statusFor maps a protocol error to an HTTP status code.
func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrUnsupportedVersion):
		return http.StatusPreconditionFailed // 412
	case errors.Is(err, ErrInvalidContentType):
		return http.StatusUnsupportedMediaType // 415
	case errors.Is(err, ErrInvalidUploadLength), errors.Is(err, ErrInvalidOffset):
		return http.StatusBadRequest // 400
	case errors.Is(err, ErrMismatchOffset):
		return http.StatusConflict // 409
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound // 404
	case errors.Is(err, ErrFileLocked):
		return http.StatusLocked // 423
	case errors.Is(err, ErrSizeExceeded):
		return http.StatusRequestEntityTooLarge // 413
	case errors.Is(err, ErrInsufficientStorage):
		return http.StatusInsufficientStorage // 507
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized // 401
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden // 403
	case errors.Is(err, ErrChecksumMismatch):
		return statusChecksumMismatch // 460
	case errors.Is(err, ErrUnsupportedChecksumAlgo):
		return http.StatusBadRequest // 400
	default:
		return http.StatusInternalServerError // 500
	}
}
