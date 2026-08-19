package ticket

// Watcher storage.
//
// A watcher is somebody kept informed about a ticket they do not own and are not
// working: a supervisor following an escalation, a partner tracking their
// entity's claim. The row is deliberately thin — the interesting part is the
// join back to the person, so the panel can name them without a request per row.

import (
	"context"
	"fmt"
)

// Watcher is one row of the panel.
type Watcher struct {
	UserID   int64  `db:"user_id"`
	PublicID string `db:"public_id"`
	Name     string `db:"name"`
	Email    string `db:"email"`
	Reason   string `db:"reason"`
}

func (r *Repository) Watchers(ctx context.Context, tenantID, ticketID int64) ([]Watcher, error) {
	rows := []Watcher{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT w.user_id,
		       u.public_id,
		       TRIM(CONCAT(u.first_name, ' ', COALESCE(u.last_name, ''))) AS name,
		       COALESCE(u.email, '')  AS email,
		       COALESCE(w.reason, '') AS reason
		FROM ticket_watchers w
		JOIN users u ON u.id = w.user_id
		WHERE w.tenant_id = ? AND w.ticket_id = ? AND u.deleted_at IS NULL
		ORDER BY u.first_name, u.last_name`, tenantID, ticketID)
	if err != nil {
		return nil, fmt.Errorf("listing watchers: %w", err)
	}
	return rows, nil
}

// AddWatcher is an upsert: watching something twice is the same as watching it
// once, and the button that calls this cannot know which it is.
func (r *Repository) AddWatcher(ctx context.Context, tenantID, ticketID, userID int64, reason string) error {
	_, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO ticket_watchers (tenant_id, ticket_id, user_id, reason)
		VALUES (?,?,?,?)
		ON DUPLICATE KEY UPDATE reason = VALUES(reason)`,
		tenantID, ticketID, userID, nullStr(reason))
	if err != nil {
		return fmt.Errorf("adding watcher: %w", err)
	}
	return nil
}

// RemoveWatcher is silent about a watcher who was not there — the caller's
// intent is "this person is not watching", which is already true.
func (r *Repository) RemoveWatcher(ctx context.Context, tenantID, ticketID, userID int64) error {
	_, err := r.db.Primary.ExecContext(ctx,
		`DELETE FROM ticket_watchers WHERE tenant_id = ? AND ticket_id = ? AND user_id = ?`,
		tenantID, ticketID, userID)
	if err != nil {
		return fmt.Errorf("removing watcher: %w", err)
	}
	return nil
}

// WatcherIDs is what the notification fan-out reads: everybody to tell when
// something happens on this ticket, beyond the requester and the assignee.
func (r *Repository) WatcherIDs(ctx context.Context, tenantID, ticketID int64) ([]int64, error) {
	ids := []int64{}
	err := r.db.Primary.SelectContext(ctx, &ids, `
		SELECT w.user_id FROM ticket_watchers w
		JOIN users u ON u.id = w.user_id
		WHERE w.tenant_id = ? AND w.ticket_id = ?
		  AND u.deleted_at IS NULL AND u.status = 'ACTIVE'`, tenantID, ticketID)
	if err != nil {
		return nil, fmt.Errorf("listing watcher ids: %w", err)
	}
	return ids, nil
}
