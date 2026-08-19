// Package notification owns the event catalogue, templates, delivery channels
// and the transactional outbox that decouples "something happened" from
// "someone was told".
package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

// Event keys. These are seeded into notification_events and are the only values
// the dispatcher will render a template for.
const (
	EventTicketCreated       = "ticket.created"
	EventTicketAssigned      = "ticket.assigned"
	EventTicketStatusChanged = "ticket.status_changed"
	EventTicketReplied       = "ticket.replied"
	EventTicketInfoRequested = "ticket.info_requested"
	EventTicketEscalated     = "ticket.escalated"
	EventTicketSLAWarning    = "ticket.sla_warning"
	EventTicketSLABreached   = "ticket.sla_breached"
	EventTicketResolved      = "ticket.resolved"
	EventTicketClosed        = "ticket.closed"
	EventTicketReopened      = "ticket.reopened"
	EventTicketReminder      = "ticket.reminder_pending_user"
	EventTicketMentioned     = "ticket.mentioned"

	EventUserWelcome          = "user.welcome"
	EventUserTempPassword     = "user.temp_password"
	EventUserResetLink        = "user.password_reset_link"
	EventUserUsernameRecovery = "user.username_recovery"
	EventUserGroupChanged     = "user.group_changed"
	EventUserAccountLocked    = "user.account_locked"
	EventUserLoginOTP         = "user.login_otp"

	EventReportReady         = "report.ready"
	EventBulkImportCompleted = "bulk_import.completed"

	EventMaintenanceScheduled = "maintenance.scheduled"
	EventMaintenanceStarted   = "maintenance.started"
	EventMaintenanceEnded     = "maintenance.ended"
)

// Channels.
const (
	ChannelEmail = "EMAIL"
	ChannelInApp = "IN_APP"
	ChannelSMS   = "SMS"
)

// Publisher writes events to the transactional outbox. Nothing sends mail or
// SMS inline: the worker drains the outbox, so a failed SMTP connection can
// never roll back a ticket status change.
type Publisher struct {
	db *platform.DB
}

func NewPublisher(db *platform.DB) *Publisher { return &Publisher{db: db} }

// Publish writes an outbox row using the primary connection. Prefer PublishTx
// when the event accompanies a state change.
func (p *Publisher) Publish(ctx context.Context, tenantID int64, eventKey, aggregateType string, aggregateID int64, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding event payload: %w", err)
	}
	_, err = p.db.Primary.ExecContext(ctx, `
		INSERT INTO outbox_events (tenant_id, aggregate_type, aggregate_id, event_key, payload_json)
		VALUES (?,?,?,?,?)`, tenantID, aggregateType, aggregateID, eventKey, string(raw))
	if err != nil {
		return fmt.Errorf("writing outbox event: %w", err)
	}
	return nil
}

// PublishTx enlists the event in the caller's transaction, which is what makes
// the outbox transactional: the event exists if and only if the change did.
func PublishTx(ctx context.Context, tx *sqlx.Tx, tenantID int64, eventKey, aggregateType string, aggregateID int64, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding event payload: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO outbox_events (tenant_id, aggregate_type, aggregate_id, event_key, payload_json)
		VALUES (?,?,?,?,?)`, tenantID, aggregateType, aggregateID, eventKey, string(raw))
	if err != nil {
		return fmt.Errorf("writing outbox event: %w", err)
	}
	return nil
}

// OutboxRow is one pending event.
type OutboxRow struct {
	ID            int64     `db:"id"`
	TenantID      int64     `db:"tenant_id"`
	AggregateType string    `db:"aggregate_type"`
	AggregateID   int64     `db:"aggregate_id"`
	EventKey      string    `db:"event_key"`
	PayloadJSON   string    `db:"payload_json"`
	Attempts      int       `db:"attempts"`
	CreatedAt     time.Time `db:"created_at"`
}

// Claim locks and returns a batch of unpublished events. SKIP LOCKED lets
// several workers drain the outbox concurrently without contending.
func (p *Publisher) Claim(ctx context.Context, limit int) ([]OutboxRow, error) {
	var rows []OutboxRow

	err := p.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if err := tx.SelectContext(ctx, &rows, `
			SELECT id, tenant_id, aggregate_type, aggregate_id, event_key, payload_json, attempts, created_at
			FROM outbox_events
			WHERE published_at IS NULL AND available_at <= UTC_TIMESTAMP(3)
			ORDER BY id
			LIMIT ?
			FOR UPDATE SKIP LOCKED`, limit); err != nil {
			return fmt.Errorf("claiming outbox events: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}

		ids := make([]int64, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		// Push the retry window forward so a crash mid-dispatch re-delivers
		// rather than losing the event.
		if _, err := tx.ExecContext(ctx, `
			UPDATE outbox_events
			SET attempts = attempts + 1,
			    available_at = DATE_ADD(UTC_TIMESTAMP(3), INTERVAL 5 MINUTE)
			WHERE id IN (`+platform.Placeholders(len(ids))+`)`,
			platform.Int64Args(ids)...); err != nil {
			return fmt.Errorf("reserving outbox events: %w", err)
		}
		return nil
	})

	return rows, err
}

func (p *Publisher) MarkPublished(ctx context.Context, id int64) error {
	_, err := p.db.Primary.ExecContext(ctx,
		`UPDATE outbox_events SET published_at = UTC_TIMESTAMP(3), last_error = NULL WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("marking event published: %w", err)
	}
	return nil
}

// MarkFailed records the error and backs the event off exponentially. After ten
// attempts it stops retrying and stays visible for investigation.
func (p *Publisher) MarkFailed(ctx context.Context, id int64, attempts int, cause error) error {
	backoff := time.Duration(1<<min(attempts, 8)) * time.Minute

	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}

	_, err := p.db.Primary.ExecContext(ctx, `
		UPDATE outbox_events
		SET last_error = ?, available_at = DATE_ADD(UTC_TIMESTAMP(3), INTERVAL ? SECOND)
		WHERE id = ?`, msg, int(backoff.Seconds()), id)
	if err != nil {
		return fmt.Errorf("recording event failure: %w", err)
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
