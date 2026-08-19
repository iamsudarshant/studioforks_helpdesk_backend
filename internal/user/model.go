// Package user owns user records, groups, roles, scopes, bulk import and the
// bulk group-move workflow.
package user

import (
	"database/sql"
	"strings"
	"time"
)

const (
	StatusActive     = "ACTIVE"
	StatusInactive   = "INACTIVE"
	StatusLocked     = "LOCKED"
	StatusExEmployee = "EX_EMPLOYEE"
)

// The six roles ComplyDesk has.
//
// ComplyDesk's own staff, who live in the platform tenant and work across every
// client:
//
//	SUPER_ADMIN         complete system access, including the settings that
//	                    affect a client's availability
//	HELPDESK_HEAD       runs the desk: assigns, transfers, escalates, closes,
//	                    and sees operational reporting for every client
//	HELPDESK_EXECUTIVE  works tickets: responds, requests information, uploads
//	                    documents, updates status, adds internal notes
//
// Client-side roles, each confined to their own workspace:
//
//	CLIENT_ADMIN        sees every entity and location allocated to them:
//	                    dashboards, search, SLA monitoring, report downloads.
//	                    Replying is OFF unless `ticket.reply.public` is granted
//	                    to the individual user.
//	CLIENT_EXECUTIVE    segmented to their own entity or location. Raises
//	                    tickets on behalf of employees; cannot edit responses.
//	EMPLOYEE            end user. Raises, replies to, reopens and closes their
//	                    own tickets.
//
// An ex-employee is NOT a role. It is a user status (EX_EMPLOYEE) plus the
// read-only EX_EMPLOYEES group, so someone who leaves keeps their history and
// their identity without being re-roled — and read-only is enforced in one
// place rather than duplicated across a parallel role.
const (
	RoleSuperAdmin        = "SUPER_ADMIN"
	RoleHelpdeskHead      = "HELPDESK_HEAD"
	RoleHelpdeskExecutive = "HELPDESK_EXECUTIVE"
	RoleClientAdmin       = "CLIENT_ADMIN"
	RoleClientExecutive   = "CLIENT_EXECUTIVE"
	RoleEmployee          = "EMPLOYEE"
)

// Deprecated role keys, retained so existing tokens, saved filters and
// integrations keep working. `roles.alias_of` maps each to its canonical role
// and the permission resolver follows it, so a user on an old key behaves
// exactly like one on its replacement.
const (
	RoleAgent               = "AGENT"
	RolePartner             = "PARTNER"
	RoleKarmaSuperAdmin     = "KARMA_SUPER_ADMIN"
	RoleKarmaAgent          = "KARMA_AGENT"
	RolePartnerExecutive    = "PARTNER_EXECUTIVE"
	RoleHelpdeskMasterAdmin = "HELPDESK_MASTER_ADMIN"
	RoleHelpdeskAdmin       = "HELPDESK_ADMIN"
	RoleClientMasterAdmin   = "CLIENT_MASTER_ADMIN"
	RoleEntityAdmin         = "ENTITY_ADMIN"
	RoleDepartmentAdmin     = "DEPARTMENT_ADMIN"
)

// StaffRoles are ComplyDesk's own roles. A user holding one lives in the
// platform tenant and works across clients rather than inside a single
// workspace.
var StaffRoles = map[string]bool{
	RoleSuperAdmin:          true,
	RoleHelpdeskHead:        true,
	RoleHelpdeskExecutive:   true,
	RoleAgent:               true,
	RoleKarmaSuperAdmin:     true,
	RoleKarmaAgent:          true,
	RoleHelpdeskMasterAdmin: true,
	RoleHelpdeskAdmin:       true,
}

// IsStaffRole reports whether a role key belongs to ComplyDesk's own staff,
// under either its canonical or a deprecated name.
func IsStaffRole(key string) bool { return StaffRoles[key] }

// IsSuperAdminRole reports whether a role key grants unrestricted control —
// including the settings an agent may not touch.
func IsSuperAdminRole(key string) bool {
	return key == RoleSuperAdmin || key == RoleKarmaSuperAdmin || key == RoleHelpdeskMasterAdmin
}

// roleRank orders the roles by authority, so "may this person administer that
// one?" has a single answer.
//
// The numbers are spaced rather than consecutive: a role inserted later slots
// between two existing ones without renumbering the ladder, and the gaps carry
// no meaning beyond order.
//
// Deprecated keys are ranked alongside their replacements, because a user still
// holding one is exactly as powerful as a user on the current key.
var roleRank = map[string]int{
	RoleSuperAdmin:          100,
	RoleKarmaSuperAdmin:     100,
	RoleHelpdeskMasterAdmin: 100,

	RoleHelpdeskHead:  80,
	RoleHelpdeskAdmin: 80,

	RoleHelpdeskExecutive: 60,
	RoleAgent:             60,
	RoleKarmaAgent:        60,

	RoleClientAdmin:       40,
	RolePartner:           40,
	RoleClientMasterAdmin: 40,

	RoleClientExecutive:  20,
	RolePartnerExecutive: 20,
	RoleEntityAdmin:      20,
	RoleDepartmentAdmin:  20,

	RoleEmployee: 10,
}

// RankOf is the authority of the strongest role a person holds.
//
// Someone with no recognised role ranks 0, which is below every real role — so
// they can be administered but can administer nobody. That is the safe way for
// an unknown key to fail.
func RankOf(roleKeys []string) int {
	best := 0
	for _, key := range roleKeys {
		if rank, ok := roleRank[key]; ok && rank > best {
			best = rank
		}
	}
	return best
}

// CanAdminister reports whether the actor outranks the target.
//
// Strictly greater, so a peer cannot act on a peer: two helpdesk executives
// resetting each other's passwords is not administration, it is a way to lock a
// colleague out. Nobody can act on themselves through an administrative route
// either — changing your own password has its own screen, which asks for the
// current one.
func CanAdminister(actorRoles, targetRoles []string) bool {
	return RankOf(actorRoles) > RankOf(targetRoles)
}

const (
	GroupActiveEmployees = "ACTIVE_EMPLOYEES"
	GroupExEmployees     = "EX_EMPLOYEES"
)

// Scope dimensions.
const (
	ScopeEntity     = "ENTITY"
	ScopeSite       = "SITE"
	ScopeDepartment = "DEPARTMENT"
	ScopeCategory   = "CATEGORY"
)

// User is the database row.
type User struct {
	ID             int64          `db:"id"`
	PublicID       string         `db:"public_id"`
	TenantID       int64          `db:"tenant_id"`
	EmployeeCode   sql.NullString `db:"employee_code"`
	Username       sql.NullString `db:"username"`
	FirstName      string         `db:"first_name"`
	LastName       sql.NullString `db:"last_name"`
	Email          sql.NullString `db:"email"`
	AltEmail       sql.NullString `db:"alt_email"`
	Mobile         sql.NullString `db:"mobile"`
	AltMobile      sql.NullString `db:"alt_mobile"`
	PANNumber      sql.NullString `db:"pan_number"`
	UANNumber      sql.NullString `db:"uan_number"`
	PFNumber       sql.NullString `db:"pf_number"`
	ESICNumber     sql.NullString `db:"esic_number"`
	DateOfJoining  sql.NullTime   `db:"date_of_joining"`
	DateOfBirth    sql.NullTime   `db:"date_of_birth"`
	LastWorkingDay sql.NullTime   `db:"last_working_day"`
	EntityID       sql.NullInt64  `db:"entity_id"`
	SiteID         sql.NullInt64  `db:"site_id"`
	DepartmentID   sql.NullInt64  `db:"department_id"`
	Designation    sql.NullString `db:"designation"`
	UserGroupID    sql.NullInt64  `db:"user_group_id"`
	// The agent responsible for this person's queries. Set when an employee is
	// moved between Active and Ex-Employee, so a departure never leaves their
	// open work without an owner. See MIGRATION 000020.
	HandlingAgentID     sql.NullInt64  `db:"handling_agent_id"`
	EmploymentChangedAt sql.NullTime   `db:"employment_changed_at"`
	EmploymentChangedBy sql.NullInt64  `db:"employment_changed_by"`
	Status              string         `db:"status"`
	PasswordHash        sql.NullString `db:"password_hash"`
	PasswordAlgo        string         `db:"password_algo"`
	MustChangePassword  bool           `db:"must_change_password"`
	PasswordChangedAt   sql.NullTime   `db:"password_changed_at"`
	PasswordExpiresAt   sql.NullTime   `db:"password_expires_at"`
	FailedLoginCount    int            `db:"failed_login_count"`
	LockedUntil         sql.NullTime   `db:"locked_until"`
	MFAEnabled          bool           `db:"mfa_enabled"`
	MFASecretEnc        []byte         `db:"mfa_secret_enc"`
	MFARecoveryJSON     sql.NullString `db:"mfa_recovery_json"`
	LastLoginAt         sql.NullTime   `db:"last_login_at"`
	LoginCount          int            `db:"login_count"`
	AvatarPath          sql.NullString `db:"avatar_path"`
	Locale              sql.NullString `db:"locale"`
	Timezone            sql.NullString `db:"timezone"`
	CustomFieldsJSON    sql.NullString `db:"custom_fields_json"`
	CreatedBy           sql.NullInt64  `db:"created_by"`
	UpdatedBy           sql.NullInt64  `db:"updated_by"`
	CreatedAt           time.Time      `db:"created_at"`
	UpdatedAt           time.Time      `db:"updated_at"`
	DeletedAt           sql.NullTime   `db:"deleted_at"`

	// Joined on the list query only. A cross-client roster has to name whose
	// employee each row is, and the sections need the roles and the posting to
	// label a person without a follow-up request per row. Every one is empty on
	// a single-record load, which never joins.
	ClientName sql.NullString `db:"client_name"`
	ClientSlug sql.NullString `db:"client_slug"`
	ClientCode sql.NullString `db:"client_code"`
	RoleNames  sql.NullString `db:"role_names"`

	// Who handles this person's queries, joined so the roster can show it
	// beside the status without a request per row.
	HandlingAgentPublicID sql.NullString `db:"handling_agent_public_id"`
	HandlingAgentName     sql.NullString `db:"handling_agent_name"`

	EntityPublicID     sql.NullString `db:"entity_public_id"`
	EntityCode         sql.NullString `db:"entity_code"`
	EntityName         sql.NullString `db:"entity_name"`
	SitePublicID       sql.NullString `db:"site_public_id"`
	SiteCode           sql.NullString `db:"site_code"`
	SiteName           sql.NullString `db:"site_name"`
	DepartmentPublicID sql.NullString `db:"department_public_id"`
	DepartmentCode     sql.NullString `db:"department_code"`
	DepartmentName     sql.NullString `db:"department_name"`
	GroupPublicID      sql.NullString `db:"group_public_id"`
	GroupKey           sql.NullString `db:"group_key"`
	GroupName          sql.NullString `db:"group_name"`
}

// FullName renders the display name used in snapshots and notifications.
func (u *User) FullName() string {
	if u.LastName.Valid && strings.TrimSpace(u.LastName.String) != "" {
		return strings.TrimSpace(u.FirstName + " " + u.LastName.String)
	}
	return strings.TrimSpace(u.FirstName)
}

// PreferredEmail returns the official address, falling back to the alternate.
// Recovery mail is sent to both when both exist.
func (u *User) PreferredEmail() string {
	if u.Email.Valid && strings.TrimSpace(u.Email.String) != "" {
		return u.Email.String
	}
	return strings.TrimSpace(u.AltEmail.String)
}

// LoginName is the username the account-recovery mail quotes back to the user.
func (u *User) LoginName() string {
	switch {
	case u.Username.Valid && u.Username.String != "":
		return u.Username.String
	case u.Email.Valid && u.Email.String != "":
		return u.Email.String
	case u.EmployeeCode.Valid && u.EmployeeCode.String != "":
		return u.EmployeeCode.String
	default:
		return u.PublicID
	}
}

// Group is a user group; EX_EMPLOYEES is the system group the bulk move targets.
type Group struct {
	ID              int64          `db:"id"`
	PublicID        string         `db:"public_id"`
	TenantID        int64          `db:"tenant_id"`
	Key             string         `db:"group_key"`
	Name            string         `db:"name"`
	Description     sql.NullString `db:"description"`
	IsSystem        bool           `db:"is_system"`
	AccessMode      string         `db:"access_mode"`
	GracePeriodDays int            `db:"grace_period_days"`
	SLAPolicyID     sql.NullInt64  `db:"sla_policy_id"`
	CreatedAt       time.Time      `db:"created_at"`
	UpdatedAt       time.Time      `db:"updated_at"`

	// Joined on the list. A group belongs to one client, so a cross-client list
	// has to name whose it is; the headcount is what makes the row actionable.
	ClientName sql.NullString `db:"client_name"`
	ClientSlug sql.NullString `db:"client_slug"`
	ClientCode sql.NullString `db:"client_code"`
	UserCount  int64          `db:"user_count"`
}

type Role struct {
	ID          int64          `db:"id"`
	PublicID    string         `db:"public_id"`
	TenantID    sql.NullInt64  `db:"tenant_id"`
	Key         string         `db:"role_key"`
	Name        string         `db:"name"`
	Description sql.NullString `db:"description"`
	Portal      string         `db:"portal"`
	IsSystem    bool           `db:"is_system"`
	CreatedAt   time.Time      `db:"created_at"`
	UpdatedAt   time.Time      `db:"updated_at"`
}

type Permission struct {
	Key         string `db:"permission_key" json:"key"`
	Group       string `db:"permission_group" json:"group"`
	Description string `db:"description" json:"description"`
}

type Scope struct {
	ScopeType string `db:"scope_type"`
	ScopeID   int64  `db:"scope_id"`
}

// Reference is the {id,name} shape the API returns for related records.
type Reference struct {
	ID   string `json:"id"`
	Code string `json:"code,omitempty"`
	Name string `json:"name"`
}
