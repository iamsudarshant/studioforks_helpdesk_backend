package ticket

import (
	"context"
	"database/sql"
	"encoding/json"
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

const ticketColumns = `t.id, t.public_id, t.tenant_id, t.ticket_number, t.category_id,
	t.subcategory_id, t.subject, t.description, t.status, t.priority, t.source,
	t.requester_id, t.requester_snapshot_json, t.entity_id, t.site_id, t.department_id,
	t.assignee_id, t.custom_fields_json, t.sla_policy_id, t.first_response_due_at,
	t.resolution_due_at, t.first_responded_at, t.resolved_at, t.closed_at,
	t.reopened_count, t.last_reopened_at, t.escalation_level, t.is_sla_breached,
	t.sla_paused_at, t.sla_paused_total_mins, t.csat_score, t.csat_comment,
	t.parent_ticket_id, t.last_activity_at, t.created_by, t.updated_by,
	t.created_at, t.updated_at`

const ticketJoins = `
	FROM tickets t
	JOIN categories c        ON c.id = t.category_id
	LEFT JOIN categories sc  ON sc.id = t.subcategory_id
	JOIN users ru            ON ru.id = t.requester_id
	LEFT JOIN users au       ON au.id = t.assignee_id
	LEFT JOIN users cu       ON cu.id = t.created_by
	LEFT JOIN entities e     ON e.id = t.entity_id
	LEFT JOIN sites s        ON s.id = t.site_id
	LEFT JOIN departments d  ON d.id = t.department_id
	LEFT JOIN tenants tn     ON tn.id = t.tenant_id`

const ticketDisplay = `,
	c.name AS category_name, c.category_key AS category_key,
	sc.name AS subcategory_name,
	CONCAT(ru.first_name, ' ', COALESCE(ru.last_name, '')) AS requester_name,
	ru.employee_code AS requester_code,
	CONCAT(au.first_name, ' ', COALESCE(au.last_name, '')) AS assignee_name,
	CONCAT(cu.first_name, ' ', COALESCE(cu.last_name, '')) AS creator_name,
	e.name AS entity_name, s.name AS site_name, d.name AS department_name,
	tn.public_id AS tenant_public_id, tn.name AS tenant_name, tn.slug AS tenant_slug,
	tn.client_code AS tenant_client_code`

// requesterDetail is the requester's statutory identity, added to the three
// single-ticket lookups and to none of the list queries.
//
// A PF or ESIC query is answered against these numbers: the agent quotes the
// UAN to EPFO, the partner checks the ESIC number against their own register.
// A rail that shows a name and nothing else forces both of them into another
// screen mid-conversation, which is how a query gets answered against the wrong
// member. Kept out of the list because a page of fifty rows has no use for
// fifty people's identifiers.
const requesterDetail = `,
	ru.public_id AS requester_public_id,
	ru.email AS requester_email, ru.mobile AS requester_mobile,
	ru.uan_number AS requester_uan, ru.pf_number AS requester_pf,
	ru.esic_number AS requester_esic, ru.pan_number AS requester_pan,
	ru.designation AS requester_designation,
	ru.date_of_joining AS requester_doj, ru.date_of_birth AS requester_dob,
	ru.last_working_day AS requester_exit_date, ru.status AS requester_status,
	rue.name AS requester_entity_name, rud.name AS requester_dept_name`

// requesterJoins carries the requester's own entity and department, which are
// their posting — not the ticket's routing, which the ticket columns already
// hold and which may legitimately differ.
const requesterJoins = `
	LEFT JOIN entities rue    ON rue.id = ru.entity_id
	LEFT JOIN departments rud ON rud.id = ru.department_id`

// Scope describes what slice of the client's tickets a caller may see. It is
// built once from the actor and applied to every read, so a handler cannot
// forget it.
type Scope struct {
	// RequesterID limits the view to tickets raised by one person (an employee
	// viewing their own).
	RequesterID *int64
	// EntityIDs and friends limit a scoped partner executive or agent. Nil means
	// unrestricted on that dimension; an empty non-nil slice matches nothing.
	EntityIDs     []int64
	SiteIDs       []int64
	DepartmentIDs []int64
	CategoryIDs   []int64
}

// Apply appends the scope to a WHERE clause.
//
// Exported so the analytics package can reuse it: a dashboard that counted rows
// the caller cannot open would leak their existence through a number.
func (s Scope) Apply(where *[]string, args *[]any) {
	if s.RequesterID != nil {
		*where = append(*where, "t.requester_id = ?")
		*args = append(*args, *s.RequesterID)
	}

	addIn := func(col string, ids []int64) {
		if ids == nil {
			return
		}
		if len(ids) == 0 {
			// Explicitly scoped to nothing must match nothing, never everything.
			*where = append(*where, "1 = 0")
			return
		}
		*where = append(*where, col+" IN ("+platform.Placeholders(len(ids))+")")
		*args = append(*args, platform.Int64Args(ids)...)
	}
	addIn("t.entity_id", s.EntityIDs)
	addIn("t.site_id", s.SiteIDs)
	addIn("t.department_id", s.DepartmentIDs)
	addIn("t.category_id", s.CategoryIDs)
}

// ListFilter is the ticket-list query. Every field maps onto a whitelisted
// column; nothing here reaches SQL as text.
type ListFilter struct {
	Query        string
	Statuses     []string
	Priorities   []string
	CategoryIDs  []int64
	EntityIDs    []int64
	SiteIDs      []int64
	DeptIDs      []int64
	AssigneeIDs  []int64
	RequesterIDs []int64
	Sources      []string
	Unassigned   bool
	Breached     bool
	Reopened     bool
	Escalated    bool
	CreatedFrom  *time.Time
	CreatedTo    *time.Time
	UpdatedFrom  *time.Time
	UpdatedTo    *time.Time

	// Targeted lookups. Each searches one field rather than the broad `Query`
	// sweep, so "employee code = DM-EMP-003" cannot also match a subject that
	// happens to contain it.
	TicketNumber string
	EmployeeName string
	EmployeeCode string
	UANNumber    string
	PFNumber     string
	ESICNumber   string

	// SLAState is one of ok, warning, serious, breached, paused.
	SLAState string

	Scope Scope
	Page  platform.Page

	// TenantIDs restricts the query to a set of clients. Staff listing across
	// clients set this; a normal tenant-scoped list leaves it nil.
	TenantIDs []int64
	// AllTenants tells the query to ignore the single-tenant constraint
	// entirely. Only staff holding ticket.view.all may set it.
	AllTenants bool
}

// applyReach translates the caller's client reach onto a list filter.
//
// Kept next to ListFilter rather than in the handler so that every list — the
// agent's, the admin's cross-client one, the counts strip — narrows the same
// way, and a new list cannot forget to.
func applyReach(f *ListFilter, reach appctx.ClientReach) {
	switch {
	case reach.All:
		f.AllTenants, f.TenantIDs = true, nil
	case len(reach.TenantIDs) > 0:
		f.AllTenants, f.TenantIDs = false, reach.TenantIDs
	default:
		// No reach must match nothing. An empty TenantIDs would fall through to
		// the single-tenant branch, so say it explicitly.
		f.AllTenants, f.TenantIDs = false, nil
		f.Scope = denyAll()
	}
}

// Sortable whitelists the columns a caller may order by, keyed by the grid's
// own column field names as well as the shorter API ones — the browser sends
// whichever field the clicked header belongs to, and an unrecognised key is
// dropped, which reads as "sorting does not work".
var Sortable = map[string]string{
	"ticket_number":    "t.ticket_number",
	"subject":          "t.subject",
	"status":           "t.status",
	"priority":         "t.priority",
	"created_at":       "t.created_at",
	"updated_at":       "t.updated_at",
	"last_activity":    "t.last_activity_at",
	"last_activity_at": "t.last_activity_at",
	"resolution_due":   "t.resolution_due_at",
	"sla":              "t.resolution_due_at",
	"age_mins":         "t.created_at",
	"requester":        "ru.first_name",
	"assignee":         "au.first_name",
	"category":         "c.name",
	"entity":           "e.name",
	"site":             "s.name",
	"department":       "d.name",
	"client":           "tn.name",
}

func (f ListFilter) where() ([]string, []any) {
	where := []string{"t.deleted_at IS NULL"}
	args := []any{}

	switch {
	case len(f.TenantIDs) > 0:
		where = append(where, "t.tenant_id IN ("+platform.Placeholders(len(f.TenantIDs))+")")
		args = append(args, platform.Int64Args(f.TenantIDs)...)
	case f.AllTenants:
		// The staff cross-client view: every client's tickets.
	default:
		// Ordinary tenant-scoped list; List prepends the tenant id to args.
		where = append(where, "t.tenant_id = ?")
	}

	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, `(t.ticket_number LIKE ? OR t.subject LIKE ?
			OR ru.first_name LIKE ? OR ru.last_name LIKE ? OR ru.employee_code LIKE ?
			OR ru.pf_number LIKE ? OR ru.uan_number LIKE ? OR ru.esic_number LIKE ?)`)
		like := "%" + q + "%"
		for i := 0; i < 8; i++ {
			args = append(args, like)
		}
	}

	addStrIn := func(col string, values []string) {
		if len(values) > 0 {
			where = append(where, col+" IN ("+platform.Placeholders(len(values))+")")
			args = append(args, platform.StringArgs(values)...)
		}
	}
	addIDIn := func(col string, ids []int64) {
		if len(ids) > 0 {
			where = append(where, col+" IN ("+platform.Placeholders(len(ids))+")")
			args = append(args, platform.Int64Args(ids)...)
		}
	}

	addStrIn("t.status", f.Statuses)
	addStrIn("t.priority", f.Priorities)
	addStrIn("t.source", f.Sources)
	addIDIn("t.category_id", f.CategoryIDs)
	addIDIn("t.entity_id", f.EntityIDs)
	addIDIn("t.site_id", f.SiteIDs)
	addIDIn("t.department_id", f.DeptIDs)
	addIDIn("t.assignee_id", f.AssigneeIDs)
	addIDIn("t.requester_id", f.RequesterIDs)

	// Targeted field lookups. `LIKE` with a trailing wildcard only, so a partial
	// ticket number or employee code still matches from the start while the
	// index on that column stays usable.
	addPrefix := func(col, value string) {
		if v := strings.TrimSpace(value); v != "" {
			where = append(where, col+" LIKE ?")
			args = append(args, v+"%")
		}
	}
	addPrefix("t.ticket_number", f.TicketNumber)
	addPrefix("ru.employee_code", f.EmployeeCode)
	addPrefix("ru.uan_number", f.UANNumber)
	addPrefix("ru.pf_number", f.PFNumber)
	addPrefix("ru.esic_number", f.ESICNumber)

	if v := strings.TrimSpace(f.EmployeeName); v != "" {
		where = append(where,
			"CONCAT(ru.first_name, ' ', COALESCE(ru.last_name, '')) LIKE ?")
		args = append(args, "%"+v+"%")
	}

	if f.Unassigned {
		where = append(where, "t.assignee_id IS NULL")
	}
	if f.Breached {
		where = append(where, "t.is_sla_breached = 1")
	}
	if f.Reopened {
		where = append(where, "t.reopened_count > 0")
	}
	if f.Escalated {
		where = append(where, "t.escalation_level > 0")
	}

	// SLA state. Measured against the resolution deadline for anything still
	// open: "serious" is inside a quarter of the window, "warning" inside half.
	switch f.SLAState {
	case "breached":
		where = append(where, "t.is_sla_breached = 1")
	case "paused":
		where = append(where, "t.sla_paused_at IS NOT NULL")
	case "ok":
		where = append(where, `t.is_sla_breached = 0
			AND (t.resolution_due_at IS NULL
			     OR t.resolution_due_at > DATE_ADD(UTC_TIMESTAMP(), INTERVAL 24 HOUR))`)
	case "warning":
		where = append(where, `t.is_sla_breached = 0 AND t.resolution_due_at IS NOT NULL
			AND t.resolution_due_at BETWEEN DATE_ADD(UTC_TIMESTAMP(), INTERVAL 4 HOUR)
			                            AND DATE_ADD(UTC_TIMESTAMP(), INTERVAL 24 HOUR)`)
	case "serious":
		where = append(where, `t.is_sla_breached = 0 AND t.resolution_due_at IS NOT NULL
			AND t.resolution_due_at <= DATE_ADD(UTC_TIMESTAMP(), INTERVAL 4 HOUR)`)
	}

	if f.CreatedFrom != nil {
		where = append(where, "t.created_at >= ?")
		args = append(args, *f.CreatedFrom)
	}
	if f.CreatedTo != nil {
		where = append(where, "t.created_at <= ?")
		args = append(args, *f.CreatedTo)
	}
	if f.UpdatedFrom != nil {
		where = append(where, "t.last_activity_at >= ?")
		args = append(args, *f.UpdatedFrom)
	}
	if f.UpdatedTo != nil {
		where = append(where, "t.last_activity_at <= ?")
		args = append(args, *f.UpdatedTo)
	}

	f.Scope.Apply(&where, &args)
	return where, args
}

// List returns a page of tickets plus the total, with the caller's scope
// already applied.
func (r *Repository) List(ctx context.Context, tenantID int64, f ListFilter) ([]Ticket, int64, error) {
	where, filterArgs := f.where()
	args := filterArgs
	if len(f.TenantIDs) == 0 && !f.AllTenants {
		// Single-tenant list: the tenant id is the first placeholder.
		args = append([]any{tenantID}, args...)
	}
	clause := " WHERE " + strings.Join(where, " AND ")

	var total int64
	if err := r.db.Primary.GetContext(ctx, &total,
		`SELECT COUNT(*)`+ticketJoins+clause, args...); err != nil {
		return nil, 0, fmt.Errorf("counting tickets: %w", err)
	}

	sortBy := f.Page.SortBy
	if sortBy == "" {
		sortBy = "t.last_activity_at"
	}

	rows := []Ticket{}
	q := `SELECT ` + ticketColumns + ticketDisplay + ticketJoins + clause +
		fmt.Sprintf(" ORDER BY %s %s, t.id DESC LIMIT ? OFFSET ?", sortBy, f.Page.SortDir)
	args = append(args, f.Page.PerPage, f.Page.Offset())

	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, 0, fmt.Errorf("listing tickets: %w", err)
	}
	return rows, total, nil
}

// CountSummary is what the ticket list header renders: a badge per status tab
// and a count per quick-filter chip.
//
// The quick counts are deliberately computed here in one pass rather than left
// to the client, because the client cannot see the rows its scope excludes and
// would otherwise under-report.
type CountSummary struct {
	ByStatus map[string]int64 `json:"by_status"`
	Quick    QuickCounts      `json:"quick"`
}

// QuickCounts mirrors the chips in `buildQuickFilters` on the client, one field
// per chip and in the same order.
type QuickCounts struct {
	All         int64 `db:"all_count"   json:"all"`
	MyTickets   int64 `db:"my_tickets"  json:"my_tickets"`
	Unassigned  int64 `db:"unassigned"  json:"unassigned"`
	PendingDept int64 `db:"pending_dept" json:"pending_dept"`
	PendingUser int64 `db:"pending_user" json:"pending_user"`
	Breached    int64 `db:"breached"    json:"breached"`
	Reopened    int64 `db:"reopened"    json:"reopened"`
	Escalated   int64 `db:"escalated"   json:"escalated"`
}

// reachClause renders the client restriction shared by the count queries.
func reachClause(reach appctx.ClientReach) ([]string, []any) {
	where := []string{"t.deleted_at IS NULL"}
	args := []any{}
	switch {
	case reach.All:
		// Every client.
	case len(reach.TenantIDs) > 0:
		where = append(where, "t.tenant_id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(reach.TenantIDs)...)
	default:
		where = append(where, "1 = 0")
	}
	return where, args
}

// Summary returns the status and quick-filter counts for one caller.
//
// `userID` is the viewer, needed for "my tickets" — a ticket is mine when I am
// either working it or waiting on it.
func (r *Repository) Summary(ctx context.Context, reach appctx.ClientReach, userID int64, scope Scope) (*CountSummary, error) {
	byStatus, err := r.Counts(ctx, reach, scope)
	if err != nil {
		return nil, err
	}

	where, args := reachClause(reach)
	scope.Apply(&where, &args)
	clause := " WHERE " + strings.Join(where, " AND ")

	var q QuickCounts
	// One aggregate pass: eight COUNTs over the same scanned rows costs the same
	// as the one the tabs already needed.
	err = r.db.Primary.GetContext(ctx, &q, `
		SELECT
			COUNT(*)                                                      AS all_count,
			COALESCE(SUM(t.assignee_id = ? OR t.requester_id = ?), 0)     AS my_tickets,
			COALESCE(SUM(t.assignee_id IS NULL
			             AND t.status NOT IN ('CLOSED','CANCELLED')), 0)  AS unassigned,
			COALESCE(SUM(t.status = 'PENDING_HELPDESK'), 0)               AS pending_dept,
			COALESCE(SUM(t.status = 'PENDING_EMPLOYEE'), 0)               AS pending_user,
			COALESCE(SUM(t.is_sla_breached = 1), 0)                       AS breached,
			COALESCE(SUM(t.reopened_count > 0), 0)                        AS reopened,
			COALESCE(SUM(t.escalation_level > 0), 0)                      AS escalated
		FROM tickets t`+clause,
		append([]any{userID, userID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("counting quick filters: %w", err)
	}

	return &CountSummary{ByStatus: byStatus, Quick: q}, nil
}

// Counts returns per-status totals for the caller's scope, which is what the
// list tabs badge.
func (r *Repository) Counts(ctx context.Context, reach appctx.ClientReach, scope Scope) (map[string]int64, error) {
	where, args := reachClause(reach)
	scope.Apply(&where, &args)

	rows := []struct {
		Status string `db:"status"`
		Count  int64  `db:"c"`
	}{}
	q := `SELECT t.status, COUNT(*) AS c FROM tickets t WHERE ` +
		strings.Join(where, " AND ") + ` GROUP BY t.status`

	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("counting tickets by status: %w", err)
	}

	// Seed every status at zero so the UI renders a stable set of tabs.
	out := map[string]int64{}
	for _, s := range append(append([]string{}, OpenStatuses...), ClosedStatuses...) {
		out[s] = 0
	}
	out[StatusResolved] = 0

	var total, open int64
	for _, row := range rows {
		out[row.Status] = row.Count
		total += row.Count
		for _, s := range OpenStatuses {
			if row.Status == s {
				open += row.Count
			}
		}
	}
	out["ALL"] = total
	out["OPEN_TOTAL"] = open
	return out, nil
}

// ResolveClientID maps a client slug or code onto its id, for the admin's
// "list one client's tickets" filter. The slug wins on collision, mirroring
// BySlug in the tenant package.
func (r *Repository) ResolveClientID(ctx context.Context, slugOrCode string) (*int64, error) {
	var id int64
	err := r.db.Primary.GetContext(ctx, &id, `
		SELECT id FROM tenants
		WHERE deleted_at IS NULL AND is_platform = 0 AND (slug = ? OR client_code = ?)
		ORDER BY (slug = ?) DESC
		LIMIT 1`, slugOrCode, slugOrCode, slugOrCode)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("resolving client: %w", err)
	}
	return &id, nil
}

// AllClientIDs lists every active client for the admin's cross-client view.
func (r *Repository) AllClientIDs(ctx context.Context) ([]int64, error) {
	ids := []int64{}
	err := r.db.Primary.SelectContext(ctx, &ids, `
		SELECT id FROM tenants
		WHERE deleted_at IS NULL AND is_platform = 0 AND status = 'ACTIVE'
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing client ids: %w", err)
	}
	return ids, nil
}

// HasEntityReplyGrant reports whether a user has been granted reply rights on a
// ticket's entity. A partner assigned to the entity without the grant may view
// and work the tickets but cannot reply.
func (r *Repository) HasEntityReplyGrant(ctx context.Context, tenantID, entityID, userID int64) (bool, error) {
	var n int
	err := r.db.Primary.GetContext(ctx, &n, `
		SELECT COUNT(*) FROM entity_assignments
		WHERE tenant_id = ? AND entity_id = ? AND user_id = ? AND can_reply = 1`,
		tenantID, entityID, userID)
	if err != nil {
		return false, fmt.Errorf("checking entity reply grant: %w", err)
	}
	return n > 0, nil
}

func (r *Repository) ByPublicID(ctx context.Context, tenantID int64, publicID string) (*Ticket, error) {
	var t Ticket
	q := `SELECT ` + ticketColumns + ticketDisplay + requesterDetail + ticketJoins + requesterJoins +
		` WHERE t.tenant_id = ? AND t.public_id = ? AND t.deleted_at IS NULL`

	if err := r.db.Primary.GetContext(ctx, &t, q, tenantID, publicID); err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading ticket: %w", err)
	}
	return &t, nil
}

// ByPublicIDInReach loads a ticket from anywhere the caller can reach.
//
// The list is cross-client for staff who have not chosen one from the switcher,
// so opening a row from that list has to resolve without knowing which client
// it belongs to first — pinning the lookup to the resolved tenant would look up
// the platform workspace and answer "not found" for every row on screen.
//
// The reach is still the boundary: a ticket outside it is not found, exactly as
// if it did not exist. The per-ticket CanSee check in Load runs on top.
func (r *Repository) ByPublicIDInReach(ctx context.Context, reach appctx.ClientReach, publicID string) (*Ticket, error) {
	where := []string{"t.public_id = ?", "t.deleted_at IS NULL"}
	args := []any{publicID}

	switch {
	case reach.All:
		// Every client.
	case len(reach.TenantIDs) > 0:
		where = append(where, "t.tenant_id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(reach.TenantIDs)...)
	default:
		where = append(where, "1 = 0")
	}

	var t Ticket
	q := `SELECT ` + ticketColumns + ticketDisplay + requesterDetail + ticketJoins + requesterJoins +
		` WHERE ` + strings.Join(where, " AND ")

	if err := r.db.Primary.GetContext(ctx, &t, q, args...); err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading ticket: %w", err)
	}
	return &t, nil
}

func (r *Repository) ByID(ctx context.Context, tenantID, id int64) (*Ticket, error) {
	var t Ticket
	q := `SELECT ` + ticketColumns + ticketDisplay + requesterDetail + ticketJoins + requesterJoins +
		` WHERE t.tenant_id = ? AND t.id = ? AND t.deleted_at IS NULL`

	if err := r.db.Primary.GetContext(ctx, &t, q, tenantID, id); err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading ticket: %w", err)
	}
	return &t, nil
}

// --- creation ---------------------------------------------------------------

type CreateParams struct {
	CategoryID    int64
	SubcategoryID *int64
	Subject       string
	Description   string
	Priority      string
	Source        string
	RequesterID   int64
	EntityID      *int64
	SiteID        *int64
	DepartmentID  *int64
	CustomFields  string
	DocumentIDs   []int64
	CreatedBy     *int64

	// Snapshot freezes the requester's identity at creation so a later profile
	// edit cannot rewrite the ticket's history.
	Snapshot map[string]any
}

// Create allocates a gapless ticket number and writes the ticket, its opening
// timeline entry and any attachments in one transaction.
func (r *Repository) Create(ctx context.Context, tenantID int64, p CreateParams) (*Ticket, error) {
	var created *Ticket

	err := platform.RetryOnDeadlock(ctx, 3, func() error {
		return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
			// The category supplies the ticket-number prefix, the SLA policy and
			// the default department, so a client can change routing without a
			// code change.
			row := struct {
				Prefix string        `db:"ticket_prefix"`
				SLA    sql.NullInt64 `db:"sla_policy_id"`
				Dept   sql.NullInt64 `db:"default_department_id"`
			}{}
			if err := tx.GetContext(ctx, &row, `
				SELECT ticket_prefix, sla_policy_id, default_department_id
				FROM categories WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
				tenantID, p.CategoryID); err != nil {
				if platform.IsNotFound(err) {
					return platform.ErrSentinelNotFound
				}
				return fmt.Errorf("loading category: %w", err)
			}

			// The client code prefixes the number. Read inside the transaction
			// rather than passed in, so the number can never be built from a
			// stale code the caller cached earlier.
			var clientCode sql.NullString
			if err := tx.GetContext(ctx, &clientCode,
				`SELECT client_code FROM tenants WHERE id = ?`, tenantID); err != nil {
				return fmt.Errorf("loading client code: %w", err)
			}

			number, err := nextTicketNumber(ctx, tx, tenantID, clientCode.String, row.Prefix)
			if err != nil {
				return err
			}

			snapshot, err := json.Marshal(p.Snapshot)
			if err != nil {
				return fmt.Errorf("encoding requester snapshot: %w", err)
			}

			department := p.DepartmentID
			if department == nil && row.Dept.Valid {
				department = &row.Dept.Int64
			}

			priority := p.Priority
			if priority == "" {
				priority = PriorityMedium
			}
			source := p.Source
			if source == "" {
				source = SourcePortal
			}

			publicID := platform.NewULID()
			res, err := tx.ExecContext(ctx, `
				INSERT INTO tickets
					(public_id, tenant_id, ticket_number, category_id, subcategory_id, subject,
					 description, status, priority, source, requester_id, requester_snapshot_json,
					 entity_id, site_id, department_id, custom_fields_json, sla_policy_id,
					 last_activity_at, created_by)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3),?)`,
				publicID, tenantID, number, p.CategoryID, p.SubcategoryID, p.Subject,
				// NEW, not OPEN: a ticket nobody has looked at yet. The move to
				// OPEN is what records first response.
				nullStr(p.Description), StatusNew, priority, source, p.RequesterID, string(snapshot),
				p.EntityID, p.SiteID, department, nullStr(p.CustomFields), row.SLA, p.CreatedBy)
			if err != nil {
				return fmt.Errorf("creating ticket: %w", err)
			}

			ticketID, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("reading ticket id: %w", err)
			}

			for _, docID := range p.DocumentIDs {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO ticket_attachments
						(tenant_id, ticket_id, document_id, context, uploaded_by)
					VALUES (?,?,?, 'REQUESTER', ?)`,
					tenantID, ticketID, docID, p.CreatedBy); err != nil {
					return fmt.Errorf("attaching document: %w", err)
				}
			}

			if err := writeTimeline(ctx, tx, tenantID, ticketID, timelineParams{
				EventType: EventCreated,
				ActorID:   p.CreatedBy,
				Summary:   "Ticket raised",
				Detail:    map[string]any{"ticket_number": number, "priority": priority},
			}); err != nil {
				return err
			}

			var t Ticket
			q := `SELECT ` + ticketColumns + ticketDisplay + ticketJoins + ` WHERE t.id = ?`
			if err := tx.GetContext(ctx, &t, q, ticketID); err != nil {
				return fmt.Errorf("reloading ticket: %w", err)
			}
			created = &t
			return nil
		})
	})

	return created, err
}

// TicketNumberFormat documents the identifier employees quote on the phone:
//
//	{CLIENT_CODE}-{CATEGORY_PREFIX}-{YEAR}-{SEQUENCE}
//	INF-PF-2026-000145
//
// Each part earns its place. The client code makes a number unambiguous when
// two clients are discussed in the same breath; the category prefix tells an
// agent what the ticket is about before they open it; the year keeps sequences
// short and lets them restart annually; the six-digit sequence sorts correctly
// as text and leaves room for a million tickets per client, per category, per
// year.
const TicketNumberFormat = "%s-%s-%d-%06d"

// nextTicketNumber allocates the next number for one (client, category, year).
//
// Concurrency is the whole problem here. Two employees raising a PF ticket in
// the same second must not receive the same number, and the sequence must not
// skip — support staff read gaps as lost tickets.
//
// The guarantee comes from three things together:
//
//   - The caller runs this inside a transaction. Every statement below shares
//     that transaction, so the allocation commits or rolls back with the ticket
//     it belongs to. A ticket can never exist without its number, nor a number
//     be burned by a ticket that failed to insert.
//   - INSERT ... ON DUPLICATE KEY UPDATE creates the counter row if this is the
//     first ticket of the year, and is itself atomic — two callers racing to
//     create the same row cannot both win.
//   - SELECT ... FOR UPDATE takes an exclusive row lock held to commit. A second
//     transaction blocks on that read rather than seeing a stale value, so the
//     read-increment-write is serialised without an application-level mutex
//     (which would only work within one process anyway).
//
// The counter is keyed by (tenant, prefix, year), so PF and ESIC number
// independently and every client starts at 1.
func nextTicketNumber(ctx context.Context, tx *sqlx.Tx, tenantID int64, clientCode, prefix string) (string, error) {
	year := time.Now().UTC().Year()

	// Ensure the counter row exists. `last_value = last_value` is a deliberate
	// no-op: an existing counter must not be reset by a concurrent first insert.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ticket_sequences (tenant_id, prefix, year, last_value)
		VALUES (?,?,?,0)
		ON DUPLICATE KEY UPDATE last_value = last_value`,
		tenantID, prefix, year); err != nil {
		return "", fmt.Errorf("preparing ticket sequence: %w", err)
	}

	// Serialise on the counter row. Everything from here to COMMIT is exclusive.
	var last int64
	if err := tx.GetContext(ctx, &last, `
		SELECT last_value FROM ticket_sequences
		WHERE tenant_id = ? AND prefix = ? AND year = ? FOR UPDATE`,
		tenantID, prefix, year); err != nil {
		return "", fmt.Errorf("locking ticket sequence: %w", err)
	}

	next := last + 1
	if _, err := tx.ExecContext(ctx, `
		UPDATE ticket_sequences SET last_value = ?
		WHERE tenant_id = ? AND prefix = ? AND year = ?`,
		next, tenantID, prefix, year); err != nil {
		return "", fmt.Errorf("advancing ticket sequence: %w", err)
	}

	// A client without a code still gets a well-formed number rather than a
	// leading dash: onboarding can create tickets before the code is filled in.
	if clientCode == "" {
		return fmt.Sprintf("%s-%d-%06d", prefix, year, next), nil
	}
	return fmt.Sprintf(TicketNumberFormat, clientCode, prefix, year, next), nil
}

// --- timeline ---------------------------------------------------------------

type timelineParams struct {
	EventType  string
	ActorID    *int64
	ActorName  string
	ActorRole  string
	Visibility string
	Summary    string
	Detail     map[string]any
}

// writeTimeline appends an immutable activity record. Every mutation calls this
// inside its own transaction, so the trail cannot drift from the data.
func writeTimeline(ctx context.Context, tx *sqlx.Tx, tenantID, ticketID int64, p timelineParams) error {
	var detail any
	if len(p.Detail) > 0 {
		raw, err := json.Marshal(p.Detail)
		if err != nil {
			return fmt.Errorf("encoding timeline detail: %w", err)
		}
		detail = string(raw)
	}

	visibility := p.Visibility
	if visibility == "" {
		visibility = VisibilityPublic
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ticket_timeline
			(public_id, tenant_id, ticket_id, event_type, actor_id, actor_name_snapshot,
			 actor_role, visibility, summary, detail_json)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		platform.NewULID(), tenantID, ticketID, p.EventType, p.ActorID,
		nullStr(p.ActorName), nullStr(p.ActorRole), visibility, p.Summary, detail); err != nil {
		return fmt.Errorf("writing timeline entry: %w", err)
	}
	return nil
}

// Timeline returns a ticket's activity. includeInternal is false for employees
// and partners, so internal-only entries never reach them.
func (r *Repository) Timeline(ctx context.Context, tenantID, ticketID int64, includeInternal bool) ([]TimelineEntry, error) {
	where := "tl.tenant_id = ? AND tl.ticket_id = ?"
	if !includeInternal {
		where += " AND tl.visibility = 'PUBLIC'"
	}

	// The snapshot first, then the user row.
	//
	// Not every writer stamps a name — the create path had none, so every
	// ticket opened with an unattributed "Ticket raised" — and the trail is
	// unreadable without one: "who did this" is the second thing anyone asks of
	// an activity log. The snapshot still wins where it exists, because it
	// records the name as it was at the time.
	rows := []TimelineEntry{}
	q := `SELECT tl.id, tl.public_id, tl.ticket_id, tl.event_type, tl.actor_id,
	             COALESCE(
	               NULLIF(TRIM(tl.actor_name_snapshot), ''),
	               NULLIF(TRIM(CONCAT(au.first_name, ' ', COALESCE(au.last_name, ''))), '')
	             ) AS actor_name_snapshot,
	             tl.actor_role, tl.visibility, tl.summary,
	             tl.detail_json, tl.created_at
	      FROM ticket_timeline tl
	      LEFT JOIN users au ON au.id = tl.actor_id
	      WHERE ` + where + ` ORDER BY tl.created_at, tl.id`

	if err := r.db.Primary.SelectContext(ctx, &rows, q, tenantID, ticketID); err != nil {
		return nil, fmt.Errorf("loading timeline: %w", err)
	}
	return rows, nil
}

func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// ResolveInReach turns public ids into internal ones for one of the reference
// tables the ticket filter bar offers.
//
// `table` is checked against a fixed allowlist, never interpolated from the
// request: these are the only five tables a filter can name, and anything else
// is a programming error rather than a value to pass through.
func (r *Repository) ResolveInReach(ctx context.Context, reach appctx.ClientReach,
	table string, publicIDs []string) ([]int64, error) {

	switch table {
	case "categories", "entities", "sites", "departments", "users":
	default:
		return nil, fmt.Errorf("unknown reference table %q", table)
	}
	if len(publicIDs) == 0 {
		return nil, nil
	}

	where := []string{"public_id IN (" + platform.Placeholders(len(publicIDs)) + ")"}
	args := platform.StringArgs(publicIDs)

	switch {
	case reach.All:
		// Every client.
	case len(reach.TenantIDs) > 0:
		where = append(where, "tenant_id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(reach.TenantIDs)...)
	default:
		where = append(where, "1 = 0")
	}

	// Every one of these tables is soft-deleted, so a removed record must not
	// still be selectable as a filter.
	where = append(where, "deleted_at IS NULL")

	ids := []int64{}
	q := `SELECT id FROM ` + table + ` WHERE ` + strings.Join(where, " AND ")
	if err := r.db.Primary.SelectContext(ctx, &ids, q, args...); err != nil {
		return nil, fmt.Errorf("resolving %s filter: %w", table, err)
	}
	return ids, nil
}
