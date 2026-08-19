package notification

import (
	"database/sql"
	"time"
)

// Notification is one delivered (or queued) message addressed to one person.
//
// Rows are written by the outbox worker only. Nothing in the read API creates
// them, which is what keeps the feed honest: if a row exists, something
// genuinely happened.
type Notification struct {
	ID         int64          `db:"id"`
	PublicID   string         `db:"public_id"`
	TenantID   int64          `db:"tenant_id"`
	UserID     int64          `db:"user_id"`
	EventKey   string         `db:"event_key"`
	Channel    string         `db:"channel"`
	Title      string         `db:"title"`
	Body       sql.NullString `db:"body"`
	Link       sql.NullString `db:"link"`
	EntityType sql.NullString `db:"entity_type"`
	EntityID   sql.NullInt64  `db:"entity_id"`
	Status     string         `db:"status"`
	ReadAt     sql.NullTime   `db:"read_at"`
	CreatedAt  time.Time      `db:"created_at"`

	// Joined context, so the feed can say who it was for and which client it
	// belongs to without a second request per row.
	RecipientName sql.NullString `db:"recipient_name"`
	ClientName    sql.NullString `db:"client_name"`
	ClientSlug    sql.NullString `db:"client_slug"`
	EventGroup    sql.NullString `db:"event_group"`
	// TargetPublicID is the subject the link points at, resolved at read time.
	// Null when the subject no longer exists, which is how a stale link is
	// recognised before it is offered to anyone.
	TargetPublicID sql.NullString `db:"target_public_id"`
}

// Event is a row of the notification catalogue — the fixed list of things the
// product can tell someone about, and the channels each may use.
type Event struct {
	Key                 string         `db:"event_key"`
	Group               string         `db:"event_group"`
	Description         string         `db:"description"`
	DefaultChannelsJSON sql.NullString `db:"default_channels_json"`
	VariablesJSON       sql.NullString `db:"variables_json"`
}

// Preference is one user's choice for one event on one channel.
type Preference struct {
	EventKey   string       `db:"event_key"`
	Channel    string       `db:"channel"`
	Enabled    bool         `db:"enabled"`
	Digest     string       `db:"digest"`
	MutedUntil sql.NullTime `db:"muted_until"`
}

// Template is the wording for one event on one channel. A row with a NULL
// tenant is the platform default; a tenant row overrides it.
type Template struct {
	ID        int64          `db:"id"`
	PublicID  string         `db:"public_id"`
	TenantID  sql.NullInt64  `db:"tenant_id"`
	EventKey  string         `db:"event_key"`
	Channel   string         `db:"channel"`
	Subject   sql.NullString `db:"subject"`
	BodyHTML  sql.NullString `db:"body_html"`
	BodyText  sql.NullString `db:"body_text"`
	IsActive  bool           `db:"is_active"`
	CreatedAt time.Time      `db:"created_at"`
	UpdatedAt time.Time      `db:"updated_at"`
}

// Channel keys as the API speaks them. The database stores them upper-case; the
// browser has always used lower-case, so the handler translates at the edge
// rather than migrating a column that several releases already depend on.
const (
	APIChannelInApp = "in_app"
	APIChannelEmail = "email"
	APIChannelSMS   = "sms"
	APIChannelPush  = "push"
)

// apiChannel maps a stored channel onto its API spelling.
func apiChannel(stored string) string {
	switch stored {
	case ChannelInApp:
		return APIChannelInApp
	case ChannelEmail:
		return APIChannelEmail
	case ChannelSMS:
		return APIChannelSMS
	case "PUSH":
		return APIChannelPush
	default:
		return APIChannelInApp
	}
}

// storedChannel maps an API channel back onto the stored spelling. The second
// return is false for anything unrecognised, so an unknown channel is rejected
// rather than silently written.
func storedChannel(api string) (string, bool) {
	switch api {
	case APIChannelInApp:
		return ChannelInApp, true
	case APIChannelEmail:
		return ChannelEmail, true
	case APIChannelSMS:
		return ChannelSMS, true
	case APIChannelPush:
		return "PUSH", true
	default:
		return "", false
	}
}

// APIChannels is the full channel list the preference matrix renders.
var APIChannels = []string{APIChannelInApp, APIChannelEmail, APIChannelSMS, APIChannelPush}
