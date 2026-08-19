package user

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/karmamgmt/complydesk/internal/platform"
)

type Preferences struct {
	UserID     int64          `db:"user_id"`
	Theme      string         `db:"theme"`
	Density    string         `db:"density"`
	Language   string         `db:"language"`
	ExtrasJSON sql.NullString `db:"extras_json"`
	UpdatedAt  time.Time      `db:"updated_at"`
}

func (r *Repository) Preferences(ctx context.Context, userID int64) (*Preferences, error) {
	var p Preferences
	err := r.db.Primary.GetContext(ctx, &p,
		`SELECT user_id, theme, density, language, extras_json, updated_at
		 FROM user_preferences WHERE user_id = ?`, userID)
	if err != nil {
		if platform.IsNotFound(err) {
			// A user who has never changed a preference still needs defaults.
			return &Preferences{UserID: userID, Theme: "system", Density: "comfortable", Language: "en"}, nil
		}
		return nil, fmt.Errorf("loading preferences: %w", err)
	}
	return &p, nil
}

type PreferencesUpdate struct {
	Theme    *string
	Density  *string
	Language *string
	Extras   *string
}

func (r *Repository) SetPreferences(ctx context.Context, userID int64, u PreferencesUpdate) error {
	current, err := r.Preferences(ctx, userID)
	if err != nil {
		return err
	}

	if u.Theme != nil {
		current.Theme = *u.Theme
	}
	if u.Density != nil {
		current.Density = *u.Density
	}
	if u.Language != nil {
		current.Language = *u.Language
	}
	extras := current.ExtrasJSON
	if u.Extras != nil {
		extras = sql.NullString{String: *u.Extras, Valid: *u.Extras != ""}
	}

	_, err = r.db.Primary.ExecContext(ctx, `
		INSERT INTO user_preferences (user_id, theme, density, language, extras_json)
		VALUES (?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			theme = VALUES(theme), density = VALUES(density),
			language = VALUES(language), extras_json = VALUES(extras_json)`,
		userID, current.Theme, current.Density, current.Language, extras)
	if err != nil {
		return fmt.Errorf("saving preferences: %w", err)
	}
	return nil
}
