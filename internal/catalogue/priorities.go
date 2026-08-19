package catalogue

// Ticket priorities as data rather than a compiled-in enum.
//
// A client can add a level — "Statutory deadline", "Board escalation" — without
// a release, which is the same reason categories and custom fields are rows.
//
// Two scopes share one table. A row with no tenant is a platform level every
// client inherits; a row with one is that client's own addition. `List` returns
// the union with the client's own winning on a key collision, so a client can
// rename "Medium" to "Normal" for themselves without affecting anyone else.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

type Priority struct {
	ID          int64          `db:"id"           json:"-"`
	PublicID    string         `db:"public_id"    json:"id"`
	TenantID    sql.NullInt64  `db:"tenant_id"    json:"-"`
	Key         string         `db:"priority_key" json:"key"`
	Name        string         `db:"name"         json:"name"`
	Description sql.NullString `db:"description"  json:"-"`
	Weight      int            `db:"weight"       json:"weight"`
	Colour      sql.NullString `db:"colour"       json:"-"`
	IsDefault   bool           `db:"is_default"   json:"is_default"`
	IsActive    bool           `db:"is_active"    json:"is_active"`
	IsSystem    bool           `db:"is_system"    json:"is_system"`
}

const priorityColumns = `id, public_id, tenant_id, priority_key, name, description,
	weight, colour, is_default, is_active, is_system`

// List returns the levels this client may raise a ticket at, highest first.
//
// The client's own rows shadow the platform's by key, which is what lets a
// client rename or retire an inherited level without touching the shared row.
func (r *Repository) Priorities(ctx context.Context, tenantID int64, includeInactive bool) ([]Priority, error) {
	where := []string{"deleted_at IS NULL", "(tenant_id IS NULL OR tenant_id = ?)"}
	args := []any{tenantID}
	if !includeInactive {
		where = append(where, "is_active = 1")
	}

	rows := []Priority{}
	if err := r.db.Primary.SelectContext(ctx, &rows,
		`SELECT `+priorityColumns+` FROM ticket_priorities
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY weight DESC, name`, args...); err != nil {
		return nil, fmt.Errorf("listing priorities: %w", err)
	}

	// Collapse the two scopes. Ordered by weight already, so the shadowing pass
	// preserves that order rather than re-sorting.
	seen := make(map[string]int, len(rows))
	out := make([]Priority, 0, len(rows))
	for _, p := range rows {
		if at, ok := seen[p.Key]; ok {
			// A client's own row replaces the platform one it shadows.
			if p.TenantID.Valid {
				out[at] = p
			}
			continue
		}
		seen[p.Key] = len(out)
		out = append(out, p)
	}
	return out, nil
}

// PriorityByKey resolves what a caller sent. Returns nil when the key is not a
// level this client has, which is how the ticket endpoints refuse one.
func (r *Repository) PriorityByKey(ctx context.Context, tenantID int64, key string) (*Priority, error) {
	list, err := r.Priorities(ctx, tenantID, false)
	if err != nil {
		return nil, err
	}
	want := strings.ToUpper(strings.TrimSpace(key))
	for i := range list {
		if list[i].Key == want {
			return &list[i], nil
		}
	}
	return nil, nil
}

// DefaultPriority is what a ticket is raised at when nobody chooses. Falls back
// to the middle of the list rather than erroring: a client that deactivated
// every default still has to be able to raise a ticket.
func (r *Repository) DefaultPriority(ctx context.Context, tenantID int64) (string, error) {
	list, err := r.Priorities(ctx, tenantID, false)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "MEDIUM", nil
	}
	for i := range list {
		if list[i].IsDefault {
			return list[i].Key, nil
		}
	}
	return list[len(list)/2].Key, nil
}

// PriorityByPublicID loads one for edit or delete, refusing a row that belongs
// to another client.
func (r *Repository) PriorityByPublicID(ctx context.Context, tenantID int64, publicID string) (*Priority, error) {
	var p Priority
	err := r.db.Primary.GetContext(ctx, &p,
		`SELECT `+priorityColumns+` FROM ticket_priorities
		  WHERE public_id = ? AND deleted_at IS NULL
		    AND (tenant_id IS NULL OR tenant_id = ?)`, publicID, tenantID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading priority: %w", err)
	}
	return &p, nil
}

type PriorityParams struct {
	Key         string
	Name        string
	Description string
	Weight      int
	Colour      string
	IsDefault   bool
	IsActive    bool
}

func (r *Repository) CreatePriority(ctx context.Context, tenantID int64, p PriorityParams) (*Priority, error) {
	publicID := platform.NewULID()

	err := r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		// Exactly one default per client, so the ticket form never has to guess
		// between two.
		if p.IsDefault {
			if _, err := tx.ExecContext(ctx,
				`UPDATE ticket_priorities SET is_default = 0 WHERE tenant_id = ?`, tenantID); err != nil {
				return fmt.Errorf("clearing the previous default: %w", err)
			}
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO ticket_priorities
				(public_id, tenant_id, priority_key, name, description, weight, colour,
				 is_default, is_active, is_system)
			VALUES (?,?,?,?,?,?,?,?,?,0)`,
			publicID, tenantID, strings.ToUpper(p.Key), p.Name, nullIfEmptyStr(p.Description),
			p.Weight, nullIfEmptyStr(p.Colour), p.IsDefault, p.IsActive)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("creating priority: %w", err)
	}
	return r.PriorityByPublicID(ctx, tenantID, publicID)
}

func (r *Repository) UpdatePriority(ctx context.Context, tenantID, id int64, publicID string, p PriorityParams) (*Priority, error) {
	err := r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if p.IsDefault {
			if _, err := tx.ExecContext(ctx,
				`UPDATE ticket_priorities SET is_default = 0 WHERE tenant_id = ?`, tenantID); err != nil {
				return fmt.Errorf("clearing the previous default: %w", err)
			}
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE ticket_priorities
			   SET name = ?, description = ?, weight = ?, colour = ?,
			       is_default = ?, is_active = ?
			 WHERE id = ?`,
			p.Name, nullIfEmptyStr(p.Description), p.Weight, nullIfEmptyStr(p.Colour),
			p.IsDefault, p.IsActive, id)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("updating priority: %w", err)
	}
	return r.PriorityByPublicID(ctx, tenantID, publicID)
}

// DeletePriority soft-deletes. Refused for a system level and for one still in
// use, because `tickets.priority` stores the key: removing the row would leave
// those tickets naming a level nothing can describe.
func (r *Repository) DeletePriority(ctx context.Context, tenantID, id int64, key string) error {
	var inUse bool
	if err := r.db.Primary.GetContext(ctx, &inUse,
		`SELECT EXISTS (SELECT 1 FROM tickets WHERE tenant_id = ? AND priority = ?)`,
		tenantID, key); err != nil {
		return fmt.Errorf("checking priority use: %w", err)
	}
	if inUse {
		return ErrPriorityInUse
	}

	_, err := r.db.Primary.ExecContext(ctx,
		`UPDATE ticket_priorities SET deleted_at = UTC_TIMESTAMP(3) WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting priority: %w", err)
	}
	return nil
}

// ErrPriorityInUse is returned rather than a generic conflict so the handler can
// explain what to do instead.
var ErrPriorityInUse = fmt.Errorf("priority is in use")

func nullIfEmptyStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
