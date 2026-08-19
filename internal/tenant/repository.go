package tenant

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

type Repository struct {
	db *platform.DB
}

func NewRepository(db *platform.DB) *Repository { return &Repository{db: db} }

const tenantColumns = `id, public_id, slug, client_code, name, legal_name, industry, status, is_platform,
	timezone, locale, date_format, ticket_prefix, contact_email, alt_email, contact_phone,
	alt_phone, address, tax_id, contract_start, contract_end, retention_policy_json,
	onboarded_at, account_manager_id, created_at, updated_at, deleted_at`

// BySlug resolves a workspace from the identifier in the URL or the
// X-Tenant-Slug header.
//
// It accepts either the slug or the client code, because the client-side
// portals are addressed by code — `/INF/user` — while internal tooling, the
// seeder and existing integrations use the slug. Making one lookup understand
// both means neither the router nor the caller has to know which it holds.
//
// The slug is tried first: it is the canonical identifier, and preferring it
// means a client code that happens to collide with another workspace's slug can
// never shadow that workspace.
func (r *Repository) BySlug(ctx context.Context, slug string) (*Tenant, error) {
	var t Tenant
	err := r.db.Primary.GetContext(ctx, &t,
		`SELECT `+tenantColumns+` FROM tenants
		 WHERE deleted_at IS NULL AND (slug = ? OR client_code = ?)
		 ORDER BY (slug = ?) DESC
		 LIMIT 1`, slug, slug, slug)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading tenant by slug: %w", err)
	}
	return &t, nil
}

func (r *Repository) ByID(ctx context.Context, id int64) (*Tenant, error) {
	var t Tenant
	err := r.db.Primary.GetContext(ctx, &t,
		`SELECT `+tenantColumns+` FROM tenants WHERE id = ? AND deleted_at IS NULL`, id)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading tenant: %w", err)
	}
	return &t, nil
}

func (r *Repository) ByPublicID(ctx context.Context, publicID string) (*Tenant, error) {
	var t Tenant
	err := r.db.Primary.GetContext(ctx, &t,
		`SELECT `+tenantColumns+` FROM tenants WHERE public_id = ? AND deleted_at IS NULL`, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading tenant: %w", err)
	}
	return &t, nil
}

func (r *Repository) ByDomain(ctx context.Context, domain string) (*Tenant, error) {
	var t Tenant
	err := r.db.Primary.GetContext(ctx, &t, `
		SELECT `+prefixColumns(tenantColumns, "t")+`
		FROM tenants t
		JOIN tenant_domains d ON d.tenant_id = t.id
		WHERE d.domain = ? AND t.deleted_at IS NULL`, domain)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading tenant by domain: %w", err)
	}
	return &t, nil
}

// prefixColumns qualifies a column list for a joined query.
func prefixColumns(cols, alias string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

type ListFilter struct {
	Query  string
	Status []string
	Page   platform.Page
}

func (r *Repository) List(ctx context.Context, f ListFilter) ([]Tenant, int64, error) {
	// Karma's own tenant is not a client and never appears in a client list.
	where := []string{"deleted_at IS NULL", "is_platform = 0"}
	args := []any{}

	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, "(name LIKE ? OR slug LIKE ? OR legal_name LIKE ? OR client_code LIKE ?)")
		like := "%" + q + "%"
		args = append(args, like, like, like, like)
	}
	if len(f.Status) > 0 {
		where = append(where, "status IN ("+platform.Placeholders(len(f.Status))+")")
		args = append(args, platform.StringArgs(f.Status)...)
	}

	clause := " WHERE " + strings.Join(where, " AND ")

	var total int64
	if err := r.db.Primary.GetContext(ctx, &total, `SELECT COUNT(*) FROM tenants`+clause, args...); err != nil {
		return nil, 0, fmt.Errorf("counting tenants: %w", err)
	}

	rows := []Tenant{}
	q := `SELECT ` + tenantColumns + ` FROM tenants` + clause + f.Page.OrderBy() + ` LIMIT ? OFFSET ?`
	args = append(args, f.Page.PerPage, f.Page.Offset())
	if err := r.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, 0, fmt.Errorf("listing tenants: %w", err)
	}
	return rows, total, nil
}

type CreateParams struct {
	Slug          string
	ClientCode    string
	Name          string
	LegalName     string
	Industry      string
	Timezone      string
	Locale        string
	DateFormat    string
	TicketPrefix  string
	ContactEmail  string
	AltEmail      string
	ContactPhone  string
	AltPhone      string
	Address       string
	TaxID         string
	ContractStart *time.Time
	ContractEnd   *time.Time
	CreatedBy     *int64
}

// TakenField names which of a new client's unique fields is already in use.
//
// The insert can collide on the workspace address or on the client code, and
// the driver reports both the same way. Reporting "slug" for a client-code
// clash points the operator at a field that is perfectly fine, so the collision
// is identified before the message is written. Returns "" when neither is taken
// — a conflict from somewhere else entirely, which stays generic rather than
// being blamed on the wrong field.
func (r *Repository) TakenField(ctx context.Context, slug, clientCode string) string {
	var n int64

	if slug != "" {
		if err := r.db.Primary.GetContext(ctx, &n,
			`SELECT COUNT(*) FROM tenants WHERE slug = ?`, strings.ToLower(slug)); err == nil && n > 0 {
			return "slug"
		}
	}
	if clientCode != "" {
		if err := r.db.Primary.GetContext(ctx, &n,
			`SELECT COUNT(*) FROM tenants WHERE client_code = ?`, strings.ToUpper(clientCode)); err == nil && n > 0 {
			return "client_code"
		}
	}
	return ""
}

// Create inserts the tenant plus its default branding, features, settings and
// system user groups in one transaction, so a tenant is never half-provisioned.
// A client is live from the moment it is created; there is no onboarding step.
func (r *Repository) Create(ctx context.Context, p CreateParams) (*Tenant, error) {
	var created *Tenant

	err := r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		publicID := platform.NewULID()

		res, err := tx.ExecContext(ctx, `
			INSERT INTO tenants
				(public_id, slug, client_code, name, legal_name, industry, status, timezone,
				 locale, date_format, ticket_prefix, contact_email, alt_email, contact_phone,
				 alt_phone, address, tax_id, contract_start, contract_end, created_by)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			publicID, p.Slug, nullString(p.ClientCode), p.Name, nullString(p.LegalName),
			nullString(p.Industry), StatusActive,
			defaultTo(p.Timezone, "Asia/Kolkata"), defaultTo(p.Locale, "en-IN"),
			defaultTo(p.DateFormat, "dd/MM/yyyy"), defaultTo(p.TicketPrefix, "HD"),
			nullString(p.ContactEmail), nullString(p.AltEmail), nullString(p.ContactPhone),
			nullString(p.AltPhone), nullString(p.Address),
			nullString(p.TaxID), p.ContractStart, p.ContractEnd, p.CreatedBy)
		if err != nil {
			if platform.IsDuplicate(err) {
				return platform.ErrSentinelConflict
			}
			return fmt.Errorf("inserting tenant: %w", err)
		}

		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("reading tenant id: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tenant_branding (tenant_id) VALUES (?)`, id); err != nil {
			return fmt.Errorf("creating branding: %w", err)
		}

		for key, enabled := range DefaultFeatures() {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO tenant_features (tenant_id, feature_key, enabled) VALUES (?,?,?)`,
				id, key, enabled); err != nil {
				return fmt.Errorf("creating feature %s: %w", key, err)
			}
		}

		for key, value := range defaultSettings() {
			raw, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("encoding default setting %s: %w", key, err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO tenant_settings (tenant_id, setting_key, value_json) VALUES (?,?,?)`,
				id, key, string(raw)); err != nil {
				return fmt.Errorf("creating setting %s: %w", key, err)
			}
		}

		// The two system groups every tenant needs from day one.
		if err := seedSystemGroups(ctx, tx, id); err != nil {
			return err
		}

		var t Tenant
		if err := tx.GetContext(ctx, &t,
			`SELECT `+tenantColumns+` FROM tenants WHERE id = ?`, id); err != nil {
			return fmt.Errorf("reloading tenant: %w", err)
		}
		created = &t
		return nil
	})

	return created, err
}

func seedSystemGroups(ctx context.Context, tx *sqlx.Tx, tenantID int64) error {
	groups := []struct {
		key, name, desc, mode string
		grace                 int
	}{
		{"ACTIVE_EMPLOYEES", "Active Employees",
			"Standard users with full access to raise and track tickets.", "FULL", 0},
		{"EX_EMPLOYEES", "Ex-Employees / Old Employees",
			"Separated employees. Read-only access to historic tickets for the configured grace period.",
			"READ_ONLY", 90},
	}
	for _, g := range groups {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_groups
				(public_id, tenant_id, group_key, name, description, is_system, access_mode, grace_period_days)
			VALUES (?,?,?,?,?,1,?,?)`,
			platform.NewULID(), tenantID, g.key, g.name, g.desc, g.mode, g.grace); err != nil {
			return fmt.Errorf("seeding group %s: %w", g.key, err)
		}
	}
	return nil
}

func defaultSettings() map[string]any {
	return map[string]any{
		SettingPasswordPolicy: map[string]any{
			"min_length": 12, "require_upper": true, "require_lower": true,
			"require_number": true, "require_symbol": false, "history_count": 5,
			"expiry_days": 90, "max_failed_attempts": 5, "lockout_minutes": 15,
		},
		SettingLoginIdentifiers:   DefaultLoginIdentifiers(),
		SettingAuthMethods:        []string{"password"},
		SettingSessionPolicy:      map[string]any{"idle_timeout_mins": 30, "max_concurrent": 5},
		SettingReopenWindowDays:   15,
		SettingAutoCloseDays:      7,
		SettingPendingUserDays:    5,
		SettingTicketNumberFormat: "{prefix}-{yyyy}-{seq:6}",
		SettingBulkOnboardingMode: OnboardingModeRandom,
		SettingUploadLimits:       map[string]any{"max_file_mb": 25, "max_files_per_ticket": 20},
		SettingRetentionPolicy: map[string]any{
			"active_ticket_years": 1, "archived_ticket_years": 7,
			"audit_log_years": 7, "document_years": 7,
		},
		SettingMandatoryUserFields: []string{"pf_number", "date_of_joining"},
		SettingPIIMasking:          map[string]any{"mask_pan": true, "mask_uan": true, "mask_pf": true},
	}
}

type UpdateParams struct {
	ClientCode    *string
	Name          *string
	LegalName     *string
	Industry      *string
	AltEmail      *string
	AltPhone      *string
	Timezone      *string
	Locale        *string
	DateFormat    *string
	TicketPrefix  *string
	ContactEmail  *string
	ContactPhone  *string
	Address       *string
	TaxID         *string
	ContractStart *time.Time
	ContractEnd   *time.Time
	Status        *string
}

func (r *Repository) Update(ctx context.Context, id int64, p UpdateParams) error {
	set := []string{}
	args := []any{}

	addStr := func(col string, v *string) {
		if v != nil {
			set = append(set, col+" = ?")
			args = append(args, nullString(*v))
		}
	}
	addStr("client_code", p.ClientCode)
	addStr("name", p.Name)
	addStr("legal_name", p.LegalName)
	addStr("industry", p.Industry)
	addStr("alt_email", p.AltEmail)
	addStr("alt_phone", p.AltPhone)
	addStr("timezone", p.Timezone)
	addStr("locale", p.Locale)
	addStr("date_format", p.DateFormat)
	addStr("ticket_prefix", p.TicketPrefix)
	addStr("contact_email", p.ContactEmail)
	addStr("contact_phone", p.ContactPhone)
	addStr("address", p.Address)
	addStr("tax_id", p.TaxID)
	addStr("status", p.Status)

	if p.ContractStart != nil {
		set = append(set, "contract_start = ?")
		args = append(args, *p.ContractStart)
	}
	if p.ContractEnd != nil {
		set = append(set, "contract_end = ?")
		args = append(args, *p.ContractEnd)
	}
	if len(set) == 0 {
		return nil
	}

	args = append(args, id)
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE tenants SET `+strings.Join(set, ", ")+` WHERE id = ? AND deleted_at IS NULL`, args...)
	if err != nil {
		return fmt.Errorf("updating tenant: %w", err)
	}
	return requireAffected(res)
}

func (r *Repository) SetStatus(ctx context.Context, id int64, status string) error {
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE tenants SET status = ? WHERE id = ? AND deleted_at IS NULL`, status, id)
	if err != nil {
		return fmt.Errorf("setting tenant status: %w", err)
	}
	return requireAffected(res)
}

func (r *Repository) SoftDelete(ctx context.Context, id int64) error {
	return r.Archive(ctx, id)
}

// Archive marks a client deleted and ARCHIVED.
//
// Nothing is removed: users, tickets, documents and audit history all stay. The
// row simply stops appearing in listings and nobody can sign into it, which is
// what makes Restore possible.
func (r *Repository) Archive(ctx context.Context, id int64) error {
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE tenants SET deleted_at = UTC_TIMESTAMP(3), status = ?
		 WHERE id = ? AND deleted_at IS NULL`, StatusArchived, id)
	if err != nil {
		return fmt.Errorf("archiving tenant: %w", err)
	}
	return requireAffected(res)
}

// Restore brings an archived client back, suspended.
//
// Deliberately not ACTIVE: restoring a workspace should not silently let
// thousands of employees back in. Activation stays a separate, considered step.
func (r *Repository) Restore(ctx context.Context, id int64) error {
	res, err := r.db.Primary.ExecContext(ctx,
		`UPDATE tenants SET deleted_at = NULL, status = ?
		 WHERE id = ? AND deleted_at IS NOT NULL`, StatusSuspended, id)
	if err != nil {
		return fmt.Errorf("restoring tenant: %w", err)
	}
	return requireAffected(res)
}

// ByPublicIDIncludingArchived finds a client whether or not it is archived.
// Needed by restore, which by definition operates on a hidden row.
func (r *Repository) ByPublicIDIncludingArchived(ctx context.Context, publicID string) (*Tenant, error) {
	var t Tenant
	err := r.db.Primary.GetContext(ctx, &t,
		`SELECT `+tenantColumns+` FROM tenants WHERE public_id = ?`, publicID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("loading archived tenant: %w", err)
	}
	return &t, nil
}

// Archived lists the clients an administrator can restore.
func (r *Repository) Archived(ctx context.Context) ([]Tenant, error) {
	rows := []Tenant{}
	err := r.db.Primary.SelectContext(ctx, &rows,
		`SELECT `+tenantColumns+` FROM tenants
		 WHERE deleted_at IS NOT NULL AND is_platform = 0
		 ORDER BY deleted_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing archived tenants: %w", err)
	}
	return rows, nil
}

// --- branding ---------------------------------------------------------------

func (r *Repository) Branding(ctx context.Context, tenantID int64) (*Branding, error) {
	var b Branding
	err := r.db.Primary.GetContext(ctx, &b, `
		SELECT tenant_id, logo_path, logo_dark_path, favicon_path, login_bg_path,
		       email_header_path, primary_color, secondary_color, accent_color,
		       show_complydesk_logo, custom_css, updated_at
		FROM tenant_branding WHERE tenant_id = ?`, tenantID)
	if err != nil {
		if platform.IsNotFound(err) {
			// A tenant created before branding existed still needs a response.
			return &Branding{
				TenantID: tenantID, PrimaryColor: "#1A73E8",
				SecondaryColor: "#5F6368", AccentColor: "#00897B",
				ShowComplyDeskLogo: true,
			}, nil
		}
		return nil, fmt.Errorf("loading branding: %w", err)
	}
	return &b, nil
}

type BrandingUpdate struct {
	LogoPath           *string
	LogoDarkPath       *string
	FaviconPath        *string
	LoginBgPath        *string
	EmailHeaderPath    *string
	PrimaryColor       *string
	SecondaryColor     *string
	AccentColor        *string
	ShowComplyDeskLogo *bool
	CustomCSS          *string
}

func (r *Repository) UpdateBranding(ctx context.Context, tenantID int64, u BrandingUpdate) error {
	set := []string{}
	args := []any{}

	addStr := func(col string, v *string) {
		if v != nil {
			set = append(set, col+" = ?")
			args = append(args, nullString(*v))
		}
	}
	addStr("logo_path", u.LogoPath)
	addStr("logo_dark_path", u.LogoDarkPath)
	addStr("favicon_path", u.FaviconPath)
	addStr("login_bg_path", u.LoginBgPath)
	addStr("email_header_path", u.EmailHeaderPath)
	addStr("custom_css", u.CustomCSS)

	if u.PrimaryColor != nil {
		set = append(set, "primary_color = ?")
		args = append(args, *u.PrimaryColor)
	}
	if u.SecondaryColor != nil {
		set = append(set, "secondary_color = ?")
		args = append(args, *u.SecondaryColor)
	}
	if u.AccentColor != nil {
		set = append(set, "accent_color = ?")
		args = append(args, *u.AccentColor)
	}
	if u.ShowComplyDeskLogo != nil {
		set = append(set, "show_complydesk_logo = ?")
		args = append(args, *u.ShowComplyDeskLogo)
	}
	if len(set) == 0 {
		return nil
	}

	args = append(args, tenantID)
	_, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO tenant_branding (tenant_id) VALUES (?)
		ON DUPLICATE KEY UPDATE tenant_id = tenant_id`, tenantID)
	if err != nil {
		return fmt.Errorf("ensuring branding row: %w", err)
	}

	if _, err := r.db.Primary.ExecContext(ctx,
		`UPDATE tenant_branding SET `+strings.Join(set, ", ")+` WHERE tenant_id = ?`, args...); err != nil {
		return fmt.Errorf("updating branding: %w", err)
	}
	return nil
}

// --- features ---------------------------------------------------------------

func (r *Repository) Features(ctx context.Context, tenantID int64) (map[string]bool, error) {
	rows := []struct {
		Key     string `db:"feature_key"`
		Enabled bool   `db:"enabled"`
	}{}
	if err := r.db.Primary.SelectContext(ctx, &rows,
		`SELECT feature_key, enabled FROM tenant_features WHERE tenant_id = ?`, tenantID); err != nil {
		return nil, fmt.Errorf("loading features: %w", err)
	}

	// Start from defaults so a feature added after the tenant was created still
	// resolves to a sane value rather than being absent.
	out := DefaultFeatures()
	for _, row := range rows {
		out[row.Key] = row.Enabled
	}
	return out, nil
}

func (r *Repository) SetFeatures(ctx context.Context, tenantID int64, features map[string]bool) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		for key, enabled := range features {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO tenant_features (tenant_id, feature_key, enabled)
				VALUES (?,?,?)
				ON DUPLICATE KEY UPDATE enabled = VALUES(enabled)`,
				tenantID, key, enabled); err != nil {
				return fmt.Errorf("setting feature %s: %w", key, err)
			}
		}
		return nil
	})
}

// --- settings ---------------------------------------------------------------

func (r *Repository) Settings(ctx context.Context, tenantID int64) (map[string]json.RawMessage, error) {
	rows := []struct {
		Key      string `db:"setting_key"`
		Value    string `db:"value_json"`
		IsSecret bool   `db:"is_secret"`
	}{}
	if err := r.db.Primary.SelectContext(ctx, &rows,
		`SELECT setting_key, value_json, is_secret FROM tenant_settings WHERE tenant_id = ?`,
		tenantID); err != nil {
		return nil, fmt.Errorf("loading settings: %w", err)
	}

	out := map[string]json.RawMessage{}
	for key, value := range defaultSettings() {
		raw, err := json.Marshal(value)
		if err != nil {
			continue
		}
		out[key] = raw
	}
	for _, row := range rows {
		out[row.Key] = json.RawMessage(row.Value)
	}
	return out, nil
}

// Setting decodes a single setting into dst, falling back to the platform
// default when the tenant has no row.
func (r *Repository) Setting(ctx context.Context, tenantID int64, key string, dst any) error {
	var raw string
	err := r.db.Primary.GetContext(ctx, &raw,
		`SELECT value_json FROM tenant_settings WHERE tenant_id = ? AND setting_key = ?`,
		tenantID, key)

	if err != nil {
		if !platform.IsNotFound(err) {
			return fmt.Errorf("loading setting %s: %w", key, err)
		}
		def, ok := defaultSettings()[key]
		if !ok {
			return platform.ErrSentinelNotFound
		}
		b, mErr := json.Marshal(def)
		if mErr != nil {
			return fmt.Errorf("encoding default setting %s: %w", key, mErr)
		}
		raw = string(b)
	}

	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		return fmt.Errorf("decoding setting %s: %w", key, err)
	}
	return nil
}

func (r *Repository) SetSetting(ctx context.Context, tenantID int64, key string, value any, isSecret bool, actorID *int64) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encoding setting %s: %w", key, err)
	}

	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		var old sql.NullString
		_ = tx.GetContext(ctx, &old,
			`SELECT value_json FROM tenant_settings WHERE tenant_id = ? AND setting_key = ?`,
			tenantID, key)

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tenant_settings (tenant_id, setting_key, value_json, is_secret, updated_by)
			VALUES (?,?,?,?,?)
			ON DUPLICATE KEY UPDATE
				value_json = VALUES(value_json),
				is_secret  = VALUES(is_secret),
				updated_by = VALUES(updated_by)`,
			tenantID, key, string(raw), isSecret, actorID); err != nil {
			return fmt.Errorf("saving setting %s: %w", key, err)
		}

		// Every configuration change keeps its prior value, with the actor.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tenant_settings_history (tenant_id, setting_key, old_value, new_value, actor_id)
			VALUES (?,?,?,?,?)`,
			tenantID, key, old, string(raw), actorID); err != nil {
			return fmt.Errorf("recording setting history: %w", err)
		}
		return nil
	})
}

// --- domains ----------------------------------------------------------------

func (r *Repository) Domains(ctx context.Context, tenantID int64) ([]struct {
	ID          int64  `db:"id" json:"-"`
	Domain      string `db:"domain" json:"domain"`
	IsPrimary   bool   `db:"is_primary" json:"is_primary"`
	SSLVerified bool   `db:"ssl_verified" json:"ssl_verified"`
}, error) {
	rows := []struct {
		ID          int64  `db:"id" json:"-"`
		Domain      string `db:"domain" json:"domain"`
		IsPrimary   bool   `db:"is_primary" json:"is_primary"`
		SSLVerified bool   `db:"ssl_verified" json:"ssl_verified"`
	}{}
	err := r.db.Primary.SelectContext(ctx, &rows,
		`SELECT id, domain, is_primary, ssl_verified FROM tenant_domains WHERE tenant_id = ? ORDER BY is_primary DESC, domain`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("loading domains: %w", err)
	}
	return rows, nil
}

func (r *Repository) AddDomain(ctx context.Context, tenantID int64, domain string, isPrimary bool) error {
	_, err := r.db.Primary.ExecContext(ctx,
		`INSERT INTO tenant_domains (tenant_id, domain, is_primary) VALUES (?,?,?)`,
		tenantID, strings.ToLower(domain), isPrimary)
	if err != nil {
		if platform.IsDuplicate(err) {
			return platform.ErrSentinelConflict
		}
		return fmt.Errorf("adding domain: %w", err)
	}
	return nil
}

func (r *Repository) RemoveDomain(ctx context.Context, tenantID int64, domain string) error {
	res, err := r.db.Primary.ExecContext(ctx,
		`DELETE FROM tenant_domains WHERE tenant_id = ? AND domain = ?`, tenantID, strings.ToLower(domain))
	if err != nil {
		return fmt.Errorf("removing domain: %w", err)
	}
	return requireAffected(res)
}

// --- ticket-prefix history --------------------------------------------------

// PrefixHistoryEntry is one recorded change to a client's ticket prefix: what
// it changed from, to, and who did it. Rendered in the client edit page, where
// the requirement that the prefix is editable but every change is attributed
// is enforced by this log rather than by making the field read-only.
type PrefixHistoryEntry struct {
	ID            int64     `db:"id"            json:"id"`
	OldPrefix     string    `db:"old_prefix"    json:"old_prefix"`
	NewPrefix     string    `db:"new_prefix"    json:"new_prefix"`
	ChangedByID   int64     `db:"changed_by"    json:"changed_by_id"`
	ChangedByName string    `db:"changed_by_name" json:"changed_by_name,omitempty"`
	CreatedAt     time.Time `db:"created_at"    json:"created_at"`
}

// RecordPrefixChange appends one row to a client's prefix history.
func (r *Repository) RecordPrefixChange(ctx context.Context, tenantID int64, oldPrefix, newPrefix string, changedBy *int64) error {
	if oldPrefix == newPrefix {
		return nil
	}
	_, err := r.db.Primary.ExecContext(ctx, `
		INSERT INTO tenant_prefix_history (tenant_id, old_prefix, new_prefix, changed_by)
		VALUES (?,?,?,?)`, tenantID, oldPrefix, newPrefix, changedBy)
	if err != nil {
		return fmt.Errorf("recording ticket prefix change: %w", err)
	}
	return nil
}

// PrefixHistory returns every recorded prefix change for a client, newest first.
func (r *Repository) PrefixHistory(ctx context.Context, tenantID int64) ([]PrefixHistoryEntry, error) {
	rows := []PrefixHistoryEntry{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT h.id, h.old_prefix, h.new_prefix, h.changed_by,
		       CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS changed_by_name,
		       h.created_at
		FROM tenant_prefix_history h
		LEFT JOIN users u ON u.id = h.changed_by
		WHERE h.tenant_id = ?
		ORDER BY h.created_at DESC, h.id DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("loading prefix history: %w", err)
	}
	return rows, nil
}

// --- helpers ----------------------------------------------------------------

func nullString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func defaultTo(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func requireAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading affected rows: %w", err)
	}
	if n == 0 {
		return platform.ErrSentinelNotFound
	}
	return nil
}
