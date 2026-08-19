package tenant

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/notification"
)

type maintenanceResponse struct {
	ID           string     `json:"id"`
	Scope        string     `json:"scope"`
	TenantID     *string    `json:"tenant_id"`
	Mode         string     `json:"mode"`
	Title        string     `json:"title"`
	Message      string     `json:"message"`
	StartsAt     time.Time  `json:"starts_at"`
	EndsAt       *time.Time `json:"ends_at"`
	IsActive     bool       `json:"is_active"`
	AllowedRoles []string   `json:"allowed_roles"`
	CreatedAt    time.Time  `json:"created_at"`
}

func toMaintenanceResponse(w MaintenanceWindow) maintenanceResponse {
	out := maintenanceResponse{
		ID: w.PublicID, Scope: w.Scope, Mode: w.Mode, Title: w.Title,
		Message: w.Message.String, StartsAt: w.StartsAt, IsActive: w.IsActive,
		CreatedAt: w.CreatedAt, AllowedRoles: []string{},
	}
	if w.EndsAt.Valid {
		out.EndsAt = &w.EndsAt.Time
	}
	if w.AllowRolesJSON.Valid {
		_ = json.Unmarshal([]byte(w.AllowRolesJSON.String), &out.AllowedRoles)
	}
	return out
}

func (h *Handler) currentMaintenance(w http.ResponseWriter, r *http.Request) {
	state, err := h.svc.Current(r.Context(), appctx.TenantID(r.Context()))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, state)
}

func (h *Handler) listWindows(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var tenantID *int64
	if raw := r.URL.Query().Get("tenant_id"); raw != "" {
		t, err := h.svc.repo.ByPublicID(ctx, raw)
		if err != nil {
			httpx.Fail(w, r, mapErr(err, "That workspace"))
			return
		}
		tenantID = &t.ID
	}

	rows, err := h.svc.repo.ListMaintenance(ctx, tenantID, r.URL.Query().Get("active_only") == "true")
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]maintenanceResponse, 0, len(rows))
	for _, row := range rows {
		item := toMaintenanceResponse(row)
		if row.TenantID.Valid {
			if t, err := h.svc.repo.ByID(ctx, row.TenantID.Int64); err == nil {
				item.TenantID = &t.PublicID
			}
		}
		out = append(out, item)
	}
	httpx.OK(w, r, out)
}

type maintenanceRequest struct {
	Scope        string   `json:"scope" validate:"required,oneof=GLOBAL TENANT"`
	TenantIDs    []string `json:"tenant_ids" validate:"omitempty,dive,len=26"`
	Mode         string   `json:"mode" validate:"required,oneof=BANNER LOCKOUT"`
	Title        string   `json:"title" validate:"required,notblank,max=191,safetext"`
	Message      string   `json:"message" validate:"omitempty,max=5000"`
	StartsAt     string   `json:"starts_at" validate:"omitempty"`
	EndsAt       string   `json:"ends_at" validate:"omitempty"`
	AllowedRoles []string `json:"allowed_roles" validate:"omitempty,dive,max=48"`
}

// createWindow schedules maintenance for every workspace or for a named set.
// Only a super admin reaches this handler; the router enforces that.
func (h *Handler) createWindow(w http.ResponseWriter, r *http.Request) {
	var req maintenanceRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if req.Scope == ScopeTenant && len(req.TenantIDs) == 0 {
		httpx.Fail(w, r, httpx.ErrField("tenant_ids", "REQUIRED",
			"Select at least one client, or choose the global scope."))
		return
	}

	ctx := r.Context()

	startsAt := time.Now().UTC()
	if req.StartsAt != "" {
		parsed, err := time.Parse(time.RFC3339, req.StartsAt)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("starts_at", "INVALID", "Provide an ISO-8601 timestamp."))
			return
		}
		startsAt = parsed.UTC()
	}

	var endsAt *time.Time
	if req.EndsAt != "" {
		parsed, err := time.Parse(time.RFC3339, req.EndsAt)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("ends_at", "INVALID", "Provide an ISO-8601 timestamp."))
			return
		}
		utc := parsed.UTC()
		if !utc.After(startsAt) {
			httpx.Fail(w, r, httpx.ErrField("ends_at", "INVALID", "The end time must be after the start time."))
			return
		}
		endsAt = &utc
	}

	var createdBy *int64
	if actor := appctx.ActorFrom(ctx); actor != nil {
		id := actor.UserID
		createdBy = &id
	}

	// Resolve target tenants up front so a bad id fails before anything is
	// written, rather than leaving half the clients in maintenance.
	targets := []*int64{nil}
	if req.Scope == ScopeTenant {
		targets = targets[:0]
		for _, publicID := range req.TenantIDs {
			t, err := h.svc.repo.ByPublicID(ctx, publicID)
			if err != nil {
				httpx.Fail(w, r, httpx.ErrField("tenant_ids", "NOT_FOUND",
					"One of the selected workspaces was not found."))
				return
			}
			id := t.ID
			targets = append(targets, &id)
		}
	}

	created := make([]maintenanceResponse, 0, len(targets))
	for _, tenantID := range targets {
		window, err := h.svc.repo.CreateMaintenance(ctx, MaintenanceParams{
			Scope: req.Scope, TenantID: tenantID, Mode: req.Mode,
			Title: req.Title, Message: req.Message,
			StartsAt: startsAt, EndsAt: endsAt,
			AllowedRoles: req.AllowedRoles, CreatedBy: createdBy,
		})
		if err != nil {
			httpx.Fail(w, r, httpx.ErrInternal(err))
			return
		}

		h.svc.InvalidateMaintenance(ctx, tenantID)
		created = append(created, toMaintenanceResponse(*window))

		h.auditor.Record(ctx, audit.Entry{
			TenantID: tenantID, Action: audit.ActionMaintenanceSet,
			EntityType: "maintenance_window", EntityID: &window.ID,
			EntityPublicID: window.PublicID,
			After:          map[string]any{"scope": req.Scope, "mode": req.Mode, "title": req.Title},
			CrossTenant:    true,
		})

		// Announce the window so users see it coming rather than hitting a wall.
		event := notification.EventMaintenanceScheduled
		if !startsAt.After(time.Now().UTC()) {
			event = notification.EventMaintenanceStarted
		}
		if tenantID != nil {
			if err := h.publisher.Publish(ctx, *tenantID, event, "maintenance_window", window.ID, map[string]any{
				"title": req.Title, "message": req.Message,
				"starts_at": startsAt, "ends_at": endsAt, "mode": req.Mode,
			}); err != nil {
				// Announcing is best-effort; the window itself is already live.
				httpx.Fail(w, r, httpx.ErrInternal(err))
				return
			}
		}
	}

	httpx.Created(w, r, created)
}

func (h *Handler) updateWindow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode         *string   `json:"mode" validate:"omitempty,oneof=BANNER LOCKOUT"`
		Title        *string   `json:"title" validate:"omitempty,max=191"`
		Message      *string   `json:"message" validate:"omitempty,max=5000"`
		StartsAt     *string   `json:"starts_at"`
		EndsAt       *string   `json:"ends_at"`
		IsActive     *bool     `json:"is_active"`
		AllowedRoles *[]string `json:"allowed_roles"`
	}
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	window, err := h.svc.repo.MaintenanceByPublicID(ctx, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That maintenance window"))
		return
	}

	update := MaintenanceUpdate{
		Mode: req.Mode, Title: req.Title, Message: req.Message,
		IsActive: req.IsActive, AllowedRoles: req.AllowedRoles,
	}
	if req.StartsAt != nil {
		parsed, err := time.Parse(time.RFC3339, *req.StartsAt)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("starts_at", "INVALID", "Provide an ISO-8601 timestamp."))
			return
		}
		utc := parsed.UTC()
		update.StartsAt = &utc
	}
	if req.EndsAt != nil {
		parsed, err := time.Parse(time.RFC3339, *req.EndsAt)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("ends_at", "INVALID", "Provide an ISO-8601 timestamp."))
			return
		}
		utc := parsed.UTC()
		update.EndsAt = &utc
	}

	if err := h.svc.repo.UpdateMaintenance(ctx, window.ID, update); err != nil {
		httpx.Fail(w, r, mapErr(err, "That maintenance window"))
		return
	}

	var tenantID *int64
	if window.TenantID.Valid {
		tenantID = &window.TenantID.Int64
	}
	h.svc.InvalidateMaintenance(ctx, tenantID)

	updated, err := h.svc.repo.MaintenanceByID(ctx, window.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		TenantID: tenantID, Action: audit.ActionMaintenanceSet,
		EntityType: "maintenance_window", EntityID: &window.ID,
		EntityPublicID: window.PublicID, CrossTenant: true,
		Before: toMaintenanceResponse(*window), After: toMaintenanceResponse(*updated),
	})
	httpx.OK(w, r, toMaintenanceResponse(*updated))
}

func (h *Handler) deleteWindow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	window, err := h.svc.repo.MaintenanceByPublicID(ctx, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That maintenance window"))
		return
	}
	if err := h.svc.repo.DeleteMaintenance(ctx, window.ID); err != nil {
		httpx.Fail(w, r, mapErr(err, "That maintenance window"))
		return
	}

	var tenantID *int64
	if window.TenantID.Valid {
		tenantID = &window.TenantID.Int64
	}
	h.svc.InvalidateMaintenance(ctx, tenantID)

	if tenantID != nil {
		_ = h.publisher.Publish(ctx, *tenantID, notification.EventMaintenanceEnded,
			"maintenance_window", window.ID, map[string]any{"title": window.Title})
	}

	h.auditor.Record(ctx, audit.Entry{
		TenantID: tenantID, Action: "tenant.maintenance_cleared",
		EntityType: "maintenance_window", EntityID: &window.ID,
		EntityPublicID: window.PublicID, CrossTenant: true,
	})
	httpx.OK(w, r, map[string]any{"message": "Maintenance window removed."})
}

func (h *Handler) tenantUsage(w http.ResponseWriter, r *http.Request) {
	t, err := h.resolveTenant(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	usage, err := h.svc.repo.Usage(r.Context(), t.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, usage)
}
