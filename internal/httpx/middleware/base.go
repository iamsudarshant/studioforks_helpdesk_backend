// Package middleware holds the HTTP middleware chain. Order matters and is
// fixed in httpx.Router: requestid -> logger -> recover -> security -> cors ->
// bodylimit -> ratelimit -> maintenance -> tenant -> auth -> rbac/scope.
package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/httpx"
)

// RequestID assigns (or echoes) a correlation id used in logs, audit rows and
// every error response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if id == "" || len(id) > 64 {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)

		ctx := appctx.WithRequestID(r.Context(), id)
		ctx = appctx.WithClientIP(ctx, clientIP(r))
		ctx = appctx.WithUserAgent(ctx, truncate(r.UserAgent(), 255))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// responseWriter captures status and byte count for the access log.
type responseWriter struct {
	http.ResponseWriter
	status  int
	written int64
}

func (w *responseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, which the
// WebSocket upgrade and streaming downloads need.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Logger emits one structured line per request with tenant and actor context.
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w}

		next.ServeHTTP(rw, r)

		if rw.status == 0 {
			rw.status = http.StatusOK
		}

		ctx := r.Context()
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"bytes", rw.written,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", appctx.ClientIP(ctx),
			"request_id", appctx.RequestID(ctx),
		}
		if t := appctx.TenantFrom(ctx); t != nil {
			attrs = append(attrs, "tenant", t.Slug)
		}
		if a := appctx.ActorFrom(ctx); a != nil {
			attrs = append(attrs, "user_id", a.PublicID, "portal", string(a.Portal))
		}

		switch {
		case rw.status >= 500:
			slog.ErrorContext(ctx, "request", attrs...)
		case rw.status >= 400:
			slog.InfoContext(ctx, "request", attrs...)
		default:
			slog.InfoContext(ctx, "request", attrs...)
		}
	})
}

// Recover converts a panic into a 500 envelope so one bad handler cannot take
// the process down or leak a stack trace to the client.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// A dropped client connection surfaces as a panic in net/http; it
			// is not a defect and must not be logged as one.
			if err, ok := rec.(error); ok && err == http.ErrAbortHandler {
				panic(rec)
			}

			slog.ErrorContext(r.Context(), "panic recovered",
				"panic", rec,
				"stack", string(debug.Stack()),
				"path", r.URL.Path,
				"request_id", appctx.RequestID(r.Context()),
			)
			httpx.Fail(w, r, httpx.New(httpx.CodeInternalError,
				"An unexpected error occurred. Quote the request id when reporting this."))
		}()
		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders applies the baseline hardening headers. Document preview
// routes tighten CSP further on their own responses.
func SecurityHeaders(isProduction bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-site")
			h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=()")
			// The API serves JSON and file streams only; nothing should ever
			// execute in a browsing context.
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
			if isProduction {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BodyLimit caps JSON request bodies. Multipart upload routes replace this with
// their own, larger limit.
func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// NoCache prevents intermediaries from storing authenticated API responses.
func NoCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
		w.Header().Set("Pragma", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// Portal records the X-Portal header so downstream handlers and the audit
// writer know which portal a request came from. Validation against the token's
// portal claim happens in Authenticate.
func Portal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := appctx.Portal(strings.ToLower(strings.TrimSpace(r.Header.Get("X-Portal"))))
		if p.Valid() {
			next.ServeHTTP(w, r.WithContext(appctx.WithPortal(r.Context(), p)))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP prefers the left-most public address in X-Forwarded-For, falling
// back to the socket peer. Trust this only behind a proxy that overwrites the
// header — documented in DEPLOYMENT.md.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for _, part := range strings.Split(xff, ",") {
			ip := strings.TrimSpace(part)
			if ip != "" && net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" && net.ParseIP(xr) != nil {
		return xr
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
