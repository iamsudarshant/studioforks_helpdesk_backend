package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/auth"
	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/tenant"
	"github.com/karmamgmt/complydesk/internal/user"
)

// Seed installs the platform catalogue (permissions, system roles, notification
// events) and optionally a demo workspace. It is idempotent: running it twice
// updates rather than duplicates.
func Seed(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	demo := fs.Bool("demo", false, "also create a demo workspace with sample data")
	showcase := fs.Bool("showcase", false, "populate the demo client with a full spread of sample tickets")
	sample := fs.Bool("sample", false, "install the full sample dataset: several live clients, the standard departments and entity catalogue, a knowledge base and sample Help requests")
	ampersand := fs.Bool("ampersand", false, "install the Ampersand Group dataset: two statutory departments, the full entity catalogue, one admin, two agents, a partner per entity, and employees with tickets")
	purge := fs.Bool("purge-dummy", false, "delete the invented sample clients and their data before seeding")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}

	db, err := platform.OpenDB(cfg.DB)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := seedPermissions(ctx, db); err != nil {
		return err
	}
	if err := seedSystemRoles(ctx, db); err != nil {
		return err
	}
	if err := seedNotificationEvents(ctx, db); err != nil {
		return err
	}
	if err := seedEntityTemplates(ctx, db); err != nil {
		return err
	}
	if err := seedModules(ctx, db); err != nil {
		return err
	}
	// Canonical roles run after the legacy ones so the alias mirroring has both
	// sides to work with.
	if err := seedCanonicalRoles(ctx, db); err != nil {
		return err
	}

	moved, err := relocateKarmaStaff(ctx, db)
	if err != nil {
		return err
	}
	if moved > 0 {
		fmt.Printf("relocated %d internal user(s) to the Karma platform tenant, preserving their client assignments\n", moved)
	}

	// A new module in the catalogue is useless until clients can see it, and an
	// installation that predates the catalogue has no rows at all. Backfilling
	// here — rather than only when a client is created — is what makes adding a
	// module a data change rather than a migration.
	backfilled, err := backfillClientModules(ctx, db)
	if err != nil {
		return err
	}
	if backfilled > 0 {
		fmt.Printf("enabled the module catalogue for %d client(s)\n", backfilled)
	}

	// Same reasoning for the query taxonomy: a query type added to §8 has to
	// reach the clients that already exist, not only the next one created.
	if types, err := backfillClientQueryTypes(ctx, db); err != nil {
		return err
	} else if types > 0 {
		fmt.Printf("refreshed the query taxonomy for %d client(s)\n", types)
	}

	fmt.Println("platform catalogue seeded")

	// These all build on the demo workspace, so asking for any of them implies
	// it rather than failing on ordering.
	if (*showcase || *sample || *ampersand) && !*demo {
		*demo = true
	}

	// Before anything is created, so the new dataset is not built alongside the
	// one being removed.
	if *purge {
		if err := purgeDummyData(ctx, db); err != nil {
			return err
		}
		if err := purgeDummyAccounts(ctx, db); err != nil {
			return err
		}
		if err := purgeAmpersandSampleTickets(ctx, db); err != nil {
			return err
		}
		// Last, so it also catches the documents the deletions above orphaned.
		if err := purgeOrphanDocuments(ctx, db); err != nil {
			return err
		}
	}

	if *demo {
		if err := seedDemoTenant(ctx, db, cfg); err != nil {
			return err
		}
	}

	if *showcase {
		if err := seedShowcase(ctx, db, cfg); err != nil {
			return err
		}
	}

	// Last: the sample pass reconciles the organisation structure of every
	// client, including any the steps above have just created.
	if *sample {
		if err := seedSample(ctx, db, cfg); err != nil {
			return err
		}
	}

	// After `--sample`, because that pass reconciles every client against the
	// generic catalogue and would otherwise reintroduce the departments this
	// one deliberately retires.
	if *ampersand {
		if err := seedAmpersand(ctx, db, cfg); err != nil {
			return err
		}
	}
	return nil
}

func seedPermissions(ctx context.Context, db *platform.DB) error {
	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		all := append(append([]permission{}, permissions...), platformPermissions...)
		for _, p := range all {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO permissions (permission_key, permission_group, description)
				VALUES (?,?,?)
				ON DUPLICATE KEY UPDATE
					permission_group = VALUES(permission_group),
					description = VALUES(description)`,
				p.Key, p.Group, p.Description); err != nil {
				return fmt.Errorf("seeding permission %s: %w", p.Key, err)
			}
		}
		return nil
	})
}

func seedSystemRoles(ctx context.Context, db *platform.DB) error {
	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		for _, role := range systemRoles {
			var id int64
			err := tx.GetContext(ctx, &id,
				`SELECT id FROM roles WHERE tenant_id IS NULL AND role_key = ?`, role.Key)

			if err != nil {
				if !platform.IsNotFound(err) {
					return fmt.Errorf("checking role %s: %w", role.Key, err)
				}
				res, insErr := tx.ExecContext(ctx, `
					INSERT INTO roles (public_id, tenant_id, role_key, name, description, portal, is_system)
					VALUES (?, NULL, ?, ?, ?, ?, 1)`,
					platform.NewULID(), role.Key, role.Name, role.Description, role.Portal)
				if insErr != nil {
					return fmt.Errorf("creating role %s: %w", role.Key, insErr)
				}
				if id, err = res.LastInsertId(); err != nil {
					return fmt.Errorf("reading role id: %w", err)
				}
			} else {
				if _, err := tx.ExecContext(ctx,
					`UPDATE roles SET name = ?, description = ?, portal = ? WHERE id = ?`,
					role.Name, role.Description, role.Portal, id); err != nil {
					return fmt.Errorf("updating role %s: %w", role.Key, err)
				}
			}

			// Re-apply the permission set so a change to the catalogue lands on
			// existing installations too.
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM role_permissions WHERE role_id = ?`, id); err != nil {
				return fmt.Errorf("clearing permissions for %s: %w", role.Key, err)
			}
			for _, perm := range role.Permissions {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO role_permissions (role_id, permission_key) VALUES (?,?)`,
					id, perm); err != nil {
					return fmt.Errorf("granting %s to %s: %w", perm, role.Key, err)
				}
			}
		}
		return nil
	})
}

func seedNotificationEvents(ctx context.Context, db *platform.DB) error {
	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		for _, e := range notificationEvents {
			vars, err := json.Marshal(e.Variables)
			if err != nil {
				return fmt.Errorf("encoding variables for %s: %w", e.Key, err)
			}
			channels, err := json.Marshal(e.DefaultChannels)
			if err != nil {
				return fmt.Errorf("encoding channels for %s: %w", e.Key, err)
			}

			if _, err := tx.ExecContext(ctx, `
				INSERT INTO notification_events
					(event_key, event_group, description, variables_json, default_channels_json)
				VALUES (?,?,?,?,?)
				ON DUPLICATE KEY UPDATE
					event_group = VALUES(event_group),
					description = VALUES(description),
					variables_json = VALUES(variables_json),
					default_channels_json = VALUES(default_channels_json)`,
				e.Key, e.Group, e.Description, string(vars), string(channels)); err != nil {
				return fmt.Errorf("seeding event %s: %w", e.Key, err)
			}
		}
		return nil
	})
}

func seedEntityTemplates(ctx context.Context, db *platform.DB) error {
	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		for _, tpl := range entityTemplates {
			categories, err := json.Marshal(tpl.DefaultCategories)
			if err != nil {
				return fmt.Errorf("encoding template categories for %s: %w", tpl.Key, err)
			}

			if _, err := tx.ExecContext(ctx, `
				INSERT INTO entity_templates
					(public_id, template_key, name, description, entity_type,
					 default_categories_json, sort_order, is_active)
				VALUES (?,?,?,?,?,?,?,1)
				ON DUPLICATE KEY UPDATE
					name = VALUES(name),
					description = VALUES(description),
					entity_type = VALUES(entity_type),
					default_categories_json = VALUES(default_categories_json),
					sort_order = VALUES(sort_order)`,
				platform.NewULID(), tpl.Key, tpl.Name, nullIfEmpty(tpl.Description),
				nullIfEmpty(tpl.EntityType), string(categories), tpl.SortOrder); err != nil {
				return fmt.Errorf("seeding entity template %s: %w", tpl.Key, err)
			}
		}
		return nil
	})
}

// reconcileDemoAssignments re-grants the demo's agent coverage.
//
// Arjun covers Ampersand; Priya deliberately covers nothing, so the isolation
// rule stays observable. Only Arjun is (re)granted — adding Priya would destroy
// the demonstration.
func reconcileDemoAssignments(ctx context.Context, db *platform.DB, demoTenantID int64) (int, error) {
	userRepo := user.NewRepository(db)

	var agentID int64
	err := db.Primary.GetContext(ctx, &agentID,
		`SELECT id FROM users WHERE email = ? AND deleted_at IS NULL`, "agent.arjun@complydesk.local")
	if err != nil {
		if platform.IsNotFound(err) {
			return 0, nil // the demo staff were never seeded here
		}
		return 0, fmt.Errorf("locating the demo agent: %w", err)
	}

	assigned, err := userRepo.IsAssigned(ctx, agentID, demoTenantID)
	if err != nil {
		return 0, err
	}
	if assigned {
		return 0, nil
	}

	if err := userRepo.AssignAgent(ctx, agentID, demoTenantID, true, nil); err != nil {
		return 0, fmt.Errorf("restoring the demo agent assignment: %w", err)
	}
	return 1, nil
}

// demoEmployees is Ampersand Group's roster.
//
// Package-level rather than local to seedDemoTenant, because the workspace is
// created once but the roster grows — `seed --showcase` reconciles it against
// an existing client so new sample people actually appear.
var demoEmployees = []struct {
	Email, First, Last, Code, PF, UAN, PAN, ESIC, Designation string
	// Ex marks someone who has left: they get EX_EMPLOYEE status, the read-only
	// group, and a last working day.
	Ex bool
}{
	{"employee@demo.local", "Anita", "Desai", "DM-EMP-001", "MH/BAN/0012345/000/0012345", "100234567890", "ABCDE1234F", "31000112345670001", "Senior Executive", false},
	{"exemployee@demo.local", "Vikram", "Singh", "DM-EMP-002", "MH/BAN/0012345/000/0067890", "100234567891", "BCDEF2345G", "31000112345670002", "Accounts Officer", true},
	{"rahul.mehta@demo.local", "Rahul", "Mehta", "DM-EMP-003", "MH/BAN/0012345/000/0023456", "100234567892", "CDEFG3456H", "31000112345670003", "Team Lead", false},
	{"sneha.kulkarni@demo.local", "Sneha", "Kulkarni", "DM-EMP-004", "MH/BAN/0012345/000/0034567", "100234567893", "DEFGH4567I", "31000112345670004", "Analyst", false},
	{"imran.shaikh@demo.local", "Imran", "Shaikh", "DM-EMP-005", "MH/BAN/0012345/000/0045678", "100234567894", "EFGHI5678J", "31000112345670005", "Shift Supervisor", false},
	{"priyanka.rao@demo.local", "Priyanka", "Rao", "DM-EMP-006", "MH/BAN/0012345/000/0056789", "100234567895", "FGHIJ6789K", "31000112345670006", "HR Coordinator", false},
	{"arun.pillai@demo.local", "Arun", "Pillai", "DM-EMP-007", "MH/BAN/0012345/000/0078901", "100234567896", "GHIJK7890L", "31000112345670007", "Maintenance Technician", false},
	{"meena.joshi@demo.local", "Meena", "Joshi", "DM-EMP-008", "MH/BAN/0012345/000/0089012", "100234567897", "HIJKL8901M", "31000112345670008", "Quality Inspector", true},
}

// seedDemoTenant creates a workspace with structure, categories, an SLA policy
// and one user per role, so the API is explorable straight after setup.
func seedDemoTenant(ctx context.Context, db *platform.DB, cfg *config.Config) error {
	repo := tenant.NewRepository(db)
	userRepo := user.NewRepository(db)
	hasher := auth.NewHasher(cfg.Auth)

	existing, err := repo.BySlug(ctx, "demo")
	if err == nil {
		// Creating the workspace again would duplicate its structure, so that is
		// skipped — but the demo's agent assignments are reconciled, because they
		// live in a table that a down-migration drops and nothing else restores.
		// Without this, rolling migrations back and forward leaves the demo with
		// no assigned agent and the isolation demo silently broken.
		restored, rerr := reconcileDemoAssignments(ctx, db, existing.ID)
		if rerr != nil {
			return rerr
		}
		// The roster gained fields after the first databases were seeded: an
		// ESIC number, a designation, a date of birth, a posting. Filling them
		// here means an existing database converges on the current sample data
		// instead of being stuck with whatever the seed knew about on the day
		// it first ran.
		filled, ferr := fillDemoStatutoryDetails(ctx, db, existing.ID)
		if ferr != nil {
			return ferr
		}
		fmt.Printf("demo workspace already exists (id %s); skipping structure\n", existing.PublicID)
		if restored > 0 {
			fmt.Printf("restored %d demo agent assignment(s)\n", restored)
		}
		if filled > 0 {
			fmt.Printf("filled statutory details for %d demo employee(s)\n", filled)
		}
		return nil
	}

	contractStart := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	contractEnd := time.Date(2028, 3, 31, 0, 0, 0, 0, time.UTC)

	demoTenant, err := repo.Create(ctx, tenant.CreateParams{
		Slug: "demo", ClientCode: "AMP001",
		Name: "Ampersand Group", LegalName: "Ampersand Group Holdings Private Limited",
		Industry: "Manufacturing & Services",
		Timezone: "Asia/Kolkata", Locale: "en-IN", TicketPrefix: "HD",
		ContactEmail: "helpdesk@ampersand.example", AltEmail: "compliance@ampersand.example",
		ContactPhone: "02240001234", AltPhone: "02240001235",
		Address:       "Ampersand House, Andheri East, Mumbai 400069",
		TaxID:         "27AABCA1234M1Z5",
		ContractStart: &contractStart, ContractEnd: &contractEnd,
	})
	if err != nil {
		return fmt.Errorf("creating demo workspace: %w", err)
	}
	if err := repo.SetStatus(ctx, demoTenant.ID, tenant.StatusActive); err != nil {
		return fmt.Errorf("activating demo workspace: %w", err)
	}

	if err := seedDemoStructure(ctx, db, demoTenant.ID); err != nil {
		return err
	}
	slaID, err := seedDemoSLA(ctx, db, demoTenant.ID)
	if err != nil {
		return err
	}
	if err := seedDemoCategories(ctx, db, demoTenant.ID, slaID); err != nil {
		return err
	}
	// Modules must be linked before subcategories, which inherit module_id.
	if err := enableModulesForTenant(ctx, db, demoTenant.ID); err != nil {
		return err
	}
	if err := seedSubcategories(ctx, db, demoTenant.ID); err != nil {
		return err
	}
	// Registrations need both entities and categories to exist first.
	if err := seedDemoRegistrations(ctx, db, demoTenant.ID); err != nil {
		return err
	}

	password := "ComplyDesk@2026"
	hash, err := hasher.Hash(password)
	if err != nil {
		return fmt.Errorf("hashing demo password: %w", err)
	}

	// ComplyDesk's own staff live in the platform tenant, never inside a client.
	// Their reach comes from agent_tenant_assignments, which is what makes
	// "cannot access tenants not assigned" demonstrable rather than aspirational.
	staffTenant, err := repo.BySlug(ctx, KarmaTenantSlug)
	if err != nil {
		return fmt.Errorf("loading the ComplyDesk staff workspace: %w", err)
	}

	complydeskStaff := []struct {
		role, email, first, last, employeeCode string
	}{
		{user.RoleSuperAdmin, "superadmin@complydesk.local", "Meera", "Nair", "CD-SA-001"},
		{user.RoleAgent, "agent.arjun@complydesk.local", "Arjun", "Rao", "CD-AG-001"},
		{user.RoleAgent, "agent.priya@complydesk.local", "Priya", "Sharma", "CD-AG-002"},
	}

	staffIDs := map[string]int64{}
	for i, acct := range complydeskStaff {
		// Mobile is an advertised login identifier, so it has to be unique:
		// FindByIdentifier refuses an ambiguous match rather than guessing which
		// account was meant, which would make sign-in by mobile simply fail.
		created, err := userRepo.Create(ctx, staffTenant.ID, user.CreateParams{
			EmployeeCode: acct.employeeCode, FirstName: acct.first, LastName: acct.last,
			Email: acct.email, Mobile: fmt.Sprintf("98000100%02d", i+1),
			PasswordHash: hash, Status: user.StatusActive,
		})
		if err != nil {
			return fmt.Errorf("creating %s: %w", acct.email, err)
		}
		role, err := userRepo.RoleByKey(ctx, staffTenant.ID, acct.role)
		if err != nil {
			return fmt.Errorf("loading role %s: %w", acct.role, err)
		}
		if err := userRepo.SetRoles(ctx, staffTenant.ID, created.ID, []int64{role.ID}, nil); err != nil {
			return fmt.Errorf("assigning role %s: %w", acct.role, err)
		}
		staffIDs[acct.email] = created.ID
	}

	// Arjun covers Ampersand. Priya deliberately does not, so the isolation rule
	// is observable: she is refused this client and sees only her own.
	if err := userRepo.AssignAgent(ctx, staffIDs["agent.arjun@complydesk.local"], demoTenant.ID, true, nil); err != nil {
		return fmt.Errorf("assigning Arjun to Ampersand: %w", err)
	}

	// Client-side accounts belong to the client itself.
	accounts := []struct {
		role, email, first, last, employeeCode string
	}{
		{user.RolePartner, "partner@demo.local", "Rahul", "Verma", "DM-PTR-001"},
		{user.RolePartner, "exec.entity@demo.local", "Kavya", "Iyer", "DM-PEX-001"},
		{user.RolePartner, "exec.dept@demo.local", "Suresh", "Menon", "DM-PEX-002"},
	}

	activeGroup, err := userRepo.GroupByKey(ctx, demoTenant.ID, user.GroupActiveEmployees)
	if err != nil {
		return fmt.Errorf("loading active group: %w", err)
	}

	doj := time.Date(2022, 4, 1, 0, 0, 0, 0, time.UTC)

	for i, acct := range accounts {
		created, err := userRepo.Create(ctx, demoTenant.ID, user.CreateParams{
			EmployeeCode: acct.employeeCode, FirstName: acct.first, LastName: acct.last,
			Email: acct.email, Mobile: fmt.Sprintf("98000200%02d", i+1),
			DateOfJoining: &doj, UserGroupID: &activeGroup.ID,
			PasswordHash: hash, Status: user.StatusActive,
		})
		if err != nil {
			return fmt.Errorf("creating %s: %w", acct.email, err)
		}
		role, err := userRepo.RoleByKey(ctx, demoTenant.ID, acct.role)
		if err != nil {
			return fmt.Errorf("loading role %s: %w", acct.role, err)
		}
		if err := userRepo.SetRoles(ctx, demoTenant.ID, created.ID, []int64{role.ID}, nil); err != nil {
			return fmt.Errorf("assigning role %s: %w", acct.role, err)
		}
	}

	// A pair of employees: one active, one already separated, so the
	// ex-employee grace-period behaviour is visible immediately.
	empRole, err := userRepo.RoleByKey(ctx, demoTenant.ID, user.RoleEmployee)
	if err != nil {
		return fmt.Errorf("loading employee role: %w", err)
	}
	exGroup, err := userRepo.GroupByKey(ctx, demoTenant.ID, user.GroupExEmployees)
	if err != nil {
		return fmt.Errorf("loading ex-employee group: %w", err)
	}

	employees := demoEmployees

	for i, emp := range employees {
		// Spread across working age, so "approaching retirement" and "joined
		// last year" both describe somebody.
		dob := time.Date(1972+(i*4)%28, time.Month(1+(i*5)%12), 1+(i*9)%28, 0, 0, 0, 0, time.UTC)

		status := user.StatusActive
		group := activeGroup
		var lwd *time.Time
		if emp.Ex {
			status = user.StatusExEmployee
			group = exGroup
			lwd = timePtr(time.Now().AddDate(0, -1, 0))
		}
		created, err := userRepo.Create(ctx, demoTenant.ID, user.CreateParams{
			EmployeeCode: emp.Code, FirstName: emp.First, LastName: emp.Last,
			Email: emp.Email, Mobile: fmt.Sprintf("98000300%02d", i+1),
			PFNumber: emp.PF, UANNumber: emp.UAN, PANNumber: emp.PAN,
			ESICNumber: emp.ESIC, Designation: emp.Designation,
			DateOfJoining: &doj, DateOfBirth: &dob, LastWorkingDay: lwd,
			UserGroupID: &group.ID, PasswordHash: hash, Status: status,
		})
		if err != nil {
			return fmt.Errorf("creating %s: %w", emp.Email, err)
		}
		if err := userRepo.SetRoles(ctx, demoTenant.ID, created.ID, []int64{empRole.ID}, nil); err != nil {
			return fmt.Errorf("assigning employee role: %w", err)
		}

		// Post each of them to an establishment, and take the department from
		// it. Without a posting the ticket rail shows a person with no place of
		// work, which is the one thing an agent checks first — and the entity
		// filters on the ticket list have nothing to filter.
		if _, err := db.Primary.ExecContext(ctx, `
			UPDATE users u
			JOIN (
				SELECT id, department_id FROM entities
				WHERE tenant_id = ? AND deleted_at IS NULL
				ORDER BY id LIMIT 1 OFFSET ?
			) e
			SET u.entity_id = e.id, u.department_id = e.department_id
			WHERE u.id = ?`, demoTenant.ID, i%len(demoEntities), created.ID); err != nil {
			return fmt.Errorf("posting %s: %w", emp.Email, err)
		}
	}

	fmt.Printf(`
demo data created
  ComplyDesk staff workspace : karma
  Client                     : demo  (Ampersand Group)
  Password for all           : %s

  ComplyDesk staff - work across every client
  /admin    superadmin@complydesk.local   Super Admin
  /agents   agent.arjun@complydesk.local  Agent   - owns Ampersand
            agent.priya@complydesk.local  Agent   - owns nothing, still works every client

  Ampersand Group - confined to their own workspace
  /partner  partner@demo.local            Partner
            exec.entity@demo.local        Partner (entity-scoped)
            exec.dept@demo.local          Partner (department-scoped)
  /user     employee@demo.local           Employee
            exemployee@demo.local         Employee (ex-employee, read-only)

  Staff sign in against the "karma" workspace, then send X-Tenant-Slug: demo
  to work on Ampersand. A partner or employee sending another client's slug is
  refused - that is the boundary that matters.
`, password)
	return nil
}

// fillDemoStatutoryDetails fills in statutory details the roster has gained since
// a database was first seeded, and returns how many rows it touched.
//
// COALESCE throughout: only a NULL is filled. Anything edited through the UI
// stays as it was, because the seed's job is to make the sample complete, not
// to assert authorship over data somebody has since maintained.
func fillDemoStatutoryDetails(ctx context.Context, db *platform.DB, tenantID int64) (int, error) {
	touched := 0
	for i, emp := range demoEmployees {
		dob := time.Date(1972+(i*4)%28, time.Month(1+(i*5)%12), 1+(i*9)%28, 0, 0, 0, 0, time.UTC)

		res, err := db.Primary.ExecContext(ctx, `
			UPDATE users
			SET esic_number   = COALESCE(esic_number, ?),
			    designation   = COALESCE(designation, ?),
			    date_of_birth = COALESCE(date_of_birth, ?)
			WHERE tenant_id = ? AND email = ? AND deleted_at IS NULL
			  AND (esic_number IS NULL OR designation IS NULL OR date_of_birth IS NULL)`,
			emp.ESIC, emp.Designation, dob, tenantID, emp.Email)
		if err != nil {
			return touched, fmt.Errorf("filling details for %s: %w", emp.Email, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			touched++
		}

		// The posting, and the department that follows from it. Employees are
		// spread across the establishments rather than all landing on the first,
		// so the entity filter on the ticket list has something to separate.
		if _, err := db.Primary.ExecContext(ctx, `
			UPDATE users u
			JOIN (
				SELECT id, department_id FROM entities
				WHERE tenant_id = ? AND deleted_at IS NULL
				ORDER BY id LIMIT 1 OFFSET ?
			) e
			SET u.entity_id = COALESCE(u.entity_id, e.id),
			    u.department_id = COALESCE(u.department_id, e.department_id)
			WHERE u.tenant_id = ? AND u.email = ? AND u.deleted_at IS NULL`,
			tenantID, i%len(demoEntities), tenantID, emp.Email); err != nil {
			return touched, fmt.Errorf("posting %s: %w", emp.Email, err)
		}
	}
	return touched, nil
}

func timePtr(t time.Time) *time.Time { return &t }

// demoEntities are Ampersand Group's registered establishments. The mix of PF
// and ESIC codes is deliberately uneven: one company is registered for both, one
// sits above the ESI wage ceiling, one is ESI-only, and corporate services has
// no statutory registration at all. That is what makes the category filter on
// the ticket form visibly do something.
var demoEntities = []struct {
	Code, Name, Type, TemplateKey string
	PFCode, ESICCode              string
	GST, CIN                      string
}{
	{
		Code: "AMP-HO", Name: "Ampersand Group Holdings Pvt Ltd",
		Type: "HEAD_OFFICE", TemplateKey: "HO",
		PFCode: "MHBAN0012345000", ESICCode: "31000123450000101",
		GST: "27AABCA1234M1Z5", CIN: "U74999MH2009PTC190123",
	},
	{
		Code: "AMP-MFG", Name: "Ampersand Manufacturing Pvt Ltd",
		Type: "PLANT", TemplateKey: "MFG",
		PFCode: "MHBAN0067890000", ESICCode: "31000678900000101",
		GST: "27AABCA6789M1Z2", CIN: "U29299MH2011PTC220456",
	},
	{
		// Professional services: PF-registered, but salaries sit above the ESI
		// wage ceiling, so there is no ESIC code.
		Code: "AMP-SVC", Name: "Ampersand Services LLP",
		Type: "DIVISION", TemplateKey: "SVC",
		PFCode: "MHBAN0034567000",
		GST:    "27AABFA3456M1Z8",
	},
	{
		// Warehousing: an ESI-heavy workforce, with PF handled by the principal
		// employer rather than this entity.
		Code: "AMP-LOG", Name: "Ampersand Logistics Pvt Ltd",
		Type: "BRANCH", TemplateKey: "LOG",
		ESICCode: "31000456780000101",
		GST:      "07AABCA4567M1Z1",
	},
	{
		// No statutory registration: raises HR and IT queries only.
		Code: "AMP-CORP", Name: "Ampersand Corporate Services",
		Type: "DIVISION", TemplateKey: "CORP",
	},
}

// demoSites are the client's locations — Mumbai, Pune and Delhi — so a ticket
// shows which office the employee who raised it works from. Three are mapped to
// an entity and one deliberately is not, because a client may run a location
// that belongs to no registered establishment.
var demoSites = []struct {
	Code, Name, City, State, Pincode, EntityCode string
	IsDefault                                    bool
}{
	{Code: "AMP-MUM", Name: "Mumbai - Andheri", City: "Mumbai", State: "Maharashtra",
		Pincode: "400069", EntityCode: "AMP-HO", IsDefault: true},
	{Code: "AMP-PUN", Name: "Pune - Chakan Plant", City: "Pune", State: "Maharashtra",
		Pincode: "410501", EntityCode: "AMP-MFG"},
	{Code: "AMP-DEL", Name: "Delhi - Okhla", City: "New Delhi", State: "Delhi",
		Pincode: "110020", EntityCode: "AMP-LOG"},
	// No entity mapped: a sales office that is not a registered establishment.
	{Code: "AMP-BLR", Name: "Bengaluru - Sales Office", City: "Bengaluru", State: "Karnataka",
		Pincode: "560001"},
}

func seedDemoStructure(ctx context.Context, db *platform.DB, tenantID int64) error {
	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		entityIDs := map[string]int64{}

		for _, e := range demoEntities {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO entities
					(public_id, tenant_id, code, name, type, template_key, is_default,
					 gst_number, cin_number, is_active)
				VALUES (?,?,?,?,?,?,1,?,?,1)`,
				platform.NewULID(), tenantID, e.Code, e.Name, e.Type, e.TemplateKey,
				nullIfEmpty(e.GST), nullIfEmpty(e.CIN))
			if err != nil {
				return fmt.Errorf("creating entity %s: %w", e.Code, err)
			}
			id, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("reading entity id: %w", err)
			}
			entityIDs[e.Code] = id
		}

		for _, site := range demoSites {
			var entityID any
			if site.EntityCode != "" {
				entityID = entityIDs[site.EntityCode]
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO sites
					(public_id, tenant_id, entity_id, code, name, city, state, pincode,
					 is_active, is_default)
				VALUES (?,?,?,?,?,?,?,?,1,?)`,
				platform.NewULID(), tenantID, entityID, site.Code, site.Name,
				site.City, site.State, site.Pincode, site.IsDefault); err != nil {
				return fmt.Errorf("creating site %s: %w", site.Code, err)
			}
		}

		departments := []struct{ code, name string }{
			{"DEP-PF", "PF & Compliance"},
			{"DEP-ESI", "ESI & Insurance"},
			{"DEP-PAY", "Payroll"},
			{"DEP-HR", "Human Resources"},
			{"DEP-IT", "Information Technology"},
		}
		for _, d := range departments {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO departments (public_id, tenant_id, code, name, is_active)
				VALUES (?,?,?,?,1)`,
				platform.NewULID(), tenantID, d.code, d.name); err != nil {
				return fmt.Errorf("creating department %s: %w", d.code, err)
			}
		}
		return nil
	})
}

// seedDemoRegistrations maps each entity to the categories it is registered for,
// carrying the EPFO establishment code for PF and the ESIC code for ESI. This is
// exactly what the ticket form reads once a requester picks a category.
func seedDemoRegistrations(ctx context.Context, db *platform.DB, tenantID int64) error {
	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		categoryIDs := map[string]int64{}
		rows := []struct {
			ID  int64  `db:"id"`
			Key string `db:"category_key"`
		}{}
		if err := tx.SelectContext(ctx, &rows,
			`SELECT id, category_key FROM categories WHERE tenant_id = ?`, tenantID); err != nil {
			return fmt.Errorf("loading categories: %w", err)
		}
		for _, row := range rows {
			categoryIDs[row.Key] = row.ID
		}

		register := func(entityID, categoryID int64, number string) error {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO entity_registrations
					(public_id, tenant_id, entity_id, category_id, registration_number, is_active)
				VALUES (?,?,?,?,?,1)
				ON DUPLICATE KEY UPDATE
					registration_number = VALUES(registration_number),
					is_active = 1`,
				platform.NewULID(), tenantID, entityID, categoryID, nullIfEmpty(number))
			return err
		}

		for _, e := range demoEntities {
			var entityID int64
			if err := tx.GetContext(ctx, &entityID,
				`SELECT id FROM entities WHERE tenant_id = ? AND code = ?`,
				tenantID, e.Code); err != nil {
				return fmt.Errorf("loading entity %s: %w", e.Code, err)
			}

			if e.PFCode != "" {
				if id, ok := categoryIDs["PF_QUERY"]; ok {
					if err := register(entityID, id, e.PFCode); err != nil {
						return fmt.Errorf("registering %s for PF: %w", e.Code, err)
					}
				}
			}
			if e.ESICCode != "" {
				if id, ok := categoryIDs["ESI_QUERY"]; ok {
					if err := register(entityID, id, e.ESICCode); err != nil {
						return fmt.Errorf("registering %s for ESI: %w", e.Code, err)
					}
				}
			}
			// Every entity handles the non-statutory categories.
			for _, key := range []string{"PAYROLL", "HR_QUERY", "IT_SUPPORT", "GENERAL"} {
				if id, ok := categoryIDs[key]; ok {
					if err := register(entityID, id, ""); err != nil {
						return fmt.Errorf("registering %s for %s: %w", e.Code, key, err)
					}
				}
			}
		}
		return nil
	})
}

func seedDemoSLA(ctx context.Context, db *platform.DB, tenantID int64) (int64, error) {
	schedule := map[string][]map[string]string{
		"mon": {{"from": "09:30", "to": "18:30"}},
		"tue": {{"from": "09:30", "to": "18:30"}},
		"wed": {{"from": "09:30", "to": "18:30"}},
		"thu": {{"from": "09:30", "to": "18:30"}},
		"fri": {{"from": "09:30", "to": "18:30"}},
		"sat": {{"from": "09:30", "to": "13:30"}},
	}
	scheduleJSON, err := json.Marshal(schedule)
	if err != nil {
		return 0, fmt.Errorf("encoding business hours: %w", err)
	}
	holidays, err := json.Marshal([]string{"2026-01-26", "2026-08-15", "2026-10-02"})
	if err != nil {
		return 0, fmt.Errorf("encoding holidays: %w", err)
	}

	escalation, err := json.Marshal([]map[string]any{
		{"at_percent": 75, "notify_roles": []string{user.RoleHelpdeskExecutive}},
		{"at_percent": 100, "notify_roles": []string{user.RoleHelpdeskAdmin}},
		{"at_percent": 150, "notify_roles": []string{user.RoleHelpdeskMasterAdmin}},
	})
	if err != nil {
		return 0, fmt.Errorf("encoding escalation ladder: %w", err)
	}
	pause, err := json.Marshal([]string{"PENDING_INFORMATION"})
	if err != nil {
		return 0, fmt.Errorf("encoding pause statuses: %w", err)
	}

	var slaID int64
	err = db.InTx(ctx, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO business_hours (public_id, tenant_id, name, timezone, schedule_json, holidays_json, is_default)
			VALUES (?,?,?,?,?,?,1)`,
			platform.NewULID(), tenantID, "Standard business hours", "Asia/Kolkata",
			string(scheduleJSON), string(holidays))
		if err != nil {
			return fmt.Errorf("creating business hours: %w", err)
		}
		hoursID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("reading business hours id: %w", err)
		}

		res, err = tx.ExecContext(ctx, `
			INSERT INTO sla_policies
				(public_id, tenant_id, name, description, first_response_mins, resolution_mins,
				 business_hours_id, escalation_json, pause_on_statuses_json, is_default, is_active)
			VALUES (?,?,?,?,?,?,?,?,?,1,1)`,
			platform.NewULID(), tenantID, "Standard SLA",
			"4 hour first response, 48 hour resolution, measured in business hours.",
			240, 2880, hoursID, string(escalation), string(pause))
		if err != nil {
			return fmt.Errorf("creating sla policy: %w", err)
		}
		if slaID, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("reading sla id: %w", err)
		}
		return nil
	})

	return slaID, err
}

func seedDemoCategories(ctx context.Context, db *platform.DB, tenantID, slaID int64) error {
	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		for order, cat := range demoCategories {
			requires, err := json.Marshal(cat.RequiresFields)
			if err != nil {
				return fmt.Errorf("encoding required fields for %s: %w", cat.Key, err)
			}

			res, err := tx.ExecContext(ctx, `
				INSERT INTO categories
					(public_id, tenant_id, category_key, name, description, ticket_prefix,
					 color, sla_policy_id, requires_fields_json, is_active, sort_order)
				VALUES (?,?,?,?,?,?,?,?,?,1,?)`,
				platform.NewULID(), tenantID, cat.Key, cat.Name, cat.Description,
				cat.Prefix, cat.Color, slaID, string(requires), order)
			if err != nil {
				return fmt.Errorf("creating category %s: %w", cat.Key, err)
			}
			categoryID, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("reading category id: %w", err)
			}

			for fieldOrder, field := range cat.Fields {
				var options any
				if len(field.Options) > 0 {
					opts := make([]map[string]string, 0, len(field.Options))
					for _, o := range field.Options {
						opts = append(opts, map[string]string{
							"value": strings.ToUpper(strings.ReplaceAll(o, " ", "_")),
							"label": o,
						})
					}
					raw, err := json.Marshal(opts)
					if err != nil {
						return fmt.Errorf("encoding options for %s: %w", field.Key, err)
					}
					options = string(raw)
				}

				if _, err := tx.ExecContext(ctx, `
					INSERT INTO category_fields
						(public_id, tenant_id, category_id, field_key, label, field_type,
						 options_json, is_required, help_text, sort_order, is_active)
					VALUES (?,?,?,?,?,?,?,?,?,?,1)`,
					platform.NewULID(), tenantID, categoryID, field.Key, field.Label,
					field.Type, options, field.Required, nullIfEmpty(field.HelpText),
					fieldOrder); err != nil {
					return fmt.Errorf("creating field %s: %w", field.Key, err)
				}
			}

			if err := seedDefaultWorkflow(ctx, tx, tenantID, categoryID); err != nil {
				return err
			}
		}
		return nil
	})
}

// defaultTransitions is the shipped status machine. It is written to the
// database per category so a tenant can change it without a code change.
var defaultTransitions = []struct {
	From, To        string
	Label           string
	RequiresComment bool
	RequiresReason  bool
}{
	// New -> Open is the acceptance step: the helpdesk has seen it. This is
	// what stops the first-response clock.
	{"NEW", "OPEN", "Accept", false, false},
	{"NEW", "IN_PROGRESS", "Start working", false, false},
	{"NEW", "CANCELLED", "Cancel", true, true},

	{"OPEN", "IN_PROGRESS", "Start working", false, false},
	{"OPEN", "PENDING_HELPDESK", "Send to another department", true, false},
	{"OPEN", "PENDING_EMPLOYEE", "Ask the employee for more information", true, false},
	{"OPEN", "ESCALATED", "Escalate", true, false},
	{"OPEN", "CANCELLED", "Cancel", true, true},

	{"IN_PROGRESS", "PENDING_HELPDESK", "Send to another department", true, false},
	{"IN_PROGRESS", "PENDING_EMPLOYEE", "Ask the employee for more information", true, false},
	{"IN_PROGRESS", "ESCALATED", "Escalate", true, false},
	{"IN_PROGRESS", "RESOLVED", "Resolve", true, false},
	{"IN_PROGRESS", "CANCELLED", "Cancel", true, true},

	// Waiting on the employee. The SLA clock is paused in this state, which is
	// why it is distinct from PENDING_HELPDESK.
	{"PENDING_EMPLOYEE", "IN_PROGRESS", "Resume", false, false},
	{"PENDING_EMPLOYEE", "PENDING_HELPDESK", "Send to another department", true, false},
	{"PENDING_EMPLOYEE", "RESOLVED", "Resolve", true, false},
	{"PENDING_EMPLOYEE", "CLOSED", "Close", false, false},
	{"PENDING_EMPLOYEE", "CANCELLED", "Cancel", true, true},

	// Waiting on the helpdesk itself. The clock keeps running: this is exactly
	// the delay an SLA exists to measure.
	{"PENDING_HELPDESK", "IN_PROGRESS", "Resume", false, false},
	{"PENDING_HELPDESK", "PENDING_EMPLOYEE", "Ask the employee for more information", true, false},
	{"PENDING_HELPDESK", "ESCALATED", "Escalate", true, false},
	{"PENDING_HELPDESK", "RESOLVED", "Resolve", true, false},
	{"PENDING_HELPDESK", "CANCELLED", "Cancel", true, true},

	{"ESCALATED", "IN_PROGRESS", "Resume work", false, false},
	{"ESCALATED", "PENDING_EMPLOYEE", "Ask the employee for more information", true, false},
	{"ESCALATED", "RESOLVED", "Resolve", true, false},
	{"ESCALATED", "CANCELLED", "Cancel", true, true},

	{"RESOLVED", "CLOSED", "Close", false, false},
	{"RESOLVED", "REOPENED", "Reopen", true, true},
	{"CLOSED", "REOPENED", "Reopen", true, true},

	{"REOPENED", "IN_PROGRESS", "Start working", false, false},
	{"REOPENED", "PENDING_HELPDESK", "Send to another department", true, false},
	{"REOPENED", "PENDING_EMPLOYEE", "Ask the employee for more information", true, false},
	{"REOPENED", "ESCALATED", "Escalate", true, false},
	{"REOPENED", "RESOLVED", "Resolve", true, false},
}

var reopenReasons = []map[string]string{
	{"value": "NOT_RESOLVED", "label": "The issue was not resolved"},
	{"value": "RECURRED", "label": "The issue came back"},
	{"value": "PARTIAL", "label": "Only partly resolved"},
	{"value": "WRONG_RESOLUTION", "label": "Resolved incorrectly"},
}

var cancelReasons = []map[string]string{
	{"value": "DUPLICATE", "label": "Duplicate of another ticket"},
	{"value": "RAISED_IN_ERROR", "label": "Raised in error"},
	{"value": "NO_LONGER_NEEDED", "label": "No longer needed"},
	{"value": "OUT_OF_SCOPE", "label": "Outside the helpdesk's scope"},
}

func seedDefaultWorkflow(ctx context.Context, tx *sqlx.Tx, tenantID, categoryID int64) error {
	for _, t := range defaultTransitions {
		var reasons any
		switch t.To {
		case "REOPENED":
			raw, err := json.Marshal(reopenReasons)
			if err != nil {
				return fmt.Errorf("encoding reopen reasons: %w", err)
			}
			reasons = string(raw)
		case "CANCELLED":
			raw, err := json.Marshal(cancelReasons)
			if err != nil {
				return fmt.Errorf("encoding cancel reasons: %w", err)
			}
			reasons = string(raw)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO category_workflows
				(tenant_id, category_id, from_status, to_status, label,
				 requires_comment, requires_reason_code, reason_codes_json, is_active)
			VALUES (?,?,?,?,?,?,?,?,1)
			ON DUPLICATE KEY UPDATE label = VALUES(label)`,
			tenantID, categoryID, t.From, t.To, t.Label,
			t.RequiresComment, t.RequiresReason, reasons); err != nil {
			return fmt.Errorf("creating transition %s->%s: %w", t.From, t.To, err)
		}
	}
	return nil
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
