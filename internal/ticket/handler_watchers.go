package ticket

// Watchers, and one person's ticket history.
//
// Both routes were declared by the frontend and never built, so the browser
// fell back to a mock: the watcher panel had nothing to list and the Tickets tab
// on a user's record rendered blank against real data. They live together
// because they are the same shape of question — "which tickets, for whom" — and
// both answer it through the caller's existing scope rather than a second one.

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// UserRoutes mounts the routes that hang off a *user* rather than a ticket.
//
// Declared by the ticket package because the query, the scope rules and the row
// shape are all the ticket list's; mounting it from the user package would mean
// a second implementation of the same thing, which is how the two ended up
// disagreeing before.
func (h *Handler) UserRoutes(r chi.Router) {
	read := middleware.RequireAnyPermission("ticket.view.own", "ticket.view.scope", "ticket.view.all")
	r.With(read).Get("/users/{id}/tickets", h.userTickets)
}

// departmentAgents answers the second half of the transfer picker: having
// chosen a department, who in it can take the ticket.
//
// Deliberately the same query the assign picker runs, with the destination
// department instead of the ticket's current one. It used to be its own SQL,
// which is how the two came to answer differently: the transfer list demanded
// an explicit mapping and so hid every generalist, while the assign list asked
// for no mapping at all and so offered the wrong line's desk.
func (h *Handler) departmentAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), appctx.ActorFrom(ctx))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ref := strings.TrimSpace(r.URL.Query().Get("department_id"))
	if ref == "" {
		httpx.Fail(w, r, httpx.ErrField("department_id", "REQUIRED",
			"Choose a department first."))
		return
	}
	departmentID, err := h.resolveByPublicID(r, "departments", t.TenantID, ref)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrField("department_id", "NOT_FOUND",
			"That department was not found for this client."))
		return
	}

	rows, err := h.users.AssignableStaff(ctx, t.TenantID, departmentID,
		strings.TrimSpace(r.URL.Query().Get("q")))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]DepartmentAgent, 0, len(rows))
	for _, row := range rows {
		out = append(out, DepartmentAgent{
			ID: row.PublicID, Name: row.Name, Email: row.Email, Role: row.RoleName,
		})
	}
	httpx.OK(w, r, out)
}

// watcherCandidates lists the people who may be added as watchers on a ticket.
//
// The panel used to borrow the assign picker's list, which is gated on
// `ticket.assign` — so an agent or an administrator without that permission was
// shown an empty dropdown and could not add anybody. Watching is not an
// assignment, and it needs its own answer.
//
// Scoped to the ticket's own client, not the request's tenant header: staff
// working from the cross-client list have not switched their session, and the
// candidates must still be that client's people plus the desk that covers them.
// Employees are excluded — a watcher receives the ticket's notifications, and
// one employee following another's PF query is a disclosure, not a feature.
//
// Deliberately the same query the assign picker runs, minus the department
// narrowing: a watcher does not have to work the line to follow the ticket.
func (h *Handler) watcherCandidates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Loading the ticket first is the access check, and the tenant isolation:
	// `Load` refuses a ticket outside the caller's scope, so this cannot be used
	// to enumerate another client's roster.
	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), appctx.ActorFrom(ctx))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	rows, err := h.users.AssignableStaff(ctx, t.TenantID, 0,
		strings.TrimSpace(r.URL.Query().Get("q")))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	// Already watching is not a candidate: offering somebody who is on the list
	// below the dropdown reads as a broken control.
	watching := map[string]bool{}
	if current, err := h.svc.Repo().Watchers(ctx, t.TenantID, t.ID); err == nil {
		for _, row := range current {
			watching[row.PublicID] = true
		}
	}

	out := make([]DepartmentAgent, 0, len(rows))
	for _, row := range rows {
		if watching[row.PublicID] {
			continue
		}
		out = append(out, DepartmentAgent{
			ID: row.PublicID, Name: row.Name, Email: row.Email, Role: row.RoleName,
		})
	}
	httpx.OK(w, r, out)
}

type watcherResponse struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Email  string `json:"email,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Whether the caller may remove this watcher. Computed here because the
	// rule — you can always stop watching yourself — is the server's.
	CanRemove bool `json:"can_remove"`
}

// listWatchers answers the panel on the ticket detail.
//
// Loading the ticket first is the access check: `Load` refuses a ticket outside
// the caller's scope with NOT_FOUND, so the watcher list cannot be used to probe
// for tickets they cannot see.
func (h *Handler) listWatchers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	rows, err := h.svc.Repo().Watchers(ctx, t.TenantID, t.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	// Managing other people's watching is an assignment-level action; removing
	// yourself never is.
	mayManage := actor != nil && actor.CanAny("ticket.assign", "ticket.update")

	out := make([]watcherResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, watcherResponse{
			ID: row.PublicID, Name: row.Name, Email: row.Email,
			Reason:    row.Reason,
			CanRemove: mayManage || (actor != nil && row.UserID == actor.UserID),
		})
	}
	httpx.OK(w, r, out)
}

type addWatcherRequest struct {
	// UserID is optional: omitted, the caller adds themselves, which is what
	// the "Watch this ticket" button does.
	UserID string `json:"user_id" validate:"omitempty,len=26"`
	Reason string `json:"reason" validate:"omitempty,max=64"`
}

func (h *Handler) addWatcher(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var req addWatcherRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	target := actor.UserID
	if ref := strings.TrimSpace(req.UserID); ref != "" && ref != actor.PublicID {
		// Adding somebody else is an act of assignment: it puts a ticket on
		// their radar and sends them its notifications.
		if !actor.CanAny("ticket.assign", "ticket.update") {
			httpx.Fail(w, r, httpx.ErrForbidden("You cannot add other people as watchers."))
			return
		}
		// Resolved inside the ticket's own client, so a watcher can never be
		// somebody who would then be unable to open what they are watching.
		id, err := h.resolveByPublicID(r, "users", t.TenantID, ref)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("user_id", "NOT_FOUND",
				"That user was not found in this client."))
			return
		}
		target = id
	}

	if err := h.svc.Repo().AddWatcher(ctx, t.TenantID, t.ID, target, req.Reason); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "ticket.watcher_added", EntityType: "ticket", EntityID: &t.ID,
		EntityPublicID: t.PublicID,
		After:          map[string]any{"user_id": target, "reason": req.Reason},
	})

	h.listWatchers(w, r)
}

func (h *Handler) removeWatcher(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// The path names the watcher; a body naming them is accepted too, because
	// several HTTP clients cannot attach one to a DELETE and the panel sent
	// `{"user_id": …}` against a route that only read the path — so removing
	// somebody else silently removed the caller instead.
	ref := chi.URLParam(r, "userId")
	if ref == "" {
		var body addWatcherRequest
		_ = httpx.Decode(r, &body)
		ref = strings.TrimSpace(body.UserID)
	}

	target := actor.UserID
	if ref != "" && ref != "me" && ref != actor.PublicID {
		if !actor.CanAny("ticket.assign", "ticket.update") {
			httpx.Fail(w, r, httpx.ErrForbidden("You cannot remove other watchers."))
			return
		}
		id, err := h.resolveByPublicID(r, "users", t.TenantID, ref)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("user_id", "NOT_FOUND", "That user was not found."))
			return
		}
		target = id
	}

	if err := h.svc.Repo().RemoveWatcher(ctx, t.TenantID, t.ID, target); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "ticket.watcher_removed", EntityType: "ticket", EntityID: &t.ID,
		EntityPublicID: t.PublicID,
		Before:         map[string]any{"user_id": target},
	})

	h.listWatchers(w, r)
}

// userTickets answers the Tickets tab on a person's record.
//
// Deliberately the same query the main list uses, with the requester pinned:
// the tab has to obey the caller's scope, paginate, and carry the same columns —
// status, priority, department, entity, dates — and a second query would be a
// second set of rules to keep in step.
//
// "Their tickets" means the ones they raised. A ticket they are merely assigned
// belongs on their queue, not on their record.
func (h *Handler) userTickets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	// Resolved across the caller's reach rather than the header's tenant: staff
	// open a user's record from the cross-client roster without having switched
	// to that client, so a pinned lookup answers "not found" for every row.
	target, err := h.users.ByPublicIDInReach(ctx, appctx.Reach(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That user"))
		return
	}

	page := platform.ParsePage(r, Sortable, "t.last_activity_at")
	filter := ListFilter{
		Scope:        h.svc.ScopeFor(actor),
		Page:         page,
		RequesterIDs: []int64{target.ID},
	}
	// The rest of the query string still applies, so the tab can be filtered
	// and sorted exactly like the main list.
	h.applyQueryFilters(r, &filter)
	// Reasserted after the query-string pass, so a `requester_id` in the URL
	// cannot widen the tab to somebody else's tickets.
	filter.RequesterIDs = []int64{target.ID}

	// The same reach translation every other list uses, so this tab narrows to
	// the caller's clients identically rather than inventing a second rule.
	applyReach(&filter, appctx.Reach(ctx))

	rows, total, err := h.svc.Repo().List(ctx, appctx.TenantID(ctx), filter)
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
