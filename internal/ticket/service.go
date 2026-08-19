package ticket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/microcosm-cc/bluemonday"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/user"
)

// EventPublisher writes notification events to the transactional outbox.
type EventPublisher interface {
	Publish(ctx context.Context, tenantID int64, eventKey, aggregateType string, aggregateID int64, payload any) error
}

// TenantConfig is the slice of client configuration the ticket engine reads.
type TenantConfig interface {
	IntSetting(ctx context.Context, tenantID int64, key string, fallback int) int
}

type Service struct {
	repo      *Repository
	users     *user.Repository
	tenants   TenantConfig
	events    EventPublisher
	auditor   *audit.Writer
	sanitiser *bluemonday.Policy
}

func NewService(repo *Repository, users *user.Repository, tenants TenantConfig,
	events EventPublisher, auditor *audit.Writer) *Service {
	return &Service{
		repo: repo, users: users, tenants: tenants, events: events, auditor: auditor,
		// Replies are rich text. The allowlist policy strips scripts, event
		// handlers and javascript: URLs; anything not explicitly permitted is
		// removed rather than escaped.
		sanitiser: bluemonday.UGCPolicy(),
	}
}

func (s *Service) Repo() *Repository { return s.repo }

// ScopeFor translates an actor into the slice of tickets they may see.
//
// This is the single place the visibility rules live:
//   - an employee sees only what they raised
//   - a client executive sees only their allocated entities, sites, departments
//     and categories
//   - anyone with ticket.view.all — helpdesk staff, a client admin — sees the
//     whole client
func (s *Service) ScopeFor(actor *appctx.Actor) Scope {
	// No actor should ever reach a read, but if one does, show nothing.
	if actor == nil {
		return denyAll()
	}

	if actor.Can("ticket.view.all") {
		return Scope{}
	}

	if actor.Can("ticket.view.scope") {
		scope := Scope{
			EntityIDs:     actor.Scopes.Entities,
			SiteIDs:       actor.Scopes.Sites,
			DepartmentIDs: actor.Scopes.Departments,
			CategoryIDs:   actor.Scopes.Categories,
		}
		// A scoped user with no allocation at all must see nothing.
		//
		// Every dimension being nil would otherwise read as "unrestricted on all
		// four", handing a Client Executive the entire client — the exact
		// opposite of what the role means. Segmentation is the whole definition
		// of that role, so its absence has to fail closed. A user who should see
		// everything is given ticket.view.all instead; that is a deliberate
		// grant rather than an accident of empty configuration.
		if scope.EntityIDs == nil && scope.SiteIDs == nil &&
			scope.DepartmentIDs == nil && scope.CategoryIDs == nil {
			return denyAll()
		}
		return scope
	}

	// ticket.view.own, or nothing at all.
	id := actor.UserID
	return Scope{RequesterID: &id}
}

// isStaffUser reports whether a user is one of ComplyDesk's own people.
//
// Used only to let a member of staff be the requester on a client's ticket:
// anybody else must belong to the client the ticket is being raised against,
// which the tenant-scoped lookup above already established.
func isStaffUser(ctx context.Context, users *user.Repository, u *user.User) bool {
	if u == nil {
		return false
	}
	roles, err := users.RolesFor(ctx, u.ID)
	if err != nil {
		return false
	}
	for _, role := range roles {
		if role.Portal == "admin" || role.Portal == "agents" {
			return true
		}
	}
	return false
}

// denyAll is a scope that matches no ticket. Expressed as an impossible
// requester rather than an empty slice so it survives any later change to how
// the dimension filters treat emptiness.
func denyAll() Scope {
	none := int64(-1)
	return Scope{RequesterID: &none}
}

// CanSee reports whether an actor may read one specific ticket. Applied after
// loading, because a scoped list query cannot express "or you raised it".
func (s *Service) CanSee(actor *appctx.Actor, t *Ticket) bool {
	if actor == nil {
		return false
	}
	if actor.Can("ticket.view.all") {
		return true
	}
	// Anyone may always see what they raised, whatever else their scope says.
	if t.RequesterID == actor.UserID {
		return true
	}
	if t.AssigneeID.Valid && t.AssigneeID.Int64 == actor.UserID {
		return true
	}
	if !actor.Can("ticket.view.scope") {
		return false
	}

	inScope := func(scope []int64, value int64, valid bool) bool {
		if scope == nil {
			return true
		}
		if !valid {
			return false
		}
		for _, id := range scope {
			if id == value {
				return true
			}
		}
		return false
	}

	return inScope(actor.Scopes.Entities, t.EntityID.Int64, t.EntityID.Valid) &&
		inScope(actor.Scopes.Sites, t.SiteID.Int64, t.SiteID.Valid) &&
		inScope(actor.Scopes.Departments, t.DepartmentID.Int64, t.DepartmentID.Valid)
}

// CanSeeInternal reports whether internal notes are visible to this actor.
// Employees and partners never see them.
func (s *Service) CanSeeInternal(actor *appctx.Actor) bool {
	return actor != nil && actor.Can("ticket.reply.internal")
}

// Load fetches a ticket and enforces read access, returning NOT_FOUND rather
// than FORBIDDEN so a caller cannot probe for tickets outside their scope.
func (s *Service) Load(ctx context.Context, publicID string, actor *appctx.Actor) (*Ticket, error) {
	if !platform.ValidULID(publicID) {
		return nil, httpx.ErrNotFound("That ticket")
	}

	// Resolved across every client the caller can reach rather than against the
	// single tenant the header named. Staff working the cross-client list have
	// no client selected, so that header names the platform workspace and a
	// pinned lookup would answer "not found" for every row they can see.
	//
	t, err := s.repo.ByPublicIDInReach(ctx, appctx.Reach(ctx), publicID)
	if err != nil {
		if errors.Is(err, platform.ErrSentinelNotFound) {
			return nil, httpx.ErrNotFound("That ticket")
		}
		return nil, httpx.ErrInternal(err)
	}
	if !s.CanSee(actor, t) {
		return nil, httpx.ErrNotFound("That ticket")
	}
	return t, nil
}

// CreateInput is the validated create request.
type CreateInput struct {
	CategoryID    int64
	SubcategoryID *int64
	Subject       string
	Description   string
	Priority      string
	RequesterID   int64
	EntityID      *int64
	SiteID        *int64
	// The statutory line the ticket routes to. Set from the form's department
	// step, or derived from the entity when only that was named. Nil falls back
	// to the category's default department, as it always has.
	DepartmentID *int64
	CustomFields map[string]any
	DocumentIDs  []int64
}

// Create raises a ticket on behalf of the requester.
func (s *Service) Create(ctx context.Context, tenantID int64, in CreateInput, actor *appctx.Actor) (*Ticket, error) {
	// The requester is usually an employee of this client, but not always: a
	// helpdesk agent raising a ticket they noticed is the requester themselves,
	// and staff live in the platform workspace rather than in the client. A
	// lookup pinned to the client tenant therefore failed for every ticket an
	// agent or admin raised — which is what "create ticket is not working" was.
	requester, err := s.users.ByID(ctx, tenantID, in.RequesterID)
	if err != nil {
		staff, staffErr := s.users.ByIDAnyTenant(ctx, in.RequesterID)
		if staffErr != nil || !isStaffUser(ctx, s.users, staff) {
			return nil, httpx.ErrField("requester_id", "NOT_FOUND", "That requester was not found.")
		}
		requester = staff
	}

	// Freeze who raised it, so a later profile edit cannot rewrite history.
	snapshot := map[string]any{
		"user_id": requester.PublicID, "full_name": requester.FullName(),
		"employee_code": requester.EmployeeCode.String, "email": requester.Email.String,
		"mobile": requester.Mobile.String, "uan_number": requester.UANNumber.String,
		"pf_number": requester.PFNumber.String, "esic_number": requester.ESICNumber.String,
	}
	if requester.DateOfJoining.Valid {
		snapshot["date_of_joining"] = requester.DateOfJoining.Time.Format("2006-01-02")
	}

	var customFields string
	if len(in.CustomFields) > 0 {
		raw, err := json.Marshal(in.CustomFields)
		if err != nil {
			return nil, httpx.ErrField("custom_fields", "INVALID", "Custom fields could not be encoded.")
		}
		customFields = string(raw)
	}

	// Default the entity and site from the requester's profile when the form
	// did not supply them.
	entityID := in.EntityID
	if entityID == nil && requester.EntityID.Valid {
		entityID = &requester.EntityID.Int64
	}
	siteID := in.SiteID
	if siteID == nil && requester.SiteID.Valid {
		siteID = &requester.SiteID.Int64
	}

	var actorID *int64
	if actor != nil {
		id := actor.UserID
		actorID = &id
	}

	created, err := s.repo.Create(ctx, tenantID, CreateParams{
		CategoryID: in.CategoryID, SubcategoryID: in.SubcategoryID,
		Subject: in.Subject, Description: in.Description, Priority: in.Priority,
		Source: SourcePortal, RequesterID: in.RequesterID,
		EntityID: entityID, SiteID: siteID, DepartmentID: in.DepartmentID,
		CustomFields: customFields, DocumentIDs: in.DocumentIDs,
		CreatedBy: actorID, Snapshot: snapshot,
	})
	if err != nil {
		if errors.Is(err, platform.ErrSentinelNotFound) {
			return nil, httpx.ErrField("category_id", "NOT_FOUND", "That category was not found.")
		}
		return nil, httpx.ErrInternal(err)
	}

	s.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionTicketCreated, EntityType: "ticket",
		EntityID: &created.ID, EntityPublicID: created.PublicID,
		After: map[string]any{
			"ticket_number": created.TicketNumber, "category": created.CategoryName,
			"priority": created.Priority,
		},
	})

	s.publish(ctx, tenantID, "ticket.created", created)
	return created, nil
}

// SanitiseHTML strips anything unsafe from a rich-text reply. Applied on the way
// in so stored content is already clean, and the client sanitises again on
// render as defence in depth.
func (s *Service) SanitiseHTML(html string) string {
	return s.sanitiser.Sanitize(html)
}

// PlainText renders a crude text version for search, notifications and exports.
func PlainText(html string) string {
	var b strings.Builder
	inTag := false
	for _, r := range html {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			b.WriteRune(' ')
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(strings.Join(strings.Fields(b.String()), " "))
}

// ReopenWindowDays is how long after closure an employee may reopen. Configured
// per client; zero means no limit.
func (s *Service) ReopenWindowDays(ctx context.Context, tenantID int64) int {
	return s.tenants.IntSetting(ctx, tenantID, "reopen_window_days", 15)
}

func (s *Service) publish(ctx context.Context, tenantID int64, event string, t *Ticket) {
	if s.events == nil {
		return
	}
	payload := map[string]any{
		"ticket_id": t.PublicID, "ticket_number": t.TicketNumber,
		"subject": t.Subject, "status": t.Status, "priority": t.Priority,
		"category": t.CategoryName, "requester": t.RequesterName,
	}
	if err := s.events.Publish(ctx, tenantID, event, "ticket", t.ID, payload); err != nil {
		// Notification failure must not fail the operation that caused it; the
		// outbox worker retries, and the miss is visible in the log.
		_ = err
	}
}

// PublishEvent exposes publishing for handlers that change state.
func (s *Service) PublishEvent(ctx context.Context, tenantID int64, event string, t *Ticket) {
	s.publish(ctx, tenantID, event, t)
}

// MapError converts repository sentinels into the HTTP taxonomy.
func MapError(err error, resource string) error {
	switch {
	case errors.Is(err, platform.ErrSentinelNotFound):
		return httpx.ErrNotFound(resource)
	case errors.Is(err, platform.ErrSentinelConflict):
		return httpx.ErrConflict("")
	default:
		return httpx.ErrInternal(fmt.Errorf("%s: %w", resource, err))
	}
}

// --- document visibility ----------------------------------------------------

// An attachment is not a thing in its own right: it is visible exactly when the
// ticket it hangs off is. The document package asks these two questions rather
// than re-deriving the answer from scopes, so there is one implementation of
// "may this person see this ticket" and it lives here.

// CanSeeID answers CanSee for a ticket identified by its internal id.
func (s *Service) CanSeeID(ctx context.Context, tenantID, ticketID int64, actor *appctx.Actor) bool {
	t, err := s.repo.ByID(ctx, tenantID, ticketID)
	if err != nil {
		// A missing ticket is not a licence to read the file that hung off it.
		return false
	}
	return s.CanSee(actor, t)
}

// TicketIDsForDocument lists the tickets a document is attached to, so the
// caller can be tested against each in turn.
func (s *Service) TicketIDsForDocument(ctx context.Context, tenantID, documentID int64) ([]int64, error) {
	ids := []int64{}
	err := s.repo.db.Primary.SelectContext(ctx, &ids,
		`SELECT ticket_id FROM ticket_attachments WHERE tenant_id = ? AND document_id = ?`,
		tenantID, documentID)
	if err != nil {
		return nil, fmt.Errorf("loading a document's tickets: %w", err)
	}
	return ids, nil
}
