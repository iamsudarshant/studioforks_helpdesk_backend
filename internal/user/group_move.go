package user

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

// TicketDisposition decides what happens to a moved user's open tickets.
const (
	TicketsKeep     = "KEEP"     // leave with the current assignee
	TicketsTransfer = "TRANSFER" // reassign to a named user
	TicketsClose    = "CLOSE"    // close them as part of the move
)

// MoveGroupParams describes a bulk move — the flow behind "multi-select users
// and move them, with their queries, to Ex-Employees with a last working day".
type MoveGroupParams struct {
	UserIDs           []int64
	TargetGroupID     int64
	LastWorkingDay    *time.Time
	PerUserLWD        map[int64]time.Time
	TicketDisposition string
	ReassignToUserID  *int64
	SetStatus         string
	ActorID           *int64
}

// MoveResult reports the outcome for one user, so the UI can show a per-row
// result rather than a single success or failure.
type MoveResult struct {
	UserID             int64  `json:"-"`
	UserPublicID       string `json:"user_id"`
	Name               string `json:"name"`
	Success            bool   `json:"success"`
	Error              string `json:"error,omitempty"`
	TicketsTransferred int    `json:"tickets_transferred"`
	FromGroup          string `json:"from_group,omitempty"`
	ToGroup            string `json:"to_group"`
	LastWorkingDay     string `json:"last_working_day,omitempty"`
	AccessExpiresOn    string `json:"access_expires_on,omitempty"`
}

// MoveSummary is the whole batch.
type MoveSummary struct {
	BatchID         string       `json:"batch_id"`
	Requested       int          `json:"requested"`
	Moved           int          `json:"moved"`
	Failed          int          `json:"failed"`
	TicketsMoved    int          `json:"tickets_moved"`
	TargetGroup     string       `json:"target_group"`
	AccessMode      string       `json:"access_mode"`
	GracePeriodDays int          `json:"grace_period_days"`
	Results         []MoveResult `json:"results"`
}

// MoveToGroup moves users between groups, optionally stamping a last working
// day and disposing of their open tickets.
//
// Each user is processed in its own transaction: one bad row must not undo the
// other 199 in a 200-user batch, and the caller gets a per-user result.
func (r *Repository) MoveToGroup(ctx context.Context, tenantID int64, p MoveGroupParams) (*MoveSummary, error) {
	target, err := r.GroupByID(ctx, tenantID, p.TargetGroupID)
	if err != nil {
		return nil, err
	}

	batchID := platform.NewULID()
	summary := &MoveSummary{
		BatchID:         batchID,
		Requested:       len(p.UserIDs),
		TargetGroup:     target.Name,
		AccessMode:      target.AccessMode,
		GracePeriodDays: target.GracePeriodDays,
		Results:         make([]MoveResult, 0, len(p.UserIDs)),
	}

	for _, userID := range p.UserIDs {
		result := r.moveOne(ctx, tenantID, userID, target, batchID, p)
		summary.Results = append(summary.Results, result)

		if result.Success {
			summary.Moved++
			summary.TicketsMoved += result.TicketsTransferred
		} else {
			summary.Failed++
		}
	}
	return summary, nil
}

func (r *Repository) moveOne(ctx context.Context, tenantID, userID int64, target *Group, batchID string, p MoveGroupParams) MoveResult {
	result := MoveResult{UserID: userID, ToGroup: target.Name}

	u, err := r.ByID(ctx, tenantID, userID)
	if err != nil {
		result.Error = "This user was not found in your workspace."
		return result
	}
	result.UserPublicID = u.PublicID
	result.Name = u.FullName()

	if u.UserGroupID.Valid {
		if from, err := r.GroupByID(ctx, tenantID, u.UserGroupID.Int64); err == nil {
			result.FromGroup = from.Name
		}
	}

	// Per-user date wins over the batch-wide one, which is what the grid in the
	// wizard edits.
	lwd := p.LastWorkingDay
	if perUser, ok := p.PerUserLWD[userID]; ok {
		lwd = &perUser
	}

	err = r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		status := p.SetStatus
		if status == "" && target.Key == GroupExEmployees {
			status = StatusExEmployee
		}

		set := "user_group_id = ?"
		args := []any{target.ID}

		if lwd != nil {
			set += ", last_working_day = ?"
			args = append(args, *lwd)
		}
		if status != "" {
			set += ", status = ?"
			args = append(args, status)
		}
		if p.ActorID != nil {
			set += ", updated_by = ?"
			args = append(args, *p.ActorID)
		}
		args = append(args, tenantID, userID)

		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET `+set+` WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
			args...); err != nil {
			return fmt.Errorf("updating user: %w", err)
		}

		moved, err := disposeTickets(ctx, tx, tenantID, userID, p)
		if err != nil {
			return err
		}
		result.TicketsTransferred = moved

		// A read-only group must not keep live sessions open.
		if target.AccessMode != "FULL" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE sessions SET revoked_at = UTC_TIMESTAMP(3), revoked_reason = 'GROUP_CHANGED'
				 WHERE user_id = ? AND revoked_at IS NULL`, userID); err != nil {
				return fmt.Errorf("revoking sessions: %w", err)
			}
		}

		var fromGroupID any
		if u.UserGroupID.Valid {
			fromGroupID = u.UserGroupID.Int64
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_group_transfers
				(tenant_id, batch_id, user_id, from_group_id, to_group_id,
				 last_working_day, tickets_transferred, actor_id)
			VALUES (?,?,?,?,?,?,?,?)`,
			tenantID, batchID, userID, fromGroupID, target.ID, lwd, moved, p.ActorID); err != nil {
			return fmt.Errorf("recording transfer: %w", err)
		}
		return nil
	})

	if err != nil {
		result.Error = "This user could not be moved. Nothing was changed for them."
		result.TicketsTransferred = 0
		return result
	}

	result.Success = true
	if lwd != nil {
		result.LastWorkingDay = lwd.Format("2006-01-02")
		if target.GracePeriodDays > 0 {
			result.AccessExpiresOn = lwd.AddDate(0, 0, target.GracePeriodDays).Format("2006-01-02")
		}
	}
	return result
}

// disposeTickets applies the chosen disposition to a moved user's open tickets.
func disposeTickets(ctx context.Context, tx *sqlx.Tx, tenantID, userID int64, p MoveGroupParams) (int, error) {
	const openStatuses = `('NEW','PENDING_DEPT','IN_PROGRESS','PENDING_USER','RESOLVED','REOPENED')`

	switch p.TicketDisposition {
	case TicketsTransfer:
		if p.ReassignToUserID == nil {
			return 0, fmt.Errorf("a target assignee is required to transfer tickets")
		}

		// Reassign tickets the moved user was working on, not the ones they raised.
		res, err := tx.ExecContext(ctx, `
			UPDATE tickets SET assignee_id = ?, updated_by = ?
			WHERE tenant_id = ? AND assignee_id = ? AND deleted_at IS NULL
			  AND status IN `+openStatuses,
			*p.ReassignToUserID, p.ActorID, tenantID, userID)
		if err != nil {
			return 0, fmt.Errorf("transferring tickets: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("reading affected rows: %w", err)
		}

		if n > 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ticket_timeline
					(public_id, tenant_id, ticket_id, event_type, actor_id, summary, detail_json)
				SELECT ?, tenant_id, id, 'TRANSFERRED', ?, 'Reassigned during a bulk user group change', NULL
				FROM tickets
				WHERE tenant_id = ? AND assignee_id = ? AND deleted_at IS NULL
				  AND status IN `+openStatuses,
				platform.NewULID(), p.ActorID, tenantID, *p.ReassignToUserID); err != nil {
				return 0, fmt.Errorf("recording ticket timeline: %w", err)
			}
		}
		return int(n), nil

	case TicketsClose:
		// Close what the user raised; an ex-employee's own open queries are the
		// ones that would otherwise sit unanswered forever.
		res, err := tx.ExecContext(ctx, `
			UPDATE tickets
			SET status = 'CLOSED', closed_at = UTC_TIMESTAMP(3), updated_by = ?
			WHERE tenant_id = ? AND requester_id = ? AND deleted_at IS NULL
			  AND status IN `+openStatuses, p.ActorID, tenantID, userID)
		if err != nil {
			return 0, fmt.Errorf("closing tickets: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("reading affected rows: %w", err)
		}
		return int(n), nil

	default: // TicketsKeep
		var count int
		if err := tx.GetContext(ctx, &count, `
			SELECT COUNT(*) FROM tickets
			WHERE tenant_id = ? AND (requester_id = ? OR assignee_id = ?)
			  AND deleted_at IS NULL AND status IN `+openStatuses,
			tenantID, userID, userID); err != nil {
			return 0, fmt.Errorf("counting tickets: %w", err)
		}
		return 0, nil
	}
}
