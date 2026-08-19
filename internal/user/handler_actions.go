package user

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/platform"
)

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	var req userRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	target, err := h.loadTarget(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// This is a PATCH: every assignment below treats an absent field as "leave
	// it alone", so the schema's create-time requirements have to be satisfied
	// from the record rather than from the request. Validating first would make
	// changing one field mean resending all of them, and a client that has to
	// resend everything will eventually resend something stale.
	if strings.TrimSpace(req.FirstName) == "" {
		req.FirstName = target.FirstName
	}
	if err := httpx.Validate(&req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	// The client this person actually belongs to. On a cross-client roster the
	// header names the platform workspace, not their client.
	tenantID := target.TenantID

	roles, _ := h.repo.RolesFor(ctx, target.ID)
	before := h.toResponse(r, target, roleKeys(roles))

	update := UpdateParams{}
	assign := func(dst **string, v string) {
		if v != "" {
			val := v
			*dst = &val
		}
	}
	assign(&update.EmployeeCode, req.EmployeeCode)
	assign(&update.Username, req.Username)
	assign(&update.FirstName, req.FirstName)
	assign(&update.LastName, req.LastName)
	assign(&update.Email, req.Email)
	assign(&update.AltEmail, req.AltEmail)
	assign(&update.Mobile, req.Mobile)
	assign(&update.AltMobile, req.AltMobile)
	assign(&update.PANNumber, req.PANNumber)
	assign(&update.UANNumber, req.UANNumber)
	assign(&update.PFNumber, req.PFNumber)
	assign(&update.ESICNumber, req.ESICNumber)
	assign(&update.Designation, req.Designation)
	assign(&update.Status, req.Status)

	if d, ok := parseDate(req.DateOfJoining); ok {
		update.DateOfJoining = &d
	}
	if d, ok := parseDate(req.DateOfBirth); ok {
		update.DateOfBirth = &d
	}
	if d, ok := parseDate(req.LastWorkingDay); ok {
		update.LastWorkingDay = &d
	}

	// Reuse the create-path resolver, then copy the resolved ids across.
	resolved := CreateParams{}
	if err := h.resolveReferences(ctx, tenantID, req, &resolved); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	update.EntityID = resolved.EntityID
	update.SiteID = resolved.SiteID
	update.DepartmentID = resolved.DepartmentID
	update.UserGroupID = resolved.UserGroupID

	if actor := appctx.ActorFrom(ctx); actor != nil {
		id := actor.UserID
		update.UpdatedBy = &id
	}

	if err := h.repo.Update(ctx, tenantID, target.ID, update); err != nil {
		var dup *DuplicateError
		if errors.As(err, &dup) {
			httpx.Fail(w, r, httpx.ErrDuplicate(dup.Field(), "Another user already has this value."))
			return
		}
		httpx.Fail(w, r, mapErr(err, "That user"))
		return
	}

	if req.Roles != nil {
		if err := h.applyRoles(ctx, tenantID, target.ID, req.Roles); err != nil {
			httpx.Fail(w, r, err)
			return
		}
	}

	updated, err := h.repo.ByID(ctx, tenantID, target.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	newRoles, _ := h.repo.RolesFor(ctx, updated.ID)
	out := []userResponse{h.toResponse(r, updated, roleKeys(newRoles))}
	out = h.enrich(r, []User{*updated}, out)

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionUserUpdated, EntityType: "user", EntityID: &target.ID,
		EntityPublicID: target.PublicID, Before: before, After: out[0],
	})
	httpx.OK(w, r, out[0])
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	target, err := h.loadTarget(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor != nil && actor.UserID == target.ID {
		httpx.Fail(w, r, httpx.ErrConflict("You cannot delete your own account."))
		return
	}

	if err := h.repo.SoftDelete(ctx, target.TenantID, target.ID); err != nil {
		httpx.Fail(w, r, mapErr(err, "That user"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionUserDeleted, EntityType: "user", EntityID: &target.ID,
		EntityPublicID: target.PublicID,
		Before:         map[string]any{"email": target.Email.String, "name": target.FullName()},
	})
	httpx.OK(w, r, map[string]any{"message": "User removed."})
}

func (h *Handler) activate(w http.ResponseWriter, r *http.Request) {
	h.setStatus(w, r, StatusActive, audit.ActionUserActivated, "User activated.")
}

func (h *Handler) deactivate(w http.ResponseWriter, r *http.Request) {
	h.setStatus(w, r, StatusInactive, audit.ActionUserDeactivated, "User deactivated and signed out.")
}

func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request, status, action, message string) {
	target, err := h.loadTarget(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if status == StatusInactive && actor != nil && actor.UserID == target.ID {
		httpx.Fail(w, r, httpx.ErrConflict("You cannot deactivate your own account."))
		return
	}

	if err := h.repo.SetStatus(ctx, target.TenantID, target.ID, status); err != nil {
		httpx.Fail(w, r, mapErr(err, "That user"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: action, EntityType: "user", EntityID: &target.ID,
		EntityPublicID: target.PublicID,
		Before:         map[string]any{"status": target.Status}, After: map[string]any{"status": status},
	})
	httpx.OK(w, r, map[string]any{"message": message, "status": status})
}

func (h *Handler) unlock(w http.ResponseWriter, r *http.Request) {
	target, err := h.loadTarget(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	if err := h.repo.Unlock(ctx, target.TenantID, target.ID); err != nil {
		httpx.Fail(w, r, mapErr(err, "That user"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "user.unlocked", EntityType: "user", EntityID: &target.ID,
		EntityPublicID: target.PublicID,
	})
	httpx.OK(w, r, map[string]any{"message": "Account unlocked."})
}

// sendResetLink powers the Reset Password button on the user table. It mails a
// single-use link that opens the "new password + confirm" screen.
// mayAdminister checks that the caller outranks the person they are acting on.
//
// Permission alone is not enough for anything that takes an account away from
// its owner. `user.send_reset` says "this person administers accounts"; it does
// not say which accounts, and without a rank check a helpdesk executive could
// reset the super admin's password and a partner could reset another partner's.
//
// Returns a FORBIDDEN naming the reason, because unlike a scope violation there
// is nothing to conceal: the caller can already see the user on screen.
func (h *Handler) mayAdminister(r *http.Request, target *User) error {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor == nil {
		return httpx.ErrUnauthenticated("")
	}

	if actor.UserID == target.ID {
		return httpx.New(httpx.CodeForbidden,
			"You cannot do this to your own account. Change your password from your profile instead.")
	}

	targetRoles, err := h.repo.RolesFor(ctx, target.ID)
	if err != nil {
		return httpx.ErrInternal(err)
	}
	keys := make([]string, 0, len(targetRoles))
	for _, role := range targetRoles {
		keys = append(keys, role.Key)
	}

	if !CanAdminister(actor.Roles, keys) {
		return httpx.New(httpx.CodeForbidden,
			"You can only do this for users below your own level.")
	}
	return nil
}

func (h *Handler) sendResetLink(w http.ResponseWriter, r *http.Request) {
	target, err := h.loadTarget(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.mayAdminister(r, target); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenant := appctx.TenantFrom(ctx)
	if tenant == nil {
		httpx.Fail(w, r, httpx.New(httpx.CodeTenantNotFound, "No workspace matches this address."))
		return
	}

	// Send the link to the portal the target actually uses, not the caller's.
	portal := appctx.PortalUser
	if roles, err := h.repo.RolesFor(ctx, target.ID); err == nil && len(roles) > 0 {
		portal = appctx.Portal(roles[0].Portal)
	}

	if err := h.reset.SendResetLink(ctx, tenant, target, portal); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]any{
		"message": "A password reset link has been sent to this user's registered email address.",
	})
}

// resetToDefaultPassword puts an account back to the password it was created
// with: the holder's PAN and year of birth, lowercase.
//
// The emailed link is the better route and stays the default — it proves the
// recipient controls the mailbox. This exists for the case the link cannot
// solve: an employee with no working email address, or one standing at the desk
// who needs access now. The password is returned once, to be read out, and the
// account is flagged to change it at next sign-in.
func (h *Handler) resetToDefaultPassword(w http.ResponseWriter, r *http.Request) {
	target, err := h.loadTarget(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.mayAdminister(r, target); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// The default is derived from the person's own identifiers, so an account
	// missing either cannot have one — and guessing would produce a password
	// nobody could be told the rule for.
	password, ok := DefaultPassword(target.PANNumber.String, target.DateOfBirth.Time)
	if !ok || !target.DateOfBirth.Valid {
		httpx.Fail(w, r, httpx.ErrField("pan_number", "INCOMPLETE",
			"This account has no PAN and date of birth, so it has no default password. "+
				"Add them, or send a reset link instead."))
		return
	}

	ctx := r.Context()
	hash, err := h.reset.HashPassword(password)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	expiryDays, historyCount := h.reset.PasswordAgeing(ctx, target.TenantID)
	if err := h.repo.SetPassword(ctx, target.ID, hash, expiryDays, historyCount); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	// SetPassword clears must_change_password, which is right for someone
	// choosing their own. This is not that: it is a password two people now
	// know, so it has to be replaced on first use.
	if err := h.repo.RequirePasswordChange(ctx, target.ID); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	actorID := appctx.ActorFrom(ctx).UserID
	h.auditor.Record(ctx, audit.Entry{
		TenantID: &target.TenantID, ActorID: &actorID, Action: audit.ActionPasswordChanged,
		EntityType: "user", EntityID: &target.ID, EntityPublicID: target.PublicID,
		After: map[string]any{"method": "reset_to_default"},
	})

	httpx.OK(w, r, map[string]any{
		"message":  target.FullName() + " can sign in with the default password and must change it.",
		"password": password,
	})
}

type setRolesRequest struct {
	Roles []string `json:"roles" validate:"required,dive,max=64"`
}

func (h *Handler) setRoles(w http.ResponseWriter, r *http.Request) {
	var req setRolesRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	target, err := h.loadTarget(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID := target.TenantID
	actor := appctx.ActorFrom(ctx)

	// Only a super admin may grant super admin; otherwise any tenant admin
	// could escalate themselves to platform level.
	for _, key := range req.Roles {
		if key == RoleSuperAdmin && (actor == nil || !actor.IsSuperAdmin) {
			httpx.Fail(w, r, httpx.ErrForbidden("You cannot grant the super administrator role."))
			return
		}
	}

	before, _ := h.repo.RolesFor(ctx, target.ID)
	if err := h.applyRoles(ctx, tenantID, target.ID, req.Roles); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	after, _ := h.repo.RolesFor(ctx, target.ID)

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionUserRolesSet, EntityType: "user", EntityID: &target.ID,
		EntityPublicID: target.PublicID,
		Before:         roleKeys(before), After: roleKeys(after),
	})
	httpx.OK(w, r, map[string]any{"roles": roleKeys(after)})
}

type setScopesRequest struct {
	Entities    []string `json:"entities" validate:"omitempty,dive,len=26"`
	Sites       []string `json:"sites" validate:"omitempty,dive,len=26"`
	Departments []string `json:"departments" validate:"omitempty,dive,len=26"`
	Categories  []string `json:"categories" validate:"omitempty,dive,len=26"`
}

// setScopes assigns a partner or agent to specific entities, sites, departments
// or categories — the "give this user only these 2 of 10 sites" requirement.
func (h *Handler) setScopes(w http.ResponseWriter, r *http.Request) {
	var req setScopesRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	target, err := h.loadTarget(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	// Entities, sites and departments are resolved inside the target's own
	// client: an allocation always names records belonging to their workspace.
	tenantID := target.TenantID

	scopes := []Scope{}

	if len(req.Entities) > 0 {
		ids, err := h.org.ResolveEntityIDs(ctx, tenantID, req.Entities)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("entities", "NOT_FOUND", "One of those entities was not found."))
			return
		}
		for _, id := range ids {
			scopes = append(scopes, Scope{ScopeType: ScopeEntity, ScopeID: id})
		}
	}
	if len(req.Sites) > 0 {
		ids, err := h.org.ResolveSiteIDs(ctx, tenantID, req.Sites)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("sites", "NOT_FOUND", "One of those sites was not found."))
			return
		}
		for _, id := range ids {
			scopes = append(scopes, Scope{ScopeType: ScopeSite, ScopeID: id})
		}
	}
	if len(req.Departments) > 0 {
		ids, err := h.org.ResolveDepartmentIDs(ctx, tenantID, req.Departments)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("departments", "NOT_FOUND", "One of those departments was not found."))
			return
		}
		for _, id := range ids {
			scopes = append(scopes, Scope{ScopeType: ScopeDepartment, ScopeID: id})
		}
	}

	before, _ := h.repo.ScopesFor(ctx, target.ID)
	if err := h.repo.SetScopes(ctx, tenantID, target.ID, scopes); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	after, _ := h.repo.ScopesFor(ctx, target.ID)

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionUserScopesSet, EntityType: "user", EntityID: &target.ID,
		EntityPublicID: target.PublicID,
		Before:         before, After: after,
	})

	// Changing scope changes what the user may see, so their tokens must be
	// re-minted. The scope hash in the access token makes that automatic, but
	// the response tells the UI to expect it.
	httpx.OK(w, r, map[string]any{
		"scopes":  h.scopeRefs(r, target.TenantID, after),
		"message": "Access updated. This user will be asked to sign in again.",
	})
}

func (h *Handler) activity(w http.ResponseWriter, r *http.Request) {
	target, err := h.loadTarget(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	page := platform.ParsePage(r, map[string]string{"created_at": "created_at"}, "created_at")
	from, to := platform.QueryDates(r, "from", "to")

	rows, total, err := h.repo.Activity(ctx, target.TenantID, target.ID,
		platform.QueryStrings(r, "action"), from, to, page)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.List(w, r, rows, platform.NewMeta(page, total))
}

// --- bulk group move --------------------------------------------------------

type moveGroupRequest struct {
	// The client these users belong to. Required only when the caller has not
	// selected one — staff working the cross-client list have not, and the
	// tenant header then names ComplyDesk's own workspace, where none of the
	// selected users exist.
	Client            string            `json:"client" validate:"omitempty,max=64"`
	UserIDs           []string          `json:"user_ids" validate:"required,min=1,max=5000,dive,len=26"`
	TargetGroupID     string            `json:"target_group_id" validate:"required,len=26"`
	LastWorkingDay    string            `json:"last_working_day" validate:"omitempty,dateonly"`
	PerUserLWD        map[string]string `json:"per_user_last_working_day"`
	TicketDisposition string            `json:"ticket_disposition" validate:"omitempty,oneof=KEEP TRANSFER CLOSE"`
	ReassignToUserID  string            `json:"reassign_to_user_id" validate:"omitempty,len=26"`
	NotifyUsers       bool              `json:"notify_users"`
}

// moveGroup implements the multi-select bulk move, typically Active Employees
// to Ex-Employees with a last working day and a decision about open tickets.
func (h *Handler) moveGroup(w http.ResponseWriter, r *http.Request) {
	var req moveGroupRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID, err := h.writeTenantID(r, req.Client, "Choose the client these users belong to.")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	group, err := h.repo.GroupByPublicID(ctx, tenantID, req.TargetGroupID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrField("target_group_id", "NOT_FOUND", "That user group was not found."))
		return
	}

	userIDs, err := h.repo.ResolveIDs(ctx, tenantID, req.UserIDs)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrField("user_ids", "NOT_FOUND",
			"One or more of the selected users were not found in your workspace."))
		return
	}

	params := MoveGroupParams{
		UserIDs:           userIDs,
		TargetGroupID:     group.ID,
		TicketDisposition: req.TicketDisposition,
		PerUserLWD:        map[int64]time.Time{},
	}
	if params.TicketDisposition == "" {
		params.TicketDisposition = TicketsKeep
	}

	if d, ok := parseDate(req.LastWorkingDay); ok {
		params.LastWorkingDay = &d
	}
	// Moving into the ex-employee group without a leaving date would leave the
	// grace period with nothing to count from.
	if group.Key == GroupExEmployees && params.LastWorkingDay == nil && len(req.PerUserLWD) == 0 {
		httpx.Fail(w, r, httpx.ErrField("last_working_day", "REQUIRED",
			"A last working day is required when moving users to the ex-employee group."))
		return
	}

	for publicID, raw := range req.PerUserLWD {
		d, ok := parseDate(raw)
		if !ok {
			httpx.Fail(w, r, httpx.ErrField("per_user_last_working_day", "INVALID",
				"Dates must be in YYYY-MM-DD format."))
			return
		}
		u, err := h.repo.ByPublicID(ctx, tenantID, publicID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("per_user_last_working_day", "NOT_FOUND",
				"One of the users in the per-user dates was not found."))
			return
		}
		params.PerUserLWD[u.ID] = d
	}

	if req.TicketDisposition == TicketsTransfer {
		if req.ReassignToUserID == "" {
			httpx.Fail(w, r, httpx.ErrField("reassign_to_user_id", "REQUIRED",
				"Choose who should take over the open tickets."))
			return
		}
		assignee, err := h.repo.ByPublicID(ctx, tenantID, req.ReassignToUserID)
		if err != nil {
			httpx.Fail(w, r, httpx.ErrField("reassign_to_user_id", "NOT_FOUND", "That user was not found."))
			return
		}
		params.ReassignToUserID = &assignee.ID
	}

	if actor := appctx.ActorFrom(ctx); actor != nil {
		id := actor.UserID
		params.ActorID = &id
	}

	summary, err := h.repo.MoveToGroup(ctx, tenantID, params)
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That user group"))
		return
	}

	if req.NotifyUsers {
		for _, result := range summary.Results {
			if !result.Success {
				continue
			}
			_ = h.publisher.Publish(ctx, tenantID, "user.group_changed", "user", 0, map[string]any{
				"user_public_id":    result.UserPublicID,
				"full_name":         result.Name,
				"to_group":          result.ToGroup,
				"last_working_day":  result.LastWorkingDay,
				"access_expires_on": result.AccessExpiresOn,
			})
		}
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionUserGroupMoved, EntityType: "user_group", EntityID: &group.ID,
		EntityPublicID: group.PublicID,
		After: map[string]any{
			"batch_id": summary.BatchID, "requested": summary.Requested,
			"moved": summary.Moved, "failed": summary.Failed,
			"tickets_moved": summary.TicketsMoved, "target_group": group.Key,
			"disposition": params.TicketDisposition,
		},
	})
	httpx.OK(w, r, summary)
}

// --- groups, roles, permissions ---------------------------------------------

type groupResponse struct {
	ID              string     `json:"id"`
	Key             string     `json:"key"`
	Name            string     `json:"name"`
	Description     string     `json:"description,omitempty"`
	IsSystem        bool       `json:"is_system"`
	AccessMode      string     `json:"access_mode"`
	GracePeriodDays int        `json:"grace_period_days"`
	UserCount       int64      `json:"user_count"`
	Client          *Reference `json:"client,omitempty"`
}

// listGroups answers the User groups tab.
//
// A user group belongs to one client — "Employees", "Ex-employees", each with
// its own access mode and grace period — so this lists them across every client
// in reach rather than against the tenant the header named. Pinned to the
// header, a staff user with no client selected was shown the platform
// workspace's groups, of which there are none, and the tab rendered blank.
func (h *Handler) listGroups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// `?client=` narrows to one client, so a form's group picker can show a
	// single client's groups while the session still spans several.
	reach := appctx.Reach(ctx)
	if ref := strings.TrimSpace(r.URL.Query().Get("client")); ref != "" {
		id, err := h.repo.ResolveClientInReach(ctx, reach, ref)
		if err != nil {
			reach = appctx.ClientReach{}
		} else {
			reach = reach.NarrowTo(id)
		}
	}

	rows, err := h.repo.ListGroupsInReach(ctx, reach)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]groupResponse, 0, len(rows))
	for _, g := range rows {
		row := groupResponse{
			ID: g.PublicID, Key: g.Key, Name: g.Name, Description: g.Description.String,
			IsSystem: g.IsSystem, AccessMode: g.AccessMode,
			// Counted by the list query itself, so the headcount cannot drift
			// from the rows it describes.
			GracePeriodDays: g.GracePeriodDays, UserCount: g.UserCount,
		}
		if g.ClientName.Valid {
			row.Client = &Reference{
				ID: g.ClientSlug.String, Code: g.ClientCode.String, Name: g.ClientName.String,
			}
		}
		out = append(out, row)
	}
	httpx.OK(w, r, out)
}

type groupRequest struct {
	// The client this belongs to, when the caller has not selected one.
	Client          string `json:"client" validate:"omitempty,max=64"`
	Key             string `json:"key" validate:"required,enumkey"`
	Name            string `json:"name" validate:"required,notblank,max=191,safetext"`
	Description     string `json:"description" validate:"omitempty,max=500"`
	AccessMode      string `json:"access_mode" validate:"required,oneof=FULL READ_ONLY NO_ACCESS"`
	GracePeriodDays int    `json:"grace_period_days" validate:"gte=0,lte=3650"`
}

func (h *Handler) createGroup(w http.ResponseWriter, r *http.Request) {
	var req groupRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID, err := h.writeTenantID(r, req.Client, "Choose the client this user group belongs to.")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	g, err := h.repo.CreateGroup(ctx, tenantID, GroupParams{
		Key: req.Key, Name: req.Name, Description: req.Description,
		AccessMode: req.AccessMode, GracePeriodDays: req.GracePeriodDays,
	})
	if err != nil {
		if errors.Is(err, platform.ErrSentinelConflict) {
			httpx.Fail(w, r, httpx.ErrDuplicate("key", "A group with this key already exists."))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "user_group.created", EntityType: "user_group", EntityID: &g.ID,
		EntityPublicID: g.PublicID, After: req,
	})
	httpx.Created(w, r, groupResponse{
		ID: g.PublicID, Key: g.Key, Name: g.Name, Description: g.Description.String,
		IsSystem: g.IsSystem, AccessMode: g.AccessMode, GracePeriodDays: g.GracePeriodDays,
	})
}

func (h *Handler) updateGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            *string `json:"name" validate:"omitempty,max=191"`
		Description     *string `json:"description" validate:"omitempty,max=500"`
		AccessMode      *string `json:"access_mode" validate:"omitempty,oneof=FULL READ_ONLY NO_ACCESS"`
		GracePeriodDays *int    `json:"grace_period_days" validate:"omitempty,gte=0,lte=3650"`
	}
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()

	g, err := h.repo.GroupInReach(ctx, appctx.Reach(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That user group"))
		return
	}
	// The group's own client, which on a cross-client list is not the one the
	// header named.
	tenantID := g.TenantID

	if err := h.repo.UpdateGroup(ctx, tenantID, g.ID, GroupUpdate{
		Name: req.Name, Description: req.Description,
		AccessMode: req.AccessMode, GracePeriodDays: req.GracePeriodDays,
	}); err != nil {
		httpx.Fail(w, r, mapErr(err, "That user group"))
		return
	}

	updated, err := h.repo.GroupByID(ctx, tenantID, g.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "user_group.updated", EntityType: "user_group", EntityID: &g.ID,
		EntityPublicID: g.PublicID, Before: g, After: updated,
	})
	httpx.OK(w, r, groupResponse{
		ID: updated.PublicID, Key: updated.Key, Name: updated.Name,
		Description: updated.Description.String, IsSystem: updated.IsSystem,
		AccessMode: updated.AccessMode, GracePeriodDays: updated.GracePeriodDays,
	})
}

func (h *Handler) deleteGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	g, err := h.repo.GroupInReach(ctx, appctx.Reach(ctx), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "That user group"))
		return
	}
	tenantID := g.TenantID

	if err := h.repo.DeleteGroup(ctx, tenantID, g.ID); err != nil {
		if errors.Is(err, platform.ErrSentinelImmutable) {
			httpx.Fail(w, r, httpx.ErrConflict(
				"This is a system group and cannot be deleted. You can rename it or change its settings instead."))
			return
		}
		httpx.Fail(w, r, mapErr(err, "That user group"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: "user_group.deleted", EntityType: "user_group", EntityID: &g.ID,
		EntityPublicID: g.PublicID, Before: g,
	})
	httpx.OK(w, r, map[string]any{"message": "User group removed."})
}

func (h *Handler) listRoles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := h.repo.ListRoles(ctx, appctx.TenantID(ctx))
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	type roleResponse struct {
		ID          string   `json:"id"`
		Key         string   `json:"key"`
		Name        string   `json:"name"`
		Description string   `json:"description,omitempty"`
		Portal      string   `json:"portal"`
		IsSystem    bool     `json:"is_system"`
		Permissions []string `json:"permissions"`
		UserCount   int64    `json:"user_count"`
	}

	// The permission set and the headcount travel with the list.
	//
	// The Roles screen needs both for every role at once — it renders the
	// matrix for whichever role is selected without a second request — and
	// fetching them per role turned one screen into a request per role.
	perms, err := h.repo.PermissionsForRoles(ctx)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	counts, err := h.repo.RoleUserCounts(ctx)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := make([]roleResponse, 0, len(rows))
	for _, role := range rows {
		granted := perms[role.ID]
		if granted == nil {
			granted = []string{}
		}
		out = append(out, roleResponse{
			ID: role.PublicID, Key: role.Key, Name: role.Name,
			Description: role.Description.String, Portal: role.Portal, IsSystem: role.IsSystem,
			Permissions: granted, UserCount: counts[role.ID],
		})
	}
	httpx.OK(w, r, out)
}

// --- custom roles -----------------------------------------------------------

type roleRequest struct {
	// The client this belongs to, when the caller has not selected one.
	Client      string `json:"client" validate:"omitempty,max=64"`
	Key         string `json:"key" validate:"required,enumkey"`
	Name        string `json:"name" validate:"required,notblank,max=128,safetext"`
	Description string `json:"description" validate:"omitempty,max=500"`
	Portal      string `json:"portal" validate:"required,oneof=admin agents partner user"`
}

// createRole adds a role for one client, alongside the system ones.
//
// A custom role always belongs to a client: the system roles carry a NULL
// tenant and are what the product's own rules reference by key, so they ship
// with the release rather than being created here. That is also why a client
// must be chosen first.
func (h *Handler) createRole(w http.ResponseWriter, r *http.Request) {
	var req roleRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	tenantID, err := h.writeTenantID(r, req.Client, "Choose the client this role belongs to.")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	role, err := h.repo.CreateRole(ctx, tenantID, RoleParams{
		Key: req.Key, Name: req.Name, Description: req.Description, Portal: req.Portal,
	})
	if err != nil {
		if errors.Is(err, platform.ErrSentinelConflict) {
			httpx.Fail(w, r, httpx.ErrDuplicate("key", "A role with this key already exists."))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionConfigChanged, EntityType: "role", EntityID: &role.ID,
		EntityPublicID: role.PublicID, After: role,
	})
	httpx.Created(w, r, map[string]any{
		"id": role.PublicID, "key": role.Key, "name": role.Name,
		"description": role.Description.String, "portal": role.Portal,
		"is_system": role.IsSystem, "permissions": []string{}, "user_count": 0,
	})
}

func (h *Handler) updateRole(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        *string `json:"name" validate:"omitempty,notblank,max=128,safetext"`
		Description *string `json:"description" validate:"omitempty,max=500"`
	}
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	role, err := h.roleFromPath(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if role.IsSystem {
		httpx.Fail(w, r, httpx.ErrConflict(
			"This is a built-in role and cannot be renamed. Create a custom role instead."))
		return
	}

	ctx := r.Context()
	if err := h.repo.UpdateRole(ctx, role.ID, RoleUpdate{
		Name: req.Name, Description: req.Description,
	}); err != nil {
		httpx.Fail(w, r, mapErr(err, "That role"))
		return
	}

	updated, err := h.repo.RoleByID(ctx, role.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionConfigChanged, EntityType: "role", EntityID: &role.ID,
		EntityPublicID: role.PublicID, Before: role, After: updated,
	})
	httpx.OK(w, r, map[string]any{
		"id": updated.PublicID, "key": updated.Key, "name": updated.Name,
		"description": updated.Description.String, "portal": updated.Portal,
		"is_system": updated.IsSystem,
	})
}

func (h *Handler) deleteRole(w http.ResponseWriter, r *http.Request) {
	role, err := h.roleFromPath(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if role.IsSystem {
		httpx.Fail(w, r, httpx.ErrConflict(
			"This is a built-in role and cannot be deleted."))
		return
	}

	ctx := r.Context()
	if err := h.repo.DeleteRole(ctx, role.ID); err != nil {
		if errors.Is(err, platform.ErrSentinelConflict) {
			httpx.Fail(w, r, httpx.ErrConflict(
				"Somebody still holds this role. Move them to another role first."))
			return
		}
		httpx.Fail(w, r, mapErr(err, "That role"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionConfigChanged, EntityType: "role", EntityID: &role.ID,
		EntityPublicID: role.PublicID, Before: role,
	})
	httpx.OK(w, r, map[string]any{"message": "Role removed."})
}

func (h *Handler) listPermissions(w http.ResponseWriter, r *http.Request) {
	rows, err := h.repo.ListPermissions(r.Context())
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, rows)
}

func (h *Handler) roleFromPath(r *http.Request) (*Role, error) {
	ctx := r.Context()
	rows, err := h.repo.ListRoles(ctx, appctx.TenantID(ctx))
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}
	id := chi.URLParam(r, "id")
	for i := range rows {
		if rows[i].PublicID == id {
			return &rows[i], nil
		}
	}
	return nil, httpx.ErrNotFound("That role")
}

func (h *Handler) rolePermissions(w http.ResponseWriter, r *http.Request) {
	role, err := h.roleFromPath(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	perms, err := h.repo.PermissionsForRole(r.Context(), role.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	httpx.OK(w, r, map[string]any{"role": role.Key, "permissions": perms})
}

func (h *Handler) setRolePermissions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Permissions []string `json:"permissions" validate:"required,dive,max=64"`
	}
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	role, err := h.roleFromPath(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if role.Key == RoleSuperAdmin && (actor == nil || !actor.IsSuperAdmin) {
		httpx.Fail(w, r, httpx.ErrForbidden("The super administrator role cannot be edited here."))
		return
	}

	before, _ := h.repo.PermissionsForRole(ctx, role.ID)
	if err := h.repo.SetRolePermissions(ctx, role.ID, req.Permissions); err != nil {
		if errors.Is(err, platform.ErrSentinelNotFound) {
			httpx.Fail(w, r, httpx.ErrField("permissions", "UNKNOWN",
				"One of those permission keys does not exist."))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	after, _ := h.repo.PermissionsForRole(ctx, role.ID)

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionConfigChanged, EntityType: "role", EntityID: &role.ID,
		EntityPublicID: role.PublicID, Before: before, After: after,
	})
	httpx.OK(w, r, map[string]any{"role": role.Key, "permissions": after})
}

// --- self service -----------------------------------------------------------

func (h *Handler) myProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	u, err := h.repo.ByID(ctx, appctx.TenantID(ctx), actor.UserID)
	if err != nil {
		httpx.Fail(w, r, mapErr(err, "Your profile"))
		return
	}
	roles, _ := h.repo.RolesFor(ctx, u.ID)

	// Users always see their own statutory numbers in full.
	out := userResponse{
		ID: u.PublicID, EmployeeCode: u.EmployeeCode.String, Username: u.Username.String,
		FirstName: u.FirstName, LastName: u.LastName.String, FullName: u.FullName(),
		Email: u.Email.String, AltEmail: u.AltEmail.String,
		Mobile: u.Mobile.String, AltMobile: u.AltMobile.String,
		PANNumber: u.PANNumber.String, UANNumber: u.UANNumber.String,
		PFNumber: u.PFNumber.String, ESICNumber: u.ESICNumber.String,
		Designation: u.Designation.String, Status: u.Status, Roles: roleKeys(roles),
		LoginCount: u.LoginCount, MFAEnabled: u.MFAEnabled, CreatedAt: u.CreatedAt,
	}
	if u.AvatarPath.Valid {
		out.AvatarURL = "/api/v1/public/documents/avatar/" + u.PublicID
	}
	if u.DateOfJoining.Valid {
		s := u.DateOfJoining.Time.Format("2006-01-02")
		out.DateOfJoining = &s
	}
	enriched := h.enrich(r, []User{*u}, []userResponse{out})
	httpx.OK(w, r, enriched[0])
}

// updateMyProfile allows only the contact fields. Statutory identifiers are
// employer-controlled: an employee who needs one corrected raises a ticket.
func (h *Handler) updateMyProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FirstName string `json:"first_name" validate:"omitempty,notblank,max=96,safetext"`
		LastName  string `json:"last_name" validate:"omitempty,max=96,safetext"`
		AltEmail  string `json:"alt_email" validate:"omitempty,email,max=191"`
		Mobile    string `json:"mobile" validate:"omitempty,mobile"`
		AltMobile string `json:"alt_mobile" validate:"omitempty,mobile"`
		Locale    string `json:"locale" validate:"omitempty,max=16"`
		Timezone  string `json:"timezone" validate:"omitempty,max=64"`
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
	tenantID := appctx.TenantID(ctx)

	update := UpdateParams{}
	assign := func(dst **string, v string) {
		if v != "" {
			val := v
			*dst = &val
		}
	}
	assign(&update.FirstName, req.FirstName)
	assign(&update.LastName, req.LastName)
	assign(&update.AltEmail, req.AltEmail)
	assign(&update.Mobile, req.Mobile)
	assign(&update.AltMobile, req.AltMobile)
	assign(&update.Locale, req.Locale)
	assign(&update.Timezone, req.Timezone)

	if err := h.repo.Update(ctx, tenantID, actor.UserID, update); err != nil {
		var dup *DuplicateError
		if errors.As(err, &dup) {
			httpx.Fail(w, r, httpx.ErrDuplicate(dup.Field(), "Another user already has this value."))
			return
		}
		httpx.Fail(w, r, mapErr(err, "Your profile"))
		return
	}

	h.auditor.Record(ctx, audit.Entry{
		Action: audit.ActionUserUpdated, EntityType: "user", EntityID: &actor.UserID,
		EntityPublicID: actor.PublicID, After: map[string]any{"self_service": true},
	})
	h.myProfile(w, r)
}

func (h *Handler) myPreferences(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	prefs, err := h.repo.Preferences(ctx, actor.UserID)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}

	out := map[string]any{
		"theme": prefs.Theme, "density": prefs.Density, "language": prefs.Language,
	}
	if prefs.ExtrasJSON.Valid {
		out["extras"] = json.RawMessage(prefs.ExtrasJSON.String)
	}
	httpx.OK(w, r, out)
}

func (h *Handler) updateMyPreferences(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Theme    *string         `json:"theme" validate:"omitempty,oneof=light dark system"`
		Density  *string         `json:"density" validate:"omitempty,oneof=comfortable compact"`
		Language *string         `json:"language" validate:"omitempty,max=16"`
		Extras   json.RawMessage `json:"extras"`
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

	update := PreferencesUpdate{Theme: req.Theme, Density: req.Density, Language: req.Language}
	if len(req.Extras) > 0 {
		if len(req.Extras) > 8192 {
			httpx.Fail(w, r, httpx.ErrField("extras", "TOO_LARGE",
				"Preference data must be under 8 KB."))
			return
		}
		raw := string(req.Extras)
		update.Extras = &raw
	}

	if err := h.repo.SetPreferences(ctx, actor.UserID, update); err != nil {
		httpx.Fail(w, r, httpx.ErrInternal(err))
		return
	}
	h.myPreferences(w, r)
}

// resolveGroupIDs turns group public ids into internal ones, across every
// client the caller can reach.
//
// Reach rather than the tenant header: filtering the roster by group is done
// from the cross-client list as often as from inside one client, and the header
// then names ComplyDesk's own workspace — where no client's groups exist. The
// filter answered "one of those groups was not found" for a group plainly on
// screen.
func (h *Handler) resolveGroupIDs(r *http.Request, publicIDs []string) ([]int64, error) {
	ctx := r.Context()
	reach := appctx.Reach(ctx)

	out := make([]int64, 0, len(publicIDs))
	for _, id := range publicIDs {
		g, err := h.repo.GroupInReach(ctx, reach, strings.TrimSpace(id))
		if err != nil {
			return nil, httpx.ErrField("group_id", "NOT_FOUND", "One of those groups was not found.")
		}
		out = append(out, g.ID)
	}
	return out, nil
}
