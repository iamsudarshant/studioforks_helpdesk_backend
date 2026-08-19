package user

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

// Assignment grants a Karma agent access to one client.
//
// This is the mechanism behind "cannot access tenants not assigned": an agent's
// reach is the set of live rows here, and every tenant-scoped request from an
// agent is checked against it. A Karma Super Admin has no rows and needs none —
// their access is unrestricted by role.
type Assignment struct {
	ID         int64      `db:"id"`
	PublicID   string     `db:"public_id"`
	AgentID    int64      `db:"agent_user_id"`
	TenantID   int64      `db:"tenant_id"`
	IsPrimary  bool       `db:"is_primary"`
	AssignedAt time.Time  `db:"assigned_at"`
	RevokedAt  *time.Time `db:"revoked_at"`

	// Joined for display.
	TenantPublicID string `db:"tenant_public_id"`
	TenantSlug     string `db:"tenant_slug"`
	TenantName     string `db:"tenant_name"`
	TenantStatus   string `db:"tenant_status"`
	ClientCode     string `db:"client_code"`
	AgentName      string `db:"agent_name"`
	AgentEmail     string `db:"agent_email"`
	// The agent's own public id. Without it a caller could list a client's
	// agents but had no handle to revoke one with.
	AgentPublicID string `db:"agent_public_id"`
}

// AssignedTenantIDs returns the clients an agent may work on. An empty slice
// means the agent has been given nothing yet, which must deny access rather
// than grant it — callers treat nil and empty differently on purpose.
func (r *Repository) AssignedTenantIDs(ctx context.Context, agentID int64) ([]int64, error) {
	ids := []int64{}
	err := r.db.Primary.SelectContext(ctx, &ids, `
		SELECT a.tenant_id
		FROM agent_tenant_assignments a
		JOIN tenants t ON t.id = a.tenant_id
		WHERE a.agent_user_id = ? AND a.revoked_at IS NULL
		  AND t.deleted_at IS NULL AND t.status <> 'SUSPENDED'`, agentID)
	if err != nil {
		return nil, fmt.Errorf("loading agent assignments: %w", err)
	}
	return ids, nil
}

// IsAssigned reports whether an agent may reach a specific client. Used by the
// tenant middleware on every request an internal user makes.
func (r *Repository) IsAssigned(ctx context.Context, agentID, tenantID int64) (bool, error) {
	var count int
	err := r.db.Primary.GetContext(ctx, &count, `
		SELECT COUNT(*) FROM agent_tenant_assignments
		WHERE agent_user_id = ? AND tenant_id = ? AND revoked_at IS NULL`, agentID, tenantID)
	if err != nil {
		return false, fmt.Errorf("checking agent assignment: %w", err)
	}
	return count > 0, nil
}

// AssignedClients lists an agent's clients for the portal's client switcher.
func (r *Repository) AssignedClients(ctx context.Context, agentID int64) ([]Assignment, error) {
	rows := []Assignment{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT a.id, a.public_id, a.agent_user_id, a.tenant_id, a.is_primary,
		       a.assigned_at, a.revoked_at,
		       t.public_id AS tenant_public_id, t.slug AS tenant_slug, t.name AS tenant_name,
		       t.status AS tenant_status,
		       COALESCE(t.client_code, '') AS client_code,
		       '' AS agent_name, '' AS agent_email, '' AS agent_public_id
		FROM agent_tenant_assignments a
		JOIN tenants t ON t.id = a.tenant_id
		WHERE a.agent_user_id = ? AND a.revoked_at IS NULL
		  AND t.deleted_at IS NULL
		ORDER BY a.is_primary DESC, t.name`, agentID)
	if err != nil {
		return nil, fmt.Errorf("listing assigned clients: %w", err)
	}
	return rows, nil
}

// AgentsForClient lists the agents assigned to one client.
func (r *Repository) AgentsForClient(ctx context.Context, tenantID int64) ([]Assignment, error) {
	rows := []Assignment{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT a.id, a.public_id, a.agent_user_id, a.tenant_id, a.is_primary,
		       a.assigned_at, a.revoked_at,
		       '' AS tenant_public_id, '' AS tenant_slug, '' AS tenant_name,
		       '' AS tenant_status, '' AS client_code,
		       CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS agent_name,
		       COALESCE(u.email, '') AS agent_email,
		       u.public_id AS agent_public_id
		FROM agent_tenant_assignments a
		JOIN users u ON u.id = a.agent_user_id
		WHERE a.tenant_id = ? AND a.revoked_at IS NULL AND u.deleted_at IS NULL
		ORDER BY a.is_primary DESC, u.first_name`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("listing client agents: %w", err)
	}
	return rows, nil
}

// AssignAgent grants an agent access to a client. Re-assigning a previously
// revoked pairing reinstates it rather than failing on the unique index.
func (r *Repository) AssignAgent(ctx context.Context, agentID, tenantID int64, isPrimary bool, assignedBy *int64) error {
	return r.db.InTx(ctx, func(tx *sqlx.Tx) error {
		// Only one primary agent per client, so demote the incumbent first.
		if isPrimary {
			if _, err := tx.ExecContext(ctx,
				`UPDATE agent_tenant_assignments SET is_primary = 0
				 WHERE tenant_id = ? AND revoked_at IS NULL`, tenantID); err != nil {
				return fmt.Errorf("demoting the previous primary agent: %w", err)
			}
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO agent_tenant_assignments
				(public_id, agent_user_id, tenant_id, is_primary, assigned_by)
			VALUES (?,?,?,?,?)
			ON DUPLICATE KEY UPDATE
				is_primary = VALUES(is_primary),
				assigned_by = VALUES(assigned_by),
				assigned_at = CURRENT_TIMESTAMP(3),
				revoked_at = NULL`,
			platform.NewULID(), agentID, tenantID, isPrimary, assignedBy); err != nil {
			return fmt.Errorf("assigning agent: %w", err)
		}
		return nil
	})
}

// RevokeAgent withdraws an agent's access to a client. The row is kept rather
// than deleted so the audit trail retains who had access and when.
func (r *Repository) RevokeAgent(ctx context.Context, agentID, tenantID int64) error {
	res, err := r.db.Primary.ExecContext(ctx, `
		UPDATE agent_tenant_assignments SET revoked_at = UTC_TIMESTAMP(3)
		WHERE agent_user_id = ? AND tenant_id = ? AND revoked_at IS NULL`, agentID, tenantID)
	if err != nil {
		return fmt.Errorf("revoking agent assignment: %w", err)
	}
	return affected(res)
}

// IsPlatformTenant reports whether a tenant is Karma's own rather than a client.
func (r *Repository) IsPlatformTenant(ctx context.Context, tenantID int64) (bool, error) {
	var isPlatform bool
	err := r.db.Primary.GetContext(ctx, &isPlatform,
		`SELECT is_platform FROM tenants WHERE id = ?`, tenantID)
	if err != nil {
		if platform.IsNotFound(err) {
			return false, platform.ErrSentinelNotFound
		}
		return false, fmt.Errorf("checking platform tenant: %w", err)
	}
	return isPlatform, nil
}

// AssignableUser resolves someone a ticket in `clientTenantID` may be assigned
// to.
//
// Two populations qualify, which is why a plain tenant-scoped lookup is wrong:
//   - client-side users belonging to that client, and
//   - Karma staff, who live in the platform tenant and reach the client through
//     an assignment (or, for a super admin, by role).
//
// Anyone else — including a Karma agent not assigned to this client — is not
// found, so a ticket can never be handed to someone who cannot open it.
// staffReachesClient is the one rule for "may this helpdesk person work this
// client's tickets", as a SQL predicate over an aliased `users u`.
//
// It expands to two `?` placeholders, both the client's tenant id.
//
// Written once because it was previously written twice and the two disagreed:
// the picker offered every member of staff, while the assign path accepted only
// a hardcoded list of role keys — a list naming `KARMA_SUPER_ADMIN` and
// `HELPDESK_MASTER_ADMIN`, neither of which exists any more. A Helpdesk
// Executive was therefore offered on every ticket and refused on most of them,
// with "that user cannot be assigned to this ticket" and nothing to act on.
//
// The rule matches `appctx.Reach`, which is what decides the same question
// everywhere else: a super admin reaches every client, an agent with no
// assignments reaches every client, and an agent with assignments reaches
// exactly those.
const staffReachesClient = `(
	EXISTS (
		SELECT 1 FROM user_roles ur2 JOIN roles ro2 ON ro2.id = ur2.role_id
		WHERE ur2.user_id = u.id AND ro2.role_key = 'SUPER_ADMIN'
	)
	OR NOT EXISTS (
		SELECT 1 FROM agent_tenant_assignments a2
		WHERE a2.agent_user_id = u.id AND a2.revoked_at IS NULL
	)
	OR EXISTS (
		SELECT 1 FROM agent_tenant_assignments a3
		WHERE a3.agent_user_id = u.id AND a3.tenant_id = ? AND a3.revoked_at IS NULL
	)
)`

func (r *Repository) AssignableUser(ctx context.Context, clientTenantID int64, publicID string) (*User, error) {
	var u User
	err := r.db.Primary.GetContext(ctx, &u, `
		SELECT `+userColumns+`
		FROM users u
		WHERE u.public_id = ? AND u.deleted_at IS NULL AND u.status = 'ACTIVE'
		  AND (
		    -- a user of this client
		    u.tenant_id = ?
		    OR EXISTS (
		      -- helpdesk staff whose remit covers this client
		      SELECT 1
		      FROM tenants kt
		      JOIN user_roles ur ON ur.user_id = u.id
		      JOIN roles ro      ON ro.id = ur.role_id
		      WHERE kt.id = u.tenant_id AND kt.is_platform = 1
		        AND ro.portal IN ('admin', 'agents')
		        AND `+staffReachesClient+`
		    )
		  )
		LIMIT 1`, publicID, clientTenantID, clientTenantID)
	if err != nil {
		if platform.IsNotFound(err) {
			return nil, platform.ErrSentinelNotFound
		}
		return nil, fmt.Errorf("resolving assignable user: %w", err)
	}
	return &u, nil
}

// worksDepartment is the one rule for "may this person be handed a ticket in
// this department", as a SQL predicate over an aliased `users u`.
//
// It expands to four `?` placeholders, in order: the client's tenant id, the
// department id, the department id again, and the client's tenant id again.
// Build the arguments with departmentArgs.
//
// Four ways to qualify, because the desk has four kinds of handler:
//
//   - an explicit department assignment, which is how an agent is allocated to
//     a statutory line;
//   - the user's own posting, which is how a client-side user belongs;
//   - an unrestricted role, because a super admin or a helpdesk head works
//     every line by definition and has no per-department row; and
//   - no department mapping of any kind, which is a generalist rather than
//     someone excluded — the desk's default state before anyone has been
//     allocated, and the one the picker must not treat as "restricted".
//
// The point of the rule is to keep out the agent who is mapped to a *different*
// line — an ESIC ticket must not offer the PF desk — and nobody else. Written
// once, and checked on submit through EligibleForDepartment, because a picker
// that offers a choice the API then refuses is worse than no picker at all.
const worksDepartment = `(
	EXISTS (
		SELECT 1 FROM department_assignments da
		WHERE da.tenant_id = ? AND da.user_id = u.id AND da.department_id = ?
	)
	OR u.department_id = ?
	OR EXISTS (
		SELECT 1 FROM user_roles urd JOIN roles rod ON rod.id = urd.role_id
		WHERE urd.user_id = u.id
		  AND rod.role_key IN ('SUPER_ADMIN', 'HELPDESK_MASTER_ADMIN', 'HELPDESK_HEAD')
	)
	OR (
		u.department_id IS NULL
		AND NOT EXISTS (
			SELECT 1 FROM department_assignments da2
			WHERE da2.tenant_id = ? AND da2.user_id = u.id
		)
	)
)`

// departmentArgs supplies worksDepartment's four placeholders in order.
func departmentArgs(clientTenantID, departmentID int64) []any {
	return []any{clientTenantID, departmentID, departmentID, clientTenantID}
}

// EligibleForDepartment answers, for one person, the question worksDepartment
// asks of a list — so the assign endpoint refuses exactly what the picker
// declines to offer.
func (r *Repository) EligibleForDepartment(ctx context.Context, clientTenantID, departmentID, userID int64) (bool, error) {
	if departmentID == 0 {
		// A ticket with no statutory line to honour restricts nobody.
		return true, nil
	}

	var ok bool
	args := append([]any{userID}, departmentArgs(clientTenantID, departmentID)...)
	if err := r.db.Primary.GetContext(ctx, &ok,
		`SELECT EXISTS (SELECT 1 FROM users u WHERE u.id = ? AND `+worksDepartment+`)`,
		args...); err != nil {
		return false, fmt.Errorf("checking department eligibility: %w", err)
	}
	return ok, nil
}

// AssignableStaffRow is one person a ticket may be handed to.
type AssignableStaffRow struct {
	PublicID string `db:"public_id"`
	Name     string `db:"name"`
	Email    string `db:"email"`
	RoleName string `db:"role_name"`
}

// AssignableStaff lists the helpdesk people a ticket can be assigned to.
//
// Helpdesk staff only. Assigning a ticket to the employee who raised it is not
// something anyone means to do, and offering the whole roster is what made the
// picker unusable. Staff live in the platform workspace rather than inside a
// client, which is why the ordinary user list — filtered to the current client —
// came back empty and the bulk-assign dropdown had nothing in it.
//
// `clientTenantID` narrows to the staff whose remit actually covers that
// client, using the same predicate the assign path checks. Pass 0 for a
// client-agnostic list — the filter bar's "assigned to" options, say, where the
// question is who exists rather than who may take this one ticket.
//
// `departmentID` narrows again, to the statutory line the ticket sits on: the
// people mapped to it, plus the generalists mapped to no line at all. An agent
// allocated to a *different* department is left out, because handing them the
// ticket puts it in a queue they do not work. Pass 0 when the question has no
// department — a bulk action across mixed tickets, or the filter bar.
//
// Narrowing matters because the two used to disagree. Every agent appeared on
// every ticket, and choosing one whose remit did not cover that client was
// refused on submit: an option that exists to be rejected.
func (r *Repository) AssignableStaff(ctx context.Context, clientTenantID, departmentID int64, query string) ([]AssignableStaffRow, error) {
	// Two populations can work a ticket, and the picker used to offer only one.
	//
	//   * ComplyDesk staff, who live in the platform workspace and reach a
	//     client through an assignment; and
	//   * the client's own people who work tickets — a client administrator, and
	//     the partners (client executives) accountable for an entity.
	//
	// Restricting this to `is_platform = 1` meant the assign and transfer
	// pickers listed four members of staff and none of the forty-one partners,
	// so a ticket about an entity could not be handed to the person responsible
	// for it.
	//
	// Employees are still excluded: they raise tickets, they do not work them,
	// and offering the whole roster is what made the picker unusable before.
	where := []string{
		"u.deleted_at IS NULL",
		"u.status = 'ACTIVE'",
		"NOT EXISTS (SELECT 1 FROM user_roles xr JOIN roles xo ON xo.id = xr.role_id" +
			" WHERE xr.user_id = u.id AND xo.role_key = 'EMPLOYEE')",
	}
	args := []any{}

	if clientTenantID != 0 {
		// Staff whose remit covers this client, or the client's own workers.
		where = append(where, `(
			(tn.is_platform = 1 AND ro.portal IN ('admin', 'agents') AND `+staffReachesClient+`)
			OR (u.tenant_id = ? AND ro.portal = 'partner')
		)`)
		args = append(args, clientTenantID, clientTenantID)
	} else {
		// Client-agnostic: who exists at all, for a filter bar's options.
		where = append(where, "(tn.is_platform = 1 AND ro.portal IN ('admin', 'agents')) OR ro.portal = 'partner'")
	}

	// The department gate, and the same predicate EligibleForDepartment checks
	// on submit — so the picker and the API cannot disagree about who is
	// allowed. Scoped by the client's tenant because a department assignment
	// belongs to a client, not to the platform.
	if departmentID != 0 {
		where = append(where, worksDepartment)
		args = append(args, departmentArgs(clientTenantID, departmentID)...)
	}

	if q := strings.TrimSpace(query); q != "" {
		where = append(where,
			"(CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) LIKE ? OR u.email LIKE ?)")
		args = append(args, "%"+q+"%", "%"+q+"%")
	}

	rows := []AssignableStaffRow{}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT DISTINCT u.public_id,
		       CONCAT(u.first_name, ' ', COALESCE(u.last_name, '')) AS name,
		       COALESCE(u.email, '') AS email,
		       MIN(ro.name) AS role_name
		FROM users u
		JOIN tenants tn    ON tn.id = u.tenant_id
		JOIN user_roles ur ON ur.user_id = u.id
		JOIN roles ro      ON ro.id = ur.role_id
		WHERE `+strings.Join(where, " AND ")+`
		GROUP BY u.id, u.public_id, u.first_name, u.last_name, u.email
		ORDER BY name`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing assignable staff: %w", err)
	}
	return rows, nil
}
