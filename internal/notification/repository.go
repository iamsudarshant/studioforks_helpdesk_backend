package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/karmamgmt/complydesk/internal/platform"
)

type Repository struct {
	db *platform.DB
}

func NewRepository(db *platform.DB) *Repository { return &Repository{db: db} }

// Audience is the slice of the notification stream one caller may read.
//
// This is the single place the visibility rules in the brief are expressed:
//
//   - an employee sees only notifications addressed to them
//   - a partner sees their client's, narrowed to the people inside their own
//     entity/department allocation
//   - an agent sees every client they are assigned to, or every client when
//     they are assigned to none
//   - an admin sees everything
//
// It is always derived from the session (see AudienceFor) and never from the
// request, so a caller cannot widen it by asking.
type Audience struct {
	// UserID is the caller. Rows addressed to them are always visible,
	// whatever else the audience allows.
	UserID int64
	// Mine restricts the feed to rows addressed to the caller — what the bell
	// reads, and the only mode an employee has.
	Mine bool
	// TenantIDs limits the feed to these clients. Ignored when AllTenants.
	TenantIDs []int64
	// AllTenants lifts the client restriction entirely.
	AllTenants bool
	// RecipientIDs limits which people's notifications are visible within the
	// client. Nil means unrestricted; an empty non-nil slice means nobody but
	// the caller themselves.
	RecipientIDs []int64
}

const notificationColumns = `n.id, n.public_id, n.tenant_id, n.user_id, n.event_key,
	n.channel, n.title, n.body, n.link, n.entity_type, n.entity_id, n.status,
	n.read_at, n.created_at,
	CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS recipient_name,
	tn.name AS client_name, tn.slug AS client_slug,
	ev.event_group AS event_group,
	tk.public_id AS target_public_id`

// notificationFrom joins the notification's subject so a link can be checked
// before it is offered.
//
// A notification outlives what it is about. Follow one whose ticket has since
// been removed and the reader lands on "That ticket was not found" with no way
// to tell whether they lack access or the thing is simply gone — the worst
// reading of a permission error. Resolving the subject here lets the handler
// present the row as history rather than as a link that fails on click.
const notificationFrom = `
	FROM notifications n
	JOIN users u              ON u.id = n.user_id
	LEFT JOIN tenants tn      ON tn.id = n.tenant_id
	LEFT JOIN notification_events ev ON ev.event_key = n.event_key
	LEFT JOIN tickets tk      ON n.entity_type = 'ticket'
	                         AND tk.id = n.entity_id
	                         AND tk.deleted_at IS NULL`

// where renders the audience plus the caller's own filters.
//
// The in-app channel is the feed. EMAIL and SMS rows are delivery records for
// the same event; including them would show every notification twice.
func (a Audience) where(unreadOnly bool, eventGroup, query string) ([]string, []any) {
	where := []string{"n.channel = 'IN_APP'"}
	args := []any{}

	if a.Mine {
		where = append(where, "n.user_id = ?")
		args = append(args, a.UserID)
	} else {
		// "Everything I may see" — the caller's own rows, plus everyone else's
		// within the clients and people they administer.
		clauses := []string{"n.user_id = ?"}
		args = append(args, a.UserID)

		wide := []string{}
		wideArgs := []any{}
		switch {
		case a.AllTenants:
			// No client restriction.
		case len(a.TenantIDs) > 0:
			wide = append(wide, "n.tenant_id IN ("+platform.Placeholders(len(a.TenantIDs))+")")
			wideArgs = append(wideArgs, platform.Int64Args(a.TenantIDs)...)
		default:
			// No client reach at all: the caller sees only their own rows.
			wide = append(wide, "1 = 0")
		}

		if a.RecipientIDs != nil {
			if len(a.RecipientIDs) == 0 {
				wide = append(wide, "1 = 0")
			} else {
				wide = append(wide, "n.user_id IN ("+platform.Placeholders(len(a.RecipientIDs))+")")
				wideArgs = append(wideArgs, platform.Int64Args(a.RecipientIDs)...)
			}
		}

		if len(wide) == 0 {
			// Unrestricted: the OR collapses to "everything".
			clauses = []string{"1 = 1"}
			args = args[:len(args)-1]
		} else {
			clauses = append(clauses, "("+strings.Join(wide, " AND ")+")")
			args = append(args, wideArgs...)
		}
		where = append(where, "("+strings.Join(clauses, " OR ")+")")
	}

	if unreadOnly {
		where = append(where, "n.read_at IS NULL")
	}
	if g := strings.TrimSpace(eventGroup); g != "" {
		where = append(where, "ev.event_group = ?")
		args = append(args, strings.ToUpper(g))
	}
	if q := strings.TrimSpace(query); q != "" {
		where = append(where, "(n.title LIKE ? OR n.body LIKE ?)")
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	return where, args
}

// List returns a page of the caller's visible notifications, newest first.
func (r *Repository) List(ctx context.Context, a Audience, unreadOnly bool, eventGroup, query string, page platform.Page) ([]Notification, int64, error) {
	where, args := a.where(unreadOnly, eventGroup, query)
	clause := " WHERE " + strings.Join(where, " AND ")

	var total int64
	if err := r.db.Primary.GetContext(ctx, &total,
		`SELECT COUNT(*)`+notificationFrom+clause, args...); err != nil {
		return nil, 0, fmt.Errorf("counting notifications: %w", err)
	}

	rows := []Notification{}
	q := `SELECT ` + notificationColumns + notificationFrom + clause +
		` ORDER BY n.created_at DESC, n.id DESC LIMIT ? OFFSET ?`
	args = append(args, page.PerPage, page.Offset())

	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, 0, fmt.Errorf("listing notifications: %w", err)
	}
	return rows, total, nil
}

// UnreadCount counts what the bell badges.
//
// Deliberately counts only rows addressed to the caller, whatever their
// audience: a badge that included every colleague's unread notifications could
// never be cleared, so it would stop meaning anything.
func (r *Repository) UnreadCount(ctx context.Context, userID int64) (int64, error) {
	var n int64
	err := r.db.Primary.GetContext(ctx, &n, `
		SELECT COUNT(*) FROM notifications
		WHERE user_id = ? AND channel = 'IN_APP' AND read_at IS NULL`, userID)
	if err != nil {
		return 0, fmt.Errorf("counting unread notifications: %w", err)
	}
	return n, nil
}

// MarkRead marks one row read. Scoped to the recipient, so nobody can clear
// somebody else's notification even if they can see it in a wider feed.
func (r *Repository) MarkRead(ctx context.Context, userID int64, publicID string) error {
	res, err := r.db.Primary.ExecContext(ctx, `
		UPDATE notifications SET read_at = UTC_TIMESTAMP(3)
		WHERE public_id = ? AND user_id = ? AND read_at IS NULL`, publicID, userID)
	if err != nil {
		return fmt.Errorf("marking notification read: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading affected rows: %w", err)
	}
	if n == 0 {
		// Already read, or not theirs. Both are a no-op rather than an error:
		// the client's optimistic update has already shown it as read.
		return nil
	}
	return nil
}

func (r *Repository) MarkAllRead(ctx context.Context, userID int64) (int64, error) {
	res, err := r.db.Primary.ExecContext(ctx, `
		UPDATE notifications SET read_at = UTC_TIMESTAMP(3)
		WHERE user_id = ? AND channel = 'IN_APP' AND read_at IS NULL`, userID)
	if err != nil {
		return 0, fmt.Errorf("marking notifications read: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reading affected rows: %w", err)
	}
	return n, nil
}

// RecipientsInScope resolves the people a scoped partner may see notifications
// for: anyone belonging to one of the entities, sites or departments the
// partner themselves is allocated to.
//
// Membership is read from two places, because the model carries it in two:
// users.entity_id/site_id/department_id is where an employee's own posting
// lives, while the *_assignments tables record partners allocated across
// several. Both count, so a partner sees their establishment's employees as
// well as the colleagues sharing the allocation.
//
// Returns a non-nil empty slice when the allocation matches nobody, which the
// audience reads as "only your own" rather than "everyone".
func (r *Repository) RecipientsInScope(ctx context.Context, tenantID int64,
	entityIDs, siteIDs, deptIDs []int64) ([]int64, error) {

	clauses := []string{}
	args := []any{tenantID}

	add := func(column, table, fk string, ids []int64) {
		if len(ids) == 0 {
			return
		}
		placeholders := platform.Placeholders(len(ids))
		clauses = append(clauses, fmt.Sprintf("u.%s IN (%s)", column, placeholders))
		args = append(args, platform.Int64Args(ids)...)

		clauses = append(clauses, fmt.Sprintf(
			"u.id IN (SELECT a.user_id FROM %s a WHERE a.tenant_id = u.tenant_id AND a.%s IN (%s))",
			table, fk, placeholders))
		args = append(args, platform.Int64Args(ids)...)
	}
	add("entity_id", "entity_assignments", "entity_id", entityIDs)
	add("site_id", "site_assignments", "site_id", siteIDs)
	add("department_id", "department_assignments", "department_id", deptIDs)

	if len(clauses) == 0 {
		return []int64{}, nil
	}

	ids := []int64{}
	q := `SELECT DISTINCT u.id FROM users u
		WHERE u.tenant_id = ? AND u.deleted_at IS NULL AND (` +
		strings.Join(clauses, " OR ") + `)`
	if err := r.db.Primary.SelectContext(ctx, &ids, q, args...); err != nil {
		return nil, fmt.Errorf("resolving notification recipients in scope: %w", err)
	}
	return ids, nil
}

// --- the event catalogue ----------------------------------------------------

// Events lists the catalogue, narrowed to the portal asking.
//
// `portal` empty returns everything, which is what the platform's own
// configuration screens want. A preferences screen passes the caller's portal:
// offering an employee a switch for `report.ready` is offering a setting that
// can never do anything, and a preference that changes nothing is worse than an
// absent one — it reads as broken rather than as not applicable.
func (r *Repository) Events(ctx context.Context, portal string) ([]Event, error) {
	where := []string{"1 = 1"}
	args := []any{}

	if portal = strings.TrimSpace(portal); portal != "" {
		// FIND_IN_SET over the comma-separated audience. The set is four values
		// wide and read whole, so it needs no join table.
		where = append(where, "FIND_IN_SET(?, audience) > 0")
		args = append(args, portal)
	}

	rows := []Event{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT event_key, event_group, description, default_channels_json, variables_json
		FROM notification_events WHERE `+strings.Join(where, " AND ")+`
		ORDER BY event_group, event_key`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing notification events: %w", err)
	}
	return rows, nil
}

// DefaultChannels decodes the catalogue's channel list into API spellings,
// falling back to in-app plus email when the column is absent or malformed.
func (e Event) DefaultChannels() []string {
	fallback := []string{APIChannelInApp, APIChannelEmail}
	if !e.DefaultChannelsJSON.Valid {
		return fallback
	}
	var raw []string
	if err := json.Unmarshal([]byte(e.DefaultChannelsJSON.String), &raw); err != nil || len(raw) == 0 {
		return fallback
	}
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		out = append(out, apiChannel(strings.ToUpper(strings.TrimSpace(c))))
	}
	return out
}

// --- per-user preferences ---------------------------------------------------

func (r *Repository) Preferences(ctx context.Context, userID int64) ([]Preference, error) {
	rows := []Preference{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT event_key, channel, enabled, digest, muted_until
		FROM notification_preferences WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("loading notification preferences: %w", err)
	}
	return rows, nil
}

// PreferenceUpdate is one cell of the matrix the preferences screen renders.
type PreferenceUpdate struct {
	EventKey string
	Channel  string // stored spelling
	Enabled  bool
}

// SavePreferences replaces the caller's matrix, digest and mute in one
// transaction, so a half-applied save cannot leave the screen disagreeing with
// the database.
func (r *Repository) SavePreferences(ctx context.Context, tenantID, userID int64,
	updates []PreferenceUpdate, digest string, mutedUntil *time.Time, clearMute bool) error {

	tx, err := r.db.Primary.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("opening preferences transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, u := range updates {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO notification_preferences (tenant_id, user_id, event_key, channel, enabled, digest)
			VALUES (?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE enabled = VALUES(enabled)`,
			tenantID, userID, u.EventKey, u.Channel, u.Enabled, digest); err != nil {
			return fmt.Errorf("saving notification preference: %w", err)
		}
	}

	// Digest and mute are per-user, not per-event, but the table keys them per
	// row; writing them across every row of the user keeps a single read of any
	// row authoritative.
	if digest != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE notification_preferences SET digest = ? WHERE user_id = ?`, digest, userID); err != nil {
			return fmt.Errorf("saving digest preference: %w", err)
		}
	}
	switch {
	case clearMute:
		if _, err := tx.ExecContext(ctx,
			`UPDATE notification_preferences SET muted_until = NULL WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("clearing notification mute: %w", err)
		}
	case mutedUntil != nil:
		if _, err := tx.ExecContext(ctx,
			`UPDATE notification_preferences SET muted_until = ? WHERE user_id = ?`, *mutedUntil, userID); err != nil {
			return fmt.Errorf("saving notification mute: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing notification preferences: %w", err)
	}
	return nil
}

// EnabledChannels reports which channels this client has switched on, so the
// preference matrix can grey out the rest rather than offering a choice that
// would never be honoured.
func (r *Repository) EnabledChannels(ctx context.Context, tenantID int64) ([]string, error) {
	rows := []string{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT DISTINCT channel FROM tenant_notification_settings
		WHERE tenant_id = ? AND enabled = 1`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("loading enabled channels: %w", err)
	}
	// A client that has never touched the settings gets the working default
	// rather than nothing at all.
	if len(rows) == 0 {
		return []string{APIChannelInApp, APIChannelEmail}, nil
	}
	out := make([]string, 0, len(rows))
	for _, c := range rows {
		out = append(out, apiChannel(c))
	}
	return out, nil
}

// --- templates --------------------------------------------------------------

const templateColumns = `t.id, t.public_id, t.tenant_id, t.event_key, t.channel,
	t.subject, t.body_html, t.body_text, t.is_active, t.created_at, t.updated_at`

// Templates lists this client's wording, falling back to the platform default
// for any event the client has not overridden.
func (r *Repository) Templates(ctx context.Context, tenantID int64, eventKey, channel string) ([]Template, error) {
	where := []string{"(t.tenant_id = ? OR t.tenant_id IS NULL)"}
	args := []any{tenantID}
	if eventKey != "" {
		where = append(where, "t.event_key = ?")
		args = append(args, eventKey)
	}
	if channel != "" {
		where = append(where, "t.channel = ?")
		args = append(args, channel)
	}

	rows := []Template{}
	q := `SELECT ` + templateColumns + ` FROM notification_templates t WHERE ` +
		strings.Join(where, " AND ") +
		// A tenant override sorts before the platform default for the same
		// event/channel pair, so the caller reads the effective row first.
		` ORDER BY t.event_key, t.channel, t.tenant_id IS NULL`
	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("listing notification templates: %w", err)
	}
	return rows, nil
}

func (r *Repository) TemplateByPublicID(ctx context.Context, tenantID int64, publicID string) (*Template, error) {
	var t Template
	err := r.db.Primary.GetContext(ctx, &t,
		`SELECT `+templateColumns+` FROM notification_templates t
		 WHERE t.public_id = ? AND (t.tenant_id = ? OR t.tenant_id IS NULL)`, publicID, tenantID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading notification template: %w", err)
	}
	return &t, nil
}

// TemplateParams is a client's override of one event's wording.
type TemplateParams struct {
	EventKey string
	Channel  string
	Subject  string
	BodyHTML string
	BodyText string
	IsActive bool
	SavedBy  *int64
}

// SaveTemplate upserts the client's override. The platform default is never
// modified: editing a default creates a tenant row that shadows it, so a client
// can always be reset by deleting their override.
func (r *Repository) SaveTemplate(ctx context.Context, tenantID int64, p TemplateParams) (*Template, error) {
	_, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO notification_templates
			(public_id, tenant_id, event_key, channel, subject, body_html, body_text, is_active, updated_by)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			subject = VALUES(subject), body_html = VALUES(body_html),
			body_text = VALUES(body_text), is_active = VALUES(is_active),
			updated_by = VALUES(updated_by)`,
		platform.NewULID(), tenantID, p.EventKey, p.Channel,
		nullIfEmpty(p.Subject), nullIfEmpty(p.BodyHTML), nullIfEmpty(p.BodyText),
		p.IsActive, p.SavedBy)
	if err != nil {
		return nil, fmt.Errorf("saving notification template: %w", err)
	}

	var t Template
	err = r.db.Primary.GetContext(ctx, &t,
		`SELECT `+templateColumns+` FROM notification_templates t
		 WHERE t.tenant_id = ? AND t.event_key = ? AND t.channel = ?`,
		tenantID, p.EventKey, p.Channel)
	if err != nil {
		return nil, fmt.Errorf("reloading notification template: %w", err)
	}
	return &t, nil
}

// Render substitutes the sample variables into a template, for the preview and
// test-send buttons. It reuses the worker's renderer so a preview cannot
// disagree with what is actually delivered.
func Render(tmpl string, vars map[string]any) string { return render(tmpl, vars) }

// WriteTest delivers a rendered sample to one person's own in-app feed.
//
// Marked SENT because it genuinely was: it goes through the same table the bell
// reads, which is the point of a test.
func (r *Repository) WriteTest(ctx context.Context, tenantID, userID int64, eventKey, title, body string) error {
	_, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO notifications
			(public_id, tenant_id, user_id, event_key, channel, title, body, status, sent_at)
		VALUES (?,?,?,?, 'IN_APP', ?, ?, 'SENT', UTC_TIMESTAMP(3))`,
		platform.NewULID(), tenantID, userID, eventKey, title, nullIfEmpty(body))
	if err != nil {
		return fmt.Errorf("writing test notification: %w", err)
	}
	return nil
}
