package catalogue

import (
	"encoding/json"
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

// Routes mounts the read surface the ticket form depends on. Category
// administration (create, edit, custom-field editor) is a later phase; these are
// the endpoints without which a ticket cannot be raised at all.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/categories", func(r chi.Router) {
		r.Get("/", h.list)
		r.Get("/{id}", h.get)
		// Drives the dynamic form renderer: fields, validation and conditional
		// visibility, so a new query domain needs no frontend change.
		r.Get("/{id}/form-schema", h.formSchema)
		r.Get("/{id}/workflow", h.workflow)
	})

	// Ticket priorities: read by the create form, edited from configuration.
	h.priorityRoutes(r)

	// The module catalogue. Reading it is open to anyone who may raise a ticket,
	// because navigation depends on it; changing a client's enablement is a
	// configuration action.
	r.Route("/modules", func(r chi.Router) {
		r.Get("/", h.listModules)
		r.With(middleware.RequirePermission("tenant.settings.edit")).
			Put("/{id}", h.setModule)
	})
}

type categoryResponse struct {
	ID  string `json:"id"`
	Key string `json:"key"`
	// ParentID and IsSubcategory drive the two-level picker on the ticket form:
	// a top-level category, then the subcategories beneath it.
	ParentID      *string `json:"parent_id"`
	DepartmentID  *string `json:"department_id,omitempty"`
	IsSubcategory bool    `json:"is_subcategory"`
	// The compliance domain this category belongs to (PF, ESIC, Payroll, ...).
	ModuleID       *string         `json:"module_id,omitempty"`
	ModuleKey      string          `json:"module_key,omitempty"`
	Name           string          `json:"name"`
	Description    string          `json:"description,omitempty"`
	TicketPrefix   string          `json:"ticket_prefix"`
	Icon           string          `json:"icon,omitempty"`
	Color          string          `json:"color,omitempty"`
	RequiresFields json.RawMessage `json:"requires_fields,omitempty"`
	IsActive       bool            `json:"is_active"`
	SortOrder      int             `json:"sort_order"`
}

func toCategoryResponse(c Category) categoryResponse {
	var parentID, moduleID, departmentID *string
	if c.DepartmentPublicID.Valid && c.DepartmentPublicID.String != "" {
		departmentID = &c.DepartmentPublicID.String
	}
	if c.ParentPublicID.Valid && c.ParentPublicID.String != "" {
		parentID = &c.ParentPublicID.String
	}
	if c.ModulePublicID.Valid && c.ModulePublicID.String != "" {
		moduleID = &c.ModulePublicID.String
	}

	return categoryResponse{
		ID: c.PublicID, Key: c.Key, Name: c.Name,
		ParentID: parentID, IsSubcategory: c.IsSubcategory,
		DepartmentID: departmentID,
		ModuleID:     moduleID, ModuleKey: c.ModuleKey.String,
		Description: c.Description.String, TicketPrefix: c.TicketPrefix,
		Icon: c.Icon.String, Color: c.Color.String,
		RequiresFields: RawJSON(c.RequiresFieldsJSON),
		IsActive:       c.IsActive, SortOrder: c.SortOrder,
	}
}

// scopedCategoryIDs narrows the list to the caller's assigned categories.
func scopedCategoryIDs(actor *appctx.Actor) []int64 {
	if actor == nil || actor.IsSuperAdmin || actor.Can("ticket.view.all") {
		return nil
	}
	return actor.Scopes.Categories
}

// readClient decides whose catalogue a request is asking for.
//
// The tenant header alone is not enough. A staff user with no client selected
// resolves to ComplyDesk's own workspace, which holds no categories — so the
// ticket form would offer an empty picker and the requester could go no
// further. `?client=` names the client explicitly; without it the header
// stands, which is what every client-portal request wants.
func (h *Handler) readClient(r *http.Request) (int64, error) {
	ctx := r.Context()
	ref := strings.TrimSpace(r.URL.Query().Get("client"))
	if ref == "" {
		return appctx.TenantID(ctx), nil
	}

	id, err := platform.ResolveClientRef(ctx, h.repo.db.Primary, appctx.Reach(ctx), ref)
	if err != nil {
		if errors.Is(err, platform.ErrSentinelNotFound) {
			return 0, httpx.ErrField("client", "NOT_FOUND", "That client was not found.")
		}
		return 0, httpx.ErrInternal(err)
	}
	return id, nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	tenantID, err := h.readClient(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	rows, err := h.repo.List(ctx, tenantID,
		r.URL.Query().Get("include_inactive") != "true",
		scopedCategoryIDs(appctx.ActorFrom(ctx)))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	// The ticket form asks twice: once for the top level (`top_level=true`), then
	// for one category's children (`parent_id=<id>`). Filtering here rather than
	// in SQL keeps the module gate and the scope filter in a single query.
	query := r.URL.Query()
	parentID := query.Get("parent_id")
	topLevelOnly := query.Get("top_level") == "true"
	// Which statutory line the caller is raising under. The ticket form asks for
	// the department first, so the categories it then offers must be that
	// department's — a client that runs PF and ESIC has no use for the Payroll
	// or IT categories the platform catalogue also ships.
	departmentID := query.Get("department_id")

	out := make([]categoryResponse, 0, len(rows))
	for _, c := range rows {
		if parentID != "" && c.ParentPublicID.String != parentID {
			continue
		}
		if topLevelOnly && c.IsSubcategory {
			continue
		}
		if departmentID != "" && c.DepartmentPublicID.String != departmentID {
			continue
		}
		out = append(out, toCategoryResponse(c))
	}
	httpx.OK(w, r, out)
}

func (h *Handler) load(r *http.Request) (*Category, error) {
	id := chi.URLParam(r, "id")
	if !platform.ValidULID(id) {
		return nil, httpx.ErrNotFound("That category")
	}

	// Resolved across reach, not against the tenant header: the ticket form
	// loads a category's schema for whichever client it is raising against, and
	// staff raising on behalf of a client have that client's id in hand without
	// having switched their whole session to it.
	c, err := h.repo.ByPublicIDInReach(r.Context(), appctx.Reach(r.Context()), id)
	if err != nil {
		if errors.Is(err, platform.ErrSentinelNotFound) {
			return nil, httpx.ErrNotFound("That category")
		}
		return nil, httpx.ErrInternal(err)
	}
	return c, nil
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	c, err := h.load(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, toCategoryResponse(*c))
}

type fieldResponse struct {
	Key          string          `json:"key"`
	Label        string          `json:"label"`
	Type         string          `json:"type"`
	Required     bool            `json:"required"`
	Options      json.RawMessage `json:"options"`
	Validation   json.RawMessage `json:"validation"`
	HelpText     string          `json:"help_text,omitempty"`
	Placeholder  string          `json:"placeholder,omitempty"`
	DefaultValue string          `json:"default_value,omitempty"`
	DependsOn    json.RawMessage `json:"depends_on,omitempty"`
	SortOrder    int             `json:"sort_order"`
	Visible      bool            `json:"visible"`
	Editable     bool            `json:"editable"`
}

// formSchema returns everything the dynamic form renderer needs for a category.
func (h *Handler) formSchema(w http.ResponseWriter, r *http.Request) {
	c, err := h.load(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	// The category's own client, not the header's: `load` resolved it across
	// reach, so the two differ whenever staff work without a client selected.
	//
	// Inherited, because a subcategory carries no fields of its own — see
	// FieldsInherited.
	fields, err := h.repo.FieldsInherited(ctx, c)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]fieldResponse, 0, len(fields))
	for _, f := range fields {
		out = append(out, fieldResponse{
			Key: f.Key, Label: f.Label, Type: f.Type, Required: f.IsRequired,
			Options: RawJSON(f.OptionsJSON), Validation: RawJSON(f.ValidationJSON),
			HelpText: f.HelpText.String, Placeholder: f.Placeholder.String,
			DefaultValue: f.DefaultValue.String, DependsOn: RawJSON(f.DependsOnJSON),
			SortOrder: f.SortOrder,
			// Per-role visibility is a later phase; today every active field is
			// shown and editable to anyone who may raise the ticket.
			Visible: true, Editable: true,
		})
	}

	httpx.OK(w, r, map[string]any{
		"category": toCategoryResponse(*c),
		"fields":   out,
	})
}

func (h *Handler) workflow(w http.ResponseWriter, r *http.Request) {
	c, err := h.load(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	rows, err := h.repo.Transitions(ctx, c.TenantID, c.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	type transitionResponse struct {
		FromStatus      string          `json:"from_status"`
		ToStatus        string          `json:"to_status"`
		Label           string          `json:"label,omitempty"`
		RequiresComment bool            `json:"requires_comment"`
		RequiresReason  bool            `json:"requires_reason_code"`
		ReasonCodes     json.RawMessage `json:"reason_codes,omitempty"`
	}

	out := make([]transitionResponse, 0, len(rows))
	for _, t := range rows {
		out = append(out, transitionResponse{
			FromStatus: t.FromStatus, ToStatus: t.ToStatus, Label: t.Label.String,
			RequiresComment: t.RequiresComment, RequiresReason: t.RequiresReason,
			ReasonCodes: RawJSON(t.ReasonCodesJSON),
		})
	}
	httpx.OK(w, r, out)
}
