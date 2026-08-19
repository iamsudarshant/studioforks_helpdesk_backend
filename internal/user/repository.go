package user

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/platform"
)

type Repository struct {
	db *platform.DB
}

func NewRepository(db *platform.DB) *Repository { return &Repository{db: db} }

const userColumns = `id, public_id, tenant_id, employee_code, username, first_name, last_name,
	email, alt_email, mobile, alt_mobile, pan_number, uan_number, pf_number, esic_number,
	date_of_joining, date_of_birth, last_working_day, entity_id, site_id, department_id,
	designation, user_group_id, handling_agent_id, employment_changed_at, employment_changed_by,
	status, password_hash, password_algo, must_change_password,
	password_changed_at, password_expires_at, failed_login_count, locked_until, mfa_enabled,
	mfa_secret_enc, mfa_recovery_json, last_login_at, login_count, avatar_path, locale,
	timezone, custom_fields_json, created_by, updated_by, created_at, updated_at, deleted_at`

// identifierColumns maps a login-identifier key onto its database column. Only
// keys present here can ever reach a query, so the tenant's configured list can
// never inject SQL.
var identifierColumns = map[string]string{
	"email":         "email",
	"alt_email":     "alt_email",
	"employee_code": "employee_code",
	"pf_number":     "pf_number",
	"uan_number":    "uan_number",
	"pan_number":    "pan_number",
	"mobile":        "mobile",
	"username":      "username",
}

// FindByIdentifier resolves a login identifier against the columns the tenant
// has enabled. Matching is case-insensitive for text identifiers, and PAN is
// upper-cased because that is its canonical form.
func (r *Repository) FindByIdentifier(ctx context.Context, tenantID int64, identifier string, allowed []string) (*User, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil, platform.ErrSentinelNotFound
	}

	clauses := make([]string, 0, len(allowed))
	args := []any{tenantID}

	for _, key := range allowed {
		col, ok := identifierColumns[key]
		if !ok {
			continue // unknown key from configuration is ignored, never interpolated
		}
		clauses = append(clauses, col+" = ?")
		switch key {
		case "pan_number":
			args = append(args, strings.ToUpper(identifier))
		case "email", "alt_email", "username":
			args = append(args, strings.ToLower(identifier))
		default:
			args = append(args, identifier)
		}
	}
	if len(clauses) == 0 {
		return nil, platform.ErrSentinelNotFound
	}

	q := `SELECT ` + userColumns + ` FROM users
	      WHERE tenant_id = ? AND deleted_at IS NULL AND (` + strings.Join(clauses, " OR ") + `)
	      LIMIT 2`

	rows := []User{}
	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("resolving identifier: %w", err)
	}
	switch len(rows) {
	case 0:
		return nil, platform.ErrSentinelNotFound
	case 1:
		return &rows[0], nil
	default:
		// Two accounts matched different columns with the same value. Refusing
		// is safer than guessing which account the caller meant.
		return nil, platform.ErrSentinelConflict
	}
}

func (r *Repository) ByID(ctx context.Context, tenantID, id int64) (*User, error) {
	var u User
	err := r.db.Primary.GetContext(ctx, &u,
		`SELECT `+userColumns+` FROM users WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
		tenantID, id)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading user: %w", err)
	}
	return &u, nil
}

// ByIDAnyTenant is used only by token verification, where the tenant is taken
// from the token itself rather than from the request.
func (r *Repository) ByIDAnyTenant(ctx context.Context, id int64) (*User, error) {
	var u User
	err := r.db.Primary.GetContext(ctx, &u,
		`SELECT `+userColumns+` FROM users WHERE id = ? AND deleted_at IS NULL`, id)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading user: %w", err)
	}
	return &u, nil
}

func (r *Repository) ByPublicID(ctx context.Context, tenantID int64, publicID string) (*User, error) {
	var u User
	err := r.db.Primary.GetContext(ctx, &u,
		`SELECT `+userColumns+` FROM users WHERE tenant_id = ? AND public_id = ? AND deleted_at IS NULL`,
		tenantID, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading user: %w", err)
	}
	return &u, nil
}

// ByPublicIDInReach loads a user from anywhere the caller can reach.
//
// The roster is cross-client for staff who have not chosen a client from the
// switcher, so opening a row from that list has to resolve without knowing
// which client the person belongs to first. Pinning the lookup to the resolved
// tenant would search the platform workspace and answer "not found" for every
// row on screen.
//
// The reach is still the boundary — somebody outside it is not found — and the
// caller's entity/site/department scope is applied on top by loadTarget.
func (r *Repository) ByPublicIDInReach(ctx context.Context, reach appctx.ClientReach, publicID string) (*User, error) {
	where := []string{"public_id = ?", "deleted_at IS NULL"}
	args := []any{publicID}

	switch {
	case reach.All:
		// Every client.
	case len(reach.TenantIDs) > 0:
		where = append(where, "tenant_id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(reach.TenantIDs)...)
	default:
		where = append(where, "1 = 0")
	}

	var u User
	err := r.db.Primary.GetContext(ctx, &u,
		`SELECT `+userColumns+` FROM users WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading user: %w", err)
	}
	return &u, nil
}

// ByPublicIDGlobal resolves a user without a tenant filter. Reserved for super
// admin token verification; every other path must use the tenant-scoped form.
func (r *Repository) ByPublicIDGlobal(ctx context.Context, publicID string) (*User, error) {
	var u User
	err := r.db.Primary.GetContext(ctx, &u,
		`SELECT `+userColumns+` FROM users WHERE public_id = ? AND deleted_at IS NULL`, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading user: %w", err)
	}
	return &u, nil
}

// FindForRecovery locates an account for forgot-username / forgot-password. It
// searches only the identifier types the caller's portal permits.
func (r *Repository) FindForRecovery(ctx context.Context, tenantID int64, identifierType, value string, allowed []string) (*User, error) {
	if identifierType != "" {
		found := false
		for _, a := range allowed {
			if a == identifierType {
				found = true
				break
			}
		}
		if !found {
			return nil, platform.ErrSentinelNotFound
		}
		return r.FindByIdentifier(ctx, tenantID, value, []string{identifierType})
	}
	return r.FindByIdentifier(ctx, tenantID, value, allowed)
}

// --- roles, permissions, scopes ---------------------------------------------

func (r *Repository) RolesFor(ctx context.Context, userID int64) ([]Role, error) {
	rows := []Role{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT r.id, r.public_id, r.tenant_id, r.role_key, r.name, r.description,
		       r.portal, r.is_system, r.created_at, r.updated_at
		FROM roles r
		JOIN user_roles ur ON ur.role_id = r.id
		WHERE ur.user_id = ?
		ORDER BY r.role_key`, userID)
	if err != nil {
		return nil, fmt.Errorf("loading user roles: %w", err)
	}
	return rows, nil
}

// PermissionsFor returns the union of permissions across a user's roles.
func (r *Repository) PermissionsFor(ctx context.Context, userID int64) ([]string, error) {
	perms := []string{}
	err := r.db.Primary.SelectContext(ctx, &perms, `
		SELECT DISTINCT rp.permission_key
		FROM role_permissions rp
		JOIN user_roles ur ON ur.role_id = rp.role_id
		WHERE ur.user_id = ?
		ORDER BY rp.permission_key`, userID)
	if err != nil {
		return nil, fmt.Errorf("loading user permissions: %w", err)
	}
	return perms, nil
}

// RawScopesFor returns the allocation exactly as it was saved.
//
// The editor needs this rather than ScopesFor: expanding a department into its
// twelve entities and handing that back would make "the PF department" and
// "these twelve entities" indistinguishable on screen, so re-saving the form
// would silently convert the first into the second — and the allocation would
// then stop following the department as entities are added to it.
func (r *Repository) RawScopesFor(ctx context.Context, userID int64) ([]Scope, error) {
	rows := []Scope{}
	if err := r.db.Primary.SelectContext(ctx, &rows,
		`SELECT scope_type, scope_id FROM user_scopes WHERE user_id = ?`, userID); err != nil {
		return nil, fmt.Errorf("loading user scopes: %w", err)
	}
	return rows, nil
}

// ScopesFor loads a user's allocation, with departments expanded to the
// entities beneath them.
//
// Allocating a department means "everything this department handles". Storing
// only the department left every consumer to decide for itself what that
// implied — and they did not: the ticket list narrowed by department, the org
// pickers narrowed by entity, and a user allocated to the PF department could
// see PF tickets but not the PF entities those tickets were about.
//
// Expanding here settles it once. An explicit entity allocation still wins: a
// user given three of a department's entities gets three, not all of them.
func (r *Repository) ScopesFor(ctx context.Context, userID int64) ([]Scope, error) {
	rows, err := r.RawScopesFor(ctx, userID)
	if err != nil {
		return nil, err
	}

	departmentIDs := []int64{}
	hasEntity := false
	for _, row := range rows {
		switch row.ScopeType {
		case ScopeDepartment:
			departmentIDs = append(departmentIDs, row.ScopeID)
		case ScopeEntity:
			hasEntity = true
		}
	}
	if hasEntity || len(departmentIDs) == 0 {
		return rows, nil
	}

	entityIDs := []int64{}
	if err := r.db.Primary.SelectContext(ctx, &entityIDs, `
		SELECT id FROM entities
		WHERE department_id IN (`+platform.Placeholders(len(departmentIDs))+`)
		  AND deleted_at IS NULL`, platform.Int64Args(departmentIDs)...); err != nil {
		return nil, fmt.Errorf("expanding department scopes: %w", err)
	}
	for _, id := range entityIDs {
		rows = append(rows, Scope{ScopeType: ScopeEntity, ScopeID: id})
	}

	// A department holding no entities must not read as "every entity": the
	// allocation is real, so it narrows to nothing rather than to everything.
	if len(entityIDs) == 0 {
		rows = append(rows, Scope{ScopeType: ScopeEntity, ScopeID: 0})
	}
	return rows, nil
}

func (r *Repository) SetRoles(ctx context.Context, tenantID, userID int64, roleIDs []int64, grantedBy *int64) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_roles WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("clearing roles: %w", err)
		}
		for _, roleID := range roleIDs {
			// Guard against attaching a role that belongs to another tenant.
			var ok int
			err := tx.GetContext(ctx, &ok,
				`SELECT COUNT(*) FROM roles WHERE id = ? AND (tenant_id IS NULL OR tenant_id = ?)`,
				roleID, tenantID)
			if err != nil {
				return fmt.Errorf("validating role: %w", err)
			}
			if ok == 0 {
				return platform.ErrSentinelNotFound
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO user_roles (user_id, role_id, granted_by) VALUES (?,?,?)`,
				userID, roleID, grantedBy); err != nil {
				return fmt.Errorf("granting role: %w", err)
			}
		}
		return nil
	})
}

func (r *Repository) SetScopes(ctx context.Context, tenantID, userID int64, scopes []Scope) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_scopes WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("clearing scopes: %w", err)
		}
		for _, s := range scopes {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO user_scopes (tenant_id, user_id, scope_type, scope_id) VALUES (?,?,?,?)`,
				tenantID, userID, s.ScopeType, s.ScopeID); err != nil {
				if platform.IsDuplicate(err) {
					continue
				}
				return fmt.Errorf("granting scope: %w", err)
			}
		}
		return nil
	})
}

func (r *Repository) RoleByKey(ctx context.Context, tenantID int64, key string) (*Role, error) {
	var role Role
	err := r.db.Primary.GetContext(ctx, &role, `
		SELECT id, public_id, tenant_id, role_key, name, description, portal, is_system, created_at, updated_at
		FROM roles
		WHERE role_key = ? AND (tenant_id IS NULL OR tenant_id = ?)
		ORDER BY tenant_id IS NULL
		LIMIT 1`, key, tenantID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading role: %w", err)
	}
	return &role, nil
}

// ListRoles returns the roles that may be assigned or edited.
//
// Deprecated aliases are excluded. They exist so a token or integration written
// against an old key keeps working — `roles.alias_of` maps each to its
// replacement and the permission resolver follows it — but showing them would
// offer the reader sixteen roles where there are six, several of them different
// names for the same thing.
func (r *Repository) ListRoles(ctx context.Context, tenantID int64) ([]Role, error) {
	rows := []Role{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT id, public_id, tenant_id, role_key, name, description, portal, is_system, created_at, updated_at
		FROM roles
		WHERE (tenant_id IS NULL OR tenant_id = ?)
		  AND is_deprecated = 0
		ORDER BY FIELD(portal, 'admin', 'agents', 'partner', 'user'), role_key`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("listing roles: %w", err)
	}
	return rows, nil
}

func (r *Repository) ListPermissions(ctx context.Context) ([]Permission, error) {
	rows := []Permission{}
	if err := r.db.Primary.SelectContext(ctx, &rows,
		`SELECT permission_key, permission_group, description FROM permissions
		 ORDER BY permission_group, permission_key`); err != nil {
		return nil, fmt.Errorf("listing permissions: %w", err)
	}
	return rows, nil
}

func (r *Repository) PermissionsForRole(ctx context.Context, roleID int64) ([]string, error) {
	perms := []string{}
	if err := r.db.Primary.SelectContext(ctx, &perms,
		`SELECT permission_key FROM role_permissions WHERE role_id = ? ORDER BY permission_key`,
		roleID); err != nil {
		return nil, fmt.Errorf("loading role permissions: %w", err)
	}
	return perms, nil
}

func (r *Repository) SetRolePermissions(ctx context.Context, roleID int64, perms []string) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM role_permissions WHERE role_id = ?`, roleID); err != nil {
			return fmt.Errorf("clearing role permissions: %w", err)
		}
		for _, p := range perms {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO role_permissions (role_id, permission_key) VALUES (?,?)`, roleID, p); err != nil {
				if platform.IsForeignKeyViolation(err) {
					return platform.ErrSentinelNotFound // unknown permission key
				}
				return fmt.Errorf("granting permission: %w", err)
			}
		}
		return nil
	})
}

// --- groups -----------------------------------------------------------------

func (r *Repository) GroupByID(ctx context.Context, tenantID, id int64) (*Group, error) {
	var g Group
	err := r.db.Primary.GetContext(ctx, &g, `
		SELECT id, public_id, tenant_id, group_key, name, description, is_system,
		       access_mode, grace_period_days, sla_policy_id, created_at, updated_at
		FROM user_groups WHERE tenant_id = ? AND id = ?`, tenantID, id)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading group: %w", err)
	}
	return &g, nil
}

func (r *Repository) GroupByPublicID(ctx context.Context, tenantID int64, publicID string) (*Group, error) {
	var g Group
	err := r.db.Primary.GetContext(ctx, &g, `
		SELECT id, public_id, tenant_id, group_key, name, description, is_system,
		       access_mode, grace_period_days, sla_policy_id, created_at, updated_at
		FROM user_groups WHERE tenant_id = ? AND public_id = ?`, tenantID, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading group: %w", err)
	}
	return &g, nil
}

func (r *Repository) GroupByKey(ctx context.Context, tenantID int64, key string) (*Group, error) {
	var g Group
	err := r.db.Primary.GetContext(ctx, &g, `
		SELECT id, public_id, tenant_id, group_key, name, description, is_system,
		       access_mode, grace_period_days, sla_policy_id, created_at, updated_at
		FROM user_groups WHERE tenant_id = ? AND group_key = ?`, tenantID, key)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading group: %w", err)
	}
	return &g, nil
}

func (r *Repository) ListGroups(ctx context.Context, tenantID int64) ([]Group, error) {
	return r.ListGroupsInReach(ctx, appctx.OneClient(tenantID))
}

// ListGroupsInReach lists user groups across the clients in reach.
//
// User groups are per-client — "Employees", "Ex-employees", each with its own
// access mode and grace period — so a staff user with no client selected was
// previously shown the platform workspace's groups, of which there are none.
// That is why the screen rendered blank rather than empty-with-a-reason.
func (r *Repository) ListGroupsInReach(ctx context.Context, reach appctx.ClientReach) ([]Group, error) {
	// user_groups has no soft-delete column: a group is removed outright, and
	// its members are moved first.
	where := []string{"1 = 1"}
	args := []any{}

	switch {
	case reach.All:
		// Every client.
	case len(reach.TenantIDs) > 0:
		where = append(where, "g.tenant_id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(reach.TenantIDs)...)
	default:
		where = append(where, "1 = 0")
	}

	rows := []Group{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT g.id, g.public_id, g.tenant_id, g.group_key, g.name, g.description,
		       g.is_system, g.access_mode, g.grace_period_days, g.sla_policy_id,
		       g.created_at, g.updated_at,
		       tn.name AS client_name, tn.slug AS client_slug,
		       COALESCE(tn.client_code, '') AS client_code,
		       (SELECT COUNT(*) FROM users u
		          WHERE u.user_group_id = g.id AND u.deleted_at IS NULL) AS user_count
		FROM user_groups g
		JOIN tenants tn ON tn.id = g.tenant_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY tn.name, g.is_system DESC, g.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing groups: %w", err)
	}
	return rows, nil
}

// GroupInReach resolves a user group from anywhere the caller can reach, so a
// group opened from the cross-client list can be edited without first switching
// to the client that owns it. The write then targets the group's own client.
func (r *Repository) GroupInReach(ctx context.Context, reach appctx.ClientReach, publicID string) (*Group, error) {
	where := []string{"g.public_id = ?"}
	args := []any{publicID}

	switch {
	case reach.All:
	case len(reach.TenantIDs) > 0:
		where = append(where, "g.tenant_id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(reach.TenantIDs)...)
	default:
		where = append(where, "1 = 0")
	}

	var g Group
	err := r.db.Primary.GetContext(ctx, &g, `
		SELECT g.id, g.public_id, g.tenant_id, g.group_key, g.name, g.description,
		       g.is_system, g.access_mode, g.grace_period_days, g.sla_policy_id,
		       g.created_at, g.updated_at
		FROM user_groups g WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading user group: %w", err)
	}
	return &g, nil
}

type GroupParams struct {
	Key             string
	Name            string
	Description     string
	AccessMode      string
	GracePeriodDays int
	SLAPolicyID     *int64
}

func (r *Repository) CreateGroup(ctx context.Context, tenantID int64, p GroupParams) (*Group, error) {
	publicID := platform.NewULID()
	res, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO user_groups
			(public_id, tenant_id, group_key, name, description, is_system, access_mode, grace_period_days, sla_policy_id)
		VALUES (?,?,?,?,?,0,?,?,?)`,
		publicID, tenantID, p.Key, p.Name, nullStr(p.Description), p.AccessMode, p.GracePeriodDays, p.SLAPolicyID)
	if err != nil {
		if platform.IsDuplicate(err) {
			return nil, platform.ErrSentinelConflict
		}
		return nil, fmt.Errorf("creating group: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading group id: %w", err)
	}
	return r.GroupByID(ctx, tenantID, id)
}

type GroupUpdate struct {
	Name            *string
	Description     *string
	AccessMode      *string
	GracePeriodDays *int
	SLAPolicyID     *int64
}

func (r *Repository) UpdateGroup(ctx context.Context, tenantID, id int64, u GroupUpdate) error {
	set := []string{}
	args := []any{}

	if u.Name != nil {
		set, args = append(set, "name = ?"), append(args, *u.Name)
	}
	if u.Description != nil {
		set, args = append(set, "description = ?"), append(args, nullStr(*u.Description))
	}
	if u.AccessMode != nil {
		set, args = append(set, "access_mode = ?"), append(args, *u.AccessMode)
	}
	if u.GracePeriodDays != nil {
		set, args = append(set, "grace_period_days = ?"), append(args, *u.GracePeriodDays)
	}
	if u.SLAPolicyID != nil {
		set, args = append(set, "sla_policy_id = ?"), append(args, *u.SLAPolicyID)
	}
	if len(set) == 0 {
		return nil
	}

	args = append(args, tenantID, id)
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE user_groups SET `+strings.Join(set, ", ")+` WHERE tenant_id = ? AND id = ?`, args...)
	if err != nil {
		return fmt.Errorf("updating group: %w", err)
	}
	return affected(res)
}

// DeleteGroup refuses to remove a system group; Ex-Employees must always exist.
func (r *Repository) DeleteGroup(ctx context.Context, tenantID, id int64) error {
	g, err := r.GroupByID(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if g.IsSystem {
		return platform.ErrSentinelImmutable
	}
	res, err := r.db.Primary.ExecContext(ctx,
		`DELETE FROM user_groups WHERE tenant_id = ? AND id = ? AND is_system = 0`, tenantID, id)
	if err != nil {
		return fmt.Errorf("deleting group: %w", err)
	}
	return affected(res)
}

// --- authentication state ---------------------------------------------------

func (r *Repository) RecordLoginSuccess(ctx context.Context, userID int64) error {
	_, err := r.db.Primary.ExecContext(ctx, `
		UPDATE users
		SET last_login_at = UTC_TIMESTAMP(3),
		    login_count = login_count + 1,
		    failed_login_count = 0,
		    locked_until = NULL
		WHERE id = ?`, userID)
	if err != nil {
		return fmt.Errorf("recording login: %w", err)
	}
	return nil
}

// RecordLoginFailure increments the counter and locks the account once the
// tenant's threshold is reached. Returns whether the account is now locked.
func (r *Repository) RecordLoginFailure(ctx context.Context, userID int64, maxFailed, lockoutMinutes int) (locked bool, err error) {
	err = r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		var count int
		if err := tx.GetContext(ctx, &count,
			`SELECT failed_login_count FROM users WHERE id = ? FOR UPDATE`, userID); err != nil {
			return fmt.Errorf("reading failure count: %w", err)
		}
		count++

		if maxFailed > 0 && count >= maxFailed {
			locked = true
			_, err := tx.ExecContext(ctx, `
				UPDATE users
				SET failed_login_count = ?, locked_until = DATE_ADD(UTC_TIMESTAMP(3), INTERVAL ? MINUTE)
				WHERE id = ?`, count, lockoutMinutes, userID)
			return err
		}

		_, err := tx.ExecContext(ctx,
			`UPDATE users SET failed_login_count = ? WHERE id = ?`, count, userID)
		return err
	})
	return locked, err
}

func (r *Repository) Unlock(ctx context.Context, tenantID, userID int64) error {
	res, err := r.db.Primary.ExecContext(ctx, `
		UPDATE users SET failed_login_count = 0, locked_until = NULL,
		                 status = CASE WHEN status = 'LOCKED' THEN 'ACTIVE' ELSE status END
		WHERE tenant_id = ? AND id = ?`, tenantID, userID)
	if err != nil {
		return fmt.Errorf("unlocking user: %w", err)
	}
	return affected(res)
}

// SetPassword rotates the password, records history and clears the
// force-change flag in one transaction.
// RequirePasswordChange flags an account to choose a new password at next
// sign-in.
//
// Needed because SetPassword clears that flag: it is written for someone
// choosing their own password, where being made to choose again immediately
// would be nonsense. An administrator setting a known default is the opposite
// case — two people know it, so it has to be replaced on first use.
func (r *Repository) RequirePasswordChange(ctx context.Context, userID int64) error {
	if _, err := r.db.Primary.ExecContext(ctx,
		`UPDATE users SET must_change_password = 1 WHERE id = ?`, userID); err != nil {
		return fmt.Errorf("requiring password change: %w", err)
	}
	return nil
}

func (r *Repository) SetPassword(ctx context.Context, userID int64, hash string, expiryDays, historyCount int) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		var expires any
		if expiryDays > 0 {
			expires = time.Now().UTC().AddDate(0, 0, expiryDays)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE users
			SET password_hash = ?, password_algo = 'argon2id', must_change_password = 0,
			    password_changed_at = UTC_TIMESTAMP(3), password_expires_at = ?,
			    failed_login_count = 0, locked_until = NULL
			WHERE id = ?`, hash, expires, userID); err != nil {
			return fmt.Errorf("setting password: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO password_history (user_id, password_hash) VALUES (?,?)`,
			userID, hash); err != nil {
			return fmt.Errorf("recording password history: %w", err)
		}

		// Keep only the last N; anything older cannot influence reuse checks.
		if historyCount > 0 {
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM password_history
				WHERE user_id = ? AND id NOT IN (
					SELECT id FROM (
						SELECT id FROM password_history
						WHERE user_id = ? ORDER BY created_at DESC LIMIT ?
					) keep
				)`, userID, userID, historyCount); err != nil {
				return fmt.Errorf("trimming password history: %w", err)
			}
		}
		return nil
	})
}

// SetTemporaryPassword stores a temporary credential and forces a change on the
// next login. Used by bulk onboarding and by the forgot-password flow.
func (r *Repository) SetTemporaryPassword(ctx context.Context, userID int64, hash string) error {
	_, err := r.db.Primary.ExecContext(ctx, `
		UPDATE users
		SET password_hash = ?, password_algo = 'argon2id', must_change_password = 1,
		    password_changed_at = UTC_TIMESTAMP(3), password_expires_at = NULL,
		    failed_login_count = 0, locked_until = NULL
		WHERE id = ?`, hash, userID)
	if err != nil {
		return fmt.Errorf("setting temporary password: %w", err)
	}
	return nil
}

func (r *Repository) PasswordHistory(ctx context.Context, userID, limit int64) ([]string, error) {
	hashes := []string{}
	err := r.db.Primary.SelectContext(ctx, &hashes,
		`SELECT password_hash FROM password_history WHERE user_id = ? ORDER BY created_at DESC LIMIT ?`,
		userID, limit)
	if err != nil {
		return nil, fmt.Errorf("loading password history: %w", err)
	}
	return hashes, nil
}

// SetAvatarPath points a user's profile picture at a stored document. An empty
// path clears the picture.
func (r *Repository) SetAvatarPath(ctx context.Context, tenantID, userID int64, path string) error {
	_, err := r.db.Primary.ExecContext(ctx, `
		UPDATE users SET avatar_path = ?, updated_at = UTC_TIMESTAMP(3)
		WHERE id = ? AND tenant_id = ?`, nullStr(path), userID, tenantID)
	if err != nil {
		return fmt.Errorf("setting avatar path: %w", err)
	}
	return nil
}

// AvatarDocID resolves the document behind a user's avatar, or returns an empty
// string when they have none. The public id is unique across tenants, so the
// avatar can be served to any signed-out browser that only knows the id.
func (r *Repository) AvatarDocID(ctx context.Context, userPublicID string) (string, error) {
	var path sql.NullString
	// `Read()`, not `Replica` directly: the field is nil whenever no read
	// replica is configured — which is every development machine and any
	// single-node deployment — and dereferencing it panicked the whole request.
	// That panic was the entire "change profile picture" bug: the upload
	// succeeded, then died on the way to clearing the previous file.
	err := r.db.Read().GetContext(ctx, &path,
		`SELECT avatar_path FROM users WHERE public_id = ? AND deleted_at IS NULL`, userPublicID)
	if err != nil {
		return "", fmt.Errorf("resolving avatar: %w", err)
	}
	return path.String, nil
}

// --- helpers ----------------------------------------------------------------

func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func affected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading affected rows: %w", err)
	}
	if n == 0 {
		return platform.ErrSentinelNotFound
	}
	return nil
}

// PermissionsForRoles returns every role's permission keys in one query.
//
// The Roles screen shows the matrix for whichever role is selected and lets the
// reader move between them, so it needs all of them up front. Asking per role
// turned opening that screen into one request per role.
func (r *Repository) PermissionsForRoles(ctx context.Context) (map[int64][]string, error) {
	rows := []struct {
		RoleID int64  `db:"role_id"`
		Key    string `db:"permission_key"`
	}{}
	if err := r.db.Primary.SelectContext(ctx, &rows,
		`SELECT role_id, permission_key FROM role_permissions ORDER BY role_id, permission_key`); err != nil {
		return nil, fmt.Errorf("listing role permissions: %w", err)
	}

	out := make(map[int64][]string)
	for _, row := range rows {
		out[row.RoleID] = append(out[row.RoleID], row.Key)
	}
	return out, nil
}

// RoleUserCounts returns how many people hold each role, so the roles list can
// say what a change would affect before it is made.
func (r *Repository) RoleUserCounts(ctx context.Context) (map[int64]int64, error) {
	rows := []struct {
		RoleID int64 `db:"role_id"`
		N      int64 `db:"n"`
	}{}
	if err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT ur.role_id, COUNT(DISTINCT ur.user_id) AS n
		FROM user_roles ur
		JOIN users u ON u.id = ur.user_id AND u.deleted_at IS NULL
		GROUP BY ur.role_id`); err != nil {
		return nil, fmt.Errorf("counting role holders: %w", err)
	}

	out := make(map[int64]int64, len(rows))
	for _, row := range rows {
		out[row.RoleID] = row.N
	}
	return out, nil
}

// --- custom roles -----------------------------------------------------------

// RoleParams is a client's own role, sitting alongside the system ones.
type RoleParams struct {
	Key         string
	Name        string
	Description string
	Portal      string
}

// CreateRole adds a role owned by one client.
//
// System roles carry a NULL tenant and are never created here: the product's
// own rules reference them by key, so they ship with the release. A custom role
// is always tenant-scoped, which is what keeps one client's role from appearing
// in another's list.
func (r *Repository) CreateRole(ctx context.Context, tenantID int64, p RoleParams) (*Role, error) {
	res, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO roles (public_id, tenant_id, role_key, name, description, portal, is_system, is_deprecated)
		VALUES (?,?,?,?,?,?,0,0)`,
		platform.NewULID(), tenantID, strings.ToUpper(strings.TrimSpace(p.Key)),
		p.Name, nullIfBlank(p.Description), p.Portal)
	if err != nil {
		if platform.IsDuplicate(err) {
			return nil, platform.ErrSentinelConflict
		}
		return nil, fmt.Errorf("creating role: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading role id: %w", err)
	}
	return r.RoleByID(ctx, id)
}

func (r *Repository) RoleByID(ctx context.Context, id int64) (*Role, error) {
	var role Role
	err := r.db.Primary.GetContext(ctx, &role, `
		SELECT id, public_id, tenant_id, role_key, name, description, portal, is_system, created_at, updated_at
		FROM roles WHERE id = ?`, id)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading role: %w", err)
	}
	return &role, nil
}

// RoleUpdate is a partial edit. The key and portal are fixed once the role
// exists: both are referenced by issued tokens and by the portal binding at
// login, so changing either would lock out everyone holding the role.
type RoleUpdate struct {
	Name        *string
	Description *string
}

func (r *Repository) UpdateRole(ctx context.Context, id int64, u RoleUpdate) error {
	set, args := []string{}, []any{}
	if u.Name != nil {
		set, args = append(set, "name = ?"), append(args, *u.Name)
	}
	if u.Description != nil {
		set, args = append(set, "description = ?"), append(args, nullIfBlank(*u.Description))
	}
	if len(set) == 0 {
		return nil
	}
	args = append(args, id)

	// `is_system = 0` in the WHERE rather than a prior read: it makes the guard
	// part of the write, so a concurrent change cannot slip past a check.
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE roles SET `+strings.Join(set, ", ")+` WHERE id = ? AND is_system = 0`, args...)
	if err != nil {
		return fmt.Errorf("updating role: %w", err)
	}
	return affectedRows(res)
}

// DeleteRole removes a custom role.
//
// Refuses while anybody still holds it: deleting it would leave those users
// with no role and therefore no access, silently. The caller is told to reassign
// them first.
func (r *Repository) DeleteRole(ctx context.Context, id int64) error {
	var holders int64
	if err := r.db.Primary.GetContext(ctx, &holders,
		`SELECT COUNT(*) FROM user_roles ur JOIN users u ON u.id = ur.user_id
		 WHERE ur.role_id = ? AND u.deleted_at IS NULL`, id); err != nil {
		return fmt.Errorf("counting role holders: %w", err)
	}
	if holders > 0 {
		return platform.ErrSentinelConflict
	}

	res, err := r.db.Primary.ExecContext(ctx,
		`DELETE FROM roles WHERE id = ? AND is_system = 0`, id)
	if err != nil {
		return fmt.Errorf("deleting role: %w", err)
	}
	return affectedRows(res)
}

func affectedRows(res interface{ RowsAffected() (int64, error) }) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading affected rows: %w", err)
	}
	if n == 0 {
		return platform.ErrSentinelNotFound
	}
	return nil
}

// ClientOption is a client a staff user may create people in.
type ClientOption struct {
	PublicID string `db:"public_id"   json:"id"`
	Slug     string `db:"slug"        json:"slug"`
	Code     string `db:"client_code" json:"code"`
	Name     string `db:"name"        json:"name"`
	// IsPlatform marks ComplyDesk's own workspace. It belongs in the list —
	// agents are created there, not against a client — but nothing else can be
	// filed against it: there is no ticket to raise for it and no department it
	// needs. Callers that want only real clients filter on this, which they
	// could not do while it was absent from the response.
	IsPlatform bool `db:"is_platform" json:"is_platform"`
}

// ClientsInReach lists the clients a caller may file a new record against.
//
// The Add-user dialog needs this because a person always belongs to exactly one
// client, and staff working across clients have not necessarily chosen one from
// the switcher. Offering only what is in reach means the picker cannot name a
// client the caller could not then write to.
func (r *Repository) ClientsInReach(ctx context.Context, reach appctx.ClientReach) ([]ClientOption, error) {
	// ComplyDesk's own workspace holds staff accounts, and agents are created
	// there rather than against a client — so it stays in the list for a caller
	// whose reach covers everything.
	where := []string{"deleted_at IS NULL", "status <> 'ARCHIVED'"}
	args := []any{}

	switch {
	case reach.All:
	case len(reach.TenantIDs) > 0:
		where = append(where, "id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(reach.TenantIDs)...)
	default:
		where = append(where, "1 = 0")
	}

	rows := []ClientOption{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT public_id, slug, COALESCE(client_code, '') AS client_code, name, is_platform
		FROM tenants WHERE `+strings.Join(where, " AND ")+`
		ORDER BY is_platform DESC, name`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing clients in reach: %w", err)
	}
	return rows, nil
}

// ResolveClientInReach turns a client reference — public id, slug or code —
// into the internal tenant id, refusing anything outside the caller's reach.
func (r *Repository) ResolveClientInReach(ctx context.Context, reach appctx.ClientReach, ref string) (int64, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, platform.ErrSentinelNotFound
	}

	where := []string{"deleted_at IS NULL", "(public_id = ? OR slug = ? OR client_code = ?)"}
	args := []any{ref, strings.ToLower(ref), strings.ToUpper(ref)}

	switch {
	case reach.All:
	case len(reach.TenantIDs) > 0:
		where = append(where, "id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(reach.TenantIDs)...)
	default:
		where = append(where, "1 = 0")
	}

	var id int64
	err := r.db.Primary.GetContext(ctx, &id,
		`SELECT id FROM tenants WHERE `+strings.Join(where, " AND ")+` LIMIT 1`, args...)
	if err != nil {
		if platform.IsNotFound(err) {
			return 0, platform.ErrSentinelNotFound
		}
		return 0, fmt.Errorf("resolving client: %w", err)
	}
	return id, nil
}

// EmployeePANTaken reports whether another employee of this client already
// holds this PAN.
//
// Scoped to employees on purpose. One person may legitimately hold both an
// agent account and a partner account — the same human, two roles — so a shared
// PAN there is correct. Two *employees* of one client sharing a PAN is a data
// entry error that would later merge two people's statutory records.
func (r *Repository) EmployeePANTaken(ctx context.Context, tenantID int64, pan string, excludeUserID int64) (bool, error) {
	pan = strings.ToUpper(strings.TrimSpace(pan))
	if pan == "" {
		return false, nil
	}

	var n int64
	err := r.db.Primary.GetContext(ctx, &n, `
		SELECT COUNT(*) FROM users u
		WHERE u.tenant_id = ? AND u.pan_number = ? AND u.deleted_at IS NULL
		  AND u.id <> ?
		  AND EXISTS (
		      SELECT 1 FROM user_roles ur JOIN roles ro ON ro.id = ur.role_id
		      WHERE ur.user_id = u.id AND ro.portal = 'user')`,
		tenantID, pan, excludeUserID)
	if err != nil {
		return false, fmt.Errorf("checking employee PAN: %w", err)
	}
	return n > 0, nil
}
