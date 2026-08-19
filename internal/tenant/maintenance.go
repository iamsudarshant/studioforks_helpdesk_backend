package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// Maintenance windows are queried on every request, so the result is cached in
// Redis for a few seconds. Turning maintenance on therefore takes effect within
// the cache TTL rather than instantly; the admin UI states this.
const maintenanceCacheTTL = 10 * time.Second

type MaintenanceParams struct {
	Scope        string
	TenantID     *int64
	Mode         string
	Title        string
	Message      string
	StartsAt     time.Time
	EndsAt       *time.Time
	AllowedRoles []string
	CreatedBy    *int64
}

func (r *Repository) CreateMaintenance(ctx context.Context, p MaintenanceParams) (*MaintenanceWindow, error) {
	roles, err := json.Marshal(p.AllowedRoles)
	if err != nil {
		return nil, fmt.Errorf("encoding allowed roles: %w", err)
	}

	publicID := platform.NewULID()
	res, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO maintenance_windows
			(public_id, scope, tenant_id, mode, title, message, starts_at, ends_at,
			 is_active, allow_roles_json, created_by)
		VALUES (?,?,?,?,?,?,?,?,1,?,?)`,
		publicID, p.Scope, p.TenantID, p.Mode, p.Title, nullString(p.Message),
		p.StartsAt, p.EndsAt, string(roles), p.CreatedBy)
	if err != nil {
		return nil, fmt.Errorf("creating maintenance window: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading maintenance id: %w", err)
	}
	return r.MaintenanceByID(ctx, id)
}

func (r *Repository) MaintenanceByID(ctx context.Context, id int64) (*MaintenanceWindow, error) {
	var w MaintenanceWindow
	err := r.db.Primary.GetContext(ctx, &w, `
		SELECT id, public_id, scope, tenant_id, mode, title, message, starts_at, ends_at,
		       is_active, allow_roles_json, created_by, created_at, updated_at
		FROM maintenance_windows WHERE id = ?`, id)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading maintenance window: %w", err)
	}
	return &w, nil
}

func (r *Repository) MaintenanceByPublicID(ctx context.Context, publicID string) (*MaintenanceWindow, error) {
	var w MaintenanceWindow
	err := r.db.Primary.GetContext(ctx, &w, `
		SELECT id, public_id, scope, tenant_id, mode, title, message, starts_at, ends_at,
		       is_active, allow_roles_json, created_by, created_at, updated_at
		FROM maintenance_windows WHERE public_id = ?`, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading maintenance window: %w", err)
	}
	return &w, nil
}

// ListMaintenance returns windows for a tenant plus every global window.
func (r *Repository) ListMaintenance(ctx context.Context, tenantID *int64, activeOnly bool) ([]MaintenanceWindow, error) {
	where := []string{"(scope = 'GLOBAL'"}
	args := []any{}
	if tenantID != nil {
		where[0] += " OR tenant_id = ?"
		args = append(args, *tenantID)
	}
	where[0] += ")"

	if activeOnly {
		where = append(where, "is_active = 1", "starts_at <= UTC_TIMESTAMP(3)",
			"(ends_at IS NULL OR ends_at > UTC_TIMESTAMP(3))")
	}

	rows := []MaintenanceWindow{}
	q := `SELECT id, public_id, scope, tenant_id, mode, title, message, starts_at, ends_at,
	             is_active, allow_roles_json, created_by, created_at, updated_at
	      FROM maintenance_windows WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY starts_at DESC LIMIT 200`

	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("listing maintenance windows: %w", err)
	}
	return rows, nil
}

type MaintenanceUpdate struct {
	Mode         *string
	Title        *string
	Message      *string
	StartsAt     *time.Time
	EndsAt       *time.Time
	IsActive     *bool
	AllowedRoles *[]string
}

func (r *Repository) UpdateMaintenance(ctx context.Context, id int64, u MaintenanceUpdate) error {
	set := []string{}
	args := []any{}

	if u.Mode != nil {
		set, args = append(set, "mode = ?"), append(args, *u.Mode)
	}
	if u.Title != nil {
		set, args = append(set, "title = ?"), append(args, *u.Title)
	}
	if u.Message != nil {
		set, args = append(set, "message = ?"), append(args, nullString(*u.Message))
	}
	if u.StartsAt != nil {
		set, args = append(set, "starts_at = ?"), append(args, *u.StartsAt)
	}
	if u.EndsAt != nil {
		set, args = append(set, "ends_at = ?"), append(args, *u.EndsAt)
	}
	if u.IsActive != nil {
		set, args = append(set, "is_active = ?"), append(args, *u.IsActive)
	}
	if u.AllowedRoles != nil {
		roles, err := json.Marshal(*u.AllowedRoles)
		if err != nil {
			return fmt.Errorf("encoding allowed roles: %w", err)
		}
		set, args = append(set, "allow_roles_json = ?"), append(args, string(roles))
	}
	if len(set) == 0 {
		return nil
	}

	args = append(args, id)
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE maintenance_windows SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("updating maintenance window: %w", err)
	}
	return requireAffected(res)
}

func (r *Repository) DeleteMaintenance(ctx context.Context, id int64) error {
	res, err := r.db.Primary.ExecContext(ctx, `DELETE FROM maintenance_windows WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting maintenance window: %w", err)
	}
	return requireAffected(res)
}

// currentMaintenance finds the window in force right now. A GLOBAL LOCKOUT
// outranks a tenant window, and a LOCKOUT outranks a BANNER.
func (r *Repository) currentMaintenance(ctx context.Context, tenantID int64) (*middleware.MaintenanceState, error) {
	var tenantArg any
	if tenantID > 0 {
		tenantArg = tenantID
	}

	rows := []MaintenanceWindow{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT id, public_id, scope, tenant_id, mode, title, message, starts_at, ends_at,
		       is_active, allow_roles_json, created_by, created_at, updated_at
		FROM maintenance_windows
		WHERE is_active = 1
		  AND starts_at <= UTC_TIMESTAMP(3)
		  AND (ends_at IS NULL OR ends_at > UTC_TIMESTAMP(3))
		  AND (scope = 'GLOBAL' OR tenant_id = ?)
		ORDER BY FIELD(mode,'LOCKOUT','BANNER'), FIELD(scope,'GLOBAL','TENANT'), starts_at DESC
		LIMIT 1`, tenantArg)
	if err != nil {
		return nil, fmt.Errorf("loading current maintenance: %w", err)
	}
	if len(rows) == 0 {
		return &middleware.MaintenanceState{Active: false}, nil
	}

	w := rows[0]
	state := &middleware.MaintenanceState{
		Active:   true,
		ID:       w.PublicID,
		Scope:    w.Scope,
		Mode:     w.Mode,
		Title:    w.Title,
		Message:  w.Message.String,
		StartsAt: &w.StartsAt,
	}
	if w.EndsAt.Valid {
		state.EndsAt = &w.EndsAt.Time
	}
	if w.AllowRolesJSON.Valid {
		_ = json.Unmarshal([]byte(w.AllowRolesJSON.String), &state.AllowedRoles)
	}
	return state, nil
}
