// Package catalogue owns query categories and their custom fields — the
// data-driven part of ticket creation. A new query domain (IT, HR, Finance) is a
// row here, never a code change.
package catalogue

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/platform"
)

type Category struct {
	ID                  int64          `db:"id"`
	PublicID            string         `db:"public_id"`
	TenantID            int64          `db:"tenant_id"`
	Key                 string         `db:"category_key"`
	Name                string         `db:"name"`
	Description         sql.NullString `db:"description"`
	ModuleID            sql.NullInt64  `db:"module_id"`
	ParentID            sql.NullInt64  `db:"parent_id"`
	IsSubcategory       bool           `db:"is_subcategory"`
	TicketPrefix        string         `db:"ticket_prefix"`
	Icon                sql.NullString `db:"icon"`
	Color               sql.NullString `db:"color"`
	SLAPolicyID         sql.NullInt64  `db:"sla_policy_id"`
	DefaultDepartmentID sql.NullInt64  `db:"default_department_id"`
	RequiresFieldsJSON  sql.NullString `db:"requires_fields_json"`
	IsActive            bool           `db:"is_active"`
	SortOrder           int            `db:"sort_order"`
	CreatedAt           time.Time      `db:"created_at"`
	UpdatedAt           time.Time      `db:"updated_at"`

	// Joined for display, so the client can build the category tree and group by
	// module without a round trip per row. Populated by List only.
	ParentPublicID     sql.NullString `db:"parent_public_id"`
	DepartmentPublicID sql.NullString `db:"department_public_id"`
	ModulePublicID     sql.NullString `db:"module_public_id"`
	ModuleKey          sql.NullString `db:"module_key"`
}

type Field struct {
	ID             int64          `db:"id"`
	PublicID       string         `db:"public_id"`
	CategoryID     int64          `db:"category_id"`
	Key            string         `db:"field_key"`
	Label          string         `db:"label"`
	Type           string         `db:"field_type"`
	OptionsJSON    sql.NullString `db:"options_json"`
	IsRequired     bool           `db:"is_required"`
	ValidationJSON sql.NullString `db:"validation_json"`
	HelpText       sql.NullString `db:"help_text"`
	Placeholder    sql.NullString `db:"placeholder"`
	DefaultValue   sql.NullString `db:"default_value"`
	VisibleToJSON  sql.NullString `db:"visible_to_json"`
	EditableByJSON sql.NullString `db:"editable_by_json"`
	DependsOnJSON  sql.NullString `db:"depends_on_json"`
	SortOrder      int            `db:"sort_order"`
	IsActive       bool           `db:"is_active"`
}

type Repository struct {
	db *platform.DB
}

func NewRepository(db *platform.DB) *Repository { return &Repository{db: db} }

const categoryColumns = `id, public_id, tenant_id, module_id, category_key, name, description,
	parent_id, is_subcategory, ticket_prefix, icon, color, sla_policy_id, default_department_id,
	requires_fields_json, is_active, sort_order, created_at, updated_at`

// The same columns qualified with the `c` alias, for the joined List query.
const prefixedCategoryColumns = `c.id, c.public_id, c.tenant_id, c.module_id, c.category_key,
	c.name, c.description, c.parent_id, c.is_subcategory, c.ticket_prefix, c.icon, c.color,
	c.sla_policy_id, c.default_department_id, c.requires_fields_json, c.is_active,
	c.sort_order, c.created_at, c.updated_at`

// List returns the categories a requester may raise a ticket against. scopeIDs
// narrows to the caller's assigned categories; nil means unrestricted.
func (r *Repository) List(ctx context.Context, tenantID int64, activeOnly bool, scopeIDs []int64) ([]Category, error) {
	where := []string{"c.tenant_id = ?", "c.deleted_at IS NULL"}
	args := []any{tenantID}

	if activeOnly {
		where = append(where, "c.is_active = 1")
	}
	if scopeIDs != nil {
		if len(scopeIDs) == 0 {
			return []Category{}, nil
		}
		where = append(where, "c.id IN ("+platform.Placeholders(len(scopeIDs))+")")
		args = append(args, platform.Int64Args(scopeIDs)...)
	}

	// A category whose module the client has switched off must disappear
	// everywhere at once — including from the ticket form. A category with no
	// module is unaffected.
	where = append(where, `(c.module_id IS NULL OR EXISTS (
		SELECT 1 FROM tenant_modules tm
		WHERE tm.tenant_id = c.tenant_id AND tm.module_id = c.module_id AND tm.enabled = 1))`)

	rows := []Category{}
	q := `SELECT ` + prefixedCategoryColumns + `,
		       p.public_id AS parent_public_id,
		       dp.public_id AS department_public_id,
		       m.public_id AS module_public_id,
		       m.module_key AS module_key
		FROM categories c
		LEFT JOIN categories p   ON p.id = c.parent_id
		LEFT JOIN departments dp ON dp.id = c.default_department_id
		LEFT JOIN modules m      ON m.id = c.module_id
		WHERE ` + strings.Join(where, " AND ") + ` ORDER BY c.sort_order, c.name`

	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("listing categories: %w", err)
	}
	return rows, nil
}

func (r *Repository) ByPublicID(ctx context.Context, tenantID int64, publicID string) (*Category, error) {
	var c Category
	err := r.db.Primary.GetContext(ctx, &c,
		`SELECT `+categoryColumns+` FROM categories
		 WHERE tenant_id = ? AND public_id = ? AND deleted_at IS NULL`, tenantID, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading category: %w", err)
	}
	return &c, nil
}

// ByPublicIDInReach loads a category from any client the caller can reach.
//
// The tenant-pinned lookup above answers "is this category in the client named
// by the header?", which is the wrong question for a staff user with no client
// selected: the header then names ComplyDesk's own workspace, which holds no
// categories at all. Resolving across reach is what lets an agent open the
// ticket form for a client they support without first switching to it.
func (r *Repository) ByPublicIDInReach(ctx context.Context, reach appctx.ClientReach, publicID string) (*Category, error) {
	where := []string{"public_id = ?", "deleted_at IS NULL"}
	args := []any{publicID}

	switch {
	case reach.All:
	case len(reach.TenantIDs) > 0:
		where = append(where, "tenant_id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(reach.TenantIDs)...)
	default:
		return nil, platform.ErrSentinelNotFound
	}

	var c Category
	err := r.db.Primary.GetContext(ctx, &c,
		`SELECT `+categoryColumns+` FROM categories WHERE `+strings.Join(where, " AND ")+` LIMIT 1`, args...)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading category: %w", err)
	}
	return &c, nil
}

// Fields returns a category's custom fields in display order.
func (r *Repository) Fields(ctx context.Context, tenantID, categoryID int64) ([]Field, error) {
	rows := []Field{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT id, public_id, category_id, field_key, label, field_type, options_json,
		       is_required, validation_json, help_text, placeholder, default_value,
		       visible_to_json, editable_by_json, depends_on_json, sort_order, is_active
		FROM category_fields
		WHERE tenant_id = ? AND category_id = ? AND is_active = 1
		ORDER BY sort_order, label`, tenantID, categoryID)
	if err != nil {
		return nil, fmt.Errorf("listing category fields: %w", err)
	}
	return rows, nil
}

// FieldsInherited returns the fields a ticket form should render for a category,
// including the ones it inherits from its parent.
//
// A subcategory exists to label a query more precisely — "PF Withdrawals" rather
// than "PF Query" — not to ask a different set of questions. The seed creates
// them with a parent's prefix and SLA and no fields of their own, so asking for
// a subcategory's own fields returned an empty form: the requester picked
// "PF Withdrawals" and was asked nothing about it.
//
// The parent's fields come first, then the subcategory's own overlay them by
// key, so a subcategory can still refine a single question without restating the
// whole set.
func (r *Repository) FieldsInherited(ctx context.Context, c *Category) ([]Field, error) {
	own, err := r.Fields(ctx, c.TenantID, c.ID)
	if err != nil {
		return nil, err
	}
	if !c.ParentID.Valid {
		return own, nil
	}

	inherited, err := r.Fields(ctx, c.TenantID, c.ParentID.Int64)
	if err != nil {
		return nil, err
	}

	// Index the overrides so a parent field the subcategory redefines is
	// replaced in place rather than appearing twice.
	overrides := make(map[string]Field, len(own))
	for _, f := range own {
		overrides[f.Key] = f
	}

	out := make([]Field, 0, len(inherited)+len(own))
	for _, f := range inherited {
		if override, ok := overrides[f.Key]; ok {
			out = append(out, override)
			delete(overrides, f.Key)
			continue
		}
		out = append(out, f)
	}
	// Whatever the subcategory adds beyond the parent's set, in its own order.
	for _, f := range own {
		if _, unused := overrides[f.Key]; unused {
			out = append(out, f)
			delete(overrides, f.Key)
		}
	}
	return out, nil
}

// Transitions returns the status moves allowed from a given status, which is how
// the ticket engine stays a table-driven state machine rather than a switch.
type Transition struct {
	FromStatus      string         `db:"from_status"`
	ToStatus        string         `db:"to_status"`
	Label           sql.NullString `db:"label"`
	RequiresComment bool           `db:"requires_comment"`
	RequiresReason  bool           `db:"requires_reason_code"`
	ReasonCodesJSON sql.NullString `db:"reason_codes_json"`
}

func (r *Repository) Transitions(ctx context.Context, tenantID, categoryID int64) ([]Transition, error) {
	rows := []Transition{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT from_status, to_status, label, requires_comment, requires_reason_code, reason_codes_json
		FROM category_workflows
		WHERE tenant_id = ? AND category_id = ? AND is_active = 1
		ORDER BY from_status, to_status`, tenantID, categoryID)
	if err != nil {
		return nil, fmt.Errorf("listing workflow transitions: %w", err)
	}
	return rows, nil
}

// RawJSON decodes a nullable JSON column, returning nil rather than an error for
// absent or malformed data — a broken options blob must not take a form down.
func RawJSON(v sql.NullString) json.RawMessage {
	if !v.Valid || strings.TrimSpace(v.String) == "" {
		return nil
	}
	if !json.Valid([]byte(v.String)) {
		return nil
	}
	return json.RawMessage(v.String)
}
