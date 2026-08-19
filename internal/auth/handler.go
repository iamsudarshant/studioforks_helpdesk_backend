package auth

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
)

type Handler struct {
	svc     *Service
	limiter *middleware.Limiter
	cfg     *config.Config
}

func NewHandler(svc *Service, limiter *middleware.Limiter, cfg *config.Config) *Handler {
	return &Handler{svc: svc, limiter: limiter, cfg: cfg}
}

// Routes mounts the public authentication surface. Each sensitive endpoint gets
// its own rate-limit bucket so throttling login does not also throttle refresh.
func (h *Handler) Routes(r chi.Router) {
	loginLimit := h.cfg.Rate.Rule(h.cfg.Rate.Login)
	otpLimit := h.cfg.Rate.Rule(h.cfg.Rate.OTP)
	forgotLimit := h.cfg.Rate.Rule(h.cfg.Rate.Forgot)

	r.Group(func(r chi.Router) {
		r.Use(h.limiter.PerTenantIP("login", loginLimit))
		r.Post("/login", h.login)
	})

	r.Group(func(r chi.Router) {
		r.Use(h.limiter.PerTenantIP("otp", otpLimit))
		r.Post("/login/otp/request", h.requestOTP)
		r.Post("/login/otp/verify", h.verifyOTP)
		r.Post("/mfa/verify", h.verifyMFA)
	})

	r.Group(func(r chi.Router) {
		r.Use(h.limiter.PerTenantIP("forgot", forgotLimit))
		r.Post("/forgot-username", h.forgotUsername)
		r.Post("/forgot-password", h.forgotPassword)
		r.Post("/reset-password", h.resetPassword)
	})

	r.Post("/refresh", h.refresh)
	r.Get("/reset-password/validate", h.validateResetToken)
}

// AuthenticatedRoutes mounts endpoints that require a session.
func (h *Handler) AuthenticatedRoutes(r chi.Router) {
	r.Get("/me", h.me)
	r.Post("/logout", h.logout)
	r.Post("/logout-all", h.logoutAll)
	r.Post("/change-password", h.changePassword)

	r.Get("/sessions", h.listSessions)
	r.Delete("/sessions/{id}", h.revokeSession)

	r.Post("/mfa/enroll", h.enrolMFA)
	r.Post("/mfa/confirm", h.confirmMFA)
	r.Post("/mfa/disable", h.disableMFA)
	r.Post("/mfa/recovery-codes", h.regenerateRecoveryCodes)
}

// --- requests ---------------------------------------------------------------

type loginRequest struct {
	Identifier string `json:"identifier" validate:"required,notblank,max=191"`
	Password   string `json:"password" validate:"required,min=1,max=128"`
	Portal     string `json:"portal" validate:"required,oneof=admin agents partner user"`
	Remember   bool   `json:"remember"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	tenant := appctx.TenantFrom(r.Context())
	if tenant == nil {
		httpx.Fail(w, r, httpx.New(httpx.CodeTenantNotFound, "No workspace matches this address."))
		return
	}

	result, err := h.svc.Login(r.Context(), tenant, LoginParams{
		Identifier: strings.TrimSpace(req.Identifier),
		Password:   req.Password,
		Portal:     appctx.Portal(req.Portal),
		Remember:   req.Remember,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// A successful sign-in clears the throttle so the user is not punished for
	// having forgotten the password a moment ago.
	h.limiter.Reset(r.Context(), "login:t:"+tenant.Slug+":ip:"+appctx.ClientIP(r.Context()))
	httpx.OK(w, r, result)
}

type otpRequest struct {
	Identifier string `json:"identifier" validate:"required,notblank,max=191"`
	Portal     string `json:"portal" validate:"required,oneof=admin agents partner user"`
}

func (h *Handler) requestOTP(w http.ResponseWriter, r *http.Request) {
	var req otpRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	tenant := appctx.TenantFrom(r.Context())
	if tenant == nil {
		httpx.Fail(w, r, httpx.New(httpx.CodeTenantNotFound, "No workspace matches this address."))
		return
	}

	result, err := h.svc.RequestLoginOTP(r.Context(), tenant,
		strings.TrimSpace(req.Identifier), appctx.Portal(req.Portal))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, result)
}

type otpVerifyRequest struct {
	Identifier string `json:"identifier" validate:"required,notblank,max=191"`
	Code       string `json:"code" validate:"required,len=6,numeric"`
	Portal     string `json:"portal" validate:"required,oneof=admin agents partner user"`
}

func (h *Handler) verifyOTP(w http.ResponseWriter, r *http.Request) {
	var req otpVerifyRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	tenant := appctx.TenantFrom(r.Context())
	if tenant == nil {
		httpx.Fail(w, r, httpx.New(httpx.CodeTenantNotFound, "No workspace matches this address."))
		return
	}

	result, err := h.svc.VerifyLoginOTP(r.Context(), tenant,
		strings.TrimSpace(req.Identifier), req.Code, appctx.Portal(req.Portal))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, result)
}

type mfaVerifyRequest struct {
	MFAToken     string `json:"mfa_token" validate:"required"`
	Code         string `json:"code" validate:"omitempty,len=6,numeric"`
	RecoveryCode string `json:"recovery_code" validate:"omitempty,max=32"`
}

func (h *Handler) verifyMFA(w http.ResponseWriter, r *http.Request) {
	var req mfaVerifyRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if req.Code == "" && req.RecoveryCode == "" {
		httpx.Fail(w, r, httpx.ErrField("code", "REQUIRED",
			"Enter the code from your authenticator app, or a recovery code."))
		return
	}

	tenant := appctx.TenantFrom(r.Context())
	if tenant == nil {
		httpx.Fail(w, r, httpx.New(httpx.CodeTenantNotFound, "No workspace matches this address."))
		return
	}

	result, err := h.svc.VerifyMFA(r.Context(), tenant, req.MFAToken, req.Code, req.RecoveryCode)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, result)
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required"`
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	result, err := h.svc.Refresh(r.Context(), appctx.TenantFrom(r.Context()), req.RefreshToken)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, result)
}

type forgotRequest struct {
	Identifier     string `json:"identifier" validate:"required,notblank,max=191"`
	IdentifierType string `json:"identifier_type" validate:"omitempty,oneof=email alt_email employee_code pf_number uan_number pan_number mobile"`
	Portal         string `json:"portal" validate:"required,oneof=admin agents partner user"`
}

func (h *Handler) forgotUsername(w http.ResponseWriter, r *http.Request) {
	var req forgotRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	tenant := appctx.TenantFrom(r.Context())
	if tenant == nil {
		httpx.Fail(w, r, httpx.New(httpx.CodeTenantNotFound, "No workspace matches this address."))
		return
	}

	result, err := h.svc.ForgotUsername(r.Context(), tenant, ForgotParams{
		Identifier:     strings.TrimSpace(req.Identifier),
		IdentifierType: req.IdentifierType,
		Portal:         appctx.Portal(req.Portal),
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, result)
}

func (h *Handler) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var req forgotRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	tenant := appctx.TenantFrom(r.Context())
	if tenant == nil {
		httpx.Fail(w, r, httpx.New(httpx.CodeTenantNotFound, "No workspace matches this address."))
		return
	}

	result, err := h.svc.ForgotPassword(r.Context(), tenant, ForgotParams{
		Identifier:     strings.TrimSpace(req.Identifier),
		IdentifierType: req.IdentifierType,
		Portal:         appctx.Portal(req.Portal),
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, result)
}

func (h *Handler) validateResetToken(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if token == "" {
		httpx.Fail(w, r, httpx.ErrField("token", "REQUIRED", "A reset token is required."))
		return
	}

	info, err := h.svc.ValidateResetToken(r.Context(), token)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, info)
}

type resetPasswordRequest struct {
	Token           string `json:"token" validate:"required"`
	NewPassword     string `json:"new_password" validate:"required,min=8,max=128"`
	ConfirmPassword string `json:"confirm_password" validate:"required,eqfield=NewPassword"`
}

func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetPasswordRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.svc.ResetPassword(r.Context(), ResetPasswordParams{
		Token:           req.Token,
		NewPassword:     req.NewPassword,
		ConfirmPassword: req.ConfirmPassword,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]any{
		"message": "Your password has been changed. Sign in with your new password.",
	})
}

// --- authenticated ----------------------------------------------------------

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := appctx.ActorFrom(ctx)
	tenant := appctx.TenantFrom(ctx)
	if actor == nil || tenant == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	me, err := h.svc.Me(ctx, tenant, actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, me)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	actor := appctx.ActorFrom(r.Context())
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}
	if err := h.svc.Logout(r.Context(), actor.SessionID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]any{"message": "Signed out."})
}

func (h *Handler) logoutAll(w http.ResponseWriter, r *http.Request) {
	actor := appctx.ActorFrom(r.Context())
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}
	if err := h.svc.LogoutAll(r.Context(), actor.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]any{"message": "Signed out of all devices."})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password" validate:"required,max=128"`
	NewPassword     string `json:"new_password" validate:"required,min=8,max=128"`
	ConfirmPassword string `json:"confirm_password" validate:"required,eqfield=NewPassword"`
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	actor := appctx.ActorFrom(r.Context())
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	if err := h.svc.ChangePassword(r.Context(), actor, ChangePasswordParams{
		CurrentPassword: req.CurrentPassword,
		NewPassword:     req.NewPassword,
		ConfirmPassword: req.ConfirmPassword,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]any{"message": "Your password has been changed."})
}

type sessionResponse struct {
	ID         string `json:"id"`
	Portal     string `json:"portal"`
	IP         string `json:"ip"`
	UserAgent  string `json:"user_agent"`
	IssuedAt   string `json:"issued_at"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
	ExpiresAt  string `json:"expires_at"`
	Current    bool   `json:"current"`
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	actor := appctx.ActorFrom(r.Context())
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	rows, err := h.svc.Sessions(r.Context(), actor.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	out := make([]sessionResponse, 0, len(rows))
	for _, s := range rows {
		item := sessionResponse{
			ID:        s.PublicID,
			Portal:    s.Portal,
			IP:        s.IP.String,
			UserAgent: s.UserAgent.String,
			IssuedAt:  s.IssuedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			ExpiresAt: s.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			Current:   s.ID == actor.SessionID,
		}
		if s.LastSeenAt.Valid {
			item.LastSeenAt = s.LastSeenAt.Time.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		out = append(out, item)
	}
	httpx.OK(w, r, out)
}

func (h *Handler) revokeSession(w http.ResponseWriter, r *http.Request) {
	actor := appctx.ActorFrom(r.Context())
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}
	if err := h.svc.RevokeSession(r.Context(), actor.UserID, chi.URLParam(r, "id")); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]any{"message": "Session ended."})
}

func (h *Handler) enrolMFA(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, tenant := appctx.ActorFrom(ctx), appctx.TenantFrom(ctx)
	if actor == nil || tenant == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	enrolment, err := h.svc.EnrolMFA(ctx, tenant, actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, enrolment)
}

type mfaCodeRequest struct {
	Code string `json:"code" validate:"required,len=6,numeric"`
}

func (h *Handler) confirmMFA(w http.ResponseWriter, r *http.Request) {
	var req mfaCodeRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	actor, tenant := appctx.ActorFrom(ctx), appctx.TenantFrom(ctx)
	if actor == nil || tenant == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	if err := h.svc.ConfirmMFA(ctx, tenant, actor, req.Code); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]any{"message": "Multi-factor authentication is now active."})
}

type mfaDisableRequest struct {
	Password string `json:"password" validate:"required,max=128"`
}

func (h *Handler) disableMFA(w http.ResponseWriter, r *http.Request) {
	var req mfaDisableRequest
	if err := httpx.Bind(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	actor, tenant := appctx.ActorFrom(ctx), appctx.TenantFrom(ctx)
	if actor == nil || tenant == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	if err := h.svc.DisableMFA(ctx, tenant, actor, req.Password); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]any{"message": "Multi-factor authentication has been turned off."})
}

func (h *Handler) regenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	actor := appctx.ActorFrom(r.Context())
	if actor == nil {
		httpx.Fail(w, r, httpx.ErrUnauthenticated(""))
		return
	}

	codes, err := h.svc.RegenerateRecoveryCodes(r.Context(), actor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]any{
		"recovery_codes": codes,
		"message":        "Store these codes somewhere safe. They will not be shown again.",
	})
}
