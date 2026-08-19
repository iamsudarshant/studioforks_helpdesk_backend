// Package document owns encrypted file storage: upload, versioning, preview,
// signed URLs and the access log that feeds the ticket timeline.
package document

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/storage"
)

// Owner types.
const (
	OwnerTicket = "TICKET"
	OwnerUser   = "USER"
	OwnerTenant = "TENANT"
	OwnerReport = "REPORT"
)

// Access actions recorded in document_access_log.
const (
	ActionView     = "VIEW"
	ActionPreview  = "PREVIEW"
	ActionDownload = "DOWNLOAD"
	ActionDelete   = "DELETE"
)

type Document struct {
	ID                 int64          `db:"id"`
	PublicID           string         `db:"public_id"`
	TenantID           int64          `db:"tenant_id"`
	OriginalName       string         `db:"original_name"`
	StoredPath         string         `db:"stored_path"`
	MimeType           string         `db:"mime_type"`
	SizeBytes          int64          `db:"size_bytes"`
	ChecksumSHA256     sql.NullString `db:"checksum_sha256"`
	DocumentCategoryID sql.NullInt64  `db:"document_category_id"`
	Description        sql.NullString `db:"description"`
	Version            int            `db:"version"`
	CurrentVersionID   sql.NullInt64  `db:"current_version_id"`
	IsEncrypted        bool           `db:"is_encrypted"`
	EncryptionKeyID    sql.NullString `db:"encryption_key_id"`
	Nonce              []byte         `db:"nonce"`
	ScanStatus         string         `db:"scan_status"`
	OwnerType          string         `db:"owner_type"`
	OwnerID            sql.NullInt64  `db:"owner_id"`
	UploadedBy         sql.NullInt64  `db:"uploaded_by"`
	RetentionUntil     sql.NullTime   `db:"retention_until"`
	CreatedAt          time.Time      `db:"created_at"`
	UpdatedAt          time.Time      `db:"updated_at"`
	DeletedAt          sql.NullTime   `db:"deleted_at"`
}

type Version struct {
	ID              int64          `db:"id"`
	PublicID        string         `db:"public_id"`
	TenantID        int64          `db:"tenant_id"`
	DocumentID      int64          `db:"document_id"`
	Version         int            `db:"version"`
	StoredPath      string         `db:"stored_path"`
	OriginalName    string         `db:"original_name"`
	MimeType        string         `db:"mime_type"`
	SizeBytes       int64          `db:"size_bytes"`
	ChecksumSHA256  sql.NullString `db:"checksum_sha256"`
	EncryptionKeyID sql.NullString `db:"encryption_key_id"`
	Nonce           []byte         `db:"nonce"`
	ChangeNote      sql.NullString `db:"change_note"`
	UploadedBy      sql.NullInt64  `db:"uploaded_by"`
	CreatedAt       time.Time      `db:"created_at"`
}

type Repository struct {
	db *platform.DB
}

func NewRepository(db *platform.DB) *Repository { return &Repository{db: db} }

const documentColumns = `id, public_id, tenant_id, original_name, stored_path, mime_type,
	size_bytes, checksum_sha256, document_category_id, description, version, current_version_id,
	is_encrypted, encryption_key_id, nonce, scan_status, owner_type, owner_id, uploaded_by,
	retention_until, created_at, updated_at, deleted_at`

type CreateParams struct {
	TenantID     int64
	OriginalName string
	MimeType     string
	Object       *storage.Object
	CategoryID   *int64
	Description  string
	OwnerType    string
	OwnerID      *int64
	UploadedBy   *int64
	ScanStatus   string
	RetainUntil  *time.Time
}

// Create records an uploaded file plus its wrapped data-encryption key and
// first version, in one transaction.
func (r *Repository) Create(ctx context.Context, p CreateParams) (*Document, error) {
	var created *Document

	err := r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO encryption_keys (key_id, wrapped_dek, algo) VALUES (?,?,?)`,
			p.Object.KeyID, p.Object.WrappedDEK, "AES-256-GCM"); err != nil {
			return fmt.Errorf("storing data key: %w", err)
		}

		publicID := platform.NewULID()
		scan := p.ScanStatus
		if scan == "" {
			scan = "SKIPPED"
		}

		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents
				(public_id, tenant_id, original_name, stored_path, mime_type, size_bytes,
				 checksum_sha256, document_category_id, description, version, is_encrypted,
				 encryption_key_id, nonce, scan_status, owner_type, owner_id, uploaded_by, retention_until)
			VALUES (?,?,?,?,?,?,?,?,?,1,1,?,?,?,?,?,?,?)`,
			publicID, p.TenantID, p.OriginalName, p.Object.Path, p.MimeType, p.Object.Size,
			p.Object.Checksum, p.CategoryID, nullStr(p.Description),
			p.Object.KeyID, p.Object.Nonce, scan, p.OwnerType, p.OwnerID, p.UploadedBy, p.RetainUntil)
		if err != nil {
			return fmt.Errorf("creating document: %w", err)
		}

		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("reading document id: %w", err)
		}

		versionRes, err := tx.ExecContext(ctx, `
			INSERT INTO document_versions
				(public_id, tenant_id, document_id, version, stored_path, original_name,
				 mime_type, size_bytes, checksum_sha256, encryption_key_id, nonce, uploaded_by)
			VALUES (?,?,?,1,?,?,?,?,?,?,?,?)`,
			platform.NewULID(), p.TenantID, id, p.Object.Path, p.OriginalName,
			p.MimeType, p.Object.Size, p.Object.Checksum, p.Object.KeyID, p.Object.Nonce, p.UploadedBy)
		if err != nil {
			return fmt.Errorf("creating document version: %w", err)
		}

		versionID, err := versionRes.LastInsertId()
		if err != nil {
			return fmt.Errorf("reading version id: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE documents SET current_version_id = ? WHERE id = ?`, versionID, id); err != nil {
			return fmt.Errorf("linking current version: %w", err)
		}

		var doc Document
		if err := tx.GetContext(ctx, &doc,
			`SELECT `+documentColumns+` FROM documents WHERE id = ?`, id); err != nil {
			return fmt.Errorf("reloading document: %w", err)
		}
		created = &doc
		return nil
	})

	return created, err
}

func (r *Repository) ByID(ctx context.Context, tenantID, id int64) (*Document, error) {
	var d Document
	err := r.db.Primary.GetContext(ctx, &d,
		`SELECT `+documentColumns+` FROM documents
		 WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, tenantID, id)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading document: %w", err)
	}
	return &d, nil
}

func (r *Repository) ByPublicID(ctx context.Context, tenantID int64, publicID string) (*Document, error) {
	var d Document
	err := r.db.Primary.GetContext(ctx, &d,
		`SELECT `+documentColumns+` FROM documents
		 WHERE tenant_id = ? AND public_id = ? AND deleted_at IS NULL`, tenantID, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading document: %w", err)
	}
	return &d, nil
}

// ByPublicIDInReach loads a document from anywhere the caller can reach.
//
// Attachments hang off tickets, and staff working the cross-client ticket list
// have no client selected — so the tenant the header names is the platform
// workspace, and a pinned lookup answers "not found" for every file on a ticket
// they are looking at. The reach is still the boundary; whether the caller may
// read this particular file is decided afterwards by authorise().
func (r *Repository) ByPublicIDInReach(ctx context.Context, reach appctx.ClientReach, publicID string) (*Document, error) {
	where := []string{"public_id = ?", "deleted_at IS NULL"}
	args := []any{publicID}

	switch {
	case reach.All:
		// Every client.
	case len(reach.TenantIDs) > 0:
		where = append(where, "tenant_id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(reach.TenantIDs)...)
	default:
		where = append(where, "1 = 0")
	}

	var d Document
	err := r.db.Primary.GetContext(ctx, &d,
		`SELECT `+documentColumns+` FROM documents WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading document: %w", err)
	}
	return &d, nil
}

// TicketRef is the minimum a document needs to know about a ticket: which one,
// and whose workspace it lives in.
type TicketRef struct {
	ID       int64 `db:"id"`
	TenantID int64 `db:"tenant_id"`
}

// TicketRefInReach resolves a ticket for an upload, anywhere the caller reaches.
func (r *Repository) TicketRefInReach(ctx context.Context, reach appctx.ClientReach, publicID string) (*TicketRef, error) {
	where := []string{"public_id = ?", "deleted_at IS NULL"}
	args := []any{publicID}

	switch {
	case reach.All:
	case len(reach.TenantIDs) > 0:
		where = append(where, "tenant_id IN ("+platform.Placeholders(len(reach.TenantIDs))+")")
		args = append(args, platform.Int64Args(reach.TenantIDs)...)
	default:
		where = append(where, "1 = 0")
	}

	var t TicketRef
	err := r.db.Primary.GetContext(ctx, &t,
		`SELECT id, tenant_id FROM tickets WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("resolving ticket: %w", err)
	}
	return &t, nil
}

// ByPublicIDAcrossTenants loads a document without a tenant filter.
//
// This is the one query in the codebase that crosses the tenant boundary, and
// it exists for exactly one caller: serving a signed link, where the browser
// cannot send a workspace header on a direct <img> or <iframe> load. It is safe
// only because the caller has already verified an HMAC that binds this document
// id, and it must stay that way — do not reach for it anywhere else.
func (r *Repository) ByPublicIDAcrossTenants(ctx context.Context, publicID string) (*Document, error) {
	var d Document
	err := r.db.Primary.GetContext(ctx, &d,
		`SELECT `+documentColumns+` FROM documents
		 WHERE public_id = ? AND deleted_at IS NULL`, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading document: %w", err)
	}
	return &d, nil
}

// ByOwner lists the documents attached to one owning record.
func (r *Repository) ByOwner(ctx context.Context, tenantID int64, ownerType string, ownerID int64) ([]Document, error) {
	rows := []Document{}
	err := r.db.Primary.SelectContext(ctx, &rows,
		`SELECT `+documentColumns+` FROM documents
		 WHERE tenant_id = ? AND owner_type = ? AND owner_id = ? AND deleted_at IS NULL
		 ORDER BY created_at DESC`, tenantID, ownerType, ownerID)
	if err != nil {
		return nil, fmt.Errorf("listing documents: %w", err)
	}
	return rows, nil
}

// ResolveIDs maps document public ids to internal ids within the tenant. Used
// when a create call references files uploaded in a previous step.
func (r *Repository) ResolveIDs(ctx context.Context, tenantID int64, publicIDs []string) ([]int64, error) {
	if len(publicIDs) == 0 {
		return []int64{}, nil
	}
	ids := []int64{}
	args := append([]any{tenantID}, platform.StringArgs(publicIDs)...)
	q := `SELECT id FROM documents WHERE tenant_id = ? AND public_id IN (` +
		platform.Placeholders(len(publicIDs)) + `) AND deleted_at IS NULL`

	if err := r.db.Primary.SelectContext(ctx, &ids, q, args...); err != nil {
		return nil, fmt.Errorf("resolving document ids: %w", err)
	}
	if len(ids) != len(publicIDs) {
		return nil, platform.ErrSentinelNotFound
	}
	return ids, nil
}

// WrappedKey returns the sealed data-encryption key for a document.
func (r *Repository) WrappedKey(ctx context.Context, keyID string) ([]byte, error) {
	var wrapped []byte
	err := r.db.Primary.GetContext(ctx, &wrapped,
		`SELECT wrapped_dek FROM encryption_keys WHERE key_id = ?`, keyID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading data key: %w", err)
	}
	return wrapped, nil
}

// AddVersion appends a new version and repoints the document at it. Previous
// versions stay retrievable.
func (r *Repository) AddVersion(ctx context.Context, tenantID, documentID int64, p CreateParams, changeNote string) (*Document, error) {
	var updated *Document

	err := r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		var current int
		if err := tx.GetContext(ctx, &current,
			`SELECT version FROM documents WHERE tenant_id = ? AND id = ? FOR UPDATE`,
			tenantID, documentID); err != nil {
			if platform.IsNotFound(err) {
				return platform.ErrSentinelNotFound
			}
			return fmt.Errorf("loading current version: %w", err)
		}
		next := current + 1

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO encryption_keys (key_id, wrapped_dek, algo) VALUES (?,?,?)`,
			p.Object.KeyID, p.Object.WrappedDEK, "AES-256-GCM"); err != nil {
			return fmt.Errorf("storing data key: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
			INSERT INTO document_versions
				(public_id, tenant_id, document_id, version, stored_path, original_name,
				 mime_type, size_bytes, checksum_sha256, encryption_key_id, nonce, change_note, uploaded_by)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			platform.NewULID(), tenantID, documentID, next, p.Object.Path, p.OriginalName,
			p.MimeType, p.Object.Size, p.Object.Checksum, p.Object.KeyID, p.Object.Nonce,
			nullStr(changeNote), p.UploadedBy)
		if err != nil {
			return fmt.Errorf("creating version: %w", err)
		}
		versionID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("reading version id: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE documents
			SET version = ?, current_version_id = ?, stored_path = ?, mime_type = ?,
			    size_bytes = ?, checksum_sha256 = ?, encryption_key_id = ?, nonce = ?,
			    original_name = ?
			WHERE tenant_id = ? AND id = ?`,
			next, versionID, p.Object.Path, p.MimeType, p.Object.Size, p.Object.Checksum,
			p.Object.KeyID, p.Object.Nonce, p.OriginalName, tenantID, documentID); err != nil {
			return fmt.Errorf("updating document: %w", err)
		}

		var doc Document
		if err := tx.GetContext(ctx, &doc,
			`SELECT `+documentColumns+` FROM documents WHERE id = ?`, documentID); err != nil {
			return fmt.Errorf("reloading document: %w", err)
		}
		updated = &doc
		return nil
	})

	return updated, err
}

func (r *Repository) Versions(ctx context.Context, tenantID, documentID int64) ([]Version, error) {
	rows := []Version{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT id, public_id, tenant_id, document_id, version, stored_path, original_name,
		       mime_type, size_bytes, checksum_sha256, encryption_key_id, nonce,
		       change_note, uploaded_by, created_at
		FROM document_versions
		WHERE tenant_id = ? AND document_id = ?
		ORDER BY version DESC`, tenantID, documentID)
	if err != nil {
		return nil, fmt.Errorf("listing versions: %w", err)
	}
	return rows, nil
}

// VersionAt loads one revision of a document.
func (r *Repository) VersionAt(ctx context.Context, tenantID, documentID int64, version int) (*Version, error) {
	var v Version
	err := r.db.Primary.GetContext(ctx, &v, `
		SELECT id, public_id, tenant_id, document_id, version, stored_path, original_name,
		       mime_type, size_bytes, checksum_sha256, encryption_key_id, nonce,
		       change_note, uploaded_by, created_at
		FROM document_versions
		WHERE tenant_id = ? AND document_id = ? AND version = ?`,
		tenantID, documentID, version)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading a document version: %w", err)
	}
	return &v, nil
}

func (r *Repository) SoftDelete(ctx context.Context, tenantID, id int64) error {
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE documents SET deleted_at = UTC_TIMESTAMP(3)
		 WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, tenantID, id)
	if err != nil {
		return fmt.Errorf("deleting document: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading affected rows: %w", err)
	}
	if n == 0 {
		return platform.ErrSentinelNotFound
	}
	return nil
}

// LogAccess records every read. This is what makes "downloaded by X at Y"
// possible on the ticket timeline and in compliance reporting.
func (r *Repository) LogAccess(ctx context.Context, tenantID, documentID int64, userID *int64, action, ip, userAgent string) {
	_, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO document_access_log (tenant_id, document_id, user_id, action, ip, user_agent)
		VALUES (?,?,?,?,?,?)`,
		tenantID, documentID, userID, action, nullStr(ip), nullStr(truncate(userAgent, 255)))
	if err != nil {
		// Access logging must not fail the download itself; the miss is logged
		// by the caller's error handling path.
		return
	}
}

type AccessLogEntry struct {
	Action    string         `db:"action" json:"action"`
	UserID    sql.NullInt64  `db:"user_id" json:"-"`
	UserName  sql.NullString `db:"user_name" json:"user_name"`
	IP        sql.NullString `db:"ip" json:"ip"`
	CreatedAt time.Time      `db:"created_at" json:"created_at"`
}

func (r *Repository) AccessLog(ctx context.Context, tenantID, documentID int64, limit int) ([]AccessLogEntry, error) {
	rows := []AccessLogEntry{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT l.action, l.user_id, CONCAT(u.first_name, ' ', COALESCE(u.last_name,'')) AS user_name,
		       l.ip, l.created_at
		FROM document_access_log l
		LEFT JOIN users u ON u.id = l.user_id
		WHERE l.tenant_id = ? AND l.document_id = ?
		ORDER BY l.created_at DESC
		LIMIT ?`, tenantID, documentID, limit)
	if err != nil {
		return nil, fmt.Errorf("loading access log: %w", err)
	}
	return rows, nil
}

// --- document categories ----------------------------------------------------

type Category struct {
	ID       int64  `db:"id" json:"-"`
	PublicID string `db:"public_id" json:"id"`
	TenantID int64  `db:"tenant_id" json:"-"`
	Key      string `db:"cat_key" json:"key"`
	Name     string `db:"name" json:"name"`
	IsActive bool   `db:"is_active" json:"is_active"`
}

func (r *Repository) Categories(ctx context.Context, tenantID int64) ([]Category, error) {
	rows := []Category{}
	err := r.db.Primary.SelectContext(ctx, &rows,
		`SELECT id, public_id, tenant_id, cat_key, name, is_active
		 FROM document_categories WHERE tenant_id = ? ORDER BY name`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("listing document categories: %w", err)
	}
	return rows, nil
}

func (r *Repository) CategoryByPublicID(ctx context.Context, tenantID int64, publicID string) (*Category, error) {
	var c Category
	err := r.db.Primary.GetContext(ctx, &c,
		`SELECT id, public_id, tenant_id, cat_key, name, is_active
		 FROM document_categories WHERE tenant_id = ? AND public_id = ?`, tenantID, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading document category: %w", err)
	}
	return &c, nil
}

func (r *Repository) CreateCategory(ctx context.Context, tenantID int64, key, name string) (*Category, error) {
	res, err := r.db.Primary.ExecContext(ctx,
		`INSERT INTO document_categories (public_id, tenant_id, cat_key, name) VALUES (?,?,?,?)`,
		platform.NewULID(), tenantID, key, name)
	if err != nil {
		if platform.IsDuplicate(err) {
			return nil, platform.ErrSentinelConflict
		}
		return nil, fmt.Errorf("creating document category: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading category id: %w", err)
	}

	var c Category
	if err := r.db.Primary.GetContext(ctx, &c,
		`SELECT id, public_id, tenant_id, cat_key, name, is_active FROM document_categories WHERE id = ?`,
		id); err != nil {
		return nil, fmt.Errorf("reloading category: %w", err)
	}
	return &c, nil
}

func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
