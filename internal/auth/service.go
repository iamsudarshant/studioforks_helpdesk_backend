package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/user"
)

// TenantConfig is the slice of tenant configuration auth needs. Declared as an
// interface to keep auth and tenant independent of each other.
type TenantConfig interface {
	PasswordPolicy(ctx context.Context, tenantID int64) Policy
	IntSetting(ctx context.Context, tenantID int64, key string, fallback int) int
	StringSetting(ctx context.Context, tenantID int64, key, fallback string) string
	FeatureEnabled(ctx context.Context, tenantID int64, key string) bool
	LoginIdentifiers(ctx context.Context, tenantID int64, portal appctx.Portal) []string
}

// EventPublisher writes to the transactional outbox. Mail and SMS are never
// sent inline; the worker picks the event up and renders the tenant's template.
type EventPublisher interface {
	Publish(ctx context.Context, tenantID int64, eventKey, aggregateType string, aggregateID int64, payload any) error
}

type Service struct {
	repo    *Repository
	users   *user.Repository
	tokens  *TokenService
	hasher  *Hasher
	sealer  *platform.Sealer
	tenants TenantConfig
	events  EventPublisher
	auditor *audit.Writer
	db      *platform.DB
	cfg     *config.Config
	lookup  ProfileLookup
}

func NewService(
	repo *Repository,
	users *user.Repository,
	tokens *TokenService,
	hasher *Hasher,
	sealer *platform.Sealer,
	tenants TenantConfig,
	events EventPublisher,
	auditor *audit.Writer,
	db *platform.DB,
	cfg *config.Config,
) *Service {
	return &Service{
		repo: repo, users: users, tokens: tokens, hasher: hasher, sealer: sealer,
		tenants: tenants, events: events, auditor: auditor, db: db, cfg: cfg,
	}
}

// --- portal binding ---------------------------------------------------------

// portalForRoles returns the portal a user's roles entitle them to. A user with
// no role for the requested portal is rejected without disclosing whether the
// account exists elsewhere.
func portalAllows(roles []user.Role, portal appctx.Portal) bool {
	for _, r := range roles {
		if appctx.Portal(r.Portal) == portal {
			return true
		}
	}
	return false
}

// --- login ------------------------------------------------------------------

type LoginParams struct {
	Identifier string
	Password   string
	Portal     appctx.Portal
	Remember   bool
}

// LoginResult is either a completed login or an instruction to perform a second
// factor. Exactly one of Tokens / StepToken is populated.
type LoginResult struct {
	AccessToken        string     `json:"access_token,omitempty"`
	RefreshToken       string     `json:"refresh_token,omitempty"`
	ExpiresAt          *time.Time `json:"expires_at,omitempty"`
	TokenType          string     `json:"token_type,omitempty"`
	MustChangePassword bool       `json:"must_change_password"`
	MFARequired        bool       `json:"mfa_required,omitempty"`
	MFAToken           string     `json:"mfa_token,omitempty"`
	// Named `me` because it carries exactly the GET /auth/me payload. Returning
	// it with the tokens saves the client a round trip on every sign-in.
	Me *MeUser `json:"me,omitempty"`
}

// genericLoginError is returned for every failure mode that would otherwise
// reveal whether an account exists, which portal it belongs to, or whether the
// password was the wrong part.
func genericLoginError() error {
	return httpx.ErrUnauthenticated("The credentials you entered are not valid for this portal.")
}

func (s *Service) Login(ctx context.Context, tenant *appctx.Tenant, p LoginParams) (*LoginResult, error) {
	identifiers := s.tenants.LoginIdentifiers(ctx, tenant.ID, p.Portal)

	u, err := s.users.FindByIdentifier(ctx, tenant.ID, p.Identifier, identifiers)
	if err != nil {
		if errors.Is(err, platform.ErrSentinelNotFound) || errors.Is(err, platform.ErrSentinelConflict) {
			// Spend comparable time on the miss path so response timing does
			// not distinguish "no such user" from "wrong password".
			_, _, _ = s.hasher.Verify(p.Password, dummyHash)
			audit.RecordLogin(ctx, s.db.Primary, &tenant.ID, nil, string(p.Portal), p.Identifier, "BAD_CREDENTIALS", nil)
			return nil, genericLoginError()
		}
		return nil, httpx.ErrInternal(err)
	}

	policy := s.tenants.PasswordPolicy(ctx, tenant.ID)

	if err := s.checkAccountState(ctx, tenant, u, p.Portal, p.Identifier); err != nil {
		return nil, err
	}

	if !u.PasswordHash.Valid || u.PasswordHash.String == "" {
		// Imported but never activated.
		audit.RecordLogin(ctx, s.db.Primary, &tenant.ID, &u.ID, string(p.Portal), p.Identifier, "BAD_CREDENTIALS", nil)
		return nil, genericLoginError()
	}

	match, needsRehash, err := s.hasher.Verify(p.Password, u.PasswordHash.String)
	if err != nil {
		slog.ErrorContext(ctx, "verifying password", "error", err, "user_id", u.PublicID)
		return nil, httpx.ErrInternal(err)
	}
	if !match {
		locked, lockErr := s.users.RecordLoginFailure(ctx, u.ID, policy.MaxFailed, policy.LockoutMinutes)
		if lockErr != nil {
			slog.ErrorContext(ctx, "recording login failure", "error", lockErr)
		}
		result := "BAD_CREDENTIALS"
		if locked {
			result = "LOCKED"
			s.auditor.Record(ctx, audit.Entry{
				TenantID: &tenant.ID, ActorID: &u.ID, Action: audit.ActionAccountLocked,
				EntityType: "user", EntityID: &u.ID, EntityPublicID: u.PublicID,
				After: map[string]any{"locked_minutes": policy.LockoutMinutes},
			})
			s.publish(ctx, tenant.ID, "user.account_locked", "user", u.ID, map[string]any{
				"user_id": u.PublicID, "minutes": policy.LockoutMinutes,
			})
		}
		audit.RecordLogin(ctx, s.db.Primary, &tenant.ID, &u.ID, string(p.Portal), p.Identifier, result, nil)
		if locked {
			return nil, httpx.New(httpx.CodeAccountLocked,
				fmt.Sprintf("Too many failed attempts. Your account is locked for %d minutes.", policy.LockoutMinutes))
		}
		return nil, genericLoginError()
	}

	// Upgrade the stored hash when the tenant has raised Argon2 parameters.
	if needsRehash {
		if newHash, hErr := s.hasher.Hash(p.Password); hErr == nil {
			if uErr := s.users.SetPassword(ctx, u.ID, newHash, policy.ExpiryDays, policy.HistoryCount); uErr != nil {
				slog.WarnContext(ctx, "rehashing password failed", "error", uErr)
			}
		}
	}

	// Second factor, when the tenant enables it and the user has enrolled.
	if u.MFAEnabled && s.tenants.FeatureEnabled(ctx, tenant.ID, "mfa") {
		stepToken, err := s.tokens.IssueStepToken(u.ID, tenant.ID, string(p.Portal), "MFA")
		if err != nil {
			return nil, httpx.ErrInternal(err)
		}
		return &LoginResult{MFARequired: true, MFAToken: stepToken}, nil
	}

	return s.completeLogin(ctx, tenant, u, p.Portal, p.Identifier)
}

// dummyHash is a real Argon2id hash of a random value, used to equalise timing
// on the account-not-found path.
const dummyHash = "$argon2id$v=19$m=65536,t=3,p=2$c29tZXNhbHRzb21lc2FsdA$RdescudvJCsgt3ub+b+dWRWJTmaaJObG"

// checkAccountState applies every non-password reason a login may be refused.
func (s *Service) checkAccountState(ctx context.Context, tenant *appctx.Tenant, u *user.User, portal appctx.Portal, identifier string) error {
	if u.LockedUntil.Valid && u.LockedUntil.Time.After(time.Now().UTC()) {
		audit.RecordLogin(ctx, s.db.Primary, &tenant.ID, &u.ID, string(portal), identifier, "LOCKED", nil)
		mins := int(time.Until(u.LockedUntil.Time).Minutes()) + 1
		return httpx.New(httpx.CodeAccountLocked,
			fmt.Sprintf("Your account is locked. Try again in %d minutes or contact your administrator.", mins))
	}

	if u.Status == user.StatusInactive {
		audit.RecordLogin(ctx, s.db.Primary, &tenant.ID, &u.ID, string(portal), identifier, "BAD_CREDENTIALS", nil)
		return genericLoginError()
	}

	roles, err := s.users.RolesFor(ctx, u.ID)
	if err != nil {
		return httpx.ErrInternal(err)
	}
	if !portalAllows(roles, portal) {
		audit.RecordLogin(ctx, s.db.Primary, &tenant.ID, &u.ID, string(portal), identifier, "PORTAL_MISMATCH", nil)
		return httpx.New(httpx.CodePortalMismatch,
			"The credentials you entered are not valid for this portal.")
	}

	// Ex-employee grace period. Past it, the account cannot sign in at all.
	if expiry := s.accessExpiry(ctx, u); expiry != nil && time.Now().UTC().After(*expiry) {
		audit.RecordLogin(ctx, s.db.Primary, &tenant.ID, &u.ID, string(portal), identifier, "ACCESS_EXPIRED", nil)
		return httpx.New(httpx.CodeAccessExpired,
			"Your access period has ended. Contact your administrator if you still need access.")
	}
	return nil
}

// accessExpiry computes last working day + the group's grace period.
func (s *Service) accessExpiry(ctx context.Context, u *user.User) *time.Time {
	if !u.LastWorkingDay.Valid || !u.UserGroupID.Valid {
		return nil
	}
	g, err := s.users.GroupByID(ctx, u.TenantID, u.UserGroupID.Int64)
	if err != nil || g.GracePeriodDays <= 0 {
		return nil
	}
	expiry := u.LastWorkingDay.Time.AddDate(0, 0, g.GracePeriodDays)
	return &expiry
}

func (s *Service) completeLogin(ctx context.Context, tenant *appctx.Tenant, u *user.User, portal appctx.Portal, identifier string) (*LoginResult, error) {
	actor, permsHash, scopeHash, err := s.buildActor(ctx, u, portal, 0)
	if err != nil {
		return nil, err
	}

	refreshToken, refreshHash, err := NewRefreshToken()
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}

	session, err := s.repo.CreateSession(ctx, CreateSessionParams{
		TenantID:  tenant.ID,
		UserID:    u.ID,
		TokenHash: refreshHash,
		Portal:    string(portal),
		IP:        appctx.ClientIP(ctx),
		UserAgent: appctx.UserAgent(ctx),
		ExpiresAt: time.Now().UTC().Add(s.cfg.Auth.RefreshTokenTTL),
	})
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}
	actor.SessionID = session.ID

	maxSessions := s.tenants.IntSetting(ctx, tenant.ID, "max_concurrent_sessions", s.cfg.Auth.MaxConcurrentSessions)
	if err := s.repo.EnforceSessionLimit(ctx, u.ID, maxSessions); err != nil {
		slog.WarnContext(ctx, "enforcing session limit", "error", err)
	}

	accessToken, expiresAt, err := s.tokens.IssueAccess(actor, tenant.Slug, permsHash, scopeHash)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}

	if err := s.users.RecordLoginSuccess(ctx, u.ID); err != nil {
		slog.WarnContext(ctx, "recording login success", "error", err)
	}
	audit.RecordLogin(ctx, s.db.Primary, &tenant.ID, &u.ID, string(portal), identifier, "SUCCESS", &session.ID)

	s.auditor.Record(ctx, audit.Entry{
		TenantID: &tenant.ID, ActorID: &u.ID, Action: audit.ActionLogin,
		EntityType: "session", EntityID: &session.ID, Portal: string(portal),
		ActorName: u.FullName(),
	})

	me, err := s.buildMe(ctx, tenant, u, actor)
	if err != nil {
		return nil, err
	}

	return &LoginResult{
		AccessToken:        accessToken,
		RefreshToken:       refreshToken,
		ExpiresAt:          &expiresAt,
		TokenType:          "Bearer",
		MustChangePassword: u.MustChangePassword,
		Me:                 me,
	}, nil
}

// --- actor hydration --------------------------------------------------------

func (s *Service) buildActor(ctx context.Context, u *user.User, portal appctx.Portal, sessionID int64) (*appctx.Actor, string, string, error) {
	roles, err := s.users.RolesFor(ctx, u.ID)
	if err != nil {
		return nil, "", "", httpx.ErrInternal(err)
	}

	perms, err := s.users.PermissionsFor(ctx, u.ID)
	if err != nil {
		return nil, "", "", httpx.ErrInternal(err)
	}

	scopes, err := s.users.ScopesFor(ctx, u.ID)
	if err != nil {
		return nil, "", "", httpx.ErrInternal(err)
	}

	roleKeys := make([]string, 0, len(roles))
	isSuper, isKarma := false, false
	for _, r := range roles {
		roleKeys = append(roleKeys, r.Key)
		// Both the canonical and the deprecated keys are recognised, so a token
		// still naming SUPER_ADMIN keeps working.
		if user.IsSuperAdminRole(r.Key) {
			isSuper = true
		}
		if user.IsStaffRole(r.Key) {
			isKarma = true
		}
	}

	permSet := make(map[string]struct{}, len(perms))
	for _, p := range perms {
		permSet[p] = struct{}{}
	}

	actor := &appctx.Actor{
		UserID:             u.ID,
		PublicID:           u.PublicID,
		TenantID:           u.TenantID,
		Portal:             portal,
		Email:              u.PreferredEmail(),
		FullName:           u.FullName(),
		Roles:              roleKeys,
		Permissions:        permSet,
		Scopes:             toScopes(scopes),
		SessionID:          sessionID,
		MustChangePassword: u.MustChangePassword,
		IsSuperAdmin:       isSuper,
		IsStaff:            isKarma,
		AccessExpiresAt:    s.accessExpiry(ctx, u),
	}

	// A Karma agent's reach is the set of clients assigned to them. Loaded here
	// so every downstream check reads one consistent list. A super admin needs
	// none: their access is unrestricted by role.
	if isKarma && !isSuper {
		assigned, err := s.users.AssignedTenantIDs(ctx, u.ID)
		if err != nil {
			return nil, "", "", httpx.ErrInternal(err)
		}
		// Non-nil even when empty: an agent with no assignments must be denied
		// everything, not granted everything.
		actor.AssignedTenantIDs = assigned
	}

	if u.UserGroupID.Valid {
		if g, err := s.users.GroupByID(ctx, u.TenantID, u.UserGroupID.Int64); err == nil {
			actor.GroupKey = g.Key
			actor.GroupAccessMode = appctx.AccessMode(g.AccessMode)
		}
	}

	return actor, hashList(perms), hashScopes(scopes), nil
}

// toScopes converts scope rows into the context representation. A dimension
// with no rows stays nil, which means "unrestricted on this dimension"; a
// dimension the user is scoped on becomes a non-nil list.
func toScopes(rows []user.Scope) appctx.Scopes {
	var s appctx.Scopes
	for _, row := range rows {
		switch row.ScopeType {
		case user.ScopeEntity:
			s.Entities = append(s.Entities, row.ScopeID)
		case user.ScopeSite:
			s.Sites = append(s.Sites, row.ScopeID)
		case user.ScopeDepartment:
			s.Departments = append(s.Departments, row.ScopeID)
		case user.ScopeCategory:
			s.Categories = append(s.Categories, row.ScopeID)
		}
	}
	return s
}

func hashList(items []string) string {
	sorted := append([]string(nil), items...)
	sort.Strings(sorted)
	return platform.SHA256Hex([]byte(strings.Join(sorted, "|")))[:16]
}

func hashScopes(rows []user.Scope) string {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s:%d", r.ScopeType, r.ScopeID))
	}
	return hashList(parts)
}

// ActorFromAccessToken implements middleware.ActorLoader.
//
// The token is trusted for identity, but the permission and scope sets are
// re-read from the database and compared against the hashes in the token. An
// administrator revoking a permission therefore invalidates outstanding tokens
// immediately rather than waiting for them to expire.
func (s *Service) ActorFromAccessToken(ctx context.Context, raw string) (*appctx.Actor, error) {
	claims, err := s.tokens.ParseAccess(raw)
	if err != nil {
		return nil, err
	}

	u, err := s.users.ByPublicIDGlobal(ctx, claims.Subject)
	if err != nil {
		return nil, httpx.ErrUnauthenticated("Your session is no longer valid. Sign in again.")
	}
	if u.TenantID != claims.TenantID {
		return nil, httpx.New(httpx.CodeTenantMismatch, "Your session does not belong to this workspace.")
	}
	if u.Status == user.StatusInactive {
		return nil, httpx.ErrUnauthenticated("This account is no longer active.")
	}

	// A revoked session must stop working before its access token expires.
	if claims.SessionID > 0 {
		session, err := s.repo.SessionByID(ctx, claims.SessionID)
		if err != nil || session.RevokedAt.Valid {
			return nil, httpx.ErrUnauthenticated("Your session has been ended. Sign in again.")
		}
	}

	actor, permsHash, scopeHash, err := s.buildActor(ctx, u, appctx.Portal(claims.Portal), claims.SessionID)
	if err != nil {
		return nil, err
	}
	if claims.PermsHash != permsHash || claims.ScopeHash != scopeHash {
		return nil, httpx.ErrUnauthenticated("Your access has changed. Sign in again.")
	}
	return actor, nil
}

// ActorFromAPIKey authenticates a machine caller. API keys act with the
// permissions recorded on the key, never a user's.
func (s *Service) ActorFromAPIKey(ctx context.Context, raw string) (*appctx.Actor, error) {
	key, err := s.repo.APIKeyByHash(ctx, platform.HashToken(raw))
	if err != nil {
		return nil, httpx.ErrUnauthenticated("The API key is not valid.")
	}
	s.repo.TouchAPIKey(ctx, key.ID)

	perms := []string{}
	if key.ScopesJSON.Valid {
		_ = json.Unmarshal([]byte(key.ScopesJSON.String), &perms)
	}
	permSet := make(map[string]struct{}, len(perms))
	for _, p := range perms {
		permSet[p] = struct{}{}
	}

	return &appctx.Actor{
		PublicID:    key.PublicID,
		TenantID:    key.TenantID,
		FullName:    "API key: " + key.Name,
		Roles:       []string{"API_KEY"},
		Permissions: permSet,
	}, nil
}

// --- refresh ----------------------------------------------------------------

func (s *Service) Refresh(ctx context.Context, tenant *appctx.Tenant, refreshToken string) (*LoginResult, error) {
	hash := platform.HashToken(refreshToken)

	session, err := s.repo.SessionByTokenHash(ctx, hash)
	if err != nil {
		return nil, httpx.ErrUnauthenticated("Your session has expired. Sign in again.")
	}

	// Presenting an already-rotated token means the token leaked. Kill the
	// whole family so neither party keeps access, and record it loudly.
	if session.RevokedAt.Valid {
		if err := s.repo.RevokeFamily(ctx, session.FamilyID, "REUSE_DETECTED"); err != nil {
			slog.ErrorContext(ctx, "revoking session family", "error", err)
		}
		s.auditor.Record(ctx, audit.Entry{
			TenantID: &session.TenantID, ActorID: &session.UserID,
			Action: audit.ActionTokenReuse, EntityType: "session", EntityID: &session.ID,
			After: map[string]any{"family_id": session.FamilyID, "reason": session.RevokedReason.String},
		})
		slog.WarnContext(ctx, "refresh token reuse detected",
			"user_id", session.UserID, "family_id", session.FamilyID)
		return nil, httpx.New(httpx.CodeTokenReused,
			"Your session was ended for security reasons. Sign in again.")
	}

	if session.ExpiresAt.Before(time.Now().UTC()) {
		return nil, httpx.ErrUnauthenticated("Your session has expired. Sign in again.")
	}
	if tenant != nil && session.TenantID != tenant.ID {
		return nil, httpx.New(httpx.CodeTenantMismatch, "Your session does not belong to this workspace.")
	}

	u, err := s.users.ByIDAnyTenant(ctx, session.UserID)
	if err != nil {
		return nil, httpx.ErrUnauthenticated("Your session is no longer valid. Sign in again.")
	}
	if u.Status == user.StatusInactive {
		return nil, httpx.ErrUnauthenticated("This account is no longer active.")
	}

	newToken, newHash, err := NewRefreshToken()
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}

	rotated, err := s.repo.RotateSession(ctx, session, newHash,
		time.Now().UTC().Add(s.cfg.Auth.RefreshTokenTTL),
		appctx.ClientIP(ctx), appctx.UserAgent(ctx))
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}

	actor, permsHash, scopeHash, err := s.buildActor(ctx, u, appctx.Portal(session.Portal), rotated.ID)
	if err != nil {
		return nil, err
	}

	slug := session.Portal
	if tenant != nil {
		slug = tenant.Slug
	}
	accessToken, expiresAt, err := s.tokens.IssueAccess(actor, slug, permsHash, scopeHash)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}

	return &LoginResult{
		AccessToken:        accessToken,
		RefreshToken:       newToken,
		ExpiresAt:          &expiresAt,
		TokenType:          "Bearer",
		MustChangePassword: u.MustChangePassword,
	}, nil
}

func (s *Service) Logout(ctx context.Context, sessionID int64) error {
	if sessionID == 0 {
		return nil
	}
	if err := s.repo.RevokeSession(ctx, sessionID, "LOGOUT"); err != nil {
		return httpx.ErrInternal(err)
	}
	s.auditor.Record(ctx, audit.Entry{Action: audit.ActionLogout, EntityType: "session", EntityID: &sessionID})
	return nil
}

func (s *Service) LogoutAll(ctx context.Context, userID int64) error {
	if err := s.repo.RevokeAllForUser(ctx, userID, "LOGOUT_ALL"); err != nil {
		return httpx.ErrInternal(err)
	}
	s.auditor.Record(ctx, audit.Entry{Action: audit.ActionLogout, EntityType: "user", EntityID: &userID,
		After: map[string]any{"scope": "all_sessions"}})
	return nil
}

func (s *Service) Sessions(ctx context.Context, userID int64) ([]Session, error) {
	rows, err := s.repo.ActiveSessions(ctx, userID)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}
	return rows, nil
}

func (s *Service) RevokeSession(ctx context.Context, userID int64, publicID string) error {
	sessions, err := s.repo.ActiveSessions(ctx, userID)
	if err != nil {
		return httpx.ErrInternal(err)
	}
	for _, sess := range sessions {
		if sess.PublicID == publicID {
			if err := s.repo.RevokeSession(ctx, sess.ID, "USER_REVOKED"); err != nil {
				return httpx.ErrInternal(err)
			}
			s.auditor.Record(ctx, audit.Entry{Action: audit.ActionSessionRevoked,
				EntityType: "session", EntityID: &sess.ID})
			return nil
		}
	}
	return httpx.ErrNotFound("That session")
}

func (s *Service) publish(ctx context.Context, tenantID int64, eventKey, aggType string, aggID int64, payload any) {
	if s.events == nil {
		return
	}
	if err := s.events.Publish(ctx, tenantID, eventKey, aggType, aggID, payload); err != nil {
		slog.ErrorContext(ctx, "publishing auth event", "error", err, "event", eventKey)
	}
}

// txPublish writes an outbox row inside an existing transaction.
func txPublish(ctx context.Context, tx *sqlx.Tx, tenantID int64, eventKey, aggType string, aggID int64, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding event payload: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO outbox_events (tenant_id, aggregate_type, aggregate_id, event_key, payload_json)
		VALUES (?,?,?,?,?)`, tenantID, aggType, aggID, eventKey, string(raw))
	if err != nil {
		return fmt.Errorf("writing outbox event: %w", err)
	}
	return nil
}
