package user

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// EmploymentChange is one employee crossing between Active and Ex-Employee.
//
// Deliberately narrower than MoveGroupParams. The bulk group-move exists for
// an HR batch and asks what should happen to everybody's tickets; this is an
// administrator changing one person on one screen, where the only questions
// worth asking are when they left and who now handles their queries.
type EmploymentChange struct {
	UserID int64
	// Status is the value being moved to: StatusActive or StatusExEmployee.
	Status string
	// LastWorkingDay is required leaving, ignored returning. Returning clears
	// the stored date — somebody who is back has no leaving date any more, and
	// leaving one behind would keep them inside the ex-employee grace window.
	LastWorkingDay *time.Time
	// HandlingAgentID is required in both directions: leaving, somebody has to
	// pick up the open queries; returning, somebody has to be their contact.
	HandlingAgentID int64
	ActorID         *int64
}

// EmploymentResult reports what the transition actually did, so the response
// can say "3 open tickets moved to Priya" rather than only "saved".
type EmploymentResult struct {
	Status         string     `json:"status"`
	LastWorkingDay *time.Time `json:"last_working_day"`
	GroupKey       string     `json:"group_key,omitempty"`
	TicketsMoved   int64      `json:"tickets_moved"`
	AgentName      string     `json:"agent_name,omitempty"`
}

// ChangeEmployment moves one employee between Active and Ex-Employee.
//
// Everything happens in one transaction because the parts are not independently
// valid: an ex-employee in the active group still has write access, and an
// employee whose tickets moved but whose status did not would be handled by an
// agent the UI says nothing about.
func (r *Repository) ChangeEmployment(ctx context.Context, tenantID int64, in EmploymentChange) (*EmploymentResult, error) {
	if in.Status != StatusActive && in.Status != StatusExEmployee {
		return nil, fmt.Errorf("unsupported employment status %q", in.Status)
	}

	out := &EmploymentResult{Status: in.Status}

	err := r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		// The group carries the access mode — EX_EMPLOYEES is read-only — so
		// the status and the group have to move together or the two disagree
		// about what this person may do.
		groupKey := GroupActiveEmployees
		if in.Status == StatusExEmployee {
			groupKey = GroupExEmployees
		}

		var groupID sql.NullInt64
		var id int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM user_groups WHERE tenant_id = ? AND group_key = ?`,
			tenantID, groupKey).Scan(&id)
		switch {
		case err == nil:
			groupID = sql.NullInt64{Int64: id, Valid: true}
			out.GroupKey = groupKey
		case err == sql.ErrNoRows:
			// A client that never had the system groups seeded still gets a
			// correct status; the group is an optimisation of access checks,
			// not the source of truth for them.
		default:
			return fmt.Errorf("loading %s group: %w", groupKey, err)
		}

		var lwd any
		if in.Status == StatusExEmployee && in.LastWorkingDay != nil {
			lwd = in.LastWorkingDay.Format("2006-01-02")
			out.LastWorkingDay = in.LastWorkingDay
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE users
			   SET status = ?,
			       last_working_day = ?,
			       user_group_id = COALESCE(?, user_group_id),
			       handling_agent_id = ?,
			       employment_changed_at = UTC_TIMESTAMP(3),
			       employment_changed_by = ?
			 WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
			in.Status, lwd, groupID, in.HandlingAgentID, in.ActorID, tenantID, in.UserID)
		if err != nil {
			return fmt.Errorf("changing employment status: %w", err)
		}
		if err := affected(res); err != nil {
			return err
		}

		if in.Status == StatusExEmployee {
			// Somebody who has left cannot answer a request for information,
			// so their open tickets move to the agent taking them on. Closed
			// and cancelled ones stay where they are: reassigning finished
			// work rewrites history for no benefit.
			moved, err := tx.ExecContext(ctx, `
				UPDATE tickets
				   SET assignee_id = ?
				 WHERE tenant_id = ? AND requester_id = ?
				   AND status NOT IN ('CLOSED', 'CANCELLED')
				   AND deleted_at IS NULL`,
				in.HandlingAgentID, tenantID, in.UserID)
			if err != nil {
				return fmt.Errorf("reassigning open tickets: %w", err)
			}
			out.TicketsMoved, _ = moved.RowsAffected()

			// Leaving ends the session. Without this they keep working until
			// the access token expires, which is exactly the window a departing
			// employee should not have.
			if _, err := tx.ExecContext(ctx, `
				UPDATE sessions SET revoked_at = UTC_TIMESTAMP(3), revoked_reason = 'EMPLOYMENT_ENDED'
				 WHERE user_id = ? AND revoked_at IS NULL`, in.UserID); err != nil {
				return fmt.Errorf("revoking sessions: %w", err)
			}
		}

		_ = tx.QueryRowContext(ctx,
			`SELECT TRIM(CONCAT(first_name, ' ', COALESCE(last_name, ''))) FROM users WHERE id = ?`,
			in.HandlingAgentID).Scan(&out.AgentName)

		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AssignableAgents lists who an employee's queries may be handed to.
//
// "Agent" here means somebody who works the desk for this client, which is two
// distinct populations:
//
//   - ComplyDesk's own staff, who live in the platform tenant and reach the
//     client through a live assignment; and
//   - the client's own administrators, who are the point of contact in a
//     workspace the desk does not staff directly.
//
// Partners and employees are deliberately excluded. A partner is segmented to
// their own entity — handing them another entity's departing employee would
// give them work they cannot open — and an employee cannot be the person their
// own queries escalate to.
//
// The role test uses EXISTS rather than a join, because a user holding two
// roles would otherwise appear twice and, worse, could match on one role while
// being excluded by the other.
func (r *Repository) AssignableAgents(ctx context.Context, tenantID int64) ([]User, error) {
	rows := []User{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT `+prefixed(userColumns, "u")+`
		FROM users u
		WHERE u.deleted_at IS NULL
		  AND u.status = ?
		  AND (
		        EXISTS (SELECT 1 FROM agent_tenant_assignments a
		                 WHERE a.agent_user_id = u.id AND a.tenant_id = ?
		                   AND a.revoked_at IS NULL)
		        OR (u.tenant_id = ? AND EXISTS (
		                SELECT 1 FROM user_roles ur JOIN roles rl ON rl.id = ur.role_id
		                 WHERE ur.user_id = u.id AND rl.role_key = ?))
		      )
		  AND NOT EXISTS (
		        SELECT 1 FROM user_roles ur2 JOIN roles rl2 ON rl2.id = ur2.role_id
		         WHERE ur2.user_id = u.id AND rl2.role_key IN (?, ?))
		ORDER BY u.first_name, u.last_name`,
		StatusActive, tenantID, tenantID,
		RoleClientAdmin, RoleClientExecutive, RoleEmployee)
	if err != nil {
		return nil, fmt.Errorf("listing assignable agents: %w", err)
	}
	return rows, nil
}
