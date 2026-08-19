# ComplyDesk — test logins

Every credential below was signed in against a running API before this file was
written. If one stops working, the cause is almost always that the database was
re-seeded or migrated — see [Troubleshooting](#troubleshooting).

**Password for every account:** `ComplyDesk@2026`

Install or repair the dataset with:

```bash
go run ./cmd/cli migrate up
go run ./cmd/cli seed --demo                    # platform catalogue + the workspace
go run ./cmd/cli seed --purge-dummy --ampersand # remove invented data, install the real set
```

Both seed passes are idempotent, so they can be re-run at any time. `--purge-dummy`
is destructive by design: it deletes the invented sample clients and the older
generated accounts. See [What `--purge-dummy` removes](#what---purge-dummy-removes).

---

## 1. Where to sign in

The URL decides which workspace you authenticate against. Getting this wrong is
the single most common cause of "these credentials aren't valid for this portal".

| Portal | URL | Who |
|---|---|---|
| Admin | `http://localhost:5173/admin` | Super Admin |
| Agents | `http://localhost:5173/agents` | Helpdesk Head, Helpdesk Executive |
| Partner | `http://localhost:5173/AMP/partners` | Client Admin, Client Executive (partners) |
| Employee | `http://localhost:5173/AMP/user` | Employees and ex-employees |

ComplyDesk's own staff sign in at the root because they belong to the platform,
not to any client. A client's people sign in under that client's **code**, so
the workspace is visible in the URL and a shared link keeps its context.

There is **one** client in the dataset:

| Client | Code | Slug |
|---|---|---|
| Ampersand Group | `AMP` | `demo` |

The code and the slug both resolve to the same workspace, so `AMP` and `demo`
are interchangeable in a URL or an `X-Tenant-Slug` header.

---

## 2. The accounts

### ComplyDesk staff — sign in at `/admin` or `/agents`

| Email | Name | Role | Covers |
|---|---|---|---|
| `superadmin@complydesk.local` | Admin Nair | Super Admin | Everything, every client |
| `pf.agent@complydesk.local` | Priya Nair | Helpdesk Executive | **PF & Compliance** |
| `esic.agent@complydesk.local` | Karthik Menon | Helpdesk Executive | **ESIC & Insurance** |

Each agent is assigned to Ampersand Group and to one statutory department, so
their queues differ. Both hold `user.update`, which is what the employee
lifecycle and the enable/disable actions are gated on.

### Ampersand Group — sign in at `/AMP/partners`

| Email | Name | Role |
|---|---|---|
| `admin@ampersand.local` | Ampersand Admin | Client Admin |

The Client Admin has **oversight, not user administration**: `user.view.all` and
`user.view.pii`, but not `user.update`. That is a deliberate part of the RBAC
model — the employee master record is maintained by the helpdesk — so the
employment and enable/disable actions are performed by an agent or the super
admin. See [Permissions](#4-permissions-that-gate-the-workflows).

### Partners — 41 accounts, one per entity

Addressed by entity code, so the account for a given entity is findable without
a lookup table:

```
partner.<entity-code-lowercased-with-dots>@ampersand.local
```

| Example | Name | Allocated entity |
|---|---|---|
| `partner.pf.wdl@ampersand.local` | Harsh Sharma | PF Withdrawals |
| `partner.pf.clm@ampersand.local` | Ritu Nair | Claim Status |
| `partner.esi.mat@ampersand.local` | Karthik Sharma | Maternity Benefit |
| `partner.esi.card@ampersand.local` | Bhavna Chatterjee | ESIC Card |

Each partner is a **Client Executive** allocated to exactly one entity, and sees
only that entity's tickets. This is the segmentation to test tenant and scope
isolation with.

### Employees — 34 accounts

```
employee01@ampersand.local … employee34@ampersand.local
```

| Example | Name | Posted to |
|---|---|---|
| `employee01@ampersand.local` | Amit Pillai | Claim Status |
| `employee02@ampersand.local` | Lakshmi Bhatt | Correction Requests |

Every employee has raised **at least one ticket**, and each sees only their own.

---

## 3. The organisation structure

```
Ampersand Group (AMP)
├── PF & Compliance ......... 24 entities ... agent: pf.agent@complydesk.local
└── ESIC & Insurance ........ 17 entities ... agent: esic.agent@complydesk.local
```

**PF & Compliance** — Claim Status, Correction Requests, EDLI, Employer
Contributions, Exit Details, KYC Updates, Member Passbook, Pension, PF
Transfers, PF Withdrawals, Service History, UAN Issues, PF Advance / Loan, PF
Nomination / e-Nomination, Death Claim, Disability Claim, Form 19 / Final
Settlement, Form 10C / Pension Withdrawal, PF Balance Inquiry, International
Worker (IW) Compliance, PF Return / ECR Filing, Establishment Registration /
Code Update, Digital Life Certificate (for pensioners), Joint Declaration / Name
Correction.

**ESIC & Insurance** — ESIC Card, ESIC Claim Status, ESIC Dispensary, ESIC
Registration, ESIC Contribution / Challan, Temporary Disablement Benefit (TDB),
Permanent Disablement Benefit (PDB), Maternity Benefit, Sickness Benefit,
Dependent Benefit, Medical / Hospitalization Claim, Accident Report / Injury
Claim, Family / Dependent Registration, IP (Insured Person) Number Issues, ESIC
Return Filing, ESIC Contribution Period Mapping, Offline / Online Claim
Submission.

Tickets are **backdated across roughly three months**, so the dashboard date
filter returns a different figure for each preset rather than the same number
every time:

| Range | Tickets |
|---|---|
| Today | 5 |
| This week | 8 |
| This month | 20 |
| Last 30 days | 25 |
| Last 90 days / All time | 34 |

Twenty of them carry a supporting document, so the Attachments tab and the
download authorisation have something real to act on.

---

## 4. Permissions that gate the workflows

| Workflow | Permission | Held by |
|---|---|---|
| Enable / disable an account | `user.update` | Super Admin, Helpdesk Head, Helpdesk Executive |
| Active ⇄ Ex-Employee | `user.update` | Super Admin, Helpdesk Head, Helpdesk Executive |
| Create an employee | `user.create` | Super Admin, Helpdesk Head, Helpdesk Executive |
| Raise a ticket | `ticket.create` | Everyone above, plus Client Admin, Client Executive, Employee |
| See every ticket in the client | `ticket.view.all` | Staff, Client Admin |
| See only an allocated entity | `ticket.view.scope` | Client Executive (partners) |
| See only your own | `ticket.view.own` | Employee |

On top of the permission, an actor may only administer accounts **below their own
rank**, so a helpdesk executive cannot reset the super admin's password. That
check is `CanAdminister`, and the API reports its answer per row as
`can_administer`, which is what the row menu renders from.

---

## 5. What `--purge-dummy` removes

Narrow and named, never inferred — deleting rows because they look invented is
how real data gets destroyed.

- The invented client workspaces `zenith` and `orbit`, and everything filed
  under them (foreign keys cascade from `tenants`).
- Accounts matching `%@demo.local`, `%@amp.com`, and the two generated agents
  `agent.arjun@` / `agent.priya@complydesk.local`, together with the tickets they
  raised. `tickets.requester_id` is `RESTRICT`, so both go in one transaction.
- Documents that nothing references any more — neither a ticket attachment nor a
  surviving owning record. Avatars and brand logos are excluded, because those
  are referenced by public id from `users.avatar_path` and
  `tenant_branding.logo_path` rather than by a foreign key.
- Ampersand tickets that nobody has worked: no reply, no resolution. Anything
  with a conversation on it is left alone, because a demo database is still
  somebody's afternoon of testing.

`superadmin@complydesk.local` is deliberately **never** removed — it is the
platform administrator the system needs to remain administrable.

---

## Troubleshooting

**"The credentials you entered are not valid for this portal."**
Almost always the wrong URL. A client's people sign in under `/AMP/…`; staff
sign in at the root. It is also what a **disabled** account is told — the API
does not distinguish, on purpose, so an attacker cannot enumerate which accounts
exist.

**A partner sees no tickets.**
Their allocation is missing its scope mirror. `entity_assignments` is only half
the record; `user_scopes` is what every entity filter actually reads. Re-run
`seed --ampersand`, which writes both through the repository rather than by
direct insert.

**The dashboard shows the same number for every date range.**
The API is running an old binary. The range is applied server-side, so restart
the API after pulling.

**Nothing loads and the screen says "ComplyDesk isn't loading right now".**
The Go API is not running, or not reachable on `:8090`. Start it with
`go run ./cmd/api`, or use `run-dev.ps1`, which starts both halves.
