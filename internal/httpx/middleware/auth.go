package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/httpx"
)

// ActorLoader is implemented by the auth service: it verifies an access token
// and hydrates the full actor (roles, permissions, scopes, group state).
type ActorLoader interface {
	ActorFromAccessToken(ctx context.Context, token string) (*appctx.Actor, error)
	ActorFromAPIKey(ctx context.Context, key string) (*appctx.Actor, error)
}

// Routes that an actor with must_change_password may still reach. Everything
// else returns PASSWORD_CHANGE_REQUIRED until the password is rotated.
var passwordChangeAllowlist = map[string]bool{
	"/api/v1/auth/me":              true,
	"/api/v1/auth/change-password": true,
	"/api/v1/auth/logout":          true,
	"/api/v1/auth/logout-all":      true,
	"/api/v1/auth/refresh":         true,
}

// Authenticator turns a bearer token into an appctx.Actor and enforces the
// invariants that must hold on every authenticated request.
type Authenticator struct {
	loader ActorLoader
}

func NewAuthenticator(loader ActorLoader) *Authenticator {
	return &Authenticator{loader: loader}
}

// Require rejects unauthenticated requests.
func (a *Authenticator) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := a.authenticate(r)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		if err := a.enforce(r, actor); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(appctx.WithActor(r.Context(), actor)))
	})
}

// Optional attaches the actor when a valid token is present and otherwise lets
// the request continue anonymously. Used by public bootstrap routes that
// personalise their response when the caller happens to be signed in.
func (a *Authenticator) Optional(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := a.authenticate(r)
		if err != nil || actor == nil {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(appctx.WithActor(r.Context(), actor)))
	})
}

func (a *Authenticator) authenticate(r *http.Request) (*appctx.Actor, error) {
	ctx := r.Context()

	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		actor, err := a.loader.ActorFromAPIKey(ctx, key)
		if err != nil {
			return nil, err
		}
		return actor, nil
	}

	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if authz == "" {
		return nil, httpx.ErrUnauthenticated("Sign in to continue.")
	}
	parts := strings.SplitN(authz, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return nil, httpx.ErrUnauthenticated("Authorization header must be a Bearer token.")
	}

	actor, err := a.loader.ActorFromAccessToken(ctx, strings.TrimSpace(parts[1]))
	if err != nil {
		var appErr *httpx.Error
		if errors.As(err, &appErr) {
			return nil, appErr
		}
		return nil, httpx.ErrUnauthenticated("Your session is no longer valid. Sign in again.")
	}
	return actor, nil
}

// enforce applies the cross-cutting rules that hold for every authenticated
// request, in the order that leaks the least information.
func (a *Authenticator) enforce(r *http.Request, actor *appctx.Actor) error {
	ctx := r.Context()

	// 1. The caller must be entitled to the client this request resolved to.
	//
	//    ComplyDesk staff work across clients. A partner or employee is bound to
	//    their own workspace and nothing else — refusing that here, at the edge,
	//    is what makes cross-client isolation true without every handler having
	//    to remember it.
	if tenant := appctx.TenantFrom(ctx); tenant != nil {
		if !actor.MayAccessTenant(tenant.ID) {
			// The message never names the client: confirming it exists would
			// itself leak across the boundary being enforced.
			return httpx.New(httpx.CodeTenantMismatch,
				"Your session does not belong to this workspace.")
		}
		// Record which client the request is operating on: for staff this differs
		// from their home tenant, and repositories scope by it.
		actor.ActiveTenantID = tenant.ID
	}

	// 2. The token must have been issued for the portal being used.
	if headerPortal := appctx.PortalFrom(ctx); headerPortal != "" && actor.Portal != "" {
		if headerPortal != actor.Portal {
			return httpx.New(httpx.CodePortalMismatch,
				"Your session is not valid for this portal.")
		}
	}

	// 3. Ex-employee grace period.
	if actor.AccessExpiresAt != nil && time.Now().UTC().After(*actor.AccessExpiresAt) {
		return httpx.New(httpx.CodeAccessExpired,
			"Your access period has ended. Contact your administrator if you need access.")
	}

	// 4. Read-only groups may not mutate anything.
	if actor.ReadOnly() && isMutating(r.Method) {
		return httpx.ErrReadOnly()
	}

	// 5. A forced password change blocks everything except the change itself.
	if actor.MustChangePassword && !passwordChangeAllowlist[r.URL.Path] {
		return httpx.New(httpx.CodePasswordChangeRequired,
			"You must set a new password before continuing.")
	}

	return nil
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}
