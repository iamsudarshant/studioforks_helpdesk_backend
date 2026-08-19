package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/export"
	"github.com/karmamgmt/complydesk/internal/ticket"
)

// Definition describes one report: what it is called, what it answers, and the
// query behind it.
//
// The query is held here rather than in the database because it is code, not
// configuration — a report whose SQL could be edited at runtime is an
// injection surface, and every one of these joins across tables a client must
// never see rows from. Which client's data it runs against is supplied
// separately, and always from the session.
type Definition struct {
	Key         string   `json:"key"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Group       string   `json:"group"`
	Columns     []string `json:"columns"`
	// build returns the SELECT body after the caller's scope clause is applied.
	build func(clause string) string
}

// Definitions are the reports named in §12 of the development brief.
//
// Each is a single grouped query. None of them page: a report is generated to a
// file, and a partial file is worse than a slow one.
var Definitions = []Definition{
	{
		Key: "entity_wise", Title: "Entity-wise Ticket Report", Group: "Volume",
		Description: "Ticket volume and outcome for each establishment.",
		Columns:     []string{"Entity", "Code", "Total", "Open", "Resolved", "Closed", "Breached"},
		build: func(clause string) string {
			return `SELECT COALESCE(e.name,'Unassigned'), COALESCE(e.code,'-'),
				COUNT(*),
				COALESCE(SUM(t.status NOT IN ('CLOSED','CANCELLED')),0),
				COALESCE(SUM(t.status='RESOLVED'),0),
				COALESCE(SUM(t.status='CLOSED'),0),
				COALESCE(SUM(t.is_sla_breached=1),0)
			FROM tickets t LEFT JOIN entities e ON e.id = t.entity_id` + clause + `
			GROUP BY e.id, e.name, e.code ORDER BY COUNT(*) DESC`
		},
	},
	{
		Key: "pending", Title: "Pending Ticket Report", Group: "Operations",
		Description: "Everything still open, and who it is waiting on.",
		Columns:     []string{"Ticket", "Subject", "Status", "Priority", "Waiting on", "Assignee", "Age (days)"},
		build: func(clause string) string {
			return `SELECT t.ticket_number, t.subject, t.status, t.priority,
				CASE t.status WHEN 'PENDING_EMPLOYEE' THEN 'Employee'
				              WHEN 'PENDING_HELPDESK' THEN 'Helpdesk'
				              ELSE 'Helpdesk' END,
				COALESCE(CONCAT(au.first_name,' ',COALESCE(au.last_name,'')),'Unassigned'),
				TIMESTAMPDIFF(DAY, t.created_at, UTC_TIMESTAMP())
			FROM tickets t LEFT JOIN users au ON au.id = t.assignee_id` + clause + `
			  AND t.status NOT IN ('CLOSED','CANCELLED','RESOLVED')
			ORDER BY t.created_at`
		},
	},
	{
		Key: "closed", Title: "Closed Ticket Report", Group: "Operations",
		Description: "Tickets closed in the period, with the time they took.",
		Columns:     []string{"Ticket", "Subject", "Category", "Closed on", "Resolution hours", "Reopened"},
		build: func(clause string) string {
			return `SELECT t.ticket_number, t.subject, c.name, DATE(t.closed_at),
				ROUND(TIMESTAMPDIFF(MINUTE, t.created_at, t.closed_at)/60.0, 1),
				t.reopened_count
			FROM tickets t JOIN categories c ON c.id = t.category_id` + clause + `
			  AND t.closed_at IS NOT NULL ORDER BY t.closed_at DESC`
		},
	},
	{
		Key: "aging", Title: "Aging Report", Group: "Operations",
		Description: "Open tickets bucketed by age: 0-2, 3-5, 6-10 and 10+ days.",
		Columns:     []string{"Bucket", "Tickets", "Breached"},
		build: func(clause string) string {
			// The bucket expression is repeated in GROUP BY rather than aliased,
			// because MySQL cannot group by a select alias in every mode.
			bucket := `CASE
				WHEN TIMESTAMPDIFF(DAY, t.created_at, UTC_TIMESTAMP()) <= 2  THEN '0-2 days'
				WHEN TIMESTAMPDIFF(DAY, t.created_at, UTC_TIMESTAMP()) <= 5  THEN '3-5 days'
				WHEN TIMESTAMPDIFF(DAY, t.created_at, UTC_TIMESTAMP()) <= 10 THEN '6-10 days'
				ELSE '10+ days' END`
			return `SELECT ` + bucket + `, COUNT(*), COALESCE(SUM(t.is_sla_breached=1),0)
			FROM tickets t` + clause + `
			  AND t.status NOT IN ('CLOSED','CANCELLED')
			GROUP BY ` + bucket + `
			ORDER BY MIN(TIMESTAMPDIFF(DAY, t.created_at, UTC_TIMESTAMP()))`
		},
	},
	{
		Key: "monthly_summary", Title: "Monthly Query Summary", Group: "Volume",
		Description: "Tickets raised and resolved per month, by category.",
		Columns:     []string{"Month", "Category", "Raised", "Resolved", "Avg resolution hours"},
		build: func(clause string) string {
			return `SELECT DATE_FORMAT(t.created_at,'%Y-%m'), c.name, COUNT(*),
				COALESCE(SUM(t.resolved_at IS NOT NULL),0),
				ROUND(AVG(CASE WHEN t.resolved_at IS NOT NULL
					THEN TIMESTAMPDIFF(MINUTE,t.created_at,t.resolved_at)/60.0 END), 1)
			FROM tickets t JOIN categories c ON c.id = t.category_id` + clause + `
			GROUP BY 1, c.id, c.name ORDER BY 1 DESC, COUNT(*) DESC`
		},
	},
	{
		Key: "executive_performance", Title: "Executive-wise Performance", Group: "People",
		Description: "Tickets handled, average turnaround and SLA compliance per agent.",
		Columns:     []string{"Executive", "Handled", "Resolved", "Avg TAT hours", "SLA compliance %"},
		build: func(clause string) string {
			return `SELECT COALESCE(CONCAT(au.first_name,' ',COALESCE(au.last_name,'')),'Unassigned'),
				COUNT(*), COALESCE(SUM(t.resolved_at IS NOT NULL),0),
				ROUND(AVG(CASE WHEN t.resolved_at IS NOT NULL
					THEN TIMESTAMPDIFF(MINUTE,t.created_at,t.resolved_at)/60.0 END), 1),
				ROUND(CASE WHEN SUM(t.resolved_at IS NOT NULL) > 0
					THEN 100.0*SUM(t.resolved_at IS NOT NULL AND t.is_sla_breached=0)
					     /SUM(t.resolved_at IS NOT NULL) END, 1)
			FROM tickets t LEFT JOIN users au ON au.id = t.assignee_id` + clause + `
			GROUP BY au.id, au.first_name, au.last_name ORDER BY COUNT(*) DESC`
		},
	},
	{
		Key: "sla_breach", Title: "SLA Breach Report", Group: "SLA",
		Description: "Every ticket that missed its resolution target.",
		Columns:     []string{"Ticket", "Subject", "Priority", "Due", "Resolved", "Overdue hours", "Assignee"},
		build: func(clause string) string {
			return `SELECT t.ticket_number, t.subject, t.priority, t.resolution_due_at,
				t.resolved_at,
				ROUND(TIMESTAMPDIFF(MINUTE, t.resolution_due_at,
					COALESCE(t.resolved_at, UTC_TIMESTAMP()))/60.0, 1),
				COALESCE(CONCAT(au.first_name,' ',COALESCE(au.last_name,'')),'Unassigned')
			FROM tickets t LEFT JOIN users au ON au.id = t.assignee_id` + clause + `
			  AND t.is_sla_breached = 1 ORDER BY t.resolution_due_at`
		},
	},
	{
		Key: "reopened", Title: "Reopened Ticket Report", Group: "Quality",
		Description: "Tickets the requester was not satisfied with the first time.",
		Columns:     []string{"Ticket", "Subject", "Category", "Times reopened", "Last reopened", "Assignee"},
		build: func(clause string) string {
			return `SELECT t.ticket_number, t.subject, c.name, t.reopened_count,
				t.last_reopened_at,
				COALESCE(CONCAT(au.first_name,' ',COALESCE(au.last_name,'')),'Unassigned')
			FROM tickets t JOIN categories c ON c.id = t.category_id
			LEFT JOIN users au ON au.id = t.assignee_id` + clause + `
			  AND t.reopened_count > 0 ORDER BY t.reopened_count DESC, t.last_reopened_at DESC`
		},
	},
	{
		Key: "category_distribution", Title: "Category Distribution", Group: "Volume",
		Description: "Which query types the client actually raises.",
		Columns:     []string{"Module", "Category", "Query type", "Tickets", "Share %"},
		build: func(clause string) string {
			return `SELECT COALESCE(m.name,'-'), c.name, COALESCE(sc.name,'-'), COUNT(*),
				ROUND(100.0*COUNT(*)/NULLIF(SUM(COUNT(*)) OVER (), 0), 1)
			FROM tickets t
			JOIN categories c       ON c.id = t.category_id
			LEFT JOIN categories sc ON sc.id = t.subcategory_id
			LEFT JOIN modules m     ON m.id = c.module_id` + clause + `
			GROUP BY m.id, m.name, c.id, c.name, sc.id, sc.name ORDER BY COUNT(*) DESC`
		},
	},
	{
		Key: "first_response", Title: "First-Response-Time Report", Group: "SLA",
		Description: "How long the desk takes to pick a ticket up, by priority.",
		Columns:     []string{"Priority", "Tickets", "Responded", "Avg first response hours", "Within target %"},
		build: func(clause string) string {
			return `SELECT t.priority, COUNT(*),
				COALESCE(SUM(t.first_responded_at IS NOT NULL),0),
				ROUND(AVG(CASE WHEN t.first_responded_at IS NOT NULL
					THEN TIMESTAMPDIFF(MINUTE,t.created_at,t.first_responded_at)/60.0 END), 1),
				ROUND(CASE WHEN SUM(t.first_responded_at IS NOT NULL) > 0
					THEN 100.0*SUM(t.first_responded_at IS NOT NULL
						AND (t.first_response_due_at IS NULL
						     OR t.first_responded_at <= t.first_response_due_at))
					     /SUM(t.first_responded_at IS NOT NULL) END, 1)
			FROM tickets t` + clause + `
			GROUP BY t.priority ORDER BY FIELD(t.priority,'CRITICAL','HIGH','MEDIUM','LOW')`
		},
	},
	{
		Key: "employee_activity", Title: "Employee / Ex-Employee Activity", Group: "People",
		Description: "Who is raising tickets, and whether they still work here.",
		Columns:     []string{"Employee", "Employee ID", "Status", "Raised", "Open", "Last raised"},
		build: func(clause string) string {
			return `SELECT CONCAT(ru.first_name,' ',COALESCE(ru.last_name,'')),
				COALESCE(ru.employee_code,'-'), ru.status, COUNT(*),
				COALESCE(SUM(t.status NOT IN ('CLOSED','CANCELLED')),0),
				MAX(t.created_at)
			FROM tickets t JOIN users ru ON ru.id = t.requester_id` + clause + `
			GROUP BY ru.id ORDER BY COUNT(*) DESC`
		},
	},
}

// DefinitionByKey finds a report, or reports that it does not exist.
func DefinitionByKey(key string) (*Definition, bool) {
	for i := range Definitions {
		if Definitions[i].Key == key {
			return &Definitions[i], true
		}
	}
	return nil, false
}

// Run executes a report for one client, under the caller's own scope.
//
// The scope is what makes a Client Executive's copy of a report cover only
// their entity: the same definition produces different numbers for different
// people, which is the point.
func (r *Repository) Run(ctx context.Context, reach appctx.ClientReach, scope ticket.Scope, key string, p Params) (*Result, error) {
	def, ok := DefinitionByKey(key)
	if !ok {
		return nil, fmt.Errorf("unknown report %q", key)
	}

	clause, args := scoped(reach, scope)
	window, windowArgs := p.window()
	clause += window
	args = append(args, windowArgs...)

	rows, err := r.db.Primary.QueryContext(ctx, def.build(clause), args...)
	if err != nil {
		return nil, fmt.Errorf("running report %s: %w", key, err)
	}
	defer func() { _ = rows.Close() }()

	out := &export.Result{
		Key: def.Key, Title: def.Title, Columns: def.Columns,
		Rows: [][]any{}, GeneratedAt: time.Now().UTC(), From: p.From, To: p.To,
	}

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("reading report columns: %w", err)
	}

	for rows.Next() {
		// Scan into []any via pointers, because a report's column types vary by
		// definition and none of them are known at compile time.
		cells := make([]any, len(cols))
		targets := make([]any, len(cols))
		for i := range cells {
			targets[i] = &cells[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("reading report row: %w", err)
		}
		for i, cell := range cells {
			// The driver hands back []byte for text and decimals; a string is
			// what both JSON and CSV want.
			if b, ok := cell.([]byte); ok {
				cells[i] = string(b)
			}
		}
		out.Rows = append(out.Rows, cells)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating report rows: %w", err)
	}

	out.RowCount = len(out.Rows)
	return out, nil
}
