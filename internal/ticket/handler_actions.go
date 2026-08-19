package ticket

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// --- conversations ----------------------------------------------------------

func (h *Handler) conversations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	rows, err := h.svc.Repo().Conversations(ctx, t.TenantID, t.ID, h.svc.CanSeeInternal(actor))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	files, err := h.svc.Repo().AttachmentsByConversation(ctx, t.TenantID, t.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	seen, err := h.svc.Repo().ReadConversationIDs(ctx, t.TenantID, t.ID, actor.UserID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	type author struct {
		ID       string `json:"id,omitempty"`
		FullName string `json:"full_name"`
		Role     string `json:"role,omitempty"`
	}
	type file struct {
		ID         int64  `json:"id"`
		DocumentID string `json:"document_id"`
		FileName   string `json:"file_name"`
		MimeType   string `json:"mime_type"`
		SizeBytes  int64  `json:"size_bytes"`
	}
	type message struct {
		ID          string     `json:"id"`
		Author      *author    `json:"author,omitempty"`
		AuthorRole  string     `json:"author_role,omitempty"`
		Visibility  string     `json:"visibility"`
		BodyHTML    string     `json:"body_html"`
		IsSystem    bool       `json:"is_system"`
		IsRead      bool       `json:"is_read"`
		CanDelete   bool       `json:"can_delete"`
		CanEdit     bool       `json:"can_edit"`
		Attachments []file     `json:"attachments,omitempty"`
		EditedAt    *time.Time `json:"edited_at,omitempty"`
		CreatedAt   time.Time  `json:"created_at"`
	}

	// Editing and withdrawing are your own words only, unless you administer the
	// desk. A helpdesk executive rewriting a partner's message would leave the
	// employee reading something nobody said.
	moderator := actor.Can("ticket.moderate")

	out := make([]message, 0, len(rows))
	for _, row := range rows {
		mine := row.AuthorID.Valid && row.AuthorID.Int64 == actor.UserID

		m := message{
			ID:         row.PublicID,
			AuthorRole: row.AuthorRole.String,
			Visibility: row.Visibility,
			BodyHTML:   row.BodyHTML.String,
			IsSystem:   row.IsSystem,
			// Your own message is read by definition; nothing marks it.
			IsRead:    mine || seen[row.ID],
			CanDelete: !row.IsSystem && (mine || moderator),
			CanEdit:   !row.IsSystem && mine,
			CreatedAt: row.CreatedAt,
		}
		if name := trimName(row.AuthorName.String); name != "" {
			m.Author = &author{FullName: name, Role: row.AuthorRole.String}
		}
		if row.EditedAt.Valid {
			edited := row.EditedAt.Time
			m.EditedAt = &edited
		}
		for _, a := range files[row.ID] {
			m.Attachments = append(m.Attachments, file{
				ID: a.ID, DocumentID: a.DocumentPubID, FileName: a.OriginalName,
				MimeType: a.MimeType, SizeBytes: a.SizeBytes,
			})
		}
		out = append(out, m)
	}
	httpx.OK(w, r, map[string]any{"items": out})
}

// --- editing, withdrawing and read receipts ---------------------------------

// conversation loads one message of a thread the caller may read.
func (h *Handler) conversation(r *http.Request) (*Ticket, *Conversation, error) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		return nil, nil, err
	}

	publicID := chi.URLParam(r, "conversationId")
	if !platform.ValidULID(publicID) {
		return nil, nil, httpx.ErrNotFound("That reply")
	}

	cv, err := h.svc.Repo().ConversationByPublicID(ctx, t.TenantID, t.ID, publicID)
	if err != nil {
		return nil, nil, MapError(err, "That reply")
	}
	// An internal note is not visible to someone who cannot see internal notes,
	// so it is not editable, deletable or markable by them either.
	if cv.Visibility == VisibilityInternal && !h.svc.CanSeeInternal(actor) {
		return nil, nil, httpx.ErrNotFound("That reply")
	}
	return t, cv, nil
}

type editReplyRequest struct {
	BodyHTML string `json:"body_html" validate:"required,notblank,max=50000"`
}

func (h *Handler) editReply(w http.ResponseWriter, r *http.Request) {
	var req editReplyRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	t, cv, err := h.conversation(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if cv.IsSystem || !cv.AuthorID.Valid || cv.AuthorID.Int64 != actor.UserID {
		httpx.Fail(w, r, httpx.ErrForbidden("You can only edit your own replies."))
		return
	}

	clean := h.svc.SanitiseHTML(req.BodyHTML)
	if PlainText(clean) == "" {
		httpx.Fail(w, r, httpx.ErrField("body_html", "EMPTY",
			"The reply is empty once formatting is removed."))
		return
	}

	id := actor.UserID
	if err := h.svc.Repo().EditConversation(ctx, t.TenantID, t.ID, cv.ID,
		clean, PlainText(clean), &id, actor.FullName); err != nil {
		httpx.Fail(w, r, MapError(err, "That reply"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionTicketReplied, EntityType: "ticket",
		EntityID: &t.ID, EntityPublicID: t.PublicID,
		After: map[string]any{"edited": cv.PublicID},
	})
	httpx.OK(w, r, map[string]any{"id": cv.PublicID, "edited": true})
}

func (h *Handler) deleteReply(w http.ResponseWriter, r *http.Request) {
	t, cv, err := h.conversation(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	mine := cv.AuthorID.Valid && cv.AuthorID.Int64 == actor.UserID
	if cv.IsSystem || !(mine || actor.Can("ticket.moderate")) {
		httpx.Fail(w, r, httpx.ErrForbidden("You can only withdraw your own replies."))
		return
	}

	id := actor.UserID
	if err := h.svc.Repo().DeleteConversation(ctx, t.TenantID, t.ID, cv.ID,
		&id, actor.FullName); err != nil {
		httpx.Fail(w, r, MapError(err, "That reply"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionTicketReplied, EntityType: "ticket",
		EntityID: &t.ID, EntityPublicID: t.PublicID,
		Before: map[string]any{"withdrawn": cv.PublicID},
	})
	httpx.OK(w, r, map[string]any{"id": cv.PublicID, "deleted": true})
}

func (h *Handler) markReplyRead(w http.ResponseWriter, r *http.Request) {
	_, cv, err := h.conversation(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	if err := h.svc.Repo().MarkConversationRead(ctx, cv.ID, appctx.ActorFrom(ctx).UserID); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, map[string]any{"id": cv.PublicID, "is_read": true})
}

type replyRequest struct {
	BodyHTML    string   `json:"body_html" validate:"required,notblank,max=50000"`
	Visibility  string   `json:"visibility" validate:"omitempty,oneof=PUBLIC INTERNAL"`
	DocumentIDs []string `json:"document_ids" validate:"omitempty,dive,len=26"`
	Mentions    []string `json:"mentions" validate:"omitempty,dive,len=26"`
}

func (h *Handler) reply(w http.ResponseWriter, r *http.Request) {
	var req replyRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	visibility := req.Visibility
	if visibility == "" {
		visibility = VisibilityPublic
	}
	// Posting an internal note needs the internal-note permission specifically;
	// holding only ticket.reply.public must not let one through.
	if visibility == VisibilityInternal && !actor.Can("ticket.reply.internal") {
		httpx.Fail(w, r, httpx.ErrForbidden("You cannot add internal notes."))
		return
	}
	if visibility == VisibilityPublic {
		canReply := actor.Can("ticket.reply.public")
		// A partner granted reply rights on this ticket's entity may reply even
		// without the general grant — the rights are per entity, and only apply
		// when the ticket actually hangs off that entity.
		if !canReply && t.EntityID.Valid {
			canReply, _ = h.svc.Repo().HasEntityReplyGrant(ctx, t.TenantID, t.EntityID.Int64, actor.UserID)
		}
		if !canReply {
			httpx.Fail(w, r, httpx.ErrForbidden("You cannot reply on this ticket."))
			return
		}
	}

	clean := h.svc.SanitiseHTML(req.BodyHTML)
	if PlainText(clean) == "" {
		httpx.Fail(w, r, httpx.ErrField("body_html", "EMPTY",
			"The reply is empty once formatting is removed."))
		return
	}

	params := ReplyParams{
		Visibility: visibility, BodyHTML: clean, BodyText: PlainText(clean),
		AuthorName: actor.FullName, Mentions: req.Mentions,
	}
	id := actor.UserID
	params.AuthorID = &id
	if len(actor.Roles) > 0 {
		params.AuthorRole = actor.Roles[0]
	}

	if len(req.DocumentIDs) > 0 {
		ids, err := h.resolveDocuments(r, t.TenantID, req.DocumentIDs)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		params.DocumentIDs = ids
	}

	created, err := h.svc.Repo().Reply(ctx, t.TenantID, t.ID, params)
	if err != nil {
		httpx.Fail(w, r, MapError(err, "That ticket"))
		return
	}

	action := audit.ActionTicketReplied
	if visibility == VisibilityInternal {
		action = audit.ActionTicketNote
	}
	h.auditor.Record(ctx, audit.Entry{
		Action: action, EntityType: "ticket", EntityID: &t.ID, EntityPublicID: t.PublicID,
		After: map[string]any{"visibility": visibility, "attachments": len(params.DocumentIDs)},
	})

	if visibility == VisibilityPublic {
		h.svc.PublishEvent(ctx, t.TenantID, "ticket.replied", t)
	}

	httpx.Created(w, r, map[string]any{
		"id": created.PublicID, "visibility": created.Visibility,
		"created_at": created.CreatedAt,
	})
}

// --- attachments ------------------------------------------------------------

func (h *Handler) attachments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), appctx.ActorFrom(ctx))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	rows, err := h.svc.Repo().Attachments(ctx, t.TenantID, t.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	type attachment struct {
		ID         int64  `json:"id"`
		DocumentID string `json:"document_id"`
		FileName   string `json:"file_name"`
		MimeType   string `json:"mime_type"`
		SizeBytes  int64  `json:"size_bytes"`
		Context    string `json:"context"`
		UploadedBy string `json:"uploaded_by"`
		// Thumbnails would need an unauthenticated image route; the client
		// falls back to a type icon, which is honest about what exists.
		ThumbnailAvailable bool      `json:"thumbnail_available"`
		HasVersions        bool      `json:"has_versions"`
		CanDelete          bool      `json:"can_delete"`
		CreatedAt          time.Time `json:"created_at"`
	}

	// Removing a file is your own upload, or the desk's prerogative.
	moderator := appctx.ActorFrom(ctx).Can("ticket.moderate")
	me := appctx.ActorFrom(ctx).UserID

	out := make([]attachment, 0, len(rows))
	for _, row := range rows {
		out = append(out, attachment{
			ID: row.ID, DocumentID: row.DocumentPubID, FileName: row.OriginalName,
			MimeType: row.MimeType, SizeBytes: row.SizeBytes, Context: row.Context,
			UploadedBy: trimName(row.UploaderName.String),
			CanDelete:  moderator || (row.UploadedBy.Valid && row.UploadedBy.Int64 == me),
			CreatedAt:  row.CreatedAt,
		})
	}
	httpx.OK(w, r, map[string]any{"items": out})
}

// detach removes one file from a ticket.
func (h *Handler) detach(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	attachmentID, err := strconv.ParseInt(chi.URLParam(r, "attachmentId"), 10, 64)
	if err != nil || attachmentID <= 0 {
		httpx.Fail(w, r, httpx.ErrNotFound("That attachment"))
		return
	}

	// Confirm ownership before acting, so the permission check and the delete
	// see the same row.
	rows, err := h.svc.Repo().Attachments(ctx, t.TenantID, t.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	var target *Attachment
	for i := range rows {
		if rows[i].ID == attachmentID {
			target = &rows[i]
			break
		}
	}
	if target == nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That attachment"))
		return
	}
	mine := target.UploadedBy.Valid && target.UploadedBy.Int64 == actor.UserID
	if !mine && !actor.Can("ticket.moderate") {
		httpx.Fail(w, r, httpx.ErrForbidden("You can only remove files you attached."))
		return
	}

	actorID := actor.UserID
	if err := h.svc.Repo().DetachDocument(ctx, t.TenantID, t.ID, attachmentID,
		&actorID, actor.FullName); err != nil {
		httpx.Fail(w, r, MapError(err, "That attachment"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionDocumentDeleted, EntityType: "ticket",
		EntityID: &t.ID, EntityPublicID: t.PublicID,
		Before: map[string]any{"file_name": target.OriginalName},
	})
	httpx.OK(w, r, map[string]any{"deleted": true})
}

type attachRequest struct {
	DocumentIDs []string `json:"document_ids" validate:"required,min=1,dive,len=26"`
	Description string   `json:"description" validate:"omitempty,max=500"`
}

// attach links already-uploaded documents to a ticket — the "upload and attach a
// document against any query in the admin panel" requirement.
func (h *Handler) attach(w http.ResponseWriter, r *http.Request) {
	var req attachRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ids, err := h.resolveDocuments(r, t.TenantID, req.DocumentIDs)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	context := "AGENT"
	if t.RequesterID == actor.UserID {
		context = "REQUESTER"
	} else if actor.Can("client.purge") {
		context = "ADMIN"
	}

	actorID := actor.UserID
	if err := h.svc.Repo().AttachDocuments(ctx, t.TenantID, t.ID, ids, context, &actorID, actor.FullName); err != nil {
		httpx.Fail(w, r, MapError(err, "That document"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionDocumentUploaded, EntityType: "ticket",
		EntityID: &t.ID, EntityPublicID: t.PublicID,
		After: map[string]any{"count": len(ids), "context": context},
	})
	httpx.OK(w, r, map[string]any{"attached": len(ids)})
}

// --- status transitions -----------------------------------------------------

type statusRequest struct {
	ToStatus   string `json:"to_status" validate:"omitempty,max=24"`
	ReasonCode string `json:"reason_code" validate:"omitempty,max=64"`
	Comment    string `json:"comment" validate:"omitempty,max=2000"`

	// The bulk endpoint calls the same thing `status`, and a caller that sends
	// that spelling here was answered "this field is not accepted by this
	// endpoint" for a payload that looked identical to the one that worked.
	// Accepted for the same reason `reason` is still accepted beside `comment`
	// on assign and transfer: two endpoints naming one concept differently is
	// the API's problem, not the caller's. `to_status` wins when both are sent.
	Status string `json:"status" validate:"omitempty,max=24"`
}

// target returns whichever spelling the caller used.
func (r statusRequest) target() string {
	if s := strings.TrimSpace(r.ToStatus); s != "" {
		return s
	}
	return strings.TrimSpace(r.Status)
}

func (h *Handler) changeStatus(w http.ResponseWriter, r *http.Request) {
	var req statusRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// Normalised to one field before anything reads it, so the rest of the
	// handler never has to know which spelling arrived.
	req.ToStatus = req.target()
	if req.ToStatus == "" {
		httpx.Fail(w, r, httpx.ErrField("to_status", "REQUIRED",
			"Choose the status to move this ticket to."))
		return
	}
	h.applyStatus(w, r, req)
}

// applyStatus validates the move against the client's configured workflow and
// applies it.
func (h *Handler) applyStatus(w http.ResponseWriter, r *http.Request, req statusRequest) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	transition, err := h.svc.Repo().FindTransition(ctx, t.TenantID, t.CategoryID, t.Status, req.ToStatus, actor.Roles)
	if err != nil {
		if !errors.Is(err, platform.ErrSentinelNotFound) {
			httpx.Fail(w, r, httpx.ErrInternal(err))
			return
		}

		// Not on the configured path. The desk may go anyway; a client-side
		// user may not.
		//
		// The detail endpoint offers staff every status (see TransitionsFor),
		// so refusing here would put moves in the menu that fail when pressed —
		// which is the failure mode this pair exists to avoid. The move still
		// has to name a real status, and it always demands a comment: going
		// outside the agreed lifecycle should say why.
		if !(actor.IsStaff || actor.IsSuperAdmin) || !isWorkableStatus(req.ToStatus) {
			httpx.Fail(w, r, httpx.New(httpx.CodeInvalidStatusTransition,
				"This ticket cannot move from "+label(t.Status)+" to "+label(req.ToStatus)+".").
				WithData("from", t.Status).WithData("to", req.ToStatus))
			return
		}
		transition = &Transition{ToStatus: req.ToStatus, RequiresComment: true, OffWorkflow: true}
	}

	// The workflow decides what the move requires, so a client can demand a
	// reason code on cancellation without a code change.
	if transition.RequiresComment && req.Comment == "" {
		message := "A comment is required for this status change."
		if transition.OffWorkflow {
			message = "This move is outside the configured workflow, so it needs a note saying why."
		}
		httpx.Fail(w, r, httpx.ErrField("comment", "REQUIRED", message))
		return
	}
	if transition.RequiresReason && req.ReasonCode == "" {
		httpx.Fail(w, r, httpx.ErrField("reason_code", "REQUIRED",
			"A reason is required for this status change."))
		return
	}

	actorID := actor.UserID
	params := ChangeStatusParams{
		ToStatus: req.ToStatus, ReasonCode: req.ReasonCode, Comment: req.Comment,
		ActorID: &actorID, ActorName: actor.FullName,
	}
	if len(actor.Roles) > 0 {
		params.ActorRole = actor.Roles[0]
	}

	updated, err := h.svc.Repo().ChangeStatus(ctx, t.TenantID, t.ID, params)
	if err != nil {
		httpx.Fail(w, r, MapError(err, "That ticket"))
		return
	}
	if updated == nil {
		updated = t
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionTicketStatus, EntityType: "ticket",
		EntityID: &t.ID, EntityPublicID: t.PublicID,
		Before: map[string]any{"status": t.Status},
		After:  map[string]any{"status": req.ToStatus, "reason_code": req.ReasonCode},
	})

	switch req.ToStatus {
	// Resolved and closed are one state now, so one event. See MIGRATION 000024.
	case StatusClosed:
		h.svc.PublishEvent(ctx, t.TenantID, "ticket.closed", updated)
	case StatusReopened:
		h.svc.PublishEvent(ctx, t.TenantID, "ticket.reopened", updated)
	default:
		h.svc.PublishEvent(ctx, t.TenantID, "ticket.status_changed", updated)
	}

	out := toResponse(updated)
	// The detail rail is re-rendered from this response, so it has to keep
	// carrying the requester's statutory identity — otherwise resolving a
	// ticket blanks the very numbers the next agent needs.
	out.Requester = withRequesterDetail(out.Requester, updated)
	httpx.OK(w, r, out)
}

func (h *Handler) close(w http.ResponseWriter, r *http.Request) {
	var req statusRequest
	_ = httpx.Decode(r, &req)
	req.ToStatus = StatusClosed
	h.applyStatus(w, r, req)
}

func (h *Handler) resolve(w http.ResponseWriter, r *http.Request) {
	var req statusRequest
	_ = httpx.Decode(r, &req)
	req.ToStatus = StatusResolved
	h.applyStatus(w, r, req)
}

// requestInfo hands the ticket back to the employee and stops the SLA clock —
// waiting on someone else is not time the desk should be judged on. Which
// statuses pause the clock is declared in SLAPausedStatuses, not decided here.
func (h *Handler) requestInfo(w http.ResponseWriter, r *http.Request) {
	var req statusRequest
	_ = httpx.Decode(r, &req)
	req.ToStatus = StatusPendingEmployee
	h.applyStatus(w, r, req)
}

// cancel withdraws a ticket that should never have been raised.
//
// It is a status, not a deletion: the thread, the timeline and any attachments
// stay exactly where they are. A helpdesk that can erase a request loses the
// only record that it was ever made, which is precisely what an audit trail
// exists to prevent.
func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	var req statusRequest
	_ = httpx.Decode(r, &req)
	req.ToStatus = StatusCancelled
	h.applyStatus(w, r, req)
}

// reopen is available to the employee who raised the ticket, within the client's
// configured window, as well as to staff.
func (h *Handler) reopen(w http.ResponseWriter, r *http.Request) {
	var req statusRequest
	_ = httpx.Decode(r, &req)

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if !h.canReopen(r, t, actor) {
		days := h.svc.ReopenWindowDays(ctx, t.TenantID)
		httpx.Fail(w, r, httpx.New(httpx.CodeReopenWindowExpired,
			"This ticket can no longer be reopened. Raise a new one referencing "+
				t.TicketNumber+".").WithData("window_days", days))
		return
	}

	req.ToStatus = StatusReopened
	h.applyStatus(w, r, req)
}

// --- assignment -------------------------------------------------------------

type assignRequest struct {
	AssigneeID   string `json:"assignee_id" validate:"omitempty,len=26"`
	DepartmentID string `json:"department_id" validate:"omitempty,len=26"`

	// Why. `comment` is the name every other ticket action uses — resolve,
	// close, reopen, cancel and request-info all take one — and this endpoint
	// calling the same thing `reason` is why assign, transfer and escalate all
	// answered "this field is not accepted by this endpoint" for a payload that
	// looked identical to the ones that worked.
	Comment string `json:"comment" validate:"omitempty,max=500"`
	// Deprecated: the original name, still accepted so anything already sending
	// it keeps working. `comment` wins when both are present.
	Reason string `json:"reason" validate:"omitempty,max=500"`
}

// note returns whichever spelling the caller used.
func (a assignRequest) note() string {
	if strings.TrimSpace(a.Comment) != "" {
		return a.Comment
	}
	return a.Reason
}

func (h *Handler) assign(w http.ResponseWriter, r *http.Request) { h.applyAssign(w, r, "ASSIGN") }

func (h *Handler) transfer(w http.ResponseWriter, r *http.Request) { h.applyAssign(w, r, "TRANSFER") }

func (h *Handler) escalate(w http.ResponseWriter, r *http.Request) { h.applyAssign(w, r, "ESCALATE") }

func (h *Handler) applyAssign(w http.ResponseWriter, r *http.Request, kind string) {
	var req assignRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// Assigning and transferring have to name a destination. Escalating does
	// not: it raises a ticket's level without changing who holds it, and
	// demanding an assignee made the action impossible to complete.
	if kind != "ESCALATE" && req.AssigneeID == "" && req.DepartmentID == "" {
		httpx.Fail(w, r, httpx.ErrField("assignee_id", "REQUIRED",
			"Choose who or which department should take this ticket."))
		return
	}

	// Escalating always has to say why.
	//
	// An escalation is a claim on somebody else's attention — it marks the
	// ticket as needing urgent handling and pushes it up the queue — and one
	// with no stated reason gives the person picking it up nothing to act on.
	// The other actions leave the comment optional because their destination is
	// itself the explanation.
	if kind == "ESCALATE" && strings.TrimSpace(req.note()) == "" {
		httpx.Fail(w, r, httpx.ErrField("comment", "REQUIRED",
			"Say why this is being escalated. Whoever picks it up needs to know what is urgent."))
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	actorID := actor.UserID
	params := AssignParams{Type: kind, Reason: req.note(), ActorID: &actorID, ActorName: actor.FullName}

	if req.AssigneeID != "" {
		// The assignee may be a client-side user or a Karma agent assigned to
		// this client, so a plain tenant-scoped lookup would miss the agents.
		assignee, err := h.users.AssignableUser(ctx, t.TenantID, req.AssigneeID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("assignee_id", "NOT_FOUND",
				"That user cannot be assigned to this ticket."))
			return
		}
		params.AssigneeID = &assignee.ID
	}
	if req.DepartmentID != "" {
		id, err := h.resolveByPublicID(r, "departments", t.TenantID, req.DepartmentID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("department_id", "NOT_FOUND", "That department was not found."))
			return
		}
		params.DepartmentID = &id
	}

	// The agent has to actually work the department the ticket will sit in.
	//
	// "Transfer to another department and its respective agent" is one action,
	// not two independent fields: naming the ESIC line and a PF agent produces a
	// ticket sitting in a queue its owner does not work. The same is true of a
	// plain assign, where the department is the ticket's existing one rather
	// than a field on the request — which is how the assign picker came to
	// offer every agent on every ticket regardless of line.
	//
	// Checked here as well as in the form, because a cascading picker makes the
	// wrong pair hard to choose and not impossible to send. Deliberately the
	// same predicate the picker filters on (see user.EligibleForDepartment), so
	// the two cannot drift: mapped to this line, or mapped to no line at all.
	if params.AssigneeID != nil {
		target := int64(0)
		switch {
		case params.DepartmentID != nil:
			target = *params.DepartmentID
		case t.DepartmentID.Valid:
			target = t.DepartmentID.Int64
		}

		ok, err := h.users.EligibleForDepartment(ctx, t.TenantID, target, *params.AssigneeID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrInternal(err))
			return
		}
		if !ok {
			httpx.Fail(w, r, httpx.ErrField("assignee_id", "INVALID",
				"That agent does not work in the department this ticket sits in."))
			return
		}
	}

	// A transfer has to move the ticket somewhere it is not already.
	//
	// Transferring to the department and agent it already sits with reads as an
	// action, records a timeline entry and notifies people, while changing
	// nothing — so the next agent sees "transferred" on a ticket that never
	// moved. Refused with the reason rather than silently accepted, because the
	// caller almost always meant to pick a different destination.
	if kind == "TRANSFER" {
		sameDept := params.DepartmentID == nil ||
			(t.DepartmentID.Valid && t.DepartmentID.Int64 == *params.DepartmentID)
		sameAssignee := params.AssigneeID == nil ||
			(t.AssigneeID.Valid && t.AssigneeID.Int64 == *params.AssigneeID)

		if sameDept && sameAssignee {
			httpx.Fail(w, r, httpx.ErrField("department_id", "NO_CHANGE",
				"This ticket is already with that department and agent. "+
					"Choose a different department, or a different agent within it."))
			return
		}
	}

	updated, err := h.svc.Repo().Assign(ctx, t.TenantID, t.ID, params)
	if err != nil {
		httpx.Fail(w, r, MapError(err, "That ticket"))
		return
	}

	action := audit.ActionTicketAssigned
	event := "ticket.assigned"
	switch kind {
	case "TRANSFER":
		action, event = audit.ActionTicketTransfer, "ticket.assigned"
	case "ESCALATE":
		action, event = audit.ActionTicketEscalated, "ticket.escalated"
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: action, EntityType: "ticket", EntityID: &t.ID, EntityPublicID: t.PublicID,
		After: map[string]any{"type": kind, "reason": req.Reason},
	})
	h.svc.PublishEvent(ctx, t.TenantID, event, updated)

	out := toResponse(updated)
	// The detail rail is re-rendered from this response, so it has to keep
	// carrying the requester's statutory identity — otherwise resolving a
	// ticket blanks the very numbers the next agent needs.
	out.Requester = withRequesterDetail(out.Requester, updated)
	httpx.OK(w, r, out)
}

// --- edit and feedback ------------------------------------------------------

type updateRequest struct {
	Subject string `json:"subject" validate:"omitempty,notblank,min=5,max=255,safetext"`
	// Catalogue-driven, like create. See PriorityCatalogue.
	Priority      string         `json:"priority" validate:"omitempty,max=32"`
	SubcategoryID string         `json:"subcategory_id" validate:"omitempty,len=26"`
	CustomFields  map[string]any `json:"custom_fields"`
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	actorID := actor.UserID
	params := UpdateParams{ActorID: &actorID, ActorName: actor.FullName}
	if req.Subject != "" {
		params.Subject = &req.Subject
	}
	if req.Priority != "" {
		priority := strings.ToUpper(strings.TrimSpace(req.Priority))
		if h.priorities != nil {
			known, err := h.priorities.KnownPriority(ctx, t.TenantID, priority)
			if err != nil {
				httpx.Fail(w, r, httpx.ErrInternal(err))
				return
			}
			if known == "" {
				httpx.Fail(w, r, httpx.ErrField("priority", "INVALID",
					"That priority is not available for this client."))
				return
			}
			priority = known
		}
		params.Priority = &priority
	}
	if req.SubcategoryID != "" {
		id, err := h.resolveSubcategory(r, t.TenantID, t.CategoryID, req.SubcategoryID)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		params.SubcategoryID = &id
	}
	if req.CustomFields != nil {
		raw, err := marshalJSON(req.CustomFields)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("custom_fields", "INVALID", "Custom fields could not be encoded."))
			return
		}
		params.CustomFields = &raw
	}

	updated, err := h.svc.Repo().Update(ctx, t.TenantID, t.ID, params)
	if err != nil {
		httpx.Fail(w, r, MapError(err, "That ticket"))
		return
	}

	// A file added to one of the category's FILE fields on an edit is an
	// attachment for the same reason it is on create: it arrived with the
	// ticket and belongs in Supporting Documents rather than sitting in the
	// field's value as an identifier nobody can open. AttachDocuments is an
	// upsert, so re-saving the form does not attach the same file twice.
	if req.CustomFields != nil {
		if fileKeys, err := h.svc.Repo().FileFieldKeys(ctx, t.TenantID, t.CategoryID); err == nil {
			refs := DocumentRefsInFields(req.CustomFields, fileKeys)
			if ids, err := h.resolveDocuments(r, t.TenantID, refs); err == nil && len(ids) > 0 {
				_ = h.svc.Repo().AttachDocuments(ctx, t.TenantID, t.ID, ids,
					"REQUESTER", &actorID, actor.FullName)
			}
		}
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionTicketUpdated, EntityType: "ticket",
		EntityID: &t.ID, EntityPublicID: t.PublicID,
		Before: map[string]any{"subject": t.Subject, "priority": t.Priority},
		After:  map[string]any{"subject": updated.Subject, "priority": updated.Priority},
	})
	out := toResponse(updated)
	// The detail rail is re-rendered from this response, so it has to keep
	// carrying the requester's statutory identity — otherwise resolving a
	// ticket blanks the very numbers the next agent needs.
	out.Requester = withRequesterDetail(out.Requester, updated)
	httpx.OK(w, r, out)
}

type feedbackRequest struct {
	Score   int    `json:"score" validate:"required,gte=1,lte=5"`
	Comment string `json:"comment" validate:"omitempty,max=1000"`
}

func (h *Handler) feedback(w http.ResponseWriter, r *http.Request) {
	var req feedbackRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	t, err := h.svc.Load(ctx, chi.URLParam(r, "id"), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// Feedback belongs to the person who raised the ticket.
	if t.RequesterID != actor.UserID {
		httpx.Fail(w, r, httpx.ErrForbidden("Only the person who raised this ticket can rate it."))
		return
	}
	if !t.ResolvedAt.Valid {
		httpx.Fail(w, r, httpx.ErrConflict("This ticket has not been resolved yet."))
		return
	}

	if err := h.svc.Repo().SetFeedback(ctx, t.TenantID, t.ID, actor.UserID, req.Score, req.Comment); err != nil {
		httpx.Fail(w, r, MapError(err, "That ticket"))
		return
	}
	httpx.OK(w, r, map[string]any{"message": "Thank you for your feedback.", "score": req.Score})
}

// trimName tidies the trailing space that CONCAT(first, ' ', last) leaves when
// the surname is NULL.
func trimName(s string) string { return strings.TrimSpace(s) }

func marshalJSON(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
