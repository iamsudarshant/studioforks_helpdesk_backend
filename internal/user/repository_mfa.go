package user

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

// StageMFASecret stores a sealed TOTP secret and hashed recovery codes without
// activating the second factor. Activation happens only after the user proves
// they can generate a valid code.
func (r *Repository) StageMFASecret(ctx context.Context, userID int64, sealedSecret []byte, recoveryJSON string) error {
	_, err := r.db.Primary.ExecContext(ctx,
		`UPDATE users SET mfa_secret_enc = ?, mfa_recovery_json = ?, mfa_enabled = 0 WHERE id = ?`,
		sealedSecret, recoveryJSON, userID)
	if err != nil {
		return fmt.Errorf("staging mfa secret: %w", err)
	}
	return nil
}

func (r *Repository) SetMFAEnabled(ctx context.Context, userID int64, enabled bool) error {
	_, err := r.db.Primary.ExecContext(ctx,
		`UPDATE users SET mfa_enabled = ? WHERE id = ?`, enabled, userID)
	if err != nil {
		return fmt.Errorf("setting mfa state: %w", err)
	}
	return nil
}

func (r *Repository) ClearMFASecret(ctx context.Context, userID int64) error {
	_, err := r.db.Primary.ExecContext(ctx,
		`UPDATE users SET mfa_secret_enc = NULL, mfa_recovery_json = NULL WHERE id = ?`, userID)
	if err != nil {
		return fmt.Errorf("clearing mfa secret: %w", err)
	}
	return nil
}

func (r *Repository) SetRecoveryCodes(ctx context.Context, userID int64, recoveryJSON string) error {
	_, err := r.db.Primary.ExecContext(ctx,
		`UPDATE users SET mfa_recovery_json = ? WHERE id = ?`, recoveryJSON, userID)
	if err != nil {
		return fmt.Errorf("setting recovery codes: %w", err)
	}
	return nil
}

// ConsumeRecoveryCode removes a used code under a row lock, so the same code
// cannot be redeemed twice by concurrent requests.
func (r *Repository) ConsumeRecoveryCode(ctx context.Context, userID int64, codeHash string) (bool, error) {
	var consumed bool

	err := r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		var raw *string
		if err := tx.GetContext(ctx, &raw,
			`SELECT mfa_recovery_json FROM users WHERE id = ? FOR UPDATE`, userID); err != nil {
			return fmt.Errorf("loading recovery codes: %w", err)
		}
		if raw == nil || *raw == "" {
			return nil
		}

		var hashes []string
		if err := json.Unmarshal([]byte(*raw), &hashes); err != nil {
			return fmt.Errorf("decoding recovery codes: %w", err)
		}

		remaining := make([]string, 0, len(hashes))
		for _, h := range hashes {
			if !consumed && platform.ConstantTimeEqual(h, codeHash) {
				consumed = true
				continue
			}
			remaining = append(remaining, h)
		}
		if !consumed {
			return nil
		}

		updated, err := json.Marshal(remaining)
		if err != nil {
			return fmt.Errorf("encoding recovery codes: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET mfa_recovery_json = ? WHERE id = ?`, string(updated), userID); err != nil {
			return fmt.Errorf("updating recovery codes: %w", err)
		}
		return nil
	})

	return consumed, err
}
