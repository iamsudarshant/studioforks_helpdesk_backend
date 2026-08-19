package user

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// Kind groups users the way the Users section is organised: ComplyDesk's own
// agents, a client's partners, and a client's employees.
//
// The grouping is derived from the portal each of a user's roles binds to,
// rather than from a hard-coded role list, so adding a role puts its holders in
// the right section without touching this file.
const (
	KindAgent    = "AGENT"
	KindPartner  = "PARTNER"
	KindEmployee = "EMPLOYEE"
)

// Kinds is the section list, in the order the UI shows them.
var Kinds = []string{KindAgent, KindPartner, KindEmployee}

// kindPortals maps a section onto the role portals that belong to it.
var kindPortals = map[string][]string{
	KindAgent:    {"admin", "agents"},
	KindPartner:  {"partner"},
	KindEmployee: {"user"},
}

// ValidKind reports whether a requested section exists.
func ValidKind(kind string) bool {
	_, ok := kindPortals[strings.ToUpper(strings.TrimSpace(kind))]
	return ok
}

// ListFilter describes a user-list query. Every field maps onto a whitelisted
// column; nothing here is interpolated into SQL.
type ListFilter struct {
	Query         string
	Status        []string
	GroupIDs      []int64
	EntityIDs     []int64
	SiteIDs       []int64
	DepartmentIDs []int64
	RoleKeys      []string
	// Kind narrows the list to one of the Users sections — agents, partners or
	// employees. Empty means every kind.
	Kind          string
	NeverLoggedIn *bool
	MissingPF     *bool
	DOJFrom       *time.Time
	DOJTo         *time.Time
	// ScopeEntities and friends are the caller's own scope. When non-nil they
	// intersect with the requested filter, so a scoped admin cannot widen their
	// view by passing a filter for another entity.
	ScopeEntities    []int64
	ScopeSites       []int64
	ScopeDepartments []int64
	// Reach is the set of clients the list covers. Staff who have not selected
	// a client see every client they can reach; a client-side user always sees
	// exactly their own. See appctx.Reach.
	Reach appctx.ClientReach
	Page  platform.Page
}

// kindClause renders the section filter as an EXISTS over the user's roles.
func kindClause(kind string) (string, []any) {
	portals, ok := kindPortals[strings.ToUpper(strings.TrimSpace(kind))]
	if !ok {
		return "", nil
	}
	return `EXISTS (
		SELECT 1 FROM user_roles ur JOIN roles ro ON ro.id = ur.role_id
		WHERE ur.user_id = u.id AND ro.portal IN (` + platform.Placeholders(len(portals)) + `))`,
		platform.StringArgs(portals)
}

// UserSortable whitelists the columns a caller may order by.
//
// Keyed by the *grid's* column field names as well as the shorter API ones,
// because the browser sends whichever field the header belongs to. A key that
// is not listed is ignored rather than interpolated — which is safe, but was
// also why clicking a column header appeared to do nothing.
var UserSortable = map[string]string{
	"name":            "u.first_name",
	"full_name":       "u.first_name",
	"email":           "u.email",
	"mobile":          "u.mobile",
	"employee_code":   "u.employee_code",
	"status":          "u.status",
	"created_at":      "u.created_at",
	"last_login_at":   "u.last_login_at",
	"designation":     "u.designation",
	"date_of_joining": "u.date_of_joining",
	"client":          "tn.name",
	"entity":          "e.name",
	"site":            "st.name",
	"department":      "dp.name",
	"group":           "gr.name",
}

// List returns a page of users plus the total, applying the requested filters,
// the caller's data scope, and the clients in reach.
func (r *Repository) List(ctx context.Context, f ListFilter) ([]User, int64, error) {
	where := []string{"u.deleted_at IS NULL"}
	args := []any{}

	switch {
	case f.Reach.All:
		// Every client.
	case len(f.Reach.TenantIDs) > 0:
		where = append(where, "u.tenant_id IN ("+platform.Placeholders(len(f.Reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(f.Reach.TenantIDs)...)
	default:
		// No client reach must list nobody, never everybody.
		where = append(where, "1 = 0")
	}

	if clause, kindArgs := kindClause(f.Kind); clause != "" {
		where = append(where, clause)
		args = append(args, kindArgs...)
	}

	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, `(u.first_name LIKE ? OR u.last_name LIKE ? OR u.email LIKE ?
			OR u.employee_code LIKE ? OR u.mobile LIKE ? OR u.pf_number LIKE ?
			OR u.uan_number LIKE ? OR u.pan_number LIKE ?)`)
		like := "%" + q + "%"
		for i := 0; i < 8; i++ {
			args = append(args, like)
		}
	}

	addIn := func(col string, values []int64) {
		if values == nil {
			return
		}
		if len(values) == 0 {
			// An explicit empty scope must match nothing, not everything.
			where = append(where, "1 = 0")
			return
		}
		where = append(where, col+" IN ("+platform.Placeholders(len(values))+")")
		args = append(args, platform.Int64Args(values)...)
	}

	if len(f.Status) > 0 {
		where = append(where, "u.status IN ("+platform.Placeholders(len(f.Status))+")")
		args = append(args, platform.StringArgs(f.Status)...)
	}
	addIn("u.user_group_id", f.GroupIDs)
	addIn("u.entity_id", intersect(f.EntityIDs, f.ScopeEntities))
	addIn("u.site_id", intersect(f.SiteIDs, f.ScopeSites))
	addIn("u.department_id", intersect(f.DepartmentIDs, f.ScopeDepartments))

	if len(f.RoleKeys) > 0 {
		where = append(where, `EXISTS (
			SELECT 1 FROM user_roles ur JOIN roles ro ON ro.id = ur.role_id
			WHERE ur.user_id = u.id AND ro.role_key IN (`+platform.Placeholders(len(f.RoleKeys))+`))`)
		args = append(args, platform.StringArgs(f.RoleKeys)...)
	}
	if f.NeverLoggedIn != nil {
		if *f.NeverLoggedIn {
			where = append(where, "u.last_login_at IS NULL")
		} else {
			where = append(where, "u.last_login_at IS NOT NULL")
		}
	}
	if f.MissingPF != nil && *f.MissingPF {
		where = append(where, "(u.pf_number IS NULL OR u.pf_number = '')")
	}
	if f.DOJFrom != nil {
		where = append(where, "u.date_of_joining >= ?")
		args = append(args, *f.DOJFrom)
	}
	if f.DOJTo != nil {
		where = append(where, "u.date_of_joining <= ?")
		args = append(args, *f.DOJTo)
	}

	clause := " WHERE " + strings.Join(where, " AND ")

	var total int64
	if err := r.db.Primary.GetContext(ctx, &total,
		`SELECT COUNT(*) FROM users u`+clause, args...); err != nil {
		return nil, 0, fmt.Errorf("counting users: %w", err)
	}

	sortBy := f.Page.SortBy
	if sortBy == "" {
		sortBy = "u.created_at"
	}
	rows := []User{}
	// The client and the role list travel with every row: a cross-client list
	// has to say whose employee this is, and the Users sections need the role
	// to label a person without a request per row.
	q := `SELECT ` + prefixed(userColumns, "u") + `,
			tn.name AS client_name, tn.slug AS client_slug,
			COALESCE(tn.client_code, '') AS client_code,
			COALESCE((
				SELECT GROUP_CONCAT(DISTINCT ro.name ORDER BY ro.name SEPARATOR ', ')
				FROM user_roles ur JOIN roles ro ON ro.id = ur.role_id
				WHERE ur.user_id = u.id
			), '') AS role_names,
			COALESCE(e.public_id, '') AS entity_public_id,
			COALESCE(e.code, '')      AS entity_code,
			COALESCE(e.name, '')      AS entity_name,
			COALESCE(st.public_id, '') AS site_public_id,
			COALESCE(st.code, '')      AS site_code,
			COALESCE(st.name, '')      AS site_name,
			COALESCE(dp.public_id, '') AS department_public_id,
			COALESCE(dp.code, '')      AS department_code,
			COALESCE(dp.name, '')      AS department_name,
			COALESCE(gr.public_id, '') AS group_public_id,
			COALESCE(gr.group_key, '') AS group_key,
			COALESCE(gr.name, '')      AS group_name,
			COALESCE(ha.public_id, '') AS handling_agent_public_id,
			COALESCE(TRIM(CONCAT(ha.first_name, ' ', COALESCE(ha.last_name, ''))), '')
			                           AS handling_agent_name
		FROM users u
		JOIN tenants tn          ON tn.id = u.tenant_id
		LEFT JOIN entities e     ON e.id = u.entity_id
		LEFT JOIN sites st       ON st.id = u.site_id
		LEFT JOIN departments dp ON dp.id = u.department_id
		LEFT JOIN user_groups gr ON gr.id = u.user_group_id
		LEFT JOIN users ha       ON ha.id = u.handling_agent_id` + clause +
		fmt.Sprintf(" ORDER BY %s %s, u.id DESC LIMIT ? OFFSET ?", sortBy, f.Page.SortDir)
	args = append(args, f.Page.PerPage, f.Page.Offset())

	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, 0, fmt.Errorf("listing users: %w", err)
	}
	return rows, total, nil
}

// SectionCount is one tile on the Users landing screen.
type SectionCount struct {
	Kind   string `json:"kind"`
	Total  int64  `json:"total"`
	Active int64  `json:"active"`
}

// SectionCounts returns the headcount per Users section across the clients in
// reach, so the landing screen can render its tiles in one request rather than
// one list query per section.
func (r *Repository) SectionCounts(ctx context.Context, reach appctx.ClientReach, kinds []string) ([]SectionCount, error) {
	out := make([]SectionCount, 0, len(kinds))

	for _, kind := range kinds {
		where := []string{"u.deleted_at IS NULL"}
		args := []any{}

		switch {
		case reach.All:
		case len(reach.TenantIDs) > 0:
			where = append(where, "u.tenant_id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
			args = append(args, platform.Int64Args(reach.TenantIDs)...)
		default:
			where = append(where, "1 = 0")
		}

		clause, kindArgs := kindClause(kind)
		if clause == "" {
			continue
		}
		where = append(where, clause)
		args = append(args, kindArgs...)

		var row SectionCount
		err := r.db.Primary.GetContext(ctx, &row, `
			SELECT COUNT(*) AS total,
			       COALESCE(SUM(u.status = 'ACTIVE'), 0) AS active
			FROM users u WHERE `+strings.Join(where, " AND "), args...)
		if err != nil {
			return nil, fmt.Errorf("counting %s users: %w", kind, err)
		}
		row.Kind = kind
		out = append(out, row)
	}
	return out, nil
}

// intersect narrows a requested filter by the caller's scope. A nil scope means
// unrestricted; a nil request means "all of my scope".
func intersect(requested, scope []int64) []int64 {
	if scope == nil {
		return requested
	}
	if requested == nil {
		return scope
	}
	allowed := make(map[int64]struct{}, len(scope))
	for _, id := range scope {
		allowed[id] = struct{}{}
	}
	out := make([]int64, 0, len(requested))
	for _, id := range requested {
		if _, ok := allowed[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

func prefixed(cols, alias string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

type CreateParams struct {
	EmployeeCode   string
	Username       string
	FirstName      string
	LastName       string
	Email          string
	AltEmail       string
	Mobile         string
	AltMobile      string
	PANNumber      string
	UANNumber      string
	PFNumber       string
	ESICNumber     string
	DateOfJoining  *time.Time
	DateOfBirth    *time.Time
	LastWorkingDay *time.Time
	EntityID       *int64
	SiteID         *int64
	DepartmentID   *int64
	Designation    string
	UserGroupID    *int64
	Status         string
	PasswordHash   string
	MustChange     bool
	CustomFields   string
	CreatedBy      *int64
}

func (r *Repository) Create(ctx context.Context, tenantID int64, p CreateParams) (*User, error) {
	publicID := platform.NewULID()
	status := p.Status
	if status == "" {
		status = StatusActive
	}

	res, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO users
			(public_id, tenant_id, employee_code, username, first_name, last_name, email,
			 alt_email, mobile, alt_mobile, pan_number, uan_number, pf_number, esic_number,
			 date_of_joining, date_of_birth, last_working_day, entity_id, site_id, department_id,
			 designation, user_group_id, status, password_hash, must_change_password,
			 custom_fields_json, created_by)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		publicID, tenantID, nullStr(p.EmployeeCode), nullStr(p.Username), p.FirstName,
		nullStr(p.LastName), nullStr(strings.ToLower(p.Email)), nullStr(strings.ToLower(p.AltEmail)),
		nullStr(p.Mobile), nullStr(p.AltMobile), nullStr(strings.ToUpper(p.PANNumber)),
		nullStr(p.UANNumber), nullStr(p.PFNumber), nullStr(p.ESICNumber),
		p.DateOfJoining, p.DateOfBirth, p.LastWorkingDay,
		p.EntityID, p.SiteID, p.DepartmentID, nullStr(p.Designation), p.UserGroupID,
		status, nullStr(p.PasswordHash), p.MustChange, nullStr(p.CustomFields), p.CreatedBy)
	if err != nil {
		if platform.IsDuplicate(err) {
			return nil, &DuplicateError{Key: platform.DuplicateKey(err)}
		}
		return nil, fmt.Errorf("creating user: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading user id: %w", err)
	}
	return r.ByID(ctx, tenantID, id)
}

// DuplicateError names the unique index that rejected an insert so the handler
// can attach the failure to the right form field.
type DuplicateError struct{ Key string }

func (e *DuplicateError) Error() string { return "duplicate value for " + e.Key }

// Field maps the index name onto the request field the user typed.
func (e *DuplicateError) Field() string {
	switch e.Key {
	case "uq_users_email":
		return "email"
	case "uq_users_employee_code":
		return "employee_code"
	case "uq_users_username":
		return "username"
	case "uq_users_pf":
		return "pf_number"
	case "uq_users_uan":
		return "uan_number"
	case "uq_users_pan":
		return "pan_number"
	default:
		return "id"
	}
}

type UpdateParams struct {
	EmployeeCode   *string
	Username       *string
	FirstName      *string
	LastName       *string
	Email          *string
	AltEmail       *string
	Mobile         *string
	AltMobile      *string
	PANNumber      *string
	UANNumber      *string
	PFNumber       *string
	ESICNumber     *string
	DateOfJoining  *time.Time
	DateOfBirth    *time.Time
	LastWorkingDay *time.Time
	EntityID       *int64
	SiteID         *int64
	DepartmentID   *int64
	Designation    *string
	UserGroupID    *int64
	Status         *string
	Locale         *string
	Timezone       *string
	CustomFields   *string
	UpdatedBy      *int64
}

func (r *Repository) Update(ctx context.Context, tenantID, id int64, p UpdateParams) error {
	set, args := []string{}, []any{}

	addStr := func(col string, v *string, transform func(string) string) {
		if v == nil {
			return
		}
		val := *v
		if transform != nil {
			val = transform(val)
		}
		set = append(set, col+" = ?")
		args = append(args, nullStr(val))
	}
	addTime := func(col string, v *time.Time) {
		if v != nil {
			set = append(set, col+" = ?")
			args = append(args, *v)
		}
	}
	addID := func(col string, v *int64) {
		if v != nil {
			set = append(set, col+" = ?")
			if *v == 0 {
				args = append(args, nil) // 0 means "clear this reference"
			} else {
				args = append(args, *v)
			}
		}
	}

	addStr("employee_code", p.EmployeeCode, nil)
	addStr("username", p.Username, strings.ToLower)
	addStr("first_name", p.FirstName, nil)
	addStr("last_name", p.LastName, nil)
	addStr("email", p.Email, strings.ToLower)
	addStr("alt_email", p.AltEmail, strings.ToLower)
	addStr("mobile", p.Mobile, nil)
	addStr("alt_mobile", p.AltMobile, nil)
	addStr("pan_number", p.PANNumber, strings.ToUpper)
	addStr("uan_number", p.UANNumber, nil)
	addStr("pf_number", p.PFNumber, nil)
	addStr("esic_number", p.ESICNumber, nil)
	addStr("designation", p.Designation, nil)
	addStr("status", p.Status, nil)
	addStr("locale", p.Locale, nil)
	addStr("timezone", p.Timezone, nil)
	addStr("custom_fields_json", p.CustomFields, nil)
	addTime("date_of_joining", p.DateOfJoining)
	addTime("date_of_birth", p.DateOfBirth)
	addTime("last_working_day", p.LastWorkingDay)
	addID("entity_id", p.EntityID)
	addID("site_id", p.SiteID)
	addID("department_id", p.DepartmentID)
	addID("user_group_id", p.UserGroupID)

	if p.UpdatedBy != nil {
		set = append(set, "updated_by = ?")
		args = append(args, *p.UpdatedBy)
	}
	if len(set) == 0 {
		return nil
	}

	args = append(args, tenantID, id)
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE users SET `+strings.Join(set, ", ")+
			` WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, args...)
	if err != nil {
		if platform.IsDuplicate(err) {
			return &DuplicateError{Key: platform.DuplicateKey(err)}
		}
		return fmt.Errorf("updating user: %w", err)
	}
	return affected(res)
}

// SoftDelete removes a user while preserving their ticket history. The unique
// identifiers are released so the same email can be re-onboarded later.
func (r *Repository) SoftDelete(ctx context.Context, tenantID, id int64) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE users
			SET deleted_at = UTC_TIMESTAMP(3),
			    status = 'INACTIVE',
			    email = CONCAT('deleted+', public_id, '@invalid'),
			    username = NULL,
			    employee_code = CONCAT('DEL-', public_id),
			    pf_number = NULL, uan_number = NULL, pan_number = NULL
			WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, tenantID, id)
		if err != nil {
			return fmt.Errorf("deleting user: %w", err)
		}
		if err := affected(res); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET revoked_at = UTC_TIMESTAMP(3), revoked_reason = 'USER_DELETED'
			 WHERE user_id = ? AND revoked_at IS NULL`, id); err != nil {
			return fmt.Errorf("revoking sessions: %w", err)
		}
		return nil
	})
}

func (r *Repository) SetStatus(ctx context.Context, tenantID, id int64, status string) error {
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE users SET status = ? WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
		status, tenantID, id)
	if err != nil {
		return fmt.Errorf("setting user status: %w", err)
	}
	if err := affected(res); err != nil {
		return err
	}

	// Deactivating must end live sessions, or the user keeps working until
	// their access token expires.
	if status == StatusInactive {
		if _, err := r.db.Primary.ExecContext(ctx,
			`UPDATE sessions SET revoked_at = UTC_TIMESTAMP(3), revoked_reason = 'USER_DEACTIVATED'
			 WHERE user_id = ? AND revoked_at IS NULL`, id); err != nil {
			return fmt.Errorf("revoking sessions: %w", err)
		}
	}
	return nil
}

// ResolveIDs maps user public ids to internal ids inside the tenant.
func (r *Repository) ResolveIDs(ctx context.Context, tenantID int64, publicIDs []string) ([]int64, error) {
	if len(publicIDs) == 0 {
		return []int64{}, nil
	}
	ids := []int64{}
	args := append([]any{tenantID}, platform.StringArgs(publicIDs)...)
	q := `SELECT id FROM users WHERE tenant_id = ? AND public_id IN (` +
		platform.Placeholders(len(publicIDs)) + `) AND deleted_at IS NULL`

	if err := r.db.Primary.SelectContext(ctx, &ids, q, args...); err != nil {
		return nil, fmt.Errorf("resolving user ids: %w", err)
	}
	if len(ids) != len(publicIDs) {
		return nil, platform.ErrSentinelNotFound
	}
	return ids, nil
}

// Activity returns a user's portal activity trail.
type ActivityEntry struct {
	// A stable identity for the row. Every list the product returns carries one:
	// the grid keys on it, and without it the whole screen throws rather than
	// rendering an unkeyed table. The audit row's own id, as a string — an
	// audit entry is not addressable by any route, so this is a key rather
	// than a handle.
	ID            string    `db:"id" json:"id"`
	Action        string    `db:"action" json:"action"`
	ResourceType  string    `db:"resource_type" json:"resource_type"`
	ResourceLabel string    `db:"resource_label" json:"resource_label"`
	Portal        string    `db:"portal" json:"portal"`
	IP            string    `db:"ip" json:"ip"`
	DurationMS    *int      `db:"duration_ms" json:"duration_ms"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
}

// Activity is what this person has done, read from the audit trail.
//
// It used to read `user_activity`, a second trail written by a recorder nothing
// ever calls — so the table was empty, and the Activity tab was blank for every
// user on the system rather than for the ones with nothing to show. The audit
// log already records the same facts against `actor_id`, is written on every
// mutating request, and is hash-chained; a parallel trail earns nothing but the
// chance to disagree with it.
func (r *Repository) Activity(ctx context.Context, tenantID, userID int64, actions []string, from, to *time.Time, page platform.Page) ([]ActivityEntry, int64, error) {
	// Cross-tenant entries are recorded against the platform workspace, so a
	// staff member's own actions on a client are found by actor rather than by
	// the client the action landed in.
	where := []string{"actor_id = ?"}
	args := []any{userID}
	if tenantID != 0 {
		where = append(where, "(tenant_id = ? OR cross_tenant = 1)")
		args = append(args, tenantID)
	}

	if len(actions) > 0 {
		where = append(where, "action IN ("+platform.Placeholders(len(actions))+")")
		args = append(args, platform.StringArgs(actions)...)
	}
	if from != nil {
		where = append(where, "created_at >= ?")
		args = append(args, *from)
	}
	if to != nil {
		where = append(where, "created_at <= ?")
		args = append(args, *to)
	}

	clause := " WHERE " + strings.Join(where, " AND ")

	var total int64
	if err := r.db.Read().GetContext(ctx, &total,
		`SELECT COUNT(*) FROM audit_logs`+clause, args...); err != nil {
		return nil, 0, fmt.Errorf("counting activity: %w", err)
	}

	rows := []struct {
		ID            int64     `db:"id"`
		Action        string    `db:"action"`
		ResourceType  *string   `db:"resource_type"`
		ResourceLabel *string   `db:"resource_label"`
		Portal        *string   `db:"portal"`
		IP            *string   `db:"ip"`
		CreatedAt     time.Time `db:"created_at"`
	}{}

	// `entity_public_id` stands in for a label: it is what the row is about,
	// and it is what someone reading the trail can look up.
	q := `SELECT id, action, entity_type AS resource_type,
	             entity_public_id AS resource_label, portal, ip, created_at
	      FROM audit_logs` + clause + ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, page.PerPage, page.Offset())

	if err := r.db.Read().SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, 0, fmt.Errorf("loading activity: %w", err)
	}

	out := make([]ActivityEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, ActivityEntry{
			ID:            strconv.FormatInt(row.ID, 10),
			Action:        row.Action,
			ResourceType:  deref(row.ResourceType),
			ResourceLabel: deref(row.ResourceLabel),
			Portal:        deref(row.Portal),
			IP:            deref(row.IP),
			CreatedAt:     row.CreatedAt,
		})
	}
	return out, total, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// GroupCounts returns the number of users in each group, for the group list.
func (r *Repository) GroupCounts(ctx context.Context, tenantID int64) (map[int64]int64, error) {
	rows := []struct {
		GroupID int64 `db:"user_group_id"`
		Count   int64 `db:"c"`
	}{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT user_group_id, COUNT(*) AS c
		FROM users
		WHERE tenant_id = ? AND deleted_at IS NULL AND user_group_id IS NOT NULL
		GROUP BY user_group_id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("counting group members: %w", err)
	}

	out := make(map[int64]int64, len(rows))
	for _, row := range rows {
		out[row.GroupID] = row.Count
	}
	return out, nil
}
