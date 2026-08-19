package help

import (
	"context"
	"fmt"
	"strings"

	"github.com/karmamgmt/complydesk/internal/platform"
)

type Repository struct {
	db *platform.DB
}

func NewRepository(db *platform.DB) *Repository { return &Repository{db: db} }

// --- FAQ --------------------------------------------------------------------

const faqColumns = `f.id, f.public_id, f.tenant_id, f.section, f.question, f.answer,
	f.sort_order, f.is_active, f.created_by, f.created_at, f.updated_at,
	CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS author_name`

const faqFrom = ` FROM faq_articles f LEFT JOIN users u ON u.id = f.created_by `

type FAQParams struct {
	Section   string
	Question  string
	Answer    string
	SortOrder int
	IsActive  bool
	CreatedBy *int64
}

func (r *Repository) FAQArticles(ctx context.Context, tenantID int64, activeOnly bool, query string) ([]FAQArticle, error) {
	where := []string{"f.tenant_id = ?", "f.deleted_at IS NULL"}
	args := []any{tenantID}
	if activeOnly {
		where = append(where, "f.is_active = 1")
	}
	if q := strings.TrimSpace(query); q != "" {
		where = append(where, "(f.question LIKE ? OR f.answer LIKE ?)")
		args = append(args, "%"+q+"%", "%"+q+"%")
	}

	rows := []FAQArticle{}
	q := `SELECT ` + faqColumns + faqFrom + ` WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY f.section, f.sort_order, f.question`
	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("listing FAQ articles: %w", err)
	}
	return rows, nil
}

func (r *Repository) FAQByPublicID(ctx context.Context, tenantID int64, publicID string) (*FAQArticle, error) {
	var a FAQArticle
	err := r.db.Primary.GetContext(ctx, &a,
		`SELECT `+faqColumns+faqFrom+` WHERE f.tenant_id = ? AND f.public_id = ? AND f.deleted_at IS NULL`,
		tenantID, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading FAQ article: %w", err)
	}
	return &a, nil
}

func (r *Repository) faqByID(ctx context.Context, tenantID, id int64) (*FAQArticle, error) {
	var a FAQArticle
	err := r.db.Primary.GetContext(ctx, &a,
		`SELECT `+faqColumns+faqFrom+` WHERE f.tenant_id = ? AND f.id = ? AND f.deleted_at IS NULL`,
		tenantID, id)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading FAQ article: %w", err)
	}
	return &a, nil
}

func (r *Repository) CreateFAQ(ctx context.Context, tenantID int64, p FAQParams) (*FAQArticle, error) {
	res, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO faq_articles (public_id, tenant_id, section, question, answer, sort_order, is_active, created_by)
		VALUES (?,?,?,?,?,?,?,?)`,
		platform.NewULID(), tenantID, strings.ToUpper(strings.TrimSpace(p.Section)), p.Question, p.Answer,
		p.SortOrder, p.IsActive, p.CreatedBy)
	if err != nil {
		return nil, fmt.Errorf("creating FAQ article: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading FAQ article id: %w", err)
	}
	return r.faqByID(ctx, tenantID, id)
}

// FAQUpdate is a partial article update. Empty question/answer/section are
// ignored so a PATCH can touch just one field.
type FAQUpdate struct {
	Section   *string
	Question  *string
	Answer    *string
	SortOrder *int
	IsActive  *bool
}

func (r *Repository) UpdateFAQ(ctx context.Context, tenantID int64, id int64, u FAQUpdate) error {
	set, args := []string{}, []any{}
	if u.Section != nil {
		set, args = append(set, "section = ?"), append(args, strings.ToUpper(strings.TrimSpace(*u.Section)))
	}
	if u.Question != nil {
		set, args = append(set, "question = ?"), append(args, *u.Question)
	}
	if u.Answer != nil {
		set, args = append(set, "answer = ?"), append(args, *u.Answer)
	}
	if u.SortOrder != nil {
		set, args = append(set, "sort_order = ?"), append(args, *u.SortOrder)
	}
	if u.IsActive != nil {
		set, args = append(set, "is_active = ?"), append(args, *u.IsActive)
	}
	if len(set) == 0 {
		return nil
	}
	args = append(args, tenantID, id)
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE faq_articles SET `+strings.Join(set, ", ")+` WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
		args...)
	if err != nil {
		return fmt.Errorf("updating FAQ article: %w", err)
	}
	return affected(res)
}

func (r *Repository) DeleteFAQ(ctx context.Context, tenantID, id int64) error {
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE faq_articles SET deleted_at = UTC_TIMESTAMP(3), is_active = 0
		 WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, tenantID, id)
	if err != nil {
		return fmt.Errorf("deleting FAQ article: %w", err)
	}
	return affected(res)
}

// --- help tickets -----------------------------------------------------------

const helpTicketColumns = `t.id, t.public_id, t.tenant_id, t.client_id, t.requester_id,
	t.subject, t.category, t.body, t.status, t.priority, t.assigned_to, t.resolved_by,
	t.resolved_at, t.created_at, t.updated_at,
	rq.public_id AS requester_public_id,
	CONCAT(rq.first_name, ' ', COALESCE(rq.last_name, '')) AS requester_name,
	rq.email AS requester_email,
	asg.public_id AS assignee_public_id,
	CONCAT(asg.first_name, ' ', COALESCE(asg.last_name, '')) AS assignee_name,
	(SELECT COUNT(*) FROM help_ticket_replies hr WHERE hr.help_ticket_id = t.id) AS reply_count`

const helpTicketFrom = ` FROM help_tickets t
	JOIN users rq ON rq.id = t.requester_id
	LEFT JOIN users asg ON asg.id = t.assigned_to `

type CreateTicketParams struct {
	ClientID    *int64
	RequesterID int64
	Subject     string
	Category    string
	Body        string
	Priority    string
}

func (r *Repository) CreateTicket(ctx context.Context, tenantID int64, p CreateTicketParams) (*HelpTicket, error) {
	res, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO help_tickets (public_id, tenant_id, client_id, requester_id, subject, category, body, status, priority)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		platform.NewULID(), tenantID, p.ClientID, p.RequesterID, p.Subject, p.Category, p.Body,
		StatusOpen, p.Priority)
	if err != nil {
		return nil, fmt.Errorf("creating help ticket: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading help ticket id: %w", err)
	}
	return r.ticketByID(ctx, tenantID, id)
}

func (r *Repository) ticketByID(ctx context.Context, tenantID, id int64) (*HelpTicket, error) {
	var t HelpTicket
	err := r.db.Primary.GetContext(ctx, &t,
		`SELECT `+helpTicketColumns+helpTicketFrom+` WHERE t.tenant_id = ? AND t.id = ? AND t.deleted_at IS NULL`,
		tenantID, id)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading help ticket: %w", err)
	}
	return &t, nil
}

func (r *Repository) TicketByPublicID(ctx context.Context, tenantID int64, publicID string) (*HelpTicket, error) {
	var t HelpTicket
	err := r.db.Primary.GetContext(ctx, &t,
		`SELECT `+helpTicketColumns+helpTicketFrom+` WHERE t.tenant_id = ? AND t.public_id = ? AND t.deleted_at IS NULL`,
		tenantID, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading help ticket: %w", err)
	}
	return &t, nil
}

// ListTickets returns the tenant's help tickets. staffOnly true narrows nothing;
// when false the list is restricted to the requester's own tickets.
// HelpFilter narrows the help-request list.
type HelpFilter struct {
	Query      string
	Statuses   []string
	Categories []string
	Priorities []string
}

func (r *Repository) ListTickets(ctx context.Context, tenantID int64, requesterID *int64, filter HelpFilter) ([]HelpTicket, error) {
	where := []string{"t.tenant_id = ?", "t.deleted_at IS NULL"}
	args := []any{tenantID}
	if requesterID != nil {
		where = append(where, "t.requester_id = ?")
		args = append(args, *requesterID)
	}

	if q := strings.TrimSpace(filter.Query); q != "" {
		where = append(where, "(t.subject LIKE ? OR t.description LIKE ?)")
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	addIn := func(col string, values []string) {
		if len(values) == 0 {
			return
		}
		where = append(where, col+" IN ("+platform.Placeholders(len(values))+")")
		for _, v := range values {
			args = append(args, v)
		}
	}
	addIn("t.status", filter.Statuses)
	addIn("t.category", filter.Categories)
	addIn("t.priority", filter.Priorities)
	rows := []HelpTicket{}
	q := `SELECT ` + helpTicketColumns + helpTicketFrom + ` WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY t.created_at DESC`
	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("listing help tickets: %w", err)
	}
	return rows, nil
}

// TicketUpdate is a staff edit of a help ticket.
type TicketUpdate struct {
	Status     *string
	Priority   *string
	AssignedTo *int64
	// ClearAssignee drops an existing assignee.
	ClearAssignee bool
	ResolvedBy    *int64
	// ClearResolved resets the resolution, e.g. when reopening a ticket.
	ClearResolved bool
}

func (r *Repository) UpdateTicket(ctx context.Context, tenantID, id int64, u TicketUpdate) error {
	set, args := []string{}, []any{}
	if u.Status != nil {
		set, args = append(set, "status = ?"), append(args, *u.Status)
	}
	if u.Priority != nil {
		set, args = append(set, "priority = ?"), append(args, *u.Priority)
	}
	if u.AssignedTo != nil {
		set, args = append(set, "assigned_to = ?"), append(args, *u.AssignedTo)
	}
	if u.ClearAssignee {
		set = append(set, "assigned_to = NULL")
	}
	if u.ResolvedBy != nil {
		set, args = append(set, "resolved_by = ?"), append(args, *u.ResolvedBy)
	}
	if u.ClearResolved {
		set = append(set, "resolved_by = NULL, resolved_at = NULL")
	}
	if len(set) == 0 {
		return nil
	}
	args = append(args, tenantID, id)
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE help_tickets SET `+strings.Join(set, ", ")+` WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
		args...)
	if err != nil {
		return fmt.Errorf("updating help ticket: %w", err)
	}
	return affected(res)
}

// --- replies ----------------------------------------------------------------

func (r *Repository) AddReply(ctx context.Context, ticketID, authorID int64, authorRole, body string) (*HelpTicketReply, error) {
	res, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO help_ticket_replies (help_ticket_id, author_id, author_role, body)
		VALUES (?,?,?,?)`, ticketID, authorID, authorRole, body)
	if err != nil {
		return nil, fmt.Errorf("adding help reply: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading help reply id: %w", err)
	}
	return r.ReplyByID(ctx, id)
}

func (r *Repository) ReplyByID(ctx context.Context, id int64) (*HelpTicketReply, error) {
	var row HelpTicketReply
	err := r.db.Primary.GetContext(ctx, &row, `
		SELECT p.id, p.help_ticket_id, p.author_id, p.author_role, p.body, p.created_at,
		       CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS author_name
		FROM help_ticket_replies p JOIN users u ON u.id = p.author_id
		WHERE p.id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("loading help reply: %w", err)
	}
	return &row, nil
}

func (r *Repository) Replies(ctx context.Context, ticketID int64) ([]HelpTicketReply, error) {
	rows := []HelpTicketReply{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT p.id, p.help_ticket_id, p.author_id, p.author_role, p.body, p.created_at,
		       CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS author_name
		FROM help_ticket_replies p JOIN users u ON u.id = p.author_id
		WHERE p.help_ticket_id = ? ORDER BY p.created_at ASC`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("listing help replies: %w", err)
	}
	return rows, nil
}

func affected(res interface{ RowsAffected() (int64, error) }) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading affected rows: %w", err)
	}
	if n == 0 {
		return platform.ErrSentinelNotFound
	}
	return nil
}

// SoftDeleteTicket withdraws a help request. Soft, like everything else here:
// the conversation it carries is a record of what was asked and answered.
func (r *Repository) SoftDeleteTicket(ctx context.Context, tenantID int64, publicID string) error {
	res, err := r.db.Primary.ExecContext(ctx, `
		UPDATE help_tickets SET deleted_at = UTC_TIMESTAMP(3)
		WHERE tenant_id = ? AND public_id = ? AND deleted_at IS NULL`, tenantID, publicID)
	if err != nil {
		return fmt.Errorf("removing help ticket: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("removing help ticket: %w", err)
	}
	if n == 0 {
		return platform.ErrSentinelNotFound
	}
	return nil
}
