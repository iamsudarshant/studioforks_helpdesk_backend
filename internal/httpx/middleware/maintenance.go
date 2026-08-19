package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/httpx"
)

// MaintenanceState describes an active window as the frontend needs to render it.
type MaintenanceState struct {
	Active       bool       `json:"active"`
	Scope        string     `json:"scope"`         // GLOBAL | TENANT
	Mode         string     `json:"mode"`          // BANNER | LOCKOUT
	Title        string     `json:"title"`         //
	Message      string     `json:"message"`       // rich text shown to users
	StartsAt     *time.Time `json:"starts_at"`     //
	EndsAt       *time.Time `json:"ends_at"`       //
	AllowedRoles []string   `json:"allowed_roles"` // roles that may still work
	ID           string     `json:"id,omitempty"`  //
}

// MaintenanceChecker is implemented by the tenant service. It is expected to be
// cheap (Redis-cached), because it runs on every request.
type MaintenanceChecker interface {
	Current(ctx context.Context, tenantID int64) (*MaintenanceState, error)
}

// alwaysReachable stays available during a lockout so the login page can render
// the maintenance notice and monitoring keeps working.
func alwaysReachable(path string) bool {
	switch {
	case strings.HasPrefix(path, "/api/v1/public/"),
		path == "/api/v1/health",
		path == "/api/v1/ready",
		path == "/api/v1/version",
		strings.HasPrefix(path, "/api/v1/docs"),
		path == "/metrics":
		return true
	}
	return false
}

// Maintenance blocks traffic while a LOCKOUT window is active. It runs before
// the auth-required handlers, but after tenant resolution so a tenant-scoped
// window can be found. Roles listed in allow_roles_json still get through, which
// is how an admin fixes things while everyone else is locked out.
func Maintenance(checker MaintenanceChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if alwaysReachable(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			ctx := r.Context()
			state, err := checker.Current(ctx, appctx.TenantID(ctx))
			if err != nil {
				// Never lock the product out because the check itself failed.
				slog.WarnContext(ctx, "maintenance check failed, allowing request", "error", err)
				next.ServeHTTP(w, r)
				return
			}
			if state == nil || !state.Active || state.Mode != "LOCKOUT" {
				next.ServeHTTP(w, r)
				return
			}

			if actor := appctx.ActorFrom(ctx); actor != nil && roleAllowed(actor, state.AllowedRoles) {
				next.ServeHTTP(w, r)
				return
			}

			httpx.Fail(w, r, httpx.New(httpx.CodeMaintenanceMode, state.Title).
				WithData("title", state.Title).
				WithData("message", state.Message).
				WithData("starts_at", state.StartsAt).
				WithData("ends_at", state.EndsAt).
				WithData("scope", state.Scope))
		})
	}
}

func roleAllowed(actor *appctx.Actor, allowed []string) bool {
	if actor.IsSuperAdmin {
		return true
	}
	for _, role := range allowed {
		if actor.HasRole(role) {
			return true
		}
	}
	return false
}
