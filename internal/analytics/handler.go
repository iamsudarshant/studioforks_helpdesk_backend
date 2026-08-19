package analytics

import (
	"context"
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
	"github.com/karmamgmt/complydesk/internal/org"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/ticket"
)

type Handler struct {
	repo    *Repository
	tickets *ticket.Service
	// org supplies the entity and department lists the report filter bar
	// offers. Read-only, and always through the caller's own reach.
	org *org.Repository
}

func NewHandler(repo *Repository, tickets *ticket.Service, orgRepo *org.Repository) *Handler {
	return &Handler{repo: repo, tickets: tickets, org: orgRepo}
}

// Routes mounts the dashboard and report surface.
//
// Everything here is a read of data the caller can already reach — the scope is
// re-derived from the session on every request — so the gate is "may see
// tickets" plus, for reports, the export permission.
func (h *Handler) Routes(r chi.Router) {
	view := middleware.RequireAnyPermission("dashboard.view", "ticket.view.own", "ticket.view.scope", "ticket.view.all")
	report := middleware.RequireAnyPermission("report.view", "report.export")

	r.Route("/dashboard", func(r chi.Router) {
		r.With(view).Get("/summary", h.summary)
		r.With(view).Get("/widgets", h.widgets)
		r.With(view).Get("/charts/{key}", h.chart)
	})

	r.Route("/reports", func(r chi.Router) {
		r.With(report).Get("/definitions", h.definitions)
		// What the filter bar offers: the entities, departments and categories
		// this caller may actually narrow by. Derived from their own reach, so a
		// partner is never offered another client's establishments.
		r.With(report).Get("/filters", h.reportFilters)
		r.With(report).Get("/{key}", h.runReport)
		// The download is the same query rendered as a file, so it shares the
		// permission rather than inventing a second one.
		r.With(middleware.RequirePermission("report.export")).Get("/{key}/export", h.exportReport)
	})
}

// scope re-derives what this caller may count. Never taken from the request.
func (h *Handler) scope(r *http.Request) ticket.Scope {
	return h.tickets.ScopeFor(appctx.ActorFrom(r.Context()))
}

// reach re-derives which clients this caller's numbers cover: the one selected
// in the switcher, or every client they can reach when none is.
func (h *Handler) reach(r *http.Request) appctx.ClientReach {
	return appctx.Reach(r.Context())
}

// kpi is one card in the strip. `Link` deep-links into a pre-filtered ticket
// list, which is what makes the number actionable rather than decorative.
type kpi struct {
	Key   string         `json:"key"`
	Label string         `json:"label"`
	Value any            `json:"value"`
	Unit  string         `json:"unit,omitempty"`
	Tone  string         `json:"tone,omitempty"`
	Link  map[string]any `json:"link,omitempty"`
}

// window reads the dashboard date range off the query string.
//
// Two shapes are accepted, because the screen has two ways of asking:
//
//	?range=today|last7|last30|last90|mtd|ytd|wtd|all   a named preset
//	?from=2026-08-01&to=2026-08-18                     an explicit range
//
// Explicit dates win when both are present, so a custom range is never
// second-guessed by a stale preset left in the URL. `to` is inclusive of the
// whole day named — a range ending "today" that stopped at midnight would omit
// everything raised since, which is most of what the reader is looking for.
//
// An unparseable value is ignored rather than rejected: a dashboard is a
// read-only overview, and failing the whole page because one query parameter is
// malformed serves nobody. The unfiltered view is the safe fallback.
func window(r *http.Request) *Window {
	q := r.URL.Query()
	w := &Window{}

	day := func(key string) *time.Time {
		raw := strings.TrimSpace(q.Get(key))
		if raw == "" {
			return nil
		}
		d, err := time.Parse("2006-01-02", raw)
		if err != nil {
			return nil
		}
		return &d
	}

	from, to := day("from"), day("to")
	if to != nil {
		end := to.AddDate(0, 0, 1)
		to = &end
	}
	if from != nil || to != nil {
		w.From, w.To = from, to
		return w
	}

	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	switch strings.TrimSpace(q.Get("range")) {
	case "today":
		w.From = &midnight
	case "wtd":
		// Monday, the week Indian payroll and compliance calendars start on.
		offset := (int(midnight.Weekday()) + 6) % 7
		start := midnight.AddDate(0, 0, -offset)
		w.From = &start
	case "mtd":
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		w.From = &start
	case "ytd":
		start := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		w.From = &start
	case "last7":
		start := midnight.AddDate(0, 0, -6)
		w.From = &start
	case "last30":
		start := midnight.AddDate(0, 0, -29)
		w.From = &start
	case "last90":
		start := midnight.AddDate(0, 0, -89)
		w.From = &start
	case "all", "":
		// No range asked for, or "everything" asked for explicitly.
	}
	return w
}

func (h *Handler) summary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)

	win := window(r)
	out, err := h.repo.Summary(ctx, h.reach(r), h.scope(r), win)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	users, err := h.repo.UserCounts(ctx, h.reach(r))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	to := func(status string) map[string]any {
		return map[string]any{"path": "tickets", "query": map[string]any{"status": status}}
	}

	kpis := []kpi{
		{Key: "total", Label: "All tickets", Value: out.Total,
			Link: map[string]any{"path": "tickets"}},
		{Key: "open", Label: "Open", Value: out.Open, Tone: "info",
			Link: map[string]any{"path": "tickets", "query": map[string]any{"quick": "open"}}},
		{Key: "pending_employee", Label: "Awaiting employee", Value: out.PendingEmployee,
			Tone: "warning", Link: to("PENDING_EMPLOYEE")},
		{Key: "pending_helpdesk", Label: "Awaiting helpdesk", Value: out.PendingHelpdesk,
			Tone: "warning", Link: to("PENDING_HELPDESK")},
		{Key: "escalated", Label: "Escalated", Value: out.Escalated, Tone: "critical",
			Link: to("ESCALATED")},
		{Key: "resolved", Label: "Resolved", Value: out.Resolved, Tone: "success",
			Link: to("RESOLVED")},
		{Key: "closed", Label: "Closed", Value: out.Closed, Link: to("CLOSED")},
	}

	// Numbers only the desk can act on. An employee looking at their own tickets
	// has no use for a headcount or an SLA percentage across the client.
	if actor.Can("ticket.view.all") || actor.Can("ticket.view.scope") {
		kpis = append(kpis,
			kpi{Key: "unassigned", Label: "Unassigned", Value: out.Unassigned, Tone: "warning",
				Link: map[string]any{"path": "tickets", "query": map[string]any{"unassigned": true}}},
			kpi{Key: "breached", Label: "SLA breached", Value: out.Breached, Tone: "critical",
				Link: map[string]any{"path": "tickets", "query": map[string]any{"breached": true}}},
		)
		if out.AvgResolutionHours != nil {
			kpis = append(kpis, kpi{Key: "avg_tat", Label: "Average resolution",
				Value: round1(*out.AvgResolutionHours), Unit: "h"})
		}
		if out.SLACompliancePct != nil {
			kpis = append(kpis, kpi{Key: "sla", Label: "SLA compliance",
				Value: round1(*out.SLACompliancePct), Unit: "%", Tone: "success"})
		}
	}

	// Headcount belongs to whoever administers people, not to everyone.
	if actor.Can("user.view.all") {
		kpis = append(kpis,
			kpi{Key: "employees", Label: "Employees", Value: users.Active,
				Link: map[string]any{"path": "users"}},
			kpi{Key: "ex_employees", Label: "Ex-employees", Value: users.ExEmployees,
				Link: map[string]any{"path": "users", "query": map[string]any{"status": "EX_EMPLOYEE"}}},
		)
	}

	// The client breakdown only means something when the numbers span more than
	// one client. Asking for it in the single-client case would be a table of
	// one row restating the KPI strip.
	body := map[string]any{
		"kpis":         kpis,
		"generated_at": time.Now().UTC(),
		// The raw counts stay available for anything that wants the numbers
		// without the presentation.
		"totals": out,
		"users":  users,
		"scope":  h.scopeDescriptor(r),
	}

	if appctx.SelectedClientID(ctx) == 0 && actor.Can("ticket.view.all") {
		clients, err := h.repo.ByClient(ctx, h.reach(r), h.scope(r), win)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrInternal(err))
			return
		}
		body["clients"] = clients
	}

	httpx.OK(w, r, body)
}

// scopeDescriptor tells the client what the numbers on screen actually cover,
// so the dashboard can title itself "All clients" or name the one selected
// rather than leaving the reader to guess.
func (h *Handler) scopeDescriptor(r *http.Request) map[string]any {
	ctx := r.Context()
	reach := appctx.Reach(ctx)

	if appctx.SelectedClientID(ctx) != 0 {
		out := map[string]any{"kind": "client"}
		if t := appctx.TenantFrom(ctx); t != nil {
			out["client_name"] = t.Name
			out["client_slug"] = t.Slug
		}
		return out
	}
	if reach.All {
		return map[string]any{"kind": "all_clients"}
	}
	return map[string]any{"kind": "assigned_clients", "client_count": len(reach.TenantIDs)}
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

// widget describes one card on the dashboard. The client renders whatever it is
// given, so adding a widget is a change here and nowhere else.
//
// `Permission` is echoed back rather than assumed: the client hides a widget it
// cannot fill, and the endpoint behind it re-checks anyway. `Order` exists
// because JSON arrays are ordered but the client sorts defensively.
type widget struct {
	Key        string `json:"key"`
	Title      string `json:"title"`
	Kind       string `json:"kind"`
	ChartType  string `json:"chart_type,omitempty"`
	Endpoint   string `json:"endpoint,omitempty"`
	Permission string `json:"permission,omitempty"`
	Order      int    `json:"order"`
	Span       int    `json:"span"`
}

func (h *Handler) widgets(w http.ResponseWriter, r *http.Request) {
	actor := appctx.ActorFrom(r.Context())

	// The KPI strip first, then the plots. An employee sees only their own
	// tickets, so a per-agent breakdown would be a chart of one person and a
	// per-entity one a chart of one entity.
	out := []widget{
		{Key: "kpis", Title: "Overview", Kind: "KPI", Order: 1, Span: 12},
		{Key: "status", Title: "Tickets by status", Kind: "CHART", ChartType: "pie",
			Endpoint: "/dashboard/charts/status", Order: 2, Span: 4},
		{Key: "trend", Title: "Raised and resolved", Kind: "CHART", ChartType: "line",
			Endpoint: "/dashboard/charts/trend", Order: 3, Span: 8},
		{Key: "priority", Title: "By priority", Kind: "CHART", ChartType: "bar",
			Endpoint: "/dashboard/charts/priority", Order: 4, Span: 4},
	}

	if actor.Can("ticket.view.all") || actor.Can("ticket.view.scope") {
		out = append(out,
			widget{Key: "category", Title: "By category", Kind: "CHART", ChartType: "donut",
				Endpoint: "/dashboard/charts/category", Order: 5, Span: 4},
			widget{Key: "entity", Title: "By establishment", Kind: "CHART", ChartType: "bar",
				Endpoint: "/dashboard/charts/entity", Order: 6, Span: 4},
			widget{Key: "site", Title: "By location", Kind: "CHART", ChartType: "bar",
				Endpoint: "/dashboard/charts/site", Order: 7, Span: 6},
		)
	}
	// A per-agent breakdown only means something to someone who sees the whole
	// client; for anyone else every bar would be themselves.
	if actor.Can("ticket.view.all") {
		out = append(out, widget{Key: "assignee", Title: "Workload by executive",
			Kind: "CHART", ChartType: "bar", Endpoint: "/dashboard/charts/assignee",
			Order: 8, Span: 6})
	}

	httpx.OK(w, r, map[string]any{"items": out})
}

func (h *Handler) chart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	key := chi.URLParam(r, "key")

	// The client's chart components read {rows, series}: rows are the plotted
	// records and series names the numeric keys within them. Shaping it here
	// rather than in the browser means a new chart is a backend change only.
	if key == "trend" {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		points, err := h.repo.Trend(ctx, h.reach(r), h.scope(r), days, window(r))
		if err != nil {
			httpx.Fail(w, r, httpx.ErrInternal(err))
			return
		}
		rows := make([]map[string]any, 0, len(points))
		for _, p := range points {
			rows = append(rows, map[string]any{
				"label": p.Day, "raised": p.Raised, "resolved": p.Resolved,
			})
		}
		httpx.OK(w, r, map[string]any{
			"key":  key,
			"rows": rows,
			"series": []map[string]string{
				{"key": "raised", "label": "Raised"},
				{"key": "resolved", "label": "Resolved"},
			},
		})
		return
	}

	buckets, err := h.repo.GroupBy(ctx, h.reach(r), h.scope(r), key, window(r))
	if err != nil {
		// An unknown dimension is a client mistake, not a server fault, and
		// naming the valid ones saves a support round trip.
		httpx.Fail(w, r, httpx.ErrField("key", "UNKNOWN",
			"Unknown chart. Valid keys: "+strings.Join(append(Dimensions, "trend"), ", ")))
		return
	}

	rows := make([]map[string]any, 0, len(buckets))
	for _, b := range buckets {
		rows = append(rows, map[string]any{"key": b.Key, "label": b.Label, "count": b.Count})
	}
	httpx.OK(w, r, map[string]any{
		"key":    key,
		"rows":   rows,
		"series": []map[string]string{{"key": "count", "label": "Tickets"}},
	})
}

func (h *Handler) definitions(w http.ResponseWriter, r *http.Request) {
	// The build closure is unexported, so the slice serialises to exactly the
	// descriptive fields and never the SQL.
	httpx.OK(w, r, map[string]any{"definitions": Definitions})
}

// params reads the reporting window and every filter the screen offers.
//
// Values are validated here and passed as bound arguments; nothing from the
// query string reaches the SQL text. Ids arrive as ULIDs and are resolved to
// internal ids by the caller, which is also what confines a filter to records
// the caller can see.
func params(r *http.Request) (Params, error) {
	var p Params
	q := r.URL.Query()

	p.Statuses = platform.QueryStrings(r, "status")
	p.Priorities = platform.QueryStrings(r, "priority")
	if v := platform.QueryBool(r, "breached"); v != nil {
		p.BreachedOnly = *v
	}

	parse := func(key string, endOfDay bool) (*time.Time, error) {
		raw := strings.TrimSpace(q.Get(key))
		if raw == "" {
			return nil, nil
		}
		t, err := time.Parse("2006-01-02", raw)
		if err != nil {
			return nil, fmt.Errorf("%s must be a date in YYYY-MM-DD form", key)
		}
		if endOfDay {
			// `to=2026-08-05` should include that whole day, not stop at
			// midnight — otherwise a one-day report is always empty.
			t = t.Add(24*time.Hour - time.Nanosecond)
		}
		return &t, nil
	}

	var err error
	if p.From, err = parse("from", false); err != nil {
		return p, err
	}
	if p.To, err = parse("to", true); err != nil {
		return p, err
	}
	return p, nil
}

func (h *Handler) run(r *http.Request) (*Result, error) {
	p, err := params(r)
	if err != nil {
		return nil, httpx.ErrField("from", "INVALID", err.Error())
	}

	key := chi.URLParam(r, "key")
	if _, ok := DefinitionByKey(key); !ok {
		return nil, httpx.ErrNotFound("That report")
	}

	ctx := r.Context()

	// Entity and department filters arrive as ULIDs. Resolving them through the
	// caller's own reach is what stops a filter naming another client's records:
	// an id outside that reach simply does not resolve.
	if ids := platform.QueryStrings(r, "entity_id"); len(ids) > 0 {
		resolved, err := h.resolveOrgIDs(ctx, "entities", ids)
		if err != nil {
			return nil, httpx.ErrField("entity_id", "NOT_FOUND", "One of those entities was not found.")
		}
		p.EntityIDs = resolved
	}
	if ids := platform.QueryStrings(r, "department_id"); len(ids) > 0 {
		resolved, err := h.resolveOrgIDs(ctx, "departments", ids)
		if err != nil {
			return nil, httpx.ErrField("department_id", "NOT_FOUND", "One of those departments was not found.")
		}
		p.DepartmentIDs = resolved
	}

	out, err := h.repo.Run(ctx, h.reach(r), h.scope(r), key, p)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}
	return out, nil
}

func (h *Handler) runReport(w http.ResponseWriter, r *http.Request) {
	out, err := h.run(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, out)
}

// exportReport downloads the same result the screen is showing, as a file.
//
// `?format=` picks the renderer: csv for anything, xlsx for a working
// spreadsheet with typed cells, pdf for something to circulate unedited. The
// query and the caller's scope are identical to the on-screen run, so the file
// can never contain a row the reader could not already see.
func (h *Handler) exportReport(w http.ResponseWriter, r *http.Request) {
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = export.FormatCSV
	}
	if !export.ValidFormat(format) {
		httpx.Fail(w, r, httpx.ErrField("format", "INVALID",
			"format must be one of "+strings.Join(export.Formats, ", ")+"."))
		return
	}

	out, err := h.run(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	body, err := out.Render(format)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	filename := fmt.Sprintf("%s-%s.%s", out.Key, time.Now().UTC().Format("2006-01-02"), format)
	w.Header().Set("Content-Type", export.ContentType(format))
	// The quoted filename keeps a browser from interpreting the dashes, and
	// `attachment` stops the file rendering inline.
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// resolveOrgIDs turns public ids into internal ones, within the caller's reach.
//
// A cross-client report may legitimately filter by entities belonging to
// several clients, so resolution goes through the reach rather than a single
// tenant — and anything outside it is reported as not found rather than
// silently dropped, which would widen the report instead of narrowing it.
func (h *Handler) resolveOrgIDs(ctx context.Context, kind string, publicIDs []string) ([]int64, error) {
	reach := appctx.Reach(ctx)
	wanted := make(map[string]struct{}, len(publicIDs))
	for _, id := range publicIDs {
		wanted[id] = struct{}{}
	}

	out := make([]int64, 0, len(publicIDs))
	if kind == "departments" {
		rows, err := h.org.Departments(ctx, reach, false, nil, platform.Page{}, org.OrgFilter{})
		if err != nil {
			return nil, err
		}
		for _, d := range rows {
			if _, ok := wanted[d.PublicID]; ok {
				out = append(out, d.ID)
			}
		}
	} else {
		rows, err := h.org.Entities(ctx, reach, false, nil, platform.Page{}, org.OrgFilter{})
		if err != nil {
			return nil, err
		}
		for _, e := range rows {
			if _, ok := wanted[e.PublicID]; ok {
				out = append(out, e.ID)
			}
		}
	}

	if len(out) != len(wanted) {
		return nil, fmt.Errorf("%d of %d %s were not found in reach", len(wanted)-len(out), len(wanted), kind)
	}
	return out, nil
}

// filterOption is one choice in the Reports filter bar.
type filterOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	// Group lets the picker show "PF & Compliance › PF Withdrawals" without a
	// second request per entity.
	Group string `json:"group,omitempty"`
}

// reportFilters answers what this caller may narrow a report by.
//
// Built from their own reach, so a partner is offered their client's
// establishments and nobody else's, and an employee — who only ever sees their
// own tickets — is offered the statuses and priorities but no org filters,
// because narrowing by an entity they cannot see would return nothing.
func (h *Handler) reportFilters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	reach := appctx.Reach(ctx)

	out := map[string]any{
		"statuses":   ticket.Statuses,
		"priorities": ticket.Priorities,
		"formats":    export.Formats,
	}

	// The org filters only mean something to somebody who sees more than their
	// own tickets.
	if actor != nil && actor.CanAny("ticket.view.all", "ticket.view.scope") {
		entities, err := h.org.Entities(ctx, reach, true, nil, platform.Page{}, org.OrgFilter{})
		if err == nil {
			rows := make([]filterOption, 0, len(entities))
			for _, e := range entities {
				rows = append(rows, filterOption{
					Value: e.PublicID, Label: e.Name, Group: e.DepartmentName.String,
				})
			}
			out["entities"] = rows
		}

		departments, err := h.org.Departments(ctx, reach, true, nil, platform.Page{}, org.OrgFilter{})
		if err == nil {
			rows := make([]filterOption, 0, len(departments))
			for _, d := range departments {
				rows = append(rows, filterOption{Value: d.PublicID, Label: d.Name, Group: d.Type})
			}
			out["departments"] = rows
		}
	}

	httpx.OK(w, r, out)
}
