package notification

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/platform"
)

type Handler struct {
	repo *Repository
}

func NewHandler(repo *Repository) *Handler { return &Handler{repo: repo} }

// Routes mounts the notification surface.
//
// The feed itself carries no permission gate: everyone may read their own
// notifications, and how much further than "their own" a caller can see is
// decided by audienceFor from the session, not by a route. Template
// administration is gated, because that is configuration rather than reading.
func (h *Handler) Routes(r chi.Router) {
	templates := middleware.RequireAnyPermission("config.notification", "config.settings")

	r.Route("/notifications", func(r chi.Router) {
		r.Get("/", h.list)
		r.Get("/unread-count", h.unreadCount)
		r.Post("/read-all", h.markAllRead)

		r.Get("/preferences", h.preferences)
		r.Put("/preferences", h.savePreferences)
		r.Get("/events", h.events)

		r.With(templates).Get("/templates", h.listTemplates)
		r.With(templates).Get("/templates/{id}", h.getTemplate)
		r.With(templates).Post("/templates", h.saveTemplate)
		r.With(templates).Patch("/templates/{id}", h.patchTemplate)
		r.With(templates).Post("/templates/{id}/preview", h.previewTemplate)
		r.With(templates).Post("/templates/{id}/test", h.testTemplate)

		// Last, so `read-all`, `preferences` and `events` are not swallowed by
		// the id pattern.
		r.Post("/{id}/read", h.markRead)
	})
}

// --- audience ---------------------------------------------------------------

// audienceFor derives what this caller may read. Never taken from the request:
// the only thing a caller may ask for is a *narrower* view (`scope=mine`).
func (h *Handler) audienceFor(r *http.Request, actor *appctx.Actor) (Audience, error) {
	a := Audience{UserID: actor.UserID}

	// The bell always asks for its own rows, whatever the role.
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("scope")), "mine") {
		a.Mine = true
		return a, nil
	}

	ctx := r.Context()

	switch {
	case actor.IsSuperAdmin:
		// The admin sees every client's stream.
		a.AllTenants = true

	case actor.IsStaff:
		// An agent assigned to specific clients sees those clients; an agent
		// with no assignment covers the whole desk and sees all of them.
		if len(actor.AssignedTenantIDs) > 0 {
			a.TenantIDs = actor.AssignedTenantIDs
		} else {
			a.AllTenants = true
		}

	case actor.Can("ticket.view.all"):
		// A client admin: the whole of their own client, and nothing outside it.
		a.TenantIDs = []int64{actor.TenantID}

	case actor.Can("ticket.view.scope"):
		// A partner executive: their client, narrowed to the people inside the
		// entities, sites and departments allocated to them.
		a.TenantIDs = []int64{actor.TenantID}
		ids, err := h.repo.RecipientsInScope(ctx, actor.TenantID,
			actor.Scopes.Entities, actor.Scopes.Sites, actor.Scopes.Departments)
		if err != nil {
			return a, httpx.ErrInternal(err)
		}
		a.RecipientIDs = ids

	default:
		// An employee sees their own and nothing else.
		a.Mine = true
	}
	return a, nil
}

// --- the feed ---------------------------------------------------------------

type notificationResponse struct {
	ID         string `json:"id"`
	EventKey   string `json:"event_key"`
	Group      string `json:"group,omitempty"`
	Title      string `json:"title"`
	Body       string `json:"body,omitempty"`
	Link       string `json:"link,omitempty"`
	EntityType string `json:"entity_type,omitempty"`
	// TargetMissing marks a notification whose subject has since been removed.
	// The link is dropped rather than offered: following it would answer "not
	// found", which reads as "you may not see this" and is the wrong thing to
	// tell someone about a ticket that simply no longer exists.
	TargetMissing bool    `json:"target_missing,omitempty"`
	IsRead        bool    `json:"is_read"`
	ReadAt        *string `json:"read_at"`
	CreatedAt     string  `json:"created_at"`

	// Who it was addressed to, and which client it belongs to. Both matter only
	// in the wider staff feed, where a row may not be the caller's own.
	RecipientName string `json:"recipient_name,omitempty"`
	IsMine        bool   `json:"is_mine"`
	ClientName    string `json:"client_name,omitempty"`
	ClientSlug    string `json:"client_slug,omitempty"`
}

func toResponse(n Notification, callerID int64) notificationResponse {
	out := notificationResponse{
		ID: n.PublicID, EventKey: n.EventKey, Title: n.Title,
		Body: n.Body.String, Link: n.Link.String,
		EntityType:    n.EntityType.String,
		Group:         n.EventGroup.String,
		IsRead:        n.ReadAt.Valid,
		RecipientName: strings.TrimSpace(n.RecipientName.String),
		IsMine:        n.UserID == callerID,
		ClientName:    n.ClientName.String,
		ClientSlug:    n.ClientSlug.String,
		CreatedAt:     n.CreatedAt.UTC().Format(time.RFC3339),
	}
	if n.ReadAt.Valid {
		s := n.ReadAt.Time.UTC().Format(time.RFC3339)
		out.ReadAt = &s
	}

	// A notification about something that no longer exists stays in the feed as
	// history — it is a true record of what happened — but stops being a link.
	if n.EntityType.String == "ticket" && !n.TargetPublicID.Valid {
		out.Link, out.TargetMissing = "", true
	}
	return out
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	audience, err := h.audienceFor(r, actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	page := platform.ParsePage(r, map[string]string{"created_at": "n.created_at"}, "n.created_at")
	unreadOnly := false
	if v := platform.QueryBool(r, "unread_only"); v != nil {
		unreadOnly = *v
	}

	rows, total, err := h.repo.List(ctx, audience, unreadOnly,
		r.URL.Query().Get("group"), r.URL.Query().Get("q"), page)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]notificationResponse, 0, len(rows))
	for _, n := range rows {
		out = append(out, toResponse(n, actor.UserID))
	}
	httpx.List(w, r, out, platform.NewMeta(page, total))
}

func (h *Handler) unreadCount(w http.ResponseWriter, r *http.Request) {
	actor := appctx.ActorFrom(r.Context())
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}
	n, err := h.repo.UnreadCount(r.Context(), actor.UserID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, map[string]any{"count": n})
}

func (h *Handler) markRead(w http.ResponseWriter, r *http.Request) {
	actor := appctx.ActorFrom(r.Context())
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}
	if err := h.repo.MarkRead(r.Context(), actor.UserID, chi.URLParam(r, "id")); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, map[string]any{"message": "Marked as read."})
}

func (h *Handler) markAllRead(w http.ResponseWriter, r *http.Request) {
	actor := appctx.ActorFrom(r.Context())
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}
	n, err := h.repo.MarkAllRead(r.Context(), actor.UserID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, map[string]any{"marked": n})
}

// --- the catalogue ----------------------------------------------------------

type eventResponse struct {
	Key      string   `json:"key"`
	Name     string   `json:"name"`
	Group    string   `json:"group"`
	Channels []string `json:"channels"`
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	// The caller's own portal, from the session — not the query string, or a
	// client-side user could ask for the desk's event list.
	rows, err := h.repo.Events(r.Context(), string(appctx.PortalFrom(r.Context())))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	out := make([]eventResponse, 0, len(rows))
	for _, e := range rows {
		out = append(out, eventResponse{
			Key: e.Key, Name: e.Description, Group: e.Group,
			Channels: e.DefaultChannels(),
		})
	}
	httpx.OK(w, r, map[string]any{"items": out})
}

// --- preferences ------------------------------------------------------------

// preferencesResponse is the shape the preferences matrix reads: a per-event
// map of channel booleans, plus which channels the client has switched on so
// the screen can grey out the rest.
type preferencesResponse struct {
	Events          map[string]map[string]bool `json:"events"`
	EnabledChannels []string                   `json:"enabled_channels"`
	Digest          string                     `json:"digest"`
	MutedUntil      *string                    `json:"muted_until"`
}

func (h *Handler) preferences(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	// Narrowed to the caller's portal: the matrix must offer only switches that
	// can actually do something.
	events, err := h.repo.Events(ctx, string(appctx.PortalFrom(ctx)))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	stored, err := h.repo.Preferences(ctx, actor.UserID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	channels, err := h.repo.EnabledChannels(ctx, appctx.TenantID(ctx))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	// Start from each event's own defaults, then lay the user's stored choices
	// on top. A user who has never opened this screen therefore sees the
	// defaults they are actually being notified under, not an empty grid.
	out := preferencesResponse{
		Events:          make(map[string]map[string]bool, len(events)),
		EnabledChannels: channels,
		Digest:          "OFF",
	}
	for _, e := range events {
		row := make(map[string]bool, len(APIChannels))
		defaults := e.DefaultChannels()
		for _, c := range APIChannels {
			row[c] = false
			for _, d := range defaults {
				if d == c {
					row[c] = true
				}
			}
		}
		out.Events[e.Key] = row
	}

	for _, p := range stored {
		row, ok := out.Events[p.EventKey]
		if !ok {
			// A preference for an event no longer in the catalogue: ignore it
			// rather than surfacing a row the screen has no label for.
			continue
		}
		row[apiChannel(p.Channel)] = p.Enabled

		if p.Digest != "" && p.Digest != "NONE" {
			out.Digest = p.Digest
		}
		if p.MutedUntil.Valid {
			s := p.MutedUntil.Time.UTC().Format(time.RFC3339)
			out.MutedUntil = &s
		}
	}

	httpx.OK(w, r, out)
}

func (h *Handler) savePreferences(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Events          map[string]map[string]bool `json:"events"`
		Digest          string                     `json:"digest" validate:"omitempty,oneof=OFF DAILY WEEKLY"`
		MutedUntil      *string                    `json:"muted_until"`
		EnabledChannels []string                   `json:"enabled_channels"`
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

	// Only events that actually exist may be written, so a stale or hand-rolled
	// payload cannot fill the table with keys nothing will ever send.
	known := map[string]struct{}{}
	// Narrowed to the caller's portal: the matrix must offer only switches that
	// can actually do something.
	events, err := h.repo.Events(ctx, string(appctx.PortalFrom(ctx)))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	for _, e := range events {
		known[e.Key] = struct{}{}
	}

	updates := make([]PreferenceUpdate, 0, len(req.Events)*len(APIChannels))
	for eventKey, channels := range req.Events {
		if _, ok := known[eventKey]; !ok {
			continue
		}
		for channel, enabled := range channels {
			stored, ok := storedChannel(channel)
			if !ok {
				httpx.Fail(w, r, httpx.ErrField("events", "INVALID",
					"Unknown notification channel "+channel+"."))
				return
			}
			updates = append(updates, PreferenceUpdate{
				EventKey: eventKey, Channel: stored, Enabled: enabled,
			})
		}
	}
	// Deterministic order keeps the write log readable and the transaction's
	// lock order stable between concurrent saves.
	sort.Slice(updates, func(i, j int) bool {
		if updates[i].EventKey != updates[j].EventKey {
			return updates[i].EventKey < updates[j].EventKey
		}
		return updates[i].Channel < updates[j].Channel
	})

	digest := strings.ToUpper(strings.TrimSpace(req.Digest))
	if digest == "" {
		digest = "OFF"
	}

	var mutedUntil *time.Time
	clearMute := req.MutedUntil == nil || strings.TrimSpace(*req.MutedUntil) == ""
	if !clearMute {
		t, err := time.Parse(time.RFC3339, *req.MutedUntil)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("muted_until", "INVALID",
				"muted_until must be an RFC3339 timestamp."))
			return
		}
		utc := t.UTC()
		mutedUntil = &utc
	}

	if err := h.repo.SavePreferences(ctx, appctx.TenantID(ctx), actor.UserID,
		updates, digest, mutedUntil, clearMute); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.preferences(w, r)
}

// --- templates --------------------------------------------------------------

type templateResponse struct {
	ID       string `json:"id"`
	EventKey string `json:"event_key"`
	Channel  string `json:"channel"`
	Subject  string `json:"subject"`
	BodyHTML string `json:"body_html"`
	BodyText string `json:"body_text"`
	IsActive bool   `json:"is_active"`
	// IsDefault marks the platform-wide wording. A client edit creates their own
	// row rather than changing it, so the default can always be restored.
	IsDefault bool   `json:"is_default"`
	UpdatedAt string `json:"updated_at"`
}

func toTemplateResponse(t Template) templateResponse {
	return templateResponse{
		ID: t.PublicID, EventKey: t.EventKey, Channel: apiChannel(t.Channel),
		Subject: t.Subject.String, BodyHTML: t.BodyHTML.String, BodyText: t.BodyText.String,
		IsActive: t.IsActive, IsDefault: !t.TenantID.Valid,
		UpdatedAt: t.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (h *Handler) listTemplates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	channel := ""
	if raw := strings.TrimSpace(r.URL.Query().Get("channel")); raw != "" {
		stored, ok := storedChannel(strings.ToLower(raw))
		if !ok {
			httpx.Fail(w, r, httpx.ErrField("channel", "INVALID", "Unknown channel."))
			return
		}
		channel = stored
	}

	rows, err := h.repo.Templates(ctx, appctx.TenantID(ctx),
		strings.TrimSpace(r.URL.Query().Get("event_key")), channel)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	// Collapse to the effective template per event/channel: the client's own
	// row when it exists, the platform default otherwise. Templates() already
	// orders overrides first, so the first row for a pair wins.
	seen := map[string]struct{}{}
	out := make([]templateResponse, 0, len(rows))
	for _, t := range rows {
		key := t.EventKey + "|" + t.Channel
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, toTemplateResponse(t))
	}
	httpx.OK(w, r, map[string]any{"items": out})
}

func (h *Handler) getTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := h.repo.TemplateByPublicID(ctx, appctx.TenantID(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That template"))
		return
	}
	httpx.OK(w, r, toTemplateResponse(*t))
}

type templateRequest struct {
	EventKey string `json:"event_key" validate:"required,notblank,max=64"`
	Channel  string `json:"channel" validate:"required,oneof=in_app email sms push"`
	Subject  string `json:"subject" validate:"omitempty,max=255"`
	BodyHTML string `json:"body_html"`
	BodyText string `json:"body_text"`
	IsActive *bool  `json:"is_active"`
}

func (h *Handler) saveTemplate(w http.ResponseWriter, r *http.Request) {
	var req templateRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.upsert(w, r, req)
}

// patchTemplate edits an existing template by id. Editing a platform default
// writes the client's own override instead, which is what keeps one client's
// wording from changing every other client's.
func (h *Handler) patchTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Subject  *string `json:"subject" validate:"omitempty,max=255"`
		BodyHTML *string `json:"body_html"`
		BodyText *string `json:"body_text"`
		IsActive *bool   `json:"is_active"`
	}
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	existing, err := h.repo.TemplateByPublicID(ctx, appctx.TenantID(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That template"))
		return
	}

	merged := templateRequest{
		EventKey: existing.EventKey,
		Channel:  apiChannel(existing.Channel),
		Subject:  existing.Subject.String,
		BodyHTML: existing.BodyHTML.String,
		BodyText: existing.BodyText.String,
		IsActive: &existing.IsActive,
	}
	if req.Subject != nil {
		merged.Subject = *req.Subject
	}
	if req.BodyHTML != nil {
		merged.BodyHTML = *req.BodyHTML
	}
	if req.BodyText != nil {
		merged.BodyText = *req.BodyText
	}
	if req.IsActive != nil {
		merged.IsActive = req.IsActive
	}
	h.upsert(w, r, merged)
}

func (h *Handler) upsert(w http.ResponseWriter, r *http.Request, req templateRequest) {
	ctx := r.Context()
	stored, ok := storedChannel(req.Channel)
	if !ok {
		httpx.Fail(w, r, httpx.ErrField("channel", "INVALID", "Unknown channel."))
		return
	}

	var by *int64
	if actor := appctx.ActorFrom(ctx); actor != nil {
		by = &actor.UserID
	}

	t, err := h.repo.SaveTemplate(ctx, appctx.TenantID(ctx), TemplateParams{
		EventKey: req.EventKey, Channel: stored,
		Subject: req.Subject, BodyHTML: req.BodyHTML, BodyText: req.BodyText,
		IsActive: req.IsActive == nil || *req.IsActive,
		SavedBy:  by,
	})
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, toTemplateResponse(*t))
}

// sampleVars are the placeholder values the preview and test-send substitute,
// so an administrator sees the shape of a real message rather than raw tokens.
var sampleVars = map[string]any{
	"ticket_number":  "PF-2026-000412",
	"subject":        "PF withdrawal — Form 19 rejected",
	"status":         "IN_PROGRESS",
	"priority":       "HIGH",
	"recipient_name": "Priya Nair",
	"requester_name": "Rahul Menon",
	"assignee_name":  "Anita Desai",
	"client_name":    "Ampersand Group",
	"entity_name":    "Ampersand Manufacturing Pvt Ltd",
	"due_at":         time.Now().UTC().Add(24 * time.Hour).Format("02 Jan 2006, 15:04"),
	"ticket_id":      "01JBQ8Z9X1A2B3C4D5E6F7G8H9",
}

func (h *Handler) previewTemplate(w http.ResponseWriter, r *http.Request) {
	// Preview may be asked for wording that has not been saved yet, so the body
	// overrides the stored row when present.
	var req struct {
		Subject  string         `json:"subject"`
		BodyHTML string         `json:"body_html"`
		BodyText string         `json:"body_text"`
		Vars     map[string]any `json:"vars"`
	}
	_ = httpx.Decode(r, &req)

	ctx := r.Context()
	t, err := h.repo.TemplateByPublicID(ctx, appctx.TenantID(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That template"))
		return
	}

	subject, bodyHTML, bodyText := t.Subject.String, t.BodyHTML.String, t.BodyText.String
	if req.Subject != "" {
		subject = req.Subject
	}
	if req.BodyHTML != "" {
		bodyHTML = req.BodyHTML
	}
	if req.BodyText != "" {
		bodyText = req.BodyText
	}

	vars := map[string]any{}
	for k, v := range sampleVars {
		vars[k] = v
	}
	for k, v := range req.Vars {
		vars[k] = v
	}

	httpx.OK(w, r, map[string]any{
		"subject":   Render(subject, vars),
		"body_html": Render(bodyHTML, vars),
		"body_text": Render(bodyText, vars),
		"vars":      vars,
	})
}

// testTemplate delivers a rendered sample to the caller's own in-app feed.
//
// It writes to the caller and nobody else, so a test can never reach a real
// user, and it goes through the same notifications table the bell reads — which
// is the only way to prove the whole path works.
func (h *Handler) testTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	t, err := h.repo.TemplateByPublicID(ctx, appctx.TenantID(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That template"))
		return
	}

	vars := map[string]any{}
	for k, v := range sampleVars {
		vars[k] = v
	}
	vars["recipient_name"] = actor.FullName

	subject := Render(t.Subject.String, vars)
	if subject == "" {
		subject = defaultSubject(t.EventKey)
	}
	body := Render(t.BodyText.String, vars)

	if err := h.repo.WriteTest(ctx, appctx.TenantID(ctx), actor.UserID, t.EventKey, subject, body); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, map[string]any{
		"message": "A test notification was sent to your own inbox.",
		"subject": subject,
		"body":    body,
	})
}
