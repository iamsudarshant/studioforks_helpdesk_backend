package platform

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/config"
)

// DB wraps the primary connection plus an optional read replica used for
// reporting and dashboard queries.
type DB struct {
	Primary *sqlx.DB
	Replica *sqlx.DB
}

// Read returns the replica when configured, otherwise the primary. Use it for
// analytics/report queries only — anything that must observe its own writes
// has to use Primary.
func (d *DB) Read() *sqlx.DB {
	if d.Replica != nil {
		return d.Replica
	}
	return d.Primary
}

func OpenDB(cfg config.DB) (*DB, error) {
	primary, err := open(cfg.DSN(), cfg)
	if err != nil {
		return nil, fmt.Errorf("primary database: %w", err)
	}

	db := &DB{Primary: primary}

	if dsn := cfg.ReplicaDSN(); dsn != "" {
		replica, err := open(dsn, cfg)
		if err != nil {
			// A missing replica degrades reporting performance but must not
			// take the API down.
			slog.Warn("read replica unavailable, falling back to primary", "error", err)
		} else {
			db.Replica = replica
		}
	}
	return db, nil
}

func open(dsn string, cfg config.DB) (*sqlx.DB, error) {
	db, err := sqlx.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func (d *DB) Close() error {
	var errs []error
	if d.Primary != nil {
		errs = append(errs, d.Primary.Close())
	}
	if d.Replica != nil {
		errs = append(errs, d.Replica.Close())
	}
	return errors.Join(errs...)
}

func (d *DB) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return d.Primary.PingContext(ctx)
}

// Txer is satisfied by both *sqlx.DB and *sqlx.Tx so repository methods can run
// inside or outside a transaction without duplication.
type Txer interface {
	sqlx.ExtContext
	GetContext(ctx context.Context, dest any, query string, args ...any) error
	SelectContext(ctx context.Context, dest any, query string, args ...any) error
}

// InTx runs fn inside a transaction, committing on success and rolling back on
// error or panic. Nested calls are not supported by design — compose at the
// service layer instead.
func (d *DB) InTx(ctx context.Context, fn func(tx *sqlx.Tx) error) error {
	tx, err := d.Primary.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}
	return tx.Commit()
}

// --- MySQL error classification --------------------------------------------

const (
	errDuplicateEntry     = 1062
	errForeignKeyParent   = 1452
	errForeignKeyChild    = 1451
	errLockWaitTimeout    = 1205
	errDeadlock           = 1213
	errDataTooLong        = 1406
	errNoReferencedRow    = 1216
	errRowIsReferenced    = 1217
	errCheckConstraintErr = 3819
)

// IsNotFound reports a zero-row result from Get/QueryRow.
func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// IsDuplicate reports a unique-constraint violation.
func IsDuplicate(err error) bool { return mysqlErrNo(err) == errDuplicateEntry }

// IsForeignKeyViolation reports a referential-integrity failure in either
// direction (missing parent, or child rows still referencing the row).
func IsForeignKeyViolation(err error) bool {
	switch mysqlErrNo(err) {
	case errForeignKeyParent, errForeignKeyChild, errNoReferencedRow, errRowIsReferenced:
		return true
	}
	return false
}

// IsRetryable reports transient failures worth retrying (deadlock, lock wait).
func IsRetryable(err error) bool {
	switch mysqlErrNo(err) {
	case errDeadlock, errLockWaitTimeout:
		return true
	}
	return false
}

// DuplicateKey extracts the index name from a duplicate-entry error so callers
// can map it onto the offending field ("uq_users_email" -> "email").
func DuplicateKey(err error) string {
	var me *mysql.MySQLError
	if !errors.As(err, &me) || me.Number != errDuplicateEntry {
		return ""
	}
	// Message form: Duplicate entry 'x' for key 'users.uq_users_email'
	const marker = "for key '"
	i := strings.Index(me.Message, marker)
	if i < 0 {
		return ""
	}
	rest := me.Message[i+len(marker):]
	j := strings.Index(rest, "'")
	if j < 0 {
		return ""
	}
	key := rest[:j]
	if dot := strings.LastIndex(key, "."); dot >= 0 {
		key = key[dot+1:]
	}
	return key
}

func mysqlErrNo(err error) uint16 {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

// RetryOnDeadlock re-runs fn up to attempts times while it fails with a
// transient lock error. Used for hot paths such as ticket-number allocation.
func RetryOnDeadlock(ctx context.Context, attempts int, fn func() error) error {
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := fn(); err != nil {
			if !IsRetryable(err) {
				return err
			}
			lastErr = err
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(20*(i+1)) * time.Millisecond):
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("exhausted %d attempts: %w", attempts, lastErr)
}
