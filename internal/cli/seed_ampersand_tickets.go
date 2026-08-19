package cli

// Sample tickets for the Ampersand Group dataset.
//
// Every employee gets at least one, filed against the entity they are posted to
// and routed to the agent who works that statutory line — so the whole chain
// Client → Department → Entity → Agent/Partner/Employee → Ticket resolves for
// every row, not only the first few.
//
// The tickets are written directly rather than through the ticket service. The
// service is the right path for a real request and the wrong one here: it
// stamps `created_at` with the current time, and a dataset where every ticket
// was raised in the same second cannot demonstrate a date filter, an SLA state
// or an ageing chart. Writing the rows lets each one be backdated to the day it
// is supposed to have been raised.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/platform"
)

// ampersandQuery is the text of one sample ticket, chosen by the department the
// requester belongs to so a PF employee does not raise an ESIC dispensary query.
type ampersandQuery struct {
	Subject     string
	Description string
	Priority    string
	// Status the ticket has reached. Spread across the workflow so the board,
	// the KPI strip and the status chart all have something in every column.
	Status string
	// Document, when the query is one a real requester would attach something
	// to. Empty means no attachment, which is also realistic.
	DocName string
	DocMime string
	// AgeDays backdates the ticket. Spread from today back through three months
	// so Today / This week / This month / Custom all return different counts.
	AgeDays int
}

var ampersandPFQueries = []ampersandQuery{
	{"PF withdrawal claim not credited", "I filed Form 19 on the EPFO portal three weeks ago and the claim still shows as under process. The amount has not reached my bank account. Please check the status with the regional office.", "HIGH", "IN_PROGRESS", "form-19-acknowledgement.pdf", "application/pdf", 0},
	{"UAN not linked to Aadhaar", "My UAN shows my Aadhaar as not verified even though I completed the KYC on the member portal last month. I cannot file any claim until this is corrected.", "MEDIUM", "NEW", "", "", 0},
	{"Transfer from previous employer pending", "I joined in March and raised a Form 13 transfer request for my PF from my previous employer. The old employer has attested it but the amount has not moved.", "MEDIUM", "PENDING_HELPDESK", "form-13-transfer.pdf", "application/pdf", 1},
	{"Passbook not updating for last two months", "My member passbook has not shown any contribution since June. Salary slips show the deduction, so the ECR may not have been filed.", "MEDIUM", "IN_PROGRESS", "salary-slip-june.pdf", "application/pdf", 2},
	{"Date of joining wrong in EPFO records", "The EPFO record shows my date of joining as 2019 but I joined in 2017. This is reducing my pensionable service. Please raise a joint declaration.", "HIGH", "PENDING_EMPLOYEE", "appointment-letter.pdf", "application/pdf", 3},
	{"PF advance for medical treatment", "I need to withdraw a PF advance for my father's surgery scheduled next month. Please advise which form applies and what documents are needed.", "HIGH", "IN_PROGRESS", "hospital-estimate.pdf", "application/pdf", 5},
	{"e-Nomination not getting submitted", "The e-nomination page throws an error every time I add my spouse's details. I have tried on two different browsers.", "LOW", "NEW", "", "", 6},
	{"Name spelling mismatch with Aadhaar", "My name in the EPFO record is spelt Kavitha and in Aadhaar it is Kavita. The claim was rejected for this reason.", "MEDIUM", "PENDING_HELPDESK", "aadhaar-copy.pdf", "application/pdf", 8},
	{"Pension claim under Form 10C", "I have completed nine years of service and want to apply for the pension withdrawal benefit. Please confirm my eligibility and the process.", "MEDIUM", "RESOLVED", "form-10c-draft.pdf", "application/pdf", 12},
	{"Employer contribution missing for March", "The March contribution does not appear in my passbook although the deduction was made from my salary.", "HIGH", "RESOLVED", "", "", 15},
	{"Service history shows a break", "My service history shows a two month gap in 2021 which never happened. I was on the payroll continuously.", "MEDIUM", "IN_PROGRESS", "", "", 18},
	{"EDLI claim for a deceased colleague's family", "Raising this on behalf of the family of a colleague who passed away last month. They need guidance on the EDLI benefit claim.", "CRITICAL", "ESCALATED", "death-certificate.pdf", "application/pdf", 20},
	{"PF balance inquiry for loan application", "My bank has asked for a PF balance statement for a home loan. Please share the latest passbook extract.", "LOW", "CLOSED", "", "", 25},
	{"Exit date not marked by previous employer", "My previous employer has not marked my date of exit, so my transfer request cannot proceed.", "MEDIUM", "PENDING_HELPDESK", "", "", 30},
	{"KYC update rejected twice", "My bank account KYC has been rejected twice. The account number and IFSC are both correct on the passbook I uploaded.", "MEDIUM", "IN_PROGRESS", "bank-passbook.pdf", "application/pdf", 35},
	{"Digital life certificate for my father", "My father is a pensioner and needs to submit his digital life certificate. He is unable to travel. What are the options?", "LOW", "RESOLVED", "", "", 45},
	{"ECR filing correction for the quarter", "Two employees in my section were left out of the ECR filed for the last quarter. Please advise on the correction.", "HIGH", "CLOSED", "ecr-statement.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", 60},
	{"International worker PF applicability", "We have a colleague on a Japanese passport joining next month. Please confirm the PF applicability and the certificate of coverage position.", "MEDIUM", "CLOSED", "", "", 75},
}

var ampersandESICQueries = []ampersandQuery{
	{"ESIC card not received", "I registered three months ago and my ESIC card has still not been issued. I need it for a consultation this week.", "HIGH", "IN_PROGRESS", "", "", 0},
	{"Dispensary change request", "I have moved from Andheri to Thane and need my dispensary changed to one near my new address.", "LOW", "NEW", "address-proof.pdf", "application/pdf", 1},
	{"Maternity benefit claim status", "I applied for the maternity benefit six weeks ago. The claim is still showing as pending and I have not received any payment.", "HIGH", "PENDING_HELPDESK", "maternity-claim-form.pdf", "application/pdf", 2},
	{"Sickness benefit for hospitalisation", "I was hospitalised for eight days in July. Please advise how to claim the sickness benefit for that period.", "MEDIUM", "IN_PROGRESS", "discharge-summary.pdf", "application/pdf", 4},
	{"IP number not showing in the portal", "My insured person number does not come up on the ESIC portal even though the contribution is being deducted.", "MEDIUM", "PENDING_EMPLOYEE", "", "", 5},
	{"Dependent registration for my mother", "I want to add my mother as a dependent so she can use the ESIC hospital. Please confirm the documents required.", "LOW", "NEW", "", "", 7},
	{"Accident on the shop floor", "A colleague slipped near the packaging line and fractured his wrist. Raising the accident report and the injury claim.", "CRITICAL", "ESCALATED", "accident-report.pdf", "application/pdf", 9},
	{"Temporary disablement benefit not paid", "I have been on medical leave for three weeks after a workplace injury. The temporary disablement benefit has not started.", "HIGH", "IN_PROGRESS", "medical-certificate.pdf", "application/pdf", 11},
	{"Contribution challan mismatch", "The challan filed for last month shows a lower amount than the total deduction. Please reconcile.", "HIGH", "RESOLVED", "esic-challan.pdf", "application/pdf", 14},
	{"Hospitalisation claim reimbursement", "I was treated at a private hospital in an emergency because the ESIC hospital was not reachable. Can I claim reimbursement?", "MEDIUM", "PENDING_HELPDESK", "hospital-bills.pdf", "application/pdf", 17},
	{"ESIC registration for new joiners", "Four new joiners in my department have not been registered with ESIC yet. They joined six weeks ago.", "HIGH", "IN_PROGRESS", "new-joiner-list.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", 22},
	{"Permanent disablement assessment", "Following my injury last year, the medical board has recommended a permanent disablement assessment. What happens next?", "HIGH", "RESOLVED", "", "", 28},
	{"Dependent benefit for a family", "Raising on behalf of the family of a colleague who died in a road accident while on duty. They need the dependent benefit.", "CRITICAL", "RESOLVED", "fir-copy.pdf", "application/pdf", 33},
	{"Return filing due date", "Please confirm the due date for the half-yearly ESIC return and what we need to provide from our side.", "LOW", "CLOSED", "", "", 40},
	{"Contribution period mapping wrong", "My contribution period has been mapped to the wrong half-year, so my benefit eligibility is being calculated incorrectly.", "MEDIUM", "CLOSED", "", "", 55},
	{"Offline claim submission", "The online claim portal has been down for two days and my submission deadline is tomorrow. Can I submit offline?", "MEDIUM", "CLOSED", "", "", 70},
}

// ensureAmpersandTickets files one query for every employee, backdated so the
// dataset spans a quarter rather than a moment.
func ensureAmpersandTickets(ctx context.Context, db *platform.DB, tenantID int64,
	employees []ampersandEmployee, agentIDs map[string]int64,
	entityIDs map[string]int64, deptIDs map[string]int64) (int, int, error) {

	// The category each department's queries are filed under. Both already
	// exist from the demo catalogue; a client missing one simply gets no
	// tickets for that line rather than a failed seed.
	categoryIDs := map[string]int64{}
	for dept, key := range map[string]string{"PF": "PF_QUERY", "ESIC": "ESI_QUERY"} {
		id, err := lookupID(ctx, db,
			`SELECT id FROM categories
			  WHERE tenant_id = ? AND category_key = ? AND is_subcategory = 0 AND deleted_at IS NULL`,
			tenantID, key)
		if err == nil {
			categoryIDs[dept] = id
		}
	}
	if len(categoryIDs) == 0 {
		return 0, 0, fmt.Errorf("no query categories for %s; run `seed --demo` first", ampersandName)
	}

	tickets, docs := 0, 0

	for i, emp := range employees {
		queries := ampersandPFQueries
		if emp.DeptCode == "ESIC" {
			queries = ampersandESICQueries
		}
		q := queries[i%len(queries)]

		categoryID, ok := categoryIDs[emp.DeptCode]
		if !ok {
			continue
		}

		created, docCount, err := insertAmpersandTicket(ctx, db, tenantID, ampersandTicketSpec{
			Query:      q,
			Employee:   emp,
			CategoryID: categoryID,
			EntityID:   entityIDs[emp.EntityCode],
			DeptID:     deptIDs[emp.DeptCode],
			AgentID:    agentIDs[emp.DeptCode],
			Seq:        i + 1,
		})
		if err != nil {
			return tickets, docs, err
		}
		if created {
			tickets++
		}
		docs += docCount
	}

	return tickets, docs, nil
}

type ampersandTicketSpec struct {
	Query      ampersandQuery
	Employee   ampersandEmployee
	CategoryID int64
	EntityID   int64
	DeptID     int64
	AgentID    int64
	Seq        int
}

// insertAmpersandTicket writes one ticket, its opening attachment and the
// agent's first reply, all inside one transaction and all backdated together.
func insertAmpersandTicket(ctx context.Context, db *platform.DB, tenantID int64,
	spec ampersandTicketSpec) (bool, int, error) {

	number := fmt.Sprintf("AMP-%s-%04d", ticketPrefixFor(spec.Employee.DeptCode), spec.Seq)

	// Idempotence is keyed on the ticket number, which is derived from the
	// employee's position in the roster and therefore stable across runs.
	var existing int64
	err := db.Primary.GetContext(ctx, &existing,
		`SELECT id FROM tickets WHERE tenant_id = ? AND ticket_number = ?`, tenantID, number)
	if err == nil {
		return false, 0, nil
	}
	if !platform.IsNotFound(err) {
		return false, 0, fmt.Errorf("checking ticket %s: %w", number, err)
	}

	raisedAt := time.Now().UTC().AddDate(0, 0, -spec.Query.AgeDays).
		Add(time.Duration(-(spec.Seq % 9)) * time.Hour)

	docs := 0
	created := false

	err = db.InTx(ctx, func(tx *sqlx.Tx) error {
		// The requester snapshot is what the ticket shows if the person is
		// later renamed or leaves, so it is written as the service would.
		snapshot, _ := json.Marshal(map[string]any{
			"full_name":     spec.Employee.Name,
			"employee_code": nil,
			"entity":        spec.Employee.EntityCode,
		})

		var entityID, deptID, agentID any
		if spec.EntityID != 0 {
			entityID = spec.EntityID
		}
		if spec.DeptID != 0 {
			deptID = spec.DeptID
		}
		// A brand-new ticket has nobody on it yet; anything further along has
		// been picked up by the line's agent.
		if spec.AgentID != 0 && spec.Query.Status != "NEW" {
			agentID = spec.AgentID
		}

		var resolvedAt any
		if spec.Query.Status == "RESOLVED" || spec.Query.Status == "CLOSED" {
			resolvedAt = raisedAt.Add(time.Duration(18+spec.Seq%40) * time.Hour)
		}

		res, err := tx.ExecContext(ctx, `
			INSERT INTO tickets
				(public_id, tenant_id, ticket_number, category_id, subject, description,
				 status, priority, source, requester_id, requester_snapshot_json,
				 entity_id, department_id, assignee_id, resolved_at,
				 created_at, updated_at, last_activity_at)
			VALUES (?,?,?,?,?,?,?,?,'PORTAL',?,?,?,?,?,?,?,?,?)`,
			platform.NewULID(), tenantID, number, spec.CategoryID,
			spec.Query.Subject, spec.Query.Description,
			spec.Query.Status, spec.Query.Priority,
			spec.Employee.ID, string(snapshot),
			entityID, deptID, agentID, resolvedAt,
			raisedAt, raisedAt, raisedAt)
		if err != nil {
			return fmt.Errorf("creating ticket %s: %w", number, err)
		}
		created = true

		ticketID, err := res.LastInsertId()
		if err != nil {
			return err
		}

		// The supporting document, stored as a real row so the Attachments tab
		// and the ticket detail both have something to show and to authorise.
		if spec.Query.DocName != "" {
			docID, err := insertAmpersandDocument(ctx, tx, tenantID, ticketID,
				spec.Employee.ID, spec.Query, raisedAt)
			if err != nil {
				return err
			}
			if docID > 0 {
				docs++
			}
		}

		// The agent's first reply, on everything they have picked up. Without
		// it the conversation tab is empty on tickets that are visibly in
		// progress, which reads as data loss rather than as a fresh queue.
		if agentID != nil {
			reply := agentReplyFor(spec.Query.Status)
			if reply != "" {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO ticket_conversations
						(public_id, tenant_id, ticket_id, author_id, author_role, visibility,
						 body_html, body_text, created_at)
					VALUES (?,?,?,?, 'HELPDESK_EXECUTIVE', 'PUBLIC', ?, ?, ?)`,
					platform.NewULID(), tenantID, ticketID, spec.AgentID,
					"<p>"+reply+"</p>", reply,
					raisedAt.Add(4*time.Hour)); err != nil {
					return fmt.Errorf("adding the agent reply to %s: %w", number, err)
				}
			}
		}

		return nil
	})
	if err != nil {
		return false, 0, err
	}
	return created, docs, nil
}

// insertAmpersandDocument writes the document row and links it to the ticket.
//
// The bytes are a small placeholder rather than a real PDF: the point is that
// the record, the link and the permission check all exist, which is what the
// Attachments tab and the download authorisation are tested against. A seed
// that wrote real binaries would need them checked into the repository.
func insertAmpersandDocument(ctx context.Context, tx *sqlx.Tx, tenantID, ticketID,
	uploaderID int64, q ampersandQuery, at time.Time) (int64, error) {

	publicID := platform.NewULID()
	// Deterministic checksum, so a re-run of the seed does not look like a
	// second distinct upload of the same file.
	sum := sha256.Sum256([]byte(publicID + q.DocName))

	res, err := tx.ExecContext(ctx, `
		INSERT INTO documents
			(public_id, tenant_id, original_name, stored_path, mime_type, size_bytes,
			 checksum_sha256, description, owner_type, owner_id, uploaded_by,
			 is_encrypted, scan_status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,'TICKET',?,?,0,'CLEAN',?,?)`,
		publicID, tenantID, q.DocName,
		fmt.Sprintf("sample/%d/%s", tenantID, publicID),
		q.DocMime, 24576, hex.EncodeToString(sum[:]),
		"Supporting document supplied when the query was raised",
		ticketID, uploaderID, at, at)
	if err != nil {
		return 0, fmt.Errorf("creating document %s: %w", q.DocName, err)
	}

	docID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ticket_attachments
			(tenant_id, ticket_id, document_id, context, uploaded_by, created_at)
		VALUES (?,?,?, 'REQUESTER', ?, ?)`,
		tenantID, ticketID, docID, uploaderID, at); err != nil {
		return 0, fmt.Errorf("attaching document %s: %w", q.DocName, err)
	}

	return docID, nil
}

func ticketPrefixFor(dept string) string {
	if dept == "ESIC" {
		return "ESI"
	}
	return "PF"
}

// agentReplyFor is what the desk said first, matched to how far the ticket got.
func agentReplyFor(status string) string {
	switch status {
	case "NEW":
		return ""
	case "PENDING_EMPLOYEE":
		return "Thank you for raising this. We have reviewed what you sent and need one more document before we can take it to the regional office — please see the request above."
	case "PENDING_HELPDESK":
		return "We have logged this with the regional office and are waiting for their confirmation. We will update you as soon as we hear back."
	case "RESOLVED":
		return "This has now been completed at the regional office and the correction is reflected in your record. Please confirm you can see it, and we will close the ticket."
	case "CLOSED":
		return "Completed and confirmed with you. Closing this ticket — please raise a new one if anything further is needed."
	case "ESCALATED":
		return "Given the urgency we have escalated this to the regional office directly and it is being handled as a priority. We will call you with an update today."
	default:
		return "Thank you for raising this. We have picked it up and are checking the position with the regional office."
	}
}
