package cli

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/user"
)

// KarmaTenantSlug is the platform tenant that holds Karma's internal staff.
// Client data never lives here; it exists so internal users have a home tenant
// without making users.tenant_id nullable.
const KarmaTenantSlug = "karma"

// seedCanonicalRoles installs the five specified roles and rewrites the previous
// eight as deprecated aliases pointing at them.
//
// Aliasing rather than deleting is what keeps the change backward compatible: an
// access token, saved filter or integration still naming HELPDESK_ADMIN keeps
// resolving, and the permission resolver follows alias_of to the canonical set.
func seedCanonicalRoles(ctx context.Context, db *platform.DB) error {
	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		for _, role := range canonicalRoles {
			var id int64
			err := tx.GetContext(ctx, &id,
				`SELECT id FROM roles WHERE tenant_id IS NULL AND role_key = ?`, role.Key)

			if err != nil {
				if !platform.IsNotFound(err) {
					return fmt.Errorf("checking role %s: %w", role.Key, err)
				}
				res, insErr := tx.ExecContext(ctx, `
					INSERT INTO roles
						(public_id, tenant_id, role_key, name, description, portal, is_system, is_deprecated)
					VALUES (?, NULL, ?, ?, ?, ?, 1, 0)`,
					platform.NewULID(), role.Key, role.Name, role.Description, role.Portal)
				if insErr != nil {
					return fmt.Errorf("creating role %s: %w", role.Key, insErr)
				}
				if id, err = res.LastInsertId(); err != nil {
					return fmt.Errorf("reading role id: %w", err)
				}
			} else if _, err := tx.ExecContext(ctx,
				`UPDATE roles SET name = ?, description = ?, portal = ?, is_deprecated = 0, alias_of = NULL
				 WHERE id = ?`,
				role.Name, role.Description, role.Portal, id); err != nil {
				return fmt.Errorf("updating role %s: %w", role.Key, err)
			}

			if _, err := tx.ExecContext(ctx,
				`DELETE FROM role_permissions WHERE role_id = ?`, id); err != nil {
				return fmt.Errorf("clearing permissions for %s: %w", role.Key, err)
			}
			for _, perm := range role.Permissions {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO role_permissions (role_id, permission_key) VALUES (?,?)`,
					id, perm); err != nil {
					return fmt.Errorf("granting %s to %s: %w", perm, role.Key, err)
				}
			}
		}

		// Point each legacy role at its canonical replacement and mirror the
		// permission set, so a user still holding the old role behaves the same.
		for legacy, canonical := range roleAliases {
			var legacyID, canonicalID int64
			if err := tx.GetContext(ctx, &legacyID,
				`SELECT id FROM roles WHERE tenant_id IS NULL AND role_key = ?`, legacy); err != nil {
				if platform.IsNotFound(err) {
					continue // never seeded in this installation
				}
				return fmt.Errorf("locating legacy role %s: %w", legacy, err)
			}
			if err := tx.GetContext(ctx, &canonicalID,
				`SELECT id FROM roles WHERE tenant_id IS NULL AND role_key = ?`, canonical); err != nil {
				return fmt.Errorf("locating canonical role %s: %w", canonical, err)
			}

			if _, err := tx.ExecContext(ctx,
				`UPDATE roles SET alias_of = ?, is_deprecated = 1 WHERE id = ?`,
				canonical, legacyID); err != nil {
				return fmt.Errorf("aliasing %s: %w", legacy, err)
			}

			if _, err := tx.ExecContext(ctx,
				`DELETE FROM role_permissions WHERE role_id = ?`, legacyID); err != nil {
				return fmt.Errorf("clearing permissions for %s: %w", legacy, err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO role_permissions (role_id, permission_key)
				SELECT ?, permission_key FROM role_permissions WHERE role_id = ?`,
				legacyID, canonicalID); err != nil {
				return fmt.Errorf("mirroring permissions onto %s: %w", legacy, err)
			}
		}
		return nil
	})
}

func seedModules(ctx context.Context, db *platform.DB) error {
	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		for _, m := range moduleCatalogue {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO modules
					(public_id, module_key, name, description, icon, color, is_core, sort_order, is_active)
				VALUES (?,?,?,?,?,?,?,?,1)
				ON DUPLICATE KEY UPDATE
					name = VALUES(name), description = VALUES(description),
					icon = VALUES(icon), color = VALUES(color),
					is_core = VALUES(is_core), sort_order = VALUES(sort_order)`,
				platform.NewULID(), m.Key, m.Name, nullIfEmpty(m.Description),
				nullIfEmpty(m.Icon), nullIfEmpty(m.Color), m.IsCore, m.SortOrder); err != nil {
				return fmt.Errorf("seeding module %s: %w", m.Key, err)
			}
		}
		return nil
	})
}

// relocateKarmaStaff moves internal users out of whichever client tenant they
// were created in and into the Karma platform tenant, then grants them an
// assignment back to that client so nothing they were working on disappears.
//
// Without the assignment step an agent would silently lose access to their own
// clients the moment this runs — the migration has to preserve reach, not just
// move rows.
func relocateKarmaStaff(ctx context.Context, db *platform.DB) (moved int, err error) {
	err = db.InTx(ctx, func(tx *sqlx.Tx) error {
		var karmaID int64
		if err := tx.GetContext(ctx, &karmaID,
			`SELECT id FROM tenants WHERE slug = ? AND is_platform = 1`, KarmaTenantSlug); err != nil {
			return fmt.Errorf("locating the Karma platform tenant: %w", err)
		}

		rows := []struct {
			UserID   int64  `db:"user_id"`
			TenantID int64  `db:"tenant_id"`
			RoleKey  string `db:"role_key"`
		}{}
		if err := tx.SelectContext(ctx, &rows, `
			SELECT DISTINCT u.id AS user_id, u.tenant_id, r.role_key
			FROM users u
			JOIN user_roles ur ON ur.user_id = u.id
			JOIN roles r       ON r.id = ur.role_id
			WHERE u.tenant_id <> ? AND u.deleted_at IS NULL
			  AND r.role_key IN (
			    'KARMA_SUPER_ADMIN','KARMA_AGENT',
			    'SUPER_ADMIN','HELPDESK_MASTER_ADMIN','HELPDESK_ADMIN','HELPDESK_EXECUTIVE'
			  )`, karmaID); err != nil {
			return fmt.Errorf("finding internal staff to relocate: %w", err)
		}

		for _, row := range rows {
			// Keep the agent's reach: assign them to the client they came from.
			// A super admin needs no assignment — their access is by role.
			if !user.IsSuperAdminRole(row.RoleKey) {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO agent_tenant_assignments
						(public_id, agent_user_id, tenant_id, is_primary)
					VALUES (?,?,?,1)
					ON DUPLICATE KEY UPDATE revoked_at = NULL`,
					platform.NewULID(), row.UserID, row.TenantID); err != nil {
					return fmt.Errorf("assigning agent %d to client %d: %w", row.UserID, row.TenantID, err)
				}
			}

			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET tenant_id = ? WHERE id = ?`, karmaID, row.UserID); err != nil {
				return fmt.Errorf("relocating user %d: %w", row.UserID, err)
			}
			moved++
		}
		return nil
	})

	return moved, err
}

// enableCoreModules switches on the core modules for a client and links the
// client's categories to their module.
// backfillClientModules enables the catalogue for every live client tenant and
// reports how many were touched.
//
// The platform tenant is skipped: Karma's own workspace raises no tickets and
// so has no modules of its own. Existing per-client opt-outs survive, because
// enableModulesForTenant only inserts rows that are missing.
// backfillClientQueryTypes adds any query type missing from an existing client.
//
// New entries in the §8 taxonomy are useless until clients can actually pick
// them, and a client seeded before an entry was added would never see it —
// seedSubcategories only runs when a workspace is first created. Existing rows
// are left alone, so a client that renamed or retired one keeps their change.
func backfillClientQueryTypes(ctx context.Context, db *platform.DB) (int, error) {
	ids := []int64{}
	if err := db.Primary.SelectContext(ctx, &ids, `
		SELECT id FROM tenants
		WHERE is_platform = 0 AND deleted_at IS NULL AND status <> 'ARCHIVED'`); err != nil {
		return 0, fmt.Errorf("listing client tenants: %w", err)
	}

	for _, id := range ids {
		if err := seedSubcategories(ctx, db, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

func backfillClientModules(ctx context.Context, db *platform.DB) (int, error) {
	ids := []int64{}
	if err := db.Primary.SelectContext(ctx, &ids, `
		SELECT id FROM tenants
		WHERE is_platform = 0 AND deleted_at IS NULL AND status <> 'ARCHIVED'`); err != nil {
		return 0, fmt.Errorf("listing client tenants: %w", err)
	}

	for _, id := range ids {
		if err := enableModulesForTenant(ctx, db, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

func enableModulesForTenant(ctx context.Context, db *platform.DB, tenantID int64) error {
	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		// `enabled = enabled` is a deliberate no-op: a client that opted out of a
		// module must stay opted out when the catalogue is re-seeded. Only
		// modules the client has never been offered are added.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tenant_modules (tenant_id, module_id, enabled)
			SELECT ?, id, 1 FROM modules WHERE is_active = 1
			ON DUPLICATE KEY UPDATE enabled = enabled`, tenantID); err != nil {
			return fmt.Errorf("enabling modules: %w", err)
		}

		for categoryKey, moduleKey := range categoryModule {
			if _, err := tx.ExecContext(ctx, `
				UPDATE categories c
				JOIN modules m ON m.module_key = ?
				SET c.module_id = m.id
				WHERE c.tenant_id = ? AND c.category_key = ?`,
				moduleKey, tenantID, categoryKey); err != nil {
				return fmt.Errorf("linking category %s to module %s: %w", categoryKey, moduleKey, err)
			}
		}
		return nil
	})
}

// seedSubcategories adds the second level under each seeded category. A
// subcategory inherits its parent's prefix and SLA; only the label differs.
func seedSubcategories(ctx context.Context, db *platform.DB, tenantID int64) error {
	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		for _, sub := range subcategories {
			var parent struct {
				ID       int64  `db:"id"`
				Prefix   string `db:"ticket_prefix"`
				ModuleID *int64 `db:"module_id"`
				SLAID    *int64 `db:"sla_policy_id"`
			}
			if err := tx.GetContext(ctx, &parent, `
				SELECT id, ticket_prefix, module_id, sla_policy_id
				FROM categories WHERE tenant_id = ? AND category_key = ? AND deleted_at IS NULL`,
				tenantID, sub.ParentKey); err != nil {
				continue // the parent category is not configured for this client
			}

			if _, err := tx.ExecContext(ctx, `
				INSERT INTO categories
					(public_id, tenant_id, module_id, category_key, name, parent_id, is_subcategory,
					 ticket_prefix, sla_policy_id, is_active, sort_order)
				VALUES (?,?,?,?,?,?,1,?,?,1,0)
				ON DUPLICATE KEY UPDATE
					name = VALUES(name), parent_id = VALUES(parent_id), is_subcategory = 1`,
				platform.NewULID(), tenantID, parent.ModuleID, sub.Key, sub.Name,
				parent.ID, parent.Prefix, parent.SLAID); err != nil {
				return fmt.Errorf("seeding subcategory %s: %w", sub.Key, err)
			}
		}
		return nil
	})
}
