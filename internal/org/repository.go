// Package org owns the client's structural records: entities, sites and
// departments. These are the dimensions user scopes and ticket routing work on.
package org

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/platform"
)

type Repository struct {
	db *platform.DB
}

func NewRepository(db *platform.DB) *Repository { return &Repository{db: db} }

// --- entities ---------------------------------------------------------------

const entityColumns = `e.id, e.public_id, e.tenant_id, e.code, e.name, e.type, e.department_id,
	e.template_key, e.is_default, e.parent_entity_id, e.address, e.registered_address,
	e.cin_number, e.gst_number, e.is_active, e.opted_out_at, e.opted_out_by,
	e.created_at, e.updated_at,
	d.public_id AS department_public_id, d.name AS department_name,
	tn.name AS client_name, tn.slug AS client_slug, COALESCE(tn.client_code, '') AS client_code`

// entityFrom joins the department, so every entity row carries the display name
// and public id of its statutory line without a second query, and the client so
// a cross-client list can say which workspace each row belongs to.
const entityFrom = ` FROM entities e
	LEFT JOIN departments d ON d.id = e.department_id
	JOIN tenants tn         ON tn.id = e.tenant_id `

// OrgSortable whitelists the columns the organisation lists may be ordered by,
// keyed by the grid's own column field names.
//
// The values are *unqualified* on purpose: each list joins its own table under
// a different alias, so the alias is added by orderBy. An unqualified `name`
// would be ambiguous — entities, departments and tenants all have one — and
// MySQL would either error or silently pick the wrong table.
var OrgSortable = map[string]string{
	"name":       "name",
	"code":       "code",
	"type":       "type",
	"is_active":  "is_active",
	"city":       "city",
	"state":      "state",
	"created_at": "created_at",
	// These two are select aliases rather than columns of the joined table, so
	// they are never prefixed.
	"client":          "@client_name",
	"department_name": "@department_name",
	"entity_count":    "@entity_count",
}

// orderBy renders a safe ORDER BY from a whitelisted sort key.
//
// `alias` is the table alias of the list being ordered — "e." for entities,
// "dp." for departments — and is prepended to a plain column. A value marked
// with a leading "@" is a select alias and is used bare.
//
// `fallback` applies when nothing was asked for or the key is unknown, so a
// list is always deterministically ordered: an unordered page is what makes
// pagination appear to repeat rows.
func orderBy(page platform.Page, alias, fallback string) string {
	if page.SortBy == "" {
		return " ORDER BY " + fallback
	}

	column := page.SortBy
	if strings.HasPrefix(column, "@") {
		column = strings.TrimPrefix(column, "@")
	} else {
		column = alias + column
	}

	dir := "ASC"
	if strings.EqualFold(page.SortDir, "DESC") {
		dir = "DESC"
	}
	return " ORDER BY " + column + " " + dir
}

// reachWhere renders the client restriction shared by every org listing.
//
// `column` is the qualified tenant column of the table being listed. A reach
// that names no client matches nothing rather than everything, so a caller with
// no client access cannot fall through to a cross-client list.
func reachWhere(column string, reach appctx.ClientReach) ([]string, []any) {
	switch {
	case reach.All:
		return nil, nil
	case len(reach.TenantIDs) > 0:
		return []string{column + " IN (" + platform.Placeholders(len(reach.TenantIDs)) + ")"},
			platform.Int64Args(reach.TenantIDs)
	default:
		return []string{"1 = 0"}, nil
	}
}

// Entities lists the establishments across the clients in reach.
//
// Staff who have not selected a client get every client's entities in one list
// — each row naming its own client — which is what the brief's "show all if no
// client is selected" means for this screen.
// OrgFilter narrows an entity, site or department listing.
//
// One struct for all three because the questions are the same — what is it
// called, is it live, which client is it under, which department does it belong
// to — and three near-identical parameter lists is how they drift apart.
//
// Every field is optional; a zero OrgFilter matches everything the caller can
// already reach.
type OrgFilter struct {
	// Query matches the name or the code, which is how people search for these:
	// half remember "PF Withdrawals" and half remember "PF-WDL".
	Query string
	// Types is the entity or department type — PF, ESIC, GENERAL and so on.
	Types []string
	// Active narrows to live or retired records. Nil means both, which is what
	// an administration screen wants; the pickers ask for live only.
	Active *bool
	// DepartmentIDs and EntityIDs narrow by parent.
	DepartmentIDs []int64
	EntityIDs     []int64
}

// apply appends the filter to a WHERE clause, prefixing columns with `alias`.
func (f OrgFilter) apply(alias string, where *[]string, args *[]any, hasType, hasDept bool) {
	if q := strings.TrimSpace(f.Query); q != "" {
		*where = append(*where, "("+alias+"name LIKE ? OR "+alias+"code LIKE ?)")
		*args = append(*args, "%"+q+"%", "%"+q+"%")
	}
	if hasType && len(f.Types) > 0 {
		*where = append(*where, alias+"type IN ("+platform.Placeholders(len(f.Types))+")")
		for _, t := range f.Types {
			*args = append(*args, t)
		}
	}
	if f.Active != nil {
		if *f.Active {
			*where = append(*where, alias+"is_active = 1")
		} else {
			*where = append(*where, alias+"is_active = 0")
		}
	}
	if hasDept && len(f.DepartmentIDs) > 0 {
		*where = append(*where, alias+"department_id IN ("+platform.Placeholders(len(f.DepartmentIDs))+")")
		*args = append(*args, platform.Int64Args(f.DepartmentIDs)...)
	}
}

func (r *Repository) Entities(ctx context.Context, reach appctx.ClientReach, activeOnly bool, ids []int64, page platform.Page, filter OrgFilter) ([]Entity, error) {
	where, args := reachWhere("e.tenant_id", reach)
	where = append(where, "e.deleted_at IS NULL")

	if activeOnly {
		where = append(where, "e.is_active = 1")
	}
	filter.apply("e.", &where, &args, true, true)
	if ids != nil {
		if len(ids) == 0 {
			return []Entity{}, nil // explicitly scoped to nothing
		}
		where = append(where, "e.id IN ("+platform.Placeholders(len(ids))+")")
		args = append(args, platform.Int64Args(ids)...)
	}

	rows := []Entity{}
	q := `SELECT ` + entityColumns + entityFrom + ` WHERE ` + strings.Join(where, " AND ") +
		orderBy(page, "e.", "tn.name, e.name")
	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("listing entities: %w", err)
	}
	return rows, nil
}

func (r *Repository) EntityByID(ctx context.Context, tenantID, id int64) (*Entity, error) {
	var e Entity
	err := r.db.Primary.GetContext(ctx, &e,
		`SELECT `+entityColumns+entityFrom+` WHERE e.tenant_id = ? AND e.id = ? AND e.deleted_at IS NULL`,
		tenantID, id)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading entity: %w", err)
	}
	return &e, nil
}

func (r *Repository) EntityByPublicID(ctx context.Context, tenantID int64, publicID string) (*Entity, error) {
	var e Entity
	err := r.db.Primary.GetContext(ctx, &e,
		`SELECT `+entityColumns+entityFrom+` WHERE e.tenant_id = ? AND e.public_id = ? AND e.deleted_at IS NULL`,
		tenantID, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading entity: %w", err)
	}
	return &e, nil
}

// --- reach-aware lookups ----------------------------------------------------
//
// A staff user working the cross-client list is looking at rows from several
// clients at once. Editing one has to resolve it without knowing which client
// it belongs to first — so these find the record anywhere in reach and return
// it with its own tenant_id, which the write then uses. The reach is still the
// boundary: a record outside it is not found, exactly as if it did not exist.

func (r *Repository) EntityInReach(ctx context.Context, reach appctx.ClientReach, publicID string) (*Entity, error) {
	where, args := reachWhere("e.tenant_id", reach)
	where = append(where, "e.public_id = ?", "e.deleted_at IS NULL")
	args = append(args, publicID)

	var e Entity
	err := r.db.Primary.GetContext(ctx, &e,
		`SELECT `+entityColumns+entityFrom+` WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading entity: %w", err)
	}
	return &e, nil
}

func (r *Repository) SiteInReach(ctx context.Context, reach appctx.ClientReach, publicID string) (*Site, error) {
	where, args := reachWhere("st.tenant_id", reach)
	where = append(where, "st.public_id = ?", "st.deleted_at IS NULL")
	args = append(args, publicID)

	var s Site
	err := r.db.Primary.GetContext(ctx, &s,
		`SELECT `+siteListColumns+` FROM sites st JOIN tenants tn ON tn.id = st.tenant_id
		 WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading site: %w", err)
	}
	return &s, nil
}

func (r *Repository) DepartmentInReach(ctx context.Context, reach appctx.ClientReach, publicID string) (*Department, error) {
	where, args := reachWhere("dp.tenant_id", reach)
	where = append(where, "dp.public_id = ?", "dp.deleted_at IS NULL")
	args = append(args, publicID)

	var d Department
	err := r.db.Primary.GetContext(ctx, &d,
		`SELECT `+departmentListColumns+` FROM departments dp JOIN tenants tn ON tn.id = dp.tenant_id
		 WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading department: %w", err)
	}
	return &d, nil
}

type EntityParams struct {
	Code              string
	Name              string
	Type              string
	DepartmentID      int64
	ParentEntityID    *int64
	Address           string
	RegisteredAddress string
	CINNumber         string
	GSTNumber         string
	IsActive          bool
}

func (r *Repository) CreateEntity(ctx context.Context, tenantID int64, p EntityParams) (*Entity, error) {
	res, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO entities
			(public_id, tenant_id, code, name, type, department_id, parent_entity_id, address,
			 registered_address, cin_number, gst_number, is_active)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		platform.NewULID(), tenantID, p.Code, p.Name, nullStr(p.Type),
		p.DepartmentID, p.ParentEntityID, nullStr(p.Address), nullStr(p.RegisteredAddress),
		nullStr(p.CINNumber), nullStr(p.GSTNumber), p.IsActive)
	if err != nil {
		if platform.IsDuplicate(err) {
			return nil, platform.ErrSentinelConflict
		}
		return nil, fmt.Errorf("creating entity: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading entity id: %w", err)
	}
	return r.EntityByID(ctx, tenantID, id)
}

type EntityUpdate struct {
	Code              *string
	Name              *string
	Type              *string
	DepartmentID      *int64
	ParentEntityID    *int64
	Address           *string
	RegisteredAddress *string
	CINNumber         *string
	GSTNumber         *string
	IsActive          *bool
}

func (r *Repository) UpdateEntity(ctx context.Context, tenantID, id int64, u EntityUpdate) error {
	set, args := []string{}, []any{}

	if u.Code != nil {
		set, args = append(set, "code = ?"), append(args, *u.Code)
	}
	if u.Name != nil {
		set, args = append(set, "name = ?"), append(args, *u.Name)
	}
	if u.Type != nil {
		set, args = append(set, "type = ?"), append(args, nullStr(*u.Type))
	}
	if u.DepartmentID != nil {
		set, args = append(set, "department_id = ?"), append(args, *u.DepartmentID)
	}
	if u.ParentEntityID != nil {
		// Guard against a cycle: an entity may not be its own parent.
		if *u.ParentEntityID == id {
			return platform.ErrSentinelConflict
		}
		set, args = append(set, "parent_entity_id = ?"), append(args, *u.ParentEntityID)
	}
	if u.Address != nil {
		set, args = append(set, "address = ?"), append(args, nullStr(*u.Address))
	}
	if u.RegisteredAddress != nil {
		set, args = append(set, "registered_address = ?"), append(args, nullStr(*u.RegisteredAddress))
	}
	if u.CINNumber != nil {
		set, args = append(set, "cin_number = ?"), append(args, nullStr(*u.CINNumber))
	}
	if u.GSTNumber != nil {
		set, args = append(set, "gst_number = ?"), append(args, nullStr(*u.GSTNumber))
	}
	if u.IsActive != nil {
		set, args = append(set, "is_active = ?"), append(args, *u.IsActive)
	}
	if len(set) == 0 {
		return nil
	}

	args = append(args, tenantID, id)
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE entities SET `+strings.Join(set, ", ")+` WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
		args...)
	if err != nil {
		if platform.IsDuplicate(err) {
			return platform.ErrSentinelConflict
		}
		return fmt.Errorf("updating entity: %w", err)
	}
	return affected(res)
}

func (r *Repository) DeleteEntity(ctx context.Context, tenantID, id int64) error {
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE entities SET deleted_at = UTC_TIMESTAMP(3), is_active = 0
		 WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, tenantID, id)
	if err != nil {
		return fmt.Errorf("deleting entity: %w", err)
	}
	return affected(res)
}

// --- sites ------------------------------------------------------------------

const siteColumns = `id, public_id, tenant_id, entity_id, code, name, address, city, state,
	pincode, is_active, is_default, created_at, updated_at`

const siteListColumns = `st.id, st.public_id, st.tenant_id, st.entity_id, st.code, st.name,
	st.address, st.city, st.state, st.pincode, st.is_active, st.is_default,
	st.created_at, st.updated_at,
	tn.name AS client_name, tn.slug AS client_slug, COALESCE(tn.client_code, '') AS client_code`

// Sites lists the locations across the clients in reach.
func (r *Repository) Sites(ctx context.Context, reach appctx.ClientReach, entityID *int64, activeOnly bool, ids []int64, page platform.Page, filter OrgFilter) ([]Site, error) {
	where, args := reachWhere("st.tenant_id", reach)
	where = append(where, "st.deleted_at IS NULL")
	// A site has no type and no department of its own; it hangs off an entity.
	filter.apply("st.", &where, &args, false, false)
	if len(filter.EntityIDs) > 0 {
		where = append(where, "st.entity_id IN ("+platform.Placeholders(len(filter.EntityIDs))+")")
		args = append(args, platform.Int64Args(filter.EntityIDs)...)
	}

	if entityID != nil {
		where = append(where, "st.entity_id = ?")
		args = append(args, *entityID)
	}
	if activeOnly {
		where = append(where, "st.is_active = 1")
	}
	if ids != nil {
		if len(ids) == 0 {
			return []Site{}, nil
		}
		where = append(where, "st.id IN ("+platform.Placeholders(len(ids))+")")
		args = append(args, platform.Int64Args(ids)...)
	}

	rows := []Site{}
	q := `SELECT ` + siteListColumns + `
		FROM sites st JOIN tenants tn ON tn.id = st.tenant_id
		WHERE ` + strings.Join(where, " AND ") + orderBy(page, "st.", "tn.name, st.name")
	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("listing sites: %w", err)
	}
	return rows, nil
}

func (r *Repository) SiteByID(ctx context.Context, tenantID, id int64) (*Site, error) {
	var s Site
	err := r.db.Primary.GetContext(ctx, &s,
		`SELECT `+siteColumns+` FROM sites WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
		tenantID, id)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading site: %w", err)
	}
	return &s, nil
}

func (r *Repository) SiteByPublicID(ctx context.Context, tenantID int64, publicID string) (*Site, error) {
	var s Site
	err := r.db.Primary.GetContext(ctx, &s,
		`SELECT `+siteColumns+` FROM sites WHERE tenant_id = ? AND public_id = ? AND deleted_at IS NULL`,
		tenantID, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading site: %w", err)
	}
	return &s, nil
}

type SiteParams struct {
	EntityID *int64
	Code     string
	Name     string
	Address  string
	City     string
	State    string
	Pincode  string
	IsActive bool
}

func (r *Repository) CreateSite(ctx context.Context, tenantID int64, p SiteParams) (*Site, error) {
	res, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO sites
			(public_id, tenant_id, entity_id, code, name, address, city, state, pincode, is_active)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		platform.NewULID(), tenantID, p.EntityID, p.Code, p.Name, nullStr(p.Address),
		nullStr(p.City), nullStr(p.State), nullStr(p.Pincode), p.IsActive)
	if err != nil {
		if platform.IsDuplicate(err) {
			return nil, platform.ErrSentinelConflict
		}
		return nil, fmt.Errorf("creating site: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading site id: %w", err)
	}
	return r.SiteByID(ctx, tenantID, id)
}

type SiteUpdate struct {
	EntityID *int64
	Code     *string
	Name     *string
	Address  *string
	City     *string
	State    *string
	Pincode  *string
	IsActive *bool
}

func (r *Repository) UpdateSite(ctx context.Context, tenantID, id int64, u SiteUpdate) error {
	set, args := []string{}, []any{}

	if u.EntityID != nil {
		set, args = append(set, "entity_id = ?"), append(args, *u.EntityID)
	}
	if u.Code != nil {
		set, args = append(set, "code = ?"), append(args, *u.Code)
	}
	if u.Name != nil {
		set, args = append(set, "name = ?"), append(args, *u.Name)
	}
	if u.Address != nil {
		set, args = append(set, "address = ?"), append(args, nullStr(*u.Address))
	}
	if u.City != nil {
		set, args = append(set, "city = ?"), append(args, nullStr(*u.City))
	}
	if u.State != nil {
		set, args = append(set, "state = ?"), append(args, nullStr(*u.State))
	}
	if u.Pincode != nil {
		set, args = append(set, "pincode = ?"), append(args, nullStr(*u.Pincode))
	}
	if u.IsActive != nil {
		set, args = append(set, "is_active = ?"), append(args, *u.IsActive)
	}
	if len(set) == 0 {
		return nil
	}

	args = append(args, tenantID, id)
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE sites SET `+strings.Join(set, ", ")+` WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
		args...)
	if err != nil {
		if platform.IsDuplicate(err) {
			return platform.ErrSentinelConflict
		}
		return fmt.Errorf("updating site: %w", err)
	}
	return affected(res)
}

func (r *Repository) DeleteSite(ctx context.Context, tenantID, id int64) error {
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE sites SET deleted_at = UTC_TIMESTAMP(3), is_active = 0
		 WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, tenantID, id)
	if err != nil {
		return fmt.Errorf("deleting site: %w", err)
	}
	return affected(res)
}

// --- departments ------------------------------------------------------------

const departmentColumns = `id, public_id, tenant_id, code, name, type, head_user_id,
	is_active, created_at, updated_at`

// departmentListColumns adds the client attribution and the entity count that
// the list view renders. The count is why an administrator can see at a glance
// which statutory lines are actually in use.
const departmentListColumns = `dp.id, dp.public_id, dp.tenant_id, dp.code, dp.name, dp.type,
	dp.head_user_id, dp.is_active, dp.created_at, dp.updated_at,
	tn.name AS client_name, tn.slug AS client_slug, COALESCE(tn.client_code, '') AS client_code,
	(SELECT COUNT(*) FROM entities e
	   WHERE e.department_id = dp.id AND e.deleted_at IS NULL) AS entity_count`

// Departments lists the statutory lines across the clients in reach.
func (r *Repository) Departments(ctx context.Context, reach appctx.ClientReach, activeOnly bool, ids []int64, page platform.Page, filter OrgFilter) ([]Department, error) {
	where, args := reachWhere("dp.tenant_id", reach)
	where = append(where, "dp.deleted_at IS NULL")
	filter.apply("dp.", &where, &args, true, false)

	if activeOnly {
		where = append(where, "dp.is_active = 1")
	}
	if ids != nil {
		if len(ids) == 0 {
			return []Department{}, nil
		}
		where = append(where, "dp.id IN ("+platform.Placeholders(len(ids))+")")
		args = append(args, platform.Int64Args(ids)...)
	}

	rows := []Department{}
	q := `SELECT ` + departmentListColumns + `
		FROM departments dp JOIN tenants tn ON tn.id = dp.tenant_id
		WHERE ` + strings.Join(where, " AND ") + orderBy(page, "dp.", "tn.name, dp.name")
	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("listing departments: %w", err)
	}
	return rows, nil
}

func (r *Repository) DepartmentByID(ctx context.Context, tenantID, id int64) (*Department, error) {
	var d Department
	err := r.db.Primary.GetContext(ctx, &d,
		`SELECT `+departmentColumns+` FROM departments WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
		tenantID, id)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading department: %w", err)
	}
	return &d, nil
}

func (r *Repository) DepartmentByPublicID(ctx context.Context, tenantID int64, publicID string) (*Department, error) {
	var d Department
	err := r.db.Primary.GetContext(ctx, &d,
		`SELECT `+departmentColumns+` FROM departments WHERE tenant_id = ? AND public_id = ? AND deleted_at IS NULL`,
		tenantID, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading department: %w", err)
	}
	return &d, nil
}

type DepartmentParams struct {
	Code       string
	Name       string
	Type       string
	HeadUserID *int64
	IsActive   bool
}

func (r *Repository) CreateDepartment(ctx context.Context, tenantID int64, p DepartmentParams) (*Department, error) {
	res, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO departments (public_id, tenant_id, code, name, type, head_user_id, is_active)
		VALUES (?,?,?,?,?,?,?)`,
		platform.NewULID(), tenantID, p.Code, p.Name, deptType(p.Type), p.HeadUserID, p.IsActive)
	if err != nil {
		if platform.IsDuplicate(err) {
			return nil, platform.ErrSentinelConflict
		}
		return nil, fmt.Errorf("creating department: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading department id: %w", err)
	}
	return r.DepartmentByID(ctx, tenantID, id)
}

type DepartmentUpdate struct {
	Code       *string
	Name       *string
	Type       *string
	HeadUserID *int64
	IsActive   *bool
}

func (r *Repository) UpdateDepartment(ctx context.Context, tenantID, id int64, u DepartmentUpdate) error {
	set, args := []string{}, []any{}

	if u.Code != nil {
		set, args = append(set, "code = ?"), append(args, *u.Code)
	}
	if u.Name != nil {
		set, args = append(set, "name = ?"), append(args, *u.Name)
	}
	if u.Type != nil {
		set, args = append(set, "type = ?"), append(args, deptType(*u.Type))
	}
	if u.HeadUserID != nil {
		set, args = append(set, "head_user_id = ?"), append(args, *u.HeadUserID)
	}
	if u.IsActive != nil {
		set, args = append(set, "is_active = ?"), append(args, *u.IsActive)
	}
	if len(set) == 0 {
		return nil
	}

	args = append(args, tenantID, id)
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE departments SET `+strings.Join(set, ", ")+` WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
		args...)
	if err != nil {
		if platform.IsDuplicate(err) {
			return platform.ErrSentinelConflict
		}
		return fmt.Errorf("updating department: %w", err)
	}
	return affected(res)
}

func (r *Repository) DeleteDepartment(ctx context.Context, tenantID, id int64) error {
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE departments SET deleted_at = UTC_TIMESTAMP(3), is_active = 0
		 WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, tenantID, id)
	if err != nil {
		return fmt.Errorf("deleting department: %w", err)
	}
	return affected(res)
}

// --- id resolution ----------------------------------------------------------

// ResolveEntityIDs maps public ids onto internal ids, rejecting any that do not
// belong to the tenant.
func (r *Repository) ResolveEntityIDs(ctx context.Context, tenantID int64, publicIDs []string) ([]int64, error) {
	return r.resolveIDs(ctx, "entities", tenantID, publicIDs)
}

func (r *Repository) ResolveSiteIDs(ctx context.Context, tenantID int64, publicIDs []string) ([]int64, error) {
	return r.resolveIDs(ctx, "sites", tenantID, publicIDs)
}

func (r *Repository) ResolveDepartmentIDs(ctx context.Context, tenantID int64, publicIDs []string) ([]int64, error) {
	return r.resolveIDs(ctx, "departments", tenantID, publicIDs)
}

// resolveIDs takes the table name from a fixed internal call site, never from
// user input.
func (r *Repository) resolveIDs(ctx context.Context, table string, tenantID int64, publicIDs []string) ([]int64, error) {
	if len(publicIDs) == 0 {
		return []int64{}, nil
	}
	switch table {
	case "entities", "sites", "departments":
	default:
		return nil, fmt.Errorf("unsupported table %q", table)
	}

	ids := []int64{}
	args := append([]any{tenantID}, platform.StringArgs(publicIDs)...)
	q := `SELECT id FROM ` + table + ` WHERE tenant_id = ? AND public_id IN (` +
		platform.Placeholders(len(publicIDs)) + `) AND deleted_at IS NULL`

	if err := r.db.Primary.SelectContext(ctx, &ids, q, args...); err != nil {
		return nil, fmt.Errorf("resolving %s ids: %w", table, err)
	}
	if len(ids) != len(publicIDs) {
		// One or more ids belong to another tenant or do not exist.
		return nil, platform.ErrSentinelNotFound
	}
	return ids, nil
}

func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// deptType normalises a department's statutory line, defaulting to GENERAL so
// legacy rows without a type are never an empty string in the API.
func deptType(t string) string {
	if strings.TrimSpace(t) == "" {
		return "GENERAL"
	}
	return strings.ToUpper(strings.TrimSpace(t))
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

// ResolveClient turns a client reference — public id, slug or code — into the
// internal tenant id. The caller applies its own reach on top; this only maps
// the name to the row.
func (r *Repository) ResolveClient(ctx context.Context, ref string) (int64, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, platform.ErrSentinelNotFound
	}

	var id int64
	err := r.db.Primary.GetContext(ctx, &id, `
		SELECT id FROM tenants
		WHERE deleted_at IS NULL AND (public_id = ? OR slug = ? OR client_code = ?)
		LIMIT 1`, ref, strings.ToLower(ref), strings.ToUpper(ref))
	if err != nil {
		if platform.IsNotFound(err) {
			return 0, platform.ErrSentinelNotFound
		}
		return 0, fmt.Errorf("resolving client: %w", err)
	}
	return id, nil
}
