package cli

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/auth"
	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/ticket"
	"github.com/karmamgmt/complydesk/internal/user"
)

// The showcase populates the Ampersand Group demo client with a spread of
// tickets that actually exercises the product: every status in the lifecycle,
// several categories, more than one establishment and site, and a conversation
// and timeline on each.
//
// It exists because a demo built from whatever happened to be created during
// testing is misleading — three tickets all sitting in REOPENED tells a visitor
// nothing about how the desk works.
//
// Tickets are created through the real repository, so numbering, the SLA policy
// and the CREATED timeline entry are produced by the same code paths the API
// uses. Only the subsequent status moves are written directly, because driving
// them through the workflow would need an HTTP actor; each one still writes a
// status-history row and a timeline entry, so the audit trail is genuine.

// showcaseTicket is one scripted ticket.
type showcaseTicket struct {
	CategoryKey    string
	SubcategoryKey string
	Subject        string
	Description    string
	Priority       string
	RequesterEmail string
	EntityCode     string
	SiteCode       string
	// Status is where the ticket ends up. The path taken to get there is
	// derived from it, so the timeline reads plausibly.
	Status string
	// AgeHours backdates creation, so the list is not one flat timestamp and
	// the age and SLA columns show a realistic spread.
	AgeHours int
	Reply    string
	Internal string
}

var showcaseTickets = []showcaseTicket{
	{
		CategoryKey: "PF_QUERY", SubcategoryKey: "PF_WITHDRAWAL",
		Subject:     "PF withdrawal not credited after 45 days",
		Description: "I applied for a full PF withdrawal on 12 June. The claim shows as settled on the EPFO portal but nothing has reached my account.",
		Priority:    "HIGH", RequesterEmail: "employee@demo.local",
		EntityCode: "AMP-MFG", SiteCode: "AMP-MUM", Status: "PENDING_EMPLOYEE", AgeHours: 52,
		Reply:    "We have raised this with the EPFO regional office. Could you share a cancelled cheque so we can confirm the bank details on record?",
		Internal: "Claim ID traced. Bank account on the UAN does not match the one in payroll.",
	},
	{
		CategoryKey: "PF_QUERY", SubcategoryKey: "PF_TRANSFER",
		Subject:     "PF transfer from previous employer still pending",
		Description: "Form 13 was submitted when I joined in April. The previous employer's balance has not moved across yet.",
		Priority:    "MEDIUM", RequesterEmail: "employee@demo.local",
		EntityCode: "AMP-HO", SiteCode: "AMP-MUM", Status: "IN_PROGRESS", AgeHours: 26,
		Reply: "The transfer claim is with the destination office. We are following up and will update you this week.",
	},
	{
		CategoryKey: "PF_QUERY", SubcategoryKey: "PF_KYC",
		Subject:     "UAN not linked to Aadhaar",
		Description: "My UAN shows KYC pending against Aadhaar. I have uploaded the document twice.",
		Priority:    "MEDIUM", RequesterEmail: "exemployee@demo.local",
		EntityCode: "AMP-SVC", SiteCode: "AMP-PUN", Status: "RESOLVED", AgeHours: 96,
		Reply: "KYC has been approved by the employer and is now showing as verified on the member portal.",
	},
	{
		CategoryKey: "ESI_QUERY", SubcategoryKey: "ESI_CARD",
		Subject:     "ESIC card not received for dependants",
		Description: "I registered my parents as dependants last month but have not received the updated card.",
		Priority:    "LOW", RequesterEmail: "employee@demo.local",
		EntityCode: "AMP-MFG", SiteCode: "AMP-MUM", Status: "CLOSED", AgeHours: 220,
		Reply: "The e-Pehchan card has been generated and emailed to you. Closing this now — reopen it if the card does not arrive.",
	},
	{
		CategoryKey: "ESI_QUERY", SubcategoryKey: "ESI_CLAIM",
		Subject:     "Sickness benefit claim rejected without reason",
		Description: "My claim for sickness benefit was rejected. The portal gives no reason and the dispensary has no record.",
		Priority:    "HIGH", RequesterEmail: "employee@demo.local",
		EntityCode: "AMP-LOG", SiteCode: "AMP-DEL", Status: "ESCALATED", AgeHours: 71,
		Reply:    "We have escalated this to the branch manager at the ESIC office and asked for the rejection reason in writing.",
		Internal: "Third rejection from this branch this quarter. Flagging to the compliance lead.",
	},
	{
		CategoryKey: "ESI_QUERY", SubcategoryKey: "ESI_DISPENSARY",
		Subject:     "Change of ESIC dispensary after relocation",
		Description: "I have moved from Mumbai to Pune and need my dispensary changed.",
		Priority:    "LOW", RequesterEmail: "employee@demo.local",
		EntityCode: "AMP-SVC", SiteCode: "AMP-PUN", Status: "OPEN", AgeHours: 8,
	},
	{
		CategoryKey: "PAYROLL",
		Subject:     "Salary slip missing for March",
		Description: "The March payslip is not available on the portal. Every other month is there.",
		Priority:    "MEDIUM", RequesterEmail: "employee@demo.local",
		EntityCode: "AMP-HO", SiteCode: "AMP-MUM", Status: "PENDING_HELPDESK", AgeHours: 34,
		Internal: "Payroll team regenerating the March run for this employee code.",
	},
	{
		CategoryKey: "HR_QUERY",
		Subject:     "Service certificate needed for a home loan",
		Description: "My bank has asked for a service certificate showing my joining date and designation.",
		Priority:    "MEDIUM", RequesterEmail: "employee@demo.local",
		EntityCode: "AMP-CORP", SiteCode: "AMP-BLR", Status: "NEW", AgeHours: 3,
	},
	{
		CategoryKey: "GENERAL",
		Subject:     "Exit formalities and full and final settlement",
		Description: "I left in May. The full and final settlement statement has not been shared.",
		Priority:    "HIGH", RequesterEmail: "exemployee@demo.local",
		EntityCode: "AMP-HO", SiteCode: "AMP-MUM", Status: "REOPENED", AgeHours: 340,
		Reply: "The settlement was processed on 20 June. Reopening because the gratuity component looks short.",
	},
	// A second wave covering the remaining PF and ESIC query types, spread over
	// the whole roster and every establishment. Without these the reports and
	// the category charts have three bars and prove nothing.
	{
		CategoryKey: "PF_QUERY", SubcategoryKey: "PF_UAN",
		Subject:     "Two UANs issued against the same PAN",
		Description: "My previous employer created a second UAN. Both show contributions and I cannot merge them.",
		Priority:    "HIGH", RequesterEmail: "rahul.mehta@demo.local",
		EntityCode: "AMP-MFG", SiteCode: "AMP-MUM", Status: "IN_PROGRESS", AgeHours: 40,
		Reply:    "We have filed the UAN merge request with the regional office and will confirm once the older one is deactivated.",
		Internal: "Both UANs traced. Older one is from the Pune establishment.",
	},
	{
		CategoryKey: "PF_QUERY", SubcategoryKey: "PF_PASSBOOK",
		Subject:     "Passbook not updating since January",
		Description: "The member passbook shows no entries after January although salary slips show PF deducted every month.",
		Priority:    "MEDIUM", RequesterEmail: "sneha.kulkarni@demo.local",
		EntityCode: "AMP-HO", SiteCode: "AMP-MUM", Status: "PENDING_HELPDESK", AgeHours: 62,
		Internal: "ECR filed but not reconciled for Feb-Apr. Raising with the accounts team.",
	},
	{
		CategoryKey: "PF_QUERY", SubcategoryKey: "PF_PENSION",
		Subject:     "Higher pension option under EPS-95",
		Description: "I want to know whether I am eligible for the higher pension option and what the deadline is.",
		Priority:    "LOW", RequesterEmail: "arun.pillai@demo.local",
		EntityCode: "AMP-SVC", SiteCode: "AMP-PUN", Status: "RESOLVED", AgeHours: 150,
		Reply: "You are eligible. The joint option form has been submitted on your behalf and acknowledged.",
	},
	{
		CategoryKey: "PF_QUERY", SubcategoryKey: "PF_EDLI",
		Subject:     "EDLI nomination for a dependant",
		Description: "I need to add my mother as an EDLI nominee following a change in family circumstances.",
		Priority:    "MEDIUM", RequesterEmail: "priyanka.rao@demo.local",
		EntityCode: "AMP-LOG", SiteCode: "AMP-DEL", Status: "OPEN", AgeHours: 14,
	},
	{
		CategoryKey: "PF_QUERY", SubcategoryKey: "PF_CONTRIBUTION",
		Subject:     "Employer contribution short for two months",
		Description: "The employer share for March and April is lower than 12% of basic.",
		Priority:    "HIGH", RequesterEmail: "imran.shaikh@demo.local",
		EntityCode: "AMP-MFG", SiteCode: "AMP-MUM", Status: "ESCALATED", AgeHours: 110,
		Reply:    "This has been escalated to the payroll lead; a revised ECR will be filed for both months.",
		Internal: "Affects 14 employees at the Mumbai plant, not just this one. Bulk correction needed.",
	},
	{
		CategoryKey: "PF_QUERY", SubcategoryKey: "PF_SERVICE_HISTORY",
		Subject:     "Service history missing the first year",
		Description: "My service history starts from 2023 although I joined in 2022.",
		Priority:    "MEDIUM", RequesterEmail: "rahul.mehta@demo.local",
		EntityCode: "AMP-HO", SiteCode: "AMP-MUM", Status: "PENDING_EMPLOYEE", AgeHours: 88,
		Reply: "Could you share your 2022 appointment letter so we can evidence the joining date to the office?",
	},
	{
		CategoryKey: "PF_QUERY", SubcategoryKey: "PF_CLAIM_STATUS",
		Subject:     "Form 19 claim stuck at 'under process'",
		Description: "The claim has shown as under process for six weeks with no movement.",
		Priority:    "MEDIUM", RequesterEmail: "sneha.kulkarni@demo.local",
		EntityCode: "AMP-SVC", SiteCode: "AMP-PUN", Status: "CLOSED", AgeHours: 400,
		Reply: "The claim was settled on 28 June and credited to your registered account. Closing this now.",
	},
	{
		CategoryKey: "PF_QUERY", SubcategoryKey: "PF_CORRECTION",
		Subject:     "Father's name spelt incorrectly on the UAN",
		Description: "The UAN record has my father's name misspelt, which is blocking KYC.",
		Priority:    "MEDIUM", RequesterEmail: "priyanka.rao@demo.local",
		EntityCode: "AMP-CORP", SiteCode: "AMP-BLR", Status: "NEW", AgeHours: 5,
	},
	{
		CategoryKey: "ESI_QUERY", SubcategoryKey: "ESI_REGISTRATION",
		Subject:     "ESIC registration pending for a new joiner",
		Description: "I joined last month and my ESIC number has not been generated yet.",
		Priority:    "HIGH", RequesterEmail: "imran.shaikh@demo.local",
		EntityCode: "AMP-LOG", SiteCode: "AMP-DEL", Status: "IN_PROGRESS", AgeHours: 30,
		Reply: "Your registration has been submitted; the insurance number is usually issued within five working days.",
	},
	{
		CategoryKey: "ESI_QUERY", SubcategoryKey: "ESI_CONTRIBUTION",
		Subject:     "ESIC contribution deducted after crossing the wage ceiling",
		Description: "My salary is now above the ESI wage limit but the deduction continues.",
		Priority:    "MEDIUM", RequesterEmail: "arun.pillai@demo.local",
		EntityCode: "AMP-MFG", SiteCode: "AMP-MUM", Status: "RESOLVED", AgeHours: 180,
		Reply: "Deductions stop at the end of the current contribution period, which is the statutory rule. No refund is due.",
	},
	{
		CategoryKey: "ESI_QUERY", SubcategoryKey: "ESI_CORRECTION",
		Subject:     "Date of birth wrong on the ESIC record",
		Description: "My ESIC record shows the wrong year of birth, which affects my dependants' eligibility.",
		Priority:    "MEDIUM", RequesterEmail: "sneha.kulkarni@demo.local",
		EntityCode: "AMP-HO", SiteCode: "AMP-MUM", Status: "PENDING_EMPLOYEE", AgeHours: 55,
		Reply: "Please share a copy of your Aadhaar so we can file the correction with the branch office.",
	},
	{
		CategoryKey: "ESI_QUERY", SubcategoryKey: "ESI_DEPENDANT",
		Subject:     "Add spouse as a dependant",
		Description: "I married recently and need my spouse added to my ESIC record.",
		Priority:    "LOW", RequesterEmail: "rahul.mehta@demo.local",
		EntityCode: "AMP-SVC", SiteCode: "AMP-PUN", Status: "OPEN", AgeHours: 20,
	},
	{
		CategoryKey: "PAYROLL", SubcategoryKey: "PAY_TDS",
		Subject:     "Form 16 not received for last financial year",
		Description: "I need Form 16 to file my return and it has not been issued.",
		Priority:    "HIGH", RequesterEmail: "priyanka.rao@demo.local",
		EntityCode: "AMP-CORP", SiteCode: "AMP-BLR", Status: "PENDING_HELPDESK", AgeHours: 46,
		Internal: "Finance regenerating Form 16 for the Bangalore cohort.",
	},
	{
		CategoryKey: "HR_QUERY", SubcategoryKey: "HR_LETTER",
		Subject:     "Address proof letter for a visa application",
		Description: "I need a letter confirming my address and employment for a visa appointment next week.",
		Priority:    "HIGH", RequesterEmail: "arun.pillai@demo.local",
		EntityCode: "AMP-HO", SiteCode: "AMP-MUM", Status: "RESOLVED", AgeHours: 60,
		Reply: "The letter has been issued and emailed to you, with a signed copy available from HR reception.",
	},
	{
		CategoryKey: "GENERAL", SubcategoryKey: "GEN_FNF",
		Subject:     "Gratuity not included in the settlement",
		Description: "I completed five years but the settlement statement shows no gratuity component.",
		Priority:    "HIGH", RequesterEmail: "meena.joshi@demo.local",
		EntityCode: "AMP-MFG", SiteCode: "AMP-MUM", Status: "ESCALATED", AgeHours: 260,
		Reply:    "Escalated to the compliance lead. Your service records confirm five years and one month.",
		Internal: "Payroll used the confirmation date rather than the joining date. Recalculating.",
	},
}

// statusPath is the sequence of moves that lands a ticket on its final status.
// Writing the intermediate steps is what makes the timeline worth looking at.
var statusPath = map[string][]string{
	"NEW":              {},
	"OPEN":             {"OPEN"},
	"IN_PROGRESS":      {"OPEN", "IN_PROGRESS"},
	"PENDING_EMPLOYEE": {"OPEN", "IN_PROGRESS", "PENDING_EMPLOYEE"},
	"PENDING_HELPDESK": {"OPEN", "IN_PROGRESS", "PENDING_HELPDESK"},
	"ESCALATED":        {"OPEN", "IN_PROGRESS", "ESCALATED"},
	"RESOLVED":         {"OPEN", "IN_PROGRESS", "RESOLVED"},
	"CLOSED":           {"OPEN", "IN_PROGRESS", "RESOLVED", "CLOSED"},
	"REOPENED":         {"OPEN", "IN_PROGRESS", "RESOLVED", "CLOSED", "REOPENED"},
}

// seedShowcase populates the demo client. It is idempotent: a second run finds
// the tickets already present and does nothing.
func seedShowcase(ctx context.Context, db *platform.DB, cfg *config.Config) error {
	var tenantID int64
	err := db.Primary.GetContext(ctx, &tenantID,
		`SELECT id FROM tenants WHERE slug = 'demo' AND deleted_at IS NULL`)
	if err != nil {
		if platform.IsNotFound(err) {
			fmt.Println("no demo workspace; run `seed --demo` first")
			return nil
		}
		return fmt.Errorf("loading the demo workspace: %w", err)
	}

	// The roster grows over time but the workspace is created once, so any
	// sample employee added since then has to be reconciled in — otherwise the
	// showcase references people who do not exist.
	if added, err := reconcileDemoEmployees(ctx, db, cfg, tenantID); err != nil {
		return err
	} else if added > 0 {
		fmt.Printf("added %d sample employee(s) to Ampersand Group\n", added)
	}

	var existing int
	if err := db.Primary.GetContext(ctx, &existing,
		`SELECT COUNT(*) FROM tickets WHERE tenant_id = ? AND source = 'SHOWCASE'`, tenantID); err != nil {
		return fmt.Errorf("counting showcase tickets: %w", err)
	}
	if existing > 0 {
		fmt.Printf("showcase already present (%d tickets); nothing to do\n", existing)
		return nil
	}

	// Tickets created ad hoc during development carry the pre-client-code
	// numbering and cluster on one status, which makes the demo read as broken.
	// They are removed so the showcase is the whole story.
	res, err := db.Primary.ExecContext(ctx,
		`DELETE FROM tickets WHERE tenant_id = ? AND source <> 'SHOWCASE'`, tenantID)
	if err != nil {
		return fmt.Errorf("clearing ad-hoc tickets: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		fmt.Printf("removed %d ad-hoc ticket(s) left over from development\n", n)
	}

	// The roster grows over time but the workspace is created once, so any
	// sample employee added since then has to be reconciled in — otherwise the
	// showcase references people who do not exist.
	if added, err := reconcileDemoEmployees(ctx, db, cfg, tenantID); err != nil {
		return err
	} else if added > 0 {
		fmt.Printf("added %d sample employee(s) to Ampersand Group\n", added)
	}

	repo := ticket.NewRepository(db)

	// Two agents, so "workload by executive" is a comparison rather than a
	// single bar. Tickets alternate between them.
	agents := []int64{}
	for _, email := range []string{"agent.arjun@complydesk.local", "agent.priya@complydesk.local"} {
		id, err := lookupID(ctx, db, `SELECT id FROM users WHERE email = ?`, email)
		if err != nil {
			continue // that agent is not seeded in this installation
		}
		agents = append(agents, id)
	}
	if len(agents) == 0 {
		return fmt.Errorf("no ComplyDesk agents seeded; run `seed --demo` first")
	}

	created := 0
	for _, row := range showcaseTickets {
		categoryID, err := lookupID(ctx, db,
			`SELECT id FROM categories WHERE tenant_id = ? AND category_key = ? AND is_subcategory = 0`,
			tenantID, row.CategoryKey)
		if err != nil {
			continue // this client does not have that category configured
		}

		var subcategoryID *int64
		if row.SubcategoryKey != "" {
			if id, err := lookupID(ctx, db,
				`SELECT id FROM categories WHERE tenant_id = ? AND category_key = ?`,
				tenantID, row.SubcategoryKey); err == nil {
				subcategoryID = &id
			}
		}

		requesterID, err := lookupID(ctx, db,
			`SELECT id FROM users WHERE tenant_id = ? AND email = ?`, tenantID, row.RequesterEmail)
		if err != nil {
			continue
		}

		entityID := optionalID(ctx, db, `SELECT id FROM entities WHERE tenant_id = ? AND code = ?`, tenantID, row.EntityCode)
		siteID := optionalID(ctx, db, `SELECT id FROM sites WHERE tenant_id = ? AND code = ?`, tenantID, row.SiteCode)

		var requester struct {
			Name string `db:"full_name"`
			Code string `db:"employee_code"`
		}
		_ = db.Primary.GetContext(ctx, &requester, `
			SELECT CONCAT(first_name, ' ', COALESCE(last_name, '')) AS full_name,
			       COALESCE(employee_code, '') AS employee_code
			FROM users WHERE id = ?`, requesterID)

		t, err := repo.Create(ctx, tenantID, ticket.CreateParams{
			CategoryID: categoryID, SubcategoryID: subcategoryID,
			Subject: row.Subject, Description: row.Description,
			Priority: row.Priority, Source: "SHOWCASE",
			RequesterID: requesterID, EntityID: entityID, SiteID: siteID,
			CreatedBy: &requesterID,
			Snapshot:  map[string]any{"full_name": requester.Name, "employee_code": requester.Code},
		})
		if err != nil {
			return fmt.Errorf("creating showcase ticket %q: %w", row.Subject, err)
		}

		if err := ageAndAdvance(ctx, db, tenantID, t.ID, agents[created%len(agents)], row); err != nil {
			return err
		}
		created++
	}

	fmt.Printf("showcase seeded: %d tickets across %d statuses for Ampersand Group\n",
		created, len(statusPath))
	return nil
}

// ageAndAdvance backdates a ticket and walks it to its final status, writing a
// status-history row and a timeline entry for each move.
func ageAndAdvance(ctx context.Context, db *platform.DB, tenantID, ticketID, agentID int64, row showcaseTicket) error {
	return db.InTx(ctx, func(tx *sqlx.Tx) error {
		created := time.Now().UTC().Add(-time.Duration(row.AgeHours) * time.Hour)

		if _, err := tx.ExecContext(ctx,
			`UPDATE tickets SET created_at = ?, last_activity_at = ? WHERE id = ?`,
			created, created, ticketID); err != nil {
			return fmt.Errorf("backdating ticket: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE ticket_timeline SET created_at = ? WHERE ticket_id = ?`, created, ticketID); err != nil {
			return fmt.Errorf("backdating timeline: %w", err)
		}

		path := statusPath[row.Status]
		// Spread the moves evenly across the ticket's life so the timeline has
		// gaps rather than a burst of events at one instant.
		step := time.Duration(row.AgeHours) * time.Hour / time.Duration(len(path)+1)
		from := ticket.StatusNew

		for i, to := range path {
			at := created.Add(step * time.Duration(i+1))

			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ticket_status_history
					(tenant_id, ticket_id, from_status, to_status, actor_id, comment, created_at)
				VALUES (?,?,?,?,?,?,?)`,
				tenantID, ticketID, from, to, agentID, "", at); err != nil {
				return fmt.Errorf("writing status history: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ticket_timeline
					(public_id, tenant_id, ticket_id, event_type, actor_id, actor_name_snapshot,
					 actor_role, visibility, summary, detail_json, created_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
				platform.NewULID(), tenantID, ticketID, "STATUS_CHANGED", agentID,
				"Arjun Rao", "HELPDESK_HEAD", "PUBLIC",
				fmt.Sprintf("Status changed from %s to %s", humanStatus(from), humanStatus(to)),
				fmt.Sprintf(`{"field":"status","from":%q,"to":%q}`, from, to), at); err != nil {
				return fmt.Errorf("writing timeline: %w", err)
			}
			from = to
		}

		last := created.Add(step * time.Duration(len(path)))
		set := `status = ?, last_activity_at = ?, assignee_id = ?`
		args := []any{row.Status, last, agentID}

		// The timestamps a dashboard reads have to agree with the status, or
		// the SLA panel contradicts the badge.
		if len(path) > 0 {
			set += `, first_responded_at = ?`
			args = append(args, created.Add(step))
		}
		if row.Status == "RESOLVED" || row.Status == "CLOSED" {
			set += `, resolved_at = ?`
			args = append(args, last)
		}
		if row.Status == "CLOSED" {
			set += `, closed_at = ?`
			args = append(args, last)
		}
		if row.Status == "REOPENED" {
			set += `, reopened_count = 1, last_reopened_at = ?`
			args = append(args, last)
		}
		if row.Status == "ESCALATED" {
			set += `, escalation_level = 1`
		}
		args = append(args, ticketID)

		if _, err := tx.ExecContext(ctx, `UPDATE tickets SET `+set+` WHERE id = ?`, args...); err != nil {
			return fmt.Errorf("setting final status: %w", err)
		}

		if row.Reply != "" {
			if err := insertConversation(ctx, tx, tenantID, ticketID, agentID, "PUBLIC", row.Reply, last); err != nil {
				return err
			}
		}
		if row.Internal != "" {
			if err := insertConversation(ctx, tx, tenantID, ticketID, agentID, "INTERNAL", row.Internal, last); err != nil {
				return err
			}
		}
		return nil
	})
}

func insertConversation(ctx context.Context, tx *sqlx.Tx, tenantID, ticketID, authorID int64, visibility, body string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO ticket_conversations
			(public_id, tenant_id, ticket_id, author_id, author_role, visibility,
			 body_html, body_text, is_system, created_at)
		VALUES (?,?,?,?,?,?,?,?,0,?)`,
		platform.NewULID(), tenantID, ticketID, authorID, "HELPDESK_HEAD", visibility,
		"<p>"+body+"</p>", body, at)
	if err != nil {
		return fmt.Errorf("writing conversation: %w", err)
	}

	event := "REPLIED"
	summary := "Replied"
	if visibility == "INTERNAL" {
		event, summary = "INTERNAL_NOTE", "Internal note added"
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO ticket_timeline
			(public_id, tenant_id, ticket_id, event_type, actor_id, actor_name_snapshot,
			 actor_role, visibility, summary, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		platform.NewULID(), tenantID, ticketID, event, authorID, "Arjun Rao",
		"HELPDESK_HEAD", visibility, summary, at)
	if err != nil {
		return fmt.Errorf("writing conversation timeline: %w", err)
	}
	return nil
}

func humanStatus(status string) string {
	// The five states, plus the withdrawal. The aliases that used to appear
	// here — OPEN, IN_PROGRESS, RESOLVED, ESCALATED — now resolve to the same
	// constants as their replacements, so listing them would be duplicate cases.
	switch status {
	case ticket.StatusNew:
		return "new"
	case ticket.StatusPendingHelpdesk:
		return "pending with the department"
	case ticket.StatusPendingEmployee:
		return "pending with the employee"
	case ticket.StatusClosed:
		return "closed"
	case ticket.StatusReopened:
		return "reopened"
	case ticket.StatusCancelled:
		return "cancelled"
	default:
		return status
	}
}

func lookupID(ctx context.Context, db *platform.DB, query string, args ...any) (int64, error) {
	var id int64
	if err := db.Primary.GetContext(ctx, &id, query, args...); err != nil {
		if platform.IsNotFound(err) {
			return 0, platform.ErrSentinelNotFound
		}
		return 0, fmt.Errorf("lookup failed: %w", err)
	}
	return id, nil
}

func optionalID(ctx context.Context, db *platform.DB, query string, args ...any) *int64 {
	var id sql.NullInt64
	if err := db.Primary.GetContext(ctx, &id, query, args...); err != nil || !id.Valid {
		return nil
	}
	return &id.Int64
}

// reconcileDemoEmployees creates any roster member the demo client is missing.
//
// Existing people are left untouched: this fills gaps, it does not reset
// anyone's record.
func reconcileDemoEmployees(ctx context.Context, db *platform.DB, cfg *config.Config, tenantID int64) (int, error) {
	userRepo := user.NewRepository(db)

	activeGroup, err := userRepo.GroupByKey(ctx, tenantID, user.GroupActiveEmployees)
	if err != nil {
		return 0, fmt.Errorf("loading active group: %w", err)
	}
	exGroup, err := userRepo.GroupByKey(ctx, tenantID, user.GroupExEmployees)
	if err != nil {
		return 0, fmt.Errorf("loading ex-employee group: %w", err)
	}

	// Every sample account shares the documented password, so the README stays
	// true for people added later.
	hash, err := auth.NewHasher(cfg.Auth).Hash("ComplyDesk@2026")
	if err != nil {
		return 0, fmt.Errorf("hashing sample password: %w", err)
	}

	role, err := userRepo.RoleByKey(ctx, tenantID, user.RoleEmployee)
	if err != nil {
		return 0, fmt.Errorf("loading employee role: %w", err)
	}

	doj := time.Date(2022, 4, 1, 0, 0, 0, 0, time.UTC)
	added := 0

	for i, emp := range demoEmployees {
		var exists int
		if err := db.Primary.GetContext(ctx, &exists,
			`SELECT COUNT(*) FROM users WHERE tenant_id = ? AND email = ?`,
			tenantID, emp.Email); err != nil {
			return 0, fmt.Errorf("checking for %s: %w", emp.Email, err)
		}
		if exists > 0 {
			continue
		}

		status, group := user.StatusActive, activeGroup
		var lwd *time.Time
		if emp.Ex {
			status, group = user.StatusExEmployee, exGroup
			lwd = timePtr(time.Now().AddDate(0, -3, 0))
		}

		created, err := userRepo.Create(ctx, tenantID, user.CreateParams{
			EmployeeCode: emp.Code, FirstName: emp.First, LastName: emp.Last,
			Email: emp.Email, Mobile: fmt.Sprintf("98000300%02d", i+1),
			PFNumber: emp.PF, UANNumber: emp.UAN, PANNumber: emp.PAN,
			ESICNumber: emp.ESIC, Designation: emp.Designation,
			DateOfJoining: &doj,
			DateOfBirth: timePtr(time.Date(1972+(i*4)%28, time.Month(1+(i*5)%12),
				1+(i*9)%28, 0, 0, 0, 0, time.UTC)),
			LastWorkingDay: lwd,
			UserGroupID:    &group.ID, PasswordHash: hash, Status: status,
		})
		if err != nil {
			return 0, fmt.Errorf("creating %s: %w", emp.Email, err)
		}
		if err := userRepo.SetRoles(ctx, tenantID, created.ID, []int64{role.ID}, nil); err != nil {
			return 0, fmt.Errorf("assigning the employee role to %s: %w", emp.Email, err)
		}
		added++
	}
	return added, nil
}
