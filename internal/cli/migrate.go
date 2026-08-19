// Package cli implements the operational commands exposed by cmd/cli.
package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	migratemysql "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/karmamgmt/complydesk/internal/config"
)

// ErrUsage signals the caller printed nothing useful and usage should be shown.
var ErrUsage = errors.New("invalid usage")

const migrationsPath = "file://db/migrations"

// CreateDatabase creates the configured schema if it does not exist. Kept
// separate from migrate so that migrations never hold DDL privileges they do
// not need.
func CreateDatabase(ctx context.Context, cfg *config.Config) error {
	db, err := sql.Open("mysql", cfg.DB.AdminDSN())
	if err != nil {
		return fmt.Errorf("connecting to server: %w", err)
	}
	defer func() { _ = db.Close() }()

	// The database name comes from configuration, not from user input, and is
	// validated here before being interpolated as an identifier.
	if !validIdentifier(cfg.DB.Name) {
		return fmt.Errorf("database name %q contains unsupported characters", cfg.DB.Name)
	}

	stmt := fmt.Sprintf(
		"CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci",
		cfg.DB.Name)
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("creating database: %w", err)
	}

	fmt.Printf("database %q is ready\n", cfg.DB.Name)
	return nil
}

func validIdentifier(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// Migrate applies or rolls back schema migrations.
func Migrate(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return ErrUsage
	}

	m, closeFn, err := openMigrator(cfg)
	if err != nil {
		return err
	}
	defer closeFn()

	switch args[0] {
	case "up":
		if len(args) > 1 {
			steps, err := strconv.Atoi(args[1])
			if err != nil || steps <= 0 {
				return fmt.Errorf("step count must be a positive integer")
			}
			err = m.Steps(steps)
			return reportMigration(m, err)
		}
		return reportMigration(m, m.Up())

	case "down":
		steps := 1
		if len(args) > 1 {
			parsed, err := strconv.Atoi(args[1])
			if err != nil || parsed <= 0 {
				return fmt.Errorf("step count must be a positive integer")
			}
			steps = parsed
		}
		// Rolling back is destructive, so it never defaults to "everything".
		return reportMigration(m, m.Steps(-steps))

	case "version":
		version, dirty, err := m.Version()
		if err != nil {
			if errors.Is(err, migrate.ErrNilVersion) {
				fmt.Println("no migrations have been applied")
				return nil
			}
			return fmt.Errorf("reading version: %w", err)
		}
		state := "clean"
		if dirty {
			state = "DIRTY — a migration failed part-way and must be resolved manually"
		}
		fmt.Printf("version %d (%s)\n", version, state)
		return nil

	case "force":
		if len(args) < 2 {
			return fmt.Errorf("force requires a version number")
		}
		version, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("version must be an integer")
		}
		if err := m.Force(version); err != nil {
			return fmt.Errorf("forcing version: %w", err)
		}
		fmt.Printf("version forced to %d; verify the schema before continuing\n", version)
		return nil

	default:
		return ErrUsage
	}
}

func reportMigration(m *migrate.Migrate, err error) error {
	if err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			fmt.Println("schema is already up to date")
			return nil
		}
		return fmt.Errorf("running migrations: %w", err)
	}

	version, dirty, vErr := m.Version()
	if vErr != nil {
		fmt.Println("migrations applied")
		return nil
	}
	fmt.Printf("migrations applied; schema is now at version %d (dirty=%t)\n", version, dirty)
	return nil
}

func openMigrator(cfg *config.Config) (*migrate.Migrate, func(), error) {
	// golang-migrate needs multi-statement support because each migration file
	// contains several statements.
	dsn := cfg.DB.DSN()
	if u, err := url.ParseQuery(cfg.DB.Params); err == nil {
		u.Set("multiStatements", "true")
		dsn = fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?%s",
			cfg.DB.User, cfg.DB.Password, cfg.DB.Host, cfg.DB.Port, cfg.DB.Name, u.Encode())
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to database: %w", err)
	}

	driver, err := migratemysql.WithInstance(db, &migratemysql.Config{})
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("preparing migration driver: %w", err)
	}

	m, err := migrate.NewWithDatabaseInstance(migrationsPath, cfg.DB.Name, driver)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("loading migrations from %s: %w", migrationsPath, err)
	}

	return m, func() {
		_, _ = m.Close()
		_ = db.Close()
	}, nil
}

var _ = mysql.MySQLError{} // keep the driver import explicit
