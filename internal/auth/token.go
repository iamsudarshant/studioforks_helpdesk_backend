package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// Claims is the access-token payload. It carries enough to authorise a request
// without a database round trip for the common case, plus hashes of the
// permission and scope sets so a revoked permission invalidates the token early.
type Claims struct {
	jwt.RegisteredClaims
	TenantID           int64    `json:"tid"`
	TenantSlug         string   `json:"tsl"`
	Portal             string   `json:"portal"`
	SessionID          int64    `json:"sid"`
	Roles              []string `json:"roles"`
	PermsHash          string   `json:"ph"`
	ScopeHash          string   `json:"sh"`
	MustChangePassword bool     `json:"mcp,omitempty"`
	SuperAdmin         bool     `json:"sa,omitempty"`
}

// TokenService issues and verifies access tokens, plus the short-lived
// intermediate tokens used by the MFA and OTP steps.
type TokenService struct {
	secret []byte
	issuer string
	cfg    config.Auth
}

func NewTokenService(cfg config.Auth) *TokenService {
	return &TokenService{secret: []byte(cfg.JWTSecret), issuer: cfg.JWTIssuer, cfg: cfg}
}

// IssueAccess mints an access token for an authenticated actor.
func (s *TokenService) IssueAccess(actor *appctx.Actor, tenantSlug string, permsHash, scopeHash string) (string, time.Time, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(s.cfg.AccessTokenTTL)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   actor.PublicID,
			Issuer:    s.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        platform.NewULID(),
		},
		TenantID:           actor.TenantID,
		TenantSlug:         tenantSlug,
		Portal:             string(actor.Portal),
		SessionID:          actor.SessionID,
		Roles:              actor.Roles,
		PermsHash:          permsHash,
		ScopeHash:          scopeHash,
		MustChangePassword: actor.MustChangePassword,
		SuperAdmin:         actor.IsSuperAdmin,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("signing access token: %w", err)
	}
	return signed, expiresAt, nil
}

// ParseAccess validates the signature and standard claims. An expired token
// returns TOKEN_EXPIRED so the client knows to attempt a silent refresh rather
// than dropping the user at the login screen.
func (s *TokenService) ParseAccess(raw string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return s.secret, nil
	},
		jwt.WithIssuer(s.issuer),
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired(),
	)

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, httpx.New(httpx.CodeTokenExpired, "Your session has expired.")
		}
		return nil, httpx.ErrUnauthenticated("Your session is no longer valid. Sign in again.")
	}
	return claims, nil
}

// --- intermediate step tokens ----------------------------------------------

// StepClaims backs the short-lived token handed to the client between the
// password step and the MFA/OTP step. It authorises nothing else.
type StepClaims struct {
	jwt.RegisteredClaims
	UserID   int64  `json:"uid"`
	TenantID int64  `json:"tid"`
	Portal   string `json:"portal"`
	Purpose  string `json:"purpose"` // MFA | OTP
}

func (s *TokenService) IssueStepToken(userID, tenantID int64, portal, purpose string) (string, error) {
	now := time.Now().UTC()
	claims := StepClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
			ID:        platform.NewULID(),
		},
		UserID:   userID,
		TenantID: tenantID,
		Portal:   portal,
		Purpose:  purpose,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("signing step token: %w", err)
	}
	return signed, nil
}

func (s *TokenService) ParseStepToken(raw, purpose string) (*StepClaims, error) {
	claims := &StepClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		return s.secret, nil
	}, jwt.WithIssuer(s.issuer), jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())

	if err != nil {
		return nil, httpx.ErrUnauthenticated("This verification step has expired. Start again.")
	}
	if claims.Purpose != purpose {
		return nil, httpx.ErrUnauthenticated("This verification step is not valid here.")
	}
	return claims, nil
}

// NewRefreshToken returns the opaque refresh token and the hash to persist. The
// plaintext is returned to the client once and never stored.
func NewRefreshToken() (token, hash string, err error) {
	token, err = platform.RandomToken(32)
	if err != nil {
		return "", "", err
	}
	return token, platform.HashToken(token), nil
}
