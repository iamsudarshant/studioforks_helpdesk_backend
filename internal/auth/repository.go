package auth

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

type Repository struct {
	db *platform.DB
}

func NewRepository(db *platform.DB) *Repository { return &Repository{db: db} }

// --- sessions ---------------------------------------------------------------

type Session struct {
	ID                int64          `db:"id"`
	PublicID          string         `db:"public_id"`
	TenantID          int64          `db:"tenant_id"`
	UserID            int64          `db:"user_id"`
	RefreshTokenHash  string         `db:"refresh_token_hash"`
	FamilyID          string         `db:"family_id"`
	Portal            string         `db:"portal"`
	IP                sql.NullString `db:"ip"`
	UserAgent         sql.NullString `db:"user_agent"`
	DeviceFingerprint sql.NullString `db:"device_fingerprint"`
	IssuedAt          time.Time      `db:"issued_at"`
	ExpiresAt         time.Time      `db:"expires_at"`
	LastSeenAt        sql.NullTime   `db:"last_seen_at"`
	RevokedAt         sql.NullTime   `db:"revoked_at"`
	RevokedReason     sql.NullString `db:"revoked_reason"`
}

const sessionColumns = `id, public_id, tenant_id, user_id, refresh_token_hash, family_id, portal,
	ip, user_agent, device_fingerprint, issued_at, expires_at, last_seen_at, revoked_at, revoked_reason`

type CreateSessionParams struct {
	TenantID  int64
	UserID    int64
	TokenHash string
	FamilyID  string
	Portal    string
	IP        string
	UserAgent string
	ExpiresAt time.Time
}

func (r *Repository) CreateSession(ctx context.Context, p CreateSessionParams) (*Session, error) {
	publicID := platform.NewULID()
	familyID := p.FamilyID
	if familyID == "" {
		familyID = platform.NewULID()
	}

	res, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO sessions
			(public_id, tenant_id, user_id, refresh_token_hash, family_id, portal,
			 ip, user_agent, expires_at, last_seen_at)
		VALUES (?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3))`,
		publicID, p.TenantID, p.UserID, p.TokenHash, familyID, p.Portal,
		nullIfBlank(p.IP), nullIfBlank(p.UserAgent), p.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("creating session: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading session id: %w", err)
	}
	return r.SessionByID(ctx, id)
}

func (r *Repository) SessionByID(ctx context.Context, id int64) (*Session, error) {
	var s Session
	err := r.db.Primary.GetContext(ctx, &s,
		`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading session: %w", err)
	}
	return &s, nil
}

// SessionByTokenHash finds a session by refresh-token hash regardless of its
// revocation state, because a revoked match is exactly what reuse detection
// needs to see.
func (r *Repository) SessionByTokenHash(ctx context.Context, hash string) (*Session, error) {
	var s Session
	err := r.db.Primary.GetContext(ctx, &s,
		`SELECT `+sessionColumns+` FROM sessions WHERE refresh_token_hash = ?`, hash)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading session by token: %w", err)
	}
	return &s, nil
}

// RotateSession revokes the presented token and issues its successor inside one
// transaction, keeping the family id so reuse can still be traced.
func (r *Repository) RotateSession(ctx context.Context, old *Session, newHash string, expiresAt time.Time, ip, ua string) (*Session, error) {
	var created *Session

	err := r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions SET revoked_at = UTC_TIMESTAMP(3), revoked_reason = 'ROTATED'
			WHERE id = ? AND revoked_at IS NULL`, old.ID); err != nil {
			return fmt.Errorf("revoking rotated session: %w", err)
		}

		publicID := platform.NewULID()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO sessions
				(public_id, tenant_id, user_id, refresh_token_hash, family_id, portal,
				 ip, user_agent, expires_at, last_seen_at)
			VALUES (?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3))`,
			publicID, old.TenantID, old.UserID, newHash, old.FamilyID, old.Portal,
			nullIfBlank(ip), nullIfBlank(ua), expiresAt)
		if err != nil {
			return fmt.Errorf("creating rotated session: %w", err)
		}

		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("reading rotated session id: %w", err)
		}

		var s Session
		if err := tx.GetContext(ctx, &s, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id); err != nil {
			return fmt.Errorf("reloading rotated session: %w", err)
		}
		created = &s
		return nil
	})

	return created, err
}

// RevokeSession ends one session.
func (r *Repository) RevokeSession(ctx context.Context, id int64, reason string) error {
	_, err := r.db.Primary.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = UTC_TIMESTAMP(3), revoked_reason = ?
		WHERE id = ? AND revoked_at IS NULL`, reason, id)
	if err != nil {
		return fmt.Errorf("revoking session: %w", err)
	}
	return nil
}

// RevokeFamily kills every session descended from the same login. This is the
// response to refresh-token reuse: the legitimate user and the thief both lose
// the session, which is the correct outcome.
func (r *Repository) RevokeFamily(ctx context.Context, familyID, reason string) error {
	_, err := r.db.Primary.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = UTC_TIMESTAMP(3), revoked_reason = ?
		WHERE family_id = ? AND revoked_at IS NULL`, reason, familyID)
	if err != nil {
		return fmt.Errorf("revoking session family: %w", err)
	}
	return nil
}

func (r *Repository) RevokeAllForUser(ctx context.Context, userID int64, reason string) error {
	_, err := r.db.Primary.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = UTC_TIMESTAMP(3), revoked_reason = ?
		WHERE user_id = ? AND revoked_at IS NULL`, reason, userID)
	if err != nil {
		return fmt.Errorf("revoking user sessions: %w", err)
	}
	return nil
}

func (r *Repository) ActiveSessions(ctx context.Context, userID int64) ([]Session, error) {
	rows := []Session{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT `+sessionColumns+` FROM sessions
		WHERE user_id = ? AND revoked_at IS NULL AND expires_at > UTC_TIMESTAMP(3)
		ORDER BY issued_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	return rows, nil
}

// EnforceSessionLimit revokes the oldest sessions beyond the configured cap.
func (r *Repository) EnforceSessionLimit(ctx context.Context, userID int64, maxSessions int) error {
	if maxSessions <= 0 {
		return nil
	}
	_, err := r.db.Primary.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = UTC_TIMESTAMP(3), revoked_reason = 'SESSION_LIMIT'
		WHERE id IN (
			SELECT id FROM (
				SELECT id FROM sessions
				WHERE user_id = ? AND revoked_at IS NULL AND expires_at > UTC_TIMESTAMP(3)
				ORDER BY issued_at DESC
				LIMIT 18446744073709551615 OFFSET ?
			) old
		)`, userID, maxSessions)
	if err != nil {
		return fmt.Errorf("enforcing session limit: %w", err)
	}
	return nil
}

func (r *Repository) TouchSession(ctx context.Context, id int64) {
	_, _ = r.db.Primary.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = UTC_TIMESTAMP(3) WHERE id = ?`, id)
}

// --- reset tokens -----------------------------------------------------------

const (
	TokenTypeResetLink    = "RESET_LINK"
	TokenTypeTempPassword = "TEMP_PASSWORD"
	TokenTypeActivation   = "ACTIVATION"
)

type ResetToken struct {
	ID          int64          `db:"id"`
	TenantID    int64          `db:"tenant_id"`
	UserID      int64          `db:"user_id"`
	TokenHash   string         `db:"token_hash"`
	TokenType   string         `db:"token_type"`
	Channel     sql.NullString `db:"channel"`
	SentTo      sql.NullString `db:"sent_to"`
	RequestedBy sql.NullInt64  `db:"requested_by"`
	IP          sql.NullString `db:"ip"`
	ExpiresAt   time.Time      `db:"expires_at"`
	UsedAt      sql.NullTime   `db:"used_at"`
	CreatedAt   time.Time      `db:"created_at"`
}

// CreateResetToken invalidates any outstanding tokens of the same type before
// issuing a new one, so an older link in an inbox stops working immediately.
func (r *Repository) CreateResetToken(ctx context.Context, tenantID, userID int64, hash, tokenType, channel, sentTo string, requestedBy *int64, ip string, ttl time.Duration) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE password_reset_tokens SET used_at = UTC_TIMESTAMP(3)
			WHERE user_id = ? AND token_type = ? AND used_at IS NULL`, userID, tokenType); err != nil {
			return fmt.Errorf("invalidating previous tokens: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO password_reset_tokens
				(tenant_id, user_id, token_hash, token_type, channel, sent_to, requested_by, ip, expires_at)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			tenantID, userID, hash, tokenType, nullIfBlank(channel), nullIfBlank(sentTo),
			requestedBy, nullIfBlank(ip), time.Now().UTC().Add(ttl)); err != nil {
			return fmt.Errorf("creating reset token: %w", err)
		}
		return nil
	})
}

func (r *Repository) ResetTokenByHash(ctx context.Context, hash string) (*ResetToken, error) {
	var t ResetToken
	err := r.db.Primary.GetContext(ctx, &t, `
		SELECT id, tenant_id, user_id, token_hash, token_type, channel, sent_to,
		       requested_by, ip, expires_at, used_at, created_at
		FROM password_reset_tokens
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > UTC_TIMESTAMP(3)`, hash)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading reset token: %w", err)
	}
	return &t, nil
}

func (r *Repository) ConsumeResetToken(ctx context.Context, id int64) error {
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE password_reset_tokens SET used_at = UTC_TIMESTAMP(3) WHERE id = ? AND used_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("consuming reset token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading affected rows: %w", err)
	}
	if n == 0 {
		// Someone already used it — treat as invalid rather than succeeding twice.
		return platform.ErrSentinelConflict
	}
	return nil
}

// --- OTP --------------------------------------------------------------------

const (
	OTPPurposeLogin        = "LOGIN"
	OTPPurposeMFA          = "MFA"
	OTPPurposeVerifyMobile = "VERIFY_MOBILE"
)

type OTP struct {
	ID          int64          `db:"id"`
	TenantID    int64          `db:"tenant_id"`
	UserID      int64          `db:"user_id"`
	Purpose     string         `db:"purpose"`
	CodeHash    string         `db:"code_hash"`
	Destination sql.NullString `db:"destination"`
	Attempts    int            `db:"attempts"`
	ExpiresAt   time.Time      `db:"expires_at"`
	ConsumedAt  sql.NullTime   `db:"consumed_at"`
	CreatedAt   time.Time      `db:"created_at"`
}

func (r *Repository) CreateOTP(ctx context.Context, tenantID, userID int64, purpose, codeHash, destination string, ttl time.Duration) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE otp_codes SET consumed_at = UTC_TIMESTAMP(3)
			WHERE user_id = ? AND purpose = ? AND consumed_at IS NULL`, userID, purpose); err != nil {
			return fmt.Errorf("invalidating previous codes: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO otp_codes (tenant_id, user_id, purpose, code_hash, destination, expires_at)
			VALUES (?,?,?,?,?,?)`,
			tenantID, userID, purpose, codeHash, nullIfBlank(destination),
			time.Now().UTC().Add(ttl)); err != nil {
			return fmt.Errorf("creating otp: %w", err)
		}
		return nil
	})
}

func (r *Repository) ActiveOTP(ctx context.Context, userID int64, purpose string) (*OTP, error) {
	var o OTP
	err := r.db.Primary.GetContext(ctx, &o, `
		SELECT id, tenant_id, user_id, purpose, code_hash, destination, attempts,
		       expires_at, consumed_at, created_at
		FROM otp_codes
		WHERE user_id = ? AND purpose = ? AND consumed_at IS NULL AND expires_at > UTC_TIMESTAMP(3)
		ORDER BY created_at DESC LIMIT 1`, userID, purpose)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading otp: %w", err)
	}
	return &o, nil
}

func (r *Repository) IncrementOTPAttempts(ctx context.Context, id int64) error {
	_, err := r.db.Primary.ExecContext(ctx,
		`UPDATE otp_codes SET attempts = attempts + 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("incrementing otp attempts: %w", err)
	}
	return nil
}

func (r *Repository) ConsumeOTP(ctx context.Context, id int64) error {
	_, err := r.db.Primary.ExecContext(ctx,
		`UPDATE otp_codes SET consumed_at = UTC_TIMESTAMP(3) WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("consuming otp: %w", err)
	}
	return nil
}

// --- API keys ---------------------------------------------------------------

type APIKey struct {
	ID         int64          `db:"id"`
	PublicID   string         `db:"public_id"`
	TenantID   int64          `db:"tenant_id"`
	Name       string         `db:"name"`
	KeyPrefix  string         `db:"key_prefix"`
	KeyHash    string         `db:"key_hash"`
	ScopesJSON sql.NullString `db:"scopes_json"`
	CreatedBy  sql.NullInt64  `db:"created_by"`
	LastUsedAt sql.NullTime   `db:"last_used_at"`
	ExpiresAt  sql.NullTime   `db:"expires_at"`
	RevokedAt  sql.NullTime   `db:"revoked_at"`
	CreatedAt  time.Time      `db:"created_at"`
}

func (r *Repository) APIKeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	var k APIKey
	err := r.db.Primary.GetContext(ctx, &k, `
		SELECT id, public_id, tenant_id, name, key_prefix, key_hash, scopes_json,
		       created_by, last_used_at, expires_at, revoked_at, created_at
		FROM api_keys
		WHERE key_hash = ? AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > UTC_TIMESTAMP(3))`, hash)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading api key: %w", err)
	}
	return &k, nil
}

func (r *Repository) TouchAPIKey(ctx context.Context, id int64) {
	_, _ = r.db.Primary.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = UTC_TIMESTAMP(3) WHERE id = ?`, id)
}

func nullIfBlank(s string) any {
	if s == "" {
		return nil
	}
	return s
}
