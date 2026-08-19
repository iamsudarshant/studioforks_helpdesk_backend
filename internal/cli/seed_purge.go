package cli

// Removing the invented data.
//
// Three generations of sample data accumulated in this database: a demo
// workspace, a "showcase" ticket spread, and a three-client sample set with
// invented companies. Between them they left clients nobody asked for, entities
// that were never part of the catalogue, and tickets attached to both.
//
// This pass removes them and leaves exactly the configured dataset behind. It
// is destructive by design and therefore explicit: `seed --purge-dummy` and
// nothing else triggers it.
//
// What "dummy" means here is narrow and named, never inferred. Deleting rows
// because they look invented is how real client data gets destroyed, so the
// invented clients are listed by slug and everything else is left alone.

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

// dummyClientSlugs are the workspaces the older sample datasets invented. The
// Ampersand workspace is deliberately absent: it is the dataset being kept, and
// its contents are reconciled in place rather than dropped.
var dummyClientSlugs = []string{"zenith", "orbit"}

// purgeDummyData deletes the invented clients and everything hanging off them.
//
// Hard deletes rather than soft ones. A soft-deleted client still occupies its
// slug and its client code, so the next seed cannot recreate a clean one, and
// the rows stay behind forever answering "no" to every query — all the cost of
// keeping them with none of the benefit. Foreign keys cascade from `tenants`,
// so removing the client removes its users, entities, tickets and documents in
// one statement.
func purgeDummyData(ctx context.Context, db *platform.DB) error {
	if len(dummyClientSlugs) == 0 {
		return nil
	}

	placeholders := make([]string, len(dummyClientSlugs))
	args := make([]any, len(dummyClientSlugs))
	for i, slug := range dummyClientSlugs {
		placeholders[i] = "?"
		args[i] = slug
	}
	inList := strings.Join(placeholders, ",")

	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		var ids []int64
		if err := tx.SelectContext(ctx, &ids,
			`SELECT id FROM tenants WHERE slug IN (`+inList+`) AND is_platform = 0`, args...); err != nil {
			return fmt.Errorf("finding the sample clients: %w", err)
		}
		if len(ids) == 0 {
			fmt.Println("no invented clients to remove")
			return nil
		}

		idArgs := platform.Int64Args(ids)
		idList := platform.Placeholders(len(ids))

		// Staff assignments point at the client from the platform tenant, which
		// is not covered by the cascade from `tenants` — the row belongs to the
		// agent, not to the client being deleted.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM agent_tenant_assignments WHERE tenant_id IN (`+idList+`)`, idArgs...); err != nil {
			return fmt.Errorf("removing agent assignments: %w", err)
		}

		res, err := tx.ExecContext(ctx,
			`DELETE FROM tenants WHERE id IN (`+idList+`)`, idArgs...)
		if err != nil {
			return fmt.Errorf("removing the sample clients: %w", err)
		}
		removed, _ := res.RowsAffected()
		fmt.Printf("removed %d invented client(s) and everything filed under them\n", removed)
		return nil
	})
}

// purgeAmpersandSampleTickets clears the tickets an earlier dataset generated
// for the client being kept, so the new pass files a clean set rather than
// adding a second one alongside.
//
// Scoped to tickets nobody has worked: anything with a reply, an attachment
// from the desk, or a resolution is left alone. A demo database is still
// somebody's afternoon of testing, and silently deleting the ticket they were
// halfway through is not a trade worth making for a tidier dataset.
func purgeAmpersandSampleTickets(ctx context.Context, db *platform.DB) error {
	tenantID, err := lookupID(ctx, db,
		`SELECT id FROM tenants WHERE slug = ? AND deleted_at IS NULL`, ampersandSlug)
	if err != nil {
		return nil // nothing to clean
	}

	res, err := db.Primary.ExecContext(ctx, `
		DELETE t FROM tickets t
		 WHERE t.tenant_id = ?
		   AND t.resolved_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM ticket_conversations c WHERE c.ticket_id = t.id)`,
		tenantID)
	if err != nil {
		return fmt.Errorf("clearing untouched sample tickets: %w", err)
	}
	if removed, _ := res.RowsAffected(); removed > 0 {
		fmt.Printf("cleared %d untouched sample ticket(s) from %s\n", removed, ampersandName)
	}
	return nil
}

// dummyAccountPatterns match the individual accounts the older seeds invented
// inside a workspace that is otherwise being kept.
//
// Listed as explicit SQL LIKE patterns rather than inferred, for the same
// reason the client list is explicit: "looks like test data" is not a safe
// basis for a DELETE. `superadmin@complydesk.local` is deliberately absent —
// it is the platform administrator the system needs to remain administrable.
var dummyAccountPatterns = []string{
	"%@demo.local", // the --demo roster: employee@, partner@, exec.entity@, ...
	"%@amp.com",    // hand-made test accounts
	"agent.arjun@complydesk.local",
	"agent.priya@complydesk.local",
}

// purgeDummyAccounts removes the invented people from a workspace that is
// otherwise being kept, along with the tickets they raised.
//
// The tickets go first because `tickets.requester_id` is RESTRICT: a ticket
// must always name a real person, so the row cannot be orphaned and the
// requester cannot be removed while one exists. That constraint is right — it
// is what stops a deleted account silently erasing its author from history —
// so this deletes both together rather than working around it.
func purgeDummyAccounts(ctx context.Context, db *platform.DB) error {
	clauses := make([]string, len(dummyAccountPatterns))
	args := make([]any, len(dummyAccountPatterns))
	for i, pattern := range dummyAccountPatterns {
		clauses[i] = "u.email LIKE ?"
		args[i] = pattern
	}
	match := "(" + strings.Join(clauses, " OR ") + ")"

	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		var ids []int64
		if err := tx.SelectContext(ctx, &ids,
			`SELECT u.id FROM users u WHERE `+match, args...); err != nil {
			return fmt.Errorf("finding the invented accounts: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}

		idArgs := platform.Int64Args(ids)
		idList := platform.Placeholders(len(ids))

		ticketRes, err := tx.ExecContext(ctx,
			`DELETE FROM tickets WHERE requester_id IN (`+idList+`)`, idArgs...)
		if err != nil {
			return fmt.Errorf("removing their tickets: %w", err)
		}

		userRes, err := tx.ExecContext(ctx,
			`DELETE FROM users WHERE id IN (`+idList+`)`, idArgs...)
		if err != nil {
			return fmt.Errorf("removing the invented accounts: %w", err)
		}

		removedTickets, _ := ticketRes.RowsAffected()
		removedUsers, _ := userRes.RowsAffected()
		fmt.Printf("removed %d invented account(s) and %d of their ticket(s)\n",
			removedUsers, removedTickets)
		return nil
	})
}

// purgeOrphanDocuments removes document rows that nothing points at any more.
//
// Two kinds accumulate. The older seeds created GENERAL-owned documents that
// were never attached to anything, and deleting a ticket leaves its documents
// behind — `documents.owner_id` is a plain column, not a foreign key, precisely
// because a document can belong to a ticket, a user avatar or a brand logo, and
// one column cannot cascade from three tables.
//
// So the sweep is defined by what still references the row, not by what it
// claims to own: a document with no ticket attachment and no owning record is
// unreachable through the product and only inflates the storage report.
// Avatars and brand logos are excluded by owner_type, because those *are*
// referenced — from `users.avatar_path` and `tenant_branding.logo_path`, which
// hold the public id rather than the numeric one.
func purgeOrphanDocuments(ctx context.Context, db *platform.DB) error {
	res, err := db.Primary.ExecContext(ctx, `
		DELETE d FROM documents d
		 WHERE d.owner_type NOT IN ('USER', 'TENANT')
		   AND NOT EXISTS (SELECT 1 FROM ticket_attachments a WHERE a.document_id = d.id)
		   AND NOT EXISTS (SELECT 1 FROM tickets t
		                    WHERE d.owner_type = 'TICKET' AND t.id = d.owner_id)`)
	if err != nil {
		return fmt.Errorf("removing orphaned documents: %w", err)
	}
	if removed, _ := res.RowsAffected(); removed > 0 {
		fmt.Printf("removed %d orphaned document(s)\n", removed)
	}
	return nil
}
