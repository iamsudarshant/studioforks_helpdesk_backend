package app

import (
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/analytics"
	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/catalogue"
	"github.com/karmamgmt/complydesk/internal/document"
	"github.com/karmamgmt/complydesk/internal/help"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/notification"
	"github.com/karmamgmt/complydesk/internal/org"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/tenant"
	"github.com/karmamgmt/complydesk/internal/ticket"
	"github.com/karmamgmt/complydesk/internal/user"
)

// mountDomainRoutes mounts every tenant-scoped, session-required route group.
func (a *App) mountDomainRoutes(r chi.Router) {
	tenantHandler := tenant.NewHandler(a.Tenants, a.Audit, a.Publisher, a.Documents)
	tenantHandler.TenantRoutes(r)

	orgHandler := org.NewHandler(a.OrgRepo, a.Audit)
	orgHandler.Routes(r)

	catalogueHandler := catalogue.NewHandler(a.CatalogueRepo, a.Audit)
	catalogueHandler.Routes(r)

	userHandler := user.NewHandler(a.UserRepo, a.OrgRepo, a.Auth, a.Tenants, a.Audit, a.Publisher, a.Cfg)
	userHandler.Routes(r)
	userHandler.BulkRoutes(r)

	// The Help Desk itself.
	ticketHandler := ticket.NewHandler(a.Tickets, a.UserRepo, a.Audit).
		WithPriorities(a.CatalogueRepo)
	ticketHandler.Routes(r)
	// "Which tickets did this person raise" belongs to the ticket engine, but
	// hangs off a user's URL. Mounted here so the ticket list's scope rules are
	// the ones that apply.
	ticketHandler.UserRoutes(r)

	// Attachments. The document package asks the ticket engine whether a caller
	// may see a file's ticket rather than deciding for itself, so an attachment
	// is visible exactly when the ticket it hangs off is. Avatars resolve their
	// owning user the same way.
	documentHandler := document.NewHandler(a.Documents, a.Tickets, a.UserRepo, a.Audit)
	documentHandler.Routes(r)

	// The Help module: self-service FAQ plus the Request Help ticket thread.
	helpHandler := help.NewHandler(a.HelpRepo, a.UserRepo)
	helpHandler.Routes(r)

	// Notifications: the in-app feed, per-user preferences and the wording
	// templates. How far past their own notifications a caller can see is
	// derived from their role inside the handler, not from a route gate.
	notificationHandler := notification.NewHandler(a.NotifyRepo)
	notificationHandler.Routes(r)

	// Dashboards, reports and saved views. All read-only aggregates over data
	// the caller can already reach — they reuse the ticket scope rather than
	// deriving their own, so a count can never include a row the caller cannot
	// open.
	analyticsHandler := analytics.NewHandler(analytics.NewRepository(a.DB), a.Tickets, a.OrgRepo)
	analyticsHandler.Routes(r)
	analyticsHandler.ViewRoutes(r)
}

// mountPlatformRoutes mounts cross-tenant super-admin administration.
func (a *App) mountPlatformRoutes(r chi.Router) {
	tenantHandler := tenant.NewHandler(a.Tenants, a.Audit, a.Publisher, a.Documents)
	tenantHandler.PlatformRoutes(r)

	ticketHandler := ticket.NewHandler(a.Tickets, a.UserRepo, a.Audit)
	ticketHandler.PlatformRoutes(r)
}

// serveBrandingAsset streams a tenant's logo, favicon or login background.
//
// This is the only unauthenticated file route, because the login page must
// render branded before anybody has a session. It serves a document only when
// that document's id is referenced by this tenant's branding row, so it cannot
// be used to read arbitrary stored files.
func (a *App) serveBrandingAsset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	requested := strings.Trim(chi.URLParam(r, "*"), "/")
	if requested == "" || !platform.ValidULID(requested) {
		httpx.Fail(w, r, httpx.ErrNotFound("That asset"))
		return
	}

	branding, err := a.TenantRepo.Branding(ctx, tenantID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That asset"))
		return
	}

	allowed := map[string]struct{}{}
	for _, p := range []string{
		branding.LogoPath.String, branding.LogoDarkPath.String,
		branding.FaviconPath.String, branding.LoginBgPath.String,
		branding.EmailHeaderPath.String,
	} {
		if p != "" {
			allowed[p] = struct{}{}
		}
	}
	if _, ok := allowed[requested]; !ok {
		httpx.Fail(w, r, httpx.ErrNotFound("That asset"))
		return
	}

	doc, err := a.Documents.Repo().ByPublicID(ctx, tenantID, requested)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That asset"))
		return
	}

	body, err := a.Documents.Open(ctx, tenantID, doc)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", doc.MimeType)
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Branding is neither secret nor volatile, so it may be cached publicly.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, body); err != nil {
		// The status line is already committed; nothing useful can be returned.
		return
	}
}
