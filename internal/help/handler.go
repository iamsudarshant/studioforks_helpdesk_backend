package help

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/user"
)

type Handler struct {
	repo  *Repository
	users *user.Repository
}

func NewHandler(repo *Repository, users *user.Repository) *Handler {
	return &Handler{repo: repo, users: users}
}

// Routes mounts the Help surface: a self-service FAQ everyone can read, and a
// "Request Help" ticket thread that only staff can answer and resolve.
func (h *Handler) Routes(r chi.Router) {
	manage := middleware.RequireAnyPermission("help.manage", "config.settings")
	staffReply := middleware.RequireAnyPermission("help.reply", "help.manage")

	r.Route("/help", func(r chi.Router) {
		// FAQ: readable by every signed-in user, maintained by staff.
		r.Get("/faq", h.listFAQ)
		r.With(manage).Post("/faq", h.createFAQ)
		r.With(manage).Patch("/faq/{id}", h.updateFAQ)
		r.With(manage).Delete("/faq/{id}", h.deleteFAQ)

		// Help tickets.
		r.Get("/tickets", h.listTickets)
		r.Post("/tickets", h.createTicket)
		r.Get("/tickets/{id}", h.getTicket)
		r.Get("/tickets/{id}/replies", h.listReplies)
		// Replies: staff answer; the requester may add follow-ups to their own.
		r.Post("/tickets/{id}/replies", h.addReply)
		// Resolve, assign, edit priority: staff only.
		r.With(staffReply).Patch("/tickets/{id}", h.updateTicket)
		// Removing one is administration, not answering it.
		r.With(manage).Delete("/tickets/{id}", h.deleteTicket)
	})
}

// deleteTicket withdraws a help request.
func (h *Handler) deleteTicket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID, err := h.readClient(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.repo.SoftDeleteTicket(ctx, tenantID, chi.URLParam(r, "id")); err != nil {
		if errors.Is(err, platform.ErrSentinelNotFound) {
			httpx.Fail(w, r, httpx.ErrNotFound("That help request"))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, map[string]any{"message": "The help request has been removed."})
}

// --- FAQ --------------------------------------------------------------------

type faqResponse struct {
	ID       string `json:"id"`
	Section  string `json:"section"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
	IsActive bool   `json:"is_active"`
}

func toFAQResponse(a FAQArticle) faqResponse {
	return faqResponse{
		ID: a.PublicID, Section: a.Section, Question: a.Question,
		Answer: a.Answer, IsActive: a.IsActive,
	}
}

// readClient decides whose help content a request is asking for.
//
// `?client=` names one explicitly; the tenant header otherwise. The header is
// right for everyone inside a client portal, and wrong for staff who have not
// selected a client — it then names ComplyDesk's own workspace, which is why
// the FAQ came back empty for every admin and agent.
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

func (h *Handler) listFAQ(w http.ResponseWriter, r *http.Request) {
	activeOnly := r.URL.Query().Get("include_inactive") != "true"

	tenantID, err := h.readClient(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	rows, err := h.repo.FAQArticles(r.Context(), tenantID, activeOnly, r.URL.Query().Get("q"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	out := make([]faqResponse, 0, len(rows))
	for _, a := range rows {
		out = append(out, toFAQResponse(a))
	}
	httpx.OK(w, r, out)
}

type faqRequest struct {
	Section   string `json:"section" validate:"required,notblank,max=96,safetext"`
	Question  string `json:"question" validate:"required,notblank,max=512"`
	Answer    string `json:"answer" validate:"required,notblank"`
	SortOrder int    `json:"sort_order"`
	IsActive  *bool  `json:"is_active"`
}

func (h *Handler) createFAQ(w http.ResponseWriter, r *http.Request) {
	var req faqRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	var by *int64
	if actor := appctx.ActorFrom(ctx); actor != nil {
		by = &actor.UserID
	}
	a, err := h.repo.CreateFAQ(ctx, appctx.TenantID(ctx), FAQParams{
		Section: req.Section, Question: req.Question, Answer: req.Answer,
		SortOrder: req.SortOrder, IsActive: req.IsActive == nil || *req.IsActive,
		CreatedBy: by,
	})
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.Created(w, r, toFAQResponse(*a))
}

func (h *Handler) updateFAQ(w http.ResponseWriter, r *http.Request) {
	var req faqRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)
	existing, err := h.repo.FAQByPublicID(ctx, tenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That article"))
		return
	}
	u := FAQUpdate{IsActive: req.IsActive}
	if req.Section != "" {
		u.Section = &req.Section
	}
	if req.Question != "" {
		u.Question = &req.Question
	}
	if req.Answer != "" {
		u.Answer = &req.Answer
	}
	if req.SortOrder != 0 {
		s := req.SortOrder
		u.SortOrder = &s
	}
	if err := h.repo.UpdateFAQ(ctx, tenantID, existing.ID, u); err != nil {
		httpx.Fail(w, r, mapErr(err))
		return
	}
	updated, err := h.repo.FAQByPublicID(ctx, tenantID, existing.PublicID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, toFAQResponse(*updated))
}

func (h *Handler) deleteFAQ(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)
	existing, err := h.repo.FAQByPublicID(ctx, tenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That article"))
		return
	}
	if err := h.repo.DeleteFAQ(ctx, tenantID, existing.ID); err != nil {
		httpx.Fail(w, r, mapErr(err))
		return
	}
	httpx.OK(w, r, map[string]any{"message": "Article removed."})
}

// --- help tickets -----------------------------------------------------------

type ticketResponse struct {
	ID            string  `json:"id"`
	Subject       string  `json:"subject"`
	Category      string  `json:"category"`
	Body          string  `json:"body"`
	Status        string  `json:"status"`
	Priority      string  `json:"priority"`
	RequesterID   string  `json:"requester_id"`
	RequesterName string  `json:"requester_name"`
	AssigneeID    string  `json:"assignee_id,omitempty"`
	AssigneeName  string  `json:"assignee_name,omitempty"`
	ResolvedAt    *string `json:"resolved_at"`
	ReplyCount    int     `json:"reply_count"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

func toTicketResponse(t HelpTicket) ticketResponse {
	out := ticketResponse{
		ID: t.PublicID, Subject: t.Subject, Category: t.Category, Body: t.Body,
		Status: t.Status, Priority: t.Priority,
		RequesterID:   t.RequesterPublicID.String,
		RequesterName: t.RequesterName.String,
		AssigneeName:  t.AssigneeName.String,
		ReplyCount:    t.ReplyCount,
		CreatedAt:     t.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:     t.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if t.AssigneePublicID.Valid {
		out.AssigneeID = t.AssigneePublicID.String
	}
	if t.ResolvedAt.Valid {
		s := t.ResolvedAt.Time.UTC().Format(time.RFC3339)
		out.ResolvedAt = &s
	}
	return out
}

func (h *Handler) listTickets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	// Staff see every help ticket in the workspace; everyone else only their own.
	var requesterID *int64
	if actor != nil && !actor.IsStaff {
		id := actor.UserID
		requesterID = &id
	}

	// Whose help requests. Staff with a client selected read that client's;
	// with none selected they read the staff workspace's own.
	tenantID, err := h.readClient(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	query := r.URL.Query()
	rows, err := h.repo.ListTickets(ctx, tenantID, requesterID, HelpFilter{
		Query:      strings.TrimSpace(query.Get("q")),
		Statuses:   platform.QueryStrings(r, "status"),
		Categories: platform.QueryStrings(r, "category"),
		Priorities: platform.QueryStrings(r, "priority"),
	})
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	out := make([]ticketResponse, 0, len(rows))
	for _, t := range rows {
		out = append(out, toTicketResponse(t))
	}
	httpx.OK(w, r, out)
}

func (h *Handler) createTicket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Subject  string `json:"subject" validate:"required,notblank,max=255"`
		Category string `json:"category" validate:"omitempty,oneof=BUG QUESTION REQUEST ACCESS"`
		Body     string `json:"body" validate:"required,notblank"`
		Priority string `json:"priority" validate:"omitempty,oneof=LOW NORMAL HIGH"`
	}
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

	// The ticket always belongs to the workspace it was raised in; client_id
	// records that workspace for staff browsing cross-client later.
	tenantID := appctx.TenantID(ctx)
	clientID := tenantID

	t, err := h.repo.CreateTicket(ctx, tenantID, CreateTicketParams{
		ClientID:    &clientID,
		RequesterID: actor.UserID,
		Subject:     req.Subject,
		Category:    strings.ToUpper(req.Category),
		Body:        req.Body,
		Priority:    strings.ToUpper(req.Priority),
	})
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.Created(w, r, toTicketResponse(*t))
}

func (h *Handler) getTicket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)
	actor := appctx.ActorFrom(ctx)

	t, err := h.repo.TicketByPublicID(ctx, tenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That request"))
		return
	}
	if actor == nil || (!actor.IsStaff && actor.UserID != t.RequesterID) {
		httpx.Fail(w, r, httpx.ErrNotFound("That request"))
		return
	}
	httpx.OK(w, r, toTicketResponse(*t))
}

func (h *Handler) listReplies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)
	t, err := h.repo.TicketByPublicID(ctx, tenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That request"))
		return
	}
	rows, err := h.repo.Replies(ctx, t.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, rows)
}

func (h *Handler) addReply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body string `json:"body" validate:"required,notblank"`
	}
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)
	actor := appctx.ActorFrom(ctx)
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	t, err := h.repo.TicketByPublicID(ctx, tenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That request"))
		return
	}

	// Only staff, or the ticket's own requester, may post.
	if !actor.IsStaff && actor.UserID != t.RequesterID {
		httpx.Fail(w, r, httpx.ErrForbidden("You can only follow up on your own requests."))
		return
	}

	role := "REQUESTER"
	if actor.IsStaff {
		role = "STAFF"
	}

	reply, err := h.repo.AddReply(ctx, t.ID, actor.UserID, role, req.Body)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.Created(w, r, reply)
}

func (h *Handler) updateTicket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status   *string `json:"status" validate:"omitempty,oneof=OPEN IN_PROGRESS RESOLVED"`
		Priority *string `json:"priority" validate:"omitempty,oneof=LOW NORMAL HIGH"`
		Assignee string  `json:"assignee_id" validate:"omitempty,len=26"`
	}
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)
	actor := appctx.ActorFrom(ctx)

	t, err := h.repo.TicketByPublicID(ctx, tenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That request"))
		return
	}

	u := TicketUpdate{}
	if req.Status != nil {
		s := strings.ToUpper(*req.Status)
		u.Status = &s
		if s == StatusResolved && actor != nil {
			by := actor.UserID
			u.ResolvedBy = &by
		}
		if s != StatusResolved {
			u.ClearResolved = true
		}
	}
	if req.Priority != nil {
		p := strings.ToUpper(*req.Priority)
		u.Priority = &p
	}
	if req.Assignee != "" {
		target, err := h.users.ByPublicID(ctx, tenantID, req.Assignee)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("assignee_id", "NOT_FOUND", "That user was not found."))
			return
		}
		u.AssignedTo = &target.ID
	}

	if err := h.repo.UpdateTicket(ctx, tenantID, t.ID, u); err != nil {
		httpx.Fail(w, r, mapErr(err))
		return
	}
	updated, err := h.repo.TicketByPublicID(ctx, tenantID, t.PublicID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, toTicketResponse(*updated))
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if platform.IsNotFound(err) {
		return httpx.ErrNotFound("That record")
	}
	return httpx.ErrInternal(err)
}
