// Command cli provides the operational tasks: migrations, seeding, creating the
// first super admin, and the maintenance jobs described in RUNBOOK.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/karmamgmt/complydesk/internal/cli"
	"github.com/karmamgmt/complydesk/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	command := os.Args[1]
	args := os.Args[2:]

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()

	var runErr error
	switch command {
	case "migrate":
		runErr = cli.Migrate(ctx, cfg, args)
	case "createdb":
		runErr = cli.CreateDatabase(ctx, cfg)
	case "seed":
		runErr = cli.Seed(ctx, cfg, args)
	case "reset":
		runErr = cli.ResetCommand(ctx, args, cfg)
	case "create-superadmin":
		runErr = cli.CreateSuperAdmin(ctx, cfg, args)
	case "create-tenant":
		runErr = cli.CreateTenant(ctx, cfg, args)
	case "purge-retention":
		runErr = cli.PurgeRetention(ctx, cfg)
	case "verify-audit-chain":
		runErr = cli.VerifyAuditChain(ctx, cfg)
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", command)
		usage()
		os.Exit(1)
	}

	if runErr != nil {
		if errors.Is(runErr, cli.ErrUsage) {
			usage()
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", runErr)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, strings.TrimSpace(`
ComplyDesk operations CLI

Usage: complydesk <command> [options]

Commands:
  createdb                       Create the configured database if it is absent
  migrate up|down|version|force  Apply, roll back or inspect schema migrations
                                   migrate up [n]      apply all, or n steps
                                   migrate down [n]    roll back n steps (default 1)
                                   migrate force <v>   clear a dirty state at version v
  seed [--demo] [--showcase]     Seed permissions, system roles and notification
       [--sample]                  events. --demo adds a demo workspace,
                                   --showcase fills it with tickets, and
                                   --sample installs the full dataset: several
                                   live clients, the standard departments and
                                   entity catalogue, an FAQ and Help requests
  reset [--yes] [--people]       Clear accumulated data — tickets, documents,
        [--empty-clients]          notifications, help requests and sites —
                                   while keeping departments, entities, the
                                   query taxonomy, roles and clients. Prints
                                   what it would do and stops unless --yes
  create-superadmin              Create a platform super administrator
                                   --email --name [--password]
  create-tenant                  Create a workspace
                                   --slug --name [--admin-email --admin-name]
  purge-retention                Delete data past its retention window
  verify-audit-chain             Verify the audit log hash chain

Configuration is read from .env and the environment. See .env.example.
`)+"\n")
}
