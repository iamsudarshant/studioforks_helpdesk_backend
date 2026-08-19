// Package httpx implements the shared HTTP surface: the response envelope, the
// typed error taxonomy documented in the API contract, and request decoding.
package httpx

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is the machine-readable error code the frontend switches on. The set
// below is exhaustive and matches BACKEND_PROMPT.md §12.3 / the frontend's
// error-handling table — adding a code here means updating both prompts.
type Code string

const (
	CodeUnauthenticated         Code = "UNAUTHENTICATED"
	CodeTokenExpired            Code = "TOKEN_EXPIRED"
	CodeTokenReused             Code = "TOKEN_REUSED"
	CodeMFARequired             Code = "MFA_REQUIRED"
	CodeOTPInvalid              Code = "OTP_INVALID"
	CodePasswordChangeRequired  Code = "PASSWORD_CHANGE_REQUIRED"
	CodePortalMismatch          Code = "PORTAL_MISMATCH"
	CodeForbidden               Code = "FORBIDDEN"
	CodeOutOfScope              Code = "OUT_OF_SCOPE"
	CodeExEmployeeReadOnly      Code = "EX_EMPLOYEE_READ_ONLY"
	CodeAccessExpired           Code = "ACCESS_EXPIRED"
	CodeAccountLocked           Code = "ACCOUNT_LOCKED"
	CodeTenantNotFound          Code = "TENANT_NOT_FOUND"
	CodeTenantSuspended         Code = "TENANT_SUSPENDED"
	CodeTenantMismatch          Code = "TENANT_MISMATCH"
	CodeMaintenanceMode         Code = "MAINTENANCE_MODE"
	CodeNotFound                Code = "NOT_FOUND"
	CodeValidationFailed        Code = "VALIDATION_FAILED"
	CodeConflict                Code = "CONFLICT"
	CodeInvalidStatusTransition Code = "INVALID_STATUS_TRANSITION"
	CodeReopenWindowExpired     Code = "REOPEN_WINDOW_EXPIRED"
	CodeDuplicateEntry          Code = "DUPLICATE_ENTRY"
	CodePayloadTooLarge         Code = "PAYLOAD_TOO_LARGE"
	CodeUnsupportedMediaType    Code = "UNSUPPORTED_MEDIA_TYPE"
	CodeVirusDetected           Code = "VIRUS_DETECTED"
	CodeRateLimited             Code = "RATE_LIMITED"
	CodeDependencyUnavailable   Code = "DEPENDENCY_UNAVAILABLE"
	CodeInternalError           Code = "INTERNAL_ERROR"
)

// statusFor maps each code to its HTTP status. Keeping the mapping in one place
// prevents handlers from inventing status codes.
var statusFor = map[Code]int{
	CodeUnauthenticated:         http.StatusUnauthorized,
	CodeTokenExpired:            http.StatusUnauthorized,
	CodeTokenReused:             http.StatusUnauthorized,
	CodeMFARequired:             http.StatusUnauthorized,
	CodeOTPInvalid:              http.StatusUnauthorized,
	CodePasswordChangeRequired:  http.StatusForbidden,
	CodePortalMismatch:          http.StatusForbidden,
	CodeForbidden:               http.StatusForbidden,
	CodeOutOfScope:              http.StatusForbidden,
	CodeExEmployeeReadOnly:      http.StatusForbidden,
	CodeAccessExpired:           http.StatusForbidden,
	CodeAccountLocked:           http.StatusLocked,
	CodeTenantNotFound:          http.StatusNotFound,
	CodeTenantSuspended:         http.StatusForbidden,
	CodeTenantMismatch:          http.StatusForbidden,
	CodeMaintenanceMode:         http.StatusServiceUnavailable,
	CodeNotFound:                http.StatusNotFound,
	CodeValidationFailed:        http.StatusUnprocessableEntity,
	CodeConflict:                http.StatusConflict,
	CodeInvalidStatusTransition: http.StatusConflict,
	CodeReopenWindowExpired:     http.StatusConflict,
	CodeDuplicateEntry:          http.StatusConflict,
	CodePayloadTooLarge:         http.StatusRequestEntityTooLarge,
	CodeUnsupportedMediaType:    http.StatusUnsupportedMediaType,
	CodeVirusDetected:           http.StatusUnprocessableEntity,
	CodeRateLimited:             http.StatusTooManyRequests,
	CodeDependencyUnavailable:   http.StatusServiceUnavailable,
	CodeInternalError:           http.StatusInternalServerError,
}

// StatusFor returns the HTTP status for a code, defaulting to 500.
func StatusFor(c Code) int {
	if s, ok := statusFor[c]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// FieldError is one entry of the error.details array.
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Row     int    `json:"row,omitempty"` // populated by bulk-import validation
}

// Error is the application error type. Handlers return it; the writer turns it
// into the envelope. `cause` never reaches the client — it goes to the log,
// correlated by request_id.
type Error struct {
	Code    Code
	Message string
	Details []FieldError
	// Data carries structured context the frontend needs to render the error,
	// e.g. the maintenance window or the retry-after seconds.
	Data  map[string]any
	cause error
	// Headers are appended to the response (Retry-After, WWW-Authenticate).
	Headers map[string]string
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

func (e *Error) Status() int { return StatusFor(e.Code) }

// WithCause attaches the internal error for logging.
func (e *Error) WithCause(err error) *Error {
	e.cause = err
	return e
}

// WithData attaches structured context for the client.
func (e *Error) WithData(key string, value any) *Error {
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	e.Data[key] = value
	return e
}

// WithHeader attaches a response header.
func (e *Error) WithHeader(key, value string) *Error {
	if e.Headers == nil {
		e.Headers = map[string]string{}
	}
	e.Headers[key] = value
	return e
}

// WithDetails attaches field-level validation details.
func (e *Error) WithDetails(details ...FieldError) *Error {
	e.Details = append(e.Details, details...)
	return e
}

// New builds an application error.
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// AsAppError extracts an *Error from an error chain.
func AsAppError(err error) (*Error, bool) {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr, true
	}
	return nil, false
}

// --- constructors for the common cases -------------------------------------

func ErrUnauthenticated(msg string) *Error {
	if msg == "" {
		msg = "Authentication is required."
	}
	return New(CodeUnauthenticated, msg)
}

func ErrForbidden(msg string) *Error {
	if msg == "" {
		msg = "You do not have permission to perform this action."
	}
	return New(CodeForbidden, msg)
}

// ErrNotFound is deliberately used for cross-tenant access too: a caller must
// never be able to distinguish "exists but forbidden" from "does not exist".
func ErrNotFound(resource string) *Error {
	if resource == "" {
		resource = "The requested resource"
	}
	return New(CodeNotFound, resource+" was not found.")
}

func ErrValidation(details ...FieldError) *Error {
	return &Error{
		Code:    CodeValidationFailed,
		Message: "One or more fields are invalid.",
		Details: details,
	}
}

func ErrField(field, code, message string) *Error {
	return ErrValidation(FieldError{Field: field, Code: code, Message: message})
}

func ErrConflict(msg string) *Error {
	if msg == "" {
		msg = "The request conflicts with the current state of the resource."
	}
	return New(CodeConflict, msg)
}

func ErrDuplicate(field, msg string) *Error {
	e := New(CodeDuplicateEntry, msg)
	if field != "" {
		e.Details = []FieldError{{Field: field, Code: "DUPLICATE", Message: msg}}
	}
	return e
}

func ErrInternal(err error) *Error {
	return New(CodeInternalError, "An unexpected error occurred. Quote the request id when reporting this.").WithCause(err)
}

func ErrRateLimited(retryAfterSeconds int) *Error {
	return New(CodeRateLimited, "Too many requests. Please slow down and try again shortly.").
		WithData("retry_after", retryAfterSeconds).
		WithHeader("Retry-After", fmt.Sprintf("%d", retryAfterSeconds))
}

func ErrOutOfScope() *Error {
	return New(CodeOutOfScope, "This record is outside the entities, sites or departments assigned to you.")
}

func ErrReadOnly() *Error {
	return New(CodeExEmployeeReadOnly, "Your account has read-only access and cannot make changes.")
}
