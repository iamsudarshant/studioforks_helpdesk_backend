package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// Clearing a database is not something to do with a shell full of DELETE
// statements typed once and never seen again. It is written here so the exact
// scope is reviewable, repeatable, and says plainly what it keeps.
//
// The division is between what a client *configured* and what the desk
// *accumulated*. Departments, entities, the query taxonomy, roles, permissions,
// user groups and the FAQ are configuration: they were set up deliberately and
// a fresh deployment is expected to start with them. Tickets, their
// conversations and attachments, documents, notifications and help requests are
// accumulation — the record of a desk having been used, which is exactly what a
// production launch should not begin with.
//
// Sites sit with the accumulation here only because they were named as
// something to clear; structurally they belong with entities.

// operationalTables are truncated in this order — children first, so a foreign
// key never blocks a parent's delete.
//
// Ordered by hand rather than by disabling the constraint checks: the order
// documents the dependency graph, and a table that has to be added later fails
// loudly in the wrong position rather than silently orphaning rows.
var operationalTables = []string{
	// Everything hanging off a ticket.
	"ticket_watchers",
	"ticket_timeline",
	"ticket_status_history",
	"ticket_sla_events",
	"ticket_feedback",
	"ticket_conversations",
	"ticket_attachments",
	"ticket_assignments",
	"tickets",

	// Files, their previews, versions and access trail.
	"document_access_log",
	"document_previews",
	"document_versions",
	"report_jobs",
	"documents",

	// The desk's own record of what it told people.
	"notifications",
	"outbox_events",

	// Help requests raised against ComplyDesk itself.
	"help_tickets",

	// Sites, and the allocations pointing at them.
	"site_assignments",
	"sites",

	// Counters derived from the above; stale the moment the above is empty.
	"metrics_daily",
}

// peopleTables clear alongside a user when --people is given.
var peopleTables = []string{
	"sessions",
	"otp_codes",
	"password_reset_tokens",
	"password_history",
	"user_roles",
	"entity_assignments",
	"department_assignments",
	"site_assignments",
	"agent_tenant_assignments",
	"notification_preferences",
}

// ResetCommand clears accumulated data while leaving a client's configuration
// standing.
func ResetCommand(ctx context.Context, args []string, cfg *config.Config) error {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	confirm := fs.Bool("yes", false,
		"actually do it; without this the command reports what it would clear and stops")
	people := fs.Bool("people", false,
		"also remove client-side users — employees and partners. Staff accounts are kept, "+
			"or nobody could sign in afterwards")
	emptyClients := fs.Bool("empty-clients", false,
		"also remove clients that hold no users and no tickets, such as those left by test runs")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := platform.OpenDB(cfg.DB)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	plan, err := buildResetPlan(ctx, db, *people, *emptyClients)
	if err != nil {
		return err
	}

	fmt.Println("This clears:")
	for _, line := range plan.clears {
		fmt.Println("  " + line)
	}
	fmt.Println("\nThis keeps:")
	for _, line := range plan.keeps {
		fmt.Println("  " + line)
	}

	if !*confirm {
		fmt.Println("\nNothing has been changed. Re-run with --yes to apply.")
		return nil
	}

	if err := applyReset(ctx, db, plan); err != nil {
		return err
	}
	fmt.Println("\nDone.")
	return nil
}

type resetPlan struct {
	clears        []string
	keeps         []string
	people        bool
	emptyClients  bool
	clientTenants []int64
}

// buildResetPlan counts what is about to go, so the operator sees the size of
// the thing before agreeing to it rather than after.
func buildResetPlan(ctx context.Context, db *platform.DB, people, emptyClients bool) (*resetPlan, error) {
	plan := &resetPlan{people: people, emptyClients: emptyClients}

	for _, table := range []string{"tickets", "documents", "notifications", "sites", "help_tickets"} {
		var n int64
		if err := db.Primary.GetContext(ctx, &n, "SELECT COUNT(*) FROM "+table); err != nil {
			return nil, fmt.Errorf("counting %s: %w", table, err)
		}
		plan.clears = append(plan.clears, fmt.Sprintf("%-16s %d", table, n))
	}
	plan.clears = append(plan.clears, "and everything hanging off them (conversations, timeline, SLA events, versions)")

	if people {
		var n int64
		if err := db.Primary.GetContext(ctx, &n, `
			SELECT COUNT(*) FROM users u
			JOIN tenants t ON t.id = u.tenant_id
			WHERE t.is_platform = 0 AND u.deleted_at IS NULL`); err != nil {
			return nil, fmt.Errorf("counting client users: %w", err)
		}
		plan.clears = append(plan.clears, fmt.Sprintf("%-16s %d (employees and partners; staff kept)", "users", n))
	}

	if emptyClients {
		ids := []int64{}
		if err := db.Primary.SelectContext(ctx, &ids, `
			SELECT t.id FROM tenants t
			WHERE t.is_platform = 0
			  AND NOT EXISTS (SELECT 1 FROM users u   WHERE u.tenant_id = t.id AND u.deleted_at IS NULL)
			  AND NOT EXISTS (SELECT 1 FROM tickets k WHERE k.tenant_id = t.id)`); err != nil {
			return nil, fmt.Errorf("finding empty clients: %w", err)
		}
		plan.clientTenants = ids
		plan.clears = append(plan.clears, fmt.Sprintf("%-16s %d (no users, no tickets)", "clients", len(ids)))
	}

	plan.keeps = []string{
		"departments and entities, with their registrations and mappings",
		"the query taxonomy: categories, custom fields and workflows",
		"roles, permissions and user groups",
		"clients, their branding, settings and SLA policies",
		"FAQ articles",
	}
	if !people {
		plan.keeps = append(plan.keeps, "every user account (pass --people to clear client-side users)")
	} else {
		plan.keeps = append(plan.keeps, "staff accounts, so the platform can still be signed into")
	}
	return plan, nil
}

func applyReset(ctx context.Context, db *platform.DB, plan *resetPlan) error {
	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		for _, table := range operationalTables {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
				return fmt.Errorf("clearing %s: %w", table, err)
			}
		}

		if plan.people {
			// Client-side users only. Removing staff would leave nobody able to
			// sign in and administer what is left.
			if _, err := tx.ExecContext(ctx, `
				DELETE ur FROM user_roles ur
				JOIN users u   ON u.id = ur.user_id
				JOIN tenants t ON t.id = u.tenant_id
				WHERE t.is_platform = 0`); err != nil {
				return fmt.Errorf("clearing client user roles: %w", err)
			}
			for _, table := range peopleTables {
				if table == "user_roles" {
					continue // handled above, joined through the tenant
				}
				if _, err := tx.ExecContext(ctx, `
					DELETE x FROM `+table+` x
					JOIN users u   ON u.id = x.user_id
					JOIN tenants t ON t.id = u.tenant_id
					WHERE t.is_platform = 0`); err != nil {
					// Not every table keys on user_id; those are skipped rather
					// than guessed at.
					continue
				}
			}
			if _, err := tx.ExecContext(ctx, `
				DELETE u FROM users u
				JOIN tenants t ON t.id = u.tenant_id
				WHERE t.is_platform = 0`); err != nil {
				return fmt.Errorf("clearing client users: %w", err)
			}
		}

		for _, id := range plan.clientTenants {
			if err := deleteEmptyClient(ctx, tx, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// clientOwnedTables are wiped when an empty client is removed. Listed rather
// than discovered so removing a client cannot quietly miss a table added later
// — a missed one fails on the foreign key instead of leaving an orphan.
var clientOwnedTables = []string{
	"entity_registrations", "entity_assignments", "department_assignments",
	"site_assignments", "entities", "departments", "sites",
	"category_fields", "category_workflows", "categories",
	"document_categories", "faq_articles", "notification_templates",
	"notification_preferences", "routing_rules", "sla_policies", "business_hours",
	"saved_views", "report_definitions", "report_schedules", "metrics_daily",
	"tenant_branding", "tenant_domains", "tenant_settings", "tenant_modules",
	"user_groups", "agent_tenant_assignments", "api_keys", "idempotency_keys",
	"bulk_import_errors", "bulk_import_jobs", "maintenance_windows",
	"otp_codes", "password_reset_tokens", "sessions",
}

func deleteEmptyClient(ctx context.Context, tx *sqlx.Tx, tenantID int64) error {
	for _, table := range clientOwnedTables {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE tenant_id = ?", tenantID); err != nil {
			// A table without a tenant_id column is not this client's to clear.
			if strings.Contains(err.Error(), "Unknown column") {
				continue
			}
			return fmt.Errorf("clearing %s for client %d: %w", table, tenantID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM tenants WHERE id = ?", tenantID); err != nil {
		return fmt.Errorf("removing client %d: %w", tenantID, err)
	}
	return nil
}
