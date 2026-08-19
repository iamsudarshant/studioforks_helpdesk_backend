package user

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
)

// maxRosterBytes caps an upload at roughly 20k rows. Beyond that the request
// should become a background job; refusing loudly is better than timing out.
const maxRosterBytes = 8 << 20 // 8 MiB

// BulkRoutes mounts the roster import.
//
// Validation and import are separate calls on purpose: an administrator uploads
// once, sees every problem in the file, fixes it, and only then commits. A
// single endpoint that did both would either import a broken roster or reject
// the whole thing over one typo.
func (h *Handler) BulkRoutes(r chi.Router) {
	imports := middleware.RequirePermission("user.bulk_import")

	r.Route("/users/bulk", func(r chi.Router) {
		r.With(imports).Get("/template", h.bulkTemplate)
		r.With(imports).Post("/validate", h.bulkValidate)
		r.With(imports).Post("/import", h.bulkImport)
		r.With(middleware.RequirePermission("user.delete")).Post("/delete", h.bulkDelete)
	})
}

func (h *Handler) bulkTemplate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="complydesk-employee-template.csv"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(BulkTemplate())
}

// roster reads the uploaded file out of the request.
func (h *Handler) roster(r *http.Request) ([]BulkRow, error) {
	if err := r.ParseMultipartForm(maxRosterBytes); err != nil {
		return nil, httpx.ErrField("file", "INVALID", "Upload the roster as a CSV file.")
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		return nil, httpx.ErrField("file", "REQUIRED", "Choose a CSV file to upload.")
	}
	defer func() { _ = file.Close() }()

	if header.Size > maxRosterBytes {
		return nil, httpx.New(httpx.CodePayloadTooLarge,
			"That roster is too large to import in one request. Split it into smaller files.")
	}

	body, err := io.ReadAll(io.LimitReader(file, maxRosterBytes))
	if err != nil {
		return nil, httpx.ErrInternal(fmt.Errorf("reading the roster: %w", err))
	}

	rows, _, err := ParseBulkCSV(bytes.NewReader(body))
	if err != nil {
		// A malformed file is the uploader's problem, and naming it saves a
		// support round trip.
		return nil, httpx.ErrField("file", "INVALID", err.Error())
	}
	return rows, nil
}

func (h *Handler) bulkValidate(w http.ResponseWriter, r *http.Request) {
	rows, err := h.roster(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	result, err := h.repo.ValidateBulk(ctx, appctx.TenantID(ctx), rows)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, result)
}

func (h *Handler) bulkImport(w http.ResponseWriter, r *http.Request) {
	rows, err := h.roster(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID := appctx.TenantID(ctx)
	actor := appctx.ActorFrom(ctx)

	group, err := h.repo.GroupByKey(ctx, tenantID, GroupActiveEmployees)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(fmt.Errorf("loading the active employee group: %w", err)))
		return
	}
	role, err := h.repo.RoleByKey(ctx, tenantID, RoleEmployee)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(fmt.Errorf("loading the employee role: %w", err)))
		return
	}

	actorID := &actor.UserID
	result, err := h.repo.ImportBulk(ctx, tenantID, rows,
		h.reset.HashPassword, group.ID, role.ID, actorID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	// Row counts only — an audit entry must never carry the passwords it just
	// handed out.
	h.auditor.Record(ctx, audit.Entry{
		Action: "user.bulk_import", EntityType: "user",
		After: map[string]any{
			"total_rows":    result.TotalRows,
			"imported_rows": result.ImportedRows,
			"invalid_rows":  result.InvalidRows,
		},
	})

	// The credentials are returned once and never stored. They can always be
	// recomputed from the employee's PF number and birth year, so keeping a
	// table of live passwords would be a liability for no benefit.
	httpx.OK(w, r, result)
}

// bulkCredentialsCSV renders the first-sign-in list an administrator hands out.
func bulkCredentialsCSV(creds []Credential) []byte {
	var b bytes.Buffer
	b.WriteString("employee_code,name,email,first_password\n")
	for _, c := range creds {
		b.WriteString(fmt.Sprintf("%s,%s,%s,%s\n",
			csvSafe(c.EmployeeCode), csvSafe(c.Name), csvSafe(c.Email), csvSafe(c.Password)))
	}
	return b.Bytes()
}

// csvSafe quotes a value and neutralises spreadsheet formulas — the same guard
// the reports use, for the same reason.
func csvSafe(s string) string {
	if s != "" && bytes.ContainsAny([]byte{s[0]}, "=+-@") {
		s = "'" + s
	}
	if bytes.ContainsAny([]byte(s), ",\"\n") {
		return `"` + string(bytes.ReplaceAll([]byte(s), []byte(`"`), []byte(`""`))) + `"`
	}
	return s
}

// --- bulk removal -----------------------------------------------------------

// bulkDelete removes several people at once.
//
// Recoverable, like the single-user delete: the record is retained so their
// tickets keep an author, and an administrator can restore them. Reported per
// user rather than as one verdict, because a selection of forty legitimately
// succeeds for most and refuses for a few — your own account among them.
func (h *Handler) bulkDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserIDs []string `json:"user_ids" validate:"required,min=1,max=500,dive,len=26"`
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

	type outcome struct {
		UserID string `json:"user_id"`
		Name   string `json:"name,omitempty"`
		OK     bool   `json:"ok"`
		Error  string `json:"error,omitempty"`
	}

	reach := appctx.Reach(ctx)
	results := make([]outcome, 0, len(req.UserIDs))
	succeeded := 0

	for _, publicID := range req.UserIDs {
		row := outcome{UserID: publicID}

		target, err := h.repo.ByPublicIDInReach(ctx, reach, publicID)
		if err != nil {
			row.Error = "Not found, or not yours to remove."
			results = append(results, row)
			continue
		}
		row.Name = target.FullName()

		if target.ID == actor.UserID {
			row.Error = "You cannot remove your own account."
			results = append(results, row)
			continue
		}

		if err := h.repo.SoftDelete(ctx, target.TenantID, target.ID); err != nil {
			row.Error = "That account could not be removed."
			results = append(results, row)
			continue
		}

		h.auditor.Record(ctx, audit.Entry{
			Action: audit.ActionUserDeleted, EntityType: "user", EntityID: &target.ID,
			EntityPublicID: target.PublicID,
		})
		row.OK = true
		succeeded++
		results = append(results, row)
	}

	httpx.OK(w, r, map[string]any{
		"requested": len(req.UserIDs),
		"succeeded": succeeded,
		"failed":    len(req.UserIDs) - succeeded,
		"results":   results,
	})
}
