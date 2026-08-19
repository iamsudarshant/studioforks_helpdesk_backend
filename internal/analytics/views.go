package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// A saved view is a named set of filters someone returns to — "my breached PF
// tickets", "everything waiting on an employee".
//
// Ownership is per user, not per role: two agents with identical permissions
// keep different working sets. Sharing makes a view visible to the rest of the
// client, but never editable by them — the owner is the only one who can change
// or delete it, so a colleague cannot silently redefine a view you rely on.
type View struct {
	ID          int64          `db:"id"           json:"-"`
	PublicID    string         `db:"public_id"    json:"id"`
	TenantID    int64          `db:"tenant_id"    json:"-"`
	UserID      int64          `db:"user_id"      json:"-"`
	Name        string         `db:"name"         json:"name"`
	Resource    string         `db:"resource"     json:"resource"`
	FiltersJSON sql.NullString `db:"filters_json" json:"-"`
	ColumnsJSON sql.NullString `db:"columns_json" json:"-"`
	IsShared    bool           `db:"is_shared"    json:"is_shared"`
	IsDefault   bool           `db:"is_default"   json:"is_default"`
	CreatedAt   time.Time      `db:"created_at"   json:"created_at"`
	UpdatedAt   time.Time      `db:"updated_at"   json:"updated_at"`

	// Joined so the list can say "shared by Priya" without a second request.
	OwnerName string `db:"owner_name" json:"owner_name,omitempty"`
	IsMine    bool   `db:"is_mine"    json:"is_mine"`
}

// viewResponse flattens the stored JSON columns back into real JSON, so the
// client receives `filters: {...}` rather than a string it has to parse.
type viewResponse struct {
	View
	Filters json.RawMessage `json:"filters"`
	Columns json.RawMessage `json:"columns"`
}

func toViewResponse(v View) viewResponse {
	out := viewResponse{View: v, Filters: json.RawMessage("{}"), Columns: json.RawMessage("[]")}
	if v.FiltersJSON.Valid && json.Valid([]byte(v.FiltersJSON.String)) {
		out.Filters = json.RawMessage(v.FiltersJSON.String)
	}
	if v.ColumnsJSON.Valid && json.Valid([]byte(v.ColumnsJSON.String)) {
		out.Columns = json.RawMessage(v.ColumnsJSON.String)
	}
	return out
}

const viewColumns = `v.id, v.public_id, v.tenant_id, v.user_id, v.name, v.resource,
	v.filters_json, v.columns_json, v.is_shared, v.is_default, v.created_at, v.updated_at`

// Views lists what this user may see: their own, plus anything shared inside
// the same client.
func (r *Repository) Views(ctx context.Context, tenantID, userID int64, resource string) ([]View, error) {
	// Arguments are collected in the order the placeholders appear in the text:
	// the `is_mine` expression in the SELECT comes before anything in the WHERE.
	// Building them in one pass avoids the reordering this used to do, which
	// silently dropped the resource filter.
	args := []any{userID}

	where := []string{"v.tenant_id = ?", "(v.user_id = ? OR v.is_shared = 1)"}
	args = append(args, tenantID, userID)

	if resource = strings.TrimSpace(resource); resource != "" {
		where = append(where, "v.resource = ?")
		args = append(args, resource)
	}

	rows := []View{}
	// `is_mine` is computed here rather than inferred client-side, because the
	// client is never told other users' ids.
	q := `SELECT ` + viewColumns + `,
			CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS owner_name,
			(v.user_id = ?) AS is_mine
		FROM saved_views v
		JOIN users u ON u.id = v.user_id
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY v.is_default DESC, v.name`

	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("listing saved views: %w", err)
	}
	return rows, nil
}

type ViewParams struct {
	Name      string
	Resource  string
	Filters   json.RawMessage
	Columns   json.RawMessage
	IsShared  bool
	IsDefault bool
}

// CreateView saves a new view for one user.
func (r *Repository) CreateView(ctx context.Context, tenantID, userID int64, p ViewParams) (*View, error) {
	if p.IsDefault {
		if err := r.clearDefault(ctx, tenantID, userID, p.Resource); err != nil {
			return nil, err
		}
	}

	publicID := platform.NewULID()
	_, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO saved_views
			(public_id, tenant_id, user_id, name, resource, filters_json, columns_json,
			 is_shared, is_default)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		publicID, tenantID, userID, p.Name, p.Resource,
		jsonOrNull(p.Filters), jsonOrNull(p.Columns), p.IsShared, p.IsDefault)
	if err != nil {
		if platform.IsDuplicate(err) {
			return nil, platform.ErrSentinelConflict
		}
		return nil, fmt.Errorf("creating saved view: %w", err)
	}
	return r.ViewByPublicID(ctx, tenantID, userID, publicID)
}

// UpdateView edits a view. Only the owner may call this; the caller checks.
func (r *Repository) UpdateView(ctx context.Context, tenantID, userID int64, publicID string, p ViewParams) (*View, error) {
	if p.IsDefault {
		if err := r.clearDefault(ctx, tenantID, userID, p.Resource); err != nil {
			return nil, err
		}
	}

	res, err := r.db.Primary.ExecContext(ctx, `
		UPDATE saved_views
		SET name = ?, filters_json = ?, columns_json = ?, is_shared = ?, is_default = ?
		WHERE tenant_id = ? AND user_id = ? AND public_id = ?`,
		p.Name, jsonOrNull(p.Filters), jsonOrNull(p.Columns), p.IsShared, p.IsDefault,
		tenantID, userID, publicID)
	if err != nil {
		return nil, fmt.Errorf("updating saved view: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either it does not exist or it belongs to someone else. Both are
		// "not found" from here: confirming it exists would disclose another
		// user's view.
		return nil, platform.ErrSentinelNotFound
	}
	return r.ViewByPublicID(ctx, tenantID, userID, publicID)
}

func (r *Repository) DeleteView(ctx context.Context, tenantID, userID int64, publicID string) error {
	res, err := r.db.Primary.ExecContext(ctx,
		`DELETE FROM saved_views WHERE tenant_id = ? AND user_id = ? AND public_id = ?`,
		tenantID, userID, publicID)
	if err != nil {
		return fmt.Errorf("deleting saved view: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return platform.ErrSentinelNotFound
	}
	return nil
}

func (r *Repository) ViewByPublicID(ctx context.Context, tenantID, userID int64, publicID string) (*View, error) {
	var v View
	err := r.db.Primary.GetContext(ctx, &v, `
		SELECT `+viewColumns+`,
			CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS owner_name,
			(v.user_id = ?) AS is_mine
		FROM saved_views v
		JOIN users u ON u.id = v.user_id
		WHERE v.tenant_id = ? AND v.public_id = ? AND (v.user_id = ? OR v.is_shared = 1)`,
		userID, tenantID, publicID, userID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading saved view: %w", err)
	}
	return &v, nil
}

// clearDefault demotes whatever was the default, so exactly one view per user
// per resource can be the one that opens automatically.
func (r *Repository) clearDefault(ctx context.Context, tenantID, userID int64, resource string) error {
	_, err := r.db.Primary.ExecContext(ctx,
		`UPDATE saved_views SET is_default = 0
		 WHERE tenant_id = ? AND user_id = ? AND resource = ?`, tenantID, userID, resource)
	if err != nil {
		return fmt.Errorf("clearing previous default view: %w", err)
	}
	return nil
}

func jsonOrNull(raw json.RawMessage) any {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil
	}
	return string(raw)
}

// --- HTTP -------------------------------------------------------------------

type viewRequest struct {
	Name      string          `json:"name" validate:"required,notblank,max=128,safetext"`
	Resource  string          `json:"resource" validate:"omitempty,oneof=tickets users documents reports"`
	Filters   json.RawMessage `json:"filters"`
	Columns   json.RawMessage `json:"columns"`
	IsShared  bool            `json:"is_shared"`
	IsDefault bool            `json:"is_default"`
}

func (req viewRequest) params() ViewParams {
	resource := req.Resource
	if resource == "" {
		resource = "tickets"
	}
	return ViewParams{
		Name: req.Name, Resource: resource, Filters: req.Filters,
		Columns: req.Columns, IsShared: req.IsShared, IsDefault: req.IsDefault,
	}
}

// ViewRoutes mounts the saved-view surface. No extra permission: a view is the
// caller's own bookmark, and the filters inside it are re-applied against their
// own scope every time the list runs.
func (h *Handler) ViewRoutes(r chi.Router) {
	r.Route("/views", func(r chi.Router) {
		r.Get("/", h.listViews)
		r.Post("/", h.createView)
		r.Get("/{id}", h.getView)
		r.Patch("/{id}", h.updateView)
		r.Delete("/{id}", h.deleteView)
	})
}

func (h *Handler) listViews(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	rows, err := h.repo.Views(ctx, appctx.TenantID(ctx), actor.UserID, r.URL.Query().Get("resource"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]viewResponse, 0, len(rows))
	for _, v := range rows {
		out = append(out, toViewResponse(v))
	}
	httpx.OK(w, r, map[string]any{"items": out})
}

func (h *Handler) getView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	v, err := h.repo.ViewByPublicID(ctx, appctx.TenantID(ctx), actor.UserID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapViewErr(err))
		return
	}
	httpx.OK(w, r, toViewResponse(*v))
}

func (h *Handler) createView(w http.ResponseWriter, r *http.Request) {
	var req viewRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	v, err := h.repo.CreateView(ctx, appctx.TenantID(ctx), actor.UserID, req.params())
	if err != nil {
		httpx.Fail(w, r, mapViewErr(err))
		return
	}
	httpx.Created(w, r, toViewResponse(*v))
}

func (h *Handler) updateView(w http.ResponseWriter, r *http.Request) {
	var req viewRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	v, err := h.repo.UpdateView(ctx, appctx.TenantID(ctx), actor.UserID, chi.URLParam(r, "id"), req.params())
	if err != nil {
		httpx.Fail(w, r, mapViewErr(err))
		return
	}
	httpx.OK(w, r, toViewResponse(*v))
}

func (h *Handler) deleteView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	if err := h.repo.DeleteView(ctx, appctx.TenantID(ctx), actor.UserID, chi.URLParam(r, "id")); err != nil {
		httpx.Fail(w, r, mapViewErr(err))
		return
	}
	httpx.OK(w, r, map[string]any{"message": "View removed."})
}

func mapViewErr(err error) error {
	switch {
	case errors.Is(err, platform.ErrSentinelNotFound):
		return httpx.ErrNotFound("That view")
	case errors.Is(err, platform.ErrSentinelConflict):
		return httpx.ErrDuplicate("name", "You already have a view with that name.")
	default:
		return httpx.ErrInternal(err)
	}
}
