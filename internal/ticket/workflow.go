package ticket

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

// AllowedTransitions returns the status moves permitted from a ticket's current
// state, for the roles the caller holds.
//
// The machine is data, not code: a client edits category_workflows and its
// lifecycle changes. A transition with no rows configured simply is not offered,
// which is why the API returns this list rather than the client hardcoding one.
func (r *Repository) AllowedTransitions(ctx context.Context, tenantID, categoryID int64, from string, roles []string) ([]Transition, error) {
	rows := []Transition{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT to_status, label, requires_comment, requires_reason_code,
		       reason_codes_json, allowed_roles_json
		FROM category_workflows
		WHERE tenant_id = ? AND category_id = ? AND from_status = ? AND is_active = 1
		ORDER BY to_status`, tenantID, categoryID, from)
	if err != nil {
		return nil, fmt.Errorf("loading allowed transitions: %w", err)
	}

	// A transition may name the roles allowed to perform it; an empty list means
	// anyone who can change status at all.
	out := make([]Transition, 0, len(rows))
	for _, t := range rows {
		if !roleAllowed(t.AllowedRolesJSON.String, roles) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// WorkableStatuses is every state a ticket can be moved to, in the order the
// lifecycle runs. The configured workflow is the normal path through it; this is
// the whole board.
//
// Used for the desk's override (see TransitionsFor): a helpdesk agent looking at
// a ticket that has gone somewhere the graph does not describe — imported, or
// left behind by a category whose workflow was later edited — has to be able to
// put it back on the rails.
func WorkableStatuses() []string {
	return []string{
		StatusNew,
		StatusPendingHelpdesk,
		StatusPendingEmployee,
		StatusClosed,
		StatusReopened,
		StatusCancelled,
	}
}

// TransitionsFor is what the action bar renders.
//
// `override` widens the configured graph to every status, which is what admin
// and agent portals get: the desk is trusted to decide where a ticket belongs,
// and a workflow that hides the move they need leaves them editing the database.
// A client-side user still gets exactly the configured path — the graph is the
// product's rules for them, not a suggestion.
//
// The configured transitions keep their labels, their required comment and their
// reason codes; the extra ones are marked so the caller can tell the two apart,
// and always require a comment, because a move outside the agreed lifecycle
// should say why.
func (r *Repository) TransitionsFor(ctx context.Context, tenantID, categoryID int64,
	from string, roles []string, override bool) ([]Transition, error) {

	configured, err := r.AllowedTransitions(ctx, tenantID, categoryID, from, roles)
	if err != nil || !override {
		return configured, err
	}

	seen := make(map[string]bool, len(configured)+len(WorkableStatuses()))
	for _, t := range configured {
		seen[t.ToStatus] = true
	}

	out := configured
	for _, status := range WorkableStatuses() {
		if status == from || seen[status] {
			continue
		}
		out = append(out, Transition{
			ToStatus:        status,
			Label:           sql.NullString{String: "Move to " + label(status), Valid: true},
			RequiresComment: true,
			OffWorkflow:     true,
		})
	}
	return out, nil
}

func roleAllowed(rawJSON string, roles []string) bool {
	if strings.TrimSpace(rawJSON) == "" {
		return true
	}
	var allowed []string
	if err := json.Unmarshal([]byte(rawJSON), &allowed); err != nil || len(allowed) == 0 {
		return true
	}
	for _, want := range allowed {
		for _, have := range roles {
			if want == have {
				return true
			}
		}
	}
	return false
}

// FindTransition returns one specific move, or not-found when it is not allowed.
func (r *Repository) FindTransition(ctx context.Context, tenantID, categoryID int64, from, to string, roles []string) (*Transition, error) {
	transitions, err := r.AllowedTransitions(ctx, tenantID, categoryID, from, roles)
	if err != nil {
		return nil, err
	}
	for i := range transitions {
		if transitions[i].ToStatus == to {
			return &transitions[i], nil
		}
	}
	return nil, platform.ErrSentinelNotFound
}

// ChangeStatusParams describes one status move.
type ChangeStatusParams struct {
	ToStatus   string
	ReasonCode string
	Comment    string
	ActorID    *int64
	ActorName  string
	ActorRole  string
}

// ChangeStatus moves a ticket and records the move.
//
// The timestamps that reporting depends on — first response, resolved, closed,
// reopen count — are maintained here rather than by the caller, so every path
// that changes status keeps them consistent.
func (r *Repository) ChangeStatus(ctx context.Context, tenantID, ticketID int64, p ChangeStatusParams) (*Ticket, error) {
	var updated *Ticket

	err := r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		var current struct {
			Status     string    `db:"status"`
			CategoryID int64     `db:"category_id"`
			UpdatedAt  time.Time `db:"updated_at"`
		}
		if err := tx.GetContext(ctx, &current, `
			SELECT status, category_id, updated_at FROM tickets
			WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL FOR UPDATE`,
			tenantID, ticketID); err != nil {
			if platform.IsNotFound(err) {
				return platform.ErrSentinelNotFound
			}
			return fmt.Errorf("loading ticket for status change: %w", err)
		}

		if current.Status == p.ToStatus {
			// Not an error worth failing on, but nothing to record either.
			return nil
		}

		// How long the ticket sat in the previous state, for cycle-time reports.
		var previousSince time.Time
		if err := tx.GetContext(ctx, &previousSince, `
			SELECT COALESCE(MAX(created_at), (SELECT created_at FROM tickets WHERE id = ?))
			FROM ticket_status_history WHERE ticket_id = ?`, ticketID, ticketID); err != nil {
			previousSince = current.UpdatedAt
		}
		duration := int64(time.Since(previousSince).Seconds())

		set := []string{"status = ?", "last_activity_at = UTC_TIMESTAMP(3)", "updated_by = ?"}
		args := []any{p.ToStatus, p.ActorID}

		switch p.ToStatus {
		case StatusClosed:
			set = append(set, "closed_at = UTC_TIMESTAMP(3)")
			// `resolved_at` is still stamped, because it is what the reopen
			// window counts from and what the satisfaction survey fires on —
			// closing *is* resolving now that the two states are one.
			set = append(set, "resolved_at = COALESCE(resolved_at, UTC_TIMESTAMP(3))")
		case StatusReopened:
			set = append(set,
				"reopened_count = reopened_count + 1",
				"last_reopened_at = UTC_TIMESTAMP(3)",
				"resolved_at = NULL", "closed_at = NULL")
		}

		args = append(args, tenantID, ticketID)
		if _, err := tx.ExecContext(ctx,
			`UPDATE tickets SET `+strings.Join(set, ", ")+` WHERE tenant_id = ? AND id = ?`,
			args...); err != nil {
			return fmt.Errorf("updating ticket status: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ticket_status_history
				(tenant_id, ticket_id, from_status, to_status, reason_code, comment,
				 actor_id, duration_in_previous_secs)
			VALUES (?,?,?,?,?,?,?,?)`,
			tenantID, ticketID, current.Status, p.ToStatus,
			nullStr(p.ReasonCode), nullStr(p.Comment), p.ActorID, duration); err != nil {
			return fmt.Errorf("recording status history: %w", err)
		}

		event := EventStatusChanged
		summary := fmt.Sprintf("Status changed from %s to %s", label(current.Status), label(p.ToStatus))
		switch p.ToStatus {
		case StatusClosed:
			event, summary = EventClosed, "Ticket closed"
		case StatusReopened:
			event, summary = EventReopened, "Ticket reopened"
		}

		detail := map[string]any{"from": current.Status, "to": p.ToStatus}
		if p.ReasonCode != "" {
			detail["reason_code"] = p.ReasonCode
		}
		if p.Comment != "" {
			detail["comment"] = p.Comment
		}

		if err := writeTimeline(ctx, tx, tenantID, ticketID, timelineParams{
			EventType: event, ActorID: p.ActorID, ActorName: p.ActorName,
			ActorRole: p.ActorRole, Summary: summary, Detail: detail,
		}); err != nil {
			return err
		}

		var t Ticket
		q := `SELECT ` + ticketColumns + ticketDisplay + requesterDetail + ticketJoins + requesterJoins + ` WHERE t.id = ?`
		if err := tx.GetContext(ctx, &t, q, ticketID); err != nil {
			return fmt.Errorf("reloading ticket: %w", err)
		}
		updated = &t
		return nil
	})

	return updated, err
}

// label renders a status for a human-readable timeline summary.
func label(status string) string {
	return strings.ReplaceAll(strings.ToLower(status), "_", " ")
}

// userName resolves a person's display name for a timeline snapshot. A missing
// id or a lookup failure yields "", so a name the record could not resolve is
// simply absent rather than an error that loses the whole entry.
func userName(ctx context.Context, tx *sqlx.Tx, id *int64) string {
	if id == nil || *id == 0 {
		return ""
	}
	var name string
	if err := tx.GetContext(ctx, &name,
		`SELECT TRIM(CONCAT(first_name, ' ', COALESCE(last_name, ''))) FROM users WHERE id = ?`,
		*id); err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// departmentName is userName's counterpart for a statutory line.
func departmentName(ctx context.Context, tx *sqlx.Tx, tenantID int64, id *int64) string {
	if id == nil || *id == 0 {
		return ""
	}
	var name string
	if err := tx.GetContext(ctx, &name,
		`SELECT name FROM departments WHERE tenant_id = ? AND id = ?`, tenantID, *id); err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// AssignParams describes an assignment or transfer.
type AssignParams struct {
	AssigneeID   *int64
	DepartmentID *int64
	Type         string // ASSIGN | TRANSFER | ESCALATE
	Reason       string
	ActorID      *int64
	ActorName    string
}

// Assign sets the ticket's assignee or department and records the handover.
//
// Assigning an untouched ticket also advances it out of OPEN, so the lifecycle
// reflects reality without the agent having to make two calls.
func (r *Repository) Assign(ctx context.Context, tenantID, ticketID int64, p AssignParams) (*Ticket, error) {
	var updated *Ticket

	err := r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		var before struct {
			AssigneeID   *int64 `db:"assignee_id"`
			DepartmentID *int64 `db:"department_id"`
			Status       string `db:"status"`
		}
		if err := tx.GetContext(ctx, &before, `
			SELECT assignee_id, department_id, status FROM tickets
			WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL FOR UPDATE`,
			tenantID, ticketID); err != nil {
			if platform.IsNotFound(err) {
				return platform.ErrSentinelNotFound
			}
			return fmt.Errorf("loading ticket for assignment: %w", err)
		}

		set := []string{"last_activity_at = UTC_TIMESTAMP(3)", "updated_by = ?"}
		args := []any{p.ActorID}

		if p.AssigneeID != nil {
			set = append(set, "assignee_id = ?")
			args = append(args, *p.AssigneeID)
		}
		if p.DepartmentID != nil {
			set = append(set, "department_id = ?")
			args = append(args, *p.DepartmentID)
		}
		if p.Type == "ESCALATE" {
			set = append(set, "escalation_level = escalation_level + 1")
		}

		// A NEW ticket that gains an owner becomes OPEN: someone at the helpdesk
		// has now looked at it, which is exactly what OPEN means. This is also
		// what stops the first-response clock, so it must happen on assignment
		// rather than waiting for the first reply.
		if before.Status == StatusNew && p.AssigneeID != nil {
			set = append(set, "status = ?")
			args = append(args, StatusOpen)
			set = append(set, "first_responded_at = COALESCE(first_responded_at, UTC_TIMESTAMP(3))")
		}

		// Escalation is a flag *alongside* the status, not a replacement for it.
		//
		// It used to overwrite the status with ESCALATED, which destroyed the
		// information the board is actually worked from: a ticket waiting on the
		// employee and a ticket waiting on the desk both became "Escalated", and
		// nobody could tell what the ticket was actually waiting for. Whether a
		// ticket is urgent and what stage it is at are two different questions,
		// and the answer to one must not erase the other.
		//
		// `escalation_level` carries the urgency; the list, the detail and the
		// filters all read it. See `is_escalated` on the response.

		args = append(args, tenantID, ticketID)
		if _, err := tx.ExecContext(ctx,
			`UPDATE tickets SET `+strings.Join(set, ", ")+` WHERE tenant_id = ? AND id = ?`,
			args...); err != nil {
			return fmt.Errorf("updating assignment: %w", err)
		}

		assignmentType := p.Type
		if assignmentType == "" {
			assignmentType = "ASSIGN"
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ticket_assignments
				(tenant_id, ticket_id, from_user_id, to_user_id, from_department_id,
				 to_department_id, assignment_type, reason, actor_id)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			tenantID, ticketID, before.AssigneeID, p.AssigneeID,
			before.DepartmentID, p.DepartmentID, assignmentType,
			nullStr(p.Reason), p.ActorID); err != nil {
			return fmt.Errorf("recording assignment: %w", err)
		}

		event, summary := EventAssigned, "Ticket assigned"
		switch assignmentType {
		case "TRANSFER":
			event, summary = EventTransferred, "Ticket transferred"
		case "ESCALATE":
			event, summary = EventEscalated, "Ticket escalated"
		}

		// Names, not row ids, and only for what actually moved.
		//
		// The timeline is read by the next person to pick the ticket up, and
		// "assigned" on its own tells them nothing — they need to know to whom,
		// and from whom. Resolved here, inside the transaction that made the
		// change, so the record still reads correctly after somebody is renamed
		// or leaves: this is a snapshot of what happened, not a live join.
		//
		// A field the action did not touch is left out entirely. Recording the
		// department a plain assign started in reads as a transfer that never
		// happened, and an escalation — which changes neither the owner nor the
		// line — would otherwise claim to have moved both.
		detail := map[string]any{"type": assignmentType}
		if p.Reason != "" {
			detail["reason"] = p.Reason
		}
		if p.AssigneeID != nil {
			if name := userName(ctx, tx, p.AssigneeID); name != "" {
				detail["to_assignee"] = name
			}
			// The previous owner only when there was one and it changed.
			if before.AssigneeID != nil && *before.AssigneeID != *p.AssigneeID {
				if name := userName(ctx, tx, before.AssigneeID); name != "" {
					detail["from_assignee"] = name
				}
			}
		}
		if p.DepartmentID != nil {
			if name := departmentName(ctx, tx, tenantID, p.DepartmentID); name != "" {
				detail["to_department"] = name
			}
			if before.DepartmentID != nil && *before.DepartmentID != *p.DepartmentID {
				if name := departmentName(ctx, tx, tenantID, before.DepartmentID); name != "" {
					detail["from_department"] = name
				}
			}
		}

		if err := writeTimeline(ctx, tx, tenantID, ticketID, timelineParams{
			EventType: event, ActorID: p.ActorID, ActorName: p.ActorName,
			Summary: summary, Detail: detail,
		}); err != nil {
			return err
		}

		var t Ticket
		q := `SELECT ` + ticketColumns + ticketDisplay + requesterDetail + ticketJoins + requesterJoins + ` WHERE t.id = ?`
		if err := tx.GetContext(ctx, &t, q, ticketID); err != nil {
			return fmt.Errorf("reloading ticket: %w", err)
		}
		updated = &t
		return nil
	})

	return updated, err
}

// UpdateParams covers the editable fields on a ticket.
type UpdateParams struct {
	Subject       *string
	Priority      *string
	CategoryID    *int64
	SubcategoryID *int64
	EntityID      *int64
	SiteID        *int64
	CustomFields  *string
	ActorID       *int64
	ActorName     string
}

func (r *Repository) Update(ctx context.Context, tenantID, ticketID int64, p UpdateParams) (*Ticket, error) {
	var updated *Ticket

	err := r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		set := []string{"last_activity_at = UTC_TIMESTAMP(3)", "updated_by = ?"}
		args := []any{p.ActorID}
		changed := map[string]any{}

		if p.Subject != nil {
			set, args = append(set, "subject = ?"), append(args, *p.Subject)
			changed["subject"] = *p.Subject
		}
		if p.Priority != nil {
			set, args = append(set, "priority = ?"), append(args, *p.Priority)
			changed["priority"] = *p.Priority
		}
		if p.CategoryID != nil {
			set, args = append(set, "category_id = ?"), append(args, *p.CategoryID)
			changed["category_id"] = *p.CategoryID
		}
		if p.SubcategoryID != nil {
			set, args = append(set, "subcategory_id = ?"), append(args, *p.SubcategoryID)
			changed["subcategory_id"] = *p.SubcategoryID
		}
		if p.EntityID != nil {
			set, args = append(set, "entity_id = ?"), append(args, *p.EntityID)
			changed["entity_id"] = *p.EntityID
		}
		if p.SiteID != nil {
			set, args = append(set, "site_id = ?"), append(args, *p.SiteID)
			changed["site_id"] = *p.SiteID
		}
		if p.CustomFields != nil {
			set, args = append(set, "custom_fields_json = ?"), append(args, nullStr(*p.CustomFields))
			changed["custom_fields"] = true
		}

		if len(changed) == 0 {
			var t Ticket
			q := `SELECT ` + ticketColumns + ticketDisplay + requesterDetail + ticketJoins + requesterJoins + ` WHERE t.tenant_id = ? AND t.id = ?`
			if err := tx.GetContext(ctx, &t, q, tenantID, ticketID); err != nil {
				return fmt.Errorf("reloading ticket: %w", err)
			}
			updated = &t
			return nil
		}

		args = append(args, tenantID, ticketID)
		res, err := tx.ExecContext(ctx,
			`UPDATE tickets SET `+strings.Join(set, ", ")+
				` WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, args...)
		if err != nil {
			return fmt.Errorf("updating ticket: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return platform.ErrSentinelNotFound
		}

		if err := writeTimeline(ctx, tx, tenantID, ticketID, timelineParams{
			EventType: EventFieldUpdated, ActorID: p.ActorID, ActorName: p.ActorName,
			Summary: "Ticket details updated", Detail: changed,
		}); err != nil {
			return err
		}

		var t Ticket
		q := `SELECT ` + ticketColumns + ticketDisplay + requesterDetail + ticketJoins + requesterJoins + ` WHERE t.id = ?`
		if err := tx.GetContext(ctx, &t, q, ticketID); err != nil {
			return fmt.Errorf("reloading ticket: %w", err)
		}
		updated = &t
		return nil
	})

	return updated, err
}
