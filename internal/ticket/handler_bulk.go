package ticket

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/export"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// bulkRoutes mounts the ticket list's export, bulk actions and the assignee
// picker they depend on.
//
// All three were stubbed in the browser's mock layer, which is why the export
// downloaded a file that would not open and bulk assign offered an empty list
// of agents: nothing was ever reaching the server.
//
// Called from inside Routes' own /tickets block rather than mounting a second
// one — chi panics on a duplicate mount — and before the /{id} routes, so
// "export" is never read as a ticket id.
func (h *Handler) bulkRoutes(r chi.Router, read func(http.Handler) http.Handler) {
	r.With(middleware.RequirePermission("ticket.export")).Get("/export", h.export)
	r.With(middleware.RequirePermission("ticket.bulk")).Post("/bulk", h.bulk)
	// Who a ticket can be handed to. Read-gated rather than assign-gated so the
	// same list can label an existing assignee.
	r.With(read).Get("/assignable", h.assignable)
}

// --- export -----------------------------------------------------------------

// exportColumns is the fixed column set. Held here rather than taken from the
// request so a caller cannot ask for a column the query does not select, and so
// the file's shape is stable between runs.
var exportColumns = []string{
	"Ticket", "Client", "Subject", "Status", "Priority", "Category",
	"Entity", "Department", "Requester", "Employee code", "Assignee",
	"Raised", "Resolution due", "Resolved", "SLA breached", "Reopened",
}

// export downloads the current list as a file.
//
// It runs exactly the query the list is showing — same filters, same scope —
// rather than a parallel one, so the file can never contain a row the reader
// could not already see, and the row count matches what was on screen.
func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = export.FormatCSV
	}
	if !export.ValidFormat(format) {
		httpx.Fail(w, r, httpx.ErrField("format", "INVALID",
			"format must be one of "+strings.Join(export.Formats, ", ")+"."))
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	filter := ListFilter{Scope: h.svc.ScopeFor(actor)}
	applyReach(&filter, appctx.Reach(ctx))
	h.applyQueryFilters(r, &filter)
	if err := h.applyReferenceFilters(r, &filter); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// An export is a file, not a page: it carries the whole result. The ceiling
	// stops one request pulling a million rows into memory, and the caller is
	// told when it bites rather than silently receiving a truncated file.
	const maxExportRows = 20000
	filter.Page = platform.Page{Page: 1, PerPage: maxExportRows, SortBy: "t.created_at", SortDir: "DESC"}

	rows, total, err := h.svc.Repo().List(ctx, appctx.TenantID(ctx), filter)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	if total > maxExportRows {
		httpx.Fail(w, r, httpx.ErrField("filters", "TOO_MANY",
			fmt.Sprintf("That is %d tickets. Narrow the filters to %d or fewer, then export.",
				total, maxExportRows)))
		return
	}

	result := &export.Result{
		Key:     "tickets",
		Title:   "Tickets",
		Columns: exportColumns,
		Rows:    make([][]any, 0, len(rows)),
		// Set below from the rows actually written.
		GeneratedAt: time.Now().UTC(),
	}

	for i := range rows {
		t := &rows[i]
		result.Rows = append(result.Rows, []any{
			t.TicketNumber,
			t.TenantName,
			t.Subject,
			humanStatus(t.Status),
			t.Priority,
			t.CategoryName,
			t.EntityName.String,
			t.DepartmentName.String,
			strings.TrimSpace(t.RequesterName),
			t.RequesterCode.String,
			strings.TrimSpace(t.AssigneeName.String),
			t.CreatedAt.UTC().Format("2006-01-02 15:04"),
			nullTime(t.ResolutionDueAt),
			nullTime(t.ResolvedAt),
			yesNo(t.IsSLABreached),
			t.ReopenedCount,
		})
	}
	result.RowCount = len(result.Rows)

	body, err := result.Render(format)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	filename := fmt.Sprintf("tickets-%s.%s", time.Now().UTC().Format("2006-01-02"), format)
	w.Header().Set("Content-Type", export.ContentType(format))
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// humanStatus turns PENDING_EMPLOYEE into "Pending employee" for a file a
// person will read rather than a machine will parse.
func humanStatus(status string) string {
	s := strings.ToLower(strings.ReplaceAll(status, "_", " "))
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func nullTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format("2006-01-02 15:04")
}

func yesNo(v bool) string {
	if v {
		return "Yes"
	}
	return "No"
}

// --- bulk actions -----------------------------------------------------------

// bulkRequest is one action applied to a set of tickets.
type bulkRequest struct {
	Action    string   `json:"action" validate:"required,oneof=STATUS ASSIGN TRANSFER ESCALATE CLOSE DELETE"`
	TicketIDs []string `json:"ticket_ids" validate:"required,min=1,max=200,dive,len=26"`

	Status       string `json:"status" validate:"omitempty,max=32"`
	AssigneeID   string `json:"assignee_id" validate:"omitempty,len=26"`
	DepartmentID string `json:"department_id" validate:"omitempty,len=26"`
	Comment      string `json:"comment" validate:"omitempty,max=2000"`
}

// bulkOutcome is what happened to one ticket. Reported per ticket rather than
// as a single pass/fail, because a bulk action over fifty tickets legitimately
// succeeds for most and fails for a few — and the operator needs to know which.
type bulkOutcome struct {
	TicketID     string `json:"ticket_id"`
	TicketNumber string `json:"ticket_number,omitempty"`
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
}

func (h *Handler) bulk(w http.ResponseWriter, r *http.Request) {
	var req bulkRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	// Every action has a permission of its own. Holding ticket.bulk lets you
	// reach this endpoint; it does not let you do something in bulk that you
	// could not do to one ticket.
	required := map[string]string{
		"STATUS":   "ticket.status.change",
		"ASSIGN":   "ticket.assign",
		"TRANSFER": "ticket.transfer",
		"ESCALATE": "ticket.escalate",
		"CLOSE":    "ticket.close",
		"DELETE":   "ticket.cancel",
	}[req.Action]
	if !actor.Can(required) {
		httpx.Fail(w, r, httpx.ErrForbidden("You do not have permission to perform this action."))
		return
	}

	outcomes := make([]bulkOutcome, 0, len(req.TicketIDs))
	succeeded := 0

	for _, publicID := range req.TicketIDs {
		outcome := bulkOutcome{TicketID: publicID}

		// Loaded one at a time, through the same path a single-ticket action
		// takes: that is what applies the caller's scope and the workflow rules
		// to each, rather than trusting a list of ids.
		t, err := h.svc.Load(ctx, publicID, actor)
		if err != nil {
			outcome.Error = "Not found, or not yours to change."
			outcomes = append(outcomes, outcome)
			continue
		}
		outcome.TicketNumber = t.TicketNumber

		if err := h.applyBulk(r, t, req, actor); err != nil {
			outcome.Error = bulkErrorMessage(err)
			outcomes = append(outcomes, outcome)
			continue
		}

		outcome.OK = true
		succeeded++
		outcomes = append(outcomes, outcome)
	}

	httpx.OK(w, r, map[string]any{
		"action":    req.Action,
		"requested": len(req.TicketIDs),
		"succeeded": succeeded,
		"failed":    len(req.TicketIDs) - succeeded,
		"results":   outcomes,
	})
}

// applyBulk performs one action on one already-loaded ticket.
//
// Each branch goes through exactly the repository call the single-ticket
// endpoint uses, including the workflow check — so a transition the client's
// workflow forbids is refused in bulk too, rather than bypassed because fifty
// tickets were selected at once.
func (h *Handler) applyBulk(r *http.Request, t *Ticket, req bulkRequest, actor *appctx.Actor) error {
	ctx := r.Context()
	actorID := actor.UserID

	changeStatus := func(to string) error {
		if _, err := h.svc.Repo().FindTransition(
			ctx, t.TenantID, t.CategoryID, t.Status, to, actor.Roles); err != nil {
			return httpx.New(httpx.CodeInvalidStatusTransition,
				"Cannot move from "+label(t.Status)+" to "+label(to)+".")
		}

		params := ChangeStatusParams{
			ToStatus: to, Comment: req.Comment,
			ActorID: &actorID, ActorName: actor.FullName,
		}
		if len(actor.Roles) > 0 {
			params.ActorRole = actor.Roles[0]
		}

		updated, err := h.svc.Repo().ChangeStatus(ctx, t.TenantID, t.ID, params)
		if err != nil {
			return MapError(err, "That ticket")
		}
		if updated == nil {
			updated = t
		}

		switch to {
		case StatusClosed:
			h.svc.PublishEvent(ctx, t.TenantID, "ticket.closed", updated)
		default:
			h.svc.PublishEvent(ctx, t.TenantID, "ticket.status_changed", updated)
		}
		return nil
	}

	assign := func(kind string, assigneeID, departmentID *int64) error {
		params := AssignParams{
			Type: kind, Reason: req.Comment,
			AssigneeID: assigneeID, DepartmentID: departmentID,
			ActorID: &actorID, ActorName: actor.FullName,
		}
		updated, err := h.svc.Repo().Assign(ctx, t.TenantID, t.ID, params)
		if err != nil {
			return MapError(err, "That ticket")
		}
		if updated == nil {
			updated = t
		}
		switch kind {
		case "ESCALATE":
			h.svc.PublishEvent(ctx, t.TenantID, "ticket.escalated", updated)
		default:
			h.svc.PublishEvent(ctx, t.TenantID, "ticket.assigned", updated)
		}
		return nil
	}

	switch req.Action {
	case "STATUS":
		status := CanonicalStatus(strings.ToUpper(strings.TrimSpace(req.Status)))
		if status == "" {
			return httpx.ErrField("status", "REQUIRED", "Choose a status.")
		}
		return changeStatus(status)

	case "CLOSE":
		return changeStatus(StatusClosed)

	// "Delete" from the list means cancel: a ticket records something that
	// happened, so it is withdrawn rather than erased and stays readable to
	// anyone auditing later.
	case "DELETE":
		return changeStatus(StatusCancelled)

	case "ESCALATE":
		return assign("ESCALATE", nil, nil)

	case "ASSIGN":
		if req.AssigneeID == "" {
			return httpx.ErrField("assignee_id", "REQUIRED", "Choose who to assign to.")
		}
		assignee, err := h.users.AssignableUser(ctx, t.TenantID, req.AssigneeID)
		if err != nil {
			return httpx.ErrField("assignee_id", "NOT_FOUND",
				"That user cannot be assigned to this ticket.")
		}
		return assign("ASSIGN", &assignee.ID, nil)

	case "TRANSFER":
		if req.DepartmentID == "" {
			return httpx.ErrField("department_id", "REQUIRED", "Choose a department.")
		}
		ids, err := h.svc.Repo().ResolveInReach(
			ctx, appctx.Reach(ctx), "departments", []string{req.DepartmentID})
		if err != nil || len(ids) == 0 {
			return httpx.ErrField("department_id", "NOT_FOUND", "That department was not found.")
		}
		return assign("TRANSFER", nil, &ids[0])
	}
	return httpx.ErrField("action", "INVALID", "Unknown action.")
}

// bulkErrorMessage renders one failure for the per-ticket results table.
func bulkErrorMessage(err error) string {
	if appErr, ok := httpx.AsAppError(err); ok {
		return appErr.Message
	}
	return "That change could not be applied."
}

// --- assignable people ------------------------------------------------------

// assignable lists the people a ticket may be handed to.
//
// Helpdesk staff, not everybody: assigning a ticket to the employee who raised
// it is not a thing anyone means to do, and offering the whole roster is how
// the picker became unusable. Staff live in the platform workspace rather than
// in the client, which is why this cannot be answered by the ordinary user list
// filtered to the current client — and why that list came back empty.
//
// Narrowed twice: to the client whose ticket it is, and to that ticket's
// statutory line. The department half is the one the picker was missing — an
// ESIC ticket offered the PF desk, and the choice was refused on submit.
func (h *Handler) assignable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// `?ticket=` names the ticket directly, and is the only form that can
	// answer "which department"; `?client=` names the client; with neither, the
	// selected client stands and a staff user with none selected gets the
	// unnarrowed list.
	clientTenantID := appctx.SelectedClientID(ctx)
	var departmentID int64
	if ref := strings.TrimSpace(r.URL.Query().Get("ticket")); ref != "" {
		if t, err := h.svc.Load(ctx, ref, appctx.ActorFrom(ctx)); err == nil {
			clientTenantID = t.TenantID
			if t.DepartmentID.Valid {
				departmentID = t.DepartmentID.Int64
			}
		}
	} else if ref := strings.TrimSpace(r.URL.Query().Get("client")); ref != "" {
		if id, err := platform.ResolveClientRef(ctx, h.svc.Repo().db.Primary, appctx.Reach(ctx), ref); err == nil {
			clientTenantID = id
		}
	}

	// An explicit department wins, so the transfer dialog can ask "who could
	// take this if I moved it to ESIC" before the move has happened.
	if ref := strings.TrimSpace(r.URL.Query().Get("department_id")); ref != "" && clientTenantID != 0 {
		id, err := h.resolveByPublicID(r, "departments", clientTenantID, ref)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("department_id", "NOT_FOUND",
				"That department was not found for this client."))
			return
		}
		departmentID = id
	}

	rows, err := h.users.AssignableStaff(ctx, clientTenantID, departmentID,
		strings.TrimSpace(r.URL.Query().Get("q")))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	type option struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email,omitempty"`
		Role  string `json:"role,omitempty"`
	}
	out := make([]option, 0, len(rows))
	for _, row := range rows {
		out = append(out, option{
			ID: row.PublicID, Name: row.Name, Email: row.Email, Role: row.RoleName,
		})
	}
	httpx.OK(w, r, out)
}
