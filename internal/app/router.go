package app

import (
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/auth"
	"github.com/karmamgmt/complydesk/internal/document"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/tenant"
)

// APIVersion is reported by /version and prefixes every route.
const APIVersion = "v1"

// Router builds the complete HTTP surface.
//
// Middleware order is deliberate and must not be reshuffled: correlation and
// logging first so every later failure is traceable, recovery before anything
// that can panic, security headers before any body is written, then tenant
// resolution, then maintenance (which needs the tenant), then authentication
// (which validates against the resolved tenant), then per-route authorisation.
func (a *App) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.Logger)
	r.Use(middleware.Recover)
	r.Use(middleware.SecurityHeaders(a.Cfg.App.IsProduction()))
	r.Use(a.corsMiddleware())
	r.Use(middleware.BodyLimit(a.Cfg.App.MaxBodyBytes))
	r.Use(middleware.Portal)
	r.Use(chimw.Timeout(a.Cfg.App.RequestTimeout))

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		httpx.Fail(w, req, httpx.ErrNotFound("The requested endpoint"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		httpx.Fail(w, req, httpx.New(httpx.CodeNotFound, "That method is not supported on this endpoint."))
	})

	if a.Cfg.Obs.MetricsEnabled {
		r.Handle(a.Cfg.Obs.MetricsPath, promhttp.Handler())
	}

	r.Route("/api/"+APIVersion, func(r chi.Router) {
		r.Use(middleware.NoCache)

		a.mountOperational(r)
		a.mountPublic(r)
		a.mountAuth(r)
		a.mountTenantScoped(r)
	})

	return r
}

// mountOperational exposes probes that must answer even during maintenance.
func (a *App) mountOperational(r chi.Router) {
	r.Get("/health", func(w http.ResponseWriter, req *http.Request) {
		httpx.OK(w, req, map[string]any{"status": "ok", "time": time.Now().UTC()})
	})

	r.Get("/ready", func(w http.ResponseWriter, req *http.Request) {
		checks := a.Health(req.Context())
		if !a.Ready(req.Context()) {
			httpx.Fail(w, req, httpx.New(httpx.CodeDependencyUnavailable,
				"One or more dependencies are unavailable.").WithData("checks", checks))
			return
		}
		httpx.OK(w, req, map[string]any{"status": "ready", "checks": checks})
	})

	r.Get("/version", func(w http.ResponseWriter, req *http.Request) {
		info := map[string]any{
			"api_version":  APIVersion,
			"environment":  a.Cfg.App.Env,
			"deprecations": []string{},
		}
		if bi, ok := debug.ReadBuildInfo(); ok {
			info["go_version"] = bi.GoVersion
			for _, setting := range bi.Settings {
				if setting.Key == "vcs.revision" {
					info["revision"] = setting.Value
				}
			}
		}
		httpx.OK(w, req, info)
	})
}

// mountPublic exposes the unauthenticated bootstrap endpoints the login page
// needs. They resolve a tenant but never require a session.
func (a *App) mountPublic(r chi.Router) {
	r.Route("/public", func(r chi.Router) {
		r.Use(a.TenantMW.Require)

		r.Get("/tenant", func(w http.ResponseWriter, req *http.Request) {
			t := appctx.TenantFrom(req.Context())
			cfg, err := a.Tenants.PublicConfig(req.Context(), t)
			if err != nil {
				httpx.Fail(w, req, tenant.ErrNotFound(err, "This workspace"))
				return
			}
			httpx.OK(w, req, cfg)
		})

		r.Get("/maintenance", func(w http.ResponseWriter, req *http.Request) {
			state, err := a.Tenants.Current(req.Context(), appctx.TenantID(req.Context()))
			if err != nil {
				httpx.Fail(w, req, httpx.ErrInternal(err))
				return
			}
			httpx.OK(w, req, state)
		})

		// Branding images are the only files served without a session, because
		// the login screen has to render before anyone signs in.
		r.Get("/assets/*", a.serveBrandingAsset)
	})

	// Signed document links. Outside the /public block above because the tenant
	// comes from the signature, not from a header a browser cannot set on a
	// direct <img> or <iframe> load.
	document.NewHandler(a.Documents, a.Tickets, a.UserRepo, a.Audit).PublicRoutes(r)

	// The generated client monogram. Outside the block above because it names
	// its own workspace in the path — the login screen for /INF/user fetches
	// Infosys's mark before any tenant header has been established.
	tenant.NewHandler(a.Tenants, a.Audit, a.Publisher, a.Documents).PublicBrandingRoutes(r)
}

// mountAuth mounts the authentication surface: public endpoints under the
// tenant, and session-required endpoints behind the authenticator.
func (a *App) mountAuth(r chi.Router) {
	authHandler := auth.NewHandler(a.Auth, a.Limiter, a.Cfg)

	r.Route("/auth", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(a.TenantMW.Require)
			r.Use(middleware.Maintenance(a.Tenants))
			authHandler.Routes(r)
		})

		r.Group(func(r chi.Router) {
			r.Use(a.TenantMW.Require)
			r.Use(a.Authenticator.Require)
			r.Use(middleware.Maintenance(a.Tenants))
			authHandler.AuthenticatedRoutes(r)
		})
	})
}

// mountTenantScoped mounts everything that requires both a tenant and a session.
func (a *App) mountTenantScoped(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(a.TenantMW.Require)
		r.Use(a.Authenticator.Require)
		r.Use(middleware.Maintenance(a.Tenants))
		r.Use(a.Limiter.PerActor("api", a.Cfg.Rate.Rule(a.Cfg.Rate.Default)))

		a.mountDomainRoutes(r)
	})

	// Staff endpoints that must work before a client is chosen.
	a.mountStaffRoutes(r)

	// Cross-client administration. Tenant resolution is optional here because
	// staff operate above any single workspace.
	//
	// The gate is staff membership, not super admin: agents create and administer
	// clients too, and each endpoint below carries its own permission check for
	// the specific verb. Keeping the outer gate coarse and the inner checks
	// precise means an agent is refused `purge` on its own merits rather than
	// being told the whole surface does not exist.
	r.Route("/admin", func(r chi.Router) {
		r.Use(a.TenantMW.Optional)
		r.Use(a.Authenticator.Require)
		r.Use(middleware.RequireStaff)

		a.mountPlatformRoutes(r)
	})
}

func (a *App) corsMiddleware() func(http.Handler) http.Handler {
	origins := a.Cfg.Tenancy.AllowedOrigins
	if len(origins) == 0 && !a.Cfg.App.IsProduction() {
		// Development convenience only; production config validation requires
		// an explicit allowlist.
		origins = []string{"*"}
	}

	return cors.Handler(cors.Options{
		AllowedOrigins: origins,
		AllowedMethods: []string{
			http.MethodGet, http.MethodPost, http.MethodPut,
			http.MethodPatch, http.MethodDelete, http.MethodOptions,
		},
		AllowedHeaders: []string{
			"Accept", "Authorization", "Content-Type", "X-Tenant-Slug", "X-Portal",
			"X-Request-Id", "X-API-Key", "Idempotency-Key", "If-None-Match",
		},
		ExposedHeaders:   []string{"X-Request-Id", "Retry-After", "Content-Disposition", "Sunset"},
		AllowCredentials: true,
		MaxAge:           300,
	})
}
