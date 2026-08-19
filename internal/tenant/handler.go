package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// Publisher writes notification events. Declared as an interface so tenant does
// not depend on the notification package's concrete type.
type Publisher interface {
	Publish(ctx context.Context, tenantID int64, eventKey, aggregateType string, aggregateID int64, payload any) error
}

// LogoUploader stores a client logo image and returns its document public id.
// Declared as an interface so tenant does not depend on the document package;
// the wiring in app passes the concrete document service in.
type LogoUploader interface {
	UploadBrandLogo(ctx context.Context, tenantSlug string, tenantID int64,
		uploaderID int64, header *multipart.FileHeader, file multipart.File) (string, error)
	// DiscardBrandLogo removes the stored document behind a replaced logo. The
	// path is a document public id recorded on the branding row.
	DiscardBrandLogo(ctx context.Context, publicID string) error
}

type Handler struct {
	svc       *Service
	auditor   *audit.Writer
	publisher Publisher
	logos     LogoUploader
}

func NewHandler(svc *Service, auditor *audit.Writer, publisher Publisher, logos LogoUploader) *Handler {
	return &Handler{svc: svc, auditor: auditor, publisher: publisher, logos: logos}
}

// TenantRoutes mounts configuration for the caller's own client.
//
// A partner reaches their client master here and nowhere else, which is what
// keeps "edit changes, but not delete or add other clients" true: this route
// group is always scoped to the caller's own client, and the cross-client
// surface lives behind RequireSuperAdmin in PlatformRoutes.
func (h *Handler) TenantRoutes(r chi.Router) {
	// "Client" is the business term; /tenant remains for compatibility.
	r.Route("/client", func(r chi.Router) {
		r.Get("/", h.getCurrent)
		r.With(middleware.RequireAnyPermission("client.update", "config.settings")).
			Patch("/", h.updateCurrent)
		r.Get("/prefix-history", h.currentPrefixHistory)
	})

	r.Route("/tenant", func(r chi.Router) {
		r.Get("/", h.getCurrent)
		r.With(middleware.RequireAnyPermission("client.update", "config.settings")).
			Patch("/", h.updateCurrent)
		r.Get("/branding", h.getBranding)
		r.With(middleware.RequirePermission("config.branding")).Put("/branding", h.updateBranding)
		r.With(middleware.RequirePermission("config.branding")).Post("/logo", h.uploadLogo)
		r.Get("/features", h.getFeatures)
		r.With(middleware.RequirePermission("config.feature")).Put("/features", h.updateFeatures)
		r.With(middleware.RequirePermission("config.settings")).Get("/settings", h.getSettings)
		r.With(middleware.RequirePermission("config.settings")).Put("/settings", h.updateSettings)
	})
}

// PlatformRoutes mounts cross-client administration, behind RequireStaff.
//
// Client records are managed by both staff roles, so the verbs are gated on
// permissions rather than on being a super admin: an agent holds client.create,
// client.update and client.delete, and a partner holds neither. The handful of
// genuinely platform-wide operations — suspending a client, editing its feature
// flags and domains, and scheduling maintenance — stay with the super admin,
// because they affect availability rather than day-to-day support work.
func (h *Handler) PlatformRoutes(r chi.Router) {
	view := middleware.RequirePermission("client.view")
	create := middleware.RequirePermission("client.create")
	update := middleware.RequirePermission("client.update")
	remove := middleware.RequirePermission("client.delete")
	superAdmin := middleware.RequireSuperAdmin

	r.Route("/tenants", func(r chi.Router) {
		r.With(view).Get("/", h.listTenants)
		r.With(create).Post("/", h.createTenant)
		r.With(view).Get("/{id}", h.getTenant)
		r.With(update).Patch("/{id}", h.updateTenant)
		r.With(remove).Delete("/{id}", h.deleteTenant)

		// Availability, not support: taking a client offline is a super-admin act.
		r.With(superAdmin).Post("/{id}/suspend", h.suspendTenant)
		r.With(superAdmin).Post("/{id}/activate", h.activateTenant)
		r.With(superAdmin).Post("/{id}/restore", h.restoreTenant)
		// Archived clients are hidden from the normal list, so they need their
		// own way to be found again.
		r.With(view).Get("/archived", h.listArchivedTenants)

		r.With(view).Get("/{id}/branding", h.getTenantBranding)
		r.With(update).Put("/{id}/branding", h.updateTenantBranding)
		r.With(update).Post("/{id}/logo", h.uploadTenantLogo)
		r.With(view).Get("/{id}/features", h.getTenantFeatures)
		r.With(superAdmin).Put("/{id}/features", h.updateTenantFeatures)
		r.With(view).Get("/{id}/settings", h.getTenantSettings)
		r.With(update).Put("/{id}/settings", h.updateTenantSettings)

		// A domain change reroutes sign-in for the whole client.
		r.With(view).Get("/{id}/domains", h.listDomains)
		r.With(superAdmin).Post("/{id}/domains", h.addDomain)
		r.With(superAdmin).Delete("/{id}/domains/{domain}", h.removeDomain)

		r.With(view).Get("/{id}/usage", h.tenantUsage)
		// Every change to a client's ticket prefix is logged; this is the log.
		r.With(view).Get("/{id}/prefix-history", h.tenantPrefixHistory)
	})

	r.Route("/maintenance", func(r chi.Router) {
		r.Get("/", h.currentMaintenance)
		r.With(superAdmin).Get("/windows", h.listWindows)
		r.With(superAdmin).Post("/windows", h.createWindow)
		r.With(superAdmin).Patch("/windows/{id}", h.updateWindow)
		r.With(superAdmin).Delete("/windows/{id}", h.deleteWindow)
	})
}

// --- current tenant ---------------------------------------------------------

type tenantResponse struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	ClientCode string `json:"client_code"`
	// Whether the code can still be changed. False once the client has any
	// ticket, because the code is stamped into every ticket number issued.
	ClientCodeLocked bool   `json:"client_code_locked"`
	Name             string `json:"name"`
	LegalName        string `json:"legal_name,omitempty"`
	Industry         string `json:"industry,omitempty"`
	Status           string `json:"status"`
	Timezone         string `json:"timezone"`
	Locale           string `json:"locale"`
	DateFormat       string `json:"date_format"`
	TicketPrefix     string `json:"ticket_prefix"`
	ContactEmail     string `json:"contact_email,omitempty"`
	AltEmail         string `json:"alt_email,omitempty"`
	ContactPhone     string `json:"contact_phone,omitempty"`
	AltPhone         string `json:"alt_phone,omitempty"`
	Address          string `json:"address,omitempty"`
	// GST number; the column is named generically because not every
	// jurisdiction calls it GST.
	GSTNumber string `json:"gst_number,omitempty"`
	// LogoURL lets a list render the client's mark without a request per row.
	// Falls back to the generated monogram, so every client always has one.
	LogoURL string `json:"logo_url"`
	// CanDelete tells the UI whether deletion is available, so the control can
	// be disabled with a reason rather than failing on click.
	CanDelete bool `json:"can_delete"`
	// DeleteSuspendsFirst warns that archiving this client will take it offline
	// on the way out, because it is still live. The confirmation says so; the
	// request is not refused for it.
	DeleteSuspendsFirst bool    `json:"delete_suspends_first"`
	ContractStart       *string `json:"contract_start"`
	ContractEnd         *string `json:"contract_end"`
	OnboardedAt         *string `json:"onboarded_at"`
	CreatedAt           string  `json:"created_at"`
	// Per-client usage counters for the roster (§8.11). Computed in one batched
	// query for the whole page so a long client list stays cheap.
	EntityCount   int64 `json:"entity_count"`
	UserCount     int64 `json:"user_count"`
	TicketCount   int64 `json:"ticket_count"`
	StorageUsedMB int64 `json:"storage_used_mb"`
}

func toTenantResponse(t *Tenant) tenantResponse {
	out := tenantResponse{
		ID: t.PublicID, Slug: t.Slug, ClientCode: t.ClientCode.String,
		Name: t.Name, LegalName: t.LegalName.String, Industry: t.Industry.String,
		Status: t.Status, Timezone: t.Timezone, Locale: t.Locale,
		DateFormat: t.DateFormat, TicketPrefix: t.TicketPrefix,
		ContactEmail: t.ContactEmail.String, AltEmail: t.AltEmail.String,
		ContactPhone: t.ContactPhone.String, AltPhone: t.AltPhone.String,
		Address: t.Address.String, GSTNumber: t.TaxID.String,
		LogoURL: MonogramURL(t.Slug),
		// Anything not already archived can go. Whether it has to be suspended
		// on the way is a question for the confirmation, not a refusal.
		CanDelete:           t.Status != StatusArchived,
		DeleteSuspendsFirst: !CanDelete(t.Status),
		CreatedAt:           t.CreatedAt.UTC().Format(time.RFC3339),
	}
	if t.ContractStart.Valid {
		s := t.ContractStart.Time.Format("2006-01-02")
		out.ContractStart = &s
	}
	if t.ContractEnd.Valid {
		s := t.ContractEnd.Time.Format("2006-01-02")
		out.ContractEnd = &s
	}
	if t.OnboardedAt.Valid {
		s := t.OnboardedAt.Time.UTC().Format(time.RFC3339)
		out.OnboardedAt = &s
	}
	return out
}

func (h *Handler) getCurrent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	row, err := h.svc.repo.ByID(ctx, appctx.TenantID(ctx))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "This workspace"))
		return
	}
	httpx.OK(w, r, toTenantResponse(row))
}

func (h *Handler) getBranding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	b, err := h.svc.repo.Branding(ctx, appctx.TenantID(ctx))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, toPublicBranding(b))
}

type brandingRequest struct {
	PrimaryColor       string `json:"primary_color" validate:"omitempty,hexcolor"`
	SecondaryColor     string `json:"secondary_color" validate:"omitempty,hexcolor"`
	AccentColor        string `json:"accent_color" validate:"omitempty,hexcolor"`
	ShowComplyDeskLogo *bool  `json:"show_complydesk_logo"`
	CustomCSS          string `json:"custom_css" validate:"omitempty,max=20000"`
	LogoPath           string `json:"logo_path" validate:"omitempty,max=512"`
	LogoDarkPath       string `json:"logo_dark_path" validate:"omitempty,max=512"`
	FaviconPath        string `json:"favicon_path" validate:"omitempty,max=512"`
	LoginBgPath        string `json:"login_bg_path" validate:"omitempty,max=512"`
	EmailHeaderPath    string `json:"email_header_path" validate:"omitempty,max=512"`
}

func (h *Handler) updateBranding(w http.ResponseWriter, r *http.Request) {
	h.applyBranding(w, r, appctx.TenantID(r.Context()))
}

func (h *Handler) applyBranding(w http.ResponseWriter, r *http.Request, tenantID int64) {
	var req brandingRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	before, _ := h.svc.repo.Branding(ctx, tenantID)

	update := BrandingUpdate{ShowComplyDeskLogo: req.ShowComplyDeskLogo}
	assign := func(dst **string, v string) {
		if v != "" {
			val := v
			*dst = &val
		}
	}
	assign(&update.PrimaryColor, req.PrimaryColor)
	assign(&update.SecondaryColor, req.SecondaryColor)
	assign(&update.AccentColor, req.AccentColor)
	assign(&update.CustomCSS, req.CustomCSS)
	assign(&update.LogoPath, req.LogoPath)
	assign(&update.LogoDarkPath, req.LogoDarkPath)
	assign(&update.FaviconPath, req.FaviconPath)
	assign(&update.LoginBgPath, req.LoginBgPath)
	assign(&update.EmailHeaderPath, req.EmailHeaderPath)

	if err := h.svc.repo.UpdateBranding(ctx, tenantID, update); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	after, err := h.svc.repo.Branding(ctx, tenantID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		TenantID: &tenantID, Action: audit.ActionConfigChanged,
		EntityType: "branding", EntityID: &tenantID,
		Before: before, After: after,
	})
	httpx.OK(w, r, toPublicBranding(after))
}

func (h *Handler) getFeatures(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	features, err := h.svc.repo.Features(ctx, appctx.TenantID(ctx))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, features)
}

func (h *Handler) updateFeatures(w http.ResponseWriter, r *http.Request) {
	h.applyFeatures(w, r, appctx.TenantID(r.Context()))
}

func (h *Handler) applyFeatures(w http.ResponseWriter, r *http.Request, tenantID int64) {
	var req map[string]bool
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if len(req) == 0 {
		httpx.Fail(w, r, httpx.ErrField("body", "REQUIRED", "Provide at least one feature to change."))
		return
	}
	for key := range req {
		if len(key) > 64 || strings.TrimSpace(key) == "" {
			httpx.Fail(w, r, httpx.ErrField(key, "INVALID", "Feature keys must be 1 to 64 characters."))
			return
		}
	}

	ctx := r.Context()
	before, _ := h.svc.repo.Features(ctx, tenantID)

	if err := h.svc.repo.SetFeatures(ctx, tenantID, req); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	after, err := h.svc.repo.Features(ctx, tenantID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		TenantID: &tenantID, Action: audit.ActionConfigChanged,
		EntityType: "features", EntityID: &tenantID, Before: before, After: after,
	})
	httpx.OK(w, r, after)
}

func (h *Handler) getSettings(w http.ResponseWriter, r *http.Request) {
	h.readSettings(w, r, appctx.TenantID(r.Context()))
}

func (h *Handler) readSettings(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	settings, err := h.svc.repo.Settings(ctx, tenantID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	// Secrets are never returned; the UI shows a masked placeholder instead.
	for _, key := range []string{SettingSMTP, SettingSMS} {
		if _, ok := settings[key]; ok {
			settings[key] = json.RawMessage(`{"configured":true,"masked":true}`)
		}
	}
	httpx.OK(w, r, settings)
}

func (h *Handler) updateSettings(w http.ResponseWriter, r *http.Request) {
	h.writeSettings(w, r, appctx.TenantID(r.Context()))
}

func (h *Handler) writeSettings(w http.ResponseWriter, r *http.Request, tenantID int64) {
	var req map[string]json.RawMessage
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if len(req) == 0 {
		httpx.Fail(w, r, httpx.ErrField("body", "REQUIRED", "Provide at least one setting to change."))
		return
	}

	ctx := r.Context()
	var actorID *int64
	if actor := appctx.ActorFrom(ctx); actor != nil {
		id := actor.UserID
		actorID = &id
	}

	for key, raw := range req {
		if len(key) > 96 || strings.TrimSpace(key) == "" {
			httpx.Fail(w, r, httpx.ErrField(key, "INVALID", "Setting keys must be 1 to 96 characters."))
			return
		}

		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			httpx.Fail(w, r, httpx.ErrField(key, "INVALID", "This setting is not valid JSON."))
			return
		}

		isSecret := key == SettingSMTP || key == SettingSMS
		if err := h.svc.repo.SetSetting(ctx, tenantID, key, value, isSecret, actorID); err != nil {
			httpx.Fail(w, r, httpx.ErrInternal(err))
			return
		}
	}

	h.auditor.Record(ctx, audit.Entry{
		TenantID: &tenantID, Action: audit.ActionConfigChanged,
		EntityType: "settings", EntityID: &tenantID,
		After: map[string]any{"keys": keysOf(req)},
	})

	h.readSettings(w, r, tenantID)
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- platform administration ------------------------------------------------

var tenantSortable = map[string]string{
	"name": "name", "slug": "slug", "status": "status", "created_at": "created_at",
}

// withUploadedLogos replaces the generated monogram with real artwork for any
// client that has some. Done in one query for the whole page rather than per
// row, so a list of fifty clients is still two round trips.
func (h *Handler) withUploadedLogos(ctx context.Context, rows []tenantResponse, ids []int64) []tenantResponse {
	if len(ids) == 0 {
		return rows
	}
	type brandRow struct {
		PublicID string `db:"public_id"`
		LogoPath string `db:"logo_path"`
	}
	found := []brandRow{}
	query, args, err := sqlx.In(`
		SELECT t.public_id, b.logo_path
		FROM tenant_branding b JOIN tenants t ON t.id = b.tenant_id
		WHERE b.tenant_id IN (?) AND b.logo_path IS NOT NULL AND b.logo_path <> ''`, ids)
	if err == nil {
		_ = h.svc.repo.db.Primary.SelectContext(ctx, &found, h.svc.repo.db.Primary.Rebind(query), args...)
	}

	byID := map[string]string{}
	for _, row := range found {
		byID[row.PublicID] = assetURL(row.LogoPath)
	}
	for i := range rows {
		if url, ok := byID[rows[i].ID]; ok && url != "" {
			rows[i].LogoURL = url
		}
	}
	return rows
}

func (h *Handler) listTenants(w http.ResponseWriter, r *http.Request) {
	page := platform.ParsePage(r, tenantSortable, "created_at")

	rows, total, err := h.svc.repo.List(r.Context(), ListFilter{
		Query:  strings.TrimSpace(r.URL.Query().Get("q")),
		Status: platform.QueryStrings(r, "status"),
		Page:   page,
	})
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]tenantResponse, 0, len(rows))
	ids := make([]int64, 0, len(rows))
	for i := range rows {
		out = append(out, toTenantResponse(&rows[i]))
		ids = append(ids, rows[i].ID)
	}
	out = h.withUploadedLogos(r.Context(), out, ids)

	if counts, err := h.svc.repo.ListUsageCounts(r.Context(), ids); err == nil {
		for i := range out {
			if c, ok := counts[ids[i]]; ok {
				out[i].EntityCount = c.EntityCount
				out[i].UserCount = c.UserCount
				out[i].TicketCount = c.TicketCount
				// Same rule as the detail, answered from a count the list
				// already had rather than a query per row.
				out[i].ClientCodeLocked = c.TicketCount > 0
				out[i].StorageUsedMB = c.StorageUsedMB
			}
		}
	}

	httpx.List(w, r, out, platform.NewMeta(page, total))
}

type createTenantRequest struct {
	Slug          string `json:"slug" validate:"required,slug,min=2,max=64"`
	ClientCode    string `json:"client_code" validate:"omitempty,max=32,safetext"`
	Name          string `json:"name" validate:"required,notblank,max=191,safetext"`
	LegalName     string `json:"legal_name" validate:"omitempty,max=191"`
	Industry      string `json:"industry" validate:"omitempty,max=96,safetext"`
	AltEmail      string `json:"alt_email" validate:"omitempty,email,max=191"`
	AltPhone      string `json:"alt_phone" validate:"omitempty,max=32"`
	Timezone      string `json:"timezone" validate:"omitempty,max=64"`
	Locale        string `json:"locale" validate:"omitempty,max=16"`
	DateFormat    string `json:"date_format" validate:"omitempty,max=32"`
	TicketPrefix  string `json:"ticket_prefix" validate:"omitempty,max=12"`
	ContactEmail  string `json:"contact_email" validate:"omitempty,email,max=191"`
	ContactPhone  string `json:"contact_phone" validate:"omitempty,max=32"`
	Address       string `json:"address" validate:"omitempty,max=1000"`
	TaxID         string `json:"tax_id" validate:"omitempty,max=64"`
	ContractStart string `json:"contract_start" validate:"omitempty,dateonly"`
	ContractEnd   string `json:"contract_end" validate:"omitempty,dateonly"`
}

func (h *Handler) createTenant(w http.ResponseWriter, r *http.Request) {
	var req createTenantRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// Reserved slugs would shadow platform hostnames.
	switch req.Slug {
	case "www", "api", "app", "admin", "static", "cdn", "mail", "public":
		httpx.Fail(w, r, httpx.ErrField("slug", "RESERVED", "This workspace address is reserved."))
		return
	}

	ctx := r.Context()
	var createdBy *int64
	if actor := appctx.ActorFrom(ctx); actor != nil {
		id := actor.UserID
		createdBy = &id
	}

	params := CreateParams{
		Slug: req.Slug, ClientCode: req.ClientCode, Name: req.Name,
		LegalName: req.LegalName, Industry: req.Industry,
		Timezone: req.Timezone, Locale: req.Locale, DateFormat: req.DateFormat,
		TicketPrefix: req.TicketPrefix,
		ContactEmail: req.ContactEmail, AltEmail: req.AltEmail,
		ContactPhone: req.ContactPhone, AltPhone: req.AltPhone,
		Address: req.Address, TaxID: req.TaxID,
		CreatedBy: createdBy,
	}
	if d, ok := parseDate(req.ContractStart); ok {
		params.ContractStart = &d
	}
	if d, ok := parseDate(req.ContractEnd); ok {
		params.ContractEnd = &d
	}

	created, err := h.svc.repo.Create(ctx, params)
	if err != nil {
		if errors.Is(err, platform.ErrSentinelConflict) {
			switch h.svc.repo.TakenField(ctx, req.Slug, req.ClientCode) {
			case "client_code":
				httpx.Fail(w, r, httpx.ErrDuplicate("client_code",
					"Another client already uses this code."))
			default:
				httpx.Fail(w, r, httpx.ErrDuplicate("slug",
					"A workspace with this address already exists."))
			}
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		TenantID: &created.ID, Action: audit.ActionTenantCreated,
		EntityType: "tenant", EntityID: &created.ID, EntityPublicID: created.PublicID,
		After: toTenantResponse(created), CrossTenant: true,
	})
	httpx.Created(w, r, toTenantResponse(created))
}

// resolveTenant loads the tenant named in the path.
func (h *Handler) resolveTenant(r *http.Request) (*Tenant, error) {
	id := chi.URLParam(r, "id")
	if !platform.ValidULID(id) {
		return nil, httpx.ErrNotFound("That workspace")
	}
	t, err := h.svc.repo.ByPublicID(r.Context(), id)
	if err != nil {
		return nil, mapErr(err, "That workspace")
	}
	return t, nil
}

// resolveArchivedTenant finds a client that is hidden from the normal lookup.
func (h *Handler) resolveArchivedTenant(r *http.Request) (*Tenant, error) {
	id := chi.URLParam(r, "id")
	if !platform.ValidULID(id) {
		return nil, httpx.ErrNotFound("That workspace")
	}
	t, err := h.svc.repo.ByPublicIDIncludingArchived(r.Context(), id)
	if err != nil {
		return nil, mapErr(err, "That workspace")
	}
	return t, nil
}

func (h *Handler) listArchivedTenants(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.repo.Archived(r.Context())
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	out := make([]tenantResponse, 0, len(rows))
	for i := range rows {
		out = append(out, toTenantResponse(&rows[i]))
	}
	httpx.OK(w, r, map[string]any{"items": out})
}

func (h *Handler) tenantPrefixHistory(w http.ResponseWriter, r *http.Request) {
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	rows, err := h.svc.repo.PrefixHistory(r.Context(), t.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, map[string]any{"items": rows})
}

func (h *Handler) getTenant(w http.ResponseWriter, r *http.Request) {
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := toTenantResponse(t)
	// The form disables the code field from this, so the reason is visible
	// before the save rather than discovered by it.
	if used, err := h.svc.repo.HasTickets(r.Context(), t.ID); err == nil {
		out.ClientCodeLocked = used
	}
	httpx.OK(w, r, out)
}

type updateTenantRequest struct {
	ClientCode    string `json:"client_code" validate:"omitempty,max=32,safetext"`
	Name          string `json:"name" validate:"omitempty,notblank,max=191,safetext"`
	LegalName     string `json:"legal_name" validate:"omitempty,max=191"`
	Industry      string `json:"industry" validate:"omitempty,max=96,safetext"`
	AltEmail      string `json:"alt_email" validate:"omitempty,email,max=191"`
	AltPhone      string `json:"alt_phone" validate:"omitempty,max=32"`
	Timezone      string `json:"timezone" validate:"omitempty,max=64"`
	Locale        string `json:"locale" validate:"omitempty,max=16"`
	DateFormat    string `json:"date_format" validate:"omitempty,max=32"`
	TicketPrefix  string `json:"ticket_prefix" validate:"omitempty,max=12"`
	ContactEmail  string `json:"contact_email" validate:"omitempty,email,max=191"`
	ContactPhone  string `json:"contact_phone" validate:"omitempty,max=32"`
	Address       string `json:"address" validate:"omitempty,max=1000"`
	TaxID         string `json:"tax_id" validate:"omitempty,max=64"`
	ContractStart string `json:"contract_start" validate:"omitempty,dateonly"`
	ContractEnd   string `json:"contract_end" validate:"omitempty,dateonly"`
	Status        string `json:"status" validate:"omitempty,oneof=ACTIVE SUSPENDED"`
}

func (h *Handler) updateTenant(w http.ResponseWriter, r *http.Request) {
	var req updateTenantRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	update := UpdateParams{}

	// The client code may be corrected while the workspace is still empty, and
	// is locked the moment it has been used.
	//
	// It is not decoration: it is part of the portal address employees and
	// partners sign in through, and it is stamped into every ticket number
	// (`AMP001-PF-2026-000145`). Changing it after tickets exist would leave
	// those numbers referring to a code that no longer exists, so the ticket a
	// customer quotes could not be found by the code printed on it.
	//
	// Before the first ticket there is nothing to break, and an operator who
	// mistyped the code during onboarding should not have to recreate the whole
	// client to fix it.
	if req.ClientCode != "" && !strings.EqualFold(req.ClientCode, t.ClientCode.String) {
		used, err := h.svc.repo.HasTickets(ctx, t.ID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrInternal(err))
			return
		}
		if used {
			httpx.Fail(w, r, httpx.ErrField("client_code", "LOCKED",
				"This client already has tickets, so its code is locked. "+
					"The code is part of every ticket number already issued, and of the "+
					"portal address employees and partners sign in through."))
			return
		}

		code := strings.ToUpper(strings.TrimSpace(req.ClientCode))
		if taken := h.svc.repo.TakenField(ctx, "", code); taken == "client_code" {
			httpx.Fail(w, r, httpx.ErrDuplicate("client_code",
				"Another client already uses this code."))
			return
		}
		update.ClientCode = &code
	}

	assign := func(dst **string, v string) {
		if v != "" {
			val := v
			*dst = &val
		}
	}
	assign(&update.Name, req.Name)
	assign(&update.LegalName, req.LegalName)
	assign(&update.Industry, req.Industry)
	assign(&update.AltEmail, req.AltEmail)
	assign(&update.AltPhone, req.AltPhone)
	assign(&update.Timezone, req.Timezone)
	assign(&update.Locale, req.Locale)
	assign(&update.DateFormat, req.DateFormat)
	assign(&update.TicketPrefix, req.TicketPrefix)
	assign(&update.ContactEmail, req.ContactEmail)
	assign(&update.ContactPhone, req.ContactPhone)
	assign(&update.Address, req.Address)
	assign(&update.TaxID, req.TaxID)
	assign(&update.Status, req.Status)
	if d, ok := parseDate(req.ContractStart); ok {
		update.ContractStart = &d
	}
	if d, ok := parseDate(req.ContractEnd); ok {
		update.ContractEnd = &d
	}

	before := toTenantResponse(t)

	var actorID *int64
	if actor := appctx.ActorFrom(ctx); actor != nil {
		id := actor.UserID
		actorID = &id
	}

	// The ticket prefix is editable any time, but every change is recorded in
	// the client's prefix history so it is always possible to see who last
	// changed the numbering and when.
	if req.TicketPrefix != "" && req.TicketPrefix != t.TicketPrefix {
		if err := h.svc.repo.RecordPrefixChange(ctx, t.ID, t.TicketPrefix, req.TicketPrefix, actorID); err != nil {
			httpx.Fail(w, r, httpx.ErrInternal(err))
			return
		}
		h.auditor.Record(ctx, audit.Entry{
			TenantID: &t.ID, Action: audit.ActionTenantPrefixSet, EntityType: "tenant",
			EntityID: &t.ID, EntityPublicID: t.PublicID,
			Before:      map[string]any{"ticket_prefix": t.TicketPrefix},
			After:       map[string]any{"ticket_prefix": req.TicketPrefix},
			CrossTenant: true,
		})
	}

	if err := h.svc.repo.Update(ctx, t.ID, update); err != nil {
		httpx.Fail(w, r, mapErr(err, "That workspace"))
		return
	}
	h.svc.InvalidateTenant(ctx, t.Slug)

	updated, err := h.svc.repo.ByID(ctx, t.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		TenantID: &t.ID, Action: audit.ActionTenantUpdated, EntityType: "tenant",
		EntityID: &t.ID, EntityPublicID: t.PublicID,
		Before: before, After: toTenantResponse(updated), CrossTenant: true,
	})
	httpx.OK(w, r, toTenantResponse(updated))
}

// deleteTenant archives a client.
//
// Deletion is a two-step act by design. A live client must be suspended first:
// archiving signs everyone out and hides their tickets, and doing that to a
// client mid-conversation with the helpdesk — from a row in a list, behind one
// confirmation — is too easy a mistake. Suspending makes the operator see the
// effect on a live workspace before it becomes permanent.
//
// Nothing is destroyed. The row, its users, tickets and documents all survive;
// the client is marked ARCHIVED and soft-deleted so it drops out of every list,
// and `POST /{id}/restore` brings it back intact.
func (h *Handler) deleteTenant(w http.ResponseWriter, r *http.Request) {
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()

	// An active client is suspended on the way out rather than refused.
	//
	// The rule used to be "suspend it first, then delete it", which was sound in
	// intent — nobody should be signed out mid-conversation without the operator
	// seeing it happen — but wrong in practice: suspending is a super-admin act
	// and deleting is not, so an agent holding client.delete could never satisfy
	// it and the button simply failed. The safeguard belongs in the confirmation,
	// which now says what will happen, not in an error the caller cannot act on.
	//
	// The effect is unchanged: the client is suspended, its sessions end, and it
	// is archived — recoverable, never erased.
	if !CanDelete(t.Status) {
		if err := h.svc.repo.SetStatus(ctx, t.ID, StatusSuspended); err != nil {
			httpx.Fail(w, r, mapErr(err, "That workspace"))
			return
		}
		h.auditor.Record(ctx, audit.Entry{
			TenantID: &t.ID, Action: audit.ActionTenantSuspended, EntityType: "tenant",
			EntityID: &t.ID, EntityPublicID: t.PublicID,
			Before:      map[string]any{"status": t.Status},
			After:       map[string]any{"status": StatusSuspended, "reason": "archived"},
			CrossTenant: true,
		})
	}

	if err := h.svc.repo.Archive(ctx, t.ID); err != nil {
		httpx.Fail(w, r, mapErr(err, "That workspace"))
		return
	}
	h.svc.InvalidateTenant(ctx, t.Slug)

	h.auditor.Record(ctx, audit.Entry{
		TenantID: &t.ID, Action: "tenant.archived", EntityType: "tenant",
		EntityID: &t.ID, EntityPublicID: t.PublicID, Before: toTenantResponse(t), CrossTenant: true,
	})
	httpx.OK(w, r, map[string]any{
		"message": t.Name + " has been archived. An administrator can restore it.",
	})
}

// restoreTenant brings an archived client back.
//
// It returns SUSPENDED rather than ACTIVE: restoring should not silently let
// thousands of employees back in. Someone decides to activate it, deliberately,
// as a separate step.
func (h *Handler) restoreTenant(w http.ResponseWriter, r *http.Request) {
	t, err := h.resolveArchivedTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	if err := h.svc.repo.Restore(ctx, t.ID); err != nil {
		httpx.Fail(w, r, mapErr(err, "That workspace"))
		return
	}
	h.svc.InvalidateTenant(ctx, t.Slug)

	h.auditor.Record(ctx, audit.Entry{
		TenantID: &t.ID, Action: "tenant.restored", EntityType: "tenant",
		EntityID: &t.ID, EntityPublicID: t.PublicID, CrossTenant: true,
	})
	httpx.OK(w, r, map[string]any{
		"message": t.Name + " has been restored, and is suspended until you activate it.",
	})
}

func (h *Handler) suspendTenant(w http.ResponseWriter, r *http.Request) {
	h.setTenantStatus(w, r, StatusSuspended, audit.ActionTenantSuspended, "Workspace suspended.")
}

func (h *Handler) activateTenant(w http.ResponseWriter, r *http.Request) {
	h.setTenantStatus(w, r, StatusActive, "tenant.activated", "Workspace activated.")
}

func (h *Handler) setTenantStatus(w http.ResponseWriter, r *http.Request, status, action, message string) {
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	if err := h.svc.repo.SetStatus(ctx, t.ID, status); err != nil {
		httpx.Fail(w, r, mapErr(err, "That workspace"))
		return
	}
	h.svc.InvalidateTenant(ctx, t.Slug)

	h.auditor.Record(ctx, audit.Entry{
		TenantID: &t.ID, Action: action, EntityType: "tenant",
		EntityID: &t.ID, EntityPublicID: t.PublicID,
		Before: map[string]any{"status": t.Status}, After: map[string]any{"status": status},
		CrossTenant: true,
	})
	httpx.OK(w, r, map[string]any{"message": message, "status": status})
}

func (h *Handler) getTenantBranding(w http.ResponseWriter, r *http.Request) {
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	b, err := h.svc.repo.Branding(r.Context(), t.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, toPublicBranding(b))
}

func (h *Handler) updateTenantBranding(w http.ResponseWriter, r *http.Request) {
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.applyBranding(w, r, t.ID)
}

// maxLogoBytes caps a client logo. Logos are small by nature and the storage
// layer enforces a tighter per-document ceiling anyway; this only stops a
// clearly-abusive upload before the document service looks at it.
const maxLogoBytes = 5 << 20

// uploadLogo replaces the caller's own client logo. The uploaded image becomes
// a TENANT-owned document and the branding reference is swapped to it, exactly
// as a profile picture swaps on the user row.
func (h *Handler) uploadLogo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}
	h.uploadLogoFor(ctx, w, r, appctx.TenantID(ctx), actor.UserID)
}

// uploadTenantLogo replaces the logo of a client the platform admin manages.
func (h *Handler) uploadTenantLogo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.uploadLogoFor(ctx, w, r, t.ID, actor.UserID)
}

func (h *Handler) uploadLogoFor(ctx context.Context, w http.ResponseWriter, r *http.Request, tenantID int64, uploaderID int64) {
	if h.logos == nil {
		httpx.Fail(w, r, httpx.ErrInternal(fmt.Errorf("logo upload is not configured")))
		return
	}

	if err := r.ParseMultipartForm(64 << 20); err != nil {
		httpx.Fail(w, r, httpx.ErrField("logo", "INVALID", "Send the logo as a multipart upload."))
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	file, header, err := r.FormFile("logo")
	if err != nil {
		httpx.Fail(w, r, httpx.ErrField("logo", "REQUIRED", "Choose a logo image to upload."))
		return
	}
	defer func() { _ = file.Close() }()

	if !strings.HasPrefix(header.Header.Get("Content-Type"), "image/") {
		httpx.Fail(w, r, httpx.ErrField("logo", "INVALID", "A logo must be an image file."))
		return
	}
	// SVG is an image to the browser and a script host to an attacker, and the
	// asset route serves it from our own origin. It is refused here rather than
	// deeper in the upload pipeline, where the message would list the document
	// attachment formats — spreadsheets and archives — and read as nonsense for
	// a logo.
	if ext := strings.ToLower(filepath.Ext(header.Filename)); ext == ".svg" {
		httpx.Fail(w, r, httpx.ErrField("logo", "UNSUPPORTED",
			"Upload the logo as a PNG, JPG, GIF or WEBP. SVG is not accepted."))
		return
	}
	if header.Size > maxLogoBytes {
		httpx.Fail(w, r, httpx.ErrField("logo", "TOO_LARGE", "The logo must be 5 MB or smaller."))
		return
	}

	t, err := h.svc.repo.ByID(ctx, tenantID)
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That workspace"))
		return
	}

	before, _ := h.svc.repo.Branding(ctx, tenantID)
	publicID, err := h.logos.UploadBrandLogo(ctx, t.Slug, tenantID, uploaderID, header, file)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// Drop the previous logo. It belongs to the same client, so nobody else's
	// file can be swept up this way.
	if before.LogoPath.Valid && before.LogoPath.String != "" && before.LogoPath.String != publicID {
		_ = h.logos.DiscardBrandLogo(ctx, before.LogoPath.String)
	}

	update := BrandingUpdate{}
	update.LogoPath = &publicID
	if err := h.svc.repo.UpdateBranding(ctx, tenantID, update); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	after, err := h.svc.repo.Branding(ctx, tenantID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		TenantID: &tenantID, Action: audit.ActionConfigChanged,
		EntityType: "branding", EntityID: &tenantID,
		Before: before, After: after,
	})

	httpx.OK(w, r, map[string]any{
		"id": publicID, "logo_url": assetURL(publicID),
	})
}

func (h *Handler) getTenantFeatures(w http.ResponseWriter, r *http.Request) {
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	features, err := h.svc.repo.Features(r.Context(), t.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, features)
}

func (h *Handler) updateTenantFeatures(w http.ResponseWriter, r *http.Request) {
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.applyFeatures(w, r, t.ID)
}

func (h *Handler) getTenantSettings(w http.ResponseWriter, r *http.Request) {
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.readSettings(w, r, t.ID)
}

func (h *Handler) updateTenantSettings(w http.ResponseWriter, r *http.Request) {
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.writeSettings(w, r, t.ID)
}

func (h *Handler) listDomains(w http.ResponseWriter, r *http.Request) {
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	rows, err := h.svc.repo.Domains(r.Context(), t.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, rows)
}

type domainRequest struct {
	Domain    string `json:"domain" validate:"required,hostname,max=191"`
	IsPrimary bool   `json:"is_primary"`
}

func (h *Handler) addDomain(w http.ResponseWriter, r *http.Request) {
	var req domainRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	if err := h.svc.repo.AddDomain(ctx, t.ID, req.Domain, req.IsPrimary); err != nil {
		if errors.Is(err, platform.ErrSentinelConflict) {
			httpx.Fail(w, r, httpx.ErrDuplicate("domain", "This domain is already mapped to a workspace."))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	h.svc.InvalidateTenant(ctx, t.Slug)

	h.auditor.Record(ctx, audit.Entry{
		TenantID: &t.ID, Action: "tenant.domain_added", EntityType: "tenant",
		EntityID: &t.ID, After: map[string]any{"domain": req.Domain}, CrossTenant: true,
	})
	httpx.Created(w, r, map[string]any{"domain": req.Domain, "is_primary": req.IsPrimary})
}

func (h *Handler) removeDomain(w http.ResponseWriter, r *http.Request) {
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	domain := chi.URLParam(r, "domain")
	if err := h.svc.repo.RemoveDomain(ctx, t.ID, domain); err != nil {
		httpx.Fail(w, r, mapErr(err, "That domain"))
		return
	}
	h.svc.InvalidateTenant(ctx, t.Slug)

	h.auditor.Record(ctx, audit.Entry{
		TenantID: &t.ID, Action: "tenant.domain_removed", EntityType: "tenant",
		EntityID: &t.ID, Before: map[string]any{"domain": domain}, CrossTenant: true,
	})
	httpx.OK(w, r, map[string]any{"message": "Domain removed."})
}

func parseDate(v string) (time.Time, bool) {
	if strings.TrimSpace(v) == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func mapErr(err error, resource string) error {
	switch {
	case errors.Is(err, platform.ErrSentinelNotFound):
		return httpx.ErrNotFound(resource)
	case errors.Is(err, platform.ErrSentinelConflict):
		return httpx.ErrConflict("")
	default:
		return httpx.ErrInternal(err)
	}
}
