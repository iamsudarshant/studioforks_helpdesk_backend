// Package help is the product's self-service surface: a searchable FAQ plus a
// "Request Help" ticket thread. Unlike compliance tickets, help tickets are
// about the product itself — raised by any user, answered and resolved only by
// ComplyDesk staff (admins and agents).
package help

import (
	"database/sql"
	"time"
)

// FAQArticle is one knowledge-base entry shown in the Help section.
type FAQArticle struct {
	ID        int64         `db:"id"`
	PublicID  string        `db:"public_id"`
	TenantID  int64         `db:"tenant_id"`
	Section   string        `db:"section"`
	Question  string        `db:"question"`
	Answer    string        `db:"answer"`
	SortOrder int           `db:"sort_order"`
	IsActive  bool          `db:"is_active"`
	CreatedBy sql.NullInt64 `db:"created_by"`
	CreatedAt time.Time     `db:"created_at"`
	UpdatedAt time.Time     `db:"updated_at"`

	// Joined for display.
	AuthorName sql.NullString `db:"author_name"`
}

// HelpTicket is a request for assistance with the product.
//
// Status lifecycle: OPEN -> (staff reply) -> PENDING/IN_PROGRESS -> RESOLVED.
// A resolved ticket can be reopened by its requester.
type HelpTicket struct {
	ID          int64         `db:"id"`
	PublicID    string        `db:"public_id"`
	TenantID    int64         `db:"tenant_id"`
	ClientID    sql.NullInt64 `db:"client_id"`
	RequesterID int64         `db:"requester_id"`
	Subject     string        `db:"subject"`
	Category    string        `db:"category"`
	Body        string        `db:"body"`
	Status      string        `db:"status"`
	Priority    string        `db:"priority"`
	AssignedTo  sql.NullInt64 `db:"assigned_to"`
	ResolvedBy  sql.NullInt64 `db:"resolved_by"`
	ResolvedAt  sql.NullTime  `db:"resolved_at"`
	CreatedAt   time.Time     `db:"created_at"`
	UpdatedAt   time.Time     `db:"updated_at"`

	// Joined for display.
	RequesterPublicID sql.NullString `db:"requester_public_id"`
	RequesterName     sql.NullString `db:"requester_name"`
	RequesterEmail    sql.NullString `db:"requester_email"`
	AssigneePublicID  sql.NullString `db:"assignee_public_id"`
	AssigneeName      sql.NullString `db:"assignee_name"`
	ReplyCount        int            `db:"reply_count"`
}

// HelpTicketReply is one message in a help ticket thread.
type HelpTicketReply struct {
	ID           int64     `db:"id"`
	HelpTicketID int64     `db:"help_ticket_id"`
	AuthorID     int64     `db:"author_id"`
	AuthorRole   string    `db:"author_role"`
	Body         string    `db:"body"`
	CreatedAt    time.Time `db:"created_at"`

	AuthorName sql.NullString `db:"author_name"`
}

// Help categories and lifecycle tokens, shared by the API and seeders.
const (
	CategoryBug      = "BUG"
	CategoryQuestion = "QUESTION"
	CategoryRequest  = "REQUEST"
	CategoryAccess   = "ACCESS"

	StatusOpen       = "OPEN"
	StatusInProgress = "IN_PROGRESS"
	StatusResolved   = "RESOLVED"

	PriorityLow    = "LOW"
	PriorityNormal = "NORMAL"
	PriorityHigh   = "HIGH"
)
