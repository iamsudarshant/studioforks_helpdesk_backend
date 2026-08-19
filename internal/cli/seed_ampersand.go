package cli

// The Ampersand Group sample dataset.
//
// One client, two statutory departments, the full entity catalogue underneath
// each, and a roster where every role in the product has somebody in it: an
// administrator, an agent per department, a partner per entity, and employees
// who have actually raised tickets.
//
// It replaces the older `--sample` dataset, which spread three invented clients
// across five departments and left most of the catalogue empty. This one is
// deliberately narrow and complete instead: everything it creates is connected
// to everything else, so `Client → Department → Entity → Agent/Partner/Employee
// → Tickets` can be walked end to end without hitting a gap.
//
// Idempotent throughout. A second run finds what the first created, updates it
// in place and adds nothing — so it is safe to re-run after a schema change.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/karmamgmt/complydesk/internal/auth"
	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/user"
)

// The client this dataset belongs to. Matched on slug so a re-run updates the
// workspace the previous run created rather than making a second one.
const (
	ampersandSlug = "demo"
	ampersandCode = "AMP"
	ampersandName = "Ampersand Group"
)

// ampersandDepartment is one statutory line and everything filed under it.
type ampersandDepartment struct {
	Code, Name, Type string
	// Prefix seeds the entity codes, so PF entities read PF-CLM and ESIC ones
	// ESI-CARD — the shape an operator already recognises from the portals.
	Prefix   string
	Entities []ampersandEntity
}

// ampersandEntity is one service line a query can be raised against.
//
// `Type` classifies what kind of work it is, which is what the routing rules
// and the reports group by. `Suffix` completes the entity code.
type ampersandEntity struct {
	Suffix, Name, Type string
}

var ampersandDepartments = []ampersandDepartment{
	{
		Code: "PF", Name: "PF & Compliance", Type: "PF", Prefix: "PF",
		Entities: []ampersandEntity{
			{"CLM", "Claim Status", "CLAIM"},
			{"COR", "Correction Requests", "SERVICE"},
			{"EDLI", "EDLI", "CLAIM"},
			{"EMPC", "Employer Contributions", "COMPLIANCE"},
			{"EXIT", "Exit Details", "SERVICE"},
			{"KYC", "KYC Updates", "SERVICE"},
			{"PBK", "Member Passbook", "SERVICE"},
			{"PEN", "Pension", "CLAIM"},
			{"TRF", "PF Transfers", "TRANSFER"},
			{"WDL", "PF Withdrawals", "CLAIM"},
			{"SVC", "Service History", "SERVICE"},
			{"UAN", "UAN Issues", "SERVICE"},
			{"ADV", "PF Advance / Loan", "CLAIM"},
			{"NOM", "PF Nomination / e-Nomination", "SERVICE"},
			{"DTH", "Death Claim", "CLAIM"},
			{"DIS", "Disability Claim", "CLAIM"},
			{"F19", "Form 19 / Final Settlement", "CLAIM"},
			{"F10C", "Form 10C / Pension Withdrawal", "CLAIM"},
			{"BAL", "PF Balance Inquiry", "SERVICE"},
			{"IW", "International Worker (IW) Compliance", "COMPLIANCE"},
			{"ECR", "PF Return / ECR Filing", "COMPLIANCE"},
			{"REG", "Establishment Registration / Code Update", "COMPLIANCE"},
			{"DLC", "Digital Life Certificate (for pensioners)", "SERVICE"},
			{"JD", "Joint Declaration / Name Correction", "SERVICE"},
		},
	},
	{
		Code: "ESIC", Name: "ESIC & Insurance", Type: "ESIC", Prefix: "ESI",
		Entities: []ampersandEntity{
			{"CARD", "ESIC Card", "SERVICE"},
			{"CLM", "ESIC Claim Status", "CLAIM"},
			{"DISP", "ESIC Dispensary", "SERVICE"},
			{"REG", "ESIC Registration", "COMPLIANCE"},
			{"CHLN", "ESIC Contribution / Challan", "COMPLIANCE"},
			{"TDB", "Temporary Disablement Benefit (TDB)", "CLAIM"},
			{"PDB", "Permanent Disablement Benefit (PDB)", "CLAIM"},
			{"MAT", "Maternity Benefit", "CLAIM"},
			{"SICK", "Sickness Benefit", "CLAIM"},
			{"DEP", "Dependent Benefit", "CLAIM"},
			{"MED", "Medical / Hospitalization Claim", "CLAIM"},
			{"ACC", "Accident Report / Injury Claim", "CLAIM"},
			{"FAM", "Family / Dependent Registration", "SERVICE"},
			{"IP", "IP (Insured Person) Number Issues", "SERVICE"},
			{"RET", "ESIC Return Filing", "COMPLIANCE"},
			{"PERIOD", "ESIC Contribution Period Mapping", "COMPLIANCE"},
			{"SUB", "Offline / Online Claim Submission", "SERVICE"},
		},
	},
}

// Names drawn from a fixed list rather than generated, so a re-run produces the
// same people and the same login addresses. A demo whose credentials change
// every time it is rebuilt cannot be written down in a runbook.
var ampersandFirstNames = []string{
	"Aarav", "Ananya", "Rohan", "Priya", "Vikram", "Sneha", "Karthik", "Meera",
	"Arjun", "Divya", "Rahul", "Kavya", "Siddharth", "Neha", "Aditya", "Pooja",
	"Manish", "Ritu", "Sanjay", "Anjali", "Nikhil", "Shreya", "Gaurav", "Isha",
	"Varun", "Tanvi", "Harsh", "Nisha", "Akash", "Deepa", "Rakesh", "Swati",
	"Amit", "Lakshmi", "Suresh", "Preeti", "Vivek", "Radha", "Kunal", "Sunita",
	"Ajay", "Bhavna",
}

var ampersandLastNames = []string{
	"Sharma", "Verma", "Iyer", "Nair", "Reddy", "Patel", "Desai", "Kulkarni",
	"Menon", "Rao", "Gupta", "Joshi", "Chauhan", "Mehta", "Pillai", "Bose",
	"Banerjee", "Chatterjee", "Sinha", "Malhotra", "Kapoor", "Bhatt",
}

// A person in the dataset, resolved from the name lists by index so that the
// same seq always produces the same person.
type ampersandPerson struct {
	First, Last, Email, EmployeeCode, Mobile, Designation string
}

func ampersandName2(seq int) (string, string) {
	first := ampersandFirstNames[seq%len(ampersandFirstNames)]
	last := ampersandLastNames[(seq*7)%len(ampersandLastNames)]
	return first, last
}

// seedAmpersand installs the dataset. Called by `seed --ampersand`.
func seedAmpersand(ctx context.Context, db *platform.DB, cfg *config.Config) error {
	tenantID, err := lookupID(ctx, db,
		`SELECT id FROM tenants WHERE slug = ? AND deleted_at IS NULL`, ampersandSlug)
	if err != nil {
		return fmt.Errorf("no %s workspace; run `seed --demo` first", ampersandName)
	}

	// The client's own identity, so the roster and the switcher name it the way
	// the brief does rather than "Demo Workspace".
	//
	// The code is renamed together with every ticket number that carries it.
	// The API refuses to change a code once tickets exist precisely because the
	// code is stamped into those numbers — so a rename done here has to keep
	// that promise rather than sidestep it, or a ticket quoted as
	// `AMP001-PF-2026-000145` would no longer be findable.
	var previous string
	_ = db.Primary.GetContext(ctx, &previous,
		`SELECT COALESCE(client_code, '') FROM tenants WHERE id = ?`, tenantID)

	if _, err := db.Primary.ExecContext(ctx,
		`UPDATE tenants SET name = ?, client_code = ? WHERE id = ?`,
		ampersandName, ampersandCode, tenantID); err != nil {
		return fmt.Errorf("naming the client: %w", err)
	}

	if previous != "" && previous != ampersandCode {
		res, err := db.Primary.ExecContext(ctx, `
			UPDATE tickets
			   SET ticket_number = CONCAT(?, SUBSTRING(ticket_number, CHAR_LENGTH(?) + 1))
			 WHERE tenant_id = ? AND ticket_number LIKE CONCAT(?, '-%')`,
			ampersandCode, previous, tenantID, previous)
		if err != nil {
			return fmt.Errorf("renumbering tickets for the new client code: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fmt.Printf("client code %s -> %s; renumbered %d ticket(s)\n", previous, ampersandCode, n)
		}
	}

	// Two-factor is part of what this dataset is meant to demonstrate, and the
	// enrolment screen refuses outright when the workspace flag is off.
	if _, err := db.Primary.ExecContext(ctx, `
		INSERT INTO tenant_features (tenant_id, feature_key, enabled) VALUES (?, 'mfa', 1)
		ON DUPLICATE KEY UPDATE enabled = 1`, tenantID); err != nil {
		return fmt.Errorf("enabling two-factor: %w", err)
	}

	deptIDs, err := ensureAmpersandDepartments(ctx, db, tenantID)
	if err != nil {
		return err
	}

	entityIDs, err := ensureAmpersandEntities(ctx, db, tenantID, deptIDs)
	if err != nil {
		return err
	}

	hash, err := auth.NewHasher(cfg.Auth).Hash(seedPassword)
	if err != nil {
		return fmt.Errorf("hashing the sample password: %w", err)
	}

	adminID, err := ensureAmpersandAdmin(ctx, db, tenantID, hash)
	if err != nil {
		return err
	}

	agentIDs, err := ensureAmpersandAgents(ctx, db, tenantID, deptIDs, hash)
	if err != nil {
		return err
	}

	partners, err := ensureAmpersandPartners(ctx, db, tenantID, entityIDs, hash)
	if err != nil {
		return err
	}

	employees, err := ensureAmpersandEmployees(ctx, db, tenantID, entityIDs, deptIDs, hash)
	if err != nil {
		return err
	}

	tickets, docs, err := ensureAmpersandTickets(ctx, db, tenantID, employees, agentIDs, entityIDs, deptIDs)
	if err != nil {
		return err
	}

	fmt.Printf("%s seeded: %d departments, %d entities, 1 admin, %d agents, %d partners, %d employees, %d tickets, %d documents\n",
		ampersandName, len(deptIDs), len(entityIDs), len(agentIDs), len(partners), len(employees), tickets, docs)
	_ = adminID
	return nil
}

// seedPassword is the one credential every sample account shares, so a tester
// needs one password rather than eighty.
const seedPassword = "ComplyDesk@2026"

// ensureAmpersandDepartments creates the two statutory lines and returns their
// ids by code. Anything else the client had is deactivated rather than deleted:
// a department may already have tickets filed against it, and removing the row
// would orphan them.
func ensureAmpersandDepartments(ctx context.Context, db *platform.DB, tenantID int64) (map[string]int64, error) {
	ids := map[string]int64{}

	for _, d := range ampersandDepartments {
		if _, err := db.Primary.ExecContext(ctx, `
			INSERT INTO departments (public_id, tenant_id, code, name, type, is_active)
			VALUES (?,?,?,?,?,1)
			ON DUPLICATE KEY UPDATE
				name = VALUES(name), type = VALUES(type),
				is_active = 1, deleted_at = NULL`,
			platform.NewULID(), tenantID, d.Code, d.Name, d.Type); err != nil {
			return nil, fmt.Errorf("seeding department %s: %w", d.Code, err)
		}

		id, err := lookupID(ctx, db,
			`SELECT id FROM departments WHERE tenant_id = ? AND code = ? AND deleted_at IS NULL`,
			tenantID, d.Code)
		if err != nil {
			return nil, fmt.Errorf("loading department %s: %w", d.Code, err)
		}
		ids[d.Code] = id
	}

	// Retire the departments the older datasets left behind — Payroll, HR,
	// General Support — so the ticket form offers the two lines this client
	// actually runs. Deactivated, not deleted: their history stays readable.
	keep := make([]string, 0, len(ids))
	args := []any{tenantID}
	for code := range ids {
		keep = append(keep, "?")
		args = append(args, code)
	}
	if _, err := db.Primary.ExecContext(ctx,
		`UPDATE departments SET is_active = 0
		  WHERE tenant_id = ? AND deleted_at IS NULL AND code NOT IN (`+strings.Join(keep, ",")+`)`,
		args...); err != nil {
		return nil, fmt.Errorf("retiring other departments: %w", err)
	}

	return ids, nil
}

// ensureAmpersandEntities creates every service line under its department and
// returns their ids by code.
func ensureAmpersandEntities(ctx context.Context, db *platform.DB, tenantID int64,
	deptIDs map[string]int64) (map[string]int64, error) {

	ids := map[string]int64{}

	for _, d := range ampersandDepartments {
		deptID, ok := deptIDs[d.Code]
		if !ok {
			continue
		}

		for _, e := range d.Entities {
			code := d.Prefix + "-" + e.Suffix

			if _, err := db.Primary.ExecContext(ctx, `
				INSERT INTO entities
					(public_id, tenant_id, department_id, code, name, type, is_active)
				VALUES (?,?,?,?,?,?,1)
				ON DUPLICATE KEY UPDATE
					department_id = VALUES(department_id), name = VALUES(name),
					type = VALUES(type), is_active = 1, deleted_at = NULL`,
				platform.NewULID(), tenantID, deptID, code, e.Name, e.Type); err != nil {
				return nil, fmt.Errorf("seeding entity %s: %w", code, err)
			}

			id, err := lookupID(ctx, db,
				`SELECT id FROM entities WHERE tenant_id = ? AND code = ? AND deleted_at IS NULL`,
				tenantID, code)
			if err != nil {
				return nil, fmt.Errorf("loading entity %s: %w", code, err)
			}
			ids[code] = id
		}
	}

	// Same reasoning as the departments: anything the previous datasets created
	// is retired rather than removed, so tickets already filed against it keep
	// resolving.
	keep := make([]string, 0, len(ids))
	args := []any{tenantID}
	for code := range ids {
		keep = append(keep, "?")
		args = append(args, code)
	}
	if _, err := db.Primary.ExecContext(ctx,
		`UPDATE entities SET is_active = 0
		  WHERE tenant_id = ? AND deleted_at IS NULL AND code NOT IN (`+strings.Join(keep, ",")+`)`,
		args...); err != nil {
		return nil, fmt.Errorf("retiring other entities: %w", err)
	}

	return ids, nil
}

// upsertPerson creates or updates one sample account and gives it a role.
//
// Everything here is reasserted on a re-run rather than skipped, because the
// point of the dataset is that it is correct after every run — a half-updated
// account from a previous schema is worse than none.
func upsertPerson(ctx context.Context, db *platform.DB, userRepo *user.Repository,
	tenantID int64, p ampersandPerson, roleKey, hash string, seq int) (int64, error) {

	uan := fmt.Sprintf("%012d", 100500000000+int64(seq)*173)
	pf := fmt.Sprintf("MH/BAN/%07d/000/%07d", 1100000+seq*7, 2200000+seq*11)
	esic := fmt.Sprintf("%017d", 3100000000000000+int64(seq)*211)

	// PAN: five letters, four digits, a check letter — the real shape, because
	// the API validates it and the default password is derived from it.
	letters := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	at := func(i int) byte { return letters[((i%26)+26)%26] }
	surname := byte('X')
	if p.Last != "" {
		surname = strings.ToUpper(p.Last)[0]
	}
	pan := fmt.Sprintf("%c%c%c%c%c%04d%c",
		at(seq*3), at(seq*5), at(seq*7), 'P', surname, (seq*1237)%10000, at(seq*13))

	dob := time.Date(1970+(seq*3)%28, time.Month(1+(seq*5)%12), 1+(seq*7)%28, 0, 0, 0, 0, time.UTC)
	doj := time.Date(2013+(seq*2)%12, time.Month(1+(seq*3)%12), 1+(seq*11)%28, 0, 0, 0, 0, time.UTC)

	var id int64
	err := db.Primary.GetContext(ctx, &id,
		`SELECT id FROM users WHERE tenant_id = ? AND email = ? AND deleted_at IS NULL`,
		tenantID, p.Email)

	switch {
	case platform.IsNotFound(err):
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
	case err != nil:
		return 0, fmt.Errorf("looking up %s: %w", p.Email, err)
	default:
		// Reset the password on every run so the documented credential always
		// works, even against an account somebody has since changed.
		if _, uerr := db.Primary.ExecContext(ctx, `
			UPDATE users
			   SET password_hash = ?, must_change_password = 0, status = ?,
			       first_name = ?, last_name = ?, designation = ?,
			       employee_code = COALESCE(employee_code, ?),
			       pan_number = COALESCE(pan_number, ?), uan_number = COALESCE(uan_number, ?),
			       pf_number = COALESCE(pf_number, ?), esic_number = COALESCE(esic_number, ?),
			       date_of_birth = COALESCE(date_of_birth, ?),
			       date_of_joining = COALESCE(date_of_joining, ?)
			 WHERE id = ?`,
			hash, user.StatusActive, p.First, p.Last, nullIfBlank(p.Designation),
			nullIfBlank(p.EmployeeCode), pan, uan, pf, esic, dob, doj, id); uerr != nil {
			return 0, fmt.Errorf("refreshing %s: %w", p.Email, uerr)
		}
	}

	role, err := userRepo.RoleByKey(ctx, tenantID, roleKey)
	if err != nil {
		return 0, fmt.Errorf("loading role %s: %w", roleKey, err)
	}
	if err := userRepo.SetRoles(ctx, tenantID, id, []int64{role.ID}, nil); err != nil {
		return 0, fmt.Errorf("assigning %s to %s: %w", roleKey, p.Email, err)
	}
	return id, nil
}

func ensureAmpersandAdmin(ctx context.Context, db *platform.DB, tenantID int64, hash string) (int64, error) {
	userRepo := user.NewRepository(db)
	p := ampersandPerson{
		First: "Ampersand", Last: "Admin", Email: "admin@ampersand.local",
		EmployeeCode: "AMP-ADM-001", Mobile: "9800000001",
		Designation: "Client Administrator",
	}
	id, err := upsertPerson(ctx, db, userRepo, tenantID, p, user.RoleClientAdmin, hash, 1)
	if err != nil {
		return 0, err
	}
	// A client admin sees the whole workspace, so they are posted to no single
	// entity — a posting would read as a segmentation they do not have.
	if _, err := db.Primary.ExecContext(ctx,
		`UPDATE users SET entity_id = NULL, department_id = NULL WHERE id = ?`, id); err != nil {
		return 0, fmt.Errorf("clearing the admin posting: %w", err)
	}
	return id, nil
}

// ensureAmpersandAgents creates the two ComplyDesk agents and ties each to one
// statutory line.
//
// Agents live in the platform tenant, not in the client: that is what makes
// them ComplyDesk's staff rather than the client's. They reach this client
// through `agent_tenant_assignments`, and the department they work is recorded
// in `department_assignments` — the same two mechanisms the product uses when
// an operator does it by hand.
func ensureAmpersandAgents(ctx context.Context, db *platform.DB, tenantID int64,
	deptIDs map[string]int64, hash string) (map[string]int64, error) {

	platformID, err := platformTenantID(ctx, db)
	if err != nil {
		return nil, err
	}
	userRepo := user.NewRepository(db)

	agents := []struct {
		Person ampersandPerson
		Dept   string
	}{
		{ampersandPerson{
			First: "Priya", Last: "Nair", Email: "pf.agent@complydesk.local",
			EmployeeCode: "CD-AGT-PF01", Mobile: "9800000011",
			Designation: "PF Compliance Specialist",
		}, "PF"},
		{ampersandPerson{
			First: "Karthik", Last: "Menon", Email: "esic.agent@complydesk.local",
			EmployeeCode: "CD-AGT-ESI1", Mobile: "9800000012",
			Designation: "ESIC Benefits Specialist",
		}, "ESIC"},
	}

	ids := map[string]int64{}
	for i, a := range agents {
		id, err := upsertPerson(ctx, db, userRepo, platformID, a.Person,
			user.RoleHelpdeskExecutive, hash, 20+i)
		if err != nil {
			return nil, err
		}

		if _, err := db.Primary.ExecContext(ctx, `
			INSERT INTO agent_tenant_assignments (public_id, agent_user_id, tenant_id, is_primary)
			VALUES (?,?,?,1)
			ON DUPLICATE KEY UPDATE revoked_at = NULL, is_primary = 1`,
			platform.NewULID(), id, tenantID); err != nil {
			return nil, fmt.Errorf("assigning agent %s to the client: %w", a.Person.Email, err)
		}

		if deptID, ok := deptIDs[a.Dept]; ok {
			if err := userRepo.AssignDepartmentUser(ctx, tenantID, deptID, id, nil); err != nil {
				return nil, fmt.Errorf("assigning agent %s to %s: %w", a.Person.Email, a.Dept, err)
			}
		}
		ids[a.Dept] = id
	}
	return ids, nil
}

// ensureAmpersandPartners creates one partner per entity and allocates them to
// it, so every entity has somebody accountable for it.
func ensureAmpersandPartners(ctx context.Context, db *platform.DB, tenantID int64,
	entityIDs map[string]int64, hash string) ([]int64, error) {

	userRepo := user.NewRepository(db)
	ids := []int64{}
	seq := 100

	for _, d := range ampersandDepartments {
		for _, e := range d.Entities {
			code := d.Prefix + "-" + e.Suffix
			entityID, ok := entityIDs[code]
			if !ok {
				continue
			}

			seq++
			first, last := ampersandName2(seq)
			p := ampersandPerson{
				First: first, Last: last,
				// Addressed by entity code, so the account for "PF Withdrawals"
				// is findable without a lookup table.
				Email:        fmt.Sprintf("partner.%s@ampersand.local", strings.ToLower(strings.ReplaceAll(code, "-", "."))),
				EmployeeCode: fmt.Sprintf("AMP-PTR-%03d", seq-100),
				Mobile:       fmt.Sprintf("98100%05d", seq),
				Designation:  e.Name + " Coordinator",
			}

			id, err := upsertPerson(ctx, db, userRepo, tenantID, p, user.RoleClientExecutive, hash, seq)
			if err != nil {
				return nil, err
			}

			// The posting, and the allocation the scope check reads. A client
			// executive with no allocation sees nothing at all — that is the
			// role failing closed — so this is what makes the account usable.
			if _, err := db.Primary.ExecContext(ctx, `
				UPDATE users u JOIN entities e ON e.id = ?
				   SET u.entity_id = e.id, u.department_id = e.department_id
				 WHERE u.id = ?`, entityID, id); err != nil {
				return nil, fmt.Errorf("posting partner %s: %w", p.Email, err)
			}

			// Through the repository, not a bare INSERT. The assignment row is
			// only half the record: the scope table is what every entity filter
			// actually consults, and writing one without the other produced a
			// partner who was visibly allocated to an entity and could see
			// none of its tickets.
			if err := userRepo.AssignEntityUser(ctx, tenantID, entityID, id, false, nil); err != nil {
				return nil, fmt.Errorf("allocating partner %s: %w", p.Email, err)
			}

			ids = append(ids, id)
		}
	}
	return ids, nil
}

// ampersandEmployee carries the posting alongside the id, because the ticket
// pass needs to file each person's query against the entity they belong to.
type ampersandEmployee struct {
	ID         int64
	Name       string
	EntityCode string
	DeptCode   string
}

// ensureAmpersandEmployees creates the 34 employees, spread across both
// departments so each statutory line has queries to work.
func ensureAmpersandEmployees(ctx context.Context, db *platform.DB, tenantID int64,
	entityIDs map[string]int64, deptIDs map[string]int64, hash string) ([]ampersandEmployee, error) {

	userRepo := user.NewRepository(db)

	// The entity each employee is posted to, walked round-robin through the
	// catalogue so every department has people and no entity collects all of
	// them.
	postings := make([]struct{ Code, Dept string }, 0, 41)
	for _, d := range ampersandDepartments {
		for _, e := range d.Entities {
			postings = append(postings, struct{ Code, Dept string }{d.Prefix + "-" + e.Suffix, d.Code})
		}
	}

	out := make([]ampersandEmployee, 0, 34)
	for i := 0; i < 34; i++ {
		seq := 200 + i
		first, last := ampersandName2(seq)
		posting := postings[i%len(postings)]

		p := ampersandPerson{
			First: first, Last: last,
			Email:        fmt.Sprintf("employee%02d@ampersand.local", i+1),
			EmployeeCode: fmt.Sprintf("AMP-EMP-%03d", i+1),
			Mobile:       fmt.Sprintf("98200%05d", seq),
			Designation:  ampersandDesignations[i%len(ampersandDesignations)],
		}

		id, err := upsertPerson(ctx, db, userRepo, tenantID, p, user.RoleEmployee, hash, seq)
		if err != nil {
			return nil, err
		}

		if entityID, ok := entityIDs[posting.Code]; ok {
			if _, err := db.Primary.ExecContext(ctx, `
				UPDATE users u JOIN entities e ON e.id = ?
				   SET u.entity_id = e.id, u.department_id = e.department_id
				 WHERE u.id = ?`, entityID, id); err != nil {
				return nil, fmt.Errorf("posting employee %s: %w", p.Email, err)
			}
		}

		// Employees belong to the active-employees group, which is what carries
		// their access mode. Without it the employment transition has no group
		// to move them out of.
		if _, err := db.Primary.ExecContext(ctx, `
			UPDATE users u
			  JOIN user_groups g ON g.tenant_id = u.tenant_id AND g.group_key = ?
			   SET u.user_group_id = g.id
			 WHERE u.id = ? AND u.user_group_id IS NULL`,
			user.GroupActiveEmployees, id); err != nil {
			return nil, fmt.Errorf("grouping employee %s: %w", p.Email, err)
		}

		out = append(out, ampersandEmployee{
			ID: id, Name: first + " " + last,
			EntityCode: posting.Code, DeptCode: posting.Dept,
		})
	}
	return out, nil
}

var ampersandDesignations = []string{
	"Machine Operator", "Shift Supervisor", "Accounts Executive", "Store Keeper",
	"Quality Inspector", "Maintenance Technician", "HR Coordinator", "Logistics Assistant",
	"Production Associate", "Security Supervisor", "Packaging Operator", "Line Lead",
}
