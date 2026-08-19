package user

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// assignmentRoutes mounts the entity / site / department assignment surface.
//
// The verbs are gated on org.update and user.update together: an admin or agent
// assigning a partner to an entity is changing both who the partner is scoped
// to and what the entity's ticket view is — the two halves of one decision.
func (h *Handler) assignmentRoutes(r chi.Router) {
	manage := middleware.RequireAnyPermission("org.update", "user.update")
	view := middleware.RequireAnyPermission("org.update", "user.update", "user.view.scope", "user.view.all")

	r.Route("/assignments", func(r chi.Router) {
		r.With(view).Get("/entities/{entityId}", h.listEntityAssignments)
		r.With(manage).Put("/entities/{entityId}/users/{userId}", h.assignEntityUser)
		r.With(manage).Patch("/entities/{entityId}/users/{userId}", h.setEntityReplyRights)
		r.With(manage).Delete("/entities/{entityId}/users/{userId}", h.revokeEntityUser)

		r.With(view).Get("/sites/{siteId}", h.listSiteAssignments)
		r.With(manage).Put("/sites/{siteId}/users/{userId}", h.assignSiteUser)
		r.With(manage).Delete("/sites/{siteId}/users/{userId}", h.revokeSiteUser)

		r.With(view).Get("/departments/{departmentId}", h.listDepartmentAssignments)
		r.With(manage).Put("/departments/{departmentId}/users/{userId}", h.assignDepartmentUser)
		r.With(manage).Delete("/departments/{departmentId}/users/{userId}", h.revokeDepartmentUser)
	})
}

// --- shared helpers ---------------------------------------------------------

// actorID returns the caller's user id as a pointer, or nil.
func actorID(r *http.Request) *int64 {
	if actor := appctx.ActorFrom(r.Context()); actor != nil {
		id := actor.UserID
		return &id
	}
	return nil
}

// resolveEntity turns an entity public id into its internal id within the
// caller's tenant. The org repository does the ownership check.
func (h *Handler) resolveEntity(ctx context.Context, tenantID int64, publicID string) (int64, error) {
	ids, err := h.org.ResolveEntityIDs(ctx, tenantID, []string{publicID})
	if err != nil || len(ids) != 1 {
		return 0, httpx.ErrNotFound("That entity")
	}
	return ids[0], nil
}

func (h *Handler) resolveSite(ctx context.Context, tenantID int64, publicID string) (int64, error) {
	ids, err := h.org.ResolveSiteIDs(ctx, tenantID, []string{publicID})
	if err != nil || len(ids) != 1 {
		return 0, httpx.ErrNotFound("That site")
	}
	return ids[0], nil
}

func (h *Handler) resolveDepartment(ctx context.Context, tenantID int64, publicID string) (int64, error) {
	ids, err := h.org.ResolveDepartmentIDs(ctx, tenantID, []string{publicID})
	if err != nil || len(ids) != 1 {
		return 0, httpx.ErrNotFound("That department")
	}
	return ids[0], nil
}

// resolveAssignableUser loads the user being assigned and verifies they are a
// client-side user of this tenant (partners, and the agents an admin may scope).
func (h *Handler) resolveAssignableUser(ctx context.Context, tenantID int64, publicID string) (*User, error) {
	u, err := h.repo.ByPublicID(ctx, tenantID, publicID)
	if err != nil {
		return nil, httpx.ErrNotFound("That user")
	}
	return u, nil
}

// --- entity assignments -----------------------------------------------------

func (h *Handler) listEntityAssignments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	entityID, err := h.resolveEntity(ctx, appctx.TenantID(ctx), chi.URLParam(r, "entityId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	rows, err := h.repo.EntityAssignments(ctx, appctx.TenantID(ctx), entityID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, map[string]any{"items": rows})
}

func (h *Handler) assignEntityUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CanReply bool `json:"can_reply"`
	}
	_ = httpx.Decode(r, &req)

	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	entityID, err := h.resolveEntity(ctx, tenantID, chi.URLParam(r, "entityId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	target, err := h.resolveAssignableUser(ctx, tenantID, chi.URLParam(r, "userId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	actorID := actorID(r)
	if err := h.repo.AssignEntityUser(ctx, tenantID, entityID, target.ID, req.CanReply, actorID); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionEntityAssignment, EntityType: "entity",
		EntityID: &entityID,
		After:    map[string]any{"user": target.PublicID, "can_reply": req.CanReply},
	})
	httpx.OK(w, r, map[string]any{
		"message":   target.FullName() + " assigned to this entity.",
		"can_reply": req.CanReply,
		"user_id":   target.PublicID,
	})
}

func (h *Handler) setEntityReplyRights(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CanReply bool `json:"can_reply"`
	}
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	entityID, err := h.resolveEntity(ctx, tenantID, chi.URLParam(r, "entityId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	target, err := h.resolveAssignableUser(ctx, tenantID, chi.URLParam(r, "userId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.repo.SetEntityReplyRights(ctx, tenantID, entityID, target.ID, req.CanReply); err != nil {
		if platform.IsNotFound(err) {
			httpx.Fail(w, r, httpx.ErrNotFound("That assignment"))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionEntityReplyGrant, EntityType: "entity",
		EntityID: &entityID,
		After:    map[string]any{"user": target.PublicID, "can_reply": req.CanReply},
	})
	httpx.OK(w, r, map[string]any{
		"message":   "Reply rights updated.",
		"can_reply": req.CanReply,
	})
}

func (h *Handler) revokeEntityUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	entityID, err := h.resolveEntity(ctx, tenantID, chi.URLParam(r, "entityId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	target, err := h.resolveAssignableUser(ctx, tenantID, chi.URLParam(r, "userId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.repo.RevokeEntityUser(ctx, tenantID, entityID, target.ID); err != nil {
		if platform.IsNotFound(err) {
			httpx.Fail(w, r, httpx.ErrNotFound("That assignment"))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "entity.revoked", EntityType: "entity", EntityID: &entityID,
		After: map[string]any{"user": target.PublicID},
	})
	httpx.OK(w, r, map[string]any{"message": target.FullName() + " removed from this entity."})
}

// --- site assignments -------------------------------------------------------

func (h *Handler) listSiteAssignments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)
	siteID, err := h.resolveSite(ctx, tenantID, chi.URLParam(r, "siteId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	rows, err := h.repo.SiteAssignments(ctx, tenantID, siteID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, map[string]any{"items": rows})
}

func (h *Handler) assignSiteUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	siteID, err := h.resolveSite(ctx, tenantID, chi.URLParam(r, "siteId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	target, err := h.resolveAssignableUser(ctx, tenantID, chi.URLParam(r, "userId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	actorID := actorID(r)
	if err := h.repo.AssignSiteUser(ctx, tenantID, siteID, target.ID, actorID); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionSiteAssignment, EntityType: "site", EntityID: &siteID,
		After: map[string]any{"user": target.PublicID},
	})
	httpx.OK(w, r, map[string]any{"message": target.FullName() + " assigned to this site."})
}

func (h *Handler) revokeSiteUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	siteID, err := h.resolveSite(ctx, tenantID, chi.URLParam(r, "siteId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	target, err := h.resolveAssignableUser(ctx, tenantID, chi.URLParam(r, "userId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.repo.RevokeSiteUser(ctx, tenantID, siteID, target.ID); err != nil {
		if platform.IsNotFound(err) {
			httpx.Fail(w, r, httpx.ErrNotFound("That assignment"))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "site.revoked", EntityType: "site", EntityID: &siteID,
		After: map[string]any{"user": target.PublicID},
	})
	httpx.OK(w, r, map[string]any{"message": target.FullName() + " removed from this site."})
}

// --- department assignments -------------------------------------------------

func (h *Handler) listDepartmentAssignments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)
	departmentID, err := h.resolveDepartment(ctx, tenantID, chi.URLParam(r, "departmentId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	rows, err := h.repo.DepartmentAssignments(ctx, tenantID, departmentID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, map[string]any{"items": rows})
}

func (h *Handler) assignDepartmentUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	departmentID, err := h.resolveDepartment(ctx, tenantID, chi.URLParam(r, "departmentId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	target, err := h.resolveAssignableUser(ctx, tenantID, chi.URLParam(r, "userId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	actorID := actorID(r)
	if err := h.repo.AssignDepartmentUser(ctx, tenantID, departmentID, target.ID, actorID); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionDeptAssignment, EntityType: "department",
		EntityID: &departmentID, After: map[string]any{"user": target.PublicID},
	})
	httpx.OK(w, r, map[string]any{"message": target.FullName() + " assigned to this department."})
}

func (h *Handler) revokeDepartmentUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	departmentID, err := h.resolveDepartment(ctx, tenantID, chi.URLParam(r, "departmentId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	target, err := h.resolveAssignableUser(ctx, tenantID, chi.URLParam(r, "userId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.repo.RevokeDepartmentUser(ctx, tenantID, departmentID, target.ID); err != nil {
		if platform.IsNotFound(err) {
			httpx.Fail(w, r, httpx.ErrNotFound("That assignment"))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "department.revoked", EntityType: "department",
		EntityID: &departmentID, After: map[string]any{"user": target.PublicID},
	})
	httpx.OK(w, r, map[string]any{"message": target.FullName() + " removed from this department."})
}

// --- user-centric view ------------------------------------------------------

// listUserAssignments returns every branch the user is scoped to, grouped by
// type. Entities carry their reply-rights grant; sites and departments are a
// plain scope membership.
func (h *Handler) listUserAssignments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	target, err := h.resolveAssignableUser(ctx, tenantID, chi.URLParam(r, "userId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	entities, err := h.repo.EntityAssignmentsForUser(ctx, tenantID, target.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	sites, err := h.repo.SiteAssignmentsForUser(ctx, tenantID, target.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	departments, err := h.repo.DepartmentAssignmentsForUser(ctx, tenantID, target.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	httpx.OK(w, r, map[string]any{
		"entities":    entities,
		"sites":       sites,
		"departments": departments,
	})
}
