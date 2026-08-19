package user

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/org"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// ResetSender is implemented by the auth service. It is declared here as an
// interface because auth already imports user; depending on it directly would
// create a cycle.
type ResetSender interface {
	SendResetLink(ctx context.Context, tenant *appctx.Tenant, target *User, portal appctx.Portal) error

	// HashPassword is here rather than in a package-level helper because the
	// auth package imports this one, so this package cannot import it back.
	// Taking the capability as an interface keeps the dependency one-way and
	// guarantees bulk import hashes exactly the way login verifies.
	HashPassword(password string) (string, error)

	// PasswordAgeing is the client's expiry and history settings, so a password
	// written from this package obeys the same policy as one written from the
	// login flow.
	PasswordAgeing(ctx context.Context, tenantID int64) (expiryDays, historyCount int)
}

// TenantConfig is the slice of tenant settings this package needs.
type TenantConfig interface {
	StringSetting(ctx context.Context, tenantID int64, key, fallback string) string
	FeatureEnabled(ctx context.Context, tenantID int64, key string) bool
}

// Publisher writes notification events to the transactional outbox.
type Publisher interface {
	Publish(ctx context.Context, tenantID int64, eventKey, aggregateType string, aggregateID int64, payload any) error
}

type Handler struct {
	repo      *Repository
	org       *org.Repository
	reset     ResetSender
	tenants   TenantConfig
	auditor   *audit.Writer
	publisher Publisher
	cfg       *config.Config
}

func NewHandler(repo *Repository, orgRepo *org.Repository, reset ResetSender,
	tenants TenantConfig, auditor *audit.Writer, publisher Publisher, cfg *config.Config) *Handler {
	return &Handler{repo: repo, org: orgRepo, reset: reset, tenants: tenants,
		auditor: auditor, publisher: publisher, cfg: cfg}
}

func (h *Handler) Routes(r chi.Router) {
	r.Route("/users", func(r chi.Router) {
		read := middleware.RequireAnyPermission("user.view.scope", "user.view.all")

		r.With(read).Get("/", h.list)
		// The Users landing screen: one tile per group of people this caller
		// administers. Mounted before /{id} so "sections" is not read as an id.
		r.With(read).Get("/sections", h.sections)
		// The clients a new person may be filed against. Mounted before /{id}
		// so "clients" is not read as a user id.
		r.With(read).Get("/clients", h.assignableClients)
		// Who an employee's queries can be handed to on an employment change.
		r.With(read).Get("/assignable-agents", h.assignableAgents)
		r.With(middleware.RequirePermission("user.create")).Post("/", h.create)
		r.With(middleware.RequirePermission("user.move_group")).Post("/move-group", h.moveGroup)

		r.Route("/{id}", func(r chi.Router) {
			r.With(middleware.RequireAnyPermission("user.view.scope", "user.view.all")).Get("/", h.get)
			r.With(middleware.RequirePermission("user.update")).Patch("/", h.update)
			r.With(middleware.RequirePermission("user.delete")).Delete("/", h.remove)
			r.With(middleware.RequirePermission("user.update")).Post("/activate", h.activate)
			r.With(middleware.RequirePermission("user.update")).Post("/deactivate", h.deactivate)
			// Whether somebody still works here, as opposed to whether their
			// account may be used. See changeEmployment.
			r.With(middleware.RequirePermission("user.update")).
				Post("/employment-status", h.changeEmployment)
			r.With(middleware.RequirePermission("user.update")).Post("/unlock", h.unlock)
			r.With(middleware.RequirePermission("user.send_reset")).Post("/send-reset-link", h.sendResetLink)
			// Same permission, same rank check: this is the same act by another
			// route, for an account whose email cannot receive the link.
			r.With(middleware.RequirePermission("user.send_reset")).
				Post("/reset-password", h.resetToDefaultPassword)
			r.With(middleware.RequirePermission("user.update")).Put("/roles", h.setRoles)
			r.With(middleware.RequireAnyPermission("user.view.scope", "user.view.all")).
				Get("/scopes", h.getScopes)
			r.With(middleware.RequirePermission("user.update")).Put("/scopes", h.setScopes)
			r.With(middleware.RequireAnyPermission("user.view.scope", "user.view.all")).Get("/assignments", h.listUserAssignments)
			r.With(middleware.RequireAnyPermission("audit.view", "user.view.all")).Get("/activity", h.activity)
		})
	})

	r.Route("/user-groups", func(r chi.Router) {
		r.Get("/", h.listGroups)
		r.With(middleware.RequirePermission("config.group")).Post("/", h.createGroup)
		r.With(middleware.RequirePermission("config.group")).Patch("/{id}", h.updateGroup)
		r.With(middleware.RequirePermission("config.group")).Delete("/{id}", h.deleteGroup)
	})

	r.Route("/roles", func(r chi.Router) {
		manageRoles := middleware.RequirePermission("config.role")

		r.Get("/", h.listRoles)
		r.With(manageRoles).Post("/", h.createRole)
		r.With(manageRoles).Patch("/{id}", h.updateRole)
		r.With(manageRoles).Delete("/{id}", h.deleteRole)
		r.With(manageRoles).Get("/{id}/permissions", h.rolePermissions)
		r.With(manageRoles).Put("/{id}/permissions", h.setRolePermissions)
	})

	r.Get("/permissions", h.listPermissions)

	r.Route("/me", func(r chi.Router) {
		r.Get("/profile", h.myProfile)
		r.Put("/profile", h.updateMyProfile)
		r.Get("/preferences", h.myPreferences)
		r.Put("/preferences", h.updateMyPreferences)
	})

	// Entity / site / department assignments for partners and agents.
	h.assignmentRoutes(r)
}

// --- response shaping -------------------------------------------------------

type userResponse struct {
	ID           string `json:"id"`
	AvatarURL    string `json:"avatar_url,omitempty"`
	EmployeeCode string `json:"employee_code"`
	Username     string `json:"username,omitempty"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	FullName     string `json:"full_name"`
	Email        string `json:"email"`
	AltEmail     string `json:"alt_email,omitempty"`
	Mobile       string `json:"mobile"`
	AltMobile    string `json:"alt_mobile,omitempty"`
	PANNumber    string `json:"pan_number"`
	UANNumber    string `json:"uan_number"`
	PFNumber     string `json:"pf_number"`
	ESICNumber   string `json:"esic_number"`
	// The default first password derives from it, and a statutory record is
	// checked against it, so the person's record has to show it.
	DateOfBirth    *string    `json:"date_of_birth"`
	DateOfJoining  *string    `json:"date_of_joining"`
	LastWorkingDay *string    `json:"last_working_day"`
	Designation    string     `json:"designation,omitempty"`
	Status         string     `json:"status"`
	Group          *Reference `json:"group"`
	Entity         *Reference `json:"entity"`
	Site           *Reference `json:"site"`
	Department     *Reference `json:"department"`
	Roles          []string   `json:"roles"`
	// CanAdminister says whether the caller outranks this person, and so may
	// reset their password or send them a link. Computed here rather than in
	// the browser: the rank ladder is the server's rule, and a UI deriving it
	// separately would drift from what the API actually allows.
	CanAdminister bool `json:"can_administer"`
	// Client names the workspace this person belongs to. Only a cross-client
	// roster renders it; a single-client screen already knows.
	Client *Reference `json:"client,omitempty"`
	// The agent responsible for this person's queries, set by the employment
	// transition. Null until somebody has been through that workflow.
	HandlingAgent *Reference `json:"handling_agent,omitempty"`
	LastLoginAt   *time.Time `json:"last_login_at"`
	LoginCount    int        `json:"login_count"`
	MFAEnabled    bool       `json:"mfa_enabled"`
	MustChangePwd bool       `json:"must_change_password"`
	LockedUntil   *time.Time `json:"locked_until"`
	CreatedAt     time.Time  `json:"created_at"`
}

// maskPII blanks the statutory identifiers unless the caller holds
// user.view.pii. Revealing them is a separate, audited action.
func maskPII(v string, allowed bool) string {
	if v == "" {
		return ""
	}
	if allowed {
		return v
	}
	if len(v) <= 4 {
		return strings.Repeat("*", len(v))
	}
	return strings.Repeat("*", len(v)-4) + v[len(v)-4:]
}

func (h *Handler) toResponse(r *http.Request, u *User, roles []string) userResponse {
	actor := appctx.ActorFrom(r.Context())
	showPII := actor != nil && actor.Can("user.view.pii")

	out := userResponse{
		ID: u.PublicID, EmployeeCode: u.EmployeeCode.String, Username: u.Username.String,
		FirstName: u.FirstName, LastName: u.LastName.String, FullName: u.FullName(),
		Email: u.Email.String, AltEmail: u.AltEmail.String,
		Mobile: u.Mobile.String, AltMobile: u.AltMobile.String,
		PANNumber:   maskPII(u.PANNumber.String, showPII),
		UANNumber:   maskPII(u.UANNumber.String, showPII),
		PFNumber:    maskPII(u.PFNumber.String, showPII),
		ESICNumber:  maskPII(u.ESICNumber.String, showPII),
		Designation: u.Designation.String, Status: u.Status,
		Roles: roles, LoginCount: u.LoginCount, MFAEnabled: u.MFAEnabled,
		MustChangePwd: u.MustChangePassword, CreatedAt: u.CreatedAt,
	}
	if u.DateOfBirth.Valid {
		s := u.DateOfBirth.Time.Format("2006-01-02")
		out.DateOfBirth = &s
	}
	if u.DateOfJoining.Valid {
		s := u.DateOfJoining.Time.Format("2006-01-02")
		out.DateOfJoining = &s
	}
	if u.LastWorkingDay.Valid {
		s := u.LastWorkingDay.Time.Format("2006-01-02")
		out.LastWorkingDay = &s
	}
	if u.LastLoginAt.Valid {
		out.LastLoginAt = &u.LastLoginAt.Time
	}
	if u.LockedUntil.Valid {
		out.LockedUntil = &u.LockedUntil.Time
	}
	if u.AvatarPath.Valid {
		out.AvatarURL = "/api/v1/public/documents/avatar/" + u.PublicID
	}
	if out.Roles == nil {
		out.Roles = []string{}
	}

	// Strictly outranked, and never the caller themselves — the same rule the
	// API enforces, so a control the UI offers is one the API will accept.
	if actor != nil {
		out.CanAdminister = actor.UserID != u.ID && CanAdminister(actor.Roles, roles)
	}
	return out
}

// toListResponse shapes one row of the roster.
//
// Unlike toResponse it adds nothing by querying: the client, the roles and the
// posting all arrived joined on the list query. That is what lets a page of two
// hundred people cost one query rather than six hundred, and it is the only way
// a cross-client roster can name each row's client at all — the per-tenant
// lookups enrich() does have no single tenant to run against.
func (h *Handler) toListResponse(r *http.Request, u *User) userResponse {
	out := h.toResponse(r, u, splitRoleNames(u.RoleNames.String))

	if u.ClientName.Valid {
		out.Client = &Reference{
			ID: u.ClientSlug.String, Code: u.ClientCode.String, Name: u.ClientName.String,
		}
	}
	ref := func(id, code, name sql.NullString) *Reference {
		if strings.TrimSpace(name.String) == "" {
			return nil
		}
		return &Reference{ID: id.String, Code: code.String, Name: name.String}
	}
	out.Entity = ref(u.EntityPublicID, u.EntityCode, u.EntityName)
	out.Site = ref(u.SitePublicID, u.SiteCode, u.SiteName)
	out.Department = ref(u.DepartmentPublicID, u.DepartmentCode, u.DepartmentName)
	out.Group = ref(u.GroupPublicID, u.GroupKey, u.GroupName)
	if u.HandlingAgentName.Valid && u.HandlingAgentName.String != "" {
		out.HandlingAgent = &Reference{
			ID: u.HandlingAgentPublicID.String, Name: u.HandlingAgentName.String,
		}
	}
	return out
}

// splitRoleNames turns the joined "Client Admin, Employee" back into a slice.
func splitRoleNames(joined string) []string {
	out := []string{}
	for _, part := range strings.Split(joined, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// enrich attaches group, entity, site and department references in bulk rather
// than one query per row.
func (h *Handler) enrich(r *http.Request, users []User, out []userResponse) []userResponse {
	ctx := r.Context()

	// Resolve names against the client the *records* belong to, not the one the
	// header named. Every caller passes a single user, and on a cross-client
	// roster that user's client is not the resolved tenant — looking up the
	// platform workspace would leave entity, site and department blank on the
	// detail screen while the list beside it showed them.
	tenantID := appctx.TenantID(ctx)
	if len(users) > 0 {
		tenantID = users[0].TenantID
	}

	groups, _ := h.repo.ListGroups(ctx, tenantID)
	groupByID := map[int64]Group{}
	for _, g := range groups {
		groupByID[g.ID] = g
	}

	entities, _ := h.org.Entities(ctx, appctx.OneClient(tenantID), false, nil, platform.Page{}, org.OrgFilter{})
	entityByID := map[int64]Reference{}
	for _, e := range entities {
		entityByID[e.ID] = Reference{ID: e.PublicID, Code: e.Code, Name: e.Name}
	}

	sites, _ := h.org.Sites(ctx, appctx.OneClient(tenantID), nil, false, nil, platform.Page{}, org.OrgFilter{})
	siteByID := map[int64]Reference{}
	for _, s := range sites {
		siteByID[s.ID] = Reference{ID: s.PublicID, Code: s.Code, Name: s.Name}
	}

	departments, _ := h.org.Departments(ctx, appctx.OneClient(tenantID), false, nil, platform.Page{}, org.OrgFilter{})
	deptByID := map[int64]Reference{}
	for _, d := range departments {
		deptByID[d.ID] = Reference{ID: d.PublicID, Code: d.Code, Name: d.Name}
	}

	for i := range users {
		if users[i].UserGroupID.Valid {
			if g, ok := groupByID[users[i].UserGroupID.Int64]; ok {
				out[i].Group = &Reference{ID: g.PublicID, Code: g.Key, Name: g.Name}
			}
		}
		if users[i].EntityID.Valid {
			if ref, ok := entityByID[users[i].EntityID.Int64]; ok {
				out[i].Entity = &ref
			}
		}
		if users[i].SiteID.Valid {
			if ref, ok := siteByID[users[i].SiteID.Int64]; ok {
				out[i].Site = &ref
			}
		}
		if users[i].DepartmentID.Valid {
			if ref, ok := deptByID[users[i].DepartmentID.Int64]; ok {
				out[i].Department = &ref
			}
		}
	}
	return out
}

// --- list / get -------------------------------------------------------------

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)
	actor := appctx.ActorFrom(ctx)

	page := platform.ParsePage(r, UserSortable, "u.created_at")
	filter := ListFilter{
		Query:         strings.TrimSpace(r.URL.Query().Get("q")),
		Status:        platform.QueryStrings(r, "status"),
		RoleKeys:      platform.QueryStrings(r, "role"),
		NeverLoggedIn: platform.QueryBool(r, "never_logged_in"),
		MissingPF:     platform.QueryBool(r, "missing_pf"),
		Page:          page,
	}
	filter.DOJFrom, filter.DOJTo = platform.QueryDates(r, "doj_from", "doj_to")

	// A caller without user.view.all only ever sees their assigned scope.
	if actor != nil && !actor.Can("user.view.all") && !actor.IsSuperAdmin {
		filter.ScopeEntities = actor.Scopes.Entities
		filter.ScopeSites = actor.Scopes.Sites
		filter.ScopeDepartments = actor.Scopes.Departments
	}

	if ids := platform.QueryStrings(r, "group_id"); len(ids) > 0 {
		resolved, err := h.resolveGroupIDs(r, ids)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		filter.GroupIDs = resolved
	}
	if ids := platform.QueryStrings(r, "entity_id"); len(ids) > 0 {
		resolved, err := h.org.ResolveEntityIDs(ctx, tenantID, ids)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("entity_id", "NOT_FOUND", "One of those entities was not found."))
			return
		}
		filter.EntityIDs = resolved
	}
	if ids := platform.QueryStrings(r, "department_id"); len(ids) > 0 {
		resolved, err := h.org.ResolveDepartmentIDs(ctx, tenantID, ids)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("department_id", "NOT_FOUND", "One of those departments was not found."))
			return
		}
		filter.DepartmentIDs = resolved
	}

	// Which Users section this is: agents, partners or employees. An unknown
	// value is refused rather than quietly listing everyone, because a typo in
	// the section name would otherwise show a partner the whole roster.
	if kind := strings.TrimSpace(r.URL.Query().Get("kind")); kind != "" {
		if !ValidKind(kind) {
			httpx.Fail(w, r, httpx.ErrField("kind", "INVALID",
				"kind must be one of "+strings.Join(Kinds, ", ")+"."))
			return
		}
		filter.Kind = kind
	}

	// A partner administers their own client's employees and nobody else's —
	// not the agents working their account, and not their fellow partners.
	// Forcing the section here rather than trusting the query means the rule
	// holds even if the browser asks for something wider, and it is why the
	// partner's Users screen has one tab where an agent's has two.
	if actor != nil && !actor.IsStaff && !actor.IsSuperAdmin {
		filter.Kind = KindEmployee
	}

	// The clients this roster covers: every one in reach when staff have not
	// selected a client, that client alone when they have.
	filter.Reach = appctx.Reach(ctx)

	rows, total, err := h.repo.List(ctx, filter)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	// Everything the row needs — client, roles, posting, group — was joined by
	// the list query, so this loop issues no further queries.
	out := make([]userResponse, len(rows))
	for i := range rows {
		out[i] = h.toListResponse(r, &rows[i])
	}

	httpx.List(w, r, out, platform.NewMeta(page, total))
}

// assignableClients answers the Add-user form's client picker.
//
// A person belongs to exactly one client, and staff working across clients have
// not necessarily chosen one from the switcher — so the form asks, and this is
// what it offers. Derived from the caller's reach, so it can never name a client
// they could not then write to.
func (h *Handler) assignableClients(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := h.repo.ClientsInReach(ctx, appctx.Reach(ctx))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, rows)
}

// sections answers the Users landing screen: one tile per group of people the
// caller administers, with a headcount on each.
//
// Which tiles appear is the hierarchy in the brief, read from the session:
// an admin administers all three, an agent the two client-side ones, and a
// partner only their own client's employees.
func (h *Handler) sections(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	var kinds []string
	switch {
	case actor.IsSuperAdmin:
		kinds = Kinds
	case actor.IsStaff:
		// Agents administer the client side. ComplyDesk's own people are the
		// admin's to manage, not another agent's.
		kinds = []string{KindPartner, KindEmployee}
	default:
		kinds = []string{KindEmployee}
	}

	counts, err := h.repo.SectionCounts(ctx, appctx.Reach(ctx), kinds)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	// Whether this caller may change what they can see. A partner reads and
	// raises change requests; everyone above them edits directly.
	canManage := actor.Can("user.create") || actor.Can("user.update")

	labels := map[string]string{
		KindAgent:    "Agents",
		KindPartner:  "Partners",
		KindEmployee: "Employees",
	}
	blurbs := map[string]string{
		KindAgent:    "ComplyDesk helpdesk staff, across every client or mapped to a department.",
		KindPartner:  "Client-side administrators, allocated to some or all of a client's entities.",
		KindEmployee: "The people who raise tickets.",
	}

	items := make([]map[string]any, 0, len(counts))
	for _, c := range counts {
		items = append(items, map[string]any{
			"kind": c.Kind, "label": labels[c.Kind], "description": blurbs[c.Kind],
			"total": c.Total, "active": c.Active,
		})
	}

	httpx.OK(w, r, map[string]any{
		"items":      items,
		"can_manage": canManage,
		// A partner cannot edit directly; the UI offers "request a change"
		// instead of an edit button, and this is what tells it which to show.
		"read_only": !canManage,
	})
}

func roleKeys(roles []Role) []string {
	out := make([]string, 0, len(roles))
	for _, role := range roles {
		out = append(out, role.Key)
	}
	return out
}

// loadTarget resolves the user in the path and enforces the caller's scope.
func (h *Handler) loadTarget(r *http.Request) (*User, error) {
	ctx := r.Context()

	id := chi.URLParam(r, "id")
	if !platform.ValidULID(id) {
		return nil, httpx.ErrNotFound("That user")
	}

	// Resolved across every client the caller can reach, not against the tenant
	// the header named: staff working the cross-client roster have no client
	// selected, so that header names the platform workspace and a pinned lookup
	// would fail for every row in the list they just clicked.
	u, err := h.repo.ByPublicIDInReach(ctx, appctx.Reach(ctx), id)
	if err != nil {
		return nil, mapErr(err, "That user")
	}

	actor := appctx.ActorFrom(ctx)
	if actor == nil || actor.IsSuperAdmin || actor.Can("user.view.all") {
		return u, nil
	}
	if actor.UserID == u.ID {
		return u, nil // always allowed to reach yourself
	}
	if !inScope(actor.Scopes.Entities, u.EntityID.Int64, u.EntityID.Valid) ||
		!inScope(actor.Scopes.Departments, u.DepartmentID.Int64, u.DepartmentID.Valid) ||
		!inScope(actor.Scopes.Sites, u.SiteID.Int64, u.SiteID.Valid) {
		// NOT_FOUND, not FORBIDDEN: existence outside the caller's scope must
		// not be disclosed.
		return nil, httpx.ErrNotFound("That user")
	}
	return u, nil
}

// inScope reports whether a value is inside a scope list. A nil list means the
// caller is not scoped on that dimension.
func inScope(scope []int64, value int64, valid bool) bool {
	if scope == nil {
		return true
	}
	if !valid {
		return false // scoped callers cannot see records with no value set
	}
	for _, id := range scope {
		if id == value {
			return true
		}
	}
	return false
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	u, err := h.loadTarget(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	roles, _ := h.repo.RolesFor(ctx, u.ID)

	out := []userResponse{h.toResponse(r, u, roleKeys(roles))}
	out = h.enrich(r, []User{*u}, out)

	scopes, _ := h.repo.ScopesFor(ctx, u.ID)
	httpx.OK(w, r, map[string]any{
		"user":   out[0],
		"scopes": h.scopeRefs(r, u.TenantID, scopes),
	})
}

// scopeRefs turns raw scope ids into named references.
//
// `tenantID` is the *target's* client, not the one the request arrived under: a
// staff user editing a partner from the cross-client roster has no client
// selected, and resolving against the platform workspace would return no names
// at all — an allocation that looks empty when it is not.
// getScopes answers "which entities, sites and departments is this person
// allocated to?" — what the Users → Partners access editor loads before it can
// offer a change.
func (h *Handler) getScopes(w http.ResponseWriter, r *http.Request) {
	u, err := h.loadTarget(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// As saved, not as expanded: this is the editor's read, and it has to show
	// what was chosen rather than what it implies.
	scopes, err := h.repo.RawScopesFor(r.Context(), u.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, h.scopeRefs(r, u.TenantID, scopes))
}

func (h *Handler) scopeRefs(r *http.Request, tenantID int64, scopes []Scope) map[string][]Reference {
	ctx := r.Context()

	byType := map[string][]int64{}
	for _, s := range scopes {
		byType[s.ScopeType] = append(byType[s.ScopeType], s.ScopeID)
	}

	out := map[string][]Reference{
		"entities": {}, "sites": {}, "departments": {}, "categories": {},
	}

	if ids := byType[ScopeEntity]; len(ids) > 0 {
		if rows, err := h.org.Entities(ctx, appctx.OneClient(tenantID), false, ids, platform.Page{}, org.OrgFilter{}); err == nil {
			for _, e := range rows {
				out["entities"] = append(out["entities"], Reference{ID: e.PublicID, Code: e.Code, Name: e.Name})
			}
		}
	}
	if ids := byType[ScopeSite]; len(ids) > 0 {
		if rows, err := h.org.Sites(ctx, appctx.OneClient(tenantID), nil, false, ids, platform.Page{}, org.OrgFilter{}); err == nil {
			for _, s := range rows {
				out["sites"] = append(out["sites"], Reference{ID: s.PublicID, Code: s.Code, Name: s.Name})
			}
		}
	}
	if ids := byType[ScopeDepartment]; len(ids) > 0 {
		if rows, err := h.org.Departments(ctx, appctx.OneClient(tenantID), false, ids, platform.Page{}, org.OrgFilter{}); err == nil {
			for _, d := range rows {
				out["departments"] = append(out["departments"], Reference{ID: d.PublicID, Code: d.Code, Name: d.Name})
			}
		}
	}
	return out
}

// --- create / update --------------------------------------------------------

type userRequest struct {
	EmployeeCode   string   `json:"employee_code" validate:"omitempty,employeeid"`
	Username       string   `json:"username" validate:"omitempty,max=96"`
	FirstName      string   `json:"first_name" validate:"required,notblank,max=96,safetext"`
	LastName       string   `json:"last_name" validate:"omitempty,max=96,safetext"`
	Email          string   `json:"email" validate:"omitempty,email,max=191"`
	AltEmail       string   `json:"alt_email" validate:"omitempty,email,max=191"`
	Mobile         string   `json:"mobile" validate:"omitempty,mobile"`
	AltMobile      string   `json:"alt_mobile" validate:"omitempty,mobile"`
	PANNumber      string   `json:"pan_number" validate:"omitempty,pan"`
	UANNumber      string   `json:"uan_number" validate:"omitempty,uan"`
	PFNumber       string   `json:"pf_number" validate:"omitempty,pfnumber"`
	ESICNumber     string   `json:"esic_number" validate:"omitempty,esic"`
	DateOfJoining  string   `json:"date_of_joining" validate:"omitempty,dateonly"`
	DateOfBirth    string   `json:"date_of_birth" validate:"omitempty,dateonly"`
	LastWorkingDay string   `json:"last_working_day" validate:"omitempty,dateonly"`
	EntityID       string   `json:"entity_id" validate:"omitempty,len=26"`
	SiteID         string   `json:"site_id" validate:"omitempty,len=26"`
	DepartmentID   string   `json:"department_id" validate:"omitempty,len=26"`
	Designation    string   `json:"designation" validate:"omitempty,max=128"`
	GroupID        string   `json:"group_id" validate:"omitempty,len=26"`
	Status         string   `json:"status" validate:"omitempty,oneof=ACTIVE INACTIVE LOCKED EX_EMPLOYEE"`
	Roles          []string `json:"roles" validate:"omitempty,dive,max=64"`
	SendInvitation *bool    `json:"send_invitation"`

	// Client names the workspace this person belongs to, by public id, slug or
	// client code. A person always belongs to exactly one client, and staff
	// working across clients have not necessarily chosen one from the switcher —
	// so the Add-user form asks, and this is what it sends. Omitted, it falls
	// back to the selected client.
	Client string `json:"client" validate:"omitempty,max=64"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req userRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()

	// Which client this person belongs to.
	//
	// Taken from the request when the form named one, otherwise from the
	// switcher. Resolving it here rather than reading the header is what fixes
	// "That entity was not found": with no client selected the header names the
	// platform workspace, so the entity picker offered a client's entities
	// while the create looked for them somewhere else entirely.
	tenantID, err := h.createTenantID(r, req)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if req.Email == "" && req.EmployeeCode == "" && req.PFNumber == "" {
		httpx.Fail(w, r, httpx.ErrField("email", "REQUIRED",
			"Provide at least an email address, an employee ID, or a PF number."))
		return
	}

	if details := h.validateIdentifiers(ctx, tenantID, req, 0); len(details) > 0 {
		httpx.Fail(w, r, httpx.ErrValidation(details...))
		return
	}

	params := CreateParams{
		EmployeeCode: req.EmployeeCode, Username: req.Username,
		FirstName: req.FirstName, LastName: req.LastName,
		Email: req.Email, AltEmail: req.AltEmail,
		Mobile: req.Mobile, AltMobile: req.AltMobile,
		PANNumber: strings.ToUpper(strings.TrimSpace(req.PANNumber)),
		UANNumber: req.UANNumber,
		// Stored in the readable MH/BAN/... form whichever way it was typed.
		PFNumber: httpx.NormalisePFNumber(req.PFNumber), ESICNumber: req.ESICNumber,
		Designation: req.Designation, Status: req.Status,
	}
	if d, ok := parseDate(req.DateOfJoining); ok {
		params.DateOfJoining = &d
	}
	if d, ok := parseDate(req.DateOfBirth); ok {
		params.DateOfBirth = &d
	}
	if d, ok := parseDate(req.LastWorkingDay); ok {
		params.LastWorkingDay = &d
	}
	if actor := appctx.ActorFrom(ctx); actor != nil {
		id := actor.UserID
		params.CreatedBy = &id
	}

	// The first password, derived the same way the bulk import derives it: the
	// PAN in lower case, an '@', and the birth year. Deriving rather than
	// inventing means an administrator can tell somebody their password without
	// a lookup, and it is always forced to change on first sign-in.
	var firstPassword string
	if password, ok := DefaultPassword(params.PANNumber, derefTime(params.DateOfBirth)); ok {
		hash, err := h.reset.HashPassword(password)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrInternal(err))
			return
		}
		params.PasswordHash = hash
		params.MustChange = true
		firstPassword = password
	}

	if err := h.resolveReferences(ctx, tenantID, req, &params); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// Every new employee lands in the active group unless told otherwise.
	if params.UserGroupID == nil {
		if g, err := h.repo.GroupByKey(ctx, tenantID, GroupActiveEmployees); err == nil {
			params.UserGroupID = &g.ID
		}
	}

	created, err := h.repo.Create(ctx, tenantID, params)
	if err != nil {
		var dup *DuplicateError
		if errors.As(err, &dup) {
			httpx.Fail(w, r, httpx.ErrDuplicate(dup.Field(),
				"Another user already has this value."))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	if len(req.Roles) > 0 {
		if err := h.applyRoles(ctx, tenantID, created.ID, req.Roles); err != nil {
			httpx.Fail(w, r, err)
			return
		}
	}

	// Invite by default: a user with no password cannot sign in otherwise.
	if req.SendInvitation == nil || *req.SendInvitation {
		if created.PreferredEmail() != "" {
			if err := h.publisher.Publish(ctx, tenantID, "user.welcome", "user", created.ID, map[string]any{
				"user_public_id": created.PublicID,
				"full_name":      created.FullName(),
				"username":       created.LoginName(),
				"recipients":     []string{created.PreferredEmail()},
			}); err != nil {
				httpx.Fail(w, r, httpx.ErrInternal(err))
				return
			}
		}
	}

	roles, _ := h.repo.RolesFor(ctx, created.ID)
	out := []userResponse{h.toResponse(r, created, roleKeys(roles))}
	out = h.enrich(r, []User{*created}, out)

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionUserCreated, EntityType: "user", EntityID: &created.ID,
		EntityPublicID: created.PublicID, After: out[0],
	})

	// The first password is returned once, to the administrator who created the
	// account, so they can pass it on. It is never stored in plain text and
	// never returned again — a later read of this user does not include it.
	body := map[string]any{"user": out[0]}
	if firstPassword != "" {
		body["first_password"] = firstPassword
	}
	httpx.Created(w, r, body)
}

// derefTime unwraps an optional date, yielding the zero time when absent.
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// createTenantID decides which client a new person is filed against.
func (h *Handler) createTenantID(r *http.Request, req userRequest) (int64, error) {
	return h.writeTenantID(r, req.Client, "Choose the client this person belongs to.")
}

// writeTenantID decides which client any new record is filed against.
//
// An explicit `client` on the request wins, then the client selected in the
// switcher. Staff with neither are reaching every client they support, and a
// create has to land in exactly one — so this refuses rather than filing the
// record in ComplyDesk's own workspace, and `missing` says which choice is
// being asked for.
func (h *Handler) writeTenantID(r *http.Request, ref, missing string) (int64, error) {
	ctx := r.Context()

	if ref := strings.TrimSpace(ref); ref != "" {
		id, err := h.repo.ResolveClientInReach(ctx, appctx.Reach(ctx), ref)
		if err != nil {
			return 0, httpx.ErrField("client", "NOT_FOUND", "That client was not found.")
		}
		return id, nil
	}

	if id := appctx.SelectedClientID(ctx); id != 0 {
		return id, nil
	}
	return 0, httpx.ErrField("client", "REQUIRED", missing)
}

// validateIdentifiers enforces the statutory rules a plain field validator
// cannot: which identifiers are mandatory for whom, and where they must be
// unique.
//
// `excludeUserID` is the record being edited, so an update does not collide
// with itself. Pass 0 when creating.
func (h *Handler) validateIdentifiers(ctx context.Context, tenantID int64,
	req userRequest, excludeUserID int64) []httpx.FieldError {

	var details []httpx.FieldError
	employee := isEmployeeRequest(req.Roles)

	// PAN is mandatory for everybody: it is what the default password is built
	// from, so an account without one cannot be signed into.
	pan := strings.ToUpper(strings.TrimSpace(req.PANNumber))
	if pan == "" {
		details = append(details, httpx.FieldError{
			Field: "pan_number", Code: "REQUIRED",
			Message: "PAN is required. It is used to build the first password."})
	}

	// PF number and date of joining are mandatory platform-wide for employees.
	if employee {
		if strings.TrimSpace(req.PFNumber) == "" {
			details = append(details, httpx.FieldError{
				Field: "pf_number", Code: "REQUIRED", Message: "PF Number is required for employees."})
		}
		if strings.TrimSpace(req.DateOfJoining) == "" {
			details = append(details, httpx.FieldError{
				Field: "date_of_joining", Code: "REQUIRED", Message: "Date of Joining is required for employees."})
		}
		// A date of birth is what the year in the default password comes from.
		if strings.TrimSpace(req.DateOfBirth) == "" {
			details = append(details, httpx.FieldError{
				Field: "date_of_birth", Code: "REQUIRED",
				Message: "Date of Birth is required. It is used to build the first password."})
		}
	}

	// An employee's PAN identifies one person to the tax authority, so two
	// employees of the same client cannot share one — a duplicate is a data
	// entry error that would later merge two people's records.
	//
	// Agents and partners are deliberately exempt: the same person legitimately
	// holds an agent account and a partner account, and refusing that would
	// force a second PAN that does not exist.
	if employee && pan != "" {
		taken, err := h.repo.EmployeePANTaken(ctx, tenantID, pan, excludeUserID)
		if err == nil && taken {
			details = append(details, httpx.FieldError{
				Field: "pan_number", Code: "DUPLICATE",
				Message: "Another employee of this client already has this PAN."})
		}
	}

	// An ex-employee has, by definition, left.
	if strings.EqualFold(req.Status, StatusExEmployee) && strings.TrimSpace(req.LastWorkingDay) == "" {
		details = append(details, httpx.FieldError{
			Field: "last_working_day", Code: "REQUIRED",
			Message: "Date of exit is required for an ex-employee."})
	}

	return details
}

func isEmployeeRequest(roles []string) bool {
	if len(roles) == 0 {
		return true // no role means a plain employee
	}
	for _, role := range roles {
		if role == RoleEmployee {
			return true
		}
	}
	return false
}

func (h *Handler) resolveReferences(ctx context.Context, tenantID int64, req userRequest, params *CreateParams) error {
	if req.EntityID != "" {
		e, err := h.org.EntityByPublicID(ctx, tenantID, req.EntityID)
		if err != nil {
			return httpx.ErrField("entity_id", "NOT_FOUND", "That entity was not found.")
		}
		params.EntityID = &e.ID
	}
	if req.SiteID != "" {
		st, err := h.org.SiteByPublicID(ctx, tenantID, req.SiteID)
		if err != nil {
			return httpx.ErrField("site_id", "NOT_FOUND", "That site was not found.")
		}
		params.SiteID = &st.ID
	}
	if req.DepartmentID != "" {
		d, err := h.org.DepartmentByPublicID(ctx, tenantID, req.DepartmentID)
		if err != nil {
			return httpx.ErrField("department_id", "NOT_FOUND", "That department was not found.")
		}
		params.DepartmentID = &d.ID
	}
	if req.GroupID != "" {
		g, err := h.repo.GroupByPublicID(ctx, tenantID, req.GroupID)
		if err != nil {
			return httpx.ErrField("group_id", "NOT_FOUND", "That user group was not found.")
		}
		params.UserGroupID = &g.ID
	}
	return nil
}

func parseDate(v string) (time.Time, bool) {
	if strings.TrimSpace(v) == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func (h *Handler) applyRoles(ctx context.Context, tenantID, userID int64, roleKeys []string) error {
	ids := make([]int64, 0, len(roleKeys))
	for _, key := range roleKeys {
		role, err := h.repo.RoleByKey(ctx, tenantID, key)
		if err != nil {
			return httpx.ErrField("roles", "NOT_FOUND", "The role "+key+" does not exist.")
		}
		ids = append(ids, role.ID)
	}
	if err := h.repo.SetRoles(ctx, tenantID, userID, ids, nil); err != nil {
		return httpx.ErrInternal(err)
	}
	return nil
}

func mapErr(err error, resource string) error {
	switch {
	case errors.Is(err, platform.ErrSentinelNotFound):
		return httpx.ErrNotFound(resource)
	case errors.Is(err, platform.ErrSentinelConflict):
		return httpx.ErrConflict("")
	case errors.Is(err, platform.ErrSentinelImmutable):
		return httpx.ErrConflict("This is a system record and cannot be changed.")
	default:
		return httpx.ErrInternal(err)
	}
}
