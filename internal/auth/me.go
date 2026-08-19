package auth

import (
	"context"
	"encoding/json"
	"time"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/user"
)

// MeUser is the /auth/me payload. Everything the frontend gates on — permissions,
// scopes, group access mode, feature flags — comes from here, so the client
// never has to infer capability from a role name.
type MeUser struct {
	User struct {
		ID                 string  `json:"id"`
		EmployeeCode       string  `json:"employee_code"`
		Username           string  `json:"username"`
		FirstName          string  `json:"first_name"`
		LastName           string  `json:"last_name"`
		FullName           string  `json:"full_name"`
		Email              string  `json:"email"`
		AltEmail           string  `json:"alt_email"`
		Mobile             string  `json:"mobile"`
		PANNumber          string  `json:"pan_number"`
		UANNumber          string  `json:"uan_number"`
		PFNumber           string  `json:"pf_number"`
		ESICNumber         string  `json:"esic_number"`
		DateOfJoining      *string `json:"date_of_joining"`
		LastWorkingDay     *string `json:"last_working_day"`
		Designation        string  `json:"designation"`
		AvatarURL          string  `json:"avatar_url"`
		Status             string  `json:"status"`
		MustChangePassword bool    `json:"must_change_password"`
		MFAEnabled         bool    `json:"mfa_enabled"`
		Locale             string  `json:"locale"`
		Timezone           string  `json:"timezone"`
		Entity             *Ref    `json:"entity"`
		Site               *Ref    `json:"site"`
		Department         *Ref    `json:"department"`
	} `json:"user"`

	Tenant struct {
		ID           string `json:"id"`
		Slug         string `json:"slug"`
		Name         string `json:"name"`
		Timezone     string `json:"timezone"`
		DateFormat   string `json:"date_format"`
		TicketPrefix string `json:"ticket_prefix"`
	} `json:"tenant"`

	Roles       []string        `json:"roles"`
	Portal      string          `json:"portal"`
	Permissions []string        `json:"permissions"`
	Scopes      MeScopes        `json:"scopes"`
	Group       *MeGroup        `json:"group"`
	Features    map[string]bool `json:"features"`
	Preferences MePreferences   `json:"preferences"`
}

type Ref struct {
	ID   string `json:"id"`
	Code string `json:"code,omitempty"`
	Name string `json:"name"`
}

type MeScopes struct {
	Entities    []Ref `json:"entities"`
	Sites       []Ref `json:"sites"`
	Departments []Ref `json:"departments"`
	Categories  []Ref `json:"categories"`
}

type MeGroup struct {
	Key             string     `json:"key"`
	Name            string     `json:"name"`
	AccessMode      string     `json:"access_mode"`
	GracePeriodDays int        `json:"grace_period_days"`
	ExpiresAt       *time.Time `json:"expires_at"`
}

type MePreferences struct {
	Theme    string          `json:"theme"`
	Density  string          `json:"density"`
	Language string          `json:"language"`
	Extras   json.RawMessage `json:"extras,omitempty"`
}

// ProfileLookup resolves scope ids into display references and reads feature
// flags. Implemented by the composition root to avoid a package cycle.
type ProfileLookup interface {
	EntityRefs(ctx context.Context, tenantID int64, ids []int64) []Ref
	SiteRefs(ctx context.Context, tenantID int64, ids []int64) []Ref
	DepartmentRefs(ctx context.Context, tenantID int64, ids []int64) []Ref
	CategoryRefs(ctx context.Context, tenantID int64, ids []int64) []Ref
	Features(ctx context.Context, tenantID int64) map[string]bool
}

// SetProfileLookup wires the reference resolver after construction.
func (s *Service) SetProfileLookup(l ProfileLookup) { s.lookup = l }

// Me builds the /auth/me payload for an authenticated request.
func (s *Service) Me(ctx context.Context, tenant *appctx.Tenant, actor *appctx.Actor) (*MeUser, error) {
	u, err := s.users.ByIDAnyTenant(ctx, actor.UserID)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}
	return s.buildMe(ctx, tenant, u, actor)
}

func (s *Service) buildMe(ctx context.Context, tenant *appctx.Tenant, u *user.User, actor *appctx.Actor) (*MeUser, error) {
	me := &MeUser{
		Roles:       actor.Roles,
		Portal:      string(actor.Portal),
		Permissions: sortedPermissions(actor.Permissions),
	}

	me.User.ID = u.PublicID
	me.User.EmployeeCode = u.EmployeeCode.String
	me.User.Username = u.Username.String
	me.User.FirstName = u.FirstName
	me.User.LastName = u.LastName.String
	me.User.FullName = u.FullName()
	me.User.Email = u.Email.String
	me.User.AltEmail = u.AltEmail.String
	me.User.Mobile = u.Mobile.String
	me.User.PANNumber = u.PANNumber.String
	me.User.UANNumber = u.UANNumber.String
	me.User.PFNumber = u.PFNumber.String
	me.User.ESICNumber = u.ESICNumber.String
	me.User.DateOfJoining = dateString(u.DateOfJoining.Valid, u.DateOfJoining.Time)
	me.User.LastWorkingDay = dateString(u.LastWorkingDay.Valid, u.LastWorkingDay.Time)
	me.User.Designation = u.Designation.String
	me.User.Status = u.Status
	me.User.MustChangePassword = u.MustChangePassword
	me.User.MFAEnabled = u.MFAEnabled
	me.User.Locale = u.Locale.String
	me.User.Timezone = u.Timezone.String
	if u.AvatarPath.Valid && u.AvatarPath.String != "" {
		me.User.AvatarURL = "/api/v1/public/documents/avatar/" + u.PublicID
	}

	me.Tenant.ID = tenant.PublicID
	me.Tenant.Slug = tenant.Slug
	me.Tenant.Name = tenant.Name
	me.Tenant.Timezone = tenant.Timezone
	me.Tenant.TicketPrefix = tenant.Prefix

	if s.lookup != nil {
		if u.EntityID.Valid {
			if refs := s.lookup.EntityRefs(ctx, u.TenantID, []int64{u.EntityID.Int64}); len(refs) > 0 {
				me.User.Entity = &refs[0]
			}
		}
		if u.SiteID.Valid {
			if refs := s.lookup.SiteRefs(ctx, u.TenantID, []int64{u.SiteID.Int64}); len(refs) > 0 {
				me.User.Site = &refs[0]
			}
		}
		if u.DepartmentID.Valid {
			if refs := s.lookup.DepartmentRefs(ctx, u.TenantID, []int64{u.DepartmentID.Int64}); len(refs) > 0 {
				me.User.Department = &refs[0]
			}
		}

		me.Scopes = MeScopes{
			Entities:    s.lookup.EntityRefs(ctx, u.TenantID, actor.Scopes.Entities),
			Sites:       s.lookup.SiteRefs(ctx, u.TenantID, actor.Scopes.Sites),
			Departments: s.lookup.DepartmentRefs(ctx, u.TenantID, actor.Scopes.Departments),
			Categories:  s.lookup.CategoryRefs(ctx, u.TenantID, actor.Scopes.Categories),
		}
		me.Features = s.lookup.Features(ctx, u.TenantID)
	}

	if me.Scopes.Entities == nil {
		me.Scopes.Entities = []Ref{}
	}
	if me.Scopes.Sites == nil {
		me.Scopes.Sites = []Ref{}
	}
	if me.Scopes.Departments == nil {
		me.Scopes.Departments = []Ref{}
	}
	if me.Scopes.Categories == nil {
		me.Scopes.Categories = []Ref{}
	}
	if me.Features == nil {
		me.Features = map[string]bool{}
	}

	if u.UserGroupID.Valid {
		if g, err := s.users.GroupByID(ctx, u.TenantID, u.UserGroupID.Int64); err == nil {
			me.Group = &MeGroup{
				Key:             g.Key,
				Name:            g.Name,
				AccessMode:      g.AccessMode,
				GracePeriodDays: g.GracePeriodDays,
				ExpiresAt:       actor.AccessExpiresAt,
			}
		}
	}

	prefs, err := s.users.Preferences(ctx, u.ID)
	if err == nil && prefs != nil {
		me.Preferences = MePreferences{
			Theme:    prefs.Theme,
			Density:  prefs.Density,
			Language: prefs.Language,
		}
		if prefs.ExtrasJSON.Valid {
			me.Preferences.Extras = json.RawMessage(prefs.ExtrasJSON.String)
		}
	} else {
		me.Preferences = MePreferences{Theme: "system", Density: "comfortable", Language: "en"}
	}

	return me, nil
}

func sortedPermissions(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func dateString(valid bool, t time.Time) *string {
	if !valid {
		return nil
	}
	s := t.Format("2006-01-02")
	return &s
}
