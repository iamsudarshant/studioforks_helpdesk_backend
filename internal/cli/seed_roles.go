package cli

import "github.com/karmamgmt/complydesk/internal/user"

// Every role key ever seeded is kept as a deprecated alias so existing tokens,
// saved filters and integrations keep working — `roles.alias_of` records the
// mapping and the permission resolver follows it.
//
//	KARMA_SUPER_ADMIN, HELPDESK_MASTER_ADMIN, SUPER_ADMIN -> SUPER_ADMIN
//	AGENT, KARMA_AGENT, HELPDESK_ADMIN                    -> HELPDESK_HEAD
//	PARTNER, CLIENT_MASTER_ADMIN                          -> CLIENT_ADMIN
//	PARTNER_EXECUTIVE, ENTITY_ADMIN, DEPARTMENT_ADMIN     -> CLIENT_EXECUTIVE
//	EMPLOYEE                                              -> EMPLOYEE (unchanged)
//
// AGENT maps to HELPDESK_HEAD rather than HELPDESK_EXECUTIVE because it carried
// the wider grant of the two; demoting existing agents would take away access
// they already had. PARTNER maps to CLIENT_ADMIN for the same reason.
var roleAliases = map[string]string{
	user.RoleKarmaSuperAdmin:     user.RoleSuperAdmin,
	user.RoleHelpdeskMasterAdmin: user.RoleSuperAdmin,
	user.RoleAgent:               user.RoleHelpdeskHead,
	user.RoleKarmaAgent:          user.RoleHelpdeskHead,
	user.RoleHelpdeskAdmin:       user.RoleHelpdeskHead,
	user.RolePartner:             user.RoleClientAdmin,
	user.RoleClientMasterAdmin:   user.RoleClientAdmin,
	user.RolePartnerExecutive:    user.RoleClientExecutive,
	user.RoleEntityAdmin:         user.RoleClientExecutive,
	user.RoleDepartmentAdmin:     user.RoleClientExecutive,
}

// platformPermissions are the ones only ComplyDesk's own staff may ever hold. A
// client-side role must never be granted these, whatever an administrator edits
// in the role editor, because they cross the client boundary.
var platformPermissions = []permission{
	{"module.view", "Platform", "View the module catalogue"},
	{"module.manage", "Platform", "Add, edit and retire modules"},
	{"module.assign", "Platform", "Enable or disable modules for a client"},
	{"agent.assign", "Platform", "Set which agent owns which client"},
	{"agent.view", "Platform", "See which agents are assigned to which clients"},
	{"partner_executive.create", "Client", "Create partner executives under a partner"},
}

// canonicalRoles is ComplyDesk's role model: three staff roles that work across
// every client, two client-side roles confined to one workspace, and the end
// user.
var canonicalRoles = []systemRole{
	{
		Key: user.RoleSuperAdmin, Name: "Super Admin", Portal: "admin",
		Description: "Complete system access. Manages clients, sites, users, categories, " +
			"SLAs, global reports, dashboards and system notifications.",
		Permissions: allPermissions(),
	},
	{
		Key: user.RoleHelpdeskHead, Name: "Helpdesk Head", Portal: "agents",
		Description: "Runs the desk across every client. Everything an executive can do, " +
			"plus assigning and transferring work, escalating, closing, and operational " +
			"reporting. Also creates and administers clients.",
		Permissions: []string{
			"ticket.create", "ticket.view.all", "ticket.update",
			"ticket.reply.public", "ticket.reply.internal", "ticket.status.change",
			"ticket.assign", "ticket.transfer", "ticket.escalate",
			"ticket.close", "ticket.reopen", "ticket.cancel",
			"ticket.bulk", "ticket.export", "ticket.watch", "ticket.moderate",
			"user.create", "user.update", "user.delete", "user.view.all", "user.view.pii",
			"user.bulk_import", "user.move_group", "user.send_reset",
			"partner_executive.create",
			// Client administration. `client.purge` stays with the super admin:
			// a head's delete is recoverable, a purge is not.
			"client.view", "client.create", "client.update", "client.delete",
			"org.view", "org.create", "org.update", "org.delete", "org.registration",
			"config.org", "config.category", "config.workflow", "config.routing",
			"config.sla", "config.group", "config.notification",
			"agent.view", "agent.assign",
			"module.view", "module.assign",
			"document.upload", "document.view", "document.download", "document.version",
			"report.view", "report.custom", "report.export", "dashboard.view",
			"audit.view", "analytics.view",
			"help.view", "help.reply", "help.manage",
		},
	},
	{
		Key: user.RoleHelpdeskExecutive, Name: "Helpdesk Executive", Portal: "agents",
		Description: "Works tickets across every client: responds, requests information, " +
			"uploads documents, updates status, adds internal notes and transfers. " +
			"Does not administer clients or delete users.",
		Permissions: []string{
			"ticket.create", "ticket.view.all", "ticket.update",
			"ticket.reply.public", "ticket.reply.internal", "ticket.status.change",
			"ticket.transfer", "ticket.escalate", "ticket.close", "ticket.reopen",
			"ticket.export", "ticket.watch",
			// Full management of a client's people and their tickets. Both staff
			// portals administer employees and partners; what separates an
			// executive from a head is client deletion and bulk operations, not
			// day-to-day user work.
			"user.view.all", "user.view.pii", "user.create", "user.update", "user.delete",
			"user.send_reset", "user.bulk_import", "user.move_group",
			"ticket.assign", "ticket.cancel",
			"partner_executive.create",
			// §5 of the development brief puts "create client" and
			// "edit / suspend / disable client" on the CD Executive row, and
			// leaves only "delete client" to the admin. An executive who
			// creates a client therefore needs to create and edit one.
			"client.view", "client.create", "client.update",
			"org.view",
			"document.upload", "document.view", "document.download", "document.version",
			"report.view", "report.export", "dashboard.view",
			"module.view",
			"help.view", "help.reply",
		},
	},
	{
		Key: user.RoleClientAdmin, Name: "Client Admin", Portal: "partner",
		Description: "Client-side administrator. Sees every entity and location allocated " +
			"to them: dashboards, ticket search, SLA monitoring and report downloads. " +
			"Cannot reply to tickets unless that permission is explicitly granted.",
		Permissions: []string{
			// Read and oversight across the whole client. Note the absence of
			// `ticket.reply.public`: replying is off by default and is granted to
			// the individual user when the client asks for it.
			"ticket.view.all", "ticket.create", "ticket.export",
			// The client's people, read-only. A partner sees every employee of
			// their client, including the statutory identifiers a query is
			// answered against — but the master record is maintained by the
			// helpdesk, so changes are asked for rather than made. Granting
			// user.create and user.update here let a partner edit and remove
			// employees outright, which is not what "oversight" means.
			"user.view.all", "user.view.pii",
			"partner_executive.create",
			"client.view", "client.update",
			// Organisation structure, read-only for the same reason. org.update
			// is what enables the disable toggle on entities, sites and
			// departments — switching an establishment off hides its tickets
			// from everybody, which is a helpdesk decision.
			"org.view",
			"document.view", "document.download", "document.upload",
			"report.view", "report.export", "dashboard.view",
			"help.view",
		},
	},
	{
		Key: user.RoleClientExecutive, Name: "Client Executive", Portal: "partner",
		Description: "Client-side user segmented to their own entity or location. Raises " +
			"tickets on behalf of the employees there and tracks them. Cannot edit " +
			"responses or reach another entity's data.",
		Permissions: []string{
			// `ticket.view.scope` rather than `.all` is what confines them: the
			// entity/site/department scope on the user does the rest.
			"ticket.create", "ticket.view.scope", "ticket.export",
			// `user.view.scope` is what makes an employee selectable as the
			// requester when raising on their behalf — but only their own.
			"user.view.scope",
			"client.view", "org.view",
			"document.view", "document.download", "document.upload",
			"report.view", "report.export", "dashboard.view",
			"help.view",
		},
	},
	{
		Key: user.RoleEmployee, Name: "Employee", Portal: "user",
		Description: "End user. Raises and tracks their own tickets, attaches and downloads " +
			"files, replies, and reopens their own resolved tickets within the window.",
		Permissions: []string{
			"ticket.create", "ticket.view.own", "ticket.reply.public",
			"ticket.close", "ticket.reopen", "ticket.feedback",
			"document.upload", "document.view", "document.download",
			// An employee's reports cover their own tickets and nothing else —
			// the ticket scope already narrows every report to what the reader
			// may see, so "my tickets, as a spreadsheet" needs no separate
			// surface and no extra confinement.
			"dashboard.view", "report.view", "report.export",
			"help.view",
		},
	},
}

// module is a compliance module in the platform catalogue.
type module struct {
	Key         string
	Name        string
	Description string
	Icon        string
	Color       string
	IsCore      bool
	SortOrder   int
}

// moduleCatalogue covers the launch modules plus the expansion set named in the
// master prompt. Adding another is a row here, never a code change.
var moduleCatalogue = []module{
	{Key: "PF", Name: "Provident Fund", Icon: "Savings", Color: "#1A73E8", IsCore: true, SortOrder: 10,
		Description: "EPFO queries: withdrawal, transfer, pension, passbook and KYC."},
	{Key: "ESIC", Name: "ESIC", Icon: "LocalHospital", Color: "#00897B", IsCore: true, SortOrder: 20,
		Description: "Employees' State Insurance: cards, dispensaries, claims and dependants."},
	{Key: "PAYROLL", Name: "Payroll", Icon: "Payments", Color: "#F9AB00", SortOrder: 30,
		Description: "Salary, payslips, reimbursements and tax deductions."},
	{Key: "HR", Name: "Human Resources", Icon: "Groups", Color: "#9334E6", SortOrder: 40,
		Description: "Leave, attendance, policy and documentation requests."},
	{Key: "LABOUR_LAW", Name: "Labour Law", Icon: "Gavel", Color: "#D93025", SortOrder: 50,
		Description: "Statutory labour law advisory and compliance queries."},
	{Key: "COMPLIANCE", Name: "Compliance", Icon: "VerifiedUser", Color: "#188038", SortOrder: 60,
		Description: "Registers, returns, filings and audit support."},
	{Key: "PT", Name: "Professional Tax", Icon: "AccountBalance", Color: "#3C4043", SortOrder: 70,
		Description: "Professional tax registration, deduction and remittance."},
	{Key: "LWF", Name: "Labour Welfare Fund", Icon: "VolunteerActivism", Color: "#E8710A", SortOrder: 80,
		Description: "Labour Welfare Fund contributions and returns."},
	{Key: "FACTORY", Name: "Factory Compliance", Icon: "Factory", Color: "#5F6368", SortOrder: 90,
		Description: "Factories Act licensing, registers and inspections."},
	{Key: "SHOPS", Name: "Shops & Establishment", Icon: "Storefront", Color: "#0B8043", SortOrder: 100,
		Description: "Shops and Establishment registration and renewals."},
	{Key: "GENERAL", Name: "General Queries", Icon: "HelpOutline", Color: "#5F6368", IsCore: true, SortOrder: 110,
		Description: "Anything that does not belong to another module."},
	{Key: "IT_SUPPORT", Name: "IT Support", Icon: "Computer", Color: "#1A73E8", SortOrder: 120,
		Description: "Hardware, software, access and connectivity issues."},
}

// categoryModule maps the seeded demo categories onto their module, so a client
// that disables Payroll stops seeing payroll categories everywhere at once.
var categoryModule = map[string]string{
	"PF_QUERY":   "PF",
	"ESI_QUERY":  "ESIC",
	"PAYROLL":    "PAYROLL",
	"HR_QUERY":   "HR",
	"IT_SUPPORT": "IT_SUPPORT",
	"GENERAL":    "GENERAL",
}

// subcategory is a second level under a category. The master prompt requires
// categories AND subcategories; these are the ones the launch modules need.
type subcategory struct {
	ParentKey string
	Key       string
	Name      string
}

var subcategories = []subcategory{
	// The twelve PF query types named in §8 of the development brief.
	{"PF_QUERY", "PF_WITHDRAWAL", "PF Withdrawals"},
	{"PF_QUERY", "PF_TRANSFER", "PF Transfers"},
	{"PF_QUERY", "PF_UAN", "UAN Issues"},
	{"PF_QUERY", "PF_KYC", "KYC Updates"},
	{"PF_QUERY", "PF_PASSBOOK", "Member Passbook"},
	{"PF_QUERY", "PF_PENSION", "Pension"},
	{"PF_QUERY", "PF_EDLI", "EDLI"},
	{"PF_QUERY", "PF_CONTRIBUTION", "Employer Contributions"},
	{"PF_QUERY", "PF_EXIT", "Exit Details"},
	{"PF_QUERY", "PF_SERVICE_HISTORY", "Service History"},
	{"PF_QUERY", "PF_CLAIM_STATUS", "Claim Status"},
	{"PF_QUERY", "PF_CORRECTION", "Correction Requests"},

	// The six ESIC types from §8.
	{"ESI_QUERY", "ESI_REGISTRATION", "ESIC Registration"},
	{"ESI_QUERY", "ESI_CARD", "ESIC Card"},
	{"ESI_QUERY", "ESI_DISPENSARY", "ESIC Dispensary"},
	{"ESI_QUERY", "ESI_CONTRIBUTION", "Employer Contributions"},
	{"ESI_QUERY", "ESI_CLAIM", "Claim Status"},
	{"ESI_QUERY", "ESI_CORRECTION", "Correction Requests"},
	// Retained beyond §8 because the demo data references it and removing a
	// subcategory would orphan the tickets filed against it.
	{"ESI_QUERY", "ESI_DEPENDANT", "Dependant Addition"},

	// General, per §8.
	{"GENERAL", "GEN_PAYROLL", "Payroll Query"},
	{"GENERAL", "GEN_FNF", "Full & Final Settlement"},
	{"GENERAL", "GEN_OTHER", "Other / Miscellaneous"},

	{"PAYROLL", "PAY_PAYSLIP", "Payslip"},
	{"PAYROLL", "PAY_AMOUNT", "Incorrect Amount"},
	{"PAYROLL", "PAY_REIMBURSEMENT", "Reimbursement"},
	{"PAYROLL", "PAY_TDS", "TDS & Tax"},

	{"HR_QUERY", "HR_LEAVE", "Leave"},
	{"HR_QUERY", "HR_ATTENDANCE", "Attendance"},
	{"HR_QUERY", "HR_LETTER", "Letter Request"},
	{"HR_QUERY", "HR_GRIEVANCE", "Grievance"},

	{"IT_SUPPORT", "IT_HARDWARE", "Hardware"},
	{"IT_SUPPORT", "IT_SOFTWARE", "Software"},
	{"IT_SUPPORT", "IT_ACCESS", "Access Request"},
	{"IT_SUPPORT", "IT_NETWORK", "Network & Connectivity"},
}
