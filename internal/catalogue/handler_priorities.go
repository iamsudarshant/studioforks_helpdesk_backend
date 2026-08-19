package catalogue

// The priority catalogue's HTTP surface.
//
// Reading is open to anyone who may raise a ticket, because the create form
// renders its dropdown from it. Changing the catalogue is configuration, gated
// on the same permission the rest of the catalogue uses.

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
)

// Letters, digits and underscores. See createPriority.
var priorityKeyPattern = regexp.MustCompile(`^[A-Z0-9_]+$`)

func (h *Handler) priorityRoutes(r chi.Router) {
	config := middleware.RequirePermission("config.category")

	r.Route("/ticket-priorities", func(r chi.Router) {
		r.Get("/", h.listPriorities)
		r.With(config).Post("/", h.createPriority)
		r.With(config).Patch("/{id}", h.updatePriority)
		r.With(config).Delete("/{id}", h.deletePriority)
	})
}

type priorityResponse struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Weight      int    `json:"weight"`
	Colour      string `json:"colour,omitempty"`
	IsDefault   bool   `json:"is_default"`
	IsActive    bool   `json:"is_active"`
	// A system level can be renamed or switched off but never deleted: existing
	// tickets store the key, and removing it would leave them unnameable.
	IsSystem bool `json:"is_system"`
	// Whether this row is the client's own or inherited from the platform.
	IsInherited bool `json:"is_inherited"`
}

func toPriorityResponse(p Priority) priorityResponse {
	return priorityResponse{
		ID: p.PublicID, Key: p.Key, Name: p.Name,
		Description: p.Description.String, Weight: p.Weight,
		Colour: p.Colour.String, IsDefault: p.IsDefault,
		IsActive: p.IsActive, IsSystem: p.IsSystem,
		IsInherited: !p.TenantID.Valid,
	}
}

func (h *Handler) listPriorities(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.readClient(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	rows, err := h.repo.Priorities(r.Context(), tenantID,
		r.URL.Query().Get("include_inactive") == "true")
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]priorityResponse, 0, len(rows))
	for _, p := range rows {
		out = append(out, toPriorityResponse(p))
	}
	httpx.OK(w, r, out)
}

type priorityRequest struct {
	// The key is what tickets store, so it is set once and never edited —
	// renaming is what `name` is for.
	Key         string `json:"key" validate:"omitempty,max=32,safetext"`
	Name        string `json:"name" validate:"required,notblank,max=64,safetext"`
	Description string `json:"description" validate:"omitempty,max=255,safetext"`
	Weight      int    `json:"weight" validate:"omitempty,min=0,max=1000"`
	Colour      string `json:"colour" validate:"omitempty,max=16"`
	IsDefault   *bool  `json:"is_default"`
	IsActive    *bool  `json:"is_active"`
	Client      string `json:"client" validate:"omitempty,max=64"`
}

func (h *Handler) createPriority(w http.ResponseWriter, r *http.Request) {
	var req priorityRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID, err := h.readClient(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// Derived from the name when not given, so the common case is one field.
	key := strings.ToUpper(strings.TrimSpace(req.Key))
	if key == "" {
		key = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(req.Name), " ", "_"))
	}
	if key == "" {
		httpx.Fail(w, r, httpx.ErrField("name", "REQUIRED", "Give the priority a name."))
		return
	}
	// The key is stored on every ticket raised at this level and appears in
	// exports and saved filters, so it is held to a shape that survives all
	// three: letters, digits and underscores.
	if !priorityKeyPattern.MatchString(key) {
		httpx.Fail(w, r, httpx.ErrField("key", "INVALID",
			"A priority key may contain only letters, numbers and underscores."))
		return
	}

	// A key already in the catalogue would shadow it rather than add a level,
	// which is a confusing way to discover a typo.
	if existing, err := h.repo.PriorityByKey(ctx, tenantID, key); err == nil && existing != nil {
		httpx.Fail(w, r, httpx.ErrDuplicate("key",
			"A priority with this key already exists. Rename the existing one instead."))
		return
	}

	created, err := h.repo.CreatePriority(ctx, tenantID, PriorityParams{
		Key: key, Name: req.Name, Description: req.Description,
		Weight: req.Weight, Colour: req.Colour,
		IsDefault: req.IsDefault != nil && *req.IsDefault,
		IsActive:  req.IsActive == nil || *req.IsActive,
	})
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.recordPriorityChange(ctx, audit.ActionConfigChanged, created, nil)
	httpx.Created(w, r, toPriorityResponse(*created))
}

func (h *Handler) updatePriority(w http.ResponseWriter, r *http.Request) {
	var req priorityRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID, err := h.readClient(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	existing, err := h.repo.PriorityByPublicID(ctx, tenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That priority"))
		return
	}

	// A platform level cannot be edited in place — that would change it for
	// every client. Editing one creates the client's own copy, which shadows
	// the shared row for this client alone.
	if !existing.TenantID.Valid {
		created, err := h.repo.CreatePriority(ctx, tenantID, PriorityParams{
			Key: existing.Key, Name: req.Name, Description: req.Description,
			Weight: pickInt(req.Weight, existing.Weight), Colour: req.Colour,
			IsDefault: req.IsDefault != nil && *req.IsDefault,
			IsActive:  req.IsActive == nil || *req.IsActive,
		})
		if err != nil {
			httpx.Fail(w, r, httpx.ErrInternal(err))
			return
		}
		h.recordPriorityChange(ctx, audit.ActionConfigChanged, created, existing)
		httpx.OK(w, r, toPriorityResponse(*created))
		return
	}

	before := *existing
	updated, err := h.repo.UpdatePriority(ctx, tenantID, existing.ID, existing.PublicID, PriorityParams{
		Key: existing.Key, Name: req.Name, Description: req.Description,
		Weight: pickInt(req.Weight, existing.Weight), Colour: req.Colour,
		IsDefault: req.IsDefault != nil && *req.IsDefault,
		IsActive:  req.IsActive == nil || *req.IsActive,
	})
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.recordPriorityChange(ctx, audit.ActionConfigChanged, updated, &before)
	httpx.OK(w, r, toPriorityResponse(*updated))
}

func (h *Handler) deletePriority(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID, err := h.readClient(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	existing, err := h.repo.PriorityByPublicID(ctx, tenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That priority"))
		return
	}

	if existing.IsSystem {
		httpx.Fail(w, r, httpx.ErrField("id", "SYSTEM",
			"This is a built-in priority and cannot be deleted. Switch it off instead if you do not want it offered."))
		return
	}
	if !existing.TenantID.Valid {
		httpx.Fail(w, r, httpx.ErrField("id", "INHERITED",
			"This priority comes from the platform catalogue. Switch it off for this client instead of deleting it."))
		return
	}

	if err := h.repo.DeletePriority(ctx, tenantID, existing.ID, existing.Key); err != nil {
		if errors.Is(err, ErrPriorityInUse) {
			httpx.Fail(w, r, httpx.ErrConflict(
				"Tickets are still using this priority. Switch it off instead — existing tickets keep the level they were raised at."))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.recordPriorityChange(ctx, audit.ActionConfigChanged, nil, existing)
	httpx.OK(w, r, map[string]any{"message": "Priority removed."})
}

func (h *Handler) recordPriorityChange(ctx context.Context, action string, after, before *Priority) {
	if h.auditor == nil {
		return
	}
	entry := audit.Entry{Action: action, EntityType: "ticket_priority"}
	if before != nil {
		entry.Before = toPriorityResponse(*before)
		entry.EntityPublicID = before.PublicID
	}
	if after != nil {
		entry.After = toPriorityResponse(*after)
		entry.EntityPublicID = after.PublicID
	}
	h.auditor.Record(ctx, entry)
}

func pickInt(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}
