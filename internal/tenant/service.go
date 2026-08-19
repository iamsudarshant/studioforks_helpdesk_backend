package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/auth"
	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// Service is the tenant domain API used by handlers and by middleware. It caches
// the two things read on every single request — tenant resolution and the
// maintenance state — behind a short Redis TTL.
type Service struct {
	repo *Repository
	rdb  *redis.Client
	cfg  *config.Config
}

func NewService(repo *Repository, rdb *redis.Client, cfg *config.Config) *Service {
	return &Service{repo: repo, rdb: rdb, cfg: cfg}
}

const tenantCacheTTL = 60 * time.Second

// BySlug implements middleware.TenantResolver.
func (s *Service) BySlug(ctx context.Context, slug string) (*appctx.Tenant, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return nil, platform.ErrSentinelNotFound
	}

	cacheKey := "tenant:slug:" + slug
	if cached := s.readCache(ctx, cacheKey); cached != nil {
		return cached, nil
	}

	row, err := s.repo.BySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	t := toContextTenant(row)
	s.writeCache(ctx, cacheKey, t)
	return t, nil
}

// ByDomain implements middleware.TenantResolver.
func (s *Service) ByDomain(ctx context.Context, domain string) (*appctx.Tenant, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil, platform.ErrSentinelNotFound
	}

	cacheKey := "tenant:domain:" + domain
	if cached := s.readCache(ctx, cacheKey); cached != nil {
		return cached, nil
	}

	row, err := s.repo.ByDomain(ctx, domain)
	if err != nil {
		return nil, err
	}
	t := toContextTenant(row)
	s.writeCache(ctx, cacheKey, t)
	return t, nil
}

// Current implements middleware.MaintenanceChecker.
func (s *Service) Current(ctx context.Context, tenantID int64) (*middleware.MaintenanceState, error) {
	cacheKey := fmt.Sprintf("maintenance:%d", tenantID)

	if s.rdb != nil {
		if raw, err := s.rdb.Get(ctx, cacheKey).Result(); err == nil {
			var state middleware.MaintenanceState
			if json.Unmarshal([]byte(raw), &state) == nil {
				return &state, nil
			}
		}
	}

	state, err := s.repo.currentMaintenance(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	if s.rdb != nil {
		if raw, err := json.Marshal(state); err == nil {
			_ = s.rdb.Set(ctx, cacheKey, raw, maintenanceCacheTTL).Err()
		}
	}
	return state, nil
}

// InvalidateMaintenance clears the cached state after an admin change so the
// window takes effect on the next request rather than after the TTL.
func (s *Service) InvalidateMaintenance(ctx context.Context, tenantID *int64) {
	if s.rdb == nil {
		return
	}
	keys := []string{"maintenance:0"}
	if tenantID != nil {
		keys = append(keys, fmt.Sprintf("maintenance:%d", *tenantID))
	} else {
		// A global window affects every cached tenant entry.
		if found, err := s.rdb.Keys(ctx, "maintenance:*").Result(); err == nil {
			keys = append(keys, found...)
		}
	}
	if len(keys) > 0 {
		_ = s.rdb.Del(ctx, keys...).Err()
	}
}

func (s *Service) InvalidateTenant(ctx context.Context, slug string) {
	if s.rdb == nil {
		return
	}
	_ = s.rdb.Del(ctx, "tenant:slug:"+strings.ToLower(slug)).Err()
	if domains, err := s.rdb.Keys(ctx, "tenant:domain:*").Result(); err == nil && len(domains) > 0 {
		_ = s.rdb.Del(ctx, domains...).Err()
	}
}

func (s *Service) readCache(ctx context.Context, key string) *appctx.Tenant {
	if s.rdb == nil {
		return nil
	}
	raw, err := s.rdb.Get(ctx, key).Result()
	if err != nil {
		return nil
	}
	var t appctx.Tenant
	if json.Unmarshal([]byte(raw), &t) != nil {
		return nil
	}
	return &t
}

func (s *Service) writeCache(ctx context.Context, key string, t *appctx.Tenant) {
	if s.rdb == nil {
		return
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return
	}
	if err := s.rdb.Set(ctx, key, raw, tenantCacheTTL).Err(); err != nil {
		slog.DebugContext(ctx, "caching tenant failed", "error", err, "key", key)
	}
}

func toContextTenant(t *Tenant) *appctx.Tenant {
	return &appctx.Tenant{
		ID:       t.ID,
		PublicID: t.PublicID,
		Slug:     t.Slug,
		Name:     t.Name,
		Status:   t.Status,
		Timezone: t.Timezone,
		Locale:   t.Locale,
		Prefix:   t.TicketPrefix,
	}
}

// Repo exposes the repository for handlers that need direct access.
func (s *Service) Repo() *Repository { return s.repo }

// --- bootstrap payload ------------------------------------------------------

// PublicBranding is what an unauthenticated client needs to render the login
// page correctly branded.
type PublicBranding struct {
	LogoURL     string `json:"logo_url"`
	LogoDarkURL string `json:"logo_dark_url"`
	// ClientLogoURL is the client's own mark, shown alongside the Karma logo for
	// client users and left empty for internal Karma users.
	ClientLogoURL      string `json:"client_logo_url"`
	FaviconURL         string `json:"favicon_url"`
	LoginBgURL         string `json:"login_bg_url"`
	PrimaryColor       string `json:"primary_color"`
	SecondaryColor     string `json:"secondary_color"`
	AccentColor        string `json:"accent_color"`
	ShowComplyDeskLogo bool   `json:"show_complydesk_logo"`
	// ShowClientLogo tells the shell whether to render the second mark at all,
	// so it never has to infer the rule from an empty string.
	ShowClientLogo bool   `json:"show_client_logo"`
	CustomCSS      string `json:"custom_css,omitempty"`
}

type PublicTenant struct {
	Tenant struct {
		Slug string `json:"slug"`
		// ClientCode is what the client-side portals are addressed by —
		// /INF/user, /INF/partners — so the UI cannot build its own URLs
		// without it. Empty for the platform tenant, which has no such portals.
		ClientCode string `json:"client_code"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Timezone   string `json:"timezone"`
		Locale     string `json:"locale"`
		DateFormat string `json:"date_format"`
	} `json:"tenant"`
	Branding         PublicBranding               `json:"branding"`
	Features         map[string]bool              `json:"features"`
	LoginIdentifiers []string                     `json:"login_identifiers"`
	AuthMethods      []string                     `json:"auth_methods"`
	PasswordPolicy   auth.Policy                  `json:"password_policy"`
	Maintenance      *middleware.MaintenanceState `json:"maintenance"`
}

// DefaultLogoURL was a path into the frontend's asset folder, which the API has
// no business knowing — and it was wrong: the file lives at the web root, so the
// dev server answered the request with index.html and every staff portal
// rendered a broken image.
//
// It is now empty on purpose. An empty logo means "ComplyDesk's own mark", and
// the client already ships that asset and knows where it is. The API only names
// an image when there is a real one to name.
const DefaultLogoURL = ""

// PublicConfig assembles the unauthenticated bootstrap payload.
func (s *Service) PublicConfig(ctx context.Context, t *appctx.Tenant) (*PublicTenant, error) {
	row, err := s.repo.ByID(ctx, t.ID)
	if err != nil {
		return nil, err
	}

	branding, err := s.repo.Branding(ctx, t.ID)
	if err != nil {
		return nil, err
	}

	features, err := s.repo.Features(ctx, t.ID)
	if err != nil {
		return nil, err
	}

	var policy auth.Policy
	if err := s.repo.Setting(ctx, t.ID, SettingPasswordPolicy, &policy); err != nil {
		policy = auth.DefaultPolicy()
	}

	var identifiers []string
	if err := s.repo.Setting(ctx, t.ID, SettingLoginIdentifiers, &identifiers); err != nil || len(identifiers) == 0 {
		identifiers = DefaultLoginIdentifiers()
	}

	var methods []string
	if err := s.repo.Setting(ctx, t.ID, SettingAuthMethods, &methods); err != nil || len(methods) == 0 {
		methods = []string{"password"}
	}
	// Advertise OTP only when the tenant actually has the feature enabled.
	if features[FeatureOTPLogin] && !contains(methods, "otp") {
		methods = append(methods, "otp")
	}
	if features[FeatureSSO] && !contains(methods, "sso") {
		methods = append(methods, "sso")
	}

	state, err := s.Current(ctx, t.ID)
	if err != nil {
		state = &middleware.MaintenanceState{Active: false}
	}

	out := &PublicTenant{
		Branding:         BrandingFor(branding, row.IsPlatform, row.Slug),
		Features:         features,
		LoginIdentifiers: identifiers,
		AuthMethods:      methods,
		PasswordPolicy:   policy,
		Maintenance:      state,
	}
	out.Tenant.Slug = row.Slug
	out.Tenant.ClientCode = row.ClientCode.String
	out.Tenant.Name = row.Name
	out.Tenant.Status = row.Status
	out.Tenant.Timezone = row.Timezone
	out.Tenant.Locale = row.Locale
	out.Tenant.DateFormat = row.DateFormat
	return out, nil
}

// BrandingFor applies the rule that branding depends on who is looking:
// internal Karma users see only the Karma logo, client users see the Karma logo
// alongside their own.
//
// It is applied here rather than in the UI so that emails, exported reports and
// any future channel obey the same rule without repeating it.
func BrandingFor(b *Branding, isPlatformTenant bool, slug string) PublicBranding {
	out := toPublicBranding(b)
	if isPlatformTenant {
		// ComplyDesk's own workspace: the client logo slot is meaningless, and
		// there is no second brand to co-display. An empty logo tells the client
		// to use its bundled ComplyDesk mark.
		out.LogoURL = ""
		out.LogoDarkURL = ""
		out.ClientLogoURL = ""
		out.ShowComplyDeskLogo = true
		out.ShowClientLogo = false
		return out
	}
	// A client workspace: the client's mark is the primary one, with ComplyDesk
	// shown beside it.
	//
	// When the client has uploaded nothing, a monogram generated from their name
	// stands in. Falling back to the ComplyDesk logo instead — as this used to —
	// made every client portal identical and lost the dual branding the brief
	// asks for.
	out.ClientLogoURL = out.LogoURL
	if out.ClientLogoURL == "" || out.ClientLogoURL == DefaultLogoURL {
		out.ClientLogoURL = MonogramURL(slug)
		out.LogoURL = out.ClientLogoURL
	}
	out.ShowClientLogo = out.ClientLogoURL != ""
	out.ShowComplyDeskLogo = true
	return out
}

func toPublicBranding(b *Branding) PublicBranding {
	out := PublicBranding{
		PrimaryColor:       b.PrimaryColor,
		SecondaryColor:     b.SecondaryColor,
		AccentColor:        b.AccentColor,
		ShowComplyDeskLogo: b.ShowComplyDeskLogo,
		CustomCSS:          b.CustomCSS.String,
	}

	out.LogoURL = assetURL(b.LogoPath.String)
	if out.LogoURL == "" {
		// No uploaded artwork. Left empty here; BrandingFor substitutes the
		// generated monogram for a client, and the platform tenant keeps it
		// empty so the client renders its own ComplyDesk mark.
		out.ShowComplyDeskLogo = true
	}
	out.LogoDarkURL = assetURL(b.LogoDarkPath.String)
	out.FaviconURL = assetURL(b.FaviconPath.String)
	out.LoginBgURL = assetURL(b.LoginBgPath.String)
	return out
}

// assetURL maps a stored branding path onto its public asset route. Branding
// images are the one class of file served without authentication, because the
// login page needs them.
func assetURL(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return "/api/v1/public/assets/" + strings.TrimPrefix(path, "/")
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// --- settings helpers used across the codebase ------------------------------

// PasswordPolicy loads the tenant's policy, falling back to the platform default.
func (s *Service) PasswordPolicy(ctx context.Context, tenantID int64) auth.Policy {
	var p auth.Policy
	if err := s.repo.Setting(ctx, tenantID, SettingPasswordPolicy, &p); err != nil {
		return auth.DefaultPolicy()
	}
	if p.MinLength == 0 {
		return auth.DefaultPolicy()
	}
	return p
}

// IntSetting reads a numeric setting with a fallback.
func (s *Service) IntSetting(ctx context.Context, tenantID int64, key string, fallback int) int {
	var v int
	if err := s.repo.Setting(ctx, tenantID, key, &v); err != nil {
		return fallback
	}
	return v
}

// StringSetting reads a string setting with a fallback.
func (s *Service) StringSetting(ctx context.Context, tenantID int64, key, fallback string) string {
	var v string
	if err := s.repo.Setting(ctx, tenantID, key, &v); err != nil || strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// FeatureEnabled reports a feature flag, defaulting to off on any error so a
// misconfiguration never silently enables something.
func (s *Service) FeatureEnabled(ctx context.Context, tenantID int64, key string) bool {
	features, err := s.repo.Features(ctx, tenantID)
	if err != nil {
		return false
	}
	return features[key]
}

// LoginIdentifiers returns the identifier columns a portal may authenticate on.
// Only the employee portal gets the full statutory set; every other role is
// restricted to email and alternate email.
func (s *Service) LoginIdentifiers(ctx context.Context, tenantID int64, portal appctx.Portal) []string {
	if portal != appctx.PortalUser {
		return StaffLoginIdentifiers()
	}
	var identifiers []string
	if err := s.repo.Setting(ctx, tenantID, SettingLoginIdentifiers, &identifiers); err != nil || len(identifiers) == 0 {
		return DefaultLoginIdentifiers()
	}
	return identifiers
}

// ErrNotFound translates the repository sentinel into the HTTP taxonomy.
func ErrNotFound(err error, resource string) error {
	if errors.Is(err, platform.ErrSentinelNotFound) {
		return httpx.ErrNotFound(resource)
	}
	return err
}
