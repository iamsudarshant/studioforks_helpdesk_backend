package ticket

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// PlatformRoutes mounts the cross-client ticket surface for the admin portal.
//
// The normal /tickets routes are tenant-scoped: an agent works one client at a
// time. The admin's Tickets section lists every client's tickets in one view,
// and keeps the full filter set the per-client list offers.
//
// The gate here is ticket.view.all, which is exactly the grant that separates
// staff from a scoped partner or employee: a partner holding ticket.view.scope
// must not be able to switch into a cross-client list by guessing a URL.
func (h *Handler) PlatformRoutes(r chi.Router) {
	r.Route("/tickets", func(r chi.Router) {
		r.With(middleware.RequirePermission("ticket.view.all")).Get("/", h.listAll)
	})
}

// listAll answers the admin Tickets section: every client's tickets, with the
// same filters the per-client list has, plus an optional `client` filter
// (slug or client code) to narrow the view to one client.
//
// Opening a ticket and replying reuse the normal ticket endpoints: the frontend
// sends the ticket's client as the X-Tenant-Slug header, so there is exactly
// one reply and one permission path, not two.
func (h *Handler) listAll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor == nil || !actor.Can("ticket.view.all") {
		httpx.Fail(w, r, httpx.ErrForbidden("This view is for ComplyDesk staff only."))
		return
	}

	page := platform.ParsePage(r, Sortable, "t.last_activity_at")
	from, to := platform.QueryDates(r, "created_from", "created_to")

	filter := ListFilter{
		Query:       strings.TrimSpace(r.URL.Query().Get("q")),
		Statuses:    platform.QueryStrings(r, "status"),
		Priorities:  platform.QueryStrings(r, "priority"),
		CreatedFrom: from, CreatedTo: to,
		Scope: Scope{},
		Page:  page,
	}
	if v := platform.QueryBool(r, "unassigned"); v != nil {
		filter.Unassigned = *v
	}
	if v := platform.QueryBool(r, "breached"); v != nil {
		filter.Breached = *v
	}
	if v := platform.QueryBool(r, "reopened"); v != nil {
		filter.Reopened = *v
	}
	if v := platform.QueryBool(r, "mine"); v != nil && *v {
		filter.AssigneeIDs = []int64{actor.UserID}
	}

	// Optional narrowing to one client by slug or client code.
	if raw := strings.TrimSpace(r.URL.Query().Get("client")); raw != "" {
		id, err := h.svc.Repo().ResolveClientID(ctx, raw)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("client", "NOT_FOUND", "That client was not found."))
			return
		}
		filter.TenantIDs = []int64{*id}
	} else {
		filter.AllTenants = true
	}

	rows, total, err := h.svc.Repo().List(ctx, 0, filter)
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
