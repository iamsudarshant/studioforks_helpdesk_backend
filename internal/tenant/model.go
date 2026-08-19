// Package tenant owns tenant records, branding, feature flags, settings
// and maintenance windows.
package tenant

import (
	"database/sql"
	"time"
)

// Status values for tenants.
//
// ARCHIVED is where a deleted client goes. Deletion is never destructive: the
// row, its users, tickets and documents all remain, and an administrator can
// restore it. What changes is that the workspace stops appearing in lists and
// nobody can sign into it.
// A client is live from the moment it is created; there is no provisioning
// state to pass through first. The old ONBOARDING value was removed in
// migration 000016 along with the wizard that set it.
const (
	StatusActive    = "ACTIVE"
	StatusSuspended = "SUSPENDED"
	StatusArchived  = "ARCHIVED"
)

// DeletableStatuses are the states a client may be deleted from.
//
// A live client cannot be deleted in one step: it has to be suspended first.
// The reason is that deletion signs out everyone in the workspace and hides
// their tickets, and doing that to a client who is mid-conversation with the
// helpdesk — from a row in a list, behind one confirmation — is too easy a
// mistake to make.
//
// A client outside this set is not refused: deleteTenant suspends it on the way
// out, so the sign-out still happens deliberately and is still audited. What
// this set now decides is only whether the confirmation has to spell that out.
var DeletableStatuses = []string{StatusSuspended, StatusArchived}

// CanDelete reports whether a client in this state may be deleted without first
// being taken offline.
func CanDelete(status string) bool {
	for _, s := range DeletableStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// Tenant is the client master row.
//
// A client IS the workspace: "Ampersand Group" is one row here, with its own
// slug, branding, users, entities and sites. The `tenant` name is retained
// internally because it is what every foreign key and index is called; the API
// and UI present it as "Client".
type Tenant struct {
	ID         int64          `db:"id"`
	PublicID   string         `db:"public_id"`
	Slug       string         `db:"slug"`
	ClientCode sql.NullString `db:"client_code"`
	Name       string         `db:"name"`
	LegalName  sql.NullString `db:"legal_name"`
	Industry   sql.NullString `db:"industry"`
	Status     string         `db:"status"`
	// IsPlatform marks Karma's own tenant rather than a client. It drives the
	// branding rule and keeps Karma out of client listings.
	IsPlatform   bool           `db:"is_platform"`
	Timezone     string         `db:"timezone"`
	Locale       string         `db:"locale"`
	DateFormat   string         `db:"date_format"`
	TicketPrefix string         `db:"ticket_prefix"`
	ContactEmail sql.NullString `db:"contact_email"`
	AltEmail     sql.NullString `db:"alt_email"`
	ContactPhone sql.NullString `db:"contact_phone"`
	AltPhone     sql.NullString `db:"alt_phone"`
	Address      sql.NullString `db:"address"`
	// TaxID holds the GST number; named generically because not every
	// jurisdiction calls it GST.
	TaxID               sql.NullString `db:"tax_id"`
	ContractStart       sql.NullTime   `db:"contract_start"`
	ContractEnd         sql.NullTime   `db:"contract_end"`
	RetentionPolicyJSON sql.NullString `db:"retention_policy_json"`
	OnboardedAt         sql.NullTime   `db:"onboarded_at"`
	AccountManagerID    sql.NullInt64  `db:"account_manager_id"`
	CreatedAt           time.Time      `db:"created_at"`
	UpdatedAt           time.Time      `db:"updated_at"`
	DeletedAt           sql.NullTime   `db:"deleted_at"`
}

// Branding is the per-tenant look and feel served to the frontend at boot.
type Branding struct {
	TenantID           int64          `db:"tenant_id"`
	LogoPath           sql.NullString `db:"logo_path"`
	LogoDarkPath       sql.NullString `db:"logo_dark_path"`
	FaviconPath        sql.NullString `db:"favicon_path"`
	LoginBgPath        sql.NullString `db:"login_bg_path"`
	EmailHeaderPath    sql.NullString `db:"email_header_path"`
	PrimaryColor       string         `db:"primary_color"`
	SecondaryColor     string         `db:"secondary_color"`
	AccentColor        string         `db:"accent_color"`
	ShowComplyDeskLogo bool           `db:"show_complydesk_logo"`
	CustomCSS          sql.NullString `db:"custom_css"`
	UpdatedAt          time.Time      `db:"updated_at"`
}

// Feature keys recognised by the platform. Unknown keys are permitted so a
// tenant can carry its own flags, but these have behaviour attached.
const (
	FeatureSMS             = "sms"
	FeatureMFA             = "mfa"
	FeatureSSO             = "sso"
	FeatureCSAT            = "csat"
	FeatureWatermark       = "watermark"
	FeatureCustomReports   = "custom_reports"
	FeatureClientReply     = "client_reply"
	FeatureEmailToTicket   = "email_to_ticket"
	FeatureOTPLogin        = "otp_login"
	FeatureBulkImport      = "bulk_import"
	FeatureDocumentPreview = "document_preview"
)

// DefaultFeatures is what a new tenant starts with.
func DefaultFeatures() map[string]bool {
	return map[string]bool{
		FeatureSMS: false,
		// On by default. Two-factor is a security control every workspace
		// should be able to switch on for itself, and shipping it off meant the
		// whole enrolment surface — Profile → Two-Factor Authentication — was
		// present, reachable, and answered "not enabled for this workspace" on
		// the first click. An administrator who does not want it can still turn
		// it off; nobody has to ask for the option to exist.
		FeatureMFA:           true,
		FeatureSSO:           false,
		FeatureCSAT:          true,
		FeatureWatermark:     false,
		FeatureCustomReports: true,
		FeatureClientReply:   false,
		FeatureEmailToTicket: false,
		// §3.1 makes one-time-password sign-in part of the employee portal:
		// ten thousand employees per client will not reliably remember a
		// password, and the alternate identifiers exist for the same reason.
		FeatureOTPLogin:        true,
		FeatureBulkImport:      true,
		FeatureDocumentPreview: true,
	}
}

// Setting keys with platform meaning. Everything here is editable from the
// admin panel; the constants exist so the code never guesses a key.
const (
	SettingPasswordPolicy      = "password_policy"
	SettingLoginIdentifiers    = "login_identifiers"
	SettingAuthMethods         = "auth_methods"
	SettingSessionPolicy       = "session_policy"
	SettingReopenWindowDays    = "reopen_window_days"
	SettingAutoCloseDays       = "auto_close_days"
	SettingPendingUserDays     = "pending_user_reminder_days"
	SettingTicketNumberFormat  = "ticket_number_format"
	SettingBulkOnboardingMode  = "bulk_onboarding_mode"
	SettingUploadLimits        = "upload_limits"
	SettingRetentionPolicy     = "retention_policy"
	SettingSMTP                = "smtp"
	SettingSMS                 = "sms"
	SettingMandatoryUserFields = "mandatory_user_fields"
	SettingPIIMasking          = "pii_masking"
)

// Bulk onboarding credential modes.
//
// RANDOM is the default. DERIVED reproduces the {pf_number}@{birth_year}
// pattern for workforces with no email, and is deliberately opt-in because the
// value is guessable by anyone holding the employee register. CSV_RETURN hands
// back a one-time expiring credentials file.
const (
	OnboardingModeRandom    = "RANDOM"
	OnboardingModeDerived   = "DERIVED"
	OnboardingModeCSVReturn = "CSV_RETURN"
)

// LoginIdentifier values a tenant may enable for employees.
const (
	IdentifierEmail        = "email"
	IdentifierAltEmail     = "alt_email"
	IdentifierEmployeeCode = "employee_code"
	IdentifierPFNumber     = "pf_number"
	IdentifierUANNumber    = "uan_number"
	IdentifierPANNumber    = "pan_number"
	IdentifierMobile       = "mobile"
	IdentifierUsername     = "username"
)

// DefaultLoginIdentifiers reflects the brief: employees may sign in with PF,
// PAN, email or a tenant-defined username, among others.
func DefaultLoginIdentifiers() []string {
	return []string{
		IdentifierEmail, IdentifierAltEmail, IdentifierEmployeeCode,
		IdentifierPFNumber, IdentifierUANNumber, IdentifierPANNumber,
		IdentifierMobile, IdentifierUsername,
	}
}

// StaffLoginIdentifiers is the restricted set for every non-employee role.
func StaffLoginIdentifiers() []string {
	return []string{IdentifierEmail, IdentifierAltEmail}
}

// Setting is a stored configuration row.
type Setting struct {
	ID        int64     `db:"id"`
	TenantID  int64     `db:"tenant_id"`
	Key       string    `db:"setting_key"`
	ValueJSON string    `db:"value_json"`
	IsSecret  bool      `db:"is_secret"`
	UpdatedAt time.Time `db:"updated_at"`
}

// MaintenanceWindow is a scheduled or active maintenance period.
type MaintenanceWindow struct {
	ID             int64          `db:"id"`
	PublicID       string         `db:"public_id"`
	Scope          string         `db:"scope"`
	TenantID       sql.NullInt64  `db:"tenant_id"`
	Mode           string         `db:"mode"`
	Title          string         `db:"title"`
	Message        sql.NullString `db:"message"`
	StartsAt       time.Time      `db:"starts_at"`
	EndsAt         sql.NullTime   `db:"ends_at"`
	IsActive       bool           `db:"is_active"`
	AllowRolesJSON sql.NullString `db:"allow_roles_json"`
	CreatedBy      sql.NullInt64  `db:"created_by"`
	CreatedAt      time.Time      `db:"created_at"`
	UpdatedAt      time.Time      `db:"updated_at"`
}

const (
	ScopeGlobal = "GLOBAL"
	ScopeTenant = "TENANT"

	ModeBanner  = "BANNER"
	ModeLockout = "LOCKOUT"
)
