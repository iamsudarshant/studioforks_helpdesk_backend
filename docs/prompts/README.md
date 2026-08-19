## Decisions taken on the BRD's open points

These are flagged inside the prompts and should be revisited if you disagree:

1. **Multi-tenancy** — shared schema with a mandatory `tenant_id` and an enforced data-layer choke point, *not* schema-per-tenant. Same isolation guarantee, one migration path, cross-tenant reporting stays possible.
2. **Portal ↔ role mapping** — the BRD put `Client Master Admin` in `/agents`; it is a client-side role, so it moved to `/partner`. `/admin` = platform operators, `/agents` = our helpdesk staff, `/partner` = all client admin roles, `/user` = employees.
3. **Bulk-onboarding temp password** — `PFNumber@BirthYear` is guessable by any colleague, so it is not the default. Default is a random 12-char temp password + activation link; the derived pattern remains available as an opt-in tenant setting for employees without email, plus a third `CSV_RETURN` mode returning a one-time expiring credentials file.
4. **Authorisation** — permission-string based, not role-name based, so tenants can edit roles from the admin panel without code changes.
5. **State machine and action availability are server-driven** — the API returns `allowed_transitions` and `permissions` on each ticket; the UI never reimplements the workflow.
