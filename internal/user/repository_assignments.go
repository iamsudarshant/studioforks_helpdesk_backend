package user

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

// EntityAssignment is one partner (or agent) assigned to an entity, with the
// reply-rights flag that decides whether they may answer its tickets.
type EntityAssignment struct {
	ID             int64          `db:"id"               json:"id"`
	PublicID       string         `db:"public_id"        json:"public_id"`
	EntityID       int64          `db:"entity_id"        json:"-"`
	EntityPublicID string         `db:"entity_public_id" json:"entity_id"`
	UserID         int64          `db:"user_id"          json:"-"`
	UserPublicID   string         `db:"user_public_id"   json:"user_id"`
	UserName       string         `db:"user_name"        json:"user_name"`
	UserEmail      sql.NullString `db:"user_email"       json:"user_email"`
	CanReply       bool           `db:"can_reply"        json:"can_reply"`
	AssignedBy     sql.NullInt64  `db:"assigned_by"      json:"-"`
	AssignedByName sql.NullString `db:"assigned_by_name" json:"assigned_by_name"`
	CreatedAt      time.Time      `db:"created_at"       json:"created_at"`
}

// SiteAssignment is one user assigned to a site.
type SiteAssignment struct {
	ID             int64          `db:"id"               json:"id"`
	PublicID       string         `db:"public_id"        json:"public_id"`
	SiteID         int64          `db:"site_id"          json:"-"`
	SitePublicID   string         `db:"site_public_id"   json:"site_id"`
	UserID         int64          `db:"user_id"          json:"-"`
	UserPublicID   string         `db:"user_public_id"   json:"user_id"`
	UserName       string         `db:"user_name"        json:"user_name"`
	UserEmail      sql.NullString `db:"user_email"       json:"user_email"`
	AssignedByName sql.NullString `db:"assigned_by_name" json:"assigned_by_name"`
	CreatedAt      time.Time      `db:"created_at"       json:"created_at"`
}

// DepartmentAssignment is one user assigned to a department.
type DepartmentAssignment struct {
	ID              int64          `db:"id"               json:"id"`
	PublicID        string         `db:"public_id"        json:"public_id"`
	DepartmentID    int64          `db:"department_id"    json:"-"`
	DepartmentPubID string         `db:"department_public_id" json:"department_id"`
	UserID          int64          `db:"user_id"          json:"-"`
	UserPublicID    string         `db:"user_public_id"   json:"user_id"`
	UserName        string         `db:"user_name"        json:"user_name"`
	UserEmail       sql.NullString `db:"user_email"       json:"user_email"`
	AssignedByName  sql.NullString `db:"assigned_by_name" json:"assigned_by_name"`
	CreatedAt       time.Time      `db:"created_at"       json:"created_at"`
}

// --- entity assignments -----------------------------------------------------

// AssignEntityUser assigns a user to an entity and mirrors it into their scope
// so every existing entity filter — tickets, users, pickers — enforces it. It
// is an upsert: re-assigning is harmless and updates the reply-rights flag.
func (r *Repository) AssignEntityUser(ctx context.Context, tenantID, entityID, userID int64, canReply bool, assignedBy *int64) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO entity_assignments (public_id, tenant_id, entity_id, user_id, can_reply, assigned_by)
			VALUES (?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE can_reply = VALUES(can_reply), assigned_by = VALUES(assigned_by)`,
			platform.NewULID(), tenantID, entityID, userID, canReply, assignedBy); err != nil {
			return fmt.Errorf("assigning entity: %w", err)
		}

		// Mirror into the user's scope so the assignment takes effect everywhere
		// a scope is consulted, without a second source of truth.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_scopes (tenant_id, user_id, scope_type, scope_id)
			VALUES (?,?,?,?)
			ON DUPLICATE KEY UPDATE scope_id = scope_id`,
			tenantID, userID, ScopeEntity, entityID); err != nil {
			return fmt.Errorf("mirroring entity scope: %w", err)
		}
		return nil
	})
}

// SetEntityReplyRights toggles whether an assigned user may reply on the
// entity's tickets.
func (r *Repository) SetEntityReplyRights(ctx context.Context, tenantID, entityID, userID int64, canReply bool) error {
	res, err := r.db.Primary.ExecContext(ctx, `
		UPDATE entity_assignments SET can_reply = ?
		WHERE tenant_id = ? AND entity_id = ? AND user_id = ?`,
		canReply, tenantID, entityID, userID)
	if err != nil {
		return fmt.Errorf("updating entity reply rights: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return platform.ErrSentinelNotFound
	}
	return nil
}

// RevokeEntityUser removes a user from an entity and clears the mirrored scope.
func (r *Repository) RevokeEntityUser(ctx context.Context, tenantID, entityID, userID int64) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM entity_assignments
			WHERE tenant_id = ? AND entity_id = ? AND user_id = ?`,
			tenantID, entityID, userID)
		if err != nil {
			return fmt.Errorf("revoking entity assignment: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return platform.ErrSentinelNotFound
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM user_scopes
			WHERE user_id = ? AND scope_type = ? AND scope_id = ?`,
			userID, ScopeEntity, entityID); err != nil {
			return fmt.Errorf("clearing entity scope: %w", err)
		}
		return nil
	})
}

// EntityAssignments lists who is assigned to an entity, newest first.
func (r *Repository) EntityAssignments(ctx context.Context, tenantID, entityID int64) ([]EntityAssignment, error) {
	rows := []EntityAssignment{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT a.id, a.public_id, a.entity_id, e.public_id AS entity_public_id,
		       a.user_id, u.public_id AS user_public_id,
		       CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS user_name,
		       u.email AS user_email, a.can_reply,
		       a.assigned_by,
		       CONCAT(ab.first_name, ' ', COALESCE(ab.last_name, '')) AS assigned_by_name,
		       a.created_at
		FROM entity_assignments a
		JOIN entities e ON e.id = a.entity_id
		JOIN users u   ON u.id = a.user_id
		LEFT JOIN users ab ON ab.id = a.assigned_by
		WHERE a.tenant_id = ? AND a.entity_id = ?
		ORDER BY a.created_at DESC, a.id DESC`, tenantID, entityID)
	if err != nil {
		return nil, fmt.Errorf("listing entity assignments: %w", err)
	}
	return rows, nil
}

// --- user-centric views ------------------------------------------------------

// UserEntityAssignment is one entity a user is scoped to, seen from the user.
type UserEntityAssignment struct {
	ID             int64          `db:"id"                json:"id"`
	PublicID       string         `db:"public_id"         json:"public_id"`
	EntityPublicID string         `db:"entity_public_id"  json:"entity_id"`
	EntityName     string         `db:"entity_name"       json:"entity_name"`
	CanReply       bool           `db:"can_reply"         json:"can_reply"`
	AssignedByName sql.NullString `db:"assigned_by_name"  json:"assigned_by_name"`
	CreatedAt      time.Time      `db:"created_at"        json:"created_at"`
}

// UserSiteAssignment is one site a user is scoped to, seen from the user.
type UserSiteAssignment struct {
	ID             int64          `db:"id"                json:"id"`
	PublicID       string         `db:"public_id"         json:"public_id"`
	SitePublicID   string         `db:"site_public_id"    json:"site_id"`
	SiteName       string         `db:"site_name"         json:"site_name"`
	AssignedByName sql.NullString `db:"assigned_by_name"  json:"assigned_by_name"`
	CreatedAt      time.Time      `db:"created_at"        json:"created_at"`
}

// UserDepartmentAssignment is one department a user is scoped to, from the user.
type UserDepartmentAssignment struct {
	ID              int64          `db:"id"                json:"id"`
	PublicID        string         `db:"public_id"         json:"public_id"`
	DepartmentPubID string         `db:"department_public_id" json:"department_id"`
	DepartmentName  string         `db:"department_name"   json:"department_name"`
	AssignedByName  sql.NullString `db:"assigned_by_name"  json:"assigned_by_name"`
	CreatedAt       time.Time      `db:"created_at"        json:"created_at"`
}

// EntityAssignmentsForUser lists the entities a user is scoped to, newest first.
func (r *Repository) EntityAssignmentsForUser(ctx context.Context, tenantID, userID int64) ([]UserEntityAssignment, error) {
	rows := []UserEntityAssignment{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT a.id, a.public_id, e.public_id AS entity_public_id, e.name AS entity_name,
		       a.can_reply,
		       CONCAT(ab.first_name, ' ', COALESCE(ab.last_name, '')) AS assigned_by_name,
		       a.created_at
		FROM entity_assignments a
		JOIN entities e ON e.id = a.entity_id
		LEFT JOIN users ab ON ab.id = a.assigned_by
		WHERE a.tenant_id = ? AND a.user_id = ?
		ORDER BY a.created_at DESC, a.id DESC`, tenantID, userID)
	if err != nil {
		return nil, fmt.Errorf("listing entity assignments for user: %w", err)
	}
	return rows, nil
}

// SiteAssignmentsForUser lists the sites a user is scoped to, newest first.
func (r *Repository) SiteAssignmentsForUser(ctx context.Context, tenantID, userID int64) ([]UserSiteAssignment, error) {
	rows := []UserSiteAssignment{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT a.id, a.public_id, s.public_id AS site_public_id, s.name AS site_name,
		       CONCAT(ab.first_name, ' ', COALESCE(ab.last_name, '')) AS assigned_by_name,
		       a.created_at
		FROM site_assignments a
		JOIN sites s ON s.id = a.site_id
		LEFT JOIN users ab ON ab.id = a.assigned_by
		WHERE a.tenant_id = ? AND a.user_id = ?
		ORDER BY a.created_at DESC, a.id DESC`, tenantID, userID)
	if err != nil {
		return nil, fmt.Errorf("listing site assignments for user: %w", err)
	}
	return rows, nil
}

// DepartmentAssignmentsForUser lists the departments a user is scoped to, newest first.
func (r *Repository) DepartmentAssignmentsForUser(ctx context.Context, tenantID, userID int64) ([]UserDepartmentAssignment, error) {
	rows := []UserDepartmentAssignment{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT a.id, a.public_id, d.public_id AS department_public_id, d.name AS department_name,
		       CONCAT(ab.first_name, ' ', COALESCE(ab.last_name, '')) AS assigned_by_name,
		       a.created_at
		FROM department_assignments a
		JOIN departments d ON d.id = a.department_id
		LEFT JOIN users ab ON ab.id = a.assigned_by
		WHERE a.tenant_id = ? AND a.user_id = ?
		ORDER BY a.created_at DESC, a.id DESC`, tenantID, userID)
	if err != nil {
		return nil, fmt.Errorf("listing department assignments for user: %w", err)
	}
	return rows, nil
}

// --- site assignments -------------------------------------------------------

func (r *Repository) AssignSiteUser(ctx context.Context, tenantID, siteID, userID int64, assignedBy *int64) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO site_assignments (public_id, tenant_id, site_id, user_id, assigned_by)
			VALUES (?,?,?,?,?)
			ON DUPLICATE KEY UPDATE assigned_by = VALUES(assigned_by)`,
			platform.NewULID(), tenantID, siteID, userID, assignedBy); err != nil {
			return fmt.Errorf("assigning site: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_scopes (tenant_id, user_id, scope_type, scope_id)
			VALUES (?,?,?,?)
			ON DUPLICATE KEY UPDATE scope_id = scope_id`,
			tenantID, userID, ScopeSite, siteID); err != nil {
			return fmt.Errorf("mirroring site scope: %w", err)
		}
		return nil
	})
}

func (r *Repository) RevokeSiteUser(ctx context.Context, tenantID, siteID, userID int64) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM site_assignments
			WHERE tenant_id = ? AND site_id = ? AND user_id = ?`,
			tenantID, siteID, userID)
		if err != nil {
			return fmt.Errorf("revoking site assignment: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return platform.ErrSentinelNotFound
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM user_scopes
			WHERE user_id = ? AND scope_type = ? AND scope_id = ?`,
			userID, ScopeSite, siteID); err != nil {
			return fmt.Errorf("clearing site scope: %w", err)
		}
		return nil
	})
}

func (r *Repository) SiteAssignments(ctx context.Context, tenantID, siteID int64) ([]SiteAssignment, error) {
	rows := []SiteAssignment{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT a.id, a.public_id, a.site_id, s.public_id AS site_public_id,
		       a.user_id, u.public_id AS user_public_id,
		       CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS user_name,
		       u.email AS user_email,
		       CONCAT(ab.first_name, ' ', COALESCE(ab.last_name, '')) AS assigned_by_name,
		       a.created_at
		FROM site_assignments a
		JOIN sites s ON s.id = a.site_id
		JOIN users u   ON u.id = a.user_id
		LEFT JOIN users ab ON ab.id = a.assigned_by
		WHERE a.tenant_id = ? AND a.site_id = ?
		ORDER BY a.created_at DESC, a.id DESC`, tenantID, siteID)
	if err != nil {
		return nil, fmt.Errorf("listing site assignments: %w", err)
	}
	return rows, nil
}

// --- department assignments -------------------------------------------------

func (r *Repository) AssignDepartmentUser(ctx context.Context, tenantID, departmentID, userID int64, assignedBy *int64) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO department_assignments (public_id, tenant_id, department_id, user_id, assigned_by)
			VALUES (?,?,?,?,?)
			ON DUPLICATE KEY UPDATE assigned_by = VALUES(assigned_by)`,
			platform.NewULID(), tenantID, departmentID, userID, assignedBy); err != nil {
			return fmt.Errorf("assigning department: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_scopes (tenant_id, user_id, scope_type, scope_id)
			VALUES (?,?,?,?)
			ON DUPLICATE KEY UPDATE scope_id = scope_id`,
			tenantID, userID, ScopeDepartment, departmentID); err != nil {
			return fmt.Errorf("mirroring department scope: %w", err)
		}
		return nil
	})
}

func (r *Repository) RevokeDepartmentUser(ctx context.Context, tenantID, departmentID, userID int64) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM department_assignments
			WHERE tenant_id = ? AND department_id = ? AND user_id = ?`,
			tenantID, departmentID, userID)
		if err != nil {
			return fmt.Errorf("revoking department assignment: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return platform.ErrSentinelNotFound
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM user_scopes
			WHERE user_id = ? AND scope_type = ? AND scope_id = ?`,
			userID, ScopeDepartment, departmentID); err != nil {
			return fmt.Errorf("clearing department scope: %w", err)
		}
		return nil
	})
}

func (r *Repository) DepartmentAssignments(ctx context.Context, tenantID, departmentID int64) ([]DepartmentAssignment, error) {
	rows := []DepartmentAssignment{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT a.id, a.public_id, a.department_id, d.public_id AS department_public_id,
		       a.user_id, u.public_id AS user_public_id,
		       CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS user_name,
		       u.email AS user_email,
		       CONCAT(ab.first_name, ' ', COALESCE(ab.last_name, '')) AS assigned_by_name,
		       a.created_at
		FROM department_assignments a
		JOIN departments d ON d.id = a.department_id
		JOIN users u       ON u.id = a.user_id
		LEFT JOIN users ab ON ab.id = a.assigned_by
		WHERE a.tenant_id = ? AND a.department_id = ?
		ORDER BY a.created_at DESC, a.id DESC`, tenantID, departmentID)
	if err != nil {
		return nil, fmt.Errorf("listing department assignments: %w", err)
	}
	return rows, nil
}
