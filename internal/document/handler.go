package document

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// maxUploadForm caps the multipart parse. The real size limit lives in the
// storage layer and is enforced against the decoded body; this only stops a
// malicious form from being buffered before we get that far.
const maxUploadForm = 64 << 20 // 64 MiB

// maxBulkDocuments caps a ZIP request. A helpdesk attachment set is a handful
// of files; anything larger is an export, which is a different feature.
const maxBulkDocuments = 25

// TicketGuard answers whether the caller may open a ticket.
//
// A document attached to a ticket inherits that ticket's visibility, so the
// answer has to come from the ticket engine rather than being re-derived here.
// Two implementations of "may this person see this ticket" would eventually
// disagree, and the disagreement would be a data leak.
type TicketGuard interface {
	CanSeeID(ctx context.Context, tenantID, ticketID int64, actor *appctx.Actor) bool
	TicketIDsForDocument(ctx context.Context, tenantID, documentID int64) ([]int64, error)
}

// AvatarUser reads and writes the document behind a profile picture. Avatars
// are documents like anything else, but they are served to signed-out browsers
// and swapped atomically, so only the reference on the user row matters.
type AvatarUser interface {
	SetAvatarPath(ctx context.Context, tenantID, userID int64, path string) error
	AvatarDocID(ctx context.Context, userPublicID string) (string, error)
}

type Handler struct {
	svc        *Service
	tickets    TicketGuard
	avatarUser AvatarUser
	auditor    *audit.Writer
}

func NewHandler(svc *Service, tickets TicketGuard, avatarUser AvatarUser, auditor *audit.Writer) *Handler {
	return &Handler{svc: svc, tickets: tickets, avatarUser: avatarUser, auditor: auditor}
}

// Routes mounts the session-required document surface.
func (h *Handler) Routes(r chi.Router) {
	view := middleware.RequirePermission("document.view")
	download := middleware.RequirePermission("document.download")

	r.Route("/documents", func(r chi.Router) {
		r.With(middleware.RequirePermission("document.upload")).Post("/", h.upload)
		r.Post("/avatar", h.avatarUpload)
		r.With(download).Post("/bulk-download", h.bulkDownload)

		r.Route("/{id}", func(r chi.Router) {
			r.With(view).Get("/", h.show)
			r.With(download).Get("/download", h.download)
			r.With(view).Get("/preview", h.preview)
			r.With(view).Post("/signed-url", h.signedURL)
			r.With(view).Get("/versions", h.versions)
			r.With(middleware.RequirePermission("document.version")).Post("/versions", h.addVersion)
			r.With(view).Get("/access-log", h.accessLog)
			r.With(middleware.RequirePermission("document.delete")).Delete("/", h.remove)
		})
	})
}

// PublicRoutes mounts the signed-link readers.
//
// A browser cannot put a bearer token on an <img src> or hand one to pdf.js, so
// those loads carry a short-lived HMAC over the document, action, viewer and
// expiry instead. See signedRead for what that signature does and does not
// prove.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Route("/public/documents", func(r chi.Router) {
		r.Get("/{id}/{action}", h.signedRead)
		r.Get("/avatar/{userPublicID}", h.avatarRead)
	})
}

// --- responses --------------------------------------------------------------

type documentResponse struct {
	ID           string `json:"id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	SizeBytes    int64  `json:"size_bytes"`
	Version      int    `json:"version"`
	Description  string `json:"description,omitempty"`
	CategoryID   string `json:"document_category_id,omitempty"`
	ScanStatus   string `json:"scan_status"`
	OwnerType    string `json:"owner_type"`
	Previewable  bool   `json:"previewable"`
	HasVersions  bool   `json:"has_versions"`
	UploadedByID *int64 `json:"-"`
	CreatedAt    string `json:"created_at"`
}

func present(doc *Document) documentResponse {
	return documentResponse{
		ID:          doc.PublicID,
		FileName:    doc.OriginalName,
		MimeType:    doc.MimeType,
		SizeBytes:   doc.SizeBytes,
		Version:     doc.Version,
		Description: doc.Description.String,
		ScanStatus:  doc.ScanStatus,
		OwnerType:   doc.OwnerType,
		Previewable: CanPreviewInline(doc.MimeType),
		HasVersions: doc.Version > 1,
		CreatedAt:   doc.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// --- upload -----------------------------------------------------------------

func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)
	actor := appctx.ActorFrom(ctx)

	if err := r.ParseMultipartForm(maxUploadForm); err != nil {
		httpx.Fail(w, r, httpx.ErrField("file", "INVALID", "Send the file as a multipart upload."))
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.Fail(w, r, httpx.ErrField("file", "REQUIRED", "Choose a file to upload."))
		return
	}
	defer func() { _ = file.Close() }()

	// The owner is derived from what the caller may reach, never taken from the
	// form. A client-supplied owner id would let anyone file a document against
	// someone else's record.
	ownerType := "GENERAL"
	var ownerID *int64

	// A file uploaded *before* the ticket exists — the attachment picker on the
	// create form — has no ticket to take its workspace from, so the form names
	// the client instead.
	//
	// Without this, staff raising a ticket on behalf of an employee uploaded
	// into whatever the header named, which with no client selected is the
	// platform workspace. The ticket was then created in the client, the
	// tenant-scoped lookup for its attachments found nothing, and the create
	// failed with "One or more attachments were not found" — so a staff-raised
	// ticket could never carry a document.
	if raw := strings.TrimSpace(r.FormValue("client")); raw != "" {
		id, err := platform.ResolveClientRef(ctx, h.svc.repo.db.Primary, appctx.Reach(ctx), raw)
		if err != nil || actor == nil || !actor.MayAccessTenant(id) {
			httpx.Fail(w, r, httpx.ErrField("client", "NOT_FOUND", "That client was not found."))
			return
		}
		tenantID = id
	}

	if raw := strings.TrimSpace(r.FormValue("ticket_id")); raw != "" {
		ticketID, ticketTenantID, err := h.ticketFor(ctx, raw, actor)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		// The file belongs in the ticket's workspace. For staff with no client
		// selected the header names the platform tenant, and storing the
		// attachment there would leave it unreachable from the ticket it was
		// attached to.
		tenantID = ticketTenantID
		ownerType, ownerID = "TICKET", &ticketID
	}

	var categoryID *int64
	if raw := strings.TrimSpace(r.FormValue("document_category_id")); raw != "" {
		cat, err := h.svc.repo.CategoryByPublicID(ctx, tenantID, raw)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("document_category_id", "NOT_FOUND",
				"That document category does not exist."))
			return
		}
		categoryID = &cat.ID
	}

	uploader := actor.UserID
	doc, err := h.svc.Upload(ctx, UploadParams{
		TenantSlug:  tenantSlug(ctx),
		TenantID:    tenantID,
		OwnerType:   ownerType,
		OwnerID:     ownerID,
		CategoryID:  categoryID,
		Description: strings.TrimSpace(r.FormValue("description")),
		UploadedBy:  &uploader,
		Header:      header,
		File:        file,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionDocumentUploaded, EntityType: "document",
		EntityID: &doc.ID, EntityPublicID: doc.PublicID,
		After: map[string]any{
			"file_name": doc.OriginalName, "mime_type": doc.MimeType,
			"size_bytes": doc.SizeBytes, "owner_type": doc.OwnerType,
		},
	})

	httpx.Created(w, r, present(doc))
}

// ticketFor resolves a ticket the caller is allowed to attach to.
//
// Returns the ticket's internal id *and* the client it belongs to, because an
// attachment must be written into the ticket's own workspace — which, for staff
// with no client selected, is not the one the request arrived under. Attaching
// a file to the platform tenant instead is what produced "That ticket was not
// found" when replying with a document.
func (h *Handler) ticketFor(ctx context.Context, publicID string, actor *appctx.Actor) (int64, int64, error) {
	if !platform.ValidULID(publicID) {
		return 0, 0, httpx.ErrField("ticket_id", "INVALID", "That is not a valid ticket reference.")
	}

	t, err := h.svc.repo.TicketRefInReach(ctx, appctx.Reach(ctx), publicID)
	if err != nil || !h.tickets.CanSeeID(ctx, t.TenantID, t.ID, actor) {
		// Same answer either way: someone who cannot see a ticket must not be
		// able to tell whether it exists.
		return 0, 0, httpx.ErrNotFound("That ticket")
	}
	return t.ID, t.TenantID, nil
}

// --- avatars ----------------------------------------------------------------

// maxAvatarBytes caps a profile picture. A portrait is a few hundred KB, so a
// 5 MB ceiling leaves room for a phone photo without letting someone file a
// video through the avatar route.
const maxAvatarBytes = 5 << 20

// avatarUpload replaces the caller's profile picture. The new file becomes a
// USER-owned document and the reference on the user row is swapped to it; the
// previous picture is removed so it does not linger in the file list.
func (h *Handler) avatarUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}
	tenantID := appctx.TenantID(ctx)

	if err := r.ParseMultipartForm(maxUploadForm); err != nil {
		httpx.Fail(w, r, httpx.ErrField("file", "INVALID", "Send the picture as a multipart upload."))
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.Fail(w, r, httpx.ErrField("file", "REQUIRED", "Choose a picture to upload."))
		return
	}
	defer func() { _ = file.Close() }()

	if !strings.HasPrefix(header.Header.Get("Content-Type"), "image/") {
		httpx.Fail(w, r, httpx.ErrField("file", "INVALID", "A profile picture must be an image file."))
		return
	}
	if header.Size > maxAvatarBytes {
		httpx.Fail(w, r, httpx.ErrField("file", "TOO_LARGE", "The picture must be 5 MB or smaller."))
		return
	}

	uploader := actor.UserID
	doc, err := h.svc.Upload(ctx, UploadParams{
		TenantSlug:  tenantSlug(ctx),
		TenantID:    tenantID,
		OwnerType:   "USER",
		OwnerID:     &actor.UserID,
		Description: "Profile picture",
		UploadedBy:  &uploader,
		Header:      header,
		File:        file,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// Drop the previous picture. It belongs to the same user, so nobody else's
	// file can be swept up this way.
	old, err := h.avatarUser.AvatarDocID(ctx, actor.PublicID)
	if err == nil && old != "" && old != doc.PublicID {
		if oldDoc, derr := h.svc.repo.ByPublicIDAcrossTenants(ctx, old); derr == nil {
			_ = h.svc.repo.SoftDelete(ctx, oldDoc.TenantID, oldDoc.ID)
		}
	}
	if err := h.avatarUser.SetAvatarPath(ctx, tenantID, actor.UserID, doc.PublicID); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionDocumentUploaded, EntityType: "user",
		EntityID: &actor.UserID, EntityPublicID: actor.PublicID,
		After: map[string]any{"avatar": doc.PublicID, "file_name": doc.OriginalName},
	})

	httpx.OK(w, r, map[string]any{
		"id": doc.PublicID, "file_name": doc.OriginalName, "mime_type": doc.MimeType,
		"size_bytes": doc.SizeBytes,
		"avatar_url": "/api/v1/public/documents/avatar/" + actor.PublicID,
	})
}

// avatarRead serves a profile picture to a signed-out browser, mirroring the
// branding asset route. Only a document referenced by a user's avatar_path is
// ever served, so the endpoint cannot be pointed at arbitrary stored files.
func (h *Handler) avatarRead(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	publicID := chi.URLParam(r, "userPublicID")
	if !platform.ValidULID(publicID) {
		httpx.Fail(w, r, httpx.ErrNotFound("That picture"))
		return
	}

	path, err := h.avatarUser.AvatarDocID(ctx, publicID)
	if err != nil || path == "" || !platform.ValidULID(path) {
		httpx.Fail(w, r, httpx.ErrNotFound("That picture"))
		return
	}

	doc, err := h.svc.repo.ByPublicIDAcrossTenants(ctx, path)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That picture"))
		return
	}

	body, err := h.svc.Open(ctx, doc.TenantID, doc)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", doc.MimeType)
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A portrait is not sensitive, so browsers may cache it briefly.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, body); err != nil {
		return
	}
}

// --- reads ------------------------------------------------------------------

// load fetches a document and enforces read access.
func (h *Handler) load(r *http.Request) (*Document, error) {
	ctx := r.Context()

	publicID := chi.URLParam(r, "id")
	if !platform.ValidULID(publicID) {
		return nil, httpx.ErrNotFound("That file")
	}

	// Resolved across every client the caller can reach, then authorised
	// against the file's *own* client. Pinning to the resolved tenant meant an
	// agent viewing a ticket from the cross-client list could not open its
	// attachments: the file lives in the client's workspace, not the platform
	// one the header names.
	doc, err := h.svc.repo.ByPublicIDInReach(ctx, appctx.Reach(ctx), publicID)
	if err != nil {
		return nil, httpx.ErrNotFound("That file")
	}
	if err := h.authorise(ctx, doc.TenantID, doc, appctx.ActorFrom(ctx)); err != nil {
		return nil, err
	}
	return doc, nil
}

// authorise decides whether an actor may read one document.
//
// Fail-closed by construction: every branch is a reason to allow, and falling
// off the end is a refusal. The reply is NOT_FOUND rather than FORBIDDEN so the
// existence of another client's file is not confirmed.
func (h *Handler) authorise(ctx context.Context, tenantID int64, doc *Document, actor *appctx.Actor) error {
	if actor == nil {
		return httpx.ErrNotFound("That file")
	}
	denied := httpx.ErrNotFound("That file")

	// Whoever uploaded it can always read it back.
	if doc.UploadedBy.Valid && doc.UploadedBy.Int64 == actor.UserID {
		return nil
	}

	switch doc.OwnerType {
	case "USER":
		// A personal document belongs to that person and to whoever
		// administers people — not to the whole workspace.
		if doc.OwnerID.Valid && doc.OwnerID.Int64 == actor.UserID {
			return nil
		}
		if actor.Can("user.view.all") {
			return nil
		}
		return denied

	case "TENANT", "BRANDING":
		// Workspace assets: logos, letterheads, policy documents.
		return nil
	}

	// Anything else is reachable through the tickets it is attached to, which
	// covers both the TICKET owner type and a file uploaded loose and linked
	// afterwards.
	ticketIDs, err := h.tickets.TicketIDsForDocument(ctx, doc.TenantID, doc.ID)
	if err != nil {
		return httpx.ErrInternal(err)
	}
	if doc.OwnerType == "TICKET" && doc.OwnerID.Valid {
		ticketIDs = append(ticketIDs, doc.OwnerID.Int64)
	}
	for _, id := range ticketIDs {
		if h.tickets.CanSeeID(ctx, doc.TenantID, id, actor) {
			return nil
		}
	}
	return denied
}

func (h *Handler) show(w http.ResponseWriter, r *http.Request) {
	doc, err := h.load(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, present(doc))
}

func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	doc, err := h.load(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.stream(w, r, doc, "attachment", "DOWNLOAD")
}

func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	doc, err := h.load(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if !CanPreviewInline(doc.MimeType) {
		httpx.Fail(w, r, httpx.ErrField("id", "NOT_PREVIEWABLE",
			"This file type cannot be previewed in the browser. Download it instead."))
		return
	}
	h.stream(w, r, doc, "inline", "PREVIEW")
}

// stream decrypts a document to the response.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request, doc *Document, disposition, action string) {
	ctx := r.Context()

	body, err := h.svc.Open(ctx, doc.TenantID, doc)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	defer func() { _ = body.Close() }()

	h.svc.LogAccess(ctx, doc.TenantID, doc, action)

	// Inline rendering is only ever offered for types the browser can display
	// safely; everything else is forced to download so a stored HTML or SVG
	// file cannot execute against this origin.
	contentType := doc.MimeType
	if disposition == "inline" && !CanPreviewInline(contentType) {
		disposition, contentType = "attachment", "application/octet-stream"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(doc.SizeBytes, 10))
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("%s; filename=%q", disposition, safeFilename(doc.OriginalName)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// Attachments are tenant data; no shared cache may keep a copy.
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, body); err != nil {
		// The status line is already committed; there is nothing to report.
		return
	}
}

// safeFilename strips anything that could break out of the header or be read as
// a path when the browser saves the file.
func safeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 32, r == '"', r == '\\', r == '/', r == 0x7f:
			return '_'
		}
		return r
	}, name)
	if name == "" {
		return "download"
	}
	if len(name) > 120 {
		return name[:120]
	}
	return name
}

// --- signed links -----------------------------------------------------------

type signedURLRequest struct {
	Action  string `json:"action" validate:"omitempty,oneof=download preview"`
	Version int    `json:"version" validate:"omitempty,min=1"`
	TTL     int    `json:"ttl_seconds" validate:"omitempty,min=10,max=3600"`
}

func (h *Handler) signedURL(w http.ResponseWriter, r *http.Request) {
	var req signedURLRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	doc, err := h.load(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	action := req.Action
	if action == "" {
		action = "preview"
	}
	// A link to download must not be issuable by someone who may only look.
	if action == "download" && !appctx.ActorFrom(r.Context()).Can("document.download") {
		httpx.Fail(w, r, httpx.ErrForbidden("You do not have permission to download files."))
		return
	}

	// A link to an old revision must name it, or the signature would cover a
	// document whose contents can change under it.
	at, err := h.svc.AsOfVersion(r.Context(), doc, req.Version)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	url, err := h.svc.SignedURL(doc, action, req.Version,
		appctx.ActorFrom(r.Context()).UserID, durationOf(req.TTL))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	httpx.OK(w, r, map[string]any{
		"url": url, "file_name": at.OriginalName, "mime_type": at.MimeType,
		"size_bytes": at.SizeBytes, "version": at.Version,
	})
}

// signedRead serves a link minted by signedURL.
//
// There is no session here: a browser cannot put a bearer token on an <img src>
// or hand one to pdf.js. The signature is therefore the whole credential, and
// it is deliberately narrow — it names one document, one action, one viewer and
// one expiry, and it is minted only after that viewer passed the same
// authorisation check the session routes apply. The window is minutes, so a
// leaked link is a short-lived read of one file rather than an account.
//
// The tenant comes from the document, not from a header, because the browser
// cannot set one on a direct load. That is safe only because the signature
// already binds the document id.
func (h *Handler) signedRead(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	publicID := chi.URLParam(r, "id")
	action := strings.ToLower(chi.URLParam(r, "action"))
	if action != "download" && action != "preview" {
		httpx.Fail(w, r, httpx.ErrNotFound("That file"))
		return
	}

	q := r.URL.Query()
	expires, _ := strconv.ParseInt(q.Get("expires"), 10, 64)
	viewer, _ := strconv.ParseInt(q.Get("viewer"), 10, 64)
	version, _ := strconv.Atoi(q.Get("version"))
	if !platform.ValidULID(publicID) || viewer == 0 {
		httpx.Fail(w, r, httpx.ErrNotFound("That file"))
		return
	}

	// Verify before touching the database, so an unsigned request costs a
	// constant-time comparison and nothing else.
	if err := h.svc.VerifySignedURL(publicID, action, version, viewer, expires, q.Get("signature")); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	current, err := h.svc.repo.ByPublicIDAcrossTenants(ctx, publicID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That file"))
		return
	}
	doc, err := h.svc.AsOfVersion(ctx, current, version)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	disposition := "attachment"
	if action == "preview" && CanPreviewInline(doc.MimeType) {
		disposition = "inline"
	}
	// Mark the request so the access log records how the file was reached.
	h.stream(w, r.WithContext(appctx.WithSignedAccess(ctx)), doc, disposition, strings.ToUpper(action))
}

func durationOf(seconds int) time.Duration {
	if seconds <= 0 {
		return 0 // the service falls back to the configured TTL
	}
	return time.Duration(seconds) * time.Second
}

// --- versions ---------------------------------------------------------------

func (h *Handler) versions(w http.ResponseWriter, r *http.Request) {
	doc, err := h.load(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	rows, err := h.svc.repo.Versions(r.Context(), doc.TenantID, doc.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	type version struct {
		ID         string `json:"id"`
		Version    int    `json:"version"`
		FileName   string `json:"file_name"`
		MimeType   string `json:"mime_type"`
		SizeBytes  int64  `json:"size_bytes"`
		ChangeNote string `json:"change_note,omitempty"`
		CreatedAt  string `json:"created_at"`
	}
	out := make([]version, 0, len(rows))
	for _, v := range rows {
		out = append(out, version{
			ID: v.PublicID, Version: v.Version, FileName: v.OriginalName,
			MimeType: v.MimeType, SizeBytes: v.SizeBytes,
			ChangeNote: v.ChangeNote.String,
			CreatedAt:  v.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	httpx.OK(w, r, map[string]any{"items": out})
}

func (h *Handler) addVersion(w http.ResponseWriter, r *http.Request) {
	doc, err := h.load(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	if err := r.ParseMultipartForm(maxUploadForm); err != nil {
		httpx.Fail(w, r, httpx.ErrField("file", "INVALID", "Send the file as a multipart upload."))
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.Fail(w, r, httpx.ErrField("file", "REQUIRED", "Choose a file to upload."))
		return
	}
	defer func() { _ = file.Close() }()

	actor := appctx.ActorFrom(ctx)
	uploader := actor.UserID

	// Store the bytes through the same validation, sniffing and encryption a
	// first upload gets, but keep the storage object rather than creating a
	// second document row for it.
	obj, filename, mimeType, err := h.svc.Store(StoreParams{
		TenantSlug: tenantSlug(ctx), OwnerType: doc.OwnerType,
		Header: header, File: file,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	updated, err := h.svc.repo.AddVersion(ctx, doc.TenantID, doc.ID, CreateParams{
		TenantID: doc.TenantID, OriginalName: filename, MimeType: mimeType,
		Object: obj, UploadedBy: &uploader,
	}, strings.TrimSpace(r.FormValue("change_note")))
	if err != nil {
		// The bytes are on disk but unreferenced; remove them.
		h.svc.Discard(obj)
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionDocumentUploaded, EntityType: "document",
		EntityID: &doc.ID, EntityPublicID: doc.PublicID,
		After: map[string]any{"version": updated.Version, "file_name": updated.OriginalName},
	})
	httpx.Created(w, r, present(updated))
}

// --- delete -----------------------------------------------------------------

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	doc, err := h.load(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	// Soft delete only. The bytes stay until retention expires, because an
	// attachment can be evidence in a dispute long after someone tidies it out
	// of a ticket.
	if err := h.svc.repo.SoftDelete(ctx, doc.TenantID, doc.ID); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionDocumentDeleted, EntityType: "document",
		EntityID: &doc.ID, EntityPublicID: doc.PublicID,
		Before: map[string]any{"file_name": doc.OriginalName},
	})
	httpx.OK(w, r, map[string]any{"deleted": true})
}

// --- bulk download ----------------------------------------------------------

type bulkRequest struct {
	DocumentIDs []string `json:"document_ids" validate:"required,min=1,max=25,dive,len=26"`
}

// bulkDownload streams the selected attachments as one ZIP.
//
// Each file is authorised individually rather than trusting that a caller who
// can see one attachment can see them all — a selection is a list of ids from
// the browser, not a proof of access.
func (h *Handler) bulkDownload(w http.ResponseWriter, r *http.Request) {
	var req bulkRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	// Resolved across the caller's reach, like every other document read: a
	// bulk download from a cross-client ticket list spans clients.
	reach := appctx.Reach(ctx)

	if len(req.DocumentIDs) > maxBulkDocuments {
		httpx.Fail(w, r, httpx.ErrField("document_ids", "TOO_MANY",
			fmt.Sprintf("Select %d files or fewer.", maxBulkDocuments)))
		return
	}

	docs := make([]*Document, 0, len(req.DocumentIDs))
	for _, id := range req.DocumentIDs {
		if !platform.ValidULID(id) {
			continue
		}
		doc, err := h.svc.repo.ByPublicIDInReach(ctx, reach, id)
		if err != nil {
			continue
		}
		if err := h.authorise(ctx, doc.TenantID, doc, actor); err != nil {
			continue
		}
		docs = append(docs, doc)
	}
	if len(docs) == 0 {
		httpx.Fail(w, r, httpx.ErrNotFound("Those files"))
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="attachments.zip"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)

	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	seen := map[string]int{}
	for _, doc := range docs {
		body, err := h.svc.ReadAll(ctx, doc.TenantID, doc)
		if err != nil {
			// One unreadable file must not abort a ten-file download.
			continue
		}

		// Two attachments can share a name; a ZIP with duplicate entries
		// extracts unpredictably.
		name := safeFilename(doc.OriginalName)
		seen[name]++
		if n := seen[name]; n > 1 {
			name = fmt.Sprintf("%d-%s", n, name)
		}

		entry, err := zw.Create(name)
		if err != nil {
			return
		}
		if _, err := entry.Write(body); err != nil {
			return
		}
		h.svc.LogAccess(ctx, doc.TenantID, doc, "DOWNLOAD")
	}
}

// --- access log -------------------------------------------------------------

func (h *Handler) accessLog(w http.ResponseWriter, r *http.Request) {
	doc, err := h.load(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// Who read a file is administrative information, not part of reading it.
	if !appctx.ActorFrom(r.Context()).Can("audit.view") {
		httpx.Fail(w, r, httpx.ErrForbidden("You do not have permission to view access history."))
		return
	}

	rows, err := h.svc.repo.AccessLog(r.Context(), doc.TenantID, doc.ID, 200)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, map[string]any{"items": rows})
}

// tenantSlug returns the resolved workspace slug, which storage uses as the
// top-level directory and as additional authenticated data when wrapping each
// file's key. A file written under one slug cannot be decrypted under another.
func tenantSlug(ctx context.Context) string {
	if t := appctx.TenantFrom(ctx); t != nil {
		return t.Slug
	}
	return ""
}
