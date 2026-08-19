package catalogue

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// Module is one compliance domain — PF, ESIC, Payroll, Labour Law and so on.
//
// The catalogue is platform-wide; which of it a given client sees is decided by
// tenant_modules. Adding a domain is a row here, never a code change, which is
// why the list is served rather than compiled in.
type Module struct {
	ID          int64          `db:"id"`
	PublicID    string         `db:"public_id"`
	Key         string         `db:"module_key"`
	Name        string         `db:"name"`
	Description sql.NullString `db:"description"`
	Icon        sql.NullString `db:"icon"`
	Color       sql.NullString `db:"color"`
	IsCore      bool           `db:"is_core"`
	SortOrder   int            `db:"sort_order"`

	// Enabled reflects this client's tenant_modules row. A core module is not
	// automatically enabled — a client may opt out of anything.
	Enabled bool `db:"enabled"`
	// CategoryCount lets the UI hide a module that would open onto nothing.
	CategoryCount int `db:"category_count"`
}

// ModulesFor lists the catalogue as one client sees it.
//
// Every active module is returned, enabled or not, because an administrator
// needs to see what could be switched on. Callers that render navigation must
// filter on Enabled themselves — `EnabledModulesFor` does it for them.
func (r *Repository) ModulesFor(ctx context.Context, tenantID int64) ([]Module, error) {
	rows := []Module{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT m.id, m.public_id, m.module_key, m.name, m.description, m.icon,
		       m.color, m.is_core, m.sort_order,
		       COALESCE(tm.enabled, 0) AS enabled,
		       (SELECT COUNT(*) FROM categories c
		         WHERE c.tenant_id = ? AND c.module_id = m.id
		           AND c.is_active = 1 AND c.is_subcategory = 0
		           AND c.deleted_at IS NULL) AS category_count
		FROM modules m
		LEFT JOIN tenant_modules tm ON tm.module_id = m.id AND tm.tenant_id = ?
		WHERE m.is_active = 1
		ORDER BY m.sort_order, m.name`, tenantID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("listing modules: %w", err)
	}
	return rows, nil
}

// SetModuleEnabled switches one module on or off for a client.
//
// Absence of a row means "not enabled", so switching on has to insert; the
// unique key on (tenant_id, module_id) makes that a single statement.
func (r *Repository) SetModuleEnabled(ctx context.Context, tenantID int64, publicID string, enabled bool, actorID int64) error {
	var moduleID int64
	if err := r.db.Primary.GetContext(ctx, &moduleID,
		`SELECT id FROM modules WHERE public_id = ? AND is_active = 1`, publicID); err != nil {
		if platform.IsNotFound(err) {
			return httpx.ErrNotFound("That module does not exist.")
		}
		return fmt.Errorf("locating module: %w", err)
	}

	if _, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO tenant_modules (tenant_id, module_id, enabled, enabled_by)
		VALUES (?,?,?,?)
		ON DUPLICATE KEY UPDATE enabled = VALUES(enabled), enabled_by = VALUES(enabled_by)`,
		tenantID, moduleID, enabled, actorID); err != nil {
		return fmt.Errorf("setting module state: %w", err)
	}
	return nil
}

// --- HTTP -------------------------------------------------------------------

type moduleResponse struct {
	ID            string `json:"id"`
	Key           string `json:"key"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	Icon          string `json:"icon,omitempty"`
	Color         string `json:"color,omitempty"`
	IsCore        bool   `json:"is_core"`
	Enabled       bool   `json:"enabled"`
	CategoryCount int    `json:"category_count"`
	SortOrder     int    `json:"sort_order"`
}

func toModuleResponse(m Module) moduleResponse {
	return moduleResponse{
		ID: m.PublicID, Key: m.Key, Name: m.Name,
		Description: m.Description.String, Icon: m.Icon.String, Color: m.Color.String,
		IsCore: m.IsCore, Enabled: m.Enabled,
		CategoryCount: m.CategoryCount, SortOrder: m.SortOrder,
	}
}

func (h *Handler) listModules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := h.repo.ModulesFor(ctx, appctx.TenantID(ctx))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	// `enabled_only=true` is what navigation asks for; administration asks for
	// everything so it can offer the rest.
	enabledOnly := r.URL.Query().Get("enabled_only") == "true"

	out := make([]moduleResponse, 0, len(rows))
	for _, m := range rows {
		if enabledOnly && !m.Enabled {
			continue
		}
		out = append(out, toModuleResponse(m))
	}
	httpx.OK(w, r, out)
}

type setModuleRequest struct {
	Enabled *bool `json:"enabled" validate:"required"`
}

func (h *Handler) setModule(w http.ResponseWriter, r *http.Request) {
	var req setModuleRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	if err := h.repo.SetModuleEnabled(ctx, appctx.TenantID(ctx),
		chi.URLParam(r, "id"), *req.Enabled, actor.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	rows, err := h.repo.ModulesFor(ctx, appctx.TenantID(ctx))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	for _, m := range rows {
		if m.PublicID == chi.URLParam(r, "id") {
			httpx.OK(w, r, toModuleResponse(m))
			return
		}
	}
	httpx.Fail(w, r, httpx.ErrNotFound("That module does not exist."))
}
