package org

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/platform"
)

type Handler struct {
	repo    *Repository
	auditor *audit.Writer
}

func NewHandler(repo *Repository, auditor *audit.Writer) *Handler {
	return &Handler{repo: repo, auditor: auditor}
}

// Routes mounts the organisation surface.
//
// The verbs are gated separately because the three portals differ exactly here:
// admins may erase, agents may do everything but only recoverably, and partners
// may correct details without adding or removing anything.
func (h *Handler) Routes(r chi.Router) {
	create := middleware.RequireAnyPermission("org.create", "config.org")
	update := middleware.RequireAnyPermission("org.update", "config.org")
	remove := middleware.RequireAnyPermission("org.delete", "config.org")
	purge := middleware.RequirePermission("org.purge")

	r.Route("/entities", func(r chi.Router) {
		r.Get("/", h.listEntities)
		// Powers the ticket form: ?category_id=<PF or ESI> narrows the list to
		// the entities actually registered for that scheme.
		r.Get("/for-category/{categoryId}", h.entitiesForCategory)
		r.With(create).Post("/", h.createEntity)
		r.With(middleware.RequirePermission("org.create")).Post("/apply-defaults", h.applyDefaultEntities)

		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.getEntity)
			r.With(update).Patch("/", h.updateEntity)
			r.With(remove).Delete("/", h.deleteEntity)
			r.With(purge).Delete("/purge", h.purgeEntity)
			r.With(update).Post("/opt-out", h.optOutEntity)
			r.With(update).Post("/opt-in", h.optInEntity)

			// PF / ESIC registrations.
			r.Get("/registrations", h.listRegistrations)
			r.With(middleware.RequireAnyPermission("org.registration", "config.org")).
				Put("/registrations/{categoryId}", h.upsertRegistration)
			r.With(middleware.RequireAnyPermission("org.registration", "config.org")).
				Delete("/registrations/{categoryId}", h.deleteRegistration)
		})
	})

	r.Route("/sites", func(r chi.Router) {
		r.Get("/", h.listSites)
		r.With(create).Post("/", h.createSite)
		r.Get("/{id}", h.getSite)
		r.With(update).Patch("/{id}", h.updateSite)
		r.With(remove).Delete("/{id}", h.deleteSite)
	})

	r.Route("/departments", func(r chi.Router) {
		r.Get("/", h.listDepartments)
		r.With(create).Post("/", h.createDepartment)
		r.Get("/{id}", h.getDepartment)
		r.With(update).Patch("/{id}", h.updateDepartment)
		r.With(remove).Delete("/{id}", h.deleteDepartment)
	})

	// Platform-level catalogue of default entities.
	r.Get("/entity-templates", h.listTemplates)
}

// --- responses --------------------------------------------------------------

// clientRef names the client a structural record belongs to. Every org row
// carries it, because a staff user with no client selected is looking at every
// client's records at once and needs to tell them apart.
type clientRef struct {
	Name string `json:"name,omitempty"`
	Slug string `json:"slug,omitempty"`
	Code string `json:"code,omitempty"`
}

func toClientRef(name, slug, code sql.NullString) clientRef {
	return clientRef{Name: name.String, Slug: slug.String, Code: code.String}
}

type entityResponse struct {
	ID             string    `json:"id"`
	Code           string    `json:"code"`
	Name           string    `json:"name"`
	Type           string    `json:"type,omitempty"`
	DepartmentID   string    `json:"department_id"`
	DepartmentName string    `json:"department_name"`
	ParentID       *string   `json:"parent_id"`
	Address        string    `json:"address,omitempty"`
	IsActive       bool      `json:"is_active"`
	Client         clientRef `json:"client"`
}

func toEntityResponse(e Entity) entityResponse {
	return entityResponse{
		ID: e.PublicID, Code: e.Code, Name: e.Name,
		Type:           e.Type.String,
		DepartmentID:   e.DepartmentPublicID.String,
		DepartmentName: e.DepartmentName.String,
		Address:        e.Address.String, IsActive: e.IsActive,
		Client: toClientRef(e.ClientName, e.ClientSlug, e.ClientCode),
	}
}

type siteResponse struct {
	ID       string    `json:"id"`
	Code     string    `json:"code"`
	Name     string    `json:"name"`
	City     string    `json:"city,omitempty"`
	State    string    `json:"state,omitempty"`
	IsActive bool      `json:"is_active"`
	Client   clientRef `json:"client"`
}

func toSiteResponse(s Site) siteResponse {
	return siteResponse{
		ID: s.PublicID, Code: s.Code, Name: s.Name,
		City: s.City.String, State: s.State.String, IsActive: s.IsActive,
		Client: toClientRef(s.ClientName, s.ClientSlug, s.ClientCode),
	}
}

type departmentResponse struct {
	ID          string    `json:"id"`
	Code        string    `json:"code"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	IsActive    bool      `json:"is_active"`
	EntityCount int       `json:"entity_count"`
	Client      clientRef `json:"client"`
}

func toDepartmentResponse(d Department) departmentResponse {
	return departmentResponse{
		ID: d.PublicID, Code: d.Code, Name: d.Name, Type: d.Type, IsActive: d.IsActive,
		EntityCount: d.EntityCount,
		Client:      toClientRef(d.ClientName, d.ClientSlug, d.ClientCode),
	}
}

// DepartmentTypes is the standard statutory line list the picker offers. Kept
// here rather than in the database because routing and reporting both branch on
// these values, so adding one is a code change by design.
var DepartmentTypes = []string{"PF", "ESIC", "GENERAL", "PAYROLL", "HR", "OTHER"}

// listReach is the client set a listing covers.
//
// `?client=` narrows it to one, which is what lets a form's entity picker show
// a single client's records while the session still spans several. Anything
// outside the caller's reach narrows to nothing rather than being ignored.
func (h *Handler) listReach(r *http.Request) appctx.ClientReach {
	ctx := r.Context()
	reach := appctx.Reach(ctx)

	ref := strings.TrimSpace(r.URL.Query().Get("client"))
	if ref == "" {
		return reach
	}

	id, err := h.repo.ResolveClient(ctx, ref)
	if err != nil {
		return appctx.ClientReach{}
	}
	return reach.NarrowTo(id)
}

// writeClient resolves the client a new record belongs to.
//
// Reads span every client a staff user can reach, but a create has to land in
// exactly one — and "the tenant the header happens to name" is the platform
// workspace when no client is selected, which is never the intent.
//
// Three sources, in order. An explicit `client` on the request wins: an admin
// working the cross-client list names the client in the form rather than
// switching their whole session to it first. Otherwise the client selected in
// the switcher stands. With neither, this refuses and says what to do about it
// rather than silently creating the record in ComplyDesk's own workspace.
func (h *Handler) writeClient(r *http.Request, ref string) (int64, error) {
	ctx := r.Context()

	if ref = strings.TrimSpace(ref); ref != "" {
		id, err := platform.ResolveClientRef(ctx, h.repo.db.Primary, appctx.Reach(ctx), ref)
		if err != nil {
			if errors.Is(err, platform.ErrSentinelNotFound) {
				return 0, httpx.ErrField("client", "NOT_FOUND", "That client was not found.")
			}
			return 0, httpx.ErrInternal(err)
		}
		return id, nil
	}

	if id := appctx.SelectedClientID(ctx); id != 0 {
		return id, nil
	}
	return 0, httpx.ErrField("client", "REQUIRED",
		"Choose a client before adding this record.")
}

// --- entities ---------------------------------------------------------------

// scopedEntityIDs narrows a listing to the caller's assigned entities. A user
// scoped to specific entities never sees the others, even in a picker.
func scopedEntityIDs(actor *appctx.Actor) []int64 {
	if actor == nil || actor.IsSuperAdmin || actor.Can("user.view.all") || actor.Can("ticket.view.all") {
		return nil
	}
	return actor.Scopes.Entities
}

// orgFilterFrom reads the list filters off the query string.
//
// `q` searches the name and the code together; `type` and `department_id` may
// repeat or be comma-separated, because a multi-select sends whichever the
// caller's HTTP library favours and neither should be the one that silently
// filters nothing.
func (h *Handler) orgFilterFrom(r *http.Request) OrgFilter {
	query := r.URL.Query()
	filter := OrgFilter{
		Query: strings.TrimSpace(query.Get("q")),
		Types: multiValue(query["type"]),
	}

	// `status=active|inactive`. Absent means both, which is what an
	// administration screen wants — a disabled record has to be findable to be
	// re-enabled.
	switch strings.ToLower(strings.TrimSpace(query.Get("status"))) {
	case "active":
		yes := true
		filter.Active = &yes
	case "inactive":
		no := false
		filter.Active = &no
	}

	ctx := r.Context()
	reach := appctx.Reach(ctx)
	for _, ref := range multiValue(query["department_id"]) {
		if d, err := h.repo.DepartmentInReach(ctx, reach, ref); err == nil {
			filter.DepartmentIDs = append(filter.DepartmentIDs, d.ID)
		}
	}
	for _, ref := range multiValue(query["entity_id"]) {
		if e, err := h.repo.EntityInReach(ctx, reach, ref); err == nil {
			filter.EntityIDs = append(filter.EntityIDs, e.ID)
		}
	}

	// A filter naming only records outside the caller's reach must match
	// nothing, not everything — otherwise narrowing widens the result.
	if len(query["department_id"]) > 0 && len(filter.DepartmentIDs) == 0 {
		filter.DepartmentIDs = []int64{0}
	}
	if len(query["entity_id"]) > 0 && len(filter.EntityIDs) == 0 {
		filter.EntityIDs = []int64{0}
	}
	return filter
}

// multiValue accepts both `?type=PF&type=ESIC` and `?type=PF,ESIC`.
func multiValue(values []string) []string {
	out := []string{}
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func (h *Handler) listEntities(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	activeOnly := r.URL.Query().Get("include_inactive") != "true"

	// Every client when staff have not picked one, that client alone when they
	// have, and always exactly their own for a client-side user.
	page := platform.ParsePage(r, OrgSortable, "")
	rows, err := h.repo.Entities(ctx, h.listReach(r), activeOnly,
		scopedEntityIDs(appctx.ActorFrom(ctx)), page, h.orgFilterFrom(r))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]entityResponse, 0, len(rows))
	for _, e := range rows {
		out = append(out, toEntityResponse(e))
	}
	httpx.OK(w, r, out)
}

func (h *Handler) getEntity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	e, err := h.repo.EntityInReach(ctx, appctx.Reach(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That entity"))
		return
	}
	httpx.OK(w, r, toEntityResponse(*e))
}

type entityRequest struct {
	// The client this record belongs to. Required only when the caller has not
	// selected one — staff listing across clients have not, and pick it in the
	// form instead. Ignored on update: moving an entity between clients would
	// strand the tickets, users and sites already pointing at it.
	Client       string `json:"client" validate:"omitempty,max=64"`
	Code         string `json:"code" validate:"required,notblank,max=64,safetext"`
	Name         string `json:"name" validate:"required,notblank,max=191,safetext"`
	Type         string `json:"type" validate:"omitempty,max=48"`
	DepartmentID string `json:"department_id" validate:"required,len=26"`
	ParentID     string `json:"parent_id" validate:"omitempty,len=26"`
	Address      string `json:"address" validate:"omitempty,max=1000"`
	IsActive     *bool  `json:"is_active"`
}

func (h *Handler) createEntity(w http.ResponseWriter, r *http.Request) {
	var req entityRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID, err := h.writeClient(r, req.Client)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// A department is mandatory: an entity is routed to a statutory line
	// through it, so it has nowhere to go without one.
	dept, deptErr := h.repo.DepartmentByPublicID(ctx, tenantID, req.DepartmentID)
	if deptErr != nil {
		httpx.Fail(w, r, httpx.ErrField("department_id", "NOT_FOUND", "That department was not found."))
		return
	}

	var parentID *int64
	if req.ParentID != "" {
		parent, err := h.repo.EntityByPublicID(ctx, tenantID, req.ParentID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("parent_id", "NOT_FOUND", "That parent entity was not found."))
			return
		}
		parentID = &parent.ID
	}

	e, err := h.repo.CreateEntity(ctx, tenantID, EntityParams{
		Code: req.Code, Name: req.Name, Type: req.Type, DepartmentID: dept.ID,
		ParentEntityID: parentID, Address: req.Address,
		IsActive: req.IsActive == nil || *req.IsActive,
	})
	if err != nil {
		if errors.Is(err, platform.ErrSentinelConflict) {
			httpx.Fail(w, r, httpx.ErrDuplicate("code", "An entity with this code already exists."))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "entity.created", EntityType: "entity", EntityID: &e.ID,
		EntityPublicID: e.PublicID, After: toEntityResponse(*e),
	})
	httpx.Created(w, r, toEntityResponse(*e))
}

func (h *Handler) updateEntity(w http.ResponseWriter, r *http.Request) {
	var req entityRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	existing, err := h.repo.EntityInReach(ctx, appctx.Reach(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That entity"))
		return
	}
	// The record's own client, not whatever the header named: a staff user
	// editing from the cross-client list has no client selected.
	tenantID = existing.TenantID

	update := EntityUpdate{IsActive: req.IsActive}
	if req.Code != "" {
		update.Code = &req.Code
	}
	if req.Name != "" {
		update.Name = &req.Name
	}
	if req.Type != "" {
		update.Type = &req.Type
	}
	if req.DepartmentID != "" {
		dept, err := h.repo.DepartmentByPublicID(ctx, tenantID, req.DepartmentID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("department_id", "NOT_FOUND", "That department was not found."))
			return
		}
		update.DepartmentID = &dept.ID
	}
	if req.Address != "" {
		update.Address = &req.Address
	}
	if req.ParentID != "" {
		parent, err := h.repo.EntityByPublicID(ctx, tenantID, req.ParentID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("parent_id", "NOT_FOUND", "That parent entity was not found."))
			return
		}
		update.ParentEntityID = &parent.ID
	}

	before := toEntityResponse(*existing)
	if err := h.repo.UpdateEntity(ctx, tenantID, existing.ID, update); err != nil {
		if errors.Is(err, platform.ErrSentinelConflict) {
			httpx.Fail(w, r, httpx.ErrConflict("An entity with this code already exists, or the parent would create a cycle."))
			return
		}
		httpx.Fail(w, r, mapErr(err, "That entity"))
		return
	}

	updated, err := h.repo.EntityByID(ctx, tenantID, existing.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "entity.updated", EntityType: "entity", EntityID: &existing.ID,
		EntityPublicID: existing.PublicID, Before: before, After: toEntityResponse(*updated),
	})
	httpx.OK(w, r, toEntityResponse(*updated))
}

func (h *Handler) deleteEntity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	existing, err := h.repo.EntityInReach(ctx, appctx.Reach(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That entity"))
		return
	}
	tenantID = existing.TenantID
	if err := h.repo.DeleteEntity(ctx, tenantID, existing.ID); err != nil {
		httpx.Fail(w, r, mapErr(err, "That entity"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "entity.deleted", EntityType: "entity", EntityID: &existing.ID,
		EntityPublicID: existing.PublicID, Before: toEntityResponse(*existing),
	})
	httpx.OK(w, r, map[string]any{"message": "Entity removed."})
}

// --- sites ------------------------------------------------------------------

func (h *Handler) listSites(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)
	actor := appctx.ActorFrom(ctx)

	var entityID *int64
	if raw := r.URL.Query().Get("entity_id"); raw != "" {
		e, err := h.repo.EntityByPublicID(ctx, tenantID, raw)
		if err != nil {
			httpx.Fail(w, r, mapErr(err, "That entity"))
			return
		}
		entityID = &e.ID
	}

	var ids []int64
	if actor != nil && !actor.IsSuperAdmin && !actor.Can("ticket.view.all") {
		ids = actor.Scopes.Sites
	}

	page := platform.ParsePage(r, OrgSortable, "")
	rows, err := h.repo.Sites(ctx, h.listReach(r), entityID,
		r.URL.Query().Get("include_inactive") != "true", ids, page, h.orgFilterFrom(r))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]siteResponse, 0, len(rows))
	for _, s := range rows {
		out = append(out, toSiteResponse(s))
	}
	httpx.OK(w, r, out)
}

func (h *Handler) getSite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s, err := h.repo.SiteInReach(ctx, appctx.Reach(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That site"))
		return
	}
	httpx.OK(w, r, toSiteResponse(*s))
}

type siteRequest struct {
	// The client this record belongs to. Required only when the caller has not
	// selected one — staff listing across clients have not, and pick it in the
	// form instead. Ignored on update: moving an entity between clients would
	// strand the tickets, users and sites already pointing at it.
	Client   string `json:"client" validate:"omitempty,max=64"`
	EntityID string `json:"entity_id" validate:"omitempty,len=26"`
	Code     string `json:"code" validate:"required,notblank,max=64,safetext"`
	Name     string `json:"name" validate:"required,notblank,max=191,safetext"`
	City     string `json:"city" validate:"omitempty,max=96"`
	State    string `json:"state" validate:"omitempty,max=96"`
	IsActive *bool  `json:"is_active"`
}

func (h *Handler) createSite(w http.ResponseWriter, r *http.Request) {
	var req siteRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID, err := h.writeClient(r, req.Client)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var entityID *int64
	if req.EntityID != "" {
		e, err := h.repo.EntityByPublicID(ctx, tenantID, req.EntityID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("entity_id", "NOT_FOUND", "That entity was not found."))
			return
		}
		entityID = &e.ID
	}

	s, err := h.repo.CreateSite(ctx, tenantID, SiteParams{
		EntityID: entityID, Code: req.Code, Name: req.Name,
		City: req.City, State: req.State,
		IsActive: req.IsActive == nil || *req.IsActive,
	})
	if err != nil {
		if errors.Is(err, platform.ErrSentinelConflict) {
			httpx.Fail(w, r, httpx.ErrDuplicate("code", "A site with this code already exists."))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "site.created", EntityType: "site", EntityID: &s.ID,
		EntityPublicID: s.PublicID, After: toSiteResponse(*s),
	})
	httpx.Created(w, r, toSiteResponse(*s))
}

func (h *Handler) updateSite(w http.ResponseWriter, r *http.Request) {
	var req siteRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	existing, err := h.repo.SiteInReach(ctx, appctx.Reach(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That site"))
		return
	}
	tenantID = existing.TenantID

	update := SiteUpdate{IsActive: req.IsActive}
	if req.Code != "" {
		update.Code = &req.Code
	}
	if req.Name != "" {
		update.Name = &req.Name
	}
	if req.City != "" {
		update.City = &req.City
	}
	if req.State != "" {
		update.State = &req.State
	}
	if req.EntityID != "" {
		e, err := h.repo.EntityByPublicID(ctx, tenantID, req.EntityID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("entity_id", "NOT_FOUND", "That entity was not found."))
			return
		}
		update.EntityID = &e.ID
	}

	before := toSiteResponse(*existing)
	if err := h.repo.UpdateSite(ctx, tenantID, existing.ID, update); err != nil {
		if errors.Is(err, platform.ErrSentinelConflict) {
			httpx.Fail(w, r, httpx.ErrDuplicate("code", "A site with this code already exists."))
			return
		}
		httpx.Fail(w, r, mapErr(err, "That site"))
		return
	}

	updated, err := h.repo.SiteByID(ctx, tenantID, existing.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "site.updated", EntityType: "site", EntityID: &existing.ID,
		EntityPublicID: existing.PublicID, Before: before, After: toSiteResponse(*updated),
	})
	httpx.OK(w, r, toSiteResponse(*updated))
}

func (h *Handler) deleteSite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	existing, err := h.repo.SiteInReach(ctx, appctx.Reach(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That site"))
		return
	}
	tenantID = existing.TenantID
	if err := h.repo.DeleteSite(ctx, tenantID, existing.ID); err != nil {
		httpx.Fail(w, r, mapErr(err, "That site"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "site.deleted", EntityType: "site", EntityID: &existing.ID,
		EntityPublicID: existing.PublicID, Before: toSiteResponse(*existing),
	})
	httpx.OK(w, r, map[string]any{"message": "Site removed."})
}

// --- departments ------------------------------------------------------------

func (h *Handler) listDepartments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	var ids []int64
	if actor != nil && !actor.IsSuperAdmin && !actor.Can("ticket.view.all") {
		ids = actor.Scopes.Departments
	}

	page := platform.ParsePage(r, OrgSortable, "")
	rows, err := h.repo.Departments(ctx, h.listReach(r),
		r.URL.Query().Get("include_inactive") != "true", ids, page, h.orgFilterFrom(r))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]departmentResponse, 0, len(rows))
	for _, d := range rows {
		out = append(out, toDepartmentResponse(d))
	}
	httpx.OK(w, r, out)
}

func (h *Handler) getDepartment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, err := h.repo.DepartmentInReach(ctx, appctx.Reach(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That department"))
		return
	}
	httpx.OK(w, r, toDepartmentResponse(*d))
}

type departmentRequest struct {
	// The client this record belongs to. Required only when the caller has not
	// selected one — staff listing across clients have not, and pick it in the
	// form instead. Ignored on update: moving an entity between clients would
	// strand the tickets, users and sites already pointing at it.
	Client   string `json:"client" validate:"omitempty,max=64"`
	Code     string `json:"code" validate:"required,notblank,max=64,safetext"`
	Name     string `json:"name" validate:"required,notblank,max=191,safetext"`
	Type     string `json:"type" validate:"omitempty,oneof=PF ESIC GENERAL PAYROLL HR OTHER"`
	IsActive *bool  `json:"is_active"`
}

func (h *Handler) createDepartment(w http.ResponseWriter, r *http.Request) {
	var req departmentRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID, err := h.writeClient(r, req.Client)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	d, err := h.repo.CreateDepartment(ctx, tenantID, DepartmentParams{
		Code: req.Code, Name: req.Name, Type: req.Type, IsActive: req.IsActive == nil || *req.IsActive,
	})
	if err != nil {
		if errors.Is(err, platform.ErrSentinelConflict) {
			httpx.Fail(w, r, httpx.ErrDuplicate("code", "A department with this code already exists."))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "department.created", EntityType: "department", EntityID: &d.ID,
		EntityPublicID: d.PublicID, After: toDepartmentResponse(*d),
	})
	httpx.Created(w, r, toDepartmentResponse(*d))
}

func (h *Handler) updateDepartment(w http.ResponseWriter, r *http.Request) {
	var req departmentRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	existing, err := h.repo.DepartmentInReach(ctx, appctx.Reach(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That department"))
		return
	}
	tenantID = existing.TenantID

	update := DepartmentUpdate{IsActive: req.IsActive}
	if req.Code != "" {
		update.Code = &req.Code
	}
	if req.Name != "" {
		update.Name = &req.Name
	}
	if req.Type != "" {
		update.Type = &req.Type
	}

	before := toDepartmentResponse(*existing)
	if err := h.repo.UpdateDepartment(ctx, tenantID, existing.ID, update); err != nil {
		if errors.Is(err, platform.ErrSentinelConflict) {
			httpx.Fail(w, r, httpx.ErrDuplicate("code", "A department with this code already exists."))
			return
		}
		httpx.Fail(w, r, mapErr(err, "That department"))
		return
	}

	updated, err := h.repo.DepartmentByID(ctx, tenantID, existing.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "department.updated", EntityType: "department", EntityID: &existing.ID,
		EntityPublicID: existing.PublicID, Before: before, After: toDepartmentResponse(*updated),
	})
	httpx.OK(w, r, toDepartmentResponse(*updated))
}

func (h *Handler) deleteDepartment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	existing, err := h.repo.DepartmentInReach(ctx, appctx.Reach(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That department"))
		return
	}
	tenantID = existing.TenantID
	if err := h.repo.DeleteDepartment(ctx, tenantID, existing.ID); err != nil {
		httpx.Fail(w, r, mapErr(err, "That department"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "department.deleted", EntityType: "department", EntityID: &existing.ID,
		EntityPublicID: existing.PublicID, Before: toDepartmentResponse(*existing),
	})
	httpx.OK(w, r, map[string]any{"message": "Department removed."})
}

// mapErr converts repository sentinels into the HTTP taxonomy. Cross-tenant
// access lands here as NOT_FOUND, which is deliberate: existence must not leak.
func mapErr(err error, resource string) error {
	switch {
	case errors.Is(err, platform.ErrSentinelNotFound):
		return httpx.ErrNotFound(resource)
	case errors.Is(err, platform.ErrSentinelConflict):
		return httpx.ErrConflict("")
	case errors.Is(err, platform.ErrSentinelImmutable):
		return httpx.ErrConflict("This record is a system record and cannot be changed.")
	default:
		return httpx.ErrInternal(err)
	}
}
