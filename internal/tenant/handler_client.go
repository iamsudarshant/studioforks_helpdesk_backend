package tenant

import (
	"errors"
	"net/http"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// clientRequest is the client master form. Every field is optional on a PATCH so
// a partner can correct one detail without resubmitting the record.
//
// client_code is deliberately absent: it is set at creation and fixed, because
// the client's portals are addressed by it. Slug and ticket prefix are managed
// by platform staff through /tenants.
type clientRequest struct {
	Name          string `json:"name" validate:"omitempty,notblank,max=191,safetext"`
	LegalName     string `json:"legal_name" validate:"omitempty,max=191,safetext"`
	Industry      string `json:"industry" validate:"omitempty,max=96,safetext"`
	Timezone      string `json:"timezone" validate:"omitempty,max=64"`
	ContactEmail  string `json:"contact_email" validate:"omitempty,email,max=191"`
	AltEmail      string `json:"alt_email" validate:"omitempty,email,max=191"`
	ContactPhone  string `json:"contact_phone" validate:"omitempty,max=32"`
	AltPhone      string `json:"alt_phone" validate:"omitempty,max=32"`
	Address       string `json:"address" validate:"omitempty,max=1000"`
	GSTNumber     string `json:"gst_number" validate:"omitempty,max=64,safetext"`
	ContractStart string `json:"contract_start" validate:"omitempty,dateonly"`
	ContractEnd   string `json:"contract_end" validate:"omitempty,dateonly"`
	Status        string `json:"status" validate:"omitempty,oneof=ACTIVE SUSPENDED"`
}

// toUpdateParams maps the request onto the repository update, leaving unsent
// fields untouched.
func (req clientRequest) toUpdateParams(allowStatus bool) (UpdateParams, error) {
	update := UpdateParams{}

	assign := func(dst **string, v string) {
		if v != "" {
			val := v
			*dst = &val
		}
	}
	assign(&update.Name, req.Name)
	assign(&update.LegalName, req.LegalName)
	assign(&update.Industry, req.Industry)
	assign(&update.Timezone, req.Timezone)
	assign(&update.ContactEmail, req.ContactEmail)
	assign(&update.AltEmail, req.AltEmail)
	assign(&update.ContactPhone, req.ContactPhone)
	assign(&update.AltPhone, req.AltPhone)
	assign(&update.Address, req.Address)
	assign(&update.TaxID, req.GSTNumber)
	if allowStatus {
		assign(&update.Status, req.Status)
	}

	if d, ok := parseDate(req.ContractStart); ok {
		update.ContractStart = &d
	}
	if d, ok := parseDate(req.ContractEnd); ok {
		update.ContractEnd = &d
	}
	if update.ContractStart != nil && update.ContractEnd != nil &&
		update.ContractEnd.Before(*update.ContractStart) {
		return update, httpx.ErrField("contract_end", "INVALID",
			"The contract end date must fall after the start date.")
	}
	return update, nil
}

// updateCurrent edits the caller's own client master. Partners reach this and
// nothing else, so they can correct details without touching another client.
func (h *Handler) updateCurrent(w http.ResponseWriter, r *http.Request) {
	var req clientRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)
	actor := appctx.ActorFrom(ctx)

	// Only someone who may suspend a client may change its status; a partner
	// correcting a phone number must not be able to deactivate themselves.
	allowStatus := actor != nil && actor.CanAny("client.delete", "tenant.manage")

	update, err := req.toUpdateParams(allowStatus)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if req.Status != "" && !allowStatus {
		httpx.Fail(w, r, httpx.ErrForbidden("You cannot change this client's status."))
		return
	}

	before, err := h.svc.repo.ByID(ctx, tenantID)
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "This client"))
		return
	}

	if err := h.svc.repo.Update(ctx, tenantID, update); err != nil {
		if errors.Is(err, platform.ErrSentinelConflict) {
			httpx.Fail(w, r, httpx.ErrDuplicate("client_code", "Another client already uses this code."))
			return
		}
		httpx.Fail(w, r, mapErr(err, "This client"))
		return
	}
	h.svc.InvalidateTenant(ctx, before.Slug)

	after, err := h.svc.repo.ByID(ctx, tenantID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		TenantID: &tenantID, Action: audit.ActionTenantUpdated,
		EntityType: "client", EntityID: &tenantID, EntityPublicID: before.PublicID,
		Before: toTenantResponse(before), After: toTenantResponse(after),
	})
	httpx.OK(w, r, toTenantResponse(after))
}

// currentPrefixHistory reports the caller's own client's ticket-prefix change
// log, so the partner portal can show who changed the numbering and when.
func (h *Handler) currentPrefixHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := h.svc.repo.PrefixHistory(ctx, appctx.TenantID(ctx))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, map[string]any{"items": rows})
}
