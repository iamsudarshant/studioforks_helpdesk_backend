package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/user"
)

// --- login OTP (mobile) -----------------------------------------------------

type OTPRequestResult struct {
	Message   string `json:"message"`
	MaskedTo  string `json:"masked_to,omitempty"`
	ExpiresIn int    `json:"expires_in_seconds"`
}

// RequestLoginOTP sends a one-time code to the account's mobile number. The
// response is identical whether or not the account exists.
func (s *Service) RequestLoginOTP(ctx context.Context, tenant *appctx.Tenant, identifier string, portal appctx.Portal) (*OTPRequestResult, error) {
	if !s.tenants.FeatureEnabled(ctx, tenant.ID, "otp_login") {
		return nil, httpx.ErrForbidden("One-time password sign-in is not enabled for this workspace.")
	}

	generic := &OTPRequestResult{
		Message:   "If an account matches, a one-time code has been sent to the registered mobile number.",
		ExpiresIn: int(s.cfg.Auth.OTPTTL.Seconds()),
	}

	allowed := s.tenants.LoginIdentifiers(ctx, tenant.ID, portal)
	u, err := s.users.FindByIdentifier(ctx, tenant.ID, identifier, allowed)
	if err != nil {
		return generic, nil
	}
	if !u.Mobile.Valid || strings.TrimSpace(u.Mobile.String) == "" {
		return generic, nil
	}

	roles, err := s.users.RolesFor(ctx, u.ID)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}
	if !portalAllows(roles, portal) {
		return generic, nil
	}

	code, err := platform.NumericCode(6)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}
	if err := s.repo.CreateOTP(ctx, tenant.ID, u.ID, OTPPurposeLogin,
		platform.HashToken(code), u.Mobile.String, s.cfg.Auth.OTPTTL); err != nil {
		return nil, httpx.ErrInternal(err)
	}

	s.publish(ctx, tenant.ID, "user.login_otp", "user", u.ID, map[string]any{
		"user_public_id": u.PublicID,
		"mobile":         u.Mobile.String,
		"code":           code,
		"expires_mins":   int(s.cfg.Auth.OTPTTL.Minutes()),
		"channel":        "SMS",
	})

	generic.MaskedTo = maskMobile(u.Mobile.String)
	return generic, nil
}

// VerifyLoginOTP completes an OTP sign-in.
func (s *Service) VerifyLoginOTP(ctx context.Context, tenant *appctx.Tenant, identifier, code string, portal appctx.Portal) (*LoginResult, error) {
	allowed := s.tenants.LoginIdentifiers(ctx, tenant.ID, portal)

	u, err := s.users.FindByIdentifier(ctx, tenant.ID, identifier, allowed)
	if err != nil {
		return nil, httpx.New(httpx.CodeOTPInvalid, "That code is not valid or has expired.")
	}
	if err := s.checkAccountState(ctx, tenant, u, portal, identifier); err != nil {
		return nil, err
	}
	if err := s.consumeOTP(ctx, u.ID, OTPPurposeLogin, code); err != nil {
		return nil, err
	}
	return s.completeLogin(ctx, tenant, u, portal, identifier)
}

// consumeOTP validates and burns a one-time code, enforcing the attempt cap.
func (s *Service) consumeOTP(ctx context.Context, userID int64, purpose, code string) error {
	record, err := s.repo.ActiveOTP(ctx, userID, purpose)
	if err != nil {
		return httpx.New(httpx.CodeOTPInvalid, "That code is not valid or has expired.")
	}

	if record.Attempts >= s.cfg.Auth.OTPMaxAttempts {
		_ = s.repo.ConsumeOTP(ctx, record.ID)
		return httpx.New(httpx.CodeOTPInvalid,
			"Too many incorrect attempts. Request a new code.")
	}

	if !platform.ConstantTimeEqual(record.CodeHash, platform.HashToken(code)) {
		if err := s.repo.IncrementOTPAttempts(ctx, record.ID); err != nil {
			slog.ErrorContext(ctx, "incrementing otp attempts", "error", err)
		}
		return httpx.New(httpx.CodeOTPInvalid, "That code is not valid or has expired.")
	}

	if err := s.repo.ConsumeOTP(ctx, record.ID); err != nil {
		return httpx.ErrInternal(err)
	}
	return nil
}

// --- TOTP multi-factor ------------------------------------------------------

type MFAEnrolment struct {
	Secret        string   `json:"secret"`
	OTPAuthURL    string   `json:"otpauth_url"`
	RecoveryCodes []string `json:"recovery_codes"`
}

// EnrolMFA generates a TOTP secret and recovery codes. The secret is not active
// until ConfirmMFA verifies a code, so a failed enrolment cannot lock the user
// out of their own account.
func (s *Service) EnrolMFA(ctx context.Context, tenant *appctx.Tenant, actor *appctx.Actor) (*MFAEnrolment, error) {
	if !s.tenants.FeatureEnabled(ctx, tenant.ID, "mfa") {
		return nil, httpx.ErrForbidden("Multi-factor authentication is not enabled for this workspace.")
	}

	u, err := s.users.ByIDAnyTenant(ctx, actor.UserID)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}

	accountName := u.PreferredEmail()
	if accountName == "" {
		accountName = u.LoginName()
	}

	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      fmt.Sprintf("ComplyDesk (%s)", tenant.Name),
		AccountName: accountName,
		Period:      30,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return nil, httpx.ErrInternal(fmt.Errorf("generating totp key: %w", err))
	}

	codes, err := generateRecoveryCodes(10)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}

	// Hash the recovery codes; the plaintext is shown once and never stored.
	hashed := make([]string, len(codes))
	for i, c := range codes {
		hashed[i] = platform.HashToken(c)
	}
	recoveryJSON, err := json.Marshal(hashed)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}

	sealed, err := s.sealer.Seal([]byte(key.Secret()), []byte(tenant.Slug))
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}

	if err := s.users.StageMFASecret(ctx, u.ID, sealed, string(recoveryJSON)); err != nil {
		return nil, httpx.ErrInternal(err)
	}

	return &MFAEnrolment{
		Secret:        key.Secret(),
		OTPAuthURL:    key.URL(),
		RecoveryCodes: codes,
	}, nil
}

// ConfirmMFA activates a staged enrolment after the user proves they can
// generate a valid code.
func (s *Service) ConfirmMFA(ctx context.Context, tenant *appctx.Tenant, actor *appctx.Actor, code string) error {
	u, err := s.users.ByIDAnyTenant(ctx, actor.UserID)
	if err != nil {
		return httpx.ErrInternal(err)
	}
	if len(u.MFASecretEnc) == 0 {
		return httpx.ErrField("code", "NOT_ENROLLED", "Start multi-factor setup before confirming it.")
	}

	secret, err := s.sealer.Open(u.MFASecretEnc, []byte(tenant.Slug))
	if err != nil {
		return httpx.ErrInternal(err)
	}
	if !totp.Validate(code, string(secret)) {
		return httpx.ErrField("code", "INVALID", "That code is not correct. Check your authenticator app and try again.")
	}

	if err := s.users.SetMFAEnabled(ctx, u.ID, true); err != nil {
		return httpx.ErrInternal(err)
	}

	s.auditor.Record(ctx, audit.Entry{
		TenantID: &tenant.ID, ActorID: &u.ID, Action: audit.ActionMFAEnrolled,
		EntityType: "user", EntityID: &u.ID, EntityPublicID: u.PublicID,
	})
	return nil
}

// VerifyMFA completes a login that stopped at the second factor.
func (s *Service) VerifyMFA(ctx context.Context, tenant *appctx.Tenant, mfaToken, code, recoveryCode string) (*LoginResult, error) {
	claims, err := s.tokens.ParseStepToken(mfaToken, "MFA")
	if err != nil {
		return nil, err
	}
	if claims.TenantID != tenant.ID {
		return nil, httpx.New(httpx.CodeTenantMismatch, "This verification does not belong to this workspace.")
	}

	u, err := s.users.ByIDAnyTenant(ctx, claims.UserID)
	if err != nil {
		return nil, httpx.ErrUnauthenticated("This verification step is no longer valid.")
	}

	switch {
	case strings.TrimSpace(recoveryCode) != "":
		ok, err := s.users.ConsumeRecoveryCode(ctx, u.ID, platform.HashToken(strings.TrimSpace(recoveryCode)))
		if err != nil {
			return nil, httpx.ErrInternal(err)
		}
		if !ok {
			return nil, httpx.ErrField("recovery_code", "INVALID", "That recovery code is not valid or has already been used.")
		}

	default:
		secret, err := s.sealer.Open(u.MFASecretEnc, []byte(tenant.Slug))
		if err != nil {
			return nil, httpx.ErrInternal(err)
		}
		if !totp.Validate(code, string(secret)) {
			return nil, httpx.ErrField("code", "INVALID", "That code is not correct.")
		}
	}

	return s.completeLogin(ctx, tenant, u, appctx.Portal(claims.Portal), u.LoginName())
}

// DisableMFA turns off the second factor. It requires the current password so a
// hijacked session cannot silently weaken the account.
func (s *Service) DisableMFA(ctx context.Context, tenant *appctx.Tenant, actor *appctx.Actor, password string) error {
	u, err := s.users.ByIDAnyTenant(ctx, actor.UserID)
	if err != nil {
		return httpx.ErrInternal(err)
	}
	if !u.PasswordHash.Valid {
		return httpx.ErrField("password", "INVALID", "Your password is not correct.")
	}
	match, _, err := s.hasher.Verify(password, u.PasswordHash.String)
	if err != nil {
		return httpx.ErrInternal(err)
	}
	if !match {
		return httpx.ErrField("password", "INVALID", "Your password is not correct.")
	}

	if err := s.users.SetMFAEnabled(ctx, u.ID, false); err != nil {
		return httpx.ErrInternal(err)
	}
	if err := s.users.ClearMFASecret(ctx, u.ID); err != nil {
		return httpx.ErrInternal(err)
	}

	s.auditor.Record(ctx, audit.Entry{
		TenantID: &tenant.ID, ActorID: &u.ID, Action: audit.ActionMFADisabled,
		EntityType: "user", EntityID: &u.ID, EntityPublicID: u.PublicID,
	})
	return nil
}

// RegenerateRecoveryCodes issues a fresh set, invalidating the old ones.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, actor *appctx.Actor) ([]string, error) {
	codes, err := generateRecoveryCodes(10)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}
	hashed := make([]string, len(codes))
	for i, c := range codes {
		hashed[i] = platform.HashToken(c)
	}
	raw, err := json.Marshal(hashed)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}
	if err := s.users.SetRecoveryCodes(ctx, actor.UserID, string(raw)); err != nil {
		return nil, httpx.ErrInternal(err)
	}
	return codes, nil
}

func generateRecoveryCodes(n int) ([]string, error) {
	codes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		part1, err := platform.RandomPassword(5)
		if err != nil {
			return nil, err
		}
		part2, err := platform.RandomPassword(5)
		if err != nil {
			return nil, err
		}
		codes = append(codes, strings.ToLower(part1+"-"+part2))
	}
	return codes, nil
}

func maskMobile(m string) string {
	if len(m) < 4 {
		return "****"
	}
	return strings.Repeat("*", len(m)-4) + m[len(m)-4:]
}

// PasswordExpired reports whether the tenant's expiry policy has elapsed. The
// login flow uses it to force a change without locking the account.
func PasswordExpired(u *user.User) bool {
	return u.PasswordExpiresAt.Valid && u.PasswordExpiresAt.Time.Before(time.Now().UTC())
}
