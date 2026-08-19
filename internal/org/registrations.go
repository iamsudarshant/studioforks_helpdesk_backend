package org

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

const registrationColumns = `r.id, r.public_id, r.tenant_id, r.entity_id, r.category_id,
	r.registration_number, r.registered_on, r.valid_until, r.notes, r.is_active,
	r.created_by, r.created_at, r.updated_at`

const registrationJoins = `
	FROM entity_registrations r
	JOIN entities e   ON e.id = r.entity_id
	JOIN categories c ON c.id = r.category_id`

const registrationDisplay = `, c.category_key AS category_key, c.name AS category_name,
	e.code AS entity_code, e.name AS entity_name`

// EntitiesForCategory returns the entities a ticket may be raised against for a
// given category — the lookup behind "on selection of PF or ESI show their
// respected entities".
//
// An entity qualifies when it is active, not opted out, and holds an active
// registration for that category. `scopeIDs` further narrows the list to the
// caller's assigned entities; nil means unrestricted, and an empty non-nil slice
// correctly returns nothing.
func (r *Repository) EntitiesForCategory(ctx context.Context, tenantID, categoryID int64, scopeIDs []int64) ([]EntityWithRegistration, error) {
	where := []string{
		"e.tenant_id = ?",
		"e.deleted_at IS NULL",
		"e.is_active = 1",
		"r.category_id = ?",
		"r.is_active = 1",
		// A registration that has lapsed must not be offered.
		"(r.valid_until IS NULL OR r.valid_until >= CURDATE())",
	}
	args := []any{tenantID, categoryID}

	if scopeIDs != nil {
		if len(scopeIDs) == 0 {
			return []EntityWithRegistration{}, nil
		}
		where = append(where, "e.id IN ("+platform.Placeholders(len(scopeIDs))+")")
		args = append(args, platform.Int64Args(scopeIDs)...)
	}

	rows := []EntityWithRegistration{}
	q := `SELECT e.id, e.public_id, e.code, e.name, e.type,
	             r.registration_number, r.registered_on, r.valid_until, 1 AS is_registered
	      FROM entity_registrations r
	      JOIN entities e ON e.id = r.entity_id
	      WHERE ` + strings.Join(where, " AND ") + `
	      ORDER BY e.name`

	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("listing entities for category: %w", err)
	}
	return rows, nil
}

// EntitiesForClient lists a client's active entities, registration or not.
//
// The fallback behind EntitiesForCategory. Filtering to registered entities is
// right when registrations exist — it is what makes picking PF offer only the
// entities holding an EPFO code — but when a client has recorded none, an
// inner join answers "nothing", and the form presented an empty, disabled
// dropdown above a message telling the requester to go and ask an
// administrator. That is a dead end on the one screen a requester actually
// needs, over a data-entry gap they cannot fix and did not cause.
//
// Offering every entity instead keeps the ticket raisable. Which of them is
// registered is still reported, so the distinction survives where it is useful
// — as information on the option, rather than as a barrier.
func (r *Repository) EntitiesForClient(ctx context.Context, tenantID int64, scopeIDs []int64) ([]EntityWithRegistration, error) {
	where := []string{"e.tenant_id = ?", "e.deleted_at IS NULL", "e.is_active = 1"}
	args := []any{tenantID}

	if scopeIDs != nil {
		if len(scopeIDs) == 0 {
			return []EntityWithRegistration{}, nil
		}
		where = append(where, "e.id IN ("+platform.Placeholders(len(scopeIDs))+")")
		args = append(args, platform.Int64Args(scopeIDs)...)
	}

	rows := []EntityWithRegistration{}
	q := `SELECT e.id, e.public_id, e.code, e.name, e.type,
	             NULL AS registration_number, NULL AS registered_on, NULL AS valid_until,
	             0 AS is_registered
	      FROM entities e
	      WHERE ` + strings.Join(where, " AND ") + `
	      ORDER BY e.name`

	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("listing entities for client: %w", err)
	}
	return rows, nil
}

// EntityWithRegistration is an entity plus the statutory number that makes it
// eligible for the chosen category, so the ticket form can show
// "Ampersand Mfg Pvt Ltd — MHBAN0012345" in one line.
type EntityWithRegistration struct {
	ID                 int64          `db:"id" json:"-"`
	PublicID           string         `db:"public_id" json:"id"`
	Code               string         `db:"code" json:"code"`
	Name               string         `db:"name" json:"name"`
	Type               sql.NullString `db:"type" json:"-"`
	RegistrationNumber sql.NullString `db:"registration_number" json:"registration_number"`
	RegisteredOn       sql.NullTime   `db:"registered_on" json:"-"`
	ValidUntil         sql.NullTime   `db:"valid_until" json:"-"`
	// IsRegistered separates an entity holding a live registration for the
	// chosen category from one merely offered so the form is usable.
	IsRegistered bool `db:"is_registered" json:"is_registered"`
}

// RegistrationsFor lists every category an entity is registered for.
func (r *Repository) RegistrationsFor(ctx context.Context, tenantID, entityID int64) ([]Registration, error) {
	rows := []Registration{}
	q := `SELECT ` + registrationColumns + registrationDisplay + registrationJoins + `
	      WHERE r.tenant_id = ? AND r.entity_id = ?
	      ORDER BY c.sort_order, c.name`

	if err := r.db.Primary.SelectContext(ctx, &rows, q, tenantID, entityID); err != nil {
		return nil, fmt.Errorf("listing entity registrations: %w", err)
	}
	return rows, nil
}

// RegistrationsForCategory lists every entity registered for one category.
func (r *Repository) RegistrationsForCategory(ctx context.Context, tenantID, categoryID int64) ([]Registration, error) {
	rows := []Registration{}
	q := `SELECT ` + registrationColumns + registrationDisplay + registrationJoins + `
	      WHERE r.tenant_id = ? AND r.category_id = ? AND e.deleted_at IS NULL
	      ORDER BY e.name`

	if err := r.db.Primary.SelectContext(ctx, &rows, q, tenantID, categoryID); err != nil {
		return nil, fmt.Errorf("listing category registrations: %w", err)
	}
	return rows, nil
}

type RegistrationParams struct {
	EntityID           int64
	CategoryID         int64
	RegistrationNumber string
	RegisteredOn       *time.Time
	ValidUntil         *time.Time
	Notes              string
	IsActive           bool
	CreatedBy          *int64
}

// UpsertRegistration creates or updates the registration for one
// (entity, category) pair. Upsert rather than insert because the natural user
// action is "this entity is registered for PF, here is the code" — repeating it
// should correct the number, not fail.
func (r *Repository) UpsertRegistration(ctx context.Context, tenantID int64, p RegistrationParams) (*Registration, error) {
	// Both sides must belong to the caller's client, or a registration could
	// link an entity to another client's category.
	if err := r.assertBelongs(ctx, tenantID, "entities", p.EntityID); err != nil {
		return nil, err
	}
	if err := r.assertBelongs(ctx, tenantID, "categories", p.CategoryID); err != nil {
		return nil, err
	}

	_, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO entity_registrations
			(public_id, tenant_id, entity_id, category_id, registration_number,
			 registered_on, valid_until, notes, is_active, created_by)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			registration_number = VALUES(registration_number),
			registered_on       = VALUES(registered_on),
			valid_until         = VALUES(valid_until),
			notes               = VALUES(notes),
			is_active           = VALUES(is_active)`,
		platform.NewULID(), tenantID, p.EntityID, p.CategoryID,
		nullStr(p.RegistrationNumber), p.RegisteredOn, p.ValidUntil,
		nullStr(p.Notes), p.IsActive, p.CreatedBy)
	if err != nil {
		return nil, fmt.Errorf("saving entity registration: %w", err)
	}

	var reg Registration
	q := `SELECT ` + registrationColumns + registrationDisplay + registrationJoins + `
	      WHERE r.tenant_id = ? AND r.entity_id = ? AND r.category_id = ?`
	if err := r.db.Primary.GetContext(ctx, &reg, q, tenantID, p.EntityID, p.CategoryID); err != nil {
		return nil, fmt.Errorf("reloading entity registration: %w", err)
	}
	return &reg, nil
}

// DeleteRegistration removes an entity from a category.
func (r *Repository) DeleteRegistration(ctx context.Context, tenantID, entityID, categoryID int64) error {
	res, err := r.db.Primary.ExecContext(ctx,
		`DELETE FROM entity_registrations WHERE tenant_id = ? AND entity_id = ? AND category_id = ?`,
		tenantID, entityID, categoryID)
	if err != nil {
		return fmt.Errorf("deleting entity registration: %w", err)
	}
	return affected(res)
}

// assertBelongs guards a cross-client reference. The table name comes from a
// fixed internal call site, never from user input.
func (r *Repository) assertBelongs(ctx context.Context, tenantID int64, table string, id int64) error {
	switch table {
	case "entities", "categories":
	default:
		return fmt.Errorf("unsupported table %q", table)
	}

	var count int
	err := r.db.Primary.GetContext(ctx, &count,
		`SELECT COUNT(*) FROM `+table+` WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
		tenantID, id)
	if err != nil {
		return fmt.Errorf("verifying %s ownership: %w", table, err)
	}
	if count == 0 {
		return platform.ErrSentinelNotFound
	}
	return nil
}

// --- default entity templates ----------------------------------------------

func (r *Repository) Templates(ctx context.Context, activeOnly bool) ([]Template, error) {
	where := "1 = 1"
	if activeOnly {
		where = "is_active = 1"
	}

	rows := []Template{}
	q := `SELECT id, public_id, template_key, name, description, entity_type,
	             default_categories_json, is_active, sort_order, created_at, updated_at
	      FROM entity_templates WHERE ` + where + ` ORDER BY sort_order, name`

	if err := r.db.Primary.SelectContext(ctx, &rows, q); err != nil {
		return nil, fmt.Errorf("listing entity templates: %w", err)
	}
	return rows, nil
}

// ApplyTemplates creates the default entities for a client, and registers each
// against the categories its template names. Already-present templates are
// skipped, so re-running never duplicates and never resurrects an entity the
// client deliberately opted out of.
func (r *Repository) ApplyTemplates(ctx context.Context, tenantID int64, entityCodePrefix string) (created int, err error) {
	templates, err := r.Templates(ctx, true)
	if err != nil {
		return 0, err
	}

	err = r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		for _, tpl := range templates {
			var existing int
			if err := tx.GetContext(ctx, &existing,
				`SELECT COUNT(*) FROM entities WHERE tenant_id = ? AND template_key = ?`,
				tenantID, tpl.Key); err != nil {
				return fmt.Errorf("checking template %s: %w", tpl.Key, err)
			}
			if existing > 0 {
				continue // already applied, or opted out — either way, leave it alone
			}

			code := strings.ToUpper(entityCodePrefix + "-" + tpl.Key)
			res, err := tx.ExecContext(ctx, `
				INSERT INTO entities
					(public_id, tenant_id, code, name, type, template_key, is_default, is_active)
				VALUES (?,?,?,?,?,?,1,1)`,
				platform.NewULID(), tenantID, code, tpl.Name,
				nullStr(tpl.EntityType.String), tpl.Key)
			if err != nil {
				if platform.IsDuplicate(err) {
					continue // a hand-made entity already owns that code
				}
				return fmt.Errorf("creating entity from template %s: %w", tpl.Key, err)
			}

			entityID, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("reading entity id: %w", err)
			}
			created++

			if !tpl.DefaultCategoriesJSON.Valid {
				continue
			}
			var categoryKeys []string
			if err := json.Unmarshal([]byte(tpl.DefaultCategoriesJSON.String), &categoryKeys); err != nil {
				return fmt.Errorf("decoding template categories for %s: %w", tpl.Key, err)
			}

			for _, key := range categoryKeys {
				var categoryID int64
				err := tx.GetContext(ctx, &categoryID,
					`SELECT id FROM categories WHERE tenant_id = ? AND category_key = ? AND deleted_at IS NULL`,
					tenantID, key)
				if err != nil {
					// The category may not exist yet — categories are configured
					// later in onboarding. Registrations can be added then.
					continue
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO entity_registrations
						(public_id, tenant_id, entity_id, category_id, is_active)
					VALUES (?,?,?,?,1)
					ON DUPLICATE KEY UPDATE is_active = VALUES(is_active)`,
					platform.NewULID(), tenantID, entityID, categoryID); err != nil {
					return fmt.Errorf("registering entity for %s: %w", key, err)
				}
			}
		}
		return nil
	})

	return created, err
}

// SetOptedOut switches a default entity off (or back on) for a client. Opting
// out is deliberately not a delete: the entity keeps its history and can be
// re-enabled, and re-running the templates will not recreate it.
func (r *Repository) SetOptedOut(ctx context.Context, tenantID, entityID int64, optedOut bool, actorID *int64) error {
	var res sql.Result
	var err error

	if optedOut {
		res, err = r.db.Primary.ExecContext(ctx, `
			UPDATE entities
			SET is_active = 0, opted_out_at = UTC_TIMESTAMP(3), opted_out_by = ?
			WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, actorID, tenantID, entityID)
	} else {
		res, err = r.db.Primary.ExecContext(ctx, `
			UPDATE entities
			SET is_active = 1, opted_out_at = NULL, opted_out_by = NULL
			WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, tenantID, entityID)
	}
	if err != nil {
		return fmt.Errorf("setting entity opt-out: %w", err)
	}
	return affected(res)
}

// EntityReferences counts the records that still point at an entity. A purge is
// refused while either is non-zero, so erasing an entity can never orphan a
// ticket's history or a user's profile.
func (r *Repository) EntityReferences(ctx context.Context, tenantID, entityID int64) (tickets, users int64, err error) {
	if err = r.db.Primary.GetContext(ctx, &tickets,
		`SELECT COUNT(*) FROM tickets WHERE tenant_id = ? AND entity_id = ?`,
		tenantID, entityID); err != nil {
		return 0, 0, fmt.Errorf("counting tickets for entity: %w", err)
	}
	if err = r.db.Primary.GetContext(ctx, &users,
		`SELECT COUNT(*) FROM users WHERE tenant_id = ? AND entity_id = ? AND deleted_at IS NULL`,
		tenantID, entityID); err != nil {
		return 0, 0, fmt.Errorf("counting users for entity: %w", err)
	}
	return tickets, users, nil
}

// PurgeEntity erases the row for good, along with its registrations. Callers
// must check EntityReferences first.
func (r *Repository) PurgeEntity(ctx context.Context, tenantID, entityID int64) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM entity_registrations WHERE tenant_id = ? AND entity_id = ?`,
			tenantID, entityID); err != nil {
			return fmt.Errorf("removing entity registrations: %w", err)
		}
		// Sites keep their own identity; unlink rather than cascade-delete them.
		if _, err := tx.ExecContext(ctx,
			`UPDATE sites SET entity_id = NULL WHERE tenant_id = ? AND entity_id = ?`,
			tenantID, entityID); err != nil {
			return fmt.Errorf("unlinking sites: %w", err)
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM entities WHERE tenant_id = ? AND id = ?`, tenantID, entityID)
		if err != nil {
			return fmt.Errorf("erasing entity: %w", err)
		}
		return affected(res)
	})
}
