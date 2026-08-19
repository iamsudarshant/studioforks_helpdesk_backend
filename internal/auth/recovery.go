package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/user"
)

// genericRecoveryMessage is returned for every forgot-username and
// forgot-password request, whether or not an account matched. The endpoints
// must never be usable to enumerate accounts.
const genericRecoveryMessage = "If an account matches the details you provided, we have sent instructions to the registered email address."

// RecoveryResult is the uniform response for both recovery flows.
type RecoveryResult struct {
	Message string `json:"message"`
}

// recoveryIdentifiers restricts which identifier types a portal may use for
// recovery. Employees may recover using their statutory numbers; every other
// role is limited to email and alternate email, per the brief.
func (s *Service) recoveryIdentifiers(ctx context.Context, tenantID int64, portal appctx.Portal) []string {
	if portal != appctx.PortalUser {
		return []string{"email", "alt_email"}
	}
	allowed := s.tenants.LoginIdentifiers(ctx, tenantID, portal)
	// Recovery deliberately excludes username: knowing the username is what the
	// user is trying to recover.
	out := make([]string, 0, len(allowed))
	for _, a := range allowed {
		if a != "username" {
			out = append(out, a)
		}
	}
	return out
}

type ForgotParams struct {
	Identifier     string
	IdentifierType string
	Portal         appctx.Portal
}

// ForgotUsername emails the account's actual username to the official address
// and, when present, the alternate address.
func (s *Service) ForgotUsername(ctx context.Context, tenant *appctx.Tenant, p ForgotParams) (*RecoveryResult, error) {
	allowed := s.recoveryIdentifiers(ctx, tenant.ID, p.Portal)

	u, err := s.users.FindForRecovery(ctx, tenant.ID, p.IdentifierType, p.Identifier, allowed)
	if err != nil {
		// Same response and no early return distinction on the miss path.
		if !errors.Is(err, platform.ErrSentinelNotFound) && !errors.Is(err, platform.ErrSentinelConflict) {
			slog.ErrorContext(ctx, "forgot username lookup", "error", err)
		}
		return &RecoveryResult{Message: genericRecoveryMessage}, nil
	}

	recipients := recoveryRecipients(u)
	if len(recipients) == 0 {
		// No address to send to. The response stays identical.
		slog.WarnContext(ctx, "username recovery requested for account with no email", "user_id", u.PublicID)
		return &RecoveryResult{Message: genericRecoveryMessage}, nil
	}

	s.publish(ctx, tenant.ID, "user.username_recovery", "user", u.ID, map[string]any{
		"user_public_id": u.PublicID,
		"username":       u.LoginName(),
		"full_name":      u.FullName(),
		"recipients":     recipients,
		"portal":         string(p.Portal),
		"tenant_slug":    tenant.Slug,
	})

	s.auditor.Record(ctx, audit.Entry{
		TenantID: &tenant.ID, ActorID: &u.ID, Action: audit.ActionUsernameRecovered,
		EntityType: "user", EntityID: &u.ID, EntityPublicID: u.PublicID,
		After: map[string]any{"sent_to": maskAll(recipients)},
	})

	return &RecoveryResult{Message: genericRecoveryMessage}, nil
}

// ForgotPassword issues a temporary password and mails it together with the
// actual username. The next login is forced through the change-password screen.
//
// This is deliberately different from SendResetLink: the brief specifies a
// temporary password for self-service recovery, and a reset link for the
// administrator-initiated flow.
func (s *Service) ForgotPassword(ctx context.Context, tenant *appctx.Tenant, p ForgotParams) (*RecoveryResult, error) {
	allowed := s.recoveryIdentifiers(ctx, tenant.ID, p.Portal)

	u, err := s.users.FindForRecovery(ctx, tenant.ID, p.IdentifierType, p.Identifier, allowed)
	if err != nil {
		if !errors.Is(err, platform.ErrSentinelNotFound) && !errors.Is(err, platform.ErrSentinelConflict) {
			slog.ErrorContext(ctx, "forgot password lookup", "error", err)
		}
		return &RecoveryResult{Message: genericRecoveryMessage}, nil
	}

	recipients := recoveryRecipients(u)
	if len(recipients) == 0 {
		slog.WarnContext(ctx, "password recovery requested for account with no email", "user_id", u.PublicID)
		return &RecoveryResult{Message: genericRecoveryMessage}, nil
	}

	tempPassword, err := platform.RandomPassword(12)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}
	hash, err := s.hasher.Hash(tempPassword)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}
	if err := s.users.SetTemporaryPassword(ctx, u.ID, hash); err != nil {
		return nil, httpx.ErrInternal(err)
	}

	// Record the issue so it can be correlated later, and so a second request
	// invalidates the first.
	if err := s.repo.CreateResetToken(ctx, tenant.ID, u.ID, platform.HashToken(tempPassword),
		TokenTypeTempPassword, "EMAIL", strings.Join(recipients, ","), nil,
		appctx.ClientIP(ctx), s.cfg.Auth.ActivationTokenTTL); err != nil {
		slog.ErrorContext(ctx, "recording temp password issue", "error", err)
	}

	// Every existing session is invalidated: a password reset must log the
	// account out everywhere.
	if err := s.repo.RevokeAllForUser(ctx, u.ID, "PASSWORD_RESET"); err != nil {
		slog.WarnContext(ctx, "revoking sessions after password reset", "error", err)
	}

	s.publish(ctx, tenant.ID, "user.temp_password", "user", u.ID, map[string]any{
		"user_public_id":     u.PublicID,
		"username":           u.LoginName(),
		"full_name":          u.FullName(),
		"temporary_password": tempPassword,
		"recipients":         recipients,
		"portal":             string(p.Portal),
		"tenant_slug":        tenant.Slug,
		"expires_hours":      int(s.cfg.Auth.ActivationTokenTTL.Hours()),
	})

	s.auditor.Record(ctx, audit.Entry{
		TenantID: &tenant.ID, ActorID: &u.ID, Action: audit.ActionPasswordReset,
		EntityType: "user", EntityID: &u.ID, EntityPublicID: u.PublicID,
		After: map[string]any{"method": "temporary_password", "sent_to": maskAll(recipients)},
	})

	return &RecoveryResult{Message: genericRecoveryMessage}, nil
}

// SendResetLink is the administrator-initiated flow behind the Reset Password
// button on the user table. It mails a single-use link that opens the
// "new password + confirm new password" screen.
func (s *Service) SendResetLink(ctx context.Context, tenant *appctx.Tenant, target *user.User, portal appctx.Portal) error {
	recipients := recoveryRecipients(target)
	if len(recipients) == 0 {
		return httpx.ErrField("user", "NO_EMAIL",
			"This user has no email or alternate email address on record, so a reset link cannot be sent.")
	}

	token, err := platform.RandomToken(32)
	if err != nil {
		return httpx.ErrInternal(err)
	}

	var requestedBy *int64
	if actor := appctx.ActorFrom(ctx); actor != nil {
		id := actor.UserID
		requestedBy = &id
	}

	if err := s.repo.CreateResetToken(ctx, tenant.ID, target.ID, platform.HashToken(token),
		TokenTypeResetLink, "EMAIL", strings.Join(recipients, ","), requestedBy,
		appctx.ClientIP(ctx), s.cfg.Auth.ResetTokenTTL); err != nil {
		return httpx.ErrInternal(err)
	}

	s.publish(ctx, tenant.ID, "user.password_reset_link", "user", target.ID, map[string]any{
		"user_public_id": target.PublicID,
		"username":       target.LoginName(),
		"full_name":      target.FullName(),
		"reset_url":      s.resetURL(tenant.Slug, portal, token),
		"recipients":     recipients,
		"expires_mins":   int(s.cfg.Auth.ResetTokenTTL.Minutes()),
		"tenant_slug":    tenant.Slug,
	})

	s.auditor.Record(ctx, audit.Entry{
		TenantID: &tenant.ID, Action: audit.ActionResetLinkSent,
		EntityType: "user", EntityID: &target.ID, EntityPublicID: target.PublicID,
		After: map[string]any{"sent_to": maskAll(recipients)},
	})
	return nil
}

// resetURL builds the frontend link the user clicks. The portal is included so
// the link opens the correct portal's reset screen.
func (s *Service) resetURL(tenantSlug string, portal appctx.Portal, token string) string {
	base := strings.TrimRight(s.cfg.App.FrontendURL, "/")
	p := string(portal)
	if p == "" {
		p = string(appctx.PortalUser)
	}
	return fmt.Sprintf("%s/%s/reset-password?token=%s&tenant=%s",
		base, p, url.QueryEscape(token), url.QueryEscape(tenantSlug))
}

// ResetTokenInfo is returned when the frontend validates a reset link before
// rendering the form.
type ResetTokenInfo struct {
	Valid      bool   `json:"valid"`
	UserMasked string `json:"user_masked,omitempty"`
	Policy     Policy `json:"policy"`
	ExpiresAt  string `json:"expires_at,omitempty"`
}

// ValidateResetToken checks a link before the user types a new password, so an
// expired link shows a "request a new one" state rather than failing on submit.
func (s *Service) ValidateResetToken(ctx context.Context, token string) (*ResetTokenInfo, error) {
	rt, err := s.repo.ResetTokenByHash(ctx, platform.HashToken(token))
	if err != nil {
		return &ResetTokenInfo{Valid: false, Policy: DefaultPolicy()}, nil
	}

	u, err := s.users.ByIDAnyTenant(ctx, rt.UserID)
	if err != nil {
		return &ResetTokenInfo{Valid: false, Policy: DefaultPolicy()}, nil
	}

	return &ResetTokenInfo{
		Valid:      true,
		UserMasked: maskEmail(u.PreferredEmail()),
		Policy:     s.tenants.PasswordPolicy(ctx, rt.TenantID),
		ExpiresAt:  rt.ExpiresAt.Format(time.RFC3339),
	}, nil
}

type ResetPasswordParams struct {
	Token           string
	NewPassword     string
	ConfirmPassword string
}

// ResetPassword completes the reset-link flow.
func (s *Service) ResetPassword(ctx context.Context, p ResetPasswordParams) error {
	if p.NewPassword != p.ConfirmPassword {
		return httpx.ErrField("confirm_password", "MISMATCH", "The two passwords do not match.")
	}

	rt, err := s.repo.ResetTokenByHash(ctx, platform.HashToken(p.Token))
	if err != nil {
		return httpx.ErrField("token", "INVALID",
			"This reset link is invalid or has expired. Request a new one.")
	}

	u, err := s.users.ByIDAnyTenant(ctx, rt.UserID)
	if err != nil {
		return httpx.ErrField("token", "INVALID", "This reset link is no longer valid.")
	}

	policy := s.tenants.PasswordPolicy(ctx, rt.TenantID)
	if err := policy.Validate(p.NewPassword, personalHints(u)...); err != nil {
		return err
	}

	if policy.HistoryCount > 0 {
		previous, err := s.users.PasswordHistory(ctx, u.ID, int64(policy.HistoryCount))
		if err != nil {
			return httpx.ErrInternal(err)
		}
		if err := s.hasher.CheckHistory(p.NewPassword, previous); err != nil {
			return err
		}
	}

	// Consume the token first: if two requests race, only one wins.
	if err := s.repo.ConsumeResetToken(ctx, rt.ID); err != nil {
		return httpx.ErrField("token", "INVALID", "This reset link has already been used.")
	}

	hash, err := s.hasher.Hash(p.NewPassword)
	if err != nil {
		return httpx.ErrInternal(err)
	}
	if err := s.users.SetPassword(ctx, u.ID, hash, policy.ExpiryDays, policy.HistoryCount); err != nil {
		return httpx.ErrInternal(err)
	}
	if err := s.repo.RevokeAllForUser(ctx, u.ID, "PASSWORD_RESET"); err != nil {
		slog.WarnContext(ctx, "revoking sessions after reset", "error", err)
	}

	s.auditor.Record(ctx, audit.Entry{
		TenantID: &rt.TenantID, ActorID: &u.ID, Action: audit.ActionPasswordChanged,
		EntityType: "user", EntityID: &u.ID, EntityPublicID: u.PublicID,
		After: map[string]any{"method": "reset_link"},
	})
	return nil
}

type ChangePasswordParams struct {
	CurrentPassword string
	NewPassword     string
	ConfirmPassword string
}

// ChangePassword handles both the voluntary change and the forced change after
// a temporary password or bulk onboarding.
func (s *Service) ChangePassword(ctx context.Context, actor *appctx.Actor, p ChangePasswordParams) error {
	if p.NewPassword != p.ConfirmPassword {
		return httpx.ErrField("confirm_password", "MISMATCH", "The two passwords do not match.")
	}
	if p.CurrentPassword == p.NewPassword {
		return httpx.ErrField("new_password", "UNCHANGED",
			"The new password must be different from your current password.")
	}

	u, err := s.users.ByIDAnyTenant(ctx, actor.UserID)
	if err != nil {
		return httpx.ErrInternal(err)
	}
	if !u.PasswordHash.Valid {
		return httpx.ErrField("current_password", "INVALID", "Your current password is not correct.")
	}

	match, _, err := s.hasher.Verify(p.CurrentPassword, u.PasswordHash.String)
	if err != nil {
		return httpx.ErrInternal(err)
	}
	if !match {
		return httpx.ErrField("current_password", "INVALID", "Your current password is not correct.")
	}

	policy := s.tenants.PasswordPolicy(ctx, u.TenantID)
	if err := policy.Validate(p.NewPassword, personalHints(u)...); err != nil {
		return err
	}

	if policy.HistoryCount > 0 {
		previous, err := s.users.PasswordHistory(ctx, u.ID, int64(policy.HistoryCount))
		if err != nil {
			return httpx.ErrInternal(err)
		}
		if err := s.hasher.CheckHistory(p.NewPassword, previous); err != nil {
			return err
		}
	}

	hash, err := s.hasher.Hash(p.NewPassword)
	if err != nil {
		return httpx.ErrInternal(err)
	}
	if err := s.users.SetPassword(ctx, u.ID, hash, policy.ExpiryDays, policy.HistoryCount); err != nil {
		return httpx.ErrInternal(err)
	}

	// Keep the current session alive, end every other one.
	if err := s.repo.RevokeAllForUser(ctx, u.ID, "PASSWORD_CHANGED"); err != nil {
		slog.WarnContext(ctx, "revoking sessions after password change", "error", err)
	}

	s.auditor.Record(ctx, audit.Entry{
		TenantID: &u.TenantID, ActorID: &u.ID, Action: audit.ActionPasswordChanged,
		EntityType: "user", EntityID: &u.ID, EntityPublicID: u.PublicID,
		After: map[string]any{"method": "self_service", "forced": u.MustChangePassword},
	})
	return nil
}

// --- helpers ----------------------------------------------------------------

// recoveryRecipients returns the official address first, then the alternate.
// Both are used when both exist, per the brief.
func recoveryRecipients(u *user.User) []string {
	seen := map[string]struct{}{}
	out := []string{}

	for _, addr := range []string{u.Email.String, u.AltEmail.String} {
		addr = strings.TrimSpace(strings.ToLower(addr))
		if addr == "" {
			continue
		}
		if _, dup := seen[addr]; dup {
			continue
		}
		seen[addr] = struct{}{}
		out = append(out, addr)
	}
	return out
}

// personalHints are the values a password must not contain.
func personalHints(u *user.User) []string {
	hints := []string{u.FirstName, u.LastName.String, u.EmployeeCode.String,
		u.PFNumber.String, u.UANNumber.String, u.PANNumber.String}
	if u.Email.Valid {
		if at := strings.Index(u.Email.String, "@"); at > 0 {
			hints = append(hints, u.Email.String[:at])
		}
	}
	out := make([]string, 0, len(hints))
	for _, h := range hints {
		if strings.TrimSpace(h) != "" {
			out = append(out, h)
		}
	}
	return out
}

func maskEmail(addr string) string {
	at := strings.Index(addr, "@")
	if at <= 0 {
		return "***"
	}
	local := addr[:at]
	if len(local) <= 2 {
		return local[:1] + "***" + addr[at:]
	}
	return local[:2] + strings.Repeat("*", len(local)-2) + addr[at:]
}

func maskAll(addrs []string) []string {
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = maskEmail(a)
	}
	return out
}
