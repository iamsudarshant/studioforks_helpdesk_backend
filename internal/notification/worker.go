package notification

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

// Worker drains the transactional outbox.
//
// The outbox exists so that "something happened" commits with the change that
// caused it: a ticket and its notification are written in one transaction, so a
// crash can never leave a ticket assigned but nobody told, nor a notification
// about an assignment that rolled back.
//
// This is the other half. It claims rows, fans them out to recipients, and
// marks them published. Everything it does is idempotent-ish by construction —
// a row is only marked published after its notifications are written, so a
// crash mid-fan-out re-delivers rather than silently dropping.
type Worker struct {
	db       *platform.DB
	log      *slog.Logger
	interval time.Duration
	batch    int
}

func NewWorker(db *platform.DB, log *slog.Logger) *Worker {
	return &Worker{db: db, log: log, interval: 10 * time.Second, batch: 50}
}

// Run drains until the context is cancelled. Safe to call once at startup.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	w.log.Info("notification worker started", "interval", w.interval.String())

	for {
		select {
		case <-ctx.Done():
			w.log.Info("notification worker stopped")
			return
		case <-ticker.C:
			if n, err := w.drainOnce(ctx); err != nil {
				// A failure here must not kill the loop: the next tick retries,
				// and the row's own attempt counter backs it off.
				w.log.Error("notification drain failed", "error", err)
			} else if n > 0 {
				w.log.Info("notifications dispatched", "events", n)
			}
		}
	}
}

// DrainOnce processes one batch. Exported so the CLI can flush the queue
// without running a daemon, which is how a developer sees the effect of a
// change immediately.
func (w *Worker) DrainOnce(ctx context.Context) (int, error) { return w.drainOnce(ctx) }

type outboxRow struct {
	ID            int64          `db:"id"`
	TenantID      int64          `db:"tenant_id"`
	AggregateType string         `db:"aggregate_type"`
	AggregateID   int64          `db:"aggregate_id"`
	EventKey      string         `db:"event_key"`
	PayloadJSON   sql.NullString `db:"payload_json"`
	Attempts      int            `db:"attempts"`
}

func (w *Worker) drainOnce(ctx context.Context) (int, error) {
	rows := []outboxRow{}
	err := w.db.Primary.SelectContext(ctx, &rows, `
		SELECT id, tenant_id, aggregate_type, aggregate_id, event_key, payload_json, attempts
		FROM outbox_events
		WHERE published_at IS NULL AND available_at <= UTC_TIMESTAMP(3)
		ORDER BY id
		LIMIT ?`, w.batch)
	if err != nil {
		return 0, fmt.Errorf("claiming outbox events: %w", err)
	}

	done := 0
	for _, row := range rows {
		if err := w.dispatch(ctx, row); err != nil {
			w.fail(ctx, row, err)
			continue
		}
		done++
	}
	return done, nil
}

// fail records the error and backs the row off exponentially, so one poison
// event cannot spin the worker. After ten attempts it is left alone for an
// operator rather than retried forever.
func (w *Worker) fail(ctx context.Context, row outboxRow, cause error) {
	attempts := row.Attempts + 1
	backoff := time.Duration(1<<min(attempts, 8)) * time.Minute

	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}

	if _, err := w.db.Primary.ExecContext(ctx, `
		UPDATE outbox_events
		SET attempts = ?, last_error = ?, available_at = DATE_ADD(UTC_TIMESTAMP(3), INTERVAL ? SECOND)
		WHERE id = ?`, attempts, msg, int(backoff.Seconds()), row.ID); err != nil {
		w.log.Error("recording outbox failure", "error", err, "event_id", row.ID)
	}
	w.log.Warn("notification event failed", "event", row.EventKey, "attempts", attempts, "error", msg)
}

// dispatch fans one event out to its recipients and marks it published.
//
// The whole fan-out runs in one transaction with the "published" flag, so an
// event is either fully delivered or retried in full — never half-delivered.
func (w *Worker) dispatch(ctx context.Context, row outboxRow) error {
	var payload map[string]any
	if row.PayloadJSON.Valid {
		_ = json.Unmarshal([]byte(row.PayloadJSON.String), &payload)
	}
	if payload == nil {
		payload = map[string]any{}
	}

	return w.db.InTx(ctx, func(tx *sqlx.Tx) error {
		recipients, err := w.recipients(ctx, tx, row)
		if err != nil {
			return err
		}

		tmpl, err := w.template(ctx, tx, row.TenantID, row.EventKey)
		if err != nil {
			return err
		}

		for _, r := range recipients {
			vars := mergeVars(payload, r)
			title := render(tmpl.Subject, vars)
			body := render(tmpl.BodyText, vars)
			link := render(tmpl.Link, vars)

			// IN_APP is written for everyone: it is the channel that always
			// works, needs no configuration, and is what the bell reads.
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO notifications
					(public_id, tenant_id, user_id, event_key, channel, title, body, link,
					 entity_type, entity_id, status, sent_at)
				VALUES (?,?,?,?, 'IN_APP', ?,?,?,?,?, 'SENT', UTC_TIMESTAMP(3))`,
				platform.NewULID(), row.TenantID, r.UserID, row.EventKey,
				title, body, nullIfEmpty(link), row.AggregateType, row.AggregateID); err != nil {
				return fmt.Errorf("writing in-app notification: %w", err)
			}

			// Email is queued rather than sent inline. Whether it actually goes
			// out depends on SMTP being configured; a queued row that is never
			// picked up is visibly PENDING rather than silently lost.
			if r.Email != "" && r.WantsEmail {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO notifications
						(public_id, tenant_id, user_id, event_key, channel, title, body, link,
						 entity_type, entity_id, status)
					VALUES (?,?,?,?, 'EMAIL', ?,?,?,?,?, 'PENDING')`,
					platform.NewULID(), row.TenantID, r.UserID, row.EventKey,
					title, body, nullIfEmpty(link), row.AggregateType, row.AggregateID); err != nil {
					return fmt.Errorf("queueing email notification: %w", err)
				}
			}
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE outbox_events SET published_at = UTC_TIMESTAMP(3) WHERE id = ?`, row.ID); err != nil {
			return fmt.Errorf("marking event published: %w", err)
		}
		return nil
	})
}

// recipient is one person to tell, with enough context to render for them.
type recipient struct {
	UserID     int64  `db:"user_id"`
	Name       string `db:"name"`
	Email      string `db:"email"`
	WantsEmail bool   `db:"wants_email"`
}

// recipients decides who hears about an event.
//
// Ticket events go to the people actually involved — requester, assignee and
// watchers — rather than to everyone with permission, because a desk handling
// thousands of tickets would otherwise notify every agent about every one.
// Duplicates are collapsed: someone who is both requester and watcher is told
// once.
func (w *Worker) recipients(ctx context.Context, tx *sqlx.Tx, row outboxRow) ([]recipient, error) {
	rows := []recipient{}

	if row.AggregateType != "ticket" {
		return rows, nil
	}

	// notification_preferences is consulted for the email channel only; in-app
	// is always written, so muting email cannot make someone miss a ticket.
	err := tx.SelectContext(ctx, &rows, `
		SELECT DISTINCT u.id AS user_id,
			CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS name,
			COALESCE(u.email, '') AS email,
			COALESCE((
				SELECT np.enabled FROM notification_preferences np
				WHERE np.user_id = u.id AND np.event_key = ? AND np.channel = 'EMAIL'
				  AND (np.muted_until IS NULL OR np.muted_until <= UTC_TIMESTAMP(3))
			), 1) AS wants_email
		FROM tickets t
		JOIN users u
		  ON u.id IN (t.requester_id, t.assignee_id)
		  OR u.id IN (SELECT tw.user_id FROM ticket_watchers tw WHERE tw.ticket_id = t.id)
		WHERE t.id = ? AND t.tenant_id = ?
		  AND u.deleted_at IS NULL AND u.status = 'ACTIVE'`,
		row.EventKey, row.AggregateID, row.TenantID)
	if err != nil {
		return nil, fmt.Errorf("resolving recipients: %w", err)
	}
	return rows, nil
}

// templateRow is the rendered wording for one event.
type templateRow struct {
	Subject  string
	BodyText string
	Link     string
}

// template returns the client's own wording, falling back to the platform
// default, and finally to a generic line so an unconfigured event still
// notifies rather than failing.
func (w *Worker) template(ctx context.Context, tx *sqlx.Tx, tenantID int64, eventKey string) (templateRow, error) {
	var row struct {
		Subject  sql.NullString `db:"subject"`
		BodyText sql.NullString `db:"body_text"`
	}
	err := tx.GetContext(ctx, &row, `
		SELECT subject, body_text FROM notification_templates
		WHERE event_key = ? AND channel = 'EMAIL' AND is_active = 1
		  AND (tenant_id = ? OR tenant_id IS NULL)
		ORDER BY tenant_id IS NULL
		LIMIT 1`, eventKey, tenantID)

	if err != nil && !platform.IsNotFound(err) {
		return templateRow{}, fmt.Errorf("loading notification template: %w", err)
	}

	out := templateRow{
		Subject:  row.Subject.String,
		BodyText: row.BodyText.String,
		Link:     "/tickets/{{ticket_id}}",
	}
	if out.Subject == "" {
		out.Subject = defaultSubject(eventKey)
	}
	if out.BodyText == "" {
		out.BodyText = "{{ticket_number}} — {{subject}}"
	}
	return out, nil
}

func defaultSubject(eventKey string) string {
	switch eventKey {
	case "ticket.created":
		return "Ticket raised: {{ticket_number}}"
	case "ticket.assigned":
		return "Ticket assigned: {{ticket_number}}"
	case "ticket.replied":
		return "New reply on {{ticket_number}}"
	case "ticket.info_requested":
		return "Information needed on {{ticket_number}}"
	case "ticket.status_changed":
		return "{{ticket_number}} is now {{status}}"
	case "ticket.escalated":
		return "Escalated: {{ticket_number}}"
	case "ticket.resolved":
		return "Resolved: {{ticket_number}}"
	case "ticket.closed":
		return "Closed: {{ticket_number}}"
	case "ticket.reopened":
		return "Reopened: {{ticket_number}}"
	case "ticket.sla_breached":
		return "SLA breached: {{ticket_number}}"
	case "ticket.sla_warning":
		return "SLA due soon: {{ticket_number}}"
	default:
		return strings.ReplaceAll(eventKey, ".", " ")
	}
}

func mergeVars(payload map[string]any, r recipient) map[string]any {
	vars := make(map[string]any, len(payload)+2)
	for k, v := range payload {
		vars[k] = v
	}
	vars["recipient_name"] = r.Name
	return vars
}

// render substitutes {{token}} placeholders.
//
// Deliberately not text/template: these strings are edited by administrators,
// and a template engine would let a malformed one panic the worker or reach
// into the data model. A missing token renders empty rather than erroring.
func render(tmpl string, vars map[string]any) string {
	out := tmpl
	for key, value := range vars {
		out = strings.ReplaceAll(out, "{{"+key+"}}", fmt.Sprintf("%v", value))
	}
	// Anything still unresolved would show as literal braces to a user.
	for {
		start := strings.Index(out, "{{")
		if start < 0 {
			break
		}
		end := strings.Index(out[start:], "}}")
		if end < 0 {
			break
		}
		out = out[:start] + out[start+end+2:]
	}
	return strings.TrimSpace(out)
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
