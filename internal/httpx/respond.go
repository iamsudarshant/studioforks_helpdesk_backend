package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// Envelope is the single response shape for every endpoint, success or failure.
type Envelope struct {
	Success   bool          `json:"success"`
	Data      any           `json:"data"`
	Meta      any           `json:"meta"`
	Error     *ErrorPayload `json:"error"`
	RequestID string        `json:"request_id"`
}

type ErrorPayload struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details []FieldError   `json:"details,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// JSON writes a success envelope with the given status.
func JSON(w http.ResponseWriter, r *http.Request, status int, data any) {
	write(w, r, status, Envelope{
		Success:   true,
		Data:      data,
		RequestID: appctx.RequestID(r.Context()),
	})
}

// OK writes 200 with a data payload.
func OK(w http.ResponseWriter, r *http.Request, data any) {
	JSON(w, r, http.StatusOK, data)
}

// Created writes 201 with the created resource.
func Created(w http.ResponseWriter, r *http.Request, data any) {
	JSON(w, r, http.StatusCreated, data)
}

// Accepted writes 202 for work handed to a background job.
func Accepted(w http.ResponseWriter, r *http.Request, data any) {
	JSON(w, r, http.StatusAccepted, data)
}

// NoContentJSON writes a 200 with an empty object — the frontend always parses
// an envelope, so a bare 204 would break it.
func NoContentJSON(w http.ResponseWriter, r *http.Request) {
	JSON(w, r, http.StatusOK, map[string]any{})
}

// List writes 200 with a data array and the pagination meta block.
func List(w http.ResponseWriter, r *http.Request, data any, meta *platform.Meta) {
	write(w, r, http.StatusOK, Envelope{
		Success:   true,
		Data:      data,
		Meta:      meta,
		RequestID: appctx.RequestID(r.Context()),
	})
}

// Fail writes an error envelope. Non-application errors are coerced to
// INTERNAL_ERROR and their detail is logged, never returned.
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	requestID := appctx.RequestID(ctx)

	appErr, ok := AsAppError(err)
	if !ok {
		switch {
		case errors.Is(err, platform.ErrSentinelNotFound):
			appErr = ErrNotFound("")
		default:
			appErr = ErrInternal(err)
		}
	}

	logAttrs := []any{
		"code", string(appErr.Code),
		"status", appErr.Status(),
		"path", r.URL.Path,
		"method", r.Method,
		"request_id", requestID,
	}
	if tenant := appctx.TenantFrom(ctx); tenant != nil {
		logAttrs = append(logAttrs, "tenant", tenant.Slug)
	}
	if actor := appctx.ActorFrom(ctx); actor != nil {
		logAttrs = append(logAttrs, "user_id", actor.PublicID)
	}
	if cause := appErr.Unwrap(); cause != nil {
		logAttrs = append(logAttrs, "error", cause.Error())
	}

	// 5xx is a defect; 4xx is expected traffic.
	if appErr.Status() >= 500 {
		slog.ErrorContext(ctx, "request failed", logAttrs...)
	} else {
		slog.DebugContext(ctx, "request rejected", logAttrs...)
	}

	for k, v := range appErr.Headers {
		w.Header().Set(k, v)
	}

	write(w, r, appErr.Status(), Envelope{
		Success: false,
		Error: &ErrorPayload{
			Code:    appErr.Code,
			Message: appErr.Message,
			Details: appErr.Details,
			Data:    appErr.Data,
		},
		RequestID: requestID,
	})
}

func write(w http.ResponseWriter, r *http.Request, status int, env Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	if r.Method == http.MethodHead {
		return
	}
	if err := json.NewEncoder(w).Encode(env); err != nil {
		// The status line is already committed; all we can do is record it.
		slog.ErrorContext(r.Context(), "encoding response", "error", err, "path", r.URL.Path)
	}
}

// --- request decoding -------------------------------------------------------

// Decode reads a JSON body into dst, rejecting unknown fields so that typos in
// client payloads surface as validation errors instead of being ignored.
func Decode(r *http.Request, dst any) error {
	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		return New(CodeUnsupportedMediaType, "Request body must be application/json.")
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// Reject trailing content so "{}{}" cannot smuggle a second document.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return ErrField("body", "INVALID", "Request body must contain exactly one JSON object.")
	}
	return nil
}

// Bind decodes a JSON body and validates it in one step. This is the standard
// entry point for every write handler.
func Bind(r *http.Request, dst any) error {
	if err := Decode(r, dst); err != nil {
		return err
	}
	return Validate(dst)
}

func decodeError(err error) error {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	var maxBytesErr *http.MaxBytesError

	switch {
	case errors.As(err, &syntaxErr):
		return ErrField("body", "MALFORMED",
			fmt.Sprintf("Request body contains malformed JSON at position %d.", syntaxErr.Offset))

	case errors.As(err, &typeErr):
		field := typeErr.Field
		if field == "" {
			field = "body"
		}
		return ErrField(field, "TYPE_MISMATCH",
			fmt.Sprintf("Expected a value of type %s.", typeErr.Type.String()))

	case errors.As(err, &maxBytesErr):
		return New(CodePayloadTooLarge, "Request body is larger than the allowed limit.")

	case errors.Is(err, io.EOF):
		return ErrField("body", "REQUIRED", "Request body must not be empty.")

	case strings.HasPrefix(err.Error(), "json: unknown field "):
		name := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return ErrField(name, "UNKNOWN_FIELD", "This field is not accepted by this endpoint.")

	default:
		return ErrField("body", "INVALID", "Request body could not be parsed.")
	}
}
