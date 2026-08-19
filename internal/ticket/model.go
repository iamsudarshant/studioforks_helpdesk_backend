// Package ticket implements the Help Desk — the platform's core module.
//
// The status machine is table-driven from category_workflows, so a client can
// change its own lifecycle without a code change. Every state change writes an
// immutable timeline entry alongside the row it mutates.
package ticket

import (
	"database/sql"
	"time"
)

// The ticket lifecycle:
//
//	NEW -> OPEN -> IN_PROGRESS <-> PENDING_EMPLOYEE / PENDING_HELPDESK
//	                            -> RESOLVED -> CLOSED
//
// with ESCALATED, REOPENED and CANCELLED available as branches off it.
//
// NEW and OPEN are deliberately distinct: NEW means nobody has looked at it
// yet, OPEN means the helpdesk has reviewed and accepted it. The gap between
// those two timestamps is first-response time, which is the number the SLA is
// actually judged on.
//
// The two PENDING states name who is holding the ticket up, because that is the
// question every dashboard asks. PENDING_EMPLOYEE stops the SLA clock — the
// helpdesk cannot be held to a target while waiting on the requester —
// PENDING_HELPDESK does not.
const (
	// The five states the desk works in. See MIGRATION 000024: OPEN and
	// IN_PROGRESS both meant "the department has it" and are now one state;
	// RESOLVED and CLOSED both meant "the work is done" and are now one.
	StatusNew             = "NEW"
	StatusPendingHelpdesk = "PENDING_HELPDESK"
	StatusPendingEmployee = "PENDING_EMPLOYEE"
	StatusClosed          = "CLOSED"
	StatusReopened        = "REOPENED"

	// Not one of the five. A withdrawn ticket is the absence of the work rather
	// than a stage of it, and it is reached by an explicit cancel.
	StatusCancelled = "CANCELLED"

	// Retained so a saved filter, an integration or a stored transition naming
	// an old state still resolves to something rather than silently matching
	// nothing. Nothing sets them any more.
	StatusOpen       = StatusPendingHelpdesk
	StatusInProgress = StatusPendingHelpdesk
	StatusResolved   = StatusClosed
	StatusEscalated  = StatusPendingHelpdesk
)

// Statuses is every current status, in lifecycle order. Exported so a filter
// bar can offer them without restating the vocabulary and drifting from it.
var Statuses = []string{
	StatusNew, StatusOpen, StatusInProgress, StatusPendingEmployee,
	StatusPendingHelpdesk, StatusEscalated, StatusResolved, StatusClosed,
	StatusReopened, StatusCancelled,
}

// Priorities is the priority vocabulary, most urgent last so a picker reads in
// the order people think about it.
var Priorities = []string{"LOW", "MEDIUM", "HIGH", "CRITICAL"}

// Superseded names, kept so a bookmarked filter, saved view or integration
// written against the previous vocabulary still resolves. Read through
// CanonicalStatus rather than indexing this directly.
var statusAliases = map[string]string{
	"ASSIGNED":            StatusOpen,
	"PENDING_INFORMATION": StatusPendingEmployee,
	"PENDING_USER":        StatusPendingEmployee,
	"PENDING_DEPT":        StatusPendingHelpdesk,
}

// CanonicalStatus resolves any status name — current or superseded — to its
// current form, returning the input unchanged when it is neither.
func CanonicalStatus(status string) string {
	if current, ok := statusAliases[status]; ok {
		return current
	}
	return status
}

// OpenStatuses are the states a ticket is considered live in. Used by counts,
// dashboards and the "still outstanding" filters.
var OpenStatuses = []string{
	StatusNew, StatusOpen, StatusInProgress,
	StatusPendingEmployee, StatusPendingHelpdesk, StatusEscalated, StatusReopened,
}

// ClosedStatuses are terminal.
var ClosedStatuses = []string{StatusClosed, StatusCancelled}

// ReopenableStatuses are the states an employee may reopen from.
var ReopenableStatuses = []string{StatusResolved, StatusClosed}

// SLAPausedStatuses stop the resolution clock. Only waiting on the requester
// counts: PENDING_HELPDESK is the helpdesk waiting on itself, which is exactly
// the delay an SLA exists to measure.
var SLAPausedStatuses = []string{StatusPendingEmployee}

const (
	PriorityLow      = "LOW"
	PriorityMedium   = "MEDIUM"
	PriorityHigh     = "HIGH"
	PriorityCritical = "CRITICAL"
)

const (
	SourcePortal = "PORTAL"
	SourceEmail  = "EMAIL"
	SourceAPI    = "API"
	SourcePhone  = "PHONE"
	SourceImport = "IMPORT"
)

// Visibility of a conversation entry. INTERNAL notes are filtered out in the
// query, never in the handler, so a serialisation slip cannot leak them.
const (
	VisibilityPublic   = "PUBLIC"
	VisibilityInternal = "INTERNAL"
)

// Timeline event types.
const (
	EventCreated         = "CREATED"
	EventStatusChanged   = "STATUS_CHANGED"
	EventAssigned        = "ASSIGNED"
	EventTransferred     = "TRANSFERRED"
	EventEscalated       = "ESCALATED"
	EventReplied         = "REPLIED"
	EventInternalNote    = "INTERNAL_NOTE"
	EventAttachmentAdded = "ATTACHMENT_ADDED"
	EventInfoRequested   = "INFO_REQUESTED"
	EventResolved        = "RESOLVED"
	EventClosed          = "CLOSED"
	EventReopened        = "REOPENED"
	EventFieldUpdated    = "FIELD_UPDATED"
	EventFeedbackGiven   = "FEEDBACK_GIVEN"
	EventWatcherAdded    = "WATCHER_ADDED"
)

// Ticket is the database row.
type Ticket struct {
	ID                    int64          `db:"id"`
	PublicID              string         `db:"public_id"`
	TenantID              int64          `db:"tenant_id"`
	TicketNumber          string         `db:"ticket_number"`
	CategoryID            int64          `db:"category_id"`
	SubcategoryID         sql.NullInt64  `db:"subcategory_id"`
	Subject               string         `db:"subject"`
	Description           sql.NullString `db:"description"`
	Status                string         `db:"status"`
	Priority              string         `db:"priority"`
	Source                string         `db:"source"`
	RequesterID           int64          `db:"requester_id"`
	RequesterSnapshotJSON sql.NullString `db:"requester_snapshot_json"`
	EntityID              sql.NullInt64  `db:"entity_id"`
	SiteID                sql.NullInt64  `db:"site_id"`
	DepartmentID          sql.NullInt64  `db:"department_id"`
	AssigneeID            sql.NullInt64  `db:"assignee_id"`
	CustomFieldsJSON      sql.NullString `db:"custom_fields_json"`
	SLAPolicyID           sql.NullInt64  `db:"sla_policy_id"`
	FirstResponseDueAt    sql.NullTime   `db:"first_response_due_at"`
	ResolutionDueAt       sql.NullTime   `db:"resolution_due_at"`
	FirstRespondedAt      sql.NullTime   `db:"first_responded_at"`
	ResolvedAt            sql.NullTime   `db:"resolved_at"`
	ClosedAt              sql.NullTime   `db:"closed_at"`
	ReopenedCount         int            `db:"reopened_count"`
	LastReopenedAt        sql.NullTime   `db:"last_reopened_at"`
	EscalationLevel       int            `db:"escalation_level"`
	IsSLABreached         bool           `db:"is_sla_breached"`
	SLAPausedAt           sql.NullTime   `db:"sla_paused_at"`
	SLAPausedTotalMins    int            `db:"sla_paused_total_mins"`
	CSATScore             sql.NullInt64  `db:"csat_score"`
	CSATComment           sql.NullString `db:"csat_comment"`
	ParentTicketID        sql.NullInt64  `db:"parent_ticket_id"`
	LastActivityAt        time.Time      `db:"last_activity_at"`
	CreatedBy             sql.NullInt64  `db:"created_by"`
	UpdatedBy             sql.NullInt64  `db:"updated_by"`
	CreatedAt             time.Time      `db:"created_at"`
	UpdatedAt             time.Time      `db:"updated_at"`

	// Joined for list and detail rendering.
	CategoryName    string         `db:"category_name"`
	CategoryKey     string         `db:"category_key"`
	SubcategoryName sql.NullString `db:"subcategory_name"`
	RequesterName   string         `db:"requester_name"`
	RequesterCode   sql.NullString `db:"requester_code"`
	// The requester's statutory identity, joined for the detail rail. An agent
	// or partner resolves a PF or ESIC query against exactly these numbers, so
	// the ticket is not actionable without them — chasing the requester's
	// profile in another screen is how a query gets answered against the wrong
	// member. Loaded only for the detail read; the list never carries them.
	RequesterPublicID    sql.NullString `db:"requester_public_id"`
	RequesterEmail       sql.NullString `db:"requester_email"`
	RequesterMobile      sql.NullString `db:"requester_mobile"`
	RequesterUAN         sql.NullString `db:"requester_uan"`
	RequesterPF          sql.NullString `db:"requester_pf"`
	RequesterESIC        sql.NullString `db:"requester_esic"`
	RequesterPAN         sql.NullString `db:"requester_pan"`
	RequesterDesignation sql.NullString `db:"requester_designation"`
	RequesterDOJ         sql.NullTime   `db:"requester_doj"`
	RequesterDOB         sql.NullTime   `db:"requester_dob"`
	RequesterExitDate    sql.NullTime   `db:"requester_exit_date"`
	RequesterStatus      sql.NullString `db:"requester_status"`
	RequesterEntityName  sql.NullString `db:"requester_entity_name"`
	RequesterDeptName    sql.NullString `db:"requester_dept_name"`
	AssigneeName         sql.NullString `db:"assignee_name"`
	// Who actually filed it, which is not always who it is for: a partner or an
	// agent raising on someone's behalf is the creator, the employee is the
	// requester. The two being different is the whole of "raised on behalf".
	CreatorName    sql.NullString `db:"creator_name"`
	EntityName     sql.NullString `db:"entity_name"`
	SiteName       sql.NullString `db:"site_name"`
	DepartmentName sql.NullString `db:"department_name"`
	// The client this ticket belongs to. Only joined in so a cross-client list
	// can render a client column without a per-row request.
	TenantPublicID   string         `db:"tenant_public_id"`
	TenantName       string         `db:"tenant_name"`
	TenantSlug       string         `db:"tenant_slug"`
	TenantClientCode sql.NullString `db:"tenant_client_code"`
}

// IsOpen reports whether the ticket is still live.
func (t *Ticket) IsOpen() bool {
	for _, s := range OpenStatuses {
		if t.Status == s {
			return true
		}
	}
	return false
}

// Conversation is one reply or internal note on a ticket.
type Conversation struct {
	ID           int64          `db:"id"`
	PublicID     string         `db:"public_id"`
	TenantID     int64          `db:"tenant_id"`
	TicketID     int64          `db:"ticket_id"`
	AuthorID     sql.NullInt64  `db:"author_id"`
	AuthorRole   sql.NullString `db:"author_role"`
	Visibility   string         `db:"visibility"`
	BodyHTML     sql.NullString `db:"body_html"`
	BodyText     sql.NullString `db:"body_text"`
	IsSystem     bool           `db:"is_system"`
	InReplyToID  sql.NullInt64  `db:"in_reply_to_id"`
	MentionsJSON sql.NullString `db:"mentions_json"`
	EditedAt     sql.NullTime   `db:"edited_at"`
	CreatedAt    time.Time      `db:"created_at"`

	AuthorName sql.NullString `db:"author_name"`
}

// TimelineEntry is an append-only record of something that happened. Nothing in
// the API updates or deletes these.
type TimelineEntry struct {
	ID         int64          `db:"id"`
	PublicID   string         `db:"public_id"`
	TicketID   int64          `db:"ticket_id"`
	EventType  string         `db:"event_type"`
	ActorID    sql.NullInt64  `db:"actor_id"`
	ActorName  sql.NullString `db:"actor_name_snapshot"`
	ActorRole  sql.NullString `db:"actor_role"`
	Visibility string         `db:"visibility"`
	Summary    string         `db:"summary"`
	DetailJSON sql.NullString `db:"detail_json"`
	CreatedAt  time.Time      `db:"created_at"`
}

// Transition is one allowed status move, read from category_workflows.
type Transition struct {
	ToStatus         string         `db:"to_status"`
	Label            sql.NullString `db:"label"`
	RequiresComment  bool           `db:"requires_comment"`
	RequiresReason   bool           `db:"requires_reason_code"`
	ReasonCodesJSON  sql.NullString `db:"reason_codes_json"`
	AllowedRolesJSON sql.NullString `db:"allowed_roles_json"`

	// OffWorkflow marks a move the configured graph does not describe, offered
	// to the desk as an override. Never set by a database read.
	OffWorkflow bool `db:"-"`
}

// Attachment links a stored document to a ticket.
type Attachment struct {
	ID             int64          `db:"id"`
	DocumentID     int64          `db:"document_id"`
	DocumentPubID  string         `db:"document_public_id"`
	ConversationID sql.NullInt64  `db:"conversation_id"`
	Context        string         `db:"context"`
	OriginalName   string         `db:"original_name"`
	MimeType       string         `db:"mime_type"`
	SizeBytes      int64          `db:"size_bytes"`
	UploadedBy     sql.NullInt64  `db:"uploaded_by"`
	UploaderName   sql.NullString `db:"uploader_name"`
	CreatedAt      time.Time      `db:"created_at"`
}
