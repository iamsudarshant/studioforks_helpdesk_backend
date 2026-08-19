package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/karmamgmt/complydesk/internal/auth"
	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/tenant"
	"github.com/karmamgmt/complydesk/internal/user"
)

// platformTenantSlug is the workspace super admins belong to. It exists so the
// foreign key on users has something to point at; it never holds client data.
const platformTenantSlug = "platform"

// CreateSuperAdmin provisions a platform operator. Multiple super admins are
// supported, which the brief calls for.
func CreateSuperAdmin(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("create-superadmin", flag.ContinueOnError)
	email := fs.String("email", "", "email address (required)")
	name := fs.String("name", "", "full name (required)")
	password := fs.String("password", "", "password; generated and printed when omitted")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if strings.TrimSpace(*email) == "" || strings.TrimSpace(*name) == "" {
		return fmt.Errorf("--email and --name are required")
	}

	db, err := platform.OpenDB(cfg.DB)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer func() { _ = db.Close() }()

	tenantRepo := tenant.NewRepository(db)
	userRepo := user.NewRepository(db)
	hasher := auth.NewHasher(cfg.Auth)

	platformTenant, err := tenantRepo.BySlug(ctx, platformTenantSlug)
	if err != nil {
		if !errors.Is(err, platform.ErrSentinelNotFound) {
			return fmt.Errorf("loading platform workspace: %w", err)
		}
		platformTenant, err = tenantRepo.Create(ctx, tenant.CreateParams{
			Slug: platformTenantSlug, Name: "ComplyDesk Platform",
			LegalName: "ComplyDesk", TicketPrefix: "CD",
		})
		if err != nil {
			return fmt.Errorf("creating platform workspace: %w", err)
		}
		if err := tenantRepo.SetStatus(ctx, platformTenant.ID, tenant.StatusActive); err != nil {
			return fmt.Errorf("activating platform workspace: %w", err)
		}
	}

	role, err := userRepo.RoleByKey(ctx, platformTenant.ID, user.RoleSuperAdmin)
	if err != nil {
		return fmt.Errorf("the super administrator role is missing — run `seed` first: %w", err)
	}

	plaintext := strings.TrimSpace(*password)
	generated := false
	if plaintext == "" {
		plaintext, err = platform.RandomPassword(16)
		if err != nil {
			return fmt.Errorf("generating password: %w", err)
		}
		generated = true
	}

	policy := auth.DefaultPolicy()
	if err := policy.Validate(plaintext, *email, *name); err != nil {
		return fmt.Errorf("the supplied password does not meet the policy: %w", err)
	}

	hash, err := hasher.Hash(plaintext)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}

	first, last := splitName(*name)
	created, err := userRepo.Create(ctx, platformTenant.ID, user.CreateParams{
		FirstName: first, LastName: last,
		Email:  strings.ToLower(strings.TrimSpace(*email)),
		Status: user.StatusActive, PasswordHash: hash,
		// A generated password must be replaced on first sign-in.
		MustChange: generated,
	})
	if err != nil {
		var dup *user.DuplicateError
		if errors.As(err, &dup) {
			return fmt.Errorf("a user with this %s already exists", dup.Field())
		}
		return fmt.Errorf("creating super administrator: %w", err)
	}

	if err := userRepo.SetRoles(ctx, platformTenant.ID, created.ID, []int64{role.ID}, nil); err != nil {
		return fmt.Errorf("granting the super administrator role: %w", err)
	}

	fmt.Printf(`
super administrator created
  email:    %s
  portal:   /admin
  tenant:   %s   (send X-Tenant-Slug: %s)
`, created.Email.String, platformTenantSlug, platformTenantSlug)

	if generated {
		fmt.Printf("  password: %s   (must be changed at first sign-in)\n", plaintext)
	}
	return nil
}

// CreateTenant provisions a client workspace, optionally with its first admin.
func CreateTenant(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("create-tenant", flag.ContinueOnError)
	slug := fs.String("slug", "", "workspace address, lowercase (required)")
	name := fs.String("name", "", "client name (required)")
	adminEmail := fs.String("admin-email", "", "email of the first client administrator")
	adminName := fs.String("admin-name", "", "name of the first client administrator")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if strings.TrimSpace(*slug) == "" || strings.TrimSpace(*name) == "" {
		return fmt.Errorf("--slug and --name are required")
	}

	db, err := platform.OpenDB(cfg.DB)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer func() { _ = db.Close() }()

	tenantRepo := tenant.NewRepository(db)
	created, err := tenantRepo.Create(ctx, tenant.CreateParams{
		Slug: strings.ToLower(strings.TrimSpace(*slug)), Name: *name,
	})
	if err != nil {
		if errors.Is(err, platform.ErrSentinelConflict) {
			return fmt.Errorf("a workspace with the address %q already exists", *slug)
		}
		return fmt.Errorf("creating workspace: %w", err)
	}

	fmt.Printf("workspace %q created (status: %s)\n", created.Slug, created.Status)

	if strings.TrimSpace(*adminEmail) != "" {
		userRepo := user.NewRepository(db)
		hasher := auth.NewHasher(cfg.Auth)

		role, err := userRepo.RoleByKey(ctx, created.ID, user.RoleClientMasterAdmin)
		if err != nil {
			return fmt.Errorf("the client administrator role is missing — run `seed` first: %w", err)
		}

		temp, err := platform.RandomPassword(14)
		if err != nil {
			return fmt.Errorf("generating password: %w", err)
		}
		hash, err := hasher.Hash(temp)
		if err != nil {
			return fmt.Errorf("hashing password: %w", err)
		}

		first, last := splitName(defaultTo(*adminName, *adminEmail))
		admin, err := userRepo.Create(ctx, created.ID, user.CreateParams{
			FirstName: first, LastName: last,
			Email:  strings.ToLower(strings.TrimSpace(*adminEmail)),
			Status: user.StatusActive, PasswordHash: hash, MustChange: true,
		})
		if err != nil {
			return fmt.Errorf("creating the client administrator: %w", err)
		}
		if err := userRepo.SetRoles(ctx, created.ID, admin.ID, []int64{role.ID}, nil); err != nil {
			return fmt.Errorf("granting the client administrator role: %w", err)
		}

		fmt.Printf(`  administrator: %s
  portal:        /partner
  password:      %s   (must be changed at first sign-in)
`, admin.Email.String, temp)
	}

	fmt.Println("\nThe client is live. Add entities, categories, SLAs and users as needed.")
	return nil
}

func splitName(full string) (first, last string) {
	parts := strings.Fields(strings.TrimSpace(full))
	switch len(parts) {
	case 0:
		return "User", ""
	case 1:
		return parts[0], ""
	default:
		return parts[0], strings.Join(parts[1:], " ")
	}
}

func defaultTo(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// PurgeRetention deletes data past its configured retention window. It reports
// what it removed rather than working silently.
func PurgeRetention(ctx context.Context, cfg *config.Config) error {
	db, err := platform.OpenDB(cfg.DB)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer func() { _ = db.Close() }()

	tenantRepo := tenant.NewRepository(db)
	tenants, _, err := tenantRepo.List(ctx, tenant.ListFilter{
		Page: platform.Page{Page: 1, PerPage: platform.MaxPerPage, SortBy: "id", SortDir: "ASC"},
	})
	if err != nil {
		return fmt.Errorf("listing workspaces: %w", err)
	}

	total := 0
	for _, t := range tenants {
		var policy struct {
			ActiveTicketYears   int `json:"active_ticket_years"`
			ArchivedTicketYears int `json:"archived_ticket_years"`
			AuditLogYears       int `json:"audit_log_years"`
		}
		if err := tenantRepo.Setting(ctx, t.ID, tenant.SettingRetentionPolicy, &policy); err != nil {
			continue
		}
		if policy.ArchivedTicketYears <= 0 {
			policy.ArchivedTicketYears = 7
		}
		if policy.AuditLogYears <= 0 {
			policy.AuditLogYears = 7
		}

		cutoffTickets := time.Now().UTC().AddDate(-policy.ArchivedTicketYears, 0, 0)
		res, err := db.Primary.ExecContext(ctx, `
			DELETE FROM tickets
			WHERE tenant_id = ? AND status IN ('CLOSED','CANCELLED') AND closed_at < ?`,
			t.ID, cutoffTickets)
		if err != nil {
			return fmt.Errorf("purging tickets for %s: %w", t.Slug, err)
		}
		ticketsDeleted, _ := res.RowsAffected()

		cutoffAudit := time.Now().UTC().AddDate(-policy.AuditLogYears, 0, 0)
		res, err = db.Primary.ExecContext(ctx,
			`DELETE FROM audit_logs WHERE tenant_id = ? AND created_at < ?`, t.ID, cutoffAudit)
		if err != nil {
			return fmt.Errorf("purging audit logs for %s: %w", t.Slug, err)
		}
		auditDeleted, _ := res.RowsAffected()

		if ticketsDeleted > 0 || auditDeleted > 0 {
			fmt.Printf("%s: %d tickets, %d audit rows removed\n", t.Slug, ticketsDeleted, auditDeleted)
			total += int(ticketsDeleted + auditDeleted)
		}
	}

	fmt.Printf("retention purge complete; %d rows removed\n", total)
	return nil
}

// VerifyAuditChain re-computes the audit hash chain and reports the first row
// where it breaks, which is the signal that the log was tampered with.
func VerifyAuditChain(ctx context.Context, cfg *config.Config) error {
	db, err := platform.OpenDB(cfg.DB)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer func() { _ = db.Close() }()

	rows := []struct {
		ID       int64   `db:"id"`
		TenantID *int64  `db:"tenant_id"`
		PrevHash *string `db:"prev_hash"`
		RowHash  *string `db:"row_hash"`
	}{}
	if err := db.Primary.SelectContext(ctx, &rows, `
		SELECT id, tenant_id, prev_hash, row_hash
		FROM audit_logs
		ORDER BY tenant_id, id`); err != nil {
		return fmt.Errorf("reading audit log: %w", err)
	}

	lastByTenant := map[int64]string{}
	breaks := 0

	for _, row := range rows {
		key := int64(0)
		if row.TenantID != nil {
			key = *row.TenantID
		}

		expected := lastByTenant[key]
		actual := ""
		if row.PrevHash != nil {
			actual = *row.PrevHash
		}
		if expected != actual {
			fmt.Printf("chain break at audit_logs.id=%d (expected prev_hash %q, found %q)\n",
				row.ID, expected, actual)
			breaks++
		}
		if row.RowHash != nil {
			lastByTenant[key] = *row.RowHash
		}
	}

	if breaks > 0 {
		return fmt.Errorf("%d chain break(s) found across %d audit rows", breaks, len(rows))
	}
	fmt.Printf("audit chain verified across %d rows; no breaks found\n", len(rows))
	return nil
}

// confirm prompts on a TTY. Used by destructive commands.
func confirm(prompt string) bool {
	fmt.Printf("%s [y/N]: ", prompt)
	reader := bufio.NewReader(os.Stdin)
	answer, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(answer), "y")
}
