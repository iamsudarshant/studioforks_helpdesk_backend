package ticket

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/user"
)

// PriorityCatalogue is the slice of the priority catalogue the ticket engine
// needs: is this level one the client offers, and what do they use by default.
//
// Named in terms of strings rather than the catalogue's own type on purpose —
// the ticket package should not have to import the catalogue package to raise a
// ticket, and a test can satisfy this with a map.
type PriorityCatalogue interface {
	// KnownPriority returns the canonical key, or "" when the client does not
	// offer that level.
	KnownPriority(ctx context.Context, tenantID int64, key string) (string, error)
	DefaultPriority(ctx context.Context, tenantID int64) (string, error)
}

type Handler struct {
	svc     *Service
	users   *user.Repository
	auditor *audit.Writer
	// Optional: nil falls back to accepting whatever was sent, which is how the
	// platform routes (which have no single client) stay working.
	priorities PriorityCatalogue
}

func NewHandler(svc *Service, users *user.Repository, auditor *audit.Writer) *Handler {
	return &Handler{svc: svc, users: users, auditor: auditor}
}

// WithPriorities attaches the priority catalogue. Separate from the constructor
// so the platform handler, which has no client to look a catalogue up in, is
// not obliged to supply one.
func (h *Handler) WithPriorities(p PriorityCatalogue) *Handler {
	h.priorities = p
	return h
}

func (h *Handler) Routes(r chi.Router) {
	r.Route("/tickets", func(r chi.Router) {
		read := middleware.RequireAnyPermission("ticket.view.own", "ticket.view.scope", "ticket.view.all")

		r.With(read).Get("/", h.list)
		r.With(read).Get("/counts", h.counts)
		r.With(middleware.RequirePermission("ticket.create")).Post("/", h.create)

		// Export, bulk actions and the assignee picker. Declared before the
		// /{id} block below so "export" is not matched as a ticket id.
		h.bulkRoutes(r, read)

		r.Route("/{id}", func(r chi.Router) {
			r.With(read).Get("/", h.get)
			r.With(middleware.RequirePermission("ticket.update")).Patch("/", h.update)

			r.With(read).Get("/timeline", h.timeline)
			r.With(read).Get("/conversations", h.conversations)
			// Replying carries no route-level gate: whether a caller may reply is
			// decided per ticket in the handler, because a partner with reply
			// rights on one entity may hold none on another.
			r.Post("/conversations", h.reply)

			// Editing and withdrawing carry no permission of their own: the
			// handler allows your own words and nothing else, unless you hold
			// ticket.moderate. A permission gate here would be a second, weaker
			// answer to the same question.
			r.With(read).Patch("/conversations/{conversationId}", h.editReply)
			r.With(read).Delete("/conversations/{conversationId}", h.deleteReply)
			r.With(read).Post("/conversations/{conversationId}/read", h.markReplyRead)

			// Watching is not an administrative act — anyone who can see a
			// ticket may follow it — so the read gate is the whole gate here.
			// Adding somebody *else* is checked in the handler, where the
			// distinction can actually be made.
			// Department → Agent for the transfer dialog.
			r.With(read).Get("/transfer-agents", h.departmentAgents)

			r.With(read).Get("/watchers", h.listWatchers)
			// Who may be added. Its own route rather than the assign picker,
			// which the panel borrowed: that list is gated on ticket.assign, so
			// anyone without it — most of the desk — saw an empty dropdown and
			// could not add a watcher at all.
			r.With(read).Get("/watcher-candidates", h.watcherCandidates)
			r.With(read).Post("/watchers", h.addWatcher)
			r.With(read).Delete("/watchers/{userId}", h.removeWatcher)
			// The body form, for clients that cannot send one on a DELETE path.
			r.With(read).Delete("/watchers", h.removeWatcher)

			r.With(read).Get("/attachments", h.attachments)
			r.With(middleware.RequirePermission("document.upload")).Post("/attachments", h.attach)
			r.With(read).Delete("/attachments/{attachmentId}", h.detach)

			r.With(middleware.RequirePermission("ticket.status.change")).Post("/status", h.changeStatus)
			r.With(middleware.RequirePermission("ticket.assign")).Post("/assign", h.assign)
			r.With(middleware.RequirePermission("ticket.transfer")).Post("/transfer", h.transfer)
			r.With(middleware.RequirePermission("ticket.escalate")).Post("/escalate", h.escalate)
			r.With(middleware.RequirePermission("ticket.status.change")).Post("/resolve", h.resolve)
			r.With(middleware.RequirePermission("ticket.status.change")).Post("/request-info", h.requestInfo)
			r.With(middleware.RequirePermission("ticket.close")).Post("/close", h.close)
			r.With(middleware.RequirePermission("ticket.cancel")).Post("/cancel", h.cancel)
			// Reopen is deliberately available to the employee who raised it,
			// within the client's configured window.
			r.With(middleware.RequirePermission("ticket.reopen")).Post("/reopen", h.reopen)
			r.With(middleware.RequirePermission("ticket.feedback")).Post("/feedback", h.feedback)
		})
	})
}

// --- responses --------------------------------------------------------------

type ticketResponse struct {
	ID           string         `json:"id"`
	TicketNumber string         `json:"ticket_number"`
	Subject      string         `json:"subject"`
	Description  string         `json:"description,omitempty"`
	Status       string         `json:"status"`
	Priority     string         `json:"priority"`
	Source       string         `json:"source"`
	Category     reference      `json:"category"`
	Subcategory  *reference     `json:"subcategory"`
	Requester    requesterBlock `json:"requester"`
	// RaisedOnBehalf marks a ticket somebody filed for someone else — a partner
	// or an agent acting for an employee. True whenever the person who created
	// it is not the person it is about, which is the only definition that
	// cannot drift from the data.
	RaisedOnBehalf bool           `json:"raised_on_behalf"`
	RaisedBy       *reference     `json:"raised_by,omitempty"`
	Assignee       *reference     `json:"assignee"`
	Entity         *reference     `json:"entity"`
	Site           *reference     `json:"site"`
	Department     *reference     `json:"department"`
	CustomFields   map[string]any `json:"custom_fields,omitempty"`
	SLA            slaBlock       `json:"sla"`
	Counts         countsBlock    `json:"counts"`
	// Files uploaded while raising the ticket. They belong to the description
	// rather than to any reply, so the conversation cannot carry them.
	Attachments []attachmentBlock `json:"attachments,omitempty"`
	// The people following this ticket. Carried on the detail read because the
	// rail renders them beside the assignee, and a second round trip to find
	// out whether anybody is watching is a request per ticket opened. The
	// dedicated /watchers routes still exist for the add and remove, which
	// answer with the updated list.
	//
	// `omitempty` so a list row — which never selects them — does not claim
	// there are none.
	Watchers        []watcherRef      `json:"watchers,omitempty"`
	CSATScore       *int64            `json:"csat_score"`
	ReopenedCount   int               `json:"reopened_count"`
	EscalationLevel int               `json:"escalation_level"`
	// IsEscalated is the indicator the board renders beside the status, rather
	// than instead of it. Derived here so every client agrees on what "escalated"
	// means instead of each one testing the level itself.
	IsEscalated    bool        `json:"is_escalated"`
	Client         clientBlock `json:"client"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
	LastActivityAt time.Time   `json:"last_activity_at"`
	ResolvedAt     *time.Time  `json:"resolved_at"`
	ClosedAt       *time.Time  `json:"closed_at"`

	// Everything the UI needs to decide what to render, computed server-side so
	// the client never reimplements the rules.
	AllowedTransitions []transitionResponse `json:"allowed_transitions,omitempty"`
	Permissions        *permissionsBlock    `json:"permissions,omitempty"`
}

type reference struct {
	ID   string `json:"id,omitempty"`
	Code string `json:"code,omitempty"`
	Name string `json:"name"`
}

// requesterBlock identifies who the ticket is for.
//
// The list needs only a name. The detail rail needs the statutory identity as
// well, because that is what the query is actually about: an agent quotes the
// UAN to EPFO and a partner checks the ESIC number against their register, and
// neither can act on a name alone. The extra fields are `omitempty` and left
// unset by the list mapper, so a page of rows stays free of identifiers nobody
// on that screen is looking at.
type requesterBlock struct {
	ID           string `json:"id,omitempty"`
	Name         string `json:"full_name"`
	EmployeeCode string `json:"employee_code,omitempty"`

	Email       string `json:"email,omitempty"`
	Mobile      string `json:"mobile,omitempty"`
	UANNumber   string `json:"uan_number,omitempty"`
	PFNumber    string `json:"pf_number,omitempty"`
	ESICNumber  string `json:"esic_number,omitempty"`
	PANNumber   string `json:"pan_number,omitempty"`
	Designation string `json:"designation,omitempty"`
	Status      string `json:"status,omitempty"`

	DateOfJoining *time.Time `json:"date_of_joining,omitempty"`
	DateOfBirth   *time.Time `json:"date_of_birth,omitempty"`
	// Set for someone who has left. The date the query relates to often falls
	// after it, which changes what can still be claimed.
	LastWorkingDay *time.Time `json:"last_working_day,omitempty"`

	// The requester's own posting, which is not necessarily the ticket's
	// routing — a member can be posted to one establishment and raise a query
	// about another.
	Entity     *reference `json:"entity,omitempty"`
	Department *reference `json:"department,omitempty"`
}

// withRequesterDetail fills the statutory half of the requester block. Called
// only from the detail mapper, where the columns have actually been selected.
func withRequesterDetail(block requesterBlock, t *Ticket) requesterBlock {
	block.ID = t.RequesterPublicID.String
	block.Email = t.RequesterEmail.String
	block.Mobile = t.RequesterMobile.String
	block.UANNumber = t.RequesterUAN.String
	block.PFNumber = t.RequesterPF.String
	block.ESICNumber = t.RequesterESIC.String
	block.PANNumber = t.RequesterPAN.String
	block.Designation = t.RequesterDesignation.String
	block.Status = t.RequesterStatus.String

	if t.RequesterDOJ.Valid {
		block.DateOfJoining = &t.RequesterDOJ.Time
	}
	if t.RequesterDOB.Valid {
		block.DateOfBirth = &t.RequesterDOB.Time
	}
	if t.RequesterExitDate.Valid {
		block.LastWorkingDay = &t.RequesterExitDate.Time
	}
	if t.RequesterEntityName.Valid {
		block.Entity = &reference{Name: t.RequesterEntityName.String}
	}
	if t.RequesterDeptName.Valid {
		block.Department = &reference{Name: t.RequesterDeptName.String}
	}
	return block
}

// clientBlock names the client a ticket belongs to. It is always populated; the
// agent portal ignores it and the admin's cross-client list renders it.
type clientBlock struct {
	ID         string `json:"id,omitempty"`
	Slug       string `json:"slug,omitempty"`
	ClientCode string `json:"client_code,omitempty"`
	Name       string `json:"name"`
}

type slaBlock struct {
	FirstResponseDueAt *time.Time `json:"first_response_due_at"`
	ResolutionDueAt    *time.Time `json:"resolution_due_at"`
	FirstRespondedAt   *time.Time `json:"first_responded_at"`
	IsBreached         bool       `json:"is_breached"`
}

type countsBlock struct {
	Reopened int `json:"reopened"`
}

// watcherRef is one follower, in the shape the rail's chips read.
type watcherRef struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	// Why they are following, when a reason was recorded.
	Reason string `json:"reason,omitempty"`
}

// attachmentBlock is one file, in the shape the conversation already uses so
// the thread and the opening post render them identically.
type attachmentBlock struct {
	ID         int64  `json:"id"`
	DocumentID string `json:"document_id"`
	FileName   string `json:"file_name"`
	MimeType   string `json:"mime_type"`
	SizeBytes  int64  `json:"size_bytes"`
}

type transitionResponse struct {
	// OffWorkflow: a move the configured lifecycle does not describe, offered to
	// the desk as an override. The client can label it differently.
	OffWorkflow     bool            `json:"off_workflow,omitempty"`
	ToStatus        string          `json:"to_status"`
	Label           string          `json:"label"`
	RequiresComment bool            `json:"requires_comment"`
	RequiresReason  bool            `json:"requires_reason_code"`
	ReasonCodes     json.RawMessage `json:"reason_codes,omitempty"`
}

type permissionsBlock struct {
	CanReply         bool `json:"can_reply"`
	CanReplyInternal bool `json:"can_reply_internal"`
	CanAssign        bool `json:"can_assign"`
	CanTransfer      bool `json:"can_transfer"`
	CanEscalate      bool `json:"can_escalate"`
	CanClose         bool `json:"can_close"`
	CanReopen        bool `json:"can_reopen"`
	CanUpload        bool `json:"can_upload"`
	CanEdit          bool `json:"can_edit"`
	CanGiveFeedback  bool `json:"can_give_feedback"`
}

func toResponse(t *Ticket) ticketResponse {
	out := ticketResponse{
		ID: t.PublicID, TicketNumber: t.TicketNumber, Subject: t.Subject,
		Description: t.Description.String, Status: t.Status, Priority: t.Priority,
		Source:         t.Source,
		Category:       reference{Code: t.CategoryKey, Name: t.CategoryName},
		Requester:      requesterBlock{Name: strings.TrimSpace(t.RequesterName), EmployeeCode: t.RequesterCode.String},
		RaisedOnBehalf: t.CreatedBy.Valid && t.CreatedBy.Int64 != t.RequesterID,
		SLA: slaBlock{
			IsBreached: t.IsSLABreached,
		},
		Counts:          countsBlock{Reopened: t.ReopenedCount},
		ReopenedCount:   t.ReopenedCount,
		EscalationLevel: t.EscalationLevel, IsEscalated: t.EscalationLevel > 0,
		Client: clientBlock{
			ID: t.TenantPublicID, Slug: t.TenantSlug,
			ClientCode: t.TenantClientCode.String, Name: t.TenantName,
		},
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		LastActivityAt: t.LastActivityAt,
	}

	if t.SubcategoryName.Valid {
		out.Subcategory = &reference{Name: t.SubcategoryName.String}
	}
	if t.AssigneeName.Valid && strings.TrimSpace(t.AssigneeName.String) != "" {
		out.Assignee = &reference{Name: strings.TrimSpace(t.AssigneeName.String)}
	}
	// Named only when it differs from the requester: on a ticket somebody
	// raised for themselves, "raised by" is the same person twice.
	if out.RaisedOnBehalf {
		if name := strings.TrimSpace(t.CreatorName.String); name != "" {
			out.RaisedBy = &reference{Name: name}
		}
	}
	if t.EntityName.Valid {
		out.Entity = &reference{Name: t.EntityName.String}
	}
	if t.SiteName.Valid {
		out.Site = &reference{Name: t.SiteName.String}
	}
	if t.DepartmentName.Valid {
		out.Department = &reference{Name: t.DepartmentName.String}
	}
	if t.FirstResponseDueAt.Valid {
		out.SLA.FirstResponseDueAt = &t.FirstResponseDueAt.Time
	}
	if t.ResolutionDueAt.Valid {
		out.SLA.ResolutionDueAt = &t.ResolutionDueAt.Time
	}
	if t.FirstRespondedAt.Valid {
		out.SLA.FirstRespondedAt = &t.FirstRespondedAt.Time
	}
	if t.ResolvedAt.Valid {
		out.ResolvedAt = &t.ResolvedAt.Time
	}
	if t.ClosedAt.Valid {
		out.ClosedAt = &t.ClosedAt.Time
	}
	if t.CSATScore.Valid {
		out.CSATScore = &t.CSATScore.Int64
	}
	if t.CustomFieldsJSON.Valid && t.CustomFieldsJSON.String != "" {
		_ = json.Unmarshal([]byte(t.CustomFieldsJSON.String), &out.CustomFields)
	}
	return out
}

// --- list -------------------------------------------------------------------

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)
	actor := appctx.ActorFrom(ctx)

	page := platform.ParsePage(r, Sortable, "t.last_activity_at")
	from, to := platform.QueryDates(r, "created_from", "created_to")

	filter := ListFilter{
		Scope:       h.svc.ScopeFor(actor),
		Page:        page,
		CreatedFrom: from, CreatedTo: to,
	}
	h.applyQueryFilters(r, &filter)

	// Reference filters arrive as ULIDs. Resolved through the caller's own
	// reach, so an id belonging to another client simply does not match.
	if err := h.applyReferenceFilters(r, &filter); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// Which clients this list covers. Staff who have not picked one from the
	// switcher get every client they can reach rather than the near-empty
	// platform workspace their header happens to name; a client-side user
	// always gets exactly their own. See appctx.Reach.
	applyReach(&filter, appctx.Reach(ctx))

	// An explicit ?client= narrows a cross-client list to one client, so the
	// admin's Tickets section and the switcher reach the same rows by two
	// routes without a second endpoint.
	if raw := strings.TrimSpace(r.URL.Query().Get("client")); raw != "" {
		id, err := h.svc.Repo().ResolveClientID(ctx, raw)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("client", "NOT_FOUND", "That client was not found."))
			return
		}
		if actor == nil || !actor.MayAccessTenant(*id) {
			httpx.Fail(w, r, httpx.ErrForbidden("That client is not available to you."))
			return
		}
		filter.TenantIDs, filter.AllTenants = []int64{*id}, false
	}

	rows, total, err := h.svc.Repo().List(ctx, tenantID, filter)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]ticketResponse, 0, len(rows))
	for i := range rows {
		out = append(out, toResponse(&rows[i]))
	}
	httpx.List(w, r, out, platform.NewMeta(page, total))
}

// applyQueryFilters reads every filter the ticket list offers off the query
// string and onto a ListFilter.
//
// Shared with the export, which is what guarantees a downloaded file contains
// exactly the rows the list was showing rather than a near-miss built by a
// second, drifting parser.
func (h *Handler) applyQueryFilters(r *http.Request, filter *ListFilter) {
	q := r.URL.Query()
	actor := appctx.ActorFrom(r.Context())

	if filter.CreatedFrom == nil && filter.CreatedTo == nil {
		filter.CreatedFrom, filter.CreatedTo = platform.QueryDates(r, "created_from", "created_to")
	}
	filter.UpdatedFrom, filter.UpdatedTo = platform.QueryDates(r, "updated_from", "updated_to")

	filter.Query = strings.TrimSpace(q.Get("q"))
	filter.Statuses = canonicalStatuses(platform.QueryStrings(r, "status"))
	filter.Priorities = platform.QueryStrings(r, "priority")
	filter.Sources = platform.QueryStrings(r, "source")

	filter.TicketNumber = strings.TrimSpace(q.Get("ticket_number"))
	filter.EmployeeName = strings.TrimSpace(q.Get("employee_name"))
	filter.EmployeeCode = strings.TrimSpace(q.Get("employee_code"))
	filter.UANNumber = strings.TrimSpace(q.Get("uan_number"))
	filter.PFNumber = strings.TrimSpace(q.Get("pf_number"))
	filter.ESICNumber = strings.TrimSpace(q.Get("esic_number"))
	filter.SLAState = strings.ToLower(strings.TrimSpace(q.Get("sla_state")))

	if v := platform.QueryBool(r, "unassigned"); v != nil {
		filter.Unassigned = *v
	}
	if v := platform.QueryBool(r, "breached"); v != nil {
		filter.Breached = *v
	}
	if v := platform.QueryBool(r, "reopened"); v != nil {
		filter.Reopened = *v
	}
	// "My tickets" for an agent means assigned to me.
	if v := platform.QueryBool(r, "mine"); v != nil && *v && actor != nil {
		filter.AssigneeIDs = []int64{actor.UserID}
	}

	// The quick-filter chips above the list. They are shorthand for filter
	// combinations rather than a dimension of their own, so they are expanded
	// here — which also means a chip and the equivalent hand-set filter produce
	// exactly the same query.
	switch strings.TrimSpace(q.Get("quick")) {
	case "my_tickets":
		if actor != nil {
			filter.AssigneeIDs = []int64{actor.UserID}
			filter.RequesterIDs = nil
		}
	case "unassigned":
		filter.Unassigned = true
	case "pending_dept":
		filter.Statuses = append(filter.Statuses, StatusPendingHelpdesk)
	case "pending_user":
		filter.Statuses = append(filter.Statuses, StatusPendingEmployee)
	case "breached":
		filter.Breached = true
	case "reopened":
		filter.Reopened = true
	case "escalated":
		filter.Escalated = true
	case "open":
		filter.Statuses = append(filter.Statuses, OpenStatuses...)
	}
}

// canonicalStatuses maps any superseded status name onto its current form, so a
// bookmarked filter or a saved view written against the old vocabulary still
// selects the right rows instead of silently matching nothing.
func canonicalStatuses(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, CanonicalStatus(strings.ToUpper(strings.TrimSpace(v))))
	}
	return out
}

// applyReferenceFilters resolves the public ids the filter bar sends — category,
// entity, site, department, assignee — into internal ids.
//
// Resolution runs through the caller's reach rather than a single tenant, both
// because a cross-client list may legitimately filter by records from several
// clients, and because an id outside that reach must narrow to nothing rather
// than be ignored — silently dropping it would widen the list instead.
func (h *Handler) applyReferenceFilters(r *http.Request, filter *ListFilter) error {
	ctx := r.Context()
	reach := appctx.Reach(ctx)

	resolve := func(table string, publicIDs []string) ([]int64, error) {
		if len(publicIDs) == 0 {
			return nil, nil
		}
		ids, err := h.svc.Repo().ResolveInReach(ctx, reach, table, publicIDs)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			// Asked for records none of which are in reach: match nothing.
			return []int64{-1}, nil
		}
		return ids, nil
	}

	type spec struct {
		param, table string
		dst          *[]int64
	}
	for _, s := range []spec{
		{"category_id", "categories", &filter.CategoryIDs},
		{"entity_id", "entities", &filter.EntityIDs},
		{"site_id", "sites", &filter.SiteIDs},
		{"department_id", "departments", &filter.DeptIDs},
		{"assignee_id", "users", &filter.AssigneeIDs},
	} {
		ids, err := resolve(s.table, platform.QueryStrings(r, s.param))
		if err != nil {
			return httpx.ErrField(s.param, "NOT_FOUND", "One of those filters was not found.")
		}
		if ids != nil {
			*s.dst = ids
		}
	}
	return nil
}

func (h *Handler) counts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	actor := appctx.ActorFrom(ctx)
	summary, err := h.svc.Repo().Summary(ctx, appctx.Reach(ctx), actor.UserID, h.svc.ScopeFor(actor))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, summary)
}

// --- create -----------------------------------------------------------------

type createRequest struct {
	CategoryID    string `json:"category_id" validate:"required,len=26"`
	SubcategoryID string `json:"subcategory_id" validate:"omitempty,len=26"`
	Subject       string `json:"subject" validate:"required,notblank,min=5,max=255,safetext"`
	Description   string `json:"description" validate:"required,notblank,max=20000"`
	// Validated against the client's priority catalogue rather than a compiled
	// enum, so a level added through configuration works without a release.
	Priority    string `json:"priority" validate:"omitempty,max=32"`
	RequesterID string `json:"requester_id" validate:"omitempty,len=26"`
	// The statutory line the query belongs to. Sent by the form as the middle
	// step of Client → Department → Entity, and validated against both ends:
	// the department has to belong to the client, and the entity to the
	// department. Optional so an integration that knows only the entity still
	// works — the department is then derived from it below.
	DepartmentID string         `json:"department_id" validate:"omitempty,len=26"`
	EntityID     string         `json:"entity_id" validate:"omitempty,len=26"`
	SiteID       string         `json:"site_id" validate:"omitempty,len=26"`
	CustomFields map[string]any `json:"custom_fields"`
	DocumentIDs  []string       `json:"document_ids" validate:"omitempty,dive,len=26"`

	// Client names the workspace this ticket belongs to, by slug or client
	// code. Only staff send it — a client-side user has exactly one workspace
	// and it is already resolved from their session.
	Client string `json:"client" validate:"omitempty,max=64"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	// A ticket belongs to exactly one client. Staff raising one from the
	// cross-client list have not necessarily chosen a client, and the header
	// then names the platform workspace — so the category, entity and requester
	// lookups below all searched somewhere the client's records are not, and
	// creation failed. `client` on the request names it explicitly; otherwise
	// the switcher does.
	tenantID := appctx.SelectedClientID(ctx)
	if ref := strings.TrimSpace(req.Client); ref != "" {
		id, err := h.svc.Repo().ResolveClientID(ctx, ref)
		if err != nil || actor == nil || !actor.MayAccessTenant(*id) {
			httpx.Fail(w, r, httpx.ErrField("client", "NOT_FOUND", "That client was not found."))
			return
		}
		tenantID = *id
	}
	if tenantID == 0 {
		httpx.Fail(w, r, httpx.ErrField("client", "REQUIRED",
			"Choose the client this ticket is for."))
		return
	}

	category, err := h.resolveCategory(r, tenantID, req.CategoryID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// The priority has to be one this client actually offers. Checked against
	// the catalogue rather than a fixed list, which is what lets a client add a
	// level without a code change — and still refuses one they have retired.
	priority := strings.ToUpper(strings.TrimSpace(req.Priority))
	if priority != "" && h.priorities != nil {
		known, err := h.priorities.KnownPriority(ctx, tenantID, priority)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrInternal(err))
			return
		}
		if known == "" {
			httpx.Fail(w, r, httpx.ErrField("priority", "INVALID",
				"That priority is not available for this client."))
			return
		}
		priority = known
	}
	if priority == "" && h.priorities != nil {
		if fallback, err := h.priorities.DefaultPriority(ctx, tenantID); err == nil {
			priority = fallback
		}
	}

	in := CreateInput{
		CategoryID: category, Subject: req.Subject, Description: req.Description,
		Priority: priority, CustomFields: req.CustomFields,
		RequesterID: actor.UserID,
	}

	// Raising on behalf of an employee requires the ability to see them.
	if req.RequesterID != "" {
		if !actor.CanAny("user.view.all", "user.view.scope") {
			httpx.Fail(w, r, httpx.ErrForbidden("You cannot raise a ticket on behalf of another user."))
			return
		}
		requester, err := h.users.ByPublicID(ctx, tenantID, req.RequesterID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("requester_id", "NOT_FOUND", "That user was not found."))
			return
		}
		in.RequesterID = requester.ID
	}

	if req.SubcategoryID != "" {
		id, err := h.resolveSubcategory(r, tenantID, category, req.SubcategoryID)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		in.SubcategoryID = &id
	}
	// Client → Department → Entity, checked here and not only in the browser.
	// A cascading form makes the wrong combination hard to pick; it does not
	// make it impossible to send, and an entity filed under another client's
	// department would route the ticket to a desk that cannot see it.
	var departmentID *int64
	if req.DepartmentID != "" {
		id, err := h.resolveByPublicID(r, "departments", tenantID, req.DepartmentID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("department_id", "NOT_FOUND",
				"That department was not found for this client."))
			return
		}
		departmentID = &id
	}

	if req.EntityID != "" {
		id, err := h.resolveByPublicID(r, "entities", tenantID, req.EntityID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("entity_id", "NOT_FOUND", "That entity was not found."))
			return
		}

		// The entity carries its own department, so the pair is checked against
		// the record rather than trusted from the request.
		var entityDept sql.NullInt64
		if err := h.svc.Repo().db.Primary.GetContext(ctx, &entityDept,
			`SELECT department_id FROM entities WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
			tenantID, id); err != nil {
			httpx.Fail(w, r, httpx.ErrInternal(err))
			return
		}
		if departmentID != nil && entityDept.Valid && entityDept.Int64 != *departmentID {
			httpx.Fail(w, r, httpx.ErrField("entity_id", "INVALID",
				"That entity does not belong to the chosen department."))
			return
		}
		// Derived when the caller named only the entity, so the ticket is
		// routed to a statutory line either way.
		if departmentID == nil && entityDept.Valid {
			departmentID = &entityDept.Int64
		}

		in.EntityID = &id
	}
	if departmentID != nil {
		in.DepartmentID = departmentID
	}
	if req.SiteID != "" {
		id, err := h.resolveByPublicID(r, "sites", tenantID, req.SiteID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("site_id", "NOT_FOUND", "That site was not found."))
			return
		}
		in.SiteID = &id
	}
	// Everything the requester sent, from either half of the form.
	//
	// `document_ids` is the dropzone at the foot of the form. The category's own
	// FILE fields — "Supporting document" — put their uploads in `custom_fields`
	// instead, and nothing linked those to the ticket: the file was stored and
	// then unreachable, with a bare identifier shown where its name should be.
	// Both are attachments, so both are attached. See repository_form_files.go.
	refs := req.DocumentIDs
	if fileKeys, err := h.svc.Repo().FileFieldKeys(ctx, tenantID, category); err == nil {
		refs = mergeRefs(refs, DocumentRefsInFields(req.CustomFields, fileKeys))
	}
	if len(refs) > 0 {
		ids, err := h.resolveDocuments(r, tenantID, refs)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		in.DocumentIDs = ids
	}

	created, err := h.svc.Create(ctx, tenantID, in, actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Created(w, r, toResponse(created))
}

// --- detail -----------------------------------------------------------------

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	out := toResponse(t)
	// Only the detail read selects the requester's statutory columns, so only
	// the detail response carries them.
	out.Requester = withRequesterDetail(out.Requester, t)

	// The files that came with the ticket itself, so the opening post shows
	// what was sent with it rather than leaving the reader to find them in
	// another tab.
	if opening, err := h.svc.Repo().OpeningAttachments(ctx, t.TenantID, t.ID); err == nil {
		for _, a := range opening {
			out.Attachments = append(out.Attachments, attachmentBlock{
				ID: a.ID, DocumentID: a.DocumentPubID, FileName: a.OriginalName,
				MimeType: a.MimeType, SizeBytes: a.SizeBytes,
			})
		}
	}

	// Who is following it. The rail asked for this and the detail never sent
	// it, so the watcher list rendered empty however many watchers a ticket
	// actually had.
	if watchers, err := h.svc.Repo().Watchers(ctx, t.TenantID, t.ID); err == nil {
		for _, row := range watchers {
			out.Watchers = append(out.Watchers, watcherRef{
				ID: row.PublicID, Name: row.Name, Email: row.Email, Reason: row.Reason,
			})
		}
	}

	// The client renders its action bar from these rather than reimplementing
	// the workflow, so a client-specific lifecycle needs no frontend change.
	// The desk sees every status; a client-side user sees the configured path.
	// See TransitionsFor.
	transitions, err := h.svc.Repo().TransitionsFor(
		ctx, t.TenantID, t.CategoryID, t.Status, actor.Roles, actor.IsStaff || actor.IsSuperAdmin)
	if err == nil && actor.Can("ticket.status.change") {
		for _, tr := range transitions {
			out.AllowedTransitions = append(out.AllowedTransitions, transitionResponse{
				ToStatus: tr.ToStatus, Label: tr.Label.String,
				RequiresComment: tr.RequiresComment, RequiresReason: tr.RequiresReason,
				ReasonCodes: rawJSON(tr.ReasonCodesJSON.String),
				OffWorkflow: tr.OffWorkflow,
			})
		}
	}

	out.Permissions = &permissionsBlock{
		CanReply:         h.canReplyOn(r, t, actor),
		CanReplyInternal: actor.Can("ticket.reply.internal"),
		CanAssign:        actor.Can("ticket.assign"),
		CanTransfer:      actor.Can("ticket.transfer"),
		CanEscalate:      actor.Can("ticket.escalate"),
		CanClose:         actor.Can("ticket.close"),
		CanReopen:        h.canReopen(r, t, actor),
		CanUpload:        actor.Can("document.upload"),
		CanEdit:          actor.Can("ticket.update"),
		CanGiveFeedback:  actor.Can("ticket.feedback") && t.RequesterID == actor.UserID && t.ResolvedAt.Valid,
	}

	httpx.OK(w, r, out)
}

// canReplyOn decides whether the caller may post a public reply on this ticket:
// the general grant, or a per-entity reply grant on the entity the ticket hangs
// off.
func (h *Handler) canReplyOn(r *http.Request, t *Ticket, actor *appctx.Actor) bool {
	if actor.Can("ticket.reply.public") {
		return true
	}
	if !t.EntityID.Valid {
		return false
	}
	ok, err := h.svc.Repo().HasEntityReplyGrant(r.Context(), t.TenantID, t.EntityID.Int64, actor.UserID)
	return err == nil && ok
}

// canReopen applies the reopen window: staff may always reopen, but the employee
// who raised it only within the client's configured period.
func (h *Handler) canReopen(r *http.Request, t *Ticket, actor *appctx.Actor) bool {
	if !actor.Can("ticket.reopen") {
		return false
	}
	reopenable := false
	for _, s := range ReopenableStatuses {
		if t.Status == s {
			reopenable = true
		}
	}
	if !reopenable {
		return false
	}
	if actor.Can("ticket.view.all") {
		return true
	}

	days := h.svc.ReopenWindowDays(r.Context(), t.TenantID)
	if days <= 0 {
		return true
	}
	closedAt := t.ClosedAt
	if !closedAt.Valid {
		closedAt = t.ResolvedAt
	}
	if !closedAt.Valid {
		return true
	}
	return time.Since(closedAt.Time) <= time.Duration(days)*24*time.Hour
}

func (h *Handler) timeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	rows, err := h.svc.Repo().Timeline(ctx, t.TenantID, t.ID, h.svc.CanSeeInternal(actor))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	// `visibility` is on the wire because the reader renders an internal entry
	// differently from a public one, and had no way to tell them apart — the
	// column was filtered on and then dropped from the response.
	type entry struct {
		ID         string          `json:"id"`
		EventType  string          `json:"event_type"`
		Actor      string          `json:"actor"`
		ActorRole  string          `json:"actor_role,omitempty"`
		Visibility string          `json:"visibility"`
		Summary    string          `json:"summary"`
		Detail     json.RawMessage `json:"detail,omitempty"`
		CreatedAt  time.Time       `json:"created_at"`
	}

	out := make([]entry, 0, len(rows))
	for _, row := range rows {
		out = append(out, entry{
			ID: row.PublicID, EventType: row.EventType,
			Actor:      strings.TrimSpace(row.ActorName.String),
			ActorRole:  strings.TrimSpace(row.ActorRole.String),
			Visibility: row.Visibility,
			Summary:    row.Summary,
			Detail:     rawJSON(row.DetailJSON.String), CreatedAt: row.CreatedAt,
		})
	}
	httpx.OK(w, r, out)
}

func rawJSON(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" || !json.Valid([]byte(s)) {
		return nil
	}
	return json.RawMessage(s)
}

// --- helpers ----------------------------------------------------------------

func (h *Handler) resolveCategory(r *http.Request, tenantID int64, publicID string) (int64, error) {
	id, err := h.resolveByPublicID(r, "categories", tenantID, publicID)
	if err != nil {
		return 0, httpx.ErrField("category_id", "NOT_FOUND", "That category was not found.")
	}
	return id, nil
}

// resolveSubcategory also verifies the subcategory belongs to the chosen
// category, so a request cannot pair a PF subcategory with an ESI ticket.
func (h *Handler) resolveSubcategory(r *http.Request, tenantID, categoryID int64, publicID string) (int64, error) {
	var row struct {
		ID       int64  `db:"id"`
		ParentID *int64 `db:"parent_id"`
	}
	err := h.svc.Repo().db.Primary.GetContext(r.Context(), &row, `
		SELECT id, parent_id FROM categories
		WHERE tenant_id = ? AND public_id = ? AND is_subcategory = 1 AND deleted_at IS NULL`,
		tenantID, publicID)
	if err != nil {
		return 0, httpx.ErrField("subcategory_id", "NOT_FOUND", "That subcategory was not found.")
	}
	if row.ParentID == nil || *row.ParentID != categoryID {
		return 0, httpx.ErrField("subcategory_id", "MISMATCH",
			"That subcategory does not belong to the chosen category.")
	}
	return row.ID, nil
}

func (h *Handler) resolveByPublicID(r *http.Request, table string, tenantID int64, publicID string) (int64, error) {
	switch table {
	case "categories", "entities", "sites", "departments", "users":
	default:
		return 0, errors.New("unsupported table")
	}

	var id int64
	err := h.svc.Repo().db.Primary.GetContext(r.Context(), &id,
		`SELECT id FROM `+table+` WHERE tenant_id = ? AND public_id = ? AND deleted_at IS NULL`,
		tenantID, publicID)
	return id, err
}

func (h *Handler) resolveDocuments(r *http.Request, tenantID int64, publicIDs []string) ([]int64, error) {
	// Nothing to resolve is not an error, and `IN ()` is not valid SQL.
	if len(publicIDs) == 0 {
		return nil, nil
	}

	ids := []int64{}
	args := append([]any{tenantID}, platform.StringArgs(publicIDs)...)
	q := `SELECT id FROM documents WHERE tenant_id = ? AND public_id IN (` +
		platform.Placeholders(len(publicIDs)) + `) AND deleted_at IS NULL`

	if err := h.svc.Repo().db.Primary.SelectContext(r.Context(), &ids, q, args...); err != nil {
		return nil, httpx.ErrInternal(err)
	}
	if len(ids) != len(publicIDs) {
		return nil, httpx.ErrField("document_ids", "NOT_FOUND",
			"One or more attachments were not found.")
	}
	return ids, nil
}
