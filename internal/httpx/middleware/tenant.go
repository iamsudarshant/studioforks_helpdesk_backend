package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// TenantResolver is implemented by the tenant service. Declared here as an
// interface so the middleware package does not import the domain packages.
type TenantResolver interface {
	BySlug(ctx context.Context, slug string) (*appctx.Tenant, error)
	ByDomain(ctx context.Context, domain string) (*appctx.Tenant, error)
}

// TenantMiddleware resolves the tenant for every request.
//
// Resolution order (BACKEND_PROMPT §4.2):
//  1. X-Tenant-Slug header
//  2. subdomain of Host
//  3. custom domain lookup
//  4. the tid claim on an authenticated request (applied later, in Authenticate)
//
// The header is never trusted on its own: Authenticate re-checks the resolved
// tenant against the token's tid claim and rejects a mismatch.
type TenantMiddleware struct {
	resolver TenantResolver
	cfg      config.Tenancy
	isProd   bool
}

func NewTenant(resolver TenantResolver, cfg config.Tenancy, isProd bool) *TenantMiddleware {
	return &TenantMiddleware{resolver: resolver, cfg: cfg, isProd: isProd}
}

// Require resolves the tenant and rejects the request when none matches. Mount
// it on every tenant-scoped route group.
func (m *TenantMiddleware) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, err := m.resolve(r)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		if tenant.Status == "SUSPENDED" {
			httpx.Fail(w, r, httpx.New(httpx.CodeTenantSuspended,
				"This workspace has been suspended. Contact ComplyDesk support."))
			return
		}
		next.ServeHTTP(w, r.WithContext(appctx.WithTenant(r.Context(), tenant)))
	})
}

// Optional resolves the tenant when it can, but lets the request through when
// it cannot. Used by cross-tenant super-admin routes and public bootstrap.
//
// An inferred tenant is dropped rather than attached. These routes exist
// precisely for callers who have not chosen a client yet, and in production
// there is no fallback to infer one — attaching the development default would
// make dev refuse requests that production allows.
func (m *TenantMiddleware) Optional(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, err := m.resolve(r)
		if err != nil || tenant.Inferred {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(appctx.WithTenant(r.Context(), tenant)))
	})
}

func (m *TenantMiddleware) resolve(r *http.Request) (*appctx.Tenant, error) {
	ctx := r.Context()

	if slug := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Tenant-Slug"))); slug != "" {
		return m.bySlug(ctx, slug)
	}

	host := hostOnly(r.Host)
	if slug, ok := m.subdomainOf(host); ok {
		return m.bySlug(ctx, slug)
	}

	if host != "" {
		tenant, err := m.resolver.ByDomain(ctx, host)
		if err == nil {
			return tenant, nil
		}
		if !errors.Is(err, platform.ErrSentinelNotFound) {
			return nil, httpx.ErrInternal(err)
		}
	}

	// A development fallback only; config.validate() forbids it in production.
	if !m.isProd && m.cfg.DefaultSlug != "" {
		tenant, err := m.bySlug(ctx, m.cfg.DefaultSlug)
		if err != nil {
			return nil, err
		}
		tenant.Inferred = true
		return tenant, nil
	}

	return nil, httpx.New(httpx.CodeTenantNotFound,
		"No workspace matches this address. Check the URL or contact your administrator.")
}

func (m *TenantMiddleware) bySlug(ctx context.Context, slug string) (*appctx.Tenant, error) {
	tenant, err := m.resolver.BySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, platform.ErrSentinelNotFound) {
			return nil, httpx.New(httpx.CodeTenantNotFound,
				"No workspace matches this address. Check the URL or contact your administrator.")
		}
		return nil, httpx.ErrInternal(err)
	}
	return tenant, nil
}

// subdomainOf extracts "acme" from "acme.complydesk.local". Reserved labels are
// rejected so www/api/admin can never be mistaken for a workspace.
func (m *TenantMiddleware) subdomainOf(host string) (string, bool) {
	base := strings.ToLower(strings.TrimSpace(m.cfg.BaseDomain))
	if base == "" || host == "" {
		return "", false
	}
	suffix := "." + base
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" || strings.Contains(label, ".") {
		return "", false
	}
	switch label {
	case "www", "api", "app", "admin", "static", "cdn", "mail":
		return "", false
	}
	return label, true
}

func hostOnly(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	return strings.TrimSuffix(host, ".")
}
