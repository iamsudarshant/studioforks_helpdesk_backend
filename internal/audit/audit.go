// Package audit writes the append-only audit trail and the per-user activity
// stream. Both are written asynchronously through a buffered writer so that
// recording an event never adds latency to the request that caused it.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/appctx"
)

// Action keys. Kept as constants so the audit explorer can offer a fixed filter
// list rather than a free-text guess.
const (
	ActionLogin             = "auth.login"
	ActionLoginFailed       = "auth.login_failed"
	ActionLogout            = "auth.logout"
	ActionTokenRefresh      = "auth.token_refresh"
	ActionTokenReuse        = "auth.token_reuse_detected"
	ActionPasswordChanged   = "auth.password_changed"
	ActionPasswordReset     = "auth.password_reset"
	ActionResetLinkSent     = "auth.reset_link_sent"
	ActionUsernameRecovered = "auth.username_recovered"
	ActionMFAEnrolled       = "auth.mfa_enrolled"
	ActionMFADisabled       = "auth.mfa_disabled"
	ActionSessionRevoked    = "auth.session_revoked"
	ActionAccountLocked     = "auth.account_locked"

	ActionUserCreated     = "user.created"
	ActionUserUpdated     = "user.updated"
	ActionUserDeleted     = "user.deleted"
	ActionUserActivated   = "user.activated"
	ActionUserDeactivated = "user.deactivated"
	ActionUserRolesSet    = "user.roles_set"
	ActionUserScopesSet   = "user.scopes_set"
	ActionUserGroupMoved  = "user.group_moved"
	// One employee crossing between Active and Ex-Employee, as opposed to the
	// batch the group-move records.
	ActionUserEmploymentChanged = "user.employment_changed"
	ActionUserBulkImport        = "user.bulk_import"
	ActionUserPIIRevealed       = "user.pii_revealed"
	ActionUserAnonymised        = "user.anonymised"

	ActionTicketCreated   = "ticket.created"
	ActionTicketUpdated   = "ticket.updated"
	ActionTicketStatus    = "ticket.status_changed"
	ActionTicketAssigned  = "ticket.assigned"
	ActionTicketTransfer  = "ticket.transferred"
	ActionTicketEscalated = "ticket.escalated"
	ActionTicketReplied   = "ticket.replied"
	ActionTicketNote      = "ticket.internal_note"
	ActionTicketClosed    = "ticket.closed"
	ActionTicketReopened  = "ticket.reopened"
	ActionTicketBulk      = "ticket.bulk_action"

	ActionDocumentUploaded   = "document.uploaded"
	ActionDocumentDownloaded = "document.downloaded"
	ActionDocumentViewed     = "document.viewed"
	ActionDocumentDeleted    = "document.deleted"

	ActionConfigChanged    = "config.changed"
	ActionTenantCreated    = "tenant.created"
	ActionTenantUpdated    = "tenant.updated"
	ActionTenantSuspended  = "tenant.suspended"
	ActionTenantPrefixSet  = "tenant.prefix_changed"
	ActionMaintenanceSet   = "tenant.maintenance_set"
	ActionEntityAssignment = "entity.assigned"
	ActionEntityReplyGrant = "entity.reply_granted"
	ActionSiteAssignment   = "site.assigned"
	ActionDeptAssignment   = "department.assigned"
	ActionAPIKeyCreated    = "apikey.created"
	ActionAPIKeyRevoked    = "apikey.revoked"
	ActionReportGenerated  = "report.generated"
	ActionReportExported   = "report.exported"
)

// Entry is one audit record.
type Entry struct {
	TenantID       *int64
	ActorID        *int64
	ActorRole      string
	ActorName      string
	Action         string
	EntityType     string
	EntityID       *int64
	EntityPublicID string
	Before         any
	After          any
	IP             string
	UserAgent      string
	Portal         string
	RequestID      string
	CrossTenant    bool
	CreatedAt      time.Time
}

// Activity is one user-activity record powering the per-user trail.
type Activity struct {
	TenantID      int64
	UserID        int64
	Action        string
	ResourceType  string
	ResourceID    *int64
	ResourceLabel string
	Portal        string
	IP            string
	Meta          any
	DurationMS    *int
	CreatedAt     time.Time
}

// Writer batches audit and activity rows onto a background goroutine.
type Writer struct {
	db       *sqlx.DB
	audits   chan Entry
	activity chan Activity
	wg       sync.WaitGroup
	stop     chan struct{}
	once     sync.Once

	// hashMu guards the per-tenant tail hash used for the tamper-evident chain.
	hashMu   sync.Mutex
	lastHash map[int64]string
}

func NewWriter(db *sqlx.DB) *Writer {
	return &Writer{
		db:       db,
		audits:   make(chan Entry, 2048),
		activity: make(chan Activity, 4096),
		stop:     make(chan struct{}),
		lastHash: map[int64]string{},
	}
}

// Start launches the flush loop. Batches are written every 500ms or when they
// reach 100 rows, whichever comes first.
func (w *Writer) Start() {
	w.wg.Add(1)
	go w.loop()
}

// Stop drains and flushes before returning, so a graceful shutdown does not
// lose the last few seconds of audit history.
func (w *Writer) Stop(ctx context.Context) {
	w.once.Do(func() { close(w.stop) })

	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("audit writer did not drain before shutdown deadline")
	}
}

func (w *Writer) loop() {
	defer w.wg.Done()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	auditBatch := make([]Entry, 0, 100)
	activityBatch := make([]Activity, 0, 200)

	flush := func() {
		if len(auditBatch) > 0 {
			w.flushAudits(auditBatch)
			auditBatch = auditBatch[:0]
		}
		if len(activityBatch) > 0 {
			w.flushActivity(activityBatch)
			activityBatch = activityBatch[:0]
		}
	}

	for {
		select {
		case e := <-w.audits:
			auditBatch = append(auditBatch, e)
			if len(auditBatch) >= 100 {
				flush()
			}

		case a := <-w.activity:
			activityBatch = append(activityBatch, a)
			if len(activityBatch) >= 200 {
				flush()
			}

		case <-ticker.C:
			flush()

		case <-w.stop:
			// Drain whatever is still queued before exiting.
			for {
				select {
				case e := <-w.audits:
					auditBatch = append(auditBatch, e)
				case a := <-w.activity:
					activityBatch = append(activityBatch, a)
				default:
					flush()
					return
				}
			}
		}
	}
}

// Record queues an audit entry, enriching it from the request context.
func (w *Writer) Record(ctx context.Context, e Entry) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.RequestID == "" {
		e.RequestID = appctx.RequestID(ctx)
	}
	if e.IP == "" {
		e.IP = appctx.ClientIP(ctx)
	}
	if e.UserAgent == "" {
		e.UserAgent = appctx.UserAgent(ctx)
	}
	if actor := appctx.ActorFrom(ctx); actor != nil {
		if e.ActorID == nil {
			id := actor.UserID
			e.ActorID = &id
		}
		if e.ActorName == "" {
			e.ActorName = actor.FullName
		}
		if e.ActorRole == "" && len(actor.Roles) > 0 {
			e.ActorRole = actor.Roles[0]
		}
		if e.Portal == "" {
			e.Portal = string(actor.Portal)
		}
		if e.TenantID == nil && actor.TenantID != 0 {
			tid := actor.TenantID
			e.TenantID = &tid
		}
	}
	if e.TenantID == nil {
		if t := appctx.TenantFrom(ctx); t != nil {
			tid := t.ID
			e.TenantID = &tid
		}
	}
	// A cross-tenant write by a super admin is flagged explicitly.
	if actor := appctx.ActorFrom(ctx); actor != nil && actor.IsSuperAdmin && e.TenantID != nil {
		if t := appctx.TenantFrom(ctx); t == nil || t.ID != *e.TenantID {
			e.CrossTenant = true
		}
	}

	select {
	case w.audits <- e:
	default:
		// Never block the request path. A dropped row is logged loudly so the
		// gap is visible during an investigation.
		slog.ErrorContext(ctx, "audit buffer full, entry dropped",
			"action", e.Action, "request_id", e.RequestID)
	}
}

// Track queues a user-activity row.
func (w *Writer) Track(ctx context.Context, a Activity) {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	if a.TenantID == 0 {
		a.TenantID = appctx.TenantID(ctx)
	}
	if a.UserID == 0 {
		a.UserID = appctx.ActorID(ctx)
	}
	if a.Portal == "" {
		a.Portal = string(appctx.PortalFrom(ctx))
	}
	if a.IP == "" {
		a.IP = appctx.ClientIP(ctx)
	}
	if a.TenantID == 0 || a.UserID == 0 {
		return // anonymous traffic has no user trail to attach to
	}

	select {
	case w.activity <- a:
	default:
		slog.WarnContext(ctx, "activity buffer full, entry dropped", "action", a.Action)
	}
}

func (w *Writer) flushAudits(batch []Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const q = `
		INSERT INTO audit_logs
			(tenant_id, actor_id, actor_role, actor_name_snapshot, action, entity_type,
			 entity_id, entity_public_id, before_json, after_json, ip, user_agent,
			 portal, request_id, cross_tenant, prev_hash, row_hash, created_at)
		VALUES
			(:tenant_id, :actor_id, :actor_role, :actor_name, :action, :entity_type,
			 :entity_id, :entity_public_id, :before_json, :after_json, :ip, :user_agent,
			 :portal, :request_id, :cross_tenant, :prev_hash, :row_hash, :created_at)`

	rows := make([]map[string]any, 0, len(batch))
	for _, e := range batch {
		before := toJSON(e.Before)
		after := toJSON(e.After)

		tenantKey := int64(0)
		if e.TenantID != nil {
			tenantKey = *e.TenantID
		}
		prev, row := w.chain(tenantKey, e, before, after)

		rows = append(rows, map[string]any{
			"tenant_id":        e.TenantID,
			"actor_id":         e.ActorID,
			"actor_role":       nullIfEmpty(e.ActorRole),
			"actor_name":       nullIfEmpty(e.ActorName),
			"action":           e.Action,
			"entity_type":      nullIfEmpty(e.EntityType),
			"entity_id":        e.EntityID,
			"entity_public_id": nullIfEmpty(e.EntityPublicID),
			"before_json":      before,
			"after_json":       after,
			"ip":               nullIfEmpty(e.IP),
			"user_agent":       nullIfEmpty(e.UserAgent),
			"portal":           nullIfEmpty(e.Portal),
			"request_id":       nullIfEmpty(e.RequestID),
			"cross_tenant":     e.CrossTenant,
			"prev_hash":        nullIfEmpty(prev),
			"row_hash":         row,
			"created_at":       e.CreatedAt,
		})
	}

	if _, err := w.db.NamedExecContext(ctx, q, rows); err != nil {
		slog.Error("writing audit batch", "error", err, "rows", len(rows))
	}
}

// chain computes the tamper-evidence hash linking this row to the previous one
// for the same tenant.
func (w *Writer) chain(tenantID int64, e Entry, before, after any) (prev, row string) {
	w.hashMu.Lock()
	defer w.hashMu.Unlock()

	prev = w.lastHash[tenantID]

	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%v|%s|%s|%v|%s|%v|%v|%s",
		prev, tenantID, e.ActorID, e.Action, e.EntityType, e.EntityID,
		e.CreatedAt.Format(time.RFC3339Nano), before, after, e.RequestID)

	row = hex.EncodeToString(h.Sum(nil))
	w.lastHash[tenantID] = row
	return prev, row
}

func (w *Writer) flushActivity(batch []Activity) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const q = `
		INSERT INTO user_activity
			(tenant_id, user_id, action, resource_type, resource_id, resource_label,
			 portal, ip, meta_json, duration_ms, created_at)
		VALUES
			(:tenant_id, :user_id, :action, :resource_type, :resource_id, :resource_label,
			 :portal, :ip, :meta_json, :duration_ms, :created_at)`

	rows := make([]map[string]any, 0, len(batch))
	for _, a := range batch {
		rows = append(rows, map[string]any{
			"tenant_id":      a.TenantID,
			"user_id":        a.UserID,
			"action":         a.Action,
			"resource_type":  nullIfEmpty(a.ResourceType),
			"resource_id":    a.ResourceID,
			"resource_label": nullIfEmpty(a.ResourceLabel),
			"portal":         nullIfEmpty(a.Portal),
			"ip":             nullIfEmpty(a.IP),
			"meta_json":      toJSON(a.Meta),
			"duration_ms":    a.DurationMS,
			"created_at":     a.CreatedAt,
		})
	}

	if _, err := w.db.NamedExecContext(ctx, q, rows); err != nil {
		slog.Error("writing activity batch", "error", err, "rows", len(rows))
	}
}

// RecordLogin writes a login attempt synchronously — the security value of this
// row is high enough to justify the round trip, and it must survive a crash.
func RecordLogin(ctx context.Context, db *sqlx.DB, tenantID, userID *int64, portal, identifier, result string, sessionID *int64) {
	const q = `
		INSERT INTO login_activity
			(tenant_id, user_id, portal, identifier_used, result, ip, user_agent, session_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

	// The identifier is stored to support "which account were they trying?"
	// investigations; it is masked so a mistyped password never lands here.
	_, err := db.ExecContext(ctx, q,
		tenantID, userID, nullIfEmpty(portal), nullIfEmpty(maskIdentifier(identifier)),
		result, nullIfEmpty(appctx.ClientIP(ctx)), nullIfEmpty(appctx.UserAgent(ctx)), sessionID)
	if err != nil {
		slog.ErrorContext(ctx, "recording login activity", "error", err, "result", result)
	}
}

// maskIdentifier keeps enough of the value to be recognisable without storing
// a full PAN/UAN/PF number in a widely-read table.
func maskIdentifier(v string) string {
	if len(v) <= 4 {
		return v
	}
	if at := indexByte(v, '@'); at > 0 {
		local := v[:at]
		if len(local) <= 2 {
			return local + "***" + v[at:]
		}
		return local[:2] + "***" + v[at:]
	}
	return v[:2] + "****" + v[len(v)-2:]
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func toJSON(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Ptr is a small helper for building Entry values inline.
func Ptr[T any](v T) *T { return &v }
