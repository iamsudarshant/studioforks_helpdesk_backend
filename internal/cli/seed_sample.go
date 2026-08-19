package cli

import (
	"context"
	"database/sql"
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

// This file builds the sample dataset the product is demonstrated and tested
// against: several live clients rather than one, the standard statutory
// departments, the entity catalogue mapped onto them, a knowledge base, and a
// few real Help requests.
//
// Everything here is idempotent. Each step looks for what it is about to write
// and updates instead of inserting, so `seed --sample` can be run against a
// database that already has some of it — which is what makes it usable as a
// repair as well as a first install.

// --- the standard organisation ----------------------------------------------

// standardDepartment is one statutory line every client gets.
//
// The type is what routing and reporting branch on; the name is only a label,
// so a client may rename "PF & Compliance" without breaking anything.
type standardDepartment struct {
	Code, Name, Type, Description string
}

var standardDepartments = []standardDepartment{
	{"PF", "PF & Compliance", "PF",
		"Provident fund: withdrawals, transfers, UAN and member service."},
	{"ESIC", "ESIC & Insurance", "ESIC",
		"Employees' State Insurance: registration, cards and dispensary."},
	{"GEN", "General Support", "GENERAL",
		"Anything that is not a statutory scheme query."},
	{"PAY", "Payroll", "PAYROLL",
		"Salary, deductions, reimbursements and Form 16."},
	{"HR", "Human Resources", "HR",
		"Employment records, letters and exit formalities."},
}

// standardEntity is one item of the service catalogue under a department.
//
// These are the entities the Organisation section lists: the specific things an
// employee raises a ticket about. Every one names the department it belongs to,
// because an entity with no department has no statutory line to route to — the
// API refuses to create one, and this list is what makes that rule concrete.
type standardEntity struct {
	Code, Name string
	// DepartmentType names the statutory line by type rather than by code,
	// because a client may rename or recode its departments but the type is
	// what routing branches on and is therefore stable.
	DepartmentType string
	Type           string
}

var standardEntities = []standardEntity{
	// Provident fund.
	{"PF-WDL", "PF Withdrawals", "PF", "CLAIM"},
	{"PF-TRF", "PF Transfers", "PF", "TRANSFER"},
	{"PF-UAN", "UAN Issues", "PF", "SERVICE"},
	{"PF-KYC", "KYC Updates", "PF", "SERVICE"},
	{"PF-PBK", "Member Passbook", "PF", "SERVICE"},
	{"PF-PEN", "Pension", "PF", "CLAIM"},
	{"PF-EDLI", "EDLI", "PF", "CLAIM"},
	{"PF-EMPC", "Employer Contributions", "PF", "COMPLIANCE"},
	{"PF-EXIT", "Exit Details", "PF", "SERVICE"},
	{"PF-SVC", "Service History", "PF", "SERVICE"},
	{"PF-CLM", "Claim Status", "PF", "CLAIM"},
	{"PF-COR", "Correction Requests", "PF", "SERVICE"},

	// Employees' State Insurance.
	{"ESI-REG", "ESIC Registration", "ESIC", "COMPLIANCE"},
	{"ESI-CARD", "ESIC Card", "ESIC", "SERVICE"},
	{"ESI-DISP", "ESIC Dispensary", "ESIC", "SERVICE"},
	{"ESI-CLM", "ESIC Claim Status", "ESIC", "CLAIM"},

	// Everything else.
	{"GEN-QRY", "General Query", "GENERAL", "SERVICE"},
	{"GEN-DOC", "Document Request", "GENERAL", "SERVICE"},
	{"PAY-SAL", "Salary & Deductions", "PAYROLL", "SERVICE"},
	{"PAY-F16", "Form 16", "PAYROLL", "SERVICE"},
	{"HR-LTR", "Employment Letters", "HR", "SERVICE"},
	{"HR-EXIT", "Exit Formalities", "HR", "SERVICE"},
}

// sampleClient is an extra client so that every cross-client screen — the
// dashboard with no client selected, the ticket list, the entity list — has
// more than one row to show. A product that is only ever demonstrated against
// one client hides exactly the bugs those screens have.
type sampleClient struct {
	Slug, Code, Name, LegalName, Industry, Prefix string
	City                                          string
	// The EPFO region and office code the client's establishment is registered
	// under. Every PF number this client's people hold starts with these two,
	// which is how real PF numbers work — they name the office that issued
	// them, not the member.
	PFRegion, PFOffice string
	Partners           []samplePerson
	Employees          []samplePerson
	Tickets            []sampleTicket
}

type samplePerson struct {
	First, Last, Email, EmployeeCode, Mobile, Designation string
	// Entity is the entity code this person is posted to, which is what a
	// partner's allocation and an employee's routing are both read from.
	EntityCode string
}

// statutoryIdentity derives a person's identifiers from their employee code.
//
// Deterministic on purpose: re-running the seed must produce the same numbers,
// or the second run would collide with the first on the unique UAN and PF
// indexes. The shapes match what the API validates — a 12-digit UAN, a slashed
// PF number, a 10-digit PAN, a 17-digit ESIC — so the sample data exercises the
// same rules real data does.
func statutoryIdentity(p samplePerson, region, office string, seq int) (uan, pf, esic, pan string) {
	// A per-person number, wide enough that three clients' worth of employees
	// never overlap.
	n := 100200300000 + int64(seq)*137

	uan = fmt.Sprintf("%012d", n)

	if region == "" {
		region = "MH"
	}
	if office == "" {
		office = "BAN"
	}
	// establishment / extension / member, in the form the portal prints.
	pf = fmt.Sprintf("%s/%s/%07d/%03d/%07d", region, office, 1000000+seq*7, 0, 2000000+seq*11)

	// 17 digits: the ESIC insurance number as issued.
	esic = fmt.Sprintf("%017d", 3100000000000000+int64(seq)*211)

	// PAN: five letters, four digits, one check letter. The fourth letter is P
	// for an individual and the fifth is the surname initial, as the real
	// format requires, so the sample data looks like what it stands in for.
	letters := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	at := func(i int) byte { return letters[((i%26)+26)%26] }
	surname := byte('X')
	if p.Last != "" {
		surname = strings.ToUpper(p.Last)[0]
	}
	pan = fmt.Sprintf("%c%c%c%c%c%04d%c",
		at(seq), at(seq*3), at(seq*5), 'P', surname,
		(seq*1237)%10000, at(seq*7))
	return uan, pf, esic, pan
}

type sampleTicket struct {
	Subject, Description, EntityCode, Status, Priority string
	// RequesterCode names the employee who raised it, so the ticket has a real
	// person behind it rather than an arbitrary id.
	RequesterCode string
	// AgeDays backdates the ticket, so the trend chart has a shape and the SLA
	// sweep has something to find.
	AgeDays int
}

var sampleClients = []sampleClient{
	{
		Slug: "zenith", Code: "ZEN001", Name: "Zenith Retail Group",
		LegalName: "Zenith Retail Group Private Limited",
		Industry:  "Retail", Prefix: "ZEN", City: "Bengaluru",
		PFRegion: "KN", PFOffice: "BNG",
		Partners: []samplePerson{
			{"Deepa", "Krishnan", "partner@zenith.local", "ZN-PA-001", "9800040001", "Head of HR", "PF-WDL"},
			{"Vivek", "Sharma", "exec@zenith.local", "ZN-PA-002", "9800040002", "HR Executive", "ESI-REG"},
		},
		Employees: []samplePerson{
			{"Nisha", "Reddy", "nisha.reddy@zenith.local", "ZN-EMP-001", "9800041001", "Store Manager", "PF-WDL"},
			{"Karthik", "Iyer", "karthik.iyer@zenith.local", "ZN-EMP-002", "9800041002", "Cashier", "PF-TRF"},
			{"Fatima", "Sheikh", "fatima.sheikh@zenith.local", "ZN-EMP-003", "9800041003", "Floor Supervisor", "ESI-CARD"},
			{"Rohit", "Malhotra", "rohit.malhotra@zenith.local", "ZN-EMP-004", "9800041004", "Warehouse Lead", "PF-UAN"},
		},
		Tickets: []sampleTicket{
			{"PF withdrawal pending for 6 weeks", "Form 19 was submitted on the portal but the claim has not moved past 'Under Process'. The member needs the funds for a medical expense.", "PF-WDL", "IN_PROGRESS", "HIGH", "ZN-EMP-001", 42},
			{"UAN not linked to Aadhaar", "The member's UAN shows a mismatch against Aadhaar and the KYC is stuck as pending with the employer.", "PF-UAN", "PENDING_EMPLOYEE", "MEDIUM", "ZN-EMP-004", 18},
			{"ESIC card not issued after registration", "Registration was completed two months ago but no card has been issued, so the member cannot use the dispensary.", "ESI-CARD", "OPEN", "MEDIUM", "ZN-EMP-003", 9},
			{"PF transfer from previous employer stalled", "Form 13 raised for a transfer-in; the previous employer has not attested it.", "PF-TRF", "PENDING_HELPDESK", "HIGH", "ZN-EMP-002", 25},
			{"Passbook not updating since April", "Contributions are being deducted but the member passbook shows no entries after April.", "PF-PBK", "NEW", "MEDIUM", "ZN-EMP-001", 3},
			{"Form 16 not received", "The member has not received Form 16 for the last financial year.", "PAY-F16", "RESOLVED", "LOW", "ZN-EMP-002", 60},
		},
	},
	{
		Slug: "orbit", Code: "ORB001", Name: "Orbit Logistics",
		LegalName: "Orbit Logistics India Private Limited",
		Industry:  "Transport & Logistics", Prefix: "ORB", City: "Pune",
		PFRegion: "MH", PFOffice: "PUN",
		Partners: []samplePerson{
			{"Sanjay", "Patil", "partner@orbit.local", "OR-PA-001", "9800050001", "Compliance Manager", "PF-WDL"},
		},
		Employees: []samplePerson{
			{"Anil", "Kumar", "anil.kumar@orbit.local", "OR-EMP-001", "9800051001", "Fleet Supervisor", "PF-WDL"},
			{"Meena", "Joseph", "meena.joseph@orbit.local", "OR-EMP-002", "9800051002", "Dispatch Coordinator", "ESI-DISP"},
			{"Suresh", "Nair", "suresh.nair@orbit.local", "OR-EMP-003", "9800051003", "Driver", "PF-PEN"},
		},
		Tickets: []sampleTicket{
			{"Pension claim rejected without reason", "Form 10D was rejected on the portal with no reason recorded against the claim.", "PF-PEN", "ESCALATED", "CRITICAL", "OR-EMP-003", 55},
			{"Dispensary allocation is in the wrong city", "The member has relocated to Pune but the ESIC dispensary is still the Mumbai one.", "ESI-DISP", "IN_PROGRESS", "MEDIUM", "OR-EMP-002", 12},
			{"Employer contribution missing for two months", "The ECR shows no contribution for the member for May and June.", "PF-EMPC", "OPEN", "HIGH", "OR-EMP-001", 20},
			{"Exit date not marked by employer", "The member left in March; the exit date is still not marked, which is blocking the withdrawal.", "PF-EXIT", "PENDING_EMPLOYEE", "MEDIUM", "OR-EMP-001", 33},
			{"Service history shows a gap", "The service history has a six-month gap that does not match the employment record.", "PF-SVC", "CLOSED", "LOW", "OR-EMP-003", 75},
		},
	},
}

// --- entry point ------------------------------------------------------------

// seedSample installs the sample dataset over whatever already exists.
func seedSample(ctx context.Context, db *platform.DB, cfg *config.Config) error {
	clients, err := clientTenantIDs(ctx, db)
	if err != nil {
		return err
	}

	// The extra clients first, so the structure pass below covers them too.
	for _, sc := range sampleClients {
		if err := ensureSampleClient(ctx, db, cfg, sc); err != nil {
			return fmt.Errorf("seeding client %s: %w", sc.Slug, err)
		}
	}

	// Re-read: the loop above may have created clients.
	clients, err = clientTenantIDs(ctx, db)
	if err != nil {
		return err
	}

	for _, c := range clients {
		if err := ensureStandardDepartments(ctx, db, c.ID); err != nil {
			return fmt.Errorf("departments for %s: %w", c.Slug, err)
		}
		if err := ensureStandardEntities(ctx, db, c.ID); err != nil {
			return fmt.Errorf("entities for %s: %w", c.Slug, err)
		}
		if err := ensureFAQ(ctx, db, c.ID); err != nil {
			return fmt.Errorf("FAQ for %s: %w", c.Slug, err)
		}
	}

	// The staff workspace gets the FAQ too.
	//
	// `clientTenantIDs` deliberately excludes it — it is not a client — but Help
	// is a screen every role has, and an admin or agent opening it saw nothing
	// at all. They need the same answers: they are the ones quoting them back
	// to a caller.
	if platformID, err := platformTenantID(ctx, db); err == nil {
		if err := ensureFAQArticles(ctx, db, platformID, staffFAQ); err != nil {
			return fmt.Errorf("FAQ for the staff workspace: %w", err)
		}
		// An earlier version of this seed put the client-facing articles here
		// too, which left an agent reading "How do I raise a ticket?" beside
		// "Why can I not assign this ticket?". Staff read what a client's people
		// read by selecting that client, so those belong in the client's own
		// workspace and nowhere else.
		if err := retireFAQArticles(ctx, db, platformID, sampleFAQ); err != nil {
			return fmt.Errorf("tidying the staff FAQ: %w", err)
		}
	}

	if err := ensureNotificationTemplates(ctx, db); err != nil {
		return err
	}
	if err := ensureHelpRequests(ctx, db); err != nil {
		return err
	}

	fmt.Printf("sample data ready across %d client(s)\n", len(clients))
	return nil
}

type clientRow struct {
	ID   int64  `db:"id"`
	Slug string `db:"slug"`
	Name string `db:"name"`
}

// clientTenantIDs lists the real clients — never the platform workspace, which
// holds ComplyDesk's own staff and has no organisation structure of its own.
func clientTenantIDs(ctx context.Context, db *platform.DB) ([]clientRow, error) {
	rows := []clientRow{}
	err := db.Primary.SelectContext(ctx, &rows,
		`SELECT id, slug, name FROM tenants
		 WHERE deleted_at IS NULL AND is_platform = 0 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing clients: %w", err)
	}
	return rows, nil
}

// --- organisation structure -------------------------------------------------

// ensureStandardDepartments gives a client the statutory lines, and corrects the
// type of any it already has under a different name.
func ensureStandardDepartments(ctx context.Context, db *platform.DB, tenantID int64) error {
	for _, d := range standardDepartments {
		// Match on type rather than code: a client that already has a PF line
		// called something else should be corrected, not given a second one.
		// Soft-deleted rows count here too: the unique index on
		// (tenant_id, code) does not exclude them, so a removed department
		// still holds its code and the insert would collide with it.
		var existing int64
		err := db.Primary.GetContext(ctx, &existing,
			`SELECT id FROM departments
			 WHERE tenant_id = ? AND (type = ? OR code = ?)
			 ORDER BY (deleted_at IS NULL) DESC, (type = ?) DESC LIMIT 1`,
			tenantID, d.Type, d.Code, d.Type)

		switch {
		case err == nil:
			if _, err := db.Primary.ExecContext(ctx,
				`UPDATE departments SET type = ?, is_active = 1, deleted_at = NULL WHERE id = ?`,
				d.Type, existing); err != nil {
				return fmt.Errorf("updating department %s: %w", d.Code, err)
			}
		case platform.IsNotFound(err):
			if _, err := db.Primary.ExecContext(ctx, `
				INSERT INTO departments (public_id, tenant_id, code, name, type, is_active)
				VALUES (?,?,?,?,?,1)`,
				platform.NewULID(), tenantID, d.Code, d.Name, d.Type); err != nil {
				return fmt.Errorf("creating department %s: %w", d.Code, err)
			}
		default:
			return fmt.Errorf("looking up department %s: %w", d.Code, err)
		}
	}
	return nil
}

// ensureStandardEntities installs the service catalogue, each item mapped to the
// department that owns it.
//
// The mapping is the point: an entity with no department cannot be routed, and
// the API refuses to create one. Existing rows are repaired rather than skipped,
// because the column was added nullable and older data has none.
func ensureStandardEntities(ctx context.Context, db *platform.DB, tenantID int64) error {
	deptIDs := map[string]int64{}
	rows := []struct {
		ID   int64  `db:"id"`
		Type string `db:"type"`
		Code string `db:"code"`
	}{}
	if err := db.Primary.SelectContext(ctx, &rows,
		`SELECT id, type, code FROM departments WHERE tenant_id = ? AND deleted_at IS NULL`,
		tenantID); err != nil {
		return fmt.Errorf("loading departments: %w", err)
	}
	for _, r := range rows {
		// Key by both, so an entity can name either the standard code or the
		// type it belongs to.
		deptIDs[r.Type] = r.ID
		deptIDs[r.Code] = r.ID
	}

	for _, e := range standardEntities {
		deptID, ok := deptIDs[e.DepartmentType]
		if !ok {
			return fmt.Errorf("entity %s names the %s department, which does not exist", e.Code, e.DepartmentType)
		}

		// Soft-deleted rows count. The unique index is on (tenant_id, code)
		// with no deleted_at in it, so a code that has been removed still
		// occupies the slot — looking only at live rows sent the seed down the
		// insert path and straight into a duplicate-key error, which is what
		// made re-running it fail on a database anyone had tidied up.
		var existing struct {
			ID      int64        `db:"id"`
			Deleted sql.NullTime `db:"deleted_at"`
		}
		err := db.Primary.GetContext(ctx, &existing,
			`SELECT id, deleted_at FROM entities WHERE tenant_id = ? AND code = ?`,
			tenantID, e.Code)

		switch {
		case err == nil:
			// Reviving a removed one is right here: the standard catalogue is
			// meant to be present, and the seed is what asserts that.
			if _, err := db.Primary.ExecContext(ctx,
				`UPDATE entities SET name = ?, type = ?, department_id = ?, is_active = 1,
				        deleted_at = NULL
				 WHERE id = ?`,
				e.Name, e.Type, deptID, existing.ID); err != nil {
				return fmt.Errorf("updating entity %s: %w", e.Code, err)
			}
		case platform.IsNotFound(err):
			if _, err := db.Primary.ExecContext(ctx, `
				INSERT INTO entities (public_id, tenant_id, code, name, type, department_id, is_active)
				VALUES (?,?,?,?,?,?,1)`,
				platform.NewULID(), tenantID, e.Code, e.Name, e.Type, deptID); err != nil {
				return fmt.Errorf("creating entity %s: %w", e.Code, err)
			}
		default:
			return fmt.Errorf("looking up entity %s: %w", e.Code, err)
		}
	}

	// Anything that predates the catalogue — a legal establishment created
	// before entities carried a department — is pointed at the client's General
	// line rather than left unroutable.
	if general, ok := deptIDs["GENERAL"]; ok {
		if _, err := db.Primary.ExecContext(ctx,
			`UPDATE entities SET department_id = ?
			 WHERE tenant_id = ? AND department_id IS NULL AND deleted_at IS NULL`,
			general, tenantID); err != nil {
			return fmt.Errorf("mapping unassigned entities: %w", err)
		}
	}
	return nil
}

// --- the extra clients ------------------------------------------------------

func ensureSampleClient(ctx context.Context, db *platform.DB, cfg *config.Config, sc sampleClient) error {
	repo := tenant.NewRepository(db)
	userRepo := user.NewRepository(db)
	hasher := auth.NewHasher(cfg.Auth)

	// Read the row directly rather than through BySlug, and without the
	// soft-delete filter: these two shipped archived and soft-deleted, so every
	// ordinary lookup reports them missing while the unique slug still collides
	// on create. Restoring the existing row is also the honest thing to do —
	// creating a second Zenith would orphan whatever was already attached to
	// the first.
	var tenantID int64
	err := db.Primary.GetContext(ctx, &tenantID,
		`SELECT id FROM tenants WHERE slug = ?`, sc.Slug)

	var t *tenant.Tenant
	switch {
	case err == nil:
		if _, uerr := db.Primary.ExecContext(ctx,
			`UPDATE tenants SET deleted_at = NULL WHERE id = ?`, tenantID); uerr != nil {
			return fmt.Errorf("restoring workspace: %w", uerr)
		}
		t, err = repo.ByID(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("loading workspace: %w", err)
		}
	case platform.IsNotFound(err):
		contractStart := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
		contractEnd := time.Date(2028, 6, 30, 0, 0, 0, 0, time.UTC)

		t, err = repo.Create(ctx, tenant.CreateParams{
			Slug: sc.Slug, ClientCode: sc.Code, Name: sc.Name, LegalName: sc.LegalName,
			Industry: sc.Industry, Timezone: "Asia/Kolkata", Locale: "en-IN",
			TicketPrefix:  sc.Prefix,
			ContactEmail:  "helpdesk@" + sc.Slug + ".example",
			ContactPhone:  "02040001234",
			Address:       sc.Name + ", " + sc.City,
			ContractStart: &contractStart, ContractEnd: &contractEnd,
		})
		if err != nil {
			return fmt.Errorf("creating workspace: %w", err)
		}
	default:
		return fmt.Errorf("looking up workspace: %w", err)
	}

	// A sample client is only useful live. These two shipped as ARCHIVED, which
	// hid them from every list the sample data is meant to populate.
	if err := repo.SetStatus(ctx, t.ID, tenant.StatusActive); err != nil {
		return fmt.Errorf("activating workspace: %w", err)
	}

	if err := ensureStandardDepartments(ctx, db, t.ID); err != nil {
		return err
	}
	if err := ensureStandardEntities(ctx, db, t.ID); err != nil {
		return err
	}
	if err := enableModulesForTenant(ctx, db, t.ID); err != nil {
		return fmt.Errorf("enabling modules: %w", err)
	}

	// The taxonomy is only installed once. The platform backfill in Seed() also
	// creates these, and seedDemoCategories inserts unconditionally, so running
	// it against a client that already has categories collides on the unique
	// (tenant, key) pair rather than being the no-op the rest of this file is.
	var categoryCount int64
	if err := db.Primary.GetContext(ctx, &categoryCount,
		`SELECT COUNT(*) FROM categories WHERE tenant_id = ? AND deleted_at IS NULL`, t.ID); err != nil {
		return fmt.Errorf("counting categories: %w", err)
	}
	if categoryCount == 0 {
		slaID, err := seedDemoSLA(ctx, db, t.ID)
		if err != nil {
			return fmt.Errorf("seeding SLA: %w", err)
		}
		if err := seedDemoCategories(ctx, db, t.ID, slaID); err != nil {
			return fmt.Errorf("seeding categories: %w", err)
		}
	}

	hash, err := hasher.Hash("ComplyDesk@2026")
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}

	// The agents who cover this client. Priya deliberately owns the extra
	// clients that Arjun does not, so "an agent assigned to specific clients
	// sees those clients" has something to be true of.
	if agentID, err := optionalStaffID(ctx, db, "agent.priya@complydesk.local"); err == nil && agentID != nil {
		if err := userRepo.AssignAgent(ctx, *agentID, t.ID, true, nil); err != nil {
			return fmt.Errorf("assigning agent: %w", err)
		}
	}

	entityIDs, err := entityIDsByCode(ctx, db, t.ID)
	if err != nil {
		return err
	}

	people := map[string]int64{}
	// `seq` numbers everyone this client has, so the derived identifiers are
	// unique within it and stable across re-runs.
	seq := 0
	add := func(p samplePerson, roleKey string) error {
		seq++
		id, err := ensurePerson(ctx, db, userRepo, t.ID, p, roleKey, hash, entityIDs, sc, seq)
		if err != nil {
			return err
		}
		people[p.EmployeeCode] = id
		return nil
	}
	for _, p := range sc.Partners {
		if err := add(p, user.RolePartner); err != nil {
			return err
		}
	}
	for _, p := range sc.Employees {
		if err := add(p, user.RoleEmployee); err != nil {
			return err
		}
	}

	return ensureSampleTickets(ctx, db, t.ID, sc, people, entityIDs)
}

// ensurePerson creates a user if they are absent and always reasserts their
// role and posting, so a partly-seeded database converges.
func ensurePerson(ctx context.Context, db *platform.DB, userRepo *user.Repository,
	tenantID int64, p samplePerson, roleKey, hash string, entityIDs map[string]int64,
	sc sampleClient, seq int) (int64, error) {

	uan, pf, esic, pan := statutoryIdentity(p, sc.PFRegion, sc.PFOffice, seq)
	// Ages and service lengths that vary, so "joined in the last year" and
	// "approaching retirement" both have someone to be true of.
	dob := time.Date(1968+(seq*3)%30, time.Month(1+(seq*5)%12), 1+(seq*7)%28, 0, 0, 0, 0, time.UTC)
	doj := time.Date(2012+(seq*2)%13, time.Month(1+(seq*3)%12), 1+(seq*11)%28, 0, 0, 0, 0, time.UTC)

	var id int64
	err := db.Primary.GetContext(ctx, &id,
		`SELECT id FROM users WHERE tenant_id = ? AND email = ? AND deleted_at IS NULL`,
		tenantID, p.Email)

	if platform.IsNotFound(err) {
		created, cerr := userRepo.Create(ctx, tenantID, user.CreateParams{
			EmployeeCode: p.EmployeeCode, FirstName: p.First, LastName: p.Last,
			Email: p.Email, Mobile: p.Mobile, Designation: p.Designation,
			PANNumber: pan, UANNumber: uan, PFNumber: pf, ESICNumber: esic,
			DateOfBirth: &dob, DateOfJoining: &doj,
			PasswordHash: hash, Status: user.StatusActive,
		})
		if cerr != nil {
			return 0, fmt.Errorf("creating %s: %w", p.Email, cerr)
		}
		id = created.ID
	} else if err != nil {
		return 0, fmt.Errorf("looking up %s: %w", p.Email, err)
	}

	// Reasserted for someone who already existed, so a database seeded before
	// these fields were carried gains them on the next run rather than staying
	// half-populated. COALESCE leaves anything already set alone: a real edit
	// made through the UI is not overwritten by the seed.
	if _, err := db.Primary.ExecContext(ctx, `
		UPDATE users
		SET pan_number      = COALESCE(pan_number, ?),
		    uan_number      = COALESCE(uan_number, ?),
		    pf_number       = COALESCE(pf_number, ?),
		    esic_number     = COALESCE(esic_number, ?),
		    date_of_birth   = COALESCE(date_of_birth, ?),
		    date_of_joining = COALESCE(date_of_joining, ?),
		    designation     = COALESCE(designation, ?)
		WHERE id = ?`,
		pan, uan, pf, esic, dob, doj, nullIfBlank(p.Designation), id); err != nil {
		return 0, fmt.Errorf("filling statutory identity for %s: %w", p.Email, err)
	}

	role, err := userRepo.RoleByKey(ctx, tenantID, roleKey)
	if err != nil {
		return 0, fmt.Errorf("loading role %s: %w", roleKey, err)
	}
	if err := userRepo.SetRoles(ctx, tenantID, id, []int64{role.ID}, nil); err != nil {
		return 0, fmt.Errorf("assigning role to %s: %w", p.Email, err)
	}

	// The posting is what a partner's allocation and an employee's routing are
	// both read from, so it is set even for a user that already existed.
	//
	// The department comes from the entity rather than being named separately:
	// every entity already belongs to exactly one department — PF Withdrawals
	// to PF, ESIC Card to ESIC — so naming it twice would only create a way for
	// the two to disagree.
	if entityID, ok := entityIDs[p.EntityCode]; ok {
		if _, err := db.Primary.ExecContext(ctx, `
			UPDATE users u
			JOIN entities e ON e.id = ?
			SET u.entity_id = e.id, u.department_id = COALESCE(u.department_id, e.department_id)
			WHERE u.id = ?`, entityID, id); err != nil {
			return 0, fmt.Errorf("posting %s: %w", p.Email, err)
		}
	}
	return id, nil
}

// nullIfBlank keeps COALESCE honest: an empty string is a value, and would
// overwrite a real designation with nothing.
func nullIfBlank(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func entityIDsByCode(ctx context.Context, db *platform.DB, tenantID int64) (map[string]int64, error) {
	rows := []struct {
		ID   int64  `db:"id"`
		Code string `db:"code"`
	}{}
	if err := db.Primary.SelectContext(ctx, &rows,
		`SELECT id, code FROM entities WHERE tenant_id = ? AND deleted_at IS NULL`, tenantID); err != nil {
		return nil, fmt.Errorf("loading entities: %w", err)
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Code] = r.ID
	}
	return out, nil
}

func optionalStaffID(ctx context.Context, db *platform.DB, email string) (*int64, error) {
	var id int64
	err := db.Primary.GetContext(ctx, &id,
		`SELECT id FROM users WHERE email = ? AND deleted_at IS NULL LIMIT 1`, email)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// --- tickets ----------------------------------------------------------------

// ensureSampleTickets gives a client a spread of real tickets: several statuses,
// several priorities, and ages that make the trend chart and the SLA sweep
// meaningful rather than a flat line at today.
func ensureSampleTickets(ctx context.Context, db *platform.DB, tenantID int64,
	sc sampleClient, people map[string]int64, entityIDs map[string]int64) error {

	// A category is required by the schema. The client's default one is enough:
	// the entity is what actually classifies these.
	var categoryID int64
	if err := db.Primary.GetContext(ctx, &categoryID,
		`SELECT id FROM categories WHERE tenant_id = ? AND deleted_at IS NULL
		 ORDER BY parent_id IS NOT NULL, id LIMIT 1`, tenantID); err != nil {
		return fmt.Errorf("loading a category: %w", err)
	}

	agentID, _ := optionalStaffID(ctx, db, "agent.priya@complydesk.local")

	for i, st := range sc.Tickets {
		requesterID, ok := people[st.RequesterCode]
		if !ok {
			continue
		}

		number := fmt.Sprintf("%s-%s-2026-%06d", sc.Code, entityPrefix(st.EntityCode), i+1)

		var existing int64
		err := db.Primary.GetContext(ctx, &existing,
			`SELECT id FROM tickets WHERE tenant_id = ? AND ticket_number = ?`, tenantID, number)
		if err == nil {
			continue // already seeded
		}
		if !platform.IsNotFound(err) {
			return fmt.Errorf("looking up ticket %s: %w", number, err)
		}

		createdAt := time.Now().UTC().AddDate(0, 0, -st.AgeDays)
		var entityID any
		if id, ok := entityIDs[st.EntityCode]; ok {
			entityID = id
		}

		// A ticket that has moved past NEW has been picked up by someone; one
		// that has not, has not. Assigning every ticket would make the
		// "unassigned" filter permanently empty.
		var assignee any
		if agentID != nil && st.Status != "NEW" {
			assignee = *agentID
		}

		var resolvedAt, closedAt any
		if st.Status == "RESOLVED" || st.Status == "CLOSED" {
			resolvedAt = createdAt.AddDate(0, 0, 4)
		}
		if st.Status == "CLOSED" {
			closedAt = createdAt.AddDate(0, 0, 6)
		}

		err = db.InTx(ctx, func(tx *sqlx.Tx) error {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO tickets
					(public_id, tenant_id, ticket_number, category_id, subject, description,
					 status, priority, source, requester_id, entity_id, assignee_id,
					 resolved_at, closed_at, last_activity_at, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?,'WEB',?,?,?,?,?,?,?,?)`,
				platform.NewULID(), tenantID, number, categoryID, st.Subject, st.Description,
				st.Status, st.Priority, requesterID, entityID, assignee,
				resolvedAt, closedAt, createdAt, createdAt, createdAt)
			if err != nil {
				return fmt.Errorf("creating ticket %s: %w", number, err)
			}
			ticketID, err := res.LastInsertId()
			if err != nil {
				return err
			}

			// The employee's own words, then the desk's reply, so the
			// conversation view has something in it.
			if err := insertConversation(ctx, tx, tenantID, ticketID, requesterID,
				"PUBLIC", st.Description, createdAt); err != nil {
				return err
			}
			if agentID != nil && st.Status != "NEW" {
				reply := "Thank you for raising this. We have taken it up with the field office and will update you as soon as we hear back."
				if err := insertConversation(ctx, tx, tenantID, ticketID, *agentID,
					"PUBLIC", reply, createdAt.Add(6*time.Hour)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// entityPrefix turns "PF-WDL" into "PF" for the ticket number.
func entityPrefix(code string) string {
	if i := strings.Index(code, "-"); i > 0 {
		return code[:i]
	}
	return code
}

// --- knowledge base ---------------------------------------------------------

type faqSeed struct {
	Section, Question, Answer string
	Order                     int
}

var sampleFAQ = []faqSeed{
	{"TICKETS", "How do I raise a ticket?",
		"Open <strong>Raise a ticket</strong> from the sidebar, choose the entity your query is about — PF Withdrawals, ESIC Card, and so on — and describe the problem. Attach any documents you already have; it usually saves a round trip.", 1},
	{"TICKETS", "How long will my ticket take?",
		"Every ticket carries an SLA based on its priority. You can see the due date on the ticket itself. Statutory queries that depend on a field office can take longer than the SLA, and the helpdesk will tell you when that happens rather than letting the clock run out silently.", 2},
	{"TICKETS", "What do the ticket statuses mean?",
		"<strong>New</strong> — raised, not yet reviewed. <strong>Open</strong> — the helpdesk has accepted it. <strong>In progress</strong> — actively being worked. <strong>Awaiting you</strong> — we need something from you before we can continue. <strong>Awaiting helpdesk</strong> — the ball is with us. <strong>Resolved</strong> — done, and you can reopen it if it was not. <strong>Closed</strong> — settled.", 3},
	{"TICKETS", "Can I reopen a ticket after it is resolved?",
		"Yes, within the reopen window your organisation has configured. Open the ticket and choose <strong>Reopen</strong>; it returns to the same team with its history intact.", 4},

	{"PF", "My PF withdrawal has been pending for weeks. What now?",
		"Raise a ticket under <strong>PF Withdrawals</strong> with your UAN and the claim ID from the EPFO portal. The helpdesk follows it up with the field office and records each response on the ticket.", 1},
	{"PF", "Why does my passbook show no recent contributions?",
		"Passbook entries appear only after the employer's ECR is filed and reconciled, which usually lags by a few weeks. If more than two months are missing, raise it under <strong>Employer Contributions</strong>.", 2},
	{"PF", "How do I correct my name or date of birth?",
		"Raise a ticket under <strong>Correction Requests</strong> and attach the supporting document — Aadhaar or PAN for a name, and a birth certificate or school record for a date of birth. Corrections are a joint request between you and the employer.", 3},
	{"PF", "What is the difference between a transfer and a withdrawal?",
		"A <strong>transfer</strong> moves your accumulated balance from a previous employer into your current account and keeps the service continuous. A <strong>withdrawal</strong> takes the money out and breaks that continuity, which affects pension eligibility.", 4},

	{"ESIC", "I have not received my ESIC card.",
		"Raise a ticket under <strong>ESIC Card</strong> with your insurance number. If registration was completed but no card issued, the helpdesk chases it with the branch office.", 1},
	{"ESIC", "My dispensary is in the wrong city.",
		"Raise it under <strong>ESIC Dispensary</strong> with your current address. A dispensary change needs the employer's endorsement, which the helpdesk arranges.", 2},

	{"ACCOUNT", "I cannot sign in.",
		"Use <strong>Forgot password</strong> on the sign-in screen. Employees can also sign in with an employee code, mobile number, UAN or PAN instead of an email address. If the account is locked after repeated failures, it unlocks automatically, or your administrator can unlock it.", 1},
	{"ACCOUNT", "How do I change what I am notified about?",
		"Open <strong>Preferences → Notifications</strong>. Each event can be switched on or off per channel. Channels your organisation has not enabled are shown greyed out rather than hidden, so you can see what exists.", 2},
	{"ACCOUNT", "Who can see my tickets?",
		"You, the helpdesk staff working them, and the administrators of your organisation whose remit covers your establishment. Other employees cannot see your tickets.", 3},

	// For the client's own administrators. They read the same FAQ as their
	// employees — it is one article set per client — so their questions sit in
	// their own section rather than in a second list nobody would find.
	{"PARTNERS", "Can I raise a ticket for one of my employees?",
		"Yes. Choose <strong>Raise a ticket</strong> and search for the employee in the <strong>Raised for</strong> field. The ticket is recorded against them, flagged as raised on your behalf, and they can follow it themselves.", 1},
	{"PARTNERS", "Why can I see my employees but not edit them?",
		"The employee master is maintained by the helpdesk, so that statutory identifiers stay consistent with what has been filed. You can see everything, including UAN, PF and ESIC numbers. To change something, raise a ticket under <strong>Correction Requests</strong> and it is amended at source.", 2},
	{"PARTNERS", "Which employees and entities can I see?",
		"Whatever your account has been allocated. A client administrator sees the whole organisation; an executive sees only the entities allocated to them. If something you expect is missing, it is an allocation to be widened rather than a fault — ask the helpdesk.", 3},
	{"PARTNERS", "Can I see the helpdesk's internal notes?",
		"No. Notes marked internal are between the helpdesk staff working the ticket. Everything addressed to you or your employee appears in the conversation.", 4},
	{"PARTNERS", "How do I get a list of my organisation's tickets?",
		"Open <strong>Reports</strong>, choose the report and the period, and download it as CSV, Excel or PDF. The ticket list itself also exports whatever you have filtered it to.", 5},
	{"PARTNERS", "Can I switch an entity off?",
		"No — switching an establishment off hides its tickets from everybody, so it is a helpdesk action. Ask the helpdesk and it is done with the effect explained first.", 6},
}

// staffFAQ is what ComplyDesk's own people read: how to work the desk, rather
// than how to ask it for something. Seeded into the platform workspace, which
// is the only FAQ an admin or agent sees when no client is selected.
var staffFAQ = []faqSeed{
	{"DESK", "Why can I not assign a ticket to a particular agent?",
		"An agent works the clients their remit covers. An agent with no clients assigned covers every client; an agent with clients assigned covers exactly those. The assign menu offers only the agents whose remit covers that ticket's client — widen it under <strong>Clients → Agents covering this client</strong>.", 1},
	{"DESK", "What is the difference between assigning and transferring?",
		"<strong>Assign</strong> gives the ticket to a person. <strong>Transfer</strong> moves it to a department, for when the query belongs to another statutory line rather than another individual. <strong>Escalate</strong> raises it without changing who holds it.", 2},
	{"DESK", "When should a note be internal?",
		"Whenever it is not addressed to the requester: what you found on the EPFO portal, what you suspect, what you are waiting on. Internal notes are visible to admins and agents only — never to the client's partners or to the employee.", 3},
	{"DESK", "The employee's statutory details look wrong. Where do I correct them?",
		"On the person's record under <strong>Users</strong>. The ticket rail shows the same identifiers so you can check them without leaving the ticket, but the record is the source.", 4},
	{"DESK", "What stops the SLA clock?",
		"Only <strong>Awaiting employee</strong>. Waiting on the helpdesk is exactly what an SLA measures, so <strong>Awaiting helpdesk</strong> keeps the clock running.", 5},

	{"CLIENTS", "How do I add a client?",
		"<strong>Clients → Add client</strong>. The name fills in the workspace address, which becomes part of the portal URL and cannot be changed afterwards. Everything else can.", 1},
	{"CLIENTS", "What happens when I delete a client?",
		"It is taken offline and archived — everyone signed in loses access immediately, and its tickets stop appearing. Nothing is erased, and an administrator can restore it. The confirmation says so before you commit.", 2},
	{"CLIENTS", "Who decides which agent covers which client?",
		"An administrator, under <strong>Clients → Agents covering this client</strong>. Leaving a client with no agents assigned means every agent covers it.", 3},

	{"USERS", "What password does a new user get?",
		"Their PAN and year of birth, lowercase — <code>abcde1234f@1990</code>. It is shown once when the account is created, with a copy button, and they must change it when they first sign in.", 1},
	{"USERS", "Can two people share a PAN?",
		"Two employees of the same client cannot — that is a data entry error which would merge two people's statutory records. An agent or a partner may share a PAN with an employee, because one person legitimately holds more than one account.", 2},
	{"USERS", "What is the difference between an agent, a partner and an employee?",
		"An <strong>agent</strong> is ComplyDesk staff and works across clients. A <strong>partner</strong> administers one client and sees its people and tickets without editing them. An <strong>employee</strong> raises and follows their own queries.", 3},

	{"REPORTS", "Why does a report show fewer tickets than I expect?",
		"Every report is scoped to what you can see. An agent's report covers the clients they are assigned to; a partner's covers their own client. Check the client selector before comparing two numbers.", 1},
}

// platformTenantID finds ComplyDesk's own workspace, where staff accounts live.
func platformTenantID(ctx context.Context, db *platform.DB) (int64, error) {
	var id int64
	err := db.Primary.GetContext(ctx, &id,
		`SELECT id FROM tenants WHERE is_platform = 1 AND deleted_at IS NULL LIMIT 1`)
	return id, err
}

// retireFAQArticles removes articles this seed installed in the wrong
// workspace, matched on the exact question so nothing hand-written is touched.
func retireFAQArticles(ctx context.Context, db *platform.DB, tenantID int64, articles []faqSeed) error {
	for _, a := range articles {
		if _, err := db.Primary.ExecContext(ctx,
			`DELETE FROM faq_articles WHERE tenant_id = ? AND question = ?`,
			tenantID, a.Question); err != nil {
			return fmt.Errorf("retiring FAQ %q: %w", a.Question, err)
		}
	}
	return nil
}

// ensureFAQ installs a client's article set.
func ensureFAQ(ctx context.Context, db *platform.DB, tenantID int64) error {
	return ensureFAQArticles(ctx, db, tenantID, sampleFAQ)
}

// ensureFAQArticles writes one set of articles into one workspace, matching on
// the question so a re-run corrects an answer rather than duplicating it.
func ensureFAQArticles(ctx context.Context, db *platform.DB, tenantID int64, articles []faqSeed) error {
	for _, a := range articles {
		var existing int64
		err := db.Primary.GetContext(ctx, &existing,
			`SELECT id FROM faq_articles WHERE tenant_id = ? AND question = ? AND deleted_at IS NULL`,
			tenantID, a.Question)

		switch {
		case err == nil:
			if _, err := db.Primary.ExecContext(ctx,
				`UPDATE faq_articles SET section = ?, answer = ?, sort_order = ?, is_active = 1 WHERE id = ?`,
				a.Section, a.Answer, a.Order, existing); err != nil {
				return fmt.Errorf("updating FAQ: %w", err)
			}
		case platform.IsNotFound(err):
			if _, err := db.Primary.ExecContext(ctx, `
				INSERT INTO faq_articles (public_id, tenant_id, section, question, answer, sort_order, is_active)
				VALUES (?,?,?,?,?,?,1)`,
				platform.NewULID(), tenantID, a.Section, a.Question, a.Answer, a.Order); err != nil {
				return fmt.Errorf("creating FAQ: %w", err)
			}
		default:
			return fmt.Errorf("looking up FAQ: %w", err)
		}
	}
	return nil
}

// --- help requests ----------------------------------------------------------

type helpSeed struct {
	Email, Subject, Category, Body, Status, Priority string
	Reply                                            string
}

var sampleHelpRequests = []helpSeed{
	{"employee@demo.local", "Cannot download an attachment on my ticket", "BUG",
		"I opened ticket AMP001-PF-2026-000003 and clicked the attachment, but nothing downloads. I have tried Chrome and Edge.",
		"IN_PROGRESS", "HIGH",
		"Thanks for reporting this — we can reproduce it and a fix is going out this week. In the meantime the preview button on the same row does work."},
	{"partner@demo.local", "Please add a new establishment to our account", "REQUEST",
		"We have registered a new establishment, Ampersand Retail Pvt Ltd, and need it added so we can raise tickets against it.",
		"OPEN", "NORMAL", ""},
	{"employee@demo.local", "How do I see my PF passbook here?", "QUESTION",
		"Is the passbook visible inside ComplyDesk, or do I still go to the EPFO portal for it?",
		"RESOLVED", "LOW",
		"The passbook itself stays on the EPFO portal — ComplyDesk tracks the query you raise about it. If the passbook is not updating, raise a ticket under Member Passbook and we will follow it up."},
}

func ensureHelpRequests(ctx context.Context, db *platform.DB) error {
	for _, h := range sampleHelpRequests {
		var requester struct {
			ID       int64 `db:"id"`
			TenantID int64 `db:"tenant_id"`
		}
		err := db.Primary.GetContext(ctx, &requester,
			`SELECT id, tenant_id FROM users WHERE email = ? AND deleted_at IS NULL LIMIT 1`, h.Email)
		if platform.IsNotFound(err) {
			continue // that sample user is not installed; nothing to attach to
		}
		if err != nil {
			return fmt.Errorf("looking up %s: %w", h.Email, err)
		}

		var existing int64
		err = db.Primary.GetContext(ctx, &existing,
			`SELECT id FROM help_tickets WHERE tenant_id = ? AND subject = ? AND deleted_at IS NULL`,
			requester.TenantID, h.Subject)
		if err == nil {
			continue
		}
		if !platform.IsNotFound(err) {
			return fmt.Errorf("looking up help request: %w", err)
		}

		var resolvedBy any
		var resolvedAt any
		staffID, serr := optionalStaffID(ctx, db, "agent.arjun@complydesk.local")
		if h.Status == "RESOLVED" && serr == nil && staffID != nil {
			resolvedBy = *staffID
			resolvedAt = time.Now().UTC().AddDate(0, 0, -2)
		}

		res, err := db.Primary.ExecContext(ctx, `
			INSERT INTO help_tickets
				(public_id, tenant_id, client_id, requester_id, subject, category, body,
				 status, priority, resolved_by, resolved_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			platform.NewULID(), requester.TenantID, requester.TenantID, requester.ID,
			h.Subject, h.Category, h.Body, h.Status, h.Priority, resolvedBy, resolvedAt)
		if err != nil {
			return fmt.Errorf("creating help request: %w", err)
		}

		if h.Reply == "" || serr != nil || staffID == nil {
			continue
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := db.Primary.ExecContext(ctx, `
			INSERT INTO help_ticket_replies (help_ticket_id, author_id, author_role, body)
			VALUES (?,?, 'STAFF', ?)`, id, *staffID, h.Reply); err != nil {
			return fmt.Errorf("creating help reply: %w", err)
		}
	}
	return nil
}

// --- notification wording ---------------------------------------------------

type templateSeed struct {
	EventKey, Subject, BodyText string
}

// sampleTemplates are the platform defaults — tenant_id NULL — that the worker
// falls back to and the Configuration screen lists. A client editing one gets
// their own row; these are never overwritten by that.
var sampleTemplates = []templateSeed{
	{"ticket.created", "Ticket raised: {{ticket_number}}",
		"Hello {{recipient_name}},\n\n{{ticket_number}} — {{subject}} — has been raised and is with the helpdesk.\n\nWe will update you here as it progresses."},
	{"ticket.assigned", "Ticket assigned: {{ticket_number}}",
		"Hello {{recipient_name}},\n\n{{ticket_number}} — {{subject}} — is now being worked by {{assignee_name}}."},
	{"ticket.replied", "New reply on {{ticket_number}}",
		"Hello {{recipient_name}},\n\nThere is a new reply on {{ticket_number}} — {{subject}}.\n\nOpen the ticket to read it and respond."},
	{"ticket.info_requested", "Information needed on {{ticket_number}}",
		"Hello {{recipient_name}},\n\nThe helpdesk needs something from you before {{ticket_number}} can move forward.\n\nOpen the ticket to see what is needed."},
	{"ticket.status_changed", "{{ticket_number}} is now {{status}}",
		"Hello {{recipient_name}},\n\n{{ticket_number}} — {{subject}} — has moved to {{status}}."},
	{"ticket.escalated", "Escalated: {{ticket_number}}",
		"Hello {{recipient_name}},\n\n{{ticket_number}} — {{subject}} — has been escalated and is being reviewed at a higher level."},
	{"ticket.resolved", "Resolved: {{ticket_number}}",
		"Hello {{recipient_name}},\n\n{{ticket_number}} — {{subject}} — has been resolved.\n\nIf this is not settled, you can reopen it from the ticket."},
	{"ticket.closed", "Closed: {{ticket_number}}",
		"Hello {{recipient_name}},\n\n{{ticket_number}} — {{subject}} — is now closed. Thank you."},
	{"ticket.reopened", "Reopened: {{ticket_number}}",
		"Hello {{recipient_name}},\n\n{{ticket_number}} — {{subject}} — has been reopened and is back with the helpdesk."},
	{"ticket.sla_warning", "Due soon: {{ticket_number}}",
		"{{ticket_number}} — {{subject}} — is due at {{due_at}} and has not been resolved."},
	{"ticket.sla_breached", "SLA breached: {{ticket_number}}",
		"{{ticket_number}} — {{subject}} — has passed its resolution deadline."},
	{"user.welcome", "Welcome to ComplyDesk",
		"Hello {{recipient_name}},\n\nAn account has been created for you on ComplyDesk, where you can raise and track your PF and ESIC queries."},
	{"user.password_reset_link", "Reset your ComplyDesk password",
		"Hello {{recipient_name}},\n\nUse the link in this message to set a new password. It expires shortly, and can be used once."},
	{"maintenance.scheduled", "Scheduled maintenance",
		"ComplyDesk will be unavailable during a planned maintenance window. We will confirm here once it is over."},
}

func ensureNotificationTemplates(ctx context.Context, db *platform.DB) error {
	for _, t := range sampleTemplates {
		// The event must exist first: notification_templates has a foreign key
		// onto the catalogue, so a template for an unseeded event would fail.
		var known int64
		if err := db.Primary.GetContext(ctx, &known,
			`SELECT COUNT(*) FROM notification_events WHERE event_key = ?`, t.EventKey); err != nil {
			return fmt.Errorf("checking event %s: %w", t.EventKey, err)
		}
		if known == 0 {
			continue
		}

		for _, channel := range []string{"EMAIL", "IN_APP"} {
			if _, err := db.Primary.ExecContext(ctx, `
				INSERT INTO notification_templates
					(public_id, tenant_id, event_key, channel, subject, body_text, is_active)
				VALUES (?, NULL, ?, ?, ?, ?, 1)
				ON DUPLICATE KEY UPDATE subject = VALUES(subject), body_text = VALUES(body_text)`,
				platform.NewULID(), t.EventKey, channel, t.Subject, t.BodyText); err != nil {
				return fmt.Errorf("seeding template %s/%s: %w", t.EventKey, channel, err)
			}
		}
	}
	return nil
}
