// Package analytics owns the read-only aggregate surface: role-aware dashboard
// widgets and the operational reports in §12 of the development brief.
//
// It deliberately reuses ticket.Scope rather than re-deriving visibility. A
// dashboard that counted rows a user cannot open would leak the existence of
// other people's tickets through a number, which is the same disclosure as
// showing the row itself.
package analytics

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/export"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/ticket"
)

type Repository struct {
	db *platform.DB
}

func NewRepository(db *platform.DB) *Repository { return &Repository{db: db} }

// scoped builds the WHERE clause every aggregate starts from: the clients in
// reach, the soft-delete filter, and the caller's own visibility.
//
// The reach is what makes an unselected client switcher mean "every client"
// rather than "the platform workspace" — see appctx.Reach.
// Window is the dashboard date range. Both ends are optional, so "everything"
// and "since Monday" are the same code path.
//
// The range is applied to `created_at` — when the ticket was raised — because
// that is the question the filter is read as answering: "what came in this
// week". Applying it to `resolved_at` instead would make "Today" show a board
// of tickets raised months ago, and applying it to both would make the counts
// on the strip disagree with the list the KPI links to.
type Window struct {
	From *time.Time
	To   *time.Time
}

// Apply appends the range to a WHERE builder. A nil receiver adds nothing,
// which is how every caller that has no range stays unchanged.
func (w *Window) Apply(where *[]string, args *[]any) {
	if w == nil {
		return
	}
	if w.From != nil {
		*where = append(*where, "t.created_at >= ?")
		*args = append(*args, w.From.UTC())
	}
	if w.To != nil {
		*where = append(*where, "t.created_at < ?")
		*args = append(*args, w.To.UTC())
	}
}

// scopedIn is `scoped` with the dashboard's date window folded in. Kept
// separate so the report queries, which carry their own From/To handling, are
// not disturbed.
func scopedIn(reach appctx.ClientReach, scope ticket.Scope, w *Window, extra ...string) (string, []any) {
	clause, args := scoped(reach, scope, extra...)
	if w == nil || (w.From == nil && w.To == nil) {
		return clause, args
	}

	var window []string
	w.Apply(&window, &args)
	return clause + " AND " + strings.Join(window, " AND "), args
}

func scoped(reach appctx.ClientReach, scope ticket.Scope, extra ...string) (string, []any) {
	where := []string{"t.deleted_at IS NULL"}
	args := []any{}

	switch {
	case reach.All:
		// Every client. Nothing to add.
	case len(reach.TenantIDs) > 0:
		where = append(where, "t.tenant_id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(reach.TenantIDs)...)
	default:
		// No reach at all must count nothing, never everything.
		where = append(where, "1 = 0")
	}

	scope.Apply(&where, &args)
	where = append(where, extra...)
	return " WHERE " + strings.Join(where, " AND "), args
}

// --- dashboard --------------------------------------------------------------

// Summary is the KPI strip. Every field is a count the caller may actually see.
type Summary struct {
	Total           int64 `db:"total"            json:"total"`
	Open            int64 `db:"open"             json:"open"`
	Unassigned      int64 `db:"unassigned"       json:"unassigned"`
	PendingEmployee int64 `db:"pending_employee" json:"pending_employee"`
	PendingHelpdesk int64 `db:"pending_helpdesk" json:"pending_helpdesk"`
	Escalated       int64 `db:"escalated"        json:"escalated"`
	Resolved        int64 `db:"resolved"         json:"resolved"`
	Closed          int64 `db:"closed"           json:"closed"`
	Reopened        int64 `db:"reopened"         json:"reopened"`
	Breached        int64 `db:"breached"         json:"breached"`
	ResolvedToday   int64 `db:"resolved_today"   json:"resolved_today"`
	RaisedThisWeek  int64 `db:"raised_this_week" json:"raised_this_week"`
	// AvgResolutionHours is the TAT headline. Null when nothing has been
	// resolved yet, rather than a misleading zero.
	AvgResolutionHours *float64 `db:"avg_resolution_hours" json:"avg_resolution_hours"`
	SLACompliancePct   *float64 `db:"sla_compliance_pct"   json:"sla_compliance_pct"`
}

func (r *Repository) Summary(ctx context.Context, reach appctx.ClientReach, scope ticket.Scope, w *Window) (*Summary, error) {
	clause, args := scopedIn(reach, scope, w)

	var s Summary
	err := r.db.Primary.GetContext(ctx, &s, `
		SELECT
			COUNT(*)                                                          AS total,
			COALESCE(SUM(t.status NOT IN ('CLOSED','CANCELLED')), 0)          AS open,
			COALESCE(SUM(t.assignee_id IS NULL
			             AND t.status NOT IN ('CLOSED','CANCELLED')), 0)      AS unassigned,
			COALESCE(SUM(t.status = 'PENDING_EMPLOYEE'), 0)                   AS pending_employee,
			COALESCE(SUM(t.status = 'PENDING_HELPDESK'), 0)                   AS pending_helpdesk,
			COALESCE(SUM(t.status = 'ESCALATED'), 0)                          AS escalated,
			COALESCE(SUM(t.status = 'RESOLVED'), 0)                           AS resolved,
			COALESCE(SUM(t.status = 'CLOSED'), 0)                             AS closed,
			COALESCE(SUM(t.reopened_count > 0), 0)                            AS reopened,
			COALESCE(SUM(t.is_sla_breached = 1), 0)                           AS breached,
			COALESCE(SUM(t.resolved_at IS NOT NULL
			             AND DATE(t.resolved_at) = UTC_DATE()), 0)            AS resolved_today,
			COALESCE(SUM(t.created_at >= DATE_SUB(UTC_TIMESTAMP(), INTERVAL 7 DAY)), 0) AS raised_this_week,
			AVG(CASE WHEN t.resolved_at IS NOT NULL
			         THEN TIMESTAMPDIFF(MINUTE, t.created_at, t.resolved_at) / 60.0 END) AS avg_resolution_hours,
			CASE WHEN SUM(t.resolved_at IS NOT NULL) > 0
			     THEN 100.0 * SUM(t.resolved_at IS NOT NULL AND t.is_sla_breached = 0)
			          / SUM(t.resolved_at IS NOT NULL) END                    AS sla_compliance_pct
		FROM tickets t`+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("building dashboard summary: %w", err)
	}
	return &s, nil
}

// Bucket is one row of any grouped chart: a label and a count.
type Bucket struct {
	Key   string `db:"k" json:"key"`
	Label string `db:"l" json:"label"`
	Count int64  `db:"c" json:"count"`
}

// GroupBy powers the categorical charts. The dimension is whitelisted, never
// interpolated from the request.
func (r *Repository) GroupBy(ctx context.Context, reach appctx.ClientReach, scope ticket.Scope, dimension string, w *Window) ([]Bucket, error) {
	// Each entry is (select expression, join). Anything not listed is refused
	// by the caller, so no request value ever reaches the query text.
	dims := map[string]struct{ key, label, join string }{
		"status":   {"t.status", "t.status", ""},
		"priority": {"t.priority", "t.priority", ""},
		"category": {"c.category_key", "c.name", " JOIN categories c ON c.id = t.category_id"},
		"entity":   {"COALESCE(e.code,'-')", "COALESCE(e.name,'Unassigned')", " LEFT JOIN entities e ON e.id = t.entity_id"},
		"site":     {"COALESCE(s.code,'-')", "COALESCE(s.name,'Unassigned')", " LEFT JOIN sites s ON s.id = t.site_id"},
		"assignee": {"COALESCE(au.public_id,'-')", "COALESCE(CONCAT(au.first_name,' ',COALESCE(au.last_name,'')),'Unassigned')", " LEFT JOIN users au ON au.id = t.assignee_id"},
		"module":   {"COALESCE(m.module_key,'-')", "COALESCE(m.name,'Uncategorised')", " JOIN categories c ON c.id = t.category_id LEFT JOIN modules m ON m.id = c.module_id"},
	}
	dim, ok := dims[dimension]
	if !ok {
		return nil, fmt.Errorf("unknown dimension %q", dimension)
	}

	clause, args := scopedIn(reach, scope, w)
	rows := []Bucket{}
	q := `SELECT ` + dim.key + ` AS k, ` + dim.label + ` AS l, COUNT(*) AS c
		FROM tickets t` + dim.join + clause + ` GROUP BY k, l ORDER BY c DESC`

	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("grouping tickets by %s: %w", dimension, err)
	}
	return rows, nil
}

// Dimensions are the groupings GroupBy accepts, for the handler's validation.
var Dimensions = []string{"status", "priority", "category", "entity", "site", "assignee", "module"}

// TrendPoint is one day on the volume chart.
type TrendPoint struct {
	Day      string `db:"d" json:"day"`
	Raised   int64  `db:"raised" json:"raised"`
	Resolved int64  `db:"resolved" json:"resolved"`
}

// Trend returns daily raised/resolved counts for the last `days` days.
//
// Both series come from one pass over the tickets rather than two queries, and
// days with no activity are simply absent — the client fills the gaps, which
// keeps the payload small for a long window.
func (r *Repository) Trend(ctx context.Context, reach appctx.ClientReach, scope ticket.Scope, days int, w *Window) ([]TrendPoint, error) {
	var clause string
	var args []any

	// An explicit range wins over the day count. The two used to be independent:
	// the strip above the chart moved with the date filter and the chart under
	// it did not, so the same screen showed two different periods at once.
	if w != nil && (w.From != nil || w.To != nil) {
		clause, args = scopedIn(reach, scope, w)
	} else {
		if days <= 0 || days > 365 {
			days = 30
		}
		clause, args = scoped(reach, scope, "t.created_at >= DATE_SUB(UTC_DATE(), INTERVAL ? DAY)")
		args = append(args, days)
	}

	rows := []TrendPoint{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT DATE(t.created_at) AS d,
		       COUNT(*) AS raised,
		       COALESCE(SUM(t.resolved_at IS NOT NULL), 0) AS resolved
		FROM tickets t`+clause+`
		GROUP BY d ORDER BY d`, args...)
	if err != nil {
		return nil, fmt.Errorf("building ticket trend: %w", err)
	}
	return rows, nil
}

// --- reports ----------------------------------------------------------------

// Result is a rendered report: named columns and the rows beneath them.
//
// The type itself lives in the export package, because the ticket list produces
// the same shape and both are rendered by the same three writers.
type Result = export.Result

// Params narrows a report. Zero values mean "no filter".
type Params struct {
	From *time.Time
	To   *time.Time

	// The filters the Reports screen offers. Each is a set of ids or values
	// already resolved by the handler, so nothing here reaches SQL as text.
	Statuses      []string
	Priorities    []string
	EntityIDs     []int64
	DepartmentIDs []int64
	CategoryIDs   []int64
	AssigneeIDs   []int64
	// BreachedOnly narrows to tickets that missed their SLA.
	BreachedOnly bool
}

// window renders every filter as an AND-clause fragment.
//
// Every report's query already ends in a WHERE built from the caller's reach
// and scope, so these append rather than start a clause. The values are always
// bound, never interpolated: the only thing that varies is how many
// placeholders each list contributes.
func (p Params) window() (string, []any) {
	clause, args := "", []any{}

	if p.From != nil {
		clause += " AND t.created_at >= ?"
		args = append(args, *p.From)
	}
	if p.To != nil {
		clause += " AND t.created_at <= ?"
		args = append(args, *p.To)
	}

	addStrings := func(column string, values []string) {
		if len(values) == 0 {
			return
		}
		clause += " AND " + column + " IN (" + platform.Placeholders(len(values)) + ")"
		args = append(args, platform.StringArgs(values)...)
	}
	addIDs := func(column string, ids []int64) {
		if len(ids) == 0 {
			return
		}
		clause += " AND " + column + " IN (" + platform.Placeholders(len(ids)) + ")"
		args = append(args, platform.Int64Args(ids)...)
	}

	addStrings("t.status", p.Statuses)
	addStrings("t.priority", p.Priorities)
	addIDs("t.entity_id", p.EntityIDs)
	addIDs("t.department_id", p.DepartmentIDs)
	addIDs("t.category_id", p.CategoryIDs)
	addIDs("t.assignee_id", p.AssigneeIDs)

	if p.BreachedOnly {
		clause += " AND t.is_sla_breached = 1"
	}
	return clause, args
}

// UserCounts is the headcount strip on an administrator's dashboard.
type UserCounts struct {
	Total       int64 `db:"total"        json:"total"`
	Active      int64 `db:"active"       json:"active"`
	ExEmployees int64 `db:"ex_employees" json:"ex_employees"`
	Inactive    int64 `db:"inactive"     json:"inactive"`
	Locked      int64 `db:"locked"       json:"locked"`
}

// UserCounts returns headcount across the clients in reach.
//
// Not scoped by ticket visibility: this counts people, not tickets, and the
// caller is already gated on `user.view.all` before it is asked for.
func (r *Repository) UserCounts(ctx context.Context, reach appctx.ClientReach) (*UserCounts, error) {
	where := []string{"u.deleted_at IS NULL"}
	args := []any{}
	switch {
	case reach.All:
		// "Employees" means the clients' people. ComplyDesk's own staff live in
		// the platform workspace and would otherwise inflate every headcount.
		where = append(where, "tn.is_platform = 0")
	case len(reach.TenantIDs) > 0:
		where = append(where, "u.tenant_id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(reach.TenantIDs)...)
	default:
		where = append(where, "1 = 0")
	}

	// An employee is somebody holding the EMPLOYEE role, not merely somebody
	// who belongs to a client.
	//
	// Without this the card counted every account in the workspace — the client
	// administrator and all 41 partners as well as the 34 employees — and
	// reported 76 under a label that says "Employees". The Users screen filters
	// by role, so the two disagreed about the same question, which is exactly
	// the mismatch this KPI is read to check.
	//
	// EXISTS rather than a join, because a user holding two roles would
	// otherwise be counted twice.
	where = append(where, `EXISTS (
		SELECT 1 FROM user_roles ur
		JOIN roles ro ON ro.id = ur.role_id
		WHERE ur.user_id = u.id AND ro.role_key = ?)`)
	args = append(args, "EMPLOYEE")

	var out UserCounts
	err := r.db.Primary.GetContext(ctx, &out, `
		SELECT COUNT(*) AS total,
		       COALESCE(SUM(u.status = 'ACTIVE'), 0)      AS active,
		       COALESCE(SUM(u.status = 'EX_EMPLOYEE'), 0) AS ex_employees,
		       COALESCE(SUM(u.status = 'INACTIVE'), 0)    AS inactive,
		       COALESCE(SUM(u.status = 'LOCKED'), 0)      AS locked
		FROM users u JOIN tenants tn ON tn.id = u.tenant_id
		WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		return nil, fmt.Errorf("counting users: %w", err)
	}
	return &out, nil
}

// ClientBreakdown is one client's row in the cross-client dashboard: the table
// that only appears when no single client is selected.
type ClientBreakdown struct {
	Slug       string `db:"slug"        json:"slug"`
	ClientCode string `db:"client_code" json:"client_code"`
	Name       string `db:"name"        json:"name"`
	Status     string `db:"status"      json:"status"`
	Total      int64  `db:"total"       json:"total"`
	Open       int64  `db:"open"        json:"open"`
	Breached   int64  `db:"breached"    json:"breached"`
	Unassigned int64  `db:"unassigned"  json:"unassigned"`
	Users      int64  `db:"users"       json:"users"`
}

// ByClient breaks the same aggregate down per client, so the "all clients"
// dashboard can say which client the numbers came from instead of presenting a
// single unattributed total.
//
// Clients with no tickets still appear: "this client has raised nothing" is
// information, and dropping the row would make a newly created client look
// like it had failed to save.
func (r *Repository) ByClient(ctx context.Context, reach appctx.ClientReach, scope ticket.Scope, w *Window) ([]ClientBreakdown, error) {
	// ComplyDesk's own workspace is where staff accounts live, not a client, so
	// it never appears as a row of the client breakdown.
	where := []string{"tn.deleted_at IS NULL", "tn.is_platform = 0"}
	args := []any{}
	switch {
	case reach.All:
	case len(reach.TenantIDs) > 0:
		where = append(where, "tn.id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(reach.TenantIDs)...)
	default:
		where = append(where, "1 = 0")
	}

	// The caller's ticket scope is applied inside the correlated aggregate, not
	// to the client list, so a scoped user sees every client they may reach with
	// only the tickets they may count.
	scopeWhere := []string{}
	scopeArgs := []any{}
	scope.Apply(&scopeWhere, &scopeArgs)
	// The date window belongs in here for the same reason the scope does: it
	// narrows which tickets are counted, not which clients are listed. A client
	// with nothing in the chosen range stays on screen showing zero, which is
	// the honest answer — dropping the row would read as "no such client".
	w.Apply(&scopeWhere, &scopeArgs)
	scopeClause := ""
	if len(scopeWhere) > 0 {
		scopeClause = " AND " + strings.Join(scopeWhere, " AND ")
	}

	// Argument order follows the SELECT list, so the scope args are repeated
	// once per correlated subquery before the outer WHERE args.
	ordered := []any{}
	for i := 0; i < 4; i++ {
		ordered = append(ordered, scopeArgs...)
	}
	ordered = append(ordered, args...)

	rows := []ClientBreakdown{}
	q := `
		SELECT tn.slug, COALESCE(tn.client_code, '') AS client_code, tn.name, tn.status,
			(SELECT COUNT(*) FROM tickets t WHERE t.tenant_id = tn.id
			   AND t.deleted_at IS NULL` + scopeClause + `) AS total,
			(SELECT COUNT(*) FROM tickets t WHERE t.tenant_id = tn.id
			   AND t.deleted_at IS NULL AND t.status NOT IN ('CLOSED','CANCELLED')` + scopeClause + `) AS open,
			(SELECT COUNT(*) FROM tickets t WHERE t.tenant_id = tn.id
			   AND t.deleted_at IS NULL AND t.is_sla_breached = 1` + scopeClause + `) AS breached,
			(SELECT COUNT(*) FROM tickets t WHERE t.tenant_id = tn.id
			   AND t.deleted_at IS NULL AND t.assignee_id IS NULL
			   AND t.status NOT IN ('CLOSED','CANCELLED')` + scopeClause + `) AS unassigned,
			(SELECT COUNT(*) FROM users u WHERE u.tenant_id = tn.id AND u.deleted_at IS NULL) AS users
		FROM tenants tn
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY total DESC, tn.name`

	if err := r.db.Primary.SelectContext(ctx, &rows, q, ordered...); err != nil {
		return nil, fmt.Errorf("building client breakdown: %w", err)
	}
	return rows, nil
}
