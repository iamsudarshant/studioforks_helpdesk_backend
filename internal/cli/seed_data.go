package cli

import "github.com/karmamgmt/complydesk/internal/user"

// permission is one entry of the platform permission catalogue. Authorisation
// is permission-based throughout, so this list is the vocabulary every role is
// built from and the admin panel's role editor renders.
type permission struct {
	Key         string
	Group       string
	Description string
}

var permissions = []permission{
	// Tickets
	{"ticket.create", "Tickets", "Raise a ticket"},
	{"ticket.view.own", "Tickets", "View tickets you raised"},
	{"ticket.view.scope", "Tickets", "View tickets within your assigned entities, sites and departments"},
	{"ticket.view.all", "Tickets", "View every ticket in the workspace"},
	{"ticket.update", "Tickets", "Edit ticket subject, priority, category and custom fields"},
	{"ticket.reply.public", "Tickets", "Reply on a ticket, visible to the requester"},
	{"ticket.reply.internal", "Tickets", "Add internal notes visible only to the helpdesk team"},
	{"ticket.status.change", "Tickets", "Change ticket status"},
	{"ticket.assign", "Tickets", "Assign a ticket to an executive"},
	{"ticket.transfer", "Tickets", "Transfer a ticket to another executive or department"},
	{"ticket.escalate", "Tickets", "Escalate a ticket"},
	{"ticket.close", "Tickets", "Close a ticket"},
	{"ticket.reopen", "Tickets", "Reopen a closed ticket"},
	{"ticket.cancel", "Tickets", "Cancel a ticket"},
	{"ticket.bulk", "Tickets", "Perform bulk actions on multiple tickets"},
	{"ticket.export", "Tickets", "Export ticket lists"},
	{"ticket.feedback", "Tickets", "Submit satisfaction feedback"},
	{"ticket.watch", "Tickets", "Add or remove ticket watchers"},
	{"ticket.moderate", "Tickets", "Edit or withdraw another person's reply, and remove their attachments"},

	// Users
	{"user.create", "Users", "Create users"},
	{"user.update", "Users", "Edit users"},
	{"user.delete", "Users", "Remove users"},
	{"user.view.scope", "Users", "View users within your assigned scope"},
	{"user.view.all", "Users", "View every user in the workspace"},
	{"user.view.pii", "Users", "See full PAN, UAN, PF and ESIC numbers"},
	{"user.bulk_import", "Users", "Import users in bulk"},
	{"user.move_group", "Users", "Move users between groups, including to ex-employees"},
	{"user.send_reset", "Users", "Send a password reset link to a user"},
	{"user.impersonate", "Users", "Sign in as another user"},

	// Documents
	{"document.upload", "Documents", "Upload documents"},
	{"document.view", "Documents", "Preview documents"},
	{"document.download", "Documents", "Download documents"},
	{"document.delete", "Documents", "Delete documents"},
	{"document.version", "Documents", "Add new versions of a document"},

	// Reports and dashboards
	{"report.view", "Reports", "Run and view standard reports"},
	{"report.custom", "Reports", "Build and save custom reports"},
	{"report.schedule", "Reports", "Schedule recurring reports"},
	{"report.export", "Reports", "Export report output"},
	{"dashboard.view", "Reports", "View dashboards"},

	// Client master.
	//
	// Split by verb because the three portals differ precisely here: admins may
	// do everything, agents may do everything except erase, and partners may
	// only edit the client they belong to.
	{"client.view", "Client", "View client master records"},
	{"client.create", "Client", "Onboard a new client"},
	{"client.update", "Client", "Edit client master details"},
	{"client.delete", "Client", "Deactivate a client (recoverable)"},
	{"client.purge", "Client", "Permanently erase a client and its data"},

	// Organisation structure, split by verb for the same reason.
	{"org.view", "Configuration", "View entities, sites and departments"},
	{"org.create", "Configuration", "Add entities, sites and departments"},
	{"org.update", "Configuration", "Edit entities, sites and departments"},
	{"org.delete", "Configuration", "Remove entities, sites and departments (recoverable)"},
	{"org.purge", "Configuration", "Permanently erase an entity, site or department"},
	{"org.registration", "Configuration", "Manage PF and ESIC registrations against entities"},

	// Configuration
	{"config.org", "Configuration", "Manage entities, sites and departments"},
	{"config.category", "Configuration", "Manage query categories and custom fields"},
	{"config.workflow", "Configuration", "Manage ticket workflow transitions"},
	{"config.routing", "Configuration", "Manage automatic routing rules"},
	{"config.sla", "Configuration", "Manage SLA policies and business hours"},
	{"config.group", "Configuration", "Manage user groups"},
	{"config.role", "Configuration", "Manage roles and permissions"},
	{"config.branding", "Configuration", "Manage logo, colours and branding"},
	{"config.feature", "Configuration", "Enable or disable features"},
	{"config.notification", "Configuration", "Manage notification events and templates"},
	{"config.settings", "Configuration", "Manage workspace settings"},
	{"config.apikey", "Configuration", "Manage API keys"},

	// Platform and audit
	{"tenant.manage", "Platform", "Create and manage client workspaces"},
	{"tenant.maintenance", "Platform", "Turn maintenance mode on and off"},
	{"audit.view", "Audit", "View the audit log and user activity"},
	{"audit.export", "Audit", "Export audit data"},
	{"analytics.view", "Audit", "View login and usage analytics"},

	// Help: the FAQ and the Request Help ticket thread.
	{"help.view", "Help", "Read the FAQ and raise or view help requests"},
	{"help.reply", "Help", "Reply to and resolve help requests"},
	{"help.manage", "Help", "Maintain the FAQ knowledge base"},
}

// systemRole is a seeded role. Portal binding is enforced at login: a user can
// only sign in at the portal one of their roles names.
type systemRole struct {
	Key         string
	Name        string
	Description string
	Portal      string
	Permissions []string
}

// allPermissions is used by roles that hold the full catalogue.
func allPermissions() []string {
	out := make([]string, 0, len(permissions))
	for _, p := range permissions {
		out = append(out, p.Key)
	}
	return out
}

// systemRoles implements the portal mapping decided in docs/prompts/README.md:
// /admin is our platform operators, /agents our helpdesk staff, /partner every
// client-side administrative role, /user employees.
var systemRoles = []systemRole{
	{
		Key: user.RoleSuperAdmin, Name: "Super Administrator", Portal: "admin",
		Description: "Full platform access across every client workspace.",
		Permissions: allPermissions(),
	},
	{
		Key: user.RoleHelpdeskMasterAdmin, Name: "Helpdesk Master Administrator", Portal: "admin",
		Description: "Complete access within a client workspace.",
		Permissions: []string{
			"ticket.create", "ticket.view.all", "ticket.update", "ticket.reply.public",
			"ticket.reply.internal", "ticket.status.change", "ticket.assign", "ticket.transfer",
			"ticket.escalate", "ticket.close", "ticket.reopen", "ticket.cancel", "ticket.bulk",
			"ticket.export", "ticket.watch",
			"user.create", "user.update", "user.delete", "user.view.all", "user.view.pii",
			"user.bulk_import", "user.move_group", "user.send_reset",
			"document.upload", "document.view", "document.download", "document.delete", "document.version",
			"report.view", "report.custom", "report.schedule", "report.export", "dashboard.view",
			"config.org", "config.category", "config.workflow", "config.routing", "config.sla",
			"config.group", "config.role", "config.branding", "config.feature",
			"config.notification", "config.settings", "config.apikey",
			// Client master and organisation: everything, erase included.
			"client.view", "client.create", "client.update", "client.delete", "client.purge",
			"org.view", "org.create", "org.update", "org.delete", "org.purge", "org.registration",
			"help.view", "help.reply", "help.manage",
			"audit.view", "audit.export", "analytics.view",
		},
	},
	{
		Key: user.RoleHelpdeskAdmin, Name: "Helpdesk Administrator", Portal: "agents",
		Description: "Runs the helpdesk: assigns work, configures categories and SLAs, sees every ticket.",
		Permissions: []string{
			"ticket.create", "ticket.view.all", "ticket.update", "ticket.reply.public",
			"ticket.reply.internal", "ticket.status.change", "ticket.assign", "ticket.transfer",
			"ticket.escalate", "ticket.close", "ticket.reopen", "ticket.cancel", "ticket.bulk",
			"ticket.export", "ticket.watch",
			"user.create", "user.update", "user.view.all", "user.view.pii",
			"user.bulk_import", "user.move_group", "user.send_reset",
			"document.upload", "document.view", "document.download", "document.delete", "document.version",
			"report.view", "report.custom", "report.export", "dashboard.view",
			"config.category", "config.workflow", "config.routing", "config.sla", "config.group",
			"config.notification",
			// Agents get the full client and organisation surface, but delete is
			// recoverable only — erasing a client is reserved for admins.
			"client.view", "client.create", "client.update", "client.delete",
			"org.view", "org.create", "org.update", "org.delete", "org.registration",
			"config.org",
			"help.view", "help.reply", "help.manage",
			"audit.view", "analytics.view",
		},
	},
	{
		Key: user.RoleHelpdeskExecutive, Name: "Helpdesk Executive", Portal: "agents",
		Description: "Resolves tickets within the entities, sites and departments assigned to them.",
		Permissions: []string{
			"ticket.create", "ticket.view.scope", "ticket.update", "ticket.reply.public",
			"ticket.reply.internal", "ticket.status.change", "ticket.assign", "ticket.transfer",
			"ticket.escalate", "ticket.close", "ticket.watch", "ticket.export",
			"user.view.scope",
			"document.upload", "document.view", "document.download", "document.version",
			"report.view", "dashboard.view",
			// Executives maintain the structure they work against, recoverably.
			"client.view",
			"org.view", "org.create", "org.update", "org.delete", "org.registration",
			"config.org",
			"help.view", "help.reply",
		},
	},
	{
		Key: user.RoleClientMasterAdmin, Name: "Client Master Administrator", Portal: "partner",
		Description: "Oversees every entity belonging to the client. May correct client " +
			"and organisation details, but never add or remove a client.",
		Permissions: []string{
			"ticket.view.all", "ticket.export",
			"user.view.all",
			"document.view", "document.download",
			"report.view", "report.export", "dashboard.view",
			// Edit only: no client.create, no client.delete, no org.create/delete.
			"client.view", "client.update",
			"org.view", "org.update",
			"help.view",
		},
	},
	{
		Key: user.RoleEntityAdmin, Name: "Entity Administrator", Portal: "partner",
		Description: "Sees tickets and users for the entities and sites assigned to them.",
		Permissions: []string{
			"ticket.view.scope", "ticket.export",
			"user.view.scope",
			"document.view", "document.download",
			"report.view", "report.export", "dashboard.view",
			"client.view", "org.view",
			"help.view",
		},
	},
	{
		Key: user.RoleDepartmentAdmin, Name: "Department Administrator", Portal: "partner",
		Description: "Handles tickets pending on their department and supplies departmental input.",
		Permissions: []string{
			"ticket.view.scope", "ticket.reply.public", "ticket.reply.internal",
			"ticket.status.change", "ticket.export",
			"user.view.scope",
			"document.view", "document.download", "document.upload",
			"report.view", "dashboard.view",
			"client.view", "org.view",
			"help.view",
		},
	},
	{
		Key: user.RoleEmployee, Name: "Employee", Portal: "user",
		Description: "Raises and tracks their own tickets.",
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

// notificationEvent seeds the catalogue the template editor and preference
// screen render.
type notificationEvent struct {
	Key             string
	Group           string
	Description     string
	Variables       []string
	DefaultChannels []string
}

var notificationEvents = []notificationEvent{
	{"ticket.created", "Tickets", "A ticket was raised",
		[]string{"ticket_number", "subject", "requester_name", "category", "link"}, []string{"EMAIL", "IN_APP"}},
	{"ticket.assigned", "Tickets", "A ticket was assigned to you",
		[]string{"ticket_number", "subject", "assignee_name", "link"}, []string{"EMAIL", "IN_APP"}},
	{"ticket.status_changed", "Tickets", "A ticket's status changed",
		[]string{"ticket_number", "subject", "from_status", "to_status", "actor_name", "link"}, []string{"EMAIL", "IN_APP"}},
	{"ticket.replied", "Tickets", "Someone replied on a ticket",
		[]string{"ticket_number", "subject", "author_name", "excerpt", "link"}, []string{"EMAIL", "IN_APP"}},
	{"ticket.info_requested", "Tickets", "More information was requested from the employee",
		[]string{"ticket_number", "subject", "message", "link"}, []string{"EMAIL", "IN_APP"}},
	{"ticket.escalated", "Tickets", "A ticket was escalated",
		[]string{"ticket_number", "subject", "level", "reason", "link"}, []string{"EMAIL", "IN_APP"}},
	{"ticket.sla_warning", "Tickets", "A ticket is approaching its SLA target",
		[]string{"ticket_number", "subject", "due_at", "percent_elapsed", "link"}, []string{"EMAIL", "IN_APP"}},
	{"ticket.sla_breached", "Tickets", "A ticket breached its SLA target",
		[]string{"ticket_number", "subject", "due_at", "link"}, []string{"EMAIL", "IN_APP"}},
	{"ticket.resolved", "Tickets", "A ticket was resolved",
		[]string{"ticket_number", "subject", "resolution", "link"}, []string{"EMAIL", "IN_APP"}},
	{"ticket.closed", "Tickets", "A ticket was closed",
		[]string{"ticket_number", "subject", "link"}, []string{"EMAIL", "IN_APP"}},
	{"ticket.reopened", "Tickets", "A ticket was reopened",
		[]string{"ticket_number", "subject", "reason", "link"}, []string{"EMAIL", "IN_APP"}},
	{"ticket.reminder_pending_user", "Tickets", "A reminder that a ticket is waiting on the employee",
		[]string{"ticket_number", "subject", "days_waiting", "link"}, []string{"EMAIL", "IN_APP"}},
	{"ticket.mentioned", "Tickets", "You were mentioned on a ticket",
		[]string{"ticket_number", "subject", "author_name", "link"}, []string{"IN_APP"}},

	{"user.welcome", "Accounts", "A new account was created",
		[]string{"full_name", "username", "activation_url", "portal_url"}, []string{"EMAIL"}},
	{"user.temp_password", "Accounts", "A temporary password was issued",
		[]string{"full_name", "username", "temporary_password", "portal_url", "expires_hours"}, []string{"EMAIL"}},
	{"user.password_reset_link", "Accounts", "A password reset link was sent",
		[]string{"full_name", "username", "reset_url", "expires_mins"}, []string{"EMAIL"}},
	{"user.username_recovery", "Accounts", "A username reminder was requested",
		[]string{"full_name", "username", "portal_url"}, []string{"EMAIL"}},
	{"user.group_changed", "Accounts", "A user was moved to a different group",
		[]string{"full_name", "to_group", "last_working_day", "access_expires_on"}, []string{"EMAIL", "IN_APP"}},
	{"user.account_locked", "Accounts", "An account was locked after failed sign-in attempts",
		[]string{"full_name", "minutes"}, []string{"EMAIL"}},
	{"user.login_otp", "Accounts", "A one-time sign-in code was sent",
		[]string{"code", "expires_mins"}, []string{"SMS"}},

	{"report.ready", "Reports", "A report finished generating",
		[]string{"report_name", "row_count", "download_url"}, []string{"EMAIL", "IN_APP"}},
	{"bulk_import.completed", "Reports", "A bulk user import finished",
		[]string{"total_rows", "imported_rows", "failed_rows", "link"}, []string{"EMAIL", "IN_APP"}},

	{"maintenance.scheduled", "Platform", "Maintenance has been scheduled",
		[]string{"title", "message", "starts_at", "ends_at"}, []string{"EMAIL", "IN_APP"}},
	{"maintenance.started", "Platform", "Maintenance has started",
		[]string{"title", "message", "ends_at"}, []string{"IN_APP"}},
	{"maintenance.ended", "Platform", "Maintenance has finished",
		[]string{"title"}, []string{"IN_APP"}},
}

// demoCategory seeds a starter query catalogue. PF and ESI ship first because
// they are the launch domain; the others prove the model generalises.
type demoCategory struct {
	Key            string
	Name           string
	Prefix         string
	Description    string
	Color          string
	RequiresFields []string
	Fields         []demoField
}

type demoField struct {
	Key        string
	Label      string
	Type       string
	Required   bool
	Options    []string
	HelpText   string
	Validation string
}

var demoCategories = []demoCategory{
	{
		Key: "PF_QUERY", Name: "PF Query", Prefix: "PF", Color: "#1A73E8",
		Description:    "Provident Fund withdrawal, transfer, pension and passbook queries.",
		RequiresFields: []string{"pf_number", "uan_number", "date_of_joining"},
		Fields: []demoField{
			{Key: "query_type", Label: "Type of PF query", Type: "SELECT", Required: true,
				Options: []string{"Withdrawal", "Transfer", "Pension", "Passbook", "KYC update", "Correction", "Other"}},
			{Key: "claim_reference", Label: "Claim reference number", Type: "TEXT",
				HelpText: "If you have already filed a claim, enter its reference number."},
			{Key: "previous_employer", Label: "Previous employer", Type: "TEXT",
				HelpText: "Required for transfer requests."},
			{Key: "exit_date", Label: "Date of exit from previous employer", Type: "DATE"},
			{Key: "amount_expected", Label: "Amount expected", Type: "CURRENCY"},
			{Key: "supporting_document", Label: "Supporting document", Type: "FILE",
				HelpText: "Form 19, Form 10C, bank passbook or any related document."},
		},
	},
	{
		Key: "ESI_QUERY", Name: "ESI Query", Prefix: "ESI", Color: "#00897B",
		Description:    "ESIC card, dispensary, claim and dependant queries.",
		RequiresFields: []string{"esic_number"},
		Fields: []demoField{
			{Key: "query_type", Label: "Type of ESI query", Type: "SELECT", Required: true,
				Options: []string{"ESIC card", "Dispensary change", "Claim status", "Dependant addition", "Other"}},
			{Key: "dispensary_name", Label: "Dispensary", Type: "TEXT"},
			{Key: "treatment_date", Label: "Date of treatment", Type: "DATE"},
			{Key: "dependant_name", Label: "Dependant name", Type: "TEXT",
				HelpText: "Required when adding a dependant."},
			{Key: "supporting_document", Label: "Supporting document", Type: "FILE"},
		},
	},
	{
		Key: "PAYROLL", Name: "Payroll & Salary", Prefix: "PAY", Color: "#F9AB00",
		Description: "Salary, payslip, reimbursement and tax deduction queries.",
		Fields: []demoField{
			{Key: "month", Label: "Salary month", Type: "DATE", Required: true},
			{Key: "issue_type", Label: "Issue", Type: "SELECT", Required: true,
				Options: []string{"Payslip not received", "Incorrect amount", "Reimbursement pending", "TDS query", "Other"}},
			{Key: "expected_amount", Label: "Expected amount", Type: "CURRENCY"},
		},
	},
	{
		Key: "IT_SUPPORT", Name: "IT Support", Prefix: "IT", Color: "#5F6368",
		Description: "Hardware, software, access and connectivity issues.",
		Fields: []demoField{
			{Key: "issue_type", Label: "Issue type", Type: "SELECT", Required: true,
				Options: []string{"Hardware", "Software", "Network", "Access request", "Email", "Other"}},
			{Key: "asset_tag", Label: "Asset tag", Type: "TEXT"},
			{Key: "urgency", Label: "Business impact", Type: "RADIO", Required: true,
				Options: []string{"Cannot work at all", "Working with difficulty", "Minor inconvenience"}},
		},
	},
	{
		Key: "HR_QUERY", Name: "HR Query", Prefix: "HR", Color: "#9334E6",
		Description: "Leave, attendance, policy and documentation queries.",
		Fields: []demoField{
			{Key: "query_type", Label: "Query type", Type: "SELECT", Required: true,
				Options: []string{"Leave", "Attendance", "Policy", "Letter request", "Grievance", "Other"}},
			{Key: "letter_type", Label: "Letter required", Type: "SELECT",
				Options: []string{"Employment letter", "Experience letter", "Salary certificate", "Address proof"}},
		},
	},
	{
		Key: "GENERAL", Name: "General Query", Prefix: "GEN", Color: "#3C4043",
		Description: "Anything that does not fit another category.",
		Fields: []demoField{
			{Key: "department_concerned", Label: "Which team should see this?", Type: "TEXT"},
		},
	},
}

// entityTemplate is a default entity applied when a client is onboarded. Each
// client keeps the ones that apply and opts out of the rest, which is why every
// client ends up with a different entity set.
type entityTemplate struct {
	Key               string
	Name              string
	Description       string
	EntityType        string
	DefaultCategories []string
	SortOrder         int
}

// entityTemplates are deliberately generic: they describe the shapes a client's
// establishments usually take, not any one client's legal names. An operator
// renames them and fills in the real PF/ESIC codes during onboarding.
var entityTemplates = []entityTemplate{
	{
		Key: "HO", Name: "Head Office", EntityType: "HEAD_OFFICE", SortOrder: 10,
		Description:       "The registered head office. Usually holds the primary PF and ESIC registration.",
		DefaultCategories: []string{"PF_QUERY", "ESI_QUERY", "PAYROLL", "HR_QUERY", "IT_SUPPORT", "GENERAL"},
	},
	{
		Key: "MFG", Name: "Manufacturing Unit", EntityType: "PLANT", SortOrder: 20,
		Description:       "A factory or plant. Normally registered for both PF and ESI.",
		DefaultCategories: []string{"PF_QUERY", "ESI_QUERY", "PAYROLL"},
	},
	{
		Key: "SVC", Name: "Services Division", EntityType: "DIVISION", SortOrder: 30,
		Description:       "A services or professional arm. Often PF-registered but above the ESI wage ceiling.",
		DefaultCategories: []string{"PF_QUERY", "PAYROLL", "HR_QUERY"},
	},
	{
		Key: "LOG", Name: "Logistics & Warehousing", EntityType: "BRANCH", SortOrder: 40,
		Description:       "Warehouse and transport operations, typically ESI-heavy.",
		DefaultCategories: []string{"ESI_QUERY", "PAYROLL"},
	},
	{
		Key: "CORP", Name: "Corporate Services", EntityType: "DIVISION", SortOrder: 50,
		Description:       "Shared corporate functions with no separate statutory registration.",
		DefaultCategories: []string{"HR_QUERY", "IT_SUPPORT", "GENERAL"},
	},
}
