package user

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
)

// employmentRequest moves one employee between Active and Ex-Employee.
//
// Two fields, and which of them is required depends on the direction — leaving
// needs a date, returning does not, and both need an agent. Validated in the
// handler rather than by struct tags because "required when status is X" is not
// something a tag can say.
type employmentRequest struct {
	Status         string `json:"status" validate:"required"`
	LastWorkingDay string `json:"last_working_day" validate:"omitempty,dateonly"`
	AgentID        string `json:"agent_id" validate:"omitempty,len=26"`
	// Client names the workspace when staff act without one selected, exactly
	// as it does on ticket creation.
	Client string `json:"client" validate:"omitempty,max=64"`
}

// changeEmployment is the single-user Active ⇄ Ex-Employee transition.
//
// Separate from activate/deactivate, which are about whether an account may be
// used at all. Employment status is about whether somebody still works here:
// an ex-employee keeps a working login and read-only access to their own
// history, which is the whole reason the two are not the same switch.
func (h *Handler) changeEmployment(w http.ResponseWriter, r *http.Request) {
	target, err := h.loadTarget(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var req employmentRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	status := strings.ToUpper(strings.TrimSpace(req.Status))
	if status != StatusActive && status != StatusExEmployee {
		httpx.Fail(w, r, httpx.ErrField("status", "INVALID",
			"Status must be ACTIVE or EX_EMPLOYEE."))
		return
	}

	if err := h.mayAdminister(r, target); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var details []httpx.FieldError

	// A leaving date is what the ex-employee grace period counts from, so
	// leaving without one has no end date for their read-only access.
	change := EmploymentChange{UserID: target.ID, Status: status}
	if status == StatusExEmployee {
		d, ok := parseDate(req.LastWorkingDay)
		if !ok {
			details = append(details, httpx.FieldError{
				Field: "last_working_day", Code: "REQUIRED",
				Message: "Enter the last working day (date of exit)."})
		} else {
			change.LastWorkingDay = &d
		}
	}

	if strings.TrimSpace(req.AgentID) == "" {
		details = append(details, httpx.FieldError{
			Field: "agent_id", Code: "REQUIRED",
			Message: "Choose the agent who will handle this employee's queries."})
	}

	if len(details) > 0 {
		httpx.Fail(w, r, httpx.ErrValidation(details...))
		return
	}

	agent, err := h.repo.ByPublicIDGlobal(ctx, req.AgentID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrField("agent_id", "NOT_FOUND", "That agent was not found."))
		return
	}
	// An agent in another client's workspace would be given work they cannot
	// open. Staff are exempt: they live in the platform tenant by design and
	// reach this client through an assignment.
	if agent.TenantID != target.TenantID {
		assigned, err := h.repo.IsAssigned(ctx, agent.ID, target.TenantID)
		if err != nil || !assigned {
			httpx.Fail(w, r, httpx.ErrField("agent_id", "INVALID",
				"That agent does not work on this client."))
			return
		}
	}
	change.HandlingAgentID = agent.ID

	if actor := appctx.ActorFrom(ctx); actor != nil {
		id := actor.UserID
		change.ActorID = &id
	}

	result, err := h.repo.ChangeEmployment(ctx, target.TenantID, change)
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That user"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionUserEmploymentChanged, EntityType: "user", EntityID: &target.ID,
		EntityPublicID: target.PublicID,
		Before: map[string]any{
			"status":           target.Status,
			"last_working_day": nullDateString(target.LastWorkingDay),
		},
		After: map[string]any{
			"status":           result.Status,
			"last_working_day": result.LastWorkingDay,
			"handling_agent":   agent.FullName(),
			"tickets_moved":    result.TicketsMoved,
		},
	})

	message := "Employee restored to active."
	if status == StatusExEmployee {
		message = "Employee marked as an ex-employee."
	}
	httpx.OK(w, r, map[string]any{
		"message":          message,
		"status":           result.Status,
		"last_working_day": result.LastWorkingDay,
		"tickets_moved":    result.TicketsMoved,
		"handling_agent":   map[string]any{"id": agent.PublicID, "name": agent.FullName()},
	})
}

// assignableAgents answers the agent picker on the employment dialog.
func (h *Handler) assignableAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID, err := h.writeTenantID(r, r.URL.Query().Get("client"),
		"Choose the client whose agents you want.")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	rows, err := h.repo.AssignableAgents(ctx, tenantID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]map[string]any, 0, len(rows))
	for i := range rows {
		out = append(out, map[string]any{
			"id":    rows[i].PublicID,
			"name":  rows[i].FullName(),
			"email": rows[i].Email.String,
		})
	}
	httpx.OK(w, r, out)
}

// nullDateString renders an optional date for the audit trail, where an absent
// value must be visibly absent rather than the zero date.
func nullDateString(t sql.NullTime) any {
	if !t.Valid {
		return nil
	}
	return t.Time.Format("2006-01-02")
}
