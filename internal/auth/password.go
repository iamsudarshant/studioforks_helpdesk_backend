// Package auth implements authentication: password hashing and policy, tokens,
// sessions, OTP/MFA, and the account-recovery flows.
package auth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/crypto/argon2"

	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// Hasher produces and verifies Argon2id password hashes in the PHC string
// format, so parameters can be raised later without invalidating old hashes.
type Hasher struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	saltLength  uint32
	keyLength   uint32
}

func NewHasher(cfg config.Auth) *Hasher {
	return &Hasher{
		memory:      cfg.ArgonMemory,
		iterations:  cfg.ArgonIterations,
		parallelism: cfg.ArgonParallelism,
		saltLength:  cfg.ArgonSaltLength,
		keyLength:   cfg.ArgonKeyLength,
	}
}

func (h *Hasher) Hash(password string) (string, error) {
	salt, err := platform.RandomBytes(int(h.saltLength))
	if err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, h.iterations, h.memory, h.parallelism, h.keyLength)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.memory, h.iterations, h.parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify reports whether password matches the encoded hash. It also reports
// whether the hash used weaker parameters than the current configuration, so
// callers can transparently re-hash on a successful login.
func (h *Hasher) Verify(password, encoded string) (match bool, needsRehash bool, err error) {
	params, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, false, err
	}

	candidate := argon2.IDKey([]byte(password), salt,
		params.iterations, params.memory, params.parallelism, uint32(len(key)))

	if subtle.ConstantTimeCompare(key, candidate) != 1 {
		return false, false, nil
	}

	outdated := params.memory < h.memory ||
		params.iterations < h.iterations ||
		uint32(len(key)) < h.keyLength
	return true, outdated, nil
}

type hashParams struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
}

var errInvalidHash = errors.New("password hash is not in the expected format")

func decodeHash(encoded string) (hashParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return hashParams{}, nil, nil, errInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return hashParams{}, nil, nil, errInvalidHash
	}
	if version != argon2.Version {
		return hashParams{}, nil, nil, fmt.Errorf("unsupported argon2 version %d", version)
	}

	var p hashParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.iterations, &p.parallelism); err != nil {
		return hashParams{}, nil, nil, errInvalidHash
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return hashParams{}, nil, nil, errInvalidHash
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return hashParams{}, nil, nil, errInvalidHash
	}
	return p, salt, key, nil
}

// --- password policy --------------------------------------------------------

// Policy is the per-tenant password policy, editable from the admin panel.
type Policy struct {
	MinLength      int  `json:"min_length"`
	RequireUpper   bool `json:"require_upper"`
	RequireLower   bool `json:"require_lower"`
	RequireNumber  bool `json:"require_number"`
	RequireSymbol  bool `json:"require_symbol"`
	HistoryCount   int  `json:"history_count"`
	ExpiryDays     int  `json:"expiry_days"`
	MaxFailed      int  `json:"max_failed_attempts"`
	LockoutMinutes int  `json:"lockout_minutes"`
}

func DefaultPolicy() Policy {
	return Policy{
		MinLength:      12,
		RequireUpper:   true,
		RequireLower:   true,
		RequireNumber:  true,
		RequireSymbol:  false,
		HistoryCount:   5,
		ExpiryDays:     90,
		MaxFailed:      5,
		LockoutMinutes: 15,
	}
}

// commonPasswords are rejected outright regardless of whether they satisfy the
// composition rules. Kept small and local; extend from a file in deployment.
var commonPasswords = map[string]struct{}{
	"password": {}, "password1": {}, "password123": {}, "passw0rd": {},
	"qwerty": {}, "qwerty123": {}, "12345678": {}, "123456789": {},
	"letmein": {}, "welcome": {}, "welcome123": {}, "admin": {}, "admin123": {},
	"iloveyou": {}, "abc123": {}, "monkey": {}, "dragon": {}, "sunshine": {},
	"complydesk": {}, "complydesk123": {}, "helpdesk": {}, "helpdesk123": {},
	"changeme": {}, "temp1234": {}, "india@123": {}, "pass@123": {},
}

// Validate checks a candidate password against the policy and returns a
// validation error listing every rule it breaks, so the UI can show all at once.
func (p Policy) Validate(password string, personal ...string) error {
	var details []httpx.FieldError

	add := func(code, msg string) {
		details = append(details, httpx.FieldError{Field: "new_password", Code: code, Message: msg})
	}

	if len([]rune(password)) < p.MinLength {
		add("TOO_SHORT", fmt.Sprintf("Password must be at least %d characters.", p.MinLength))
	}
	if len(password) > 128 {
		add("TOO_LONG", "Password must be 128 characters or fewer.")
	}

	var hasUpper, hasLower, hasNumber, hasSymbol bool
	for _, r := range password {
		switch {
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsDigit(r):
			hasNumber = true
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			hasSymbol = true
		}
	}
	if p.RequireUpper && !hasUpper {
		add("NEEDS_UPPER", "Password must include an uppercase letter.")
	}
	if p.RequireLower && !hasLower {
		add("NEEDS_LOWER", "Password must include a lowercase letter.")
	}
	if p.RequireNumber && !hasNumber {
		add("NEEDS_NUMBER", "Password must include a number.")
	}
	if p.RequireSymbol && !hasSymbol {
		add("NEEDS_SYMBOL", "Password must include a symbol.")
	}

	lower := strings.ToLower(password)
	if _, common := commonPasswords[lower]; common {
		add("TOO_COMMON", "This password is too common. Choose something less predictable.")
	}

	// Reject passwords built from the user's own identifiers — the exact
	// weakness that rules out PFNumber@BirthYear as a default temp password.
	for _, hint := range personal {
		hint = strings.ToLower(strings.TrimSpace(hint))
		if len(hint) < 4 {
			continue
		}
		if strings.Contains(lower, hint) {
			add("CONTAINS_PERSONAL_DATA",
				"Password must not contain your name, email, employee code or statutory numbers.")
			break
		}
	}

	if len(details) > 0 {
		return httpx.ErrValidation(details...)
	}
	return nil
}

// CheckHistory rejects reuse of a recent password.
func (h *Hasher) CheckHistory(password string, previousHashes []string) error {
	for _, prev := range previousHashes {
		match, _, err := h.Verify(password, prev)
		if err != nil {
			continue // a malformed legacy hash must not block a password change
		}
		if match {
			return httpx.ErrField("new_password", "REUSED",
				"You have used this password recently. Choose a different one.")
		}
	}
	return nil
}

// HashPassword exposes the service's hasher to packages that cannot import this
// one.
//
// The user package needs it for bulk import, but auth already imports user, so
// the dependency has to run the other way — through an interface user declares
// and this satisfies. Sharing the hasher rather than constructing a second one
// guarantees imported passwords verify with exactly the parameters login uses.
func (s *Service) HashPassword(password string) (string, error) {
	return s.hasher.Hash(password)
}

// PasswordAgeing exposes the two numbers a caller needs to store a password
// under the client's policy: how long before it expires, and how many previous
// ones to keep for the reuse check.
//
// Two ints rather than the Policy itself, because the user package cannot see
// this one — auth imports it, so the dependency has to stay one-way.
func (s *Service) PasswordAgeing(ctx context.Context, tenantID int64) (expiryDays, historyCount int) {
	p := s.tenants.PasswordPolicy(ctx, tenantID)
	return p.ExpiryDays, p.HistoryCount
}
