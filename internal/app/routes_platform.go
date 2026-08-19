package app

import (
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/tenant"
)

// mountStaffRoutes mounts the endpoints ComplyDesk staff need before they have
// picked a client to work on.
//
// These deliberately sit outside the tenant-scoped group: a staff user signing
// in has to list the clients before they can send an X-Tenant-Slug for one.
//
// The path stays `/karma` for backward compatibility with issued tokens and
// existing clients of the API.
func (a *App) mountStaffRoutes(r chi.Router) {
	r.Route("/karma", func(r chi.Router) {
		r.Use(a.TenantMW.Optional)
		r.Use(a.Authenticator.Require)

		// The client switcher: every client, with the caller's own flagged.
		r.Get("/clients", a.assignedClients)

		// Recording which agent owns which client.
		r.With(middleware.RequirePermission("agent.assign")).
			Put("/clients/{clientId}/agents/{agentId}", a.assignAgent)
		r.With(middleware.RequirePermission("agent.assign")).
			Delete("/clients/{clientId}/agents/{agentId}", a.revokeAgent)
		r.With(middleware.RequireAnyPermission("agent.view", "agent.assign")).
			Get("/clients/{clientId}/agents", a.clientAgents)
	})
}

type clientSummary struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	ClientCode string `json:"client_code,omitempty"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	// IsOwned marks a client this agent is responsible for. Every staff user can
	// work every client, so this drives ordering and the "my clients" filter
	// rather than access.
	IsOwned   bool `json:"is_owned"`
	IsPrimary bool `json:"is_primary"`
}

// assignedClients answers "which clients may I work on?" — the list the staff
// portals' switcher renders.
//
// Both staff roles get every client, because both work across the platform. The
// difference is ownership, not reach: an agent's own clients are flagged
// `is_owned` so the switcher can lead with them, and `owned_count` lets the UI
// offer a "my clients" filter without a second request.
func (a *App) assignedClients(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}
	if !actor.IsStaff {
		// A client-side user has exactly one workspace and no switcher.
		httpx.Fail(w, r, httpx.ErrForbidden("This is available to ComplyDesk staff only."))
		return
	}

	rows, _, err := a.TenantRepo.List(ctx, tenantListAll())
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	// Which of them this agent owns. A super admin owns none by design — their
	// remit is the whole platform — so the lookup is skipped for them.
	owned := map[int64]bool{}
	primary := map[int64]bool{}
	if !actor.IsSuperAdmin {
		assignments, err := a.UserRepo.AssignedClients(ctx, actor.UserID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrInternal(err))
			return
		}
		for _, as := range assignments {
			owned[as.TenantID] = true
			primary[as.TenantID] = as.IsPrimary
		}
	}

	out := make([]clientSummary, 0, len(rows))
	for i := range rows {
		out = append(out, clientSummary{
			ID: rows[i].PublicID, Slug: rows[i].Slug,
			ClientCode: rows[i].ClientCode.String, Name: rows[i].Name,
			Status:    rows[i].Status,
			IsOwned:   owned[rows[i].ID],
			IsPrimary: primary[rows[i].ID],
		})
	}

	// Owned clients first, then alphabetically, so an agent's usual work is at
	// the top of the list without hiding the rest.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsOwned != out[j].IsOwned {
			return out[i].IsOwned
		}
		return out[i].Name < out[j].Name
	})

	httpx.OK(w, r, map[string]any{
		"clients": out,
		// Retained for compatibility: it has always meant "reaches every client",
		// which is now true of all staff.
		"unrestricted": true,
		"owned_count":  len(owned),
	})
}

func (a *App) clientAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	client, err := a.TenantRepo.ByPublicID(ctx, chi.URLParam(r, "clientId"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That client"))
		return
	}

	rows, err := a.UserRepo.AgentsForClient(ctx, client.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	type agent struct {
		// The agent's own id, which is what the revoke route takes.
		ID        string `json:"id"`
		Name      string `json:"name"`
		Email     string `json:"email"`
		IsPrimary bool   `json:"is_primary"`
	}
	out := make([]agent, 0, len(rows))
	for _, row := range rows {
		out = append(out, agent{
			ID: row.AgentPublicID, Name: row.AgentName,
			Email: row.AgentEmail, IsPrimary: row.IsPrimary,
		})
	}
	httpx.OK(w, r, out)
}

func (a *App) assignAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IsPrimary bool `json:"is_primary"`
	}
	_ = httpx.Decode(r, &req)

	ctx := r.Context()

	client, err := a.TenantRepo.ByPublicID(ctx, chi.URLParam(r, "clientId"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That client"))
		return
	}
	// Staff live in the platform tenant, so look them up there.
	karma, err := a.TenantRepo.BySlug(ctx, "karma")
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	agent, err := a.UserRepo.ByPublicID(ctx, karma.ID, chi.URLParam(r, "agentId"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That agent"))
		return
	}

	var assignedBy *int64
	if actor := appctx.ActorFrom(ctx); actor != nil {
		id := actor.UserID
		assignedBy = &id
	}

	if err := a.UserRepo.AssignAgent(ctx, agent.ID, client.ID, req.IsPrimary, assignedBy); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	// "Owns" is the primary agent specifically; everyone else covers the client
	// without owning it, and saying otherwise misreports what was just done.
	verb := " now covers "
	if req.IsPrimary {
		verb = " now owns "
	}
	httpx.OK(w, r, map[string]any{
		"message": agent.FullName() + verb + client.Name + ".",
	})
}

func (a *App) revokeAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	client, err := a.TenantRepo.ByPublicID(ctx, chi.URLParam(r, "clientId"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That client"))
		return
	}
	karma, err := a.TenantRepo.BySlug(ctx, "karma")
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	agent, err := a.UserRepo.ByPublicID(ctx, karma.ID, chi.URLParam(r, "agentId"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That agent"))
		return
	}

	if err := a.UserRepo.RevokeAgent(ctx, agent.ID, client.ID); err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That assignment"))
		return
	}
	httpx.OK(w, r, map[string]any{
		"message": agent.FullName() + " no longer owns " + client.Name + ".",
	})
}

// tenantListAll builds the filter the staff client list uses.
func tenantListAll() tenant.ListFilter {
	return tenant.ListFilter{
		Page: platform.Page{Page: 1, PerPage: platform.MaxPerPage, SortBy: "name", SortDir: "ASC"},
	}
}
