package middleware

import (
	"net/http"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/httpx"
)

// RequirePermission gates a route on a permission key. Authorisation is always
// permission-based, never role-name based, so tenants can edit roles from the
// admin panel without a code change.
func RequirePermission(perm string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor := appctx.ActorFrom(r.Context())
			if actor == nil {
				httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
				return
			}
			if !actor.Can(perm) {
				httpx.Fail(w, r, httpx.ErrForbidden(
					"You do not have permission to perform this action."))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAnyPermission gates a route on holding at least one of the keys.
func RequireAnyPermission(perms ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor := appctx.ActorFrom(r.Context())
			if actor == nil {
				httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
				return
			}
			if !actor.CanAny(perms...) {
				httpx.Fail(w, r, httpx.ErrForbidden(
					"You do not have permission to perform this action."))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireSuperAdmin gates the cross-tenant platform routes. It is the only
// place role identity is checked directly, because super admin is a platform
// property rather than a tenant-editable role.
// RequireStaff admits ComplyDesk's own people — super admins and agents — and
// nobody else.
//
// It gates the cross-client administration surface. Like RequireSuperAdmin it
// answers NOT_FOUND rather than FORBIDDEN, because the existence of that surface
// is not something a client-side user should be able to probe for.
func RequireStaff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := appctx.ActorFrom(r.Context())
		if actor == nil {
			httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
			return
		}
		if !actor.IsStaff && !actor.IsSuperAdmin {
			httpx.Fail(w, r, httpx.ErrNotFound("The requested endpoint"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func RequireSuperAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := appctx.ActorFrom(r.Context())
		if actor == nil {
			httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
			return
		}
		if !actor.IsSuperAdmin {
			// Deliberately NOT_FOUND: the existence of the platform admin
			// surface is not disclosed to tenant users.
			httpx.Fail(w, r, httpx.ErrNotFound("The requested endpoint"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequirePortal restricts a route group to specific portals, on top of the
// portal binding already enforced at login.
func RequirePortal(allowed ...appctx.Portal) func(http.Handler) http.Handler {
	set := make(map[appctx.Portal]struct{}, len(allowed))
	for _, p := range allowed {
		set[p] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor := appctx.ActorFrom(r.Context())
			if actor == nil {
				httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
				return
			}
			if _, ok := set[actor.Portal]; !ok {
				httpx.Fail(w, r, httpx.New(httpx.CodePortalMismatch,
					"This area is not available from your portal."))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireWritable rejects mutations from read-only groups. Authenticate already
// applies this globally; mount it explicitly on any route reachable outside the
// standard chain.
func RequireWritable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if actor := appctx.ActorFrom(r.Context()); actor != nil && actor.ReadOnly() {
			httpx.Fail(w, r, httpx.ErrReadOnly())
			return
		}
		next.ServeHTTP(w, r)
	})
}
