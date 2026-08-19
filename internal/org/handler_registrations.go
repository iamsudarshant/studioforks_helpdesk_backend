package org

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// --- the ticket-form lookup -------------------------------------------------

type categoryEntityResponse struct {
	ID                 string `json:"id"`
	Code               string `json:"code"`
	Name               string `json:"name"`
	RegistrationNumber string `json:"registration_number,omitempty"`
	// IsRegistered says whether this entity actually holds a live registration
	// for the chosen category, or is being offered because the client has
	// recorded none at all.
	IsRegistered bool `json:"is_registered"`
	// Label is what the form should render, so the client does not have to
	// decide how to combine the name and the statutory number.
	Label string `json:"label"`
}

// entitiesForCategory answers "on selection of PF or ESI, show their respected
// entities": only entities holding an active, unexpired registration for the
// chosen category, narrowed to the caller's assigned entities.
func (h *Handler) entitiesForCategory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// `?client=` when the ticket form is raising on behalf of a client the
	// caller has not switched to; the header otherwise.
	tenantID, err := h.writeClient(r, r.URL.Query().Get("client"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	category, err := h.categoryByPublicID(ctx, tenantID, chi.URLParam(r, "categoryId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	scope := scopedEntityIDs(appctx.ActorFrom(ctx))
	rows, err := h.repo.EntitiesForCategory(ctx, tenantID, category.ID, scope)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	// Nothing registered for this category: offer the client's entities rather
	// than an empty picker the requester cannot act on.
	registered := len(rows) > 0
	if !registered {
		rows, err = h.repo.EntitiesForClient(ctx, tenantID, scope)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrInternal(err))
			return
		}
	}

	out := make([]categoryEntityResponse, 0, len(rows))
	for _, e := range rows {
		item := categoryEntityResponse{
			ID: e.PublicID, Code: e.Code, Name: e.Name,
			RegistrationNumber: e.RegistrationNumber.String,
			IsRegistered:       e.IsRegistered,
			Label:              e.Name,
		}
		if e.RegistrationNumber.Valid && e.RegistrationNumber.String != "" {
			item.Label = e.Name + " — " + e.RegistrationNumber.String
		}
		out = append(out, item)
	}

	httpx.OK(w, r, map[string]any{
		"category": map[string]any{
			"id": category.PublicID, "key": category.Key, "name": category.Name,
		},
		"entities": out,
		// Whether these are the registered establishments or every one the
		// client has. The form says so quietly rather than blocking on it.
		"registered": registered,
		"message":    entityMessage(len(out), registered, category.Name),
	})
}

// entityMessage explains an unusual list, and says nothing about a normal one.
func entityMessage(count int, registered bool, categoryName string) string {
	switch {
	case count == 0:
		// Genuinely nothing to offer: the client has no active entity at all,
		// which is a setup gap rather than a missing registration.
		return "This client has no active entity yet. Ask your administrator to add one."
	case !registered:
		return "No entity is registered against " + categoryName +
			" yet, so all of them are listed."
	default:
		return ""
	}
}

// categoryRef is the slice of a category this package needs, read directly to
// avoid depending on the catalogue package.
type categoryRef struct {
	ID       int64  `db:"id"`
	PublicID string `db:"public_id"`
	Key      string `db:"category_key"`
	Name     string `db:"name"`
}

func (h *Handler) categoryByPublicID(ctx contextish, tenantID int64, publicID string) (*categoryRef, error) {
	if !platform.ValidULID(publicID) {
		return nil, httpx.ErrNotFound("That category")
	}

	var c categoryRef
	err := h.repo.db.Primary.GetContext(ctx, &c,
		`SELECT id, public_id, category_key, name FROM categories
		 WHERE tenant_id = ? AND public_id = ? AND deleted_at IS NULL`, tenantID, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, httpx.ErrNotFound("That category")
		}
		return nil, httpx.ErrInternal(err)
	}
	return &c, nil
}

// contextish is context.Context; aliased so the signature above stays short.
type contextish = interface {
	Deadline() (time.Time, bool)
	Done() <-chan struct{}
	Err() error
	Value(any) any
}

// parseDate reads an optional YYYY-MM-DD field.
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

// --- registrations ----------------------------------------------------------

type registrationResponse struct {
	CategoryID         string  `json:"category_id"`
	CategoryKey        string  `json:"category_key"`
	CategoryName       string  `json:"category_name"`
	RegistrationNumber string  `json:"registration_number,omitempty"`
	RegisteredOn       *string `json:"registered_on"`
	ValidUntil         *string `json:"valid_until"`
	Notes              string  `json:"notes,omitempty"`
	IsActive           bool    `json:"is_active"`
}

func toRegistrationResponse(reg Registration, categoryPublicID string) registrationResponse {
	out := registrationResponse{
		CategoryID: categoryPublicID, CategoryKey: reg.CategoryKey, CategoryName: reg.CategoryName,
		RegistrationNumber: reg.RegistrationNumber.String,
		Notes:              reg.Notes.String, IsActive: reg.IsActive,
	}
	if reg.RegisteredOn.Valid {
		s := reg.RegisteredOn.Time.Format("2006-01-02")
		out.RegisteredOn = &s
	}
	if reg.ValidUntil.Valid {
		s := reg.ValidUntil.Time.Format("2006-01-02")
		out.ValidUntil = &s
	}
	return out
}

func (h *Handler) listRegistrations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	entity, err := h.repo.EntityByPublicID(ctx, tenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That entity"))
		return
	}

	rows, err := h.repo.RegistrationsFor(ctx, tenantID, entity.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]registrationResponse, 0, len(rows))
	for _, reg := range rows {
		publicID, _ := h.categoryPublicID(ctx, reg.CategoryID)
		out = append(out, toRegistrationResponse(reg, publicID))
	}
	httpx.OK(w, r, out)
}

func (h *Handler) categoryPublicID(ctx contextish, id int64) (string, error) {
	var publicID string
	err := h.repo.db.Primary.GetContext(ctx, &publicID,
		`SELECT public_id FROM categories WHERE id = ?`, id)
	return publicID, err
}

type registrationRequest struct {
	RegistrationNumber string `json:"registration_number" validate:"omitempty,max=64,safetext"`
	RegisteredOn       string `json:"registered_on" validate:"omitempty,dateonly"`
	ValidUntil         string `json:"valid_until" validate:"omitempty,dateonly"`
	Notes              string `json:"notes" validate:"omitempty,max=500"`
	IsActive           *bool  `json:"is_active"`
}

// upsertRegistration records that an entity is registered for a category — the
// EPFO establishment code for PF, the ESIC code for ESI.
func (h *Handler) upsertRegistration(w http.ResponseWriter, r *http.Request) {
	var req registrationRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	entity, err := h.repo.EntityByPublicID(ctx, tenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That entity"))
		return
	}
	category, err := h.categoryByPublicID(ctx, tenantID, chi.URLParam(r, "categoryId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	params := RegistrationParams{
		EntityID: entity.ID, CategoryID: category.ID,
		RegistrationNumber: req.RegistrationNumber, Notes: req.Notes,
		IsActive: req.IsActive == nil || *req.IsActive,
	}
	if d, ok := parseDate(req.RegisteredOn); ok {
		params.RegisteredOn = &d
	}
	if d, ok := parseDate(req.ValidUntil); ok {
		params.ValidUntil = &d
	}
	if params.RegisteredOn != nil && params.ValidUntil != nil &&
		params.ValidUntil.Before(*params.RegisteredOn) {
		httpx.Fail(w, r, httpx.ErrField("valid_until", "INVALID",
			"The expiry date must fall after the registration date."))
		return
	}
	if actor := appctx.ActorFrom(ctx); actor != nil {
		id := actor.UserID
		params.CreatedBy = &id
	}

	reg, err := h.repo.UpsertRegistration(ctx, tenantID, params)
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That entity or category"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "entity.registration_set", EntityType: "entity", EntityID: &entity.ID,
		EntityPublicID: entity.PublicID,
		After: map[string]any{
			"category": category.Key, "registration_number": req.RegistrationNumber,
			"is_active": params.IsActive,
		},
	})
	httpx.OK(w, r, toRegistrationResponse(*reg, category.PublicID))
}

func (h *Handler) deleteRegistration(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	entity, err := h.repo.EntityByPublicID(ctx, tenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That entity"))
		return
	}
	category, err := h.categoryByPublicID(ctx, tenantID, chi.URLParam(r, "categoryId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.repo.DeleteRegistration(ctx, tenantID, entity.ID, category.ID); err != nil {
		httpx.Fail(w, r, mapErr(err, "That registration"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "entity.registration_removed", EntityType: "entity", EntityID: &entity.ID,
		EntityPublicID: entity.PublicID, Before: map[string]any{"category": category.Key},
	})
	httpx.OK(w, r, map[string]any{"message": "Registration removed."})
}

// --- opt in / opt out / purge ----------------------------------------------

// optOutEntity switches a default entity off for this client. Deliberately not a
// delete: the entity keeps its history, can be switched back on, and re-running
// the default templates will not resurrect it.
func (h *Handler) optOutEntity(w http.ResponseWriter, r *http.Request) {
	h.setOptOut(w, r, true, "entity.opted_out", "Entity switched off for this client.")
}

func (h *Handler) optInEntity(w http.ResponseWriter, r *http.Request) {
	h.setOptOut(w, r, false, "entity.opted_in", "Entity switched back on.")
}

func (h *Handler) setOptOut(w http.ResponseWriter, r *http.Request, optedOut bool, action, message string) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	entity, err := h.repo.EntityByPublicID(ctx, tenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That entity"))
		return
	}

	var actorID *int64
	if actor := appctx.ActorFrom(ctx); actor != nil {
		id := actor.UserID
		actorID = &id
	}

	if err := h.repo.SetOptedOut(ctx, tenantID, entity.ID, optedOut, actorID); err != nil {
		httpx.Fail(w, r, mapErr(err, "That entity"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: action, EntityType: "entity", EntityID: &entity.ID,
		EntityPublicID: entity.PublicID,
		After:          map[string]any{"opted_out": optedOut},
	})
	httpx.OK(w, r, map[string]any{"message": message, "opted_out": optedOut})
}

// purgeEntity erases an entity permanently. Reserved for admins; agents get the
// recoverable delete instead. Refused while the entity is still referenced, so
// a purge can never orphan a ticket's history.
func (h *Handler) purgeEntity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)

	entity, err := h.repo.EntityByPublicID(ctx, tenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That entity"))
		return
	}

	tickets, users, err := h.repo.EntityReferences(ctx, tenantID, entity.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	if tickets > 0 || users > 0 {
		httpx.Fail(w, r, httpx.ErrConflict(
			"This entity is still referenced by existing records and cannot be erased. "+
				"Deactivate it instead — it will stop appearing on new tickets.").
			WithData("tickets", tickets).WithData("users", users))
		return
	}

	if err := h.repo.PurgeEntity(ctx, tenantID, entity.ID); err != nil {
		httpx.Fail(w, r, mapErr(err, "That entity"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "entity.purged", EntityType: "entity", EntityID: &entity.ID,
		EntityPublicID: entity.PublicID, Before: toEntityResponse(*entity),
	})
	httpx.OK(w, r, map[string]any{"message": "Entity permanently removed."})
}

// --- default templates ------------------------------------------------------

func (h *Handler) listTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := h.repo.Templates(r.Context(), r.URL.Query().Get("include_inactive") != "true")
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	type templateResponse struct {
		ID          string `json:"id"`
		Key         string `json:"key"`
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		EntityType  string `json:"entity_type,omitempty"`
		IsActive    bool   `json:"is_active"`
	}

	out := make([]templateResponse, 0, len(rows))
	for _, t := range rows {
		out = append(out, templateResponse{
			ID: t.PublicID, Key: t.Key, Name: t.Name,
			Description: t.Description.String, EntityType: t.EntityType.String,
			IsActive: t.IsActive,
		})
	}
	httpx.OK(w, r, out)
}

// applyDefaultEntities creates this client's default entities from the platform
// catalogue. Idempotent: templates already applied — or deliberately opted out —
// are left alone.
func (h *Handler) applyDefaultEntities(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CodePrefix string `json:"code_prefix" validate:"omitempty,max=12,safetext"`
	}
	// The body is optional — a bare POST applies the defaults with a prefix
	// derived from the client slug — so a decode failure is not an error here.
	_ = httpx.Decode(r, &req)

	ctx := r.Context()
	tenant := appctx.TenantFrom(ctx)
	if tenant == nil {
		httpx.Fail(w, r, httpx.New(httpx.CodeTenantNotFound, "No workspace matches this address."))
		return
	}

	prefix := req.CodePrefix
	if prefix == "" {
		prefix = defaultPrefix(tenant.Slug)
	}

	created, err := h.repo.ApplyTemplates(ctx, tenant.ID, prefix)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "entity.defaults_applied", EntityType: "tenant", EntityID: &tenant.ID,
		After: map[string]any{"created": created, "code_prefix": prefix},
	})
	httpx.OK(w, r, map[string]any{
		"created": created,
		"message": pluraliseCreated(created),
	})
}

func pluraliseCreated(n int) string {
	switch n {
	case 0:
		return "No new entities to add — the defaults are already in place."
	case 1:
		return "1 default entity added."
	default:
		return itoa(n) + " default entities added."
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// defaultPrefix turns a client slug into a short entity code prefix.
func defaultPrefix(slug string) string {
	out := ""
	for _, r := range slug {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			out += string(r)
		}
		if len(out) >= 6 {
			break
		}
	}
	if out == "" {
		return "ENT"
	}
	return upper(out)
}

func upper(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			r -= 32
		}
		out = append(out, r)
	}
	return string(out)
}
