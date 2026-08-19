package ticket

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/notification"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// SLASweeper watches for tickets that are about to miss, or have missed, their
// resolution target.
//
// Due timestamps are computed when a ticket is created, but nothing acts on
// them: a target only means something if crossing it is noticed. This is what
// notices.
//
// Two events are raised. A warning fires before the deadline, while there is
// still time to do something about it; a breach fires after. Each is recorded
// in ticket_sla_events so it is emitted once per ticket rather than every time
// the sweep runs — without that, a ticket overdue for a week would notify every
// five minutes.
type SLASweeper struct {
	db        *platform.DB
	publisher *notification.Publisher
	log       *slog.Logger
	interval  time.Duration
	// warnBefore is how far ahead of the deadline the warning fires. Two hours
	// matches the brief's "SLA-Breach-Imminent" trigger.
	warnBefore time.Duration
}

func NewSLASweeper(db *platform.DB, publisher *notification.Publisher, log *slog.Logger) *SLASweeper {
	return &SLASweeper{
		db: db, publisher: publisher, log: log,
		interval: 5 * time.Minute, warnBefore: 2 * time.Hour,
	}
}

// Run sweeps until the context is cancelled.
func (s *SLASweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	s.log.Info("sla sweeper started", "interval", s.interval.String())

	// Sweep once immediately: after a restart, anything that breached while the
	// process was down should be flagged now rather than one interval later.
	if err := s.sweepOnce(ctx); err != nil {
		s.log.Error("sla sweep failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			s.log.Info("sla sweeper stopped")
			return
		case <-ticker.C:
			if err := s.sweepOnce(ctx); err != nil {
				s.log.Error("sla sweep failed", "error", err)
			}
		}
	}
}

// SweepOnce runs a single pass. Exported so the CLI can trigger it.
func (s *SLASweeper) SweepOnce(ctx context.Context) error { return s.sweepOnce(ctx) }

type slaCandidate struct {
	ID           int64  `db:"id"`
	TenantID     int64  `db:"tenant_id"`
	TicketNumber string `db:"ticket_number"`
	Subject      string `db:"subject"`
	Status       string `db:"status"`
	PublicID     string `db:"public_id"`
}

func (s *SLASweeper) sweepOnce(ctx context.Context) error {
	if err := s.flag(ctx, "SLA_BREACHED", "ticket.sla_breached", `
		t.resolution_due_at IS NOT NULL
		AND t.resolution_due_at <= UTC_TIMESTAMP(3)`); err != nil {
		return err
	}
	return s.flag(ctx, "SLA_WARNING", "ticket.sla_warning", `
		t.resolution_due_at IS NOT NULL
		AND t.resolution_due_at > UTC_TIMESTAMP(3)
		AND t.resolution_due_at <= DATE_ADD(UTC_TIMESTAMP(3), INTERVAL ? MINUTE)`)
}

// flag finds tickets matching a condition that have not already had this event
// recorded, marks them, and publishes.
func (s *SLASweeper) flag(ctx context.Context, event, eventKey, condition string) error {
	args := []any{event}
	if event == "SLA_WARNING" {
		args = append(args, int(s.warnBefore.Minutes()))
	}

	rows := []slaCandidate{}
	// A paused ticket is excluded: PENDING_EMPLOYEE stops the clock, so the desk
	// must not be blamed for time spent waiting on the requester.
	err := s.db.Primary.SelectContext(ctx, &rows, `
		SELECT t.id, t.tenant_id, t.ticket_number, t.subject, t.status, t.public_id
		FROM tickets t
		WHERE t.deleted_at IS NULL
		  AND t.status NOT IN ('RESOLVED','CLOSED','CANCELLED','PENDING_EMPLOYEE')
		  AND `+condition+`
		  AND NOT EXISTS (
		      SELECT 1 FROM ticket_sla_events e
		      WHERE e.ticket_id = t.id AND e.event = ?)
		LIMIT 500`, append(args[1:], args[0])...)
	if err != nil {
		return fmt.Errorf("finding %s candidates: %w", event, err)
	}

	for _, row := range rows {
		if err := s.record(ctx, row, event, eventKey); err != nil {
			s.log.Error("recording sla event", "error", err, "ticket", row.TicketNumber)
			continue
		}
	}

	if len(rows) > 0 {
		s.log.Info("sla events raised", "event", event, "tickets", len(rows))
	}
	return nil
}

// record writes the marker, the timeline entry and the outbox event together,
// so a ticket cannot be flagged without its notification being queued.
func (s *SLASweeper) record(ctx context.Context, row slaCandidate, event, eventKey string) error {
	return s.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ticket_sla_events (tenant_id, ticket_id, event, level, occurred_at)
			VALUES (?,?,?,0,UTC_TIMESTAMP(3))`,
			row.TenantID, row.ID, event); err != nil {
			return fmt.Errorf("writing sla event: %w", err)
		}

		if event == "SLA_BREACHED" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE tickets SET is_sla_breached = 1 WHERE id = ?`, row.ID); err != nil {
				return fmt.Errorf("flagging breach: %w", err)
			}
		}

		summary := "SLA breached"
		if event == "SLA_WARNING" {
			summary = "SLA due soon"
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ticket_timeline
				(public_id, tenant_id, ticket_id, event_type, actor_name_snapshot, actor_role,
				 visibility, summary, created_at)
			VALUES (?,?,?,?, 'System', 'SYSTEM', 'PUBLIC', ?, UTC_TIMESTAMP(3))`,
			platform.NewULID(), row.TenantID, row.ID, event, summary); err != nil {
			return fmt.Errorf("writing sla timeline entry: %w", err)
		}

		return notification.PublishTx(ctx, tx, row.TenantID, eventKey, "ticket", row.ID, map[string]any{
			"ticket_number": row.TicketNumber,
			"ticket_id":     row.PublicID,
			"subject":       row.Subject,
			"status":        row.Status,
		})
	})
}
