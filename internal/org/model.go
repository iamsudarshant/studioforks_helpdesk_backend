package org

import (
	"database/sql"
	"time"
)

// Entity is a legally registered establishment of the client — the unit that
// actually holds the statutory registrations. A client typically has many, each
// registered for a different mix of schemes: one company may be registered with
// EPFO but not ESIC, another with both.
//
// That mix is what drives ticket creation: choosing PF on the form lists only
// the entities with an active PF registration (see Registration).
//
// Entities are also the primary access-scoping dimension — a partner or agent
// assigned to specific entities sees only their users and tickets.
type Entity struct {
	ID       int64          `db:"id"`
	PublicID string         `db:"public_id"`
	TenantID int64          `db:"tenant_id"`
	Code     string         `db:"code"`
	Name     string         `db:"name"`
	Type     sql.NullString `db:"type"`
	// DepartmentID is mandatory on create and every update: an entity is
	// routed to a statutory line (PF, ESIC, General) through its department.
	DepartmentID       sql.NullInt64  `db:"department_id"`
	DepartmentPublicID sql.NullString `db:"department_public_id"`
	DepartmentName     sql.NullString `db:"department_name"`
	// TemplateKey is set when the entity was created from a default template,
	// so the UI can separate "standard" from "added by you".
	TemplateKey       sql.NullString `db:"template_key"`
	IsDefault         bool           `db:"is_default"`
	ParentEntityID    sql.NullInt64  `db:"parent_entity_id"`
	Address           sql.NullString `db:"address"`
	RegisteredAddress sql.NullString `db:"registered_address"`
	CINNumber         sql.NullString `db:"cin_number"`
	GSTNumber         sql.NullString `db:"gst_number"`
	IsActive          bool           `db:"is_active"`
	// OptedOutAt records a client deliberately switching a default entity off,
	// which is different from an entity that was never applicable.
	OptedOutAt sql.NullTime  `db:"opted_out_at"`
	OptedOutBy sql.NullInt64 `db:"opted_out_by"`
	CreatedAt  time.Time     `db:"created_at"`
	UpdatedAt  time.Time     `db:"updated_at"`

	// Client attribution, joined on every listing. Only a cross-client list
	// renders it; a single-client screen already knows whose records it shows.
	ClientName sql.NullString `db:"client_name"`
	ClientSlug sql.NullString `db:"client_slug"`
	ClientCode sql.NullString `db:"client_code"`
}

// Registration links an entity to a query category it is registered for, and
// carries the statutory number for that scheme: the EPFO establishment code for
// a PF category, the ESIC code for ESI. Categories with no statutory number
// (IT, HR) simply leave RegistrationNumber null.
type Registration struct {
	ID                 int64          `db:"id"`
	PublicID           string         `db:"public_id"`
	TenantID           int64          `db:"tenant_id"`
	EntityID           int64          `db:"entity_id"`
	CategoryID         int64          `db:"category_id"`
	RegistrationNumber sql.NullString `db:"registration_number"`
	RegisteredOn       sql.NullTime   `db:"registered_on"`
	ValidUntil         sql.NullTime   `db:"valid_until"`
	Notes              sql.NullString `db:"notes"`
	IsActive           bool           `db:"is_active"`
	CreatedBy          sql.NullInt64  `db:"created_by"`
	CreatedAt          time.Time      `db:"created_at"`
	UpdatedAt          time.Time      `db:"updated_at"`

	// Joined for display.
	CategoryKey  string `db:"category_key"`
	CategoryName string `db:"category_name"`
	EntityCode   string `db:"entity_code"`
	EntityName   string `db:"entity_name"`
}

// Template is a platform-level default entity applied when a client is created.
// A client keeps the ones that apply and opts out of the rest, which is why
// every client ends up with a different entity set.
type Template struct {
	ID                    int64          `db:"id"`
	PublicID              string         `db:"public_id"`
	Key                   string         `db:"template_key"`
	Name                  string         `db:"name"`
	Description           sql.NullString `db:"description"`
	EntityType            sql.NullString `db:"entity_type"`
	DefaultCategoriesJSON sql.NullString `db:"default_categories_json"`
	IsActive              bool           `db:"is_active"`
	SortOrder             int            `db:"sort_order"`
	CreatedAt             time.Time      `db:"created_at"`
	UpdatedAt             time.Time      `db:"updated_at"`
}

// Site is a client location — Mumbai, Pune, Delhi. It belongs to the client, so
// that a ticket shows which office the employee who raised it works from.
//
// EntityID is optional on purpose: a client may run locations that are not
// mapped to any registered entity, and a client may have no sites at all.
// Partner and agent users can be assigned to specific sites, which then bounds
// everything they can act on.
type Site struct {
	ID        int64          `db:"id"`
	PublicID  string         `db:"public_id"`
	TenantID  int64          `db:"tenant_id"`
	EntityID  sql.NullInt64  `db:"entity_id"`
	Code      string         `db:"code"`
	Name      string         `db:"name"`
	Address   sql.NullString `db:"address"`
	City      sql.NullString `db:"city"`
	State     sql.NullString `db:"state"`
	Pincode   sql.NullString `db:"pincode"`
	IsActive  bool           `db:"is_active"`
	IsDefault bool           `db:"is_default"`
	CreatedAt time.Time      `db:"created_at"`
	UpdatedAt time.Time      `db:"updated_at"`

	// Client attribution, joined on every listing. Only a cross-client list
	// renders it; a single-client screen already knows whose records it shows.
	ClientName sql.NullString `db:"client_name"`
	ClientSlug sql.NullString `db:"client_slug"`
	ClientCode sql.NullString `db:"client_code"`
}

// Department is the internal team a ticket can be routed to or pended on.
//
// Type is a standard statutory line — PF, ESIC, GENERAL — so agents can be
// mapped to "all departments" or a specific line, and entity routing has a
// stable grouping to work from.
type Department struct {
	ID         int64         `db:"id"`
	PublicID   string        `db:"public_id"`
	TenantID   int64         `db:"tenant_id"`
	Code       string        `db:"code"`
	Name       string        `db:"name"`
	Type       string        `db:"type"`
	HeadUserID sql.NullInt64 `db:"head_user_id"`
	IsActive   bool          `db:"is_active"`
	CreatedAt  time.Time     `db:"created_at"`
	UpdatedAt  time.Time     `db:"updated_at"`

	// EntityCount is how many establishments hang off this department. Joined on
	// the list so an administrator can see which lines are actually in use, and
	// so deleting one can warn about what it would orphan.
	EntityCount int `db:"entity_count"`

	// Client attribution, joined on every listing. Only a cross-client list
	// renders it; a single-client screen already knows whose records it shows.
	ClientName sql.NullString `db:"client_name"`
	ClientSlug sql.NullString `db:"client_slug"`
	ClientCode sql.NullString `db:"client_code"`
}
