// Package appctx carries request-scoped values (request id, tenant, actor)
// through context.Context with typed accessors. Handlers and repositories read
// tenancy and identity from here — never from headers directly — so that the
// tenant choke point in the data layer cannot be bypassed.
package appctx

import (
	"context"
	"time"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyTenant
	keyActor
	keyPortal
	keyClientIP
	keyUserAgent
	keySignedAccess
)

// Portal identifies which of the four browser portals a request came from.
type Portal string

const (
	PortalAdmin   Portal = "admin"
	PortalAgents  Portal = "agents"
	PortalPartner Portal = "partner"
	PortalUser    Portal = "user"
)

func (p Portal) Valid() bool {
	switch p {
	case PortalAdmin, PortalAgents, PortalPartner, PortalUser:
		return true
	}
	return false
}

// Tenant is the resolved tenant for the current request.
type Tenant struct {
	ID       int64
	PublicID string
	Slug     string
	Name     string
	Status   string
	Timezone string
	Locale   string
	Prefix   string

	// Inferred marks a tenant the caller never actually named — in practice the
	// development default-slug fallback. Routes that must behave the same in dev
	// as in production (where the fallback is forbidden) ignore it, so a Karma
	// agent listing their clients is not silently judged against a workspace
	// they never asked for.
	Inferred bool
}

// AccessMode mirrors user_groups.access_mode.
type AccessMode string

const (
	AccessFull     AccessMode = "FULL"
	AccessReadOnly AccessMode = "READ_ONLY"
	AccessNone     AccessMode = "NO_ACCESS"
)

// Scopes holds the entity/site/department/category ids a scoped user may reach.
// A nil slice means "not scoped on this dimension"; an empty (non-nil) slice
// means "explicitly scoped to nothing" and must deny access.
type Scopes struct {
	Entities    []int64
	Sites       []int64
	Departments []int64
	Categories  []int64
}

// Empty reports whether any dimension is explicitly scoped to nothing.
func (s Scopes) Empty() bool {
	return (s.Entities != nil && len(s.Entities) == 0) ||
		(s.Sites != nil && len(s.Sites) == 0) ||
		(s.Departments != nil && len(s.Departments) == 0)
}

// Actor is the authenticated user behind the current request.
type Actor struct {
	UserID             int64
	PublicID           string
	TenantID           int64
	Portal             Portal
	Email              string
	FullName           string
	Roles              []string
	Permissions        map[string]struct{}
	Scopes             Scopes
	SessionID          int64
	MustChangePassword bool
	GroupKey           string
	GroupAccessMode    AccessMode
	AccessExpiresAt    *time.Time
	IsSuperAdmin       bool

	// IsStaff marks a ComplyDesk employee — a super admin or an agent. Their home
	// tenant is the platform tenant and they work across clients rather than
	// inside one.
	IsStaff bool
	// AssignedTenantIDs are the clients this agent owns.
	//
	// These express responsibility, not permission: an agent may work any client,
	// and this drives routing, the "my clients" filter and reporting. It is
	// deliberately NOT consulted by MayAccessTenant — see the note there.
	AssignedTenantIDs []int64
	// ActiveTenantID is the client the request is operating on, which for staff
	// differs from their home tenant.
	ActiveTenantID int64
}

// MayAccessTenant reports whether the actor may operate on a client.
//
// This is the single choke point behind "no cross-tenant visibility", and the
// rule is deliberately asymmetric:
//
//   - A client-side user — partner or employee — never leaves their own
//     workspace. This is the boundary that matters: it is what stops one client
//     seeing another's employees, tickets and documents.
//   - ComplyDesk's own staff work across clients. An agent resolves tickets for
//     any client and can create and administer clients, which is impossible if
//     they cannot see the ones they were not assigned.
//
// Agent assignments still exist, but as ownership rather than permission; see
// Actor.AssignedTenantIDs. If the product ever needs agents confined again,
// this function is the only place that changes.
func (a *Actor) MayAccessTenant(tenantID int64) bool {
	if a == nil || tenantID == 0 {
		return false
	}
	if a.IsSuperAdmin || a.IsStaff {
		return true
	}
	// A client-side user never leaves their own tenant.
	if a.TenantID == tenantID {
		return true
	}
	return false
}

// ClientReach describes which clients one request covers.
//
// A zero value reaches nothing. Exactly one of All or TenantIDs is meaningful:
// All lifts the client restriction entirely, TenantIDs names the clients.
type ClientReach struct {
	All       bool
	TenantIDs []int64
}

// OneClient is the reach of a request that is pinned to a single client — used
// where the client is already known from something other than the session, such
// as the id in the path.
func OneClient(tenantID int64) ClientReach {
	if tenantID == 0 {
		return ClientReach{}
	}
	return ClientReach{TenantIDs: []int64{tenantID}}
}

// Reach answers "which clients does this request cover?".
//
// This is the single rule behind the brief's "if no client is selected, show
// everything". ComplyDesk's own staff belong to the platform workspace rather
// than to any client, so until they pick one from the switcher every request
// still resolves to their own home tenant. That state — resolved tenant equals
// the actor's own — is what "no client selected" means on the wire, and the
// honest answer to it is every client rather than the near-empty platform
// workspace the header happens to name.
//
// An agent who owns specific clients gets those; an agent who owns none covers
// the whole desk and gets all of them. A client-side user always gets exactly
// their own workspace, whatever they send.
//
// Dashboards, ticket lists and the notification feed all call this, so a count
// on one screen can never disagree with a list on another.
func Reach(ctx context.Context) ClientReach {
	actor := ActorFrom(ctx)
	tenantID := TenantID(ctx)

	if actor == nil {
		return ClientReach{}
	}

	// A client-side user never leaves their own workspace.
	if !actor.IsStaff && !actor.IsSuperAdmin {
		return ClientReach{TenantIDs: []int64{actor.TenantID}}
	}

	// Staff with a client chosen: that client alone.
	if tenantID != 0 && tenantID != actor.TenantID {
		return ClientReach{TenantIDs: []int64{tenantID}}
	}

	// No client chosen. A super admin's remit is the platform; an agent's is
	// the clients they own, or the whole desk when they own none.
	if actor.IsSuperAdmin || len(actor.AssignedTenantIDs) == 0 {
		return ClientReach{All: true}
	}
	return ClientReach{TenantIDs: actor.AssignedTenantIDs}
}

// NarrowTo restricts a reach to one client, when that client is inside it.
//
// Used by the pickers a form fills in: choosing a client in the Add-user dialog
// must narrow the entity, site and department lists to that client without
// switching the whole session to it.
//
// A client outside the reach yields an empty reach rather than the original —
// failing closed, so a bad id shows nothing rather than everything.
func (r ClientReach) NarrowTo(tenantID int64) ClientReach {
	if tenantID == 0 {
		return r
	}
	if r.All {
		return ClientReach{TenantIDs: []int64{tenantID}}
	}
	for _, id := range r.TenantIDs {
		if id == tenantID {
			return ClientReach{TenantIDs: []int64{tenantID}}
		}
	}
	return ClientReach{}
}

// SelectedClientID returns the client a staff request is pinned to, or 0 when
// the request spans every client they can reach. Handlers that must write
// somewhere — creating a ticket, adding a user — need a single client and use
// this to insist on one.
func SelectedClientID(ctx context.Context) int64 {
	actor := ActorFrom(ctx)
	tenantID := TenantID(ctx)
	if actor == nil {
		return tenantID
	}
	if (actor.IsStaff || actor.IsSuperAdmin) && tenantID == actor.TenantID {
		return 0
	}
	return tenantID
}

// Can reports whether the actor holds a permission. Super admins hold all of
// them implicitly; every other role must have the permission granted.
func (a *Actor) Can(perm string) bool {
	if a == nil {
		return false
	}
	if a.IsSuperAdmin {
		return true
	}
	_, ok := a.Permissions[perm]
	return ok
}

// CanAny reports whether the actor holds at least one of the permissions.
func (a *Actor) CanAny(perms ...string) bool {
	for _, p := range perms {
		if a.Can(p) {
			return true
		}
	}
	return false
}

// HasRole reports whether the actor carries a role key.
func (a *Actor) HasRole(role string) bool {
	if a == nil {
		return false
	}
	for _, r := range a.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// ReadOnly reports whether the actor's user group forbids mutations
// (the ex-employee case).
func (a *Actor) ReadOnly() bool {
	return a != nil && a.GroupAccessMode == AccessReadOnly
}

// --- context plumbing -------------------------------------------------------

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(keyRequestID).(string)
	return v
}

func WithTenant(ctx context.Context, t *Tenant) context.Context {
	return context.WithValue(ctx, keyTenant, t)
}

// TenantFrom returns the resolved tenant, or nil on non-tenant-scoped routes.
func TenantFrom(ctx context.Context) *Tenant {
	v, _ := ctx.Value(keyTenant).(*Tenant)
	return v
}

// TenantID returns the resolved tenant id, or 0 when none is present. Every
// repository method must inject this into its WHERE clause.
func TenantID(ctx context.Context) int64 {
	if t := TenantFrom(ctx); t != nil {
		return t.ID
	}
	return 0
}

func WithActor(ctx context.Context, a *Actor) context.Context {
	return context.WithValue(ctx, keyActor, a)
}

// ActorFrom returns the authenticated actor, or nil for anonymous requests.
func ActorFrom(ctx context.Context) *Actor {
	v, _ := ctx.Value(keyActor).(*Actor)
	return v
}

// ActorID returns the authenticated user id, or 0 for anonymous requests.
func ActorID(ctx context.Context) int64 {
	if a := ActorFrom(ctx); a != nil {
		return a.UserID
	}
	return 0
}

func WithPortal(ctx context.Context, p Portal) context.Context {
	return context.WithValue(ctx, keyPortal, p)
}

func PortalFrom(ctx context.Context) Portal {
	v, _ := ctx.Value(keyPortal).(Portal)
	return v
}

func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, keyClientIP, ip)
}

func ClientIP(ctx context.Context) string {
	v, _ := ctx.Value(keyClientIP).(string)
	return v
}

func WithUserAgent(ctx context.Context, ua string) context.Context {
	return context.WithValue(ctx, keyUserAgent, ua)
}

func UserAgent(ctx context.Context) string {
	v, _ := ctx.Value(keyUserAgent).(string)
	return v
}

// WithSignedAccess marks a request as authorised by a short-lived signed URL
// rather than a bearer token (used for inline document preview/download).
func WithSignedAccess(ctx context.Context) context.Context {
	return context.WithValue(ctx, keySignedAccess, true)
}

func IsSignedAccess(ctx context.Context) bool {
	v, _ := ctx.Value(keySignedAccess).(bool)
	return v
}
