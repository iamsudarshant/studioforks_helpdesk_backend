# ComplyDesk API — the ticket lifecycle

Base path `/api/v1`. Every route below is live in the Go API.

## Conventions

**Headers**

| Header | Required | Notes |
|---|---|---|
| `Authorization: Bearer <jwt>` | yes | except `/auth/*` and `/public/*` |
| `X-Tenant-Slug` | yes | the workspace **slug or client code** — `demo` and `AMP` both resolve |
| `X-Portal` | yes | `admin` \| `agents` \| `partner` \| `user`; must match the token |
| `Idempotency-Key` | on POST/PUT | replays return the original response |

**Envelope** — one shape, always:

```json
{ "success": true, "data": {...}, "meta": {"page":1,"per_page":25,"total":142},
  "error": null, "request_id": "..." }
```

**Errors** — `error.code` is stable and machine-readable; `error.message` is safe
to show a user.

| Code | HTTP | Means |
|---|---|---|
| `VALIDATION_FAILED` | 422 | with a `details[]` of `{field, code, message}` |
| `UNAUTHENTICATED` | 401 | no or bad credentials |
| `TOKEN_EXPIRED` | 401 | refresh and retry |
| `FORBIDDEN` | 403 | authenticated, not permitted |
| `TENANT_MISMATCH` | 403 | wrong workspace for this session |
| `PORTAL_MISMATCH` | 403 | token not valid for this portal |
| `NOT_FOUND` | 404 | also used to hide surfaces a caller may not know exist |
| `INVALID_STATUS_TRANSITION` | 409 | not legal in this category's workflow |
| `REOPEN_WINDOW_EXPIRED` | 409 | past the client's configured window |
| `RATE_LIMITED` | 429 | `Retry-After` is set |

Unknown request fields are **rejected**, not ignored: a typo in a field name
fails loudly instead of silently doing nothing.

---

## Creating a ticket

### `POST /tickets`

Requires `ticket.create`.

```json
{
  "category_id":    "01J8...",       // required, ULID
  "subcategory_id": "01J9...",       // optional
  "subject":        "PF withdrawal not credited",
  "description":    "Applied on 12 June, still pending.",
  "priority":       "MEDIUM",        // optional, defaults per category
  "requester_id":   "01JA...",       // optional — raise on behalf of an employee
  "department_id":  "01JB...",       // the statutory line — see below
  "entity_id":      "01JC...",       // the establishment inside that line
  "site_id":        "01JD...",
  "custom_fields":  { "uan": "100987654321" },
  "document_ids":   ["01JD..."]      // pre-uploaded attachments
}
```

`requester_id` is what lets a Client Executive or Helpdesk user raise a ticket
**on behalf of** an employee. It is permitted only when the caller can already
see that user, so it cannot be used to reach across a scope. The ticket belongs
to the employee; `created_by` records who actually raised it.

#### Client → Department → Entity

The form asks two questions: **which department**, then **which of its
entities**. The query domain is resolved from the department rather than asked
for — `GET /categories?top_level=true&department_id=…` narrowed to one statutory
line returns that line's domain, so putting it on screen asked the requester to
re-answer the question they had just answered. Migration `000022` populated
`categories.default_department_id` by matching a category's module key to a
department's type, which is what makes that filter possible: a client running PF
and ESIC is no longer offered the Payroll, IT, HR and General categories the
platform catalogue also ships.

The three narrow each other and the relationship is **verified server-side**, not
only in the form:

- `department_id` must belong to the client the ticket is being raised for.
- `entity_id` must belong to that department. A mismatch is refused with
  `VALIDATION_FAILED` / `entity_id` / `INVALID`.
- Sending only `entity_id` is still valid: the department is derived from the
  entity, so an integration that knows one does not have to send both.

A cascading form makes the wrong combination hard to pick; it does not make it
impossible to send, and an entity filed under another client's department would
route the ticket to a desk that cannot see it.

#### Attachments arrive from two places

`document_ids` is the dropzone at the foot of the form. A category can also
declare a **FILE** custom field — "Supporting document" ships with every PF and
ESI category — whose value in `custom_fields` is a document `public_id`, or a
list of them.

Both are attachments and both are linked into `ticket_attachments`. Only the
first used to be, so a file sent through the form field was stored, charged
against quota, and then unreachable: a document is authorised *through the
tickets it is attached to*, so an unlinked one is authorised against an empty
set. The ticket showed a 26-character identifier where a filename should have
been. See `internal/ticket/repository_form_files.go`, and migration `000025`
for the tickets raised before the fix.

Every reference is resolved against the ticket's own tenant, so an id copied
from another client resolves to nothing rather than to somebody else's file.
`PATCH /tickets/{id}` links newly added FILE-field documents the same way; the
`(ticket_id, document_id)` unique key makes re-saving the form a no-op.

**201** returns the full ticket, including its number:

```json
{ "id": "01JE...", "ticket_number": "INF-PF-2026-000145", "status": "NEW", ... }
```

### Ticket numbers

```
{CLIENT_CODE}-{CATEGORY_PREFIX}-{YEAR}-{SEQUENCE}
INF-PF-2026-000145
```

Allocated inside the creating transaction under `SELECT ... FOR UPDATE` on a
`(tenant, prefix, year)` counter row, so concurrent creates cannot duplicate a
number or leave a gap. See `nextTicketNumber` in `internal/ticket/repository.go`.

---

## Reading

| Route | Permission | Notes |
|---|---|---|
| `GET /tickets` | `ticket.view.*` | filter, sort, paginate — scoped to the caller |
| `GET /tickets/counts` | `ticket.view.*` | `{by_status, quick}` for tabs and chips |
| `GET /tickets/{id}` | `ticket.view.*` | includes `allowed_transitions` and `permissions` |
| `GET /tickets/{id}/timeline` | `ticket.view.*` | the immutable log |
| `GET /tickets/{id}/conversations` | `ticket.view.*` | internal notes filtered in SQL |
| `GET /tickets/{id}/attachments` | `ticket.view.*` | name, size, uploader, date |
| `GET /tickets/{id}/watchers` | `ticket.view.*` | who is following it |
| `GET /tickets/{id}/watcher-candidates` | `ticket.view.*` | who may be added |
| `GET /tickets/assignable` | `ticket.view.*` | who this ticket may be handed to |
| `GET /tickets/{id}/transfer-agents` | `ticket.view.*` | who, in a named department, may take it |

`GET /tickets` accepts `q`, `status[]`, `priority[]`, `category_id[]`,
`entity_id[]`, `assignee_id[]`, `unassigned`, `breached`, `reopened`,
`created_from`, `created_to`, `page`, `per_page`, `sort_by`, `sort_dir`.
`q` searches ticket number, subject, requester name, employee code, PF, UAN and
ESIC number.

The detail response carries what the UI needs to render controls without
guessing:

```json
{
  "allowed_transitions": [
    { "to_status": "IN_PROGRESS", "label": "Start working",
      "requires_comment": false, "requires_reason_code": false }
  ],
  "permissions": { "can_reply": true, "can_assign": true, "can_reopen": false }
}
```

`allowed_transitions` comes from `category_workflows` filtered by the caller's
role, so the client never hardcodes the lifecycle.

The detail read also carries two blocks the list does not, both `omitempty` so a
row never claims there are none:

```json
{
  "attachments": [
    { "id": 50, "document_id": "01JD…", "file_name": "form13.pdf",
      "mime_type": "application/pdf", "size_bytes": 18433 }
  ],
  "watchers": [
    { "id": "01JA…", "name": "Ampersand Admin",
      "email": "admin@ampersand.local", "reason": "Client oversight" }
  ]
}
```

`attachments` is the files that arrived with the ticket itself — those with no
`conversation_id` — so the opening post can show what was sent with it rather
than sending the reader to another tab.

---

## Lifecycle actions

```
NEW ──assign/accept──> PENDING_HELPDESK <──> PENDING_EMPLOYEE
                              │                     │
                              └────────┬────────────┘
                                       ▼
                                    CLOSED ──> REOPENED ──┐
                                       ▲                  │
                                       └──────────────────┘

NEW / PENDING_HELPDESK ──withdraw──> CANCELLED
```

Five states, since migration `000024`. `NEW` means nobody has looked at it, and
the interval before it leaves is **first-response time**, which is what the SLA
is judged on. `PENDING_EMPLOYEE` pauses the resolution clock; `PENDING_HELPDESK`
does not — that delay is exactly what an SLA exists to measure. `CANCELLED` is
not a stage of the work but the absence of it, reached by an explicit withdrawal
rather than chosen from a status list; the thread, the timeline and the
attachments all stay exactly where they are.

Escalation is **not** in this diagram on purpose: it is a flag beside the
status, carried by `escalation_level` and reported as `is_escalated`.

| Route | Permission | Body |
|---|---|---|
| `POST /tickets/{id}/status` | `ticket.status.change` | `{to_status, comment?, reason_code?}` |
| `POST /tickets/{id}/assign` | `ticket.assign` | `{assignee_id, comment?}` — the assignee must work the ticket's department |
| `POST /tickets/{id}/transfer` | `ticket.transfer` | `{department_id?, assignee_id?, reason}` |
| `POST /tickets/{id}/escalate` | `ticket.escalate` | `{reason, notify_ids?}` |
| `POST /tickets/{id}/close` | `ticket.close` | `{comment?}` |
| `POST /tickets/{id}/reopen` | `ticket.reopen` | `{reason_code, comment}` |
| `POST /tickets/{id}/feedback` | `ticket.feedback` | `{score, comment?}` |
| `PATCH /tickets/{id}` | `ticket.update` | `{subject?, priority?, custom_fields?}` |

Every transition is validated against `category_workflows`. An illegal move
returns `INVALID_STATUS_TRANSITION` with a message naming both states — it never
half-applies. `requires_comment` and `requires_reason_code` are enforced
server-side, not only in the dialog.

Assigning a `NEW` ticket moves it to `OPEN` and stamps `first_responded_at`, so
first response is recorded when work actually starts.

---

## Conversation and attachments

### `POST /tickets/{id}/conversations`

Requires `ticket.reply.public` or `ticket.reply.internal`.

```json
{ "body_html": "<p>Your Form 13 has been submitted.</p>",
  "visibility": "PUBLIC",
  "document_ids": ["01JD..."],
  "mentions": ["01JA..."] }
```

`visibility: "INTERNAL"` requires `ticket.reply.internal`. Internal notes are
excluded **in the SQL query** for viewers without it, not filtered in the
handler and certainly not in the browser — so an employee's response cannot
contain them even in a payload they never render.

`body_html` is sanitised with bluemonday on the way in and again on render.

### `POST /tickets/{id}/attachments`

Requires `document.upload`. `multipart/form-data`.

Validated on: extension, declared MIME, **sniffed** content type, and size.
Files are encrypted at rest with AES-256-GCM using a per-file key wrapped by the
master KEK. There is a virus-scan hook before the file is accepted.

Allowed formats and size limits are per-client configuration, not constants.

---

## The immutable timeline

### `GET /tickets/{id}/timeline`

```json
[
  { "id": "01JF…", "event_type": "STATUS_CHANGED",
    "actor": "Priya Nair", "actor_role": "HELPDESK_EXECUTIVE",
    "visibility": "PUBLIC",
    "summary": "Status changed from pending helpdesk to pending employee",
    "detail": { "from": "PENDING_HELPDESK", "to": "PENDING_EMPLOYEE",
                "comment": "Waiting for the passbook" },
    "created_at": "2026-08-19T05:30:00Z" },

  { "id": "01JG…", "event_type": "TRANSFERRED",
    "actor": "Admin Nair", "visibility": "PUBLIC",
    "summary": "Ticket transferred",
    "detail": { "type": "TRANSFER", "reason": "ESIC handles this one",
                "from_assignee": "Priya Nair",  "to_assignee": "Karthik Menon",
                "from_department": "PF & Compliance",
                "to_department": "ESIC & Insurance" },
    "created_at": "2026-08-19T05:00:00Z" }
]
```

`actor` is the **name**, snapshotted at write time and falling back to the user
row when a writer did not stamp one — the create path did not, so every ticket
used to open with an unattributed "Ticket raised".

An assignment event names the people and departments involved, and **only the
ones that actually changed**: recording the department a plain assign started in
reads as a transfer that never happened, and an escalation changes neither owner
nor line.

`visibility` is on the wire because the reader renders an internal entry
differently from a public one. It was filtered on and then dropped from the
response.

#### What the client is expected to do with this

The payload is the audit row; it is not what a person should be shown. Raw
status codes and reason codes are internal vocabulary, and the employee whose
ticket it is has never seen either. The SPA renders each entry as a titled event
with the change stated in the client's own status language:

> **Ticket reopened**
> Status changed from **Closed** to **Reopen**
> **Reason:** Not resolved
> **Comment:** Member says the entry is still missing
> Updated by Priya Nair · 19 Aug 2026, 11:25

The full technical record — before/after, the raw payload, IP, user agent,
portal and request id — stays in `audit_logs`, which is a different table
answering a different question. See
[WORKFLOWS.md §15](WORKFLOWS.md#15-activity-timeline).

Append-only. There is no update or delete route, and none in the repository —
entries are written by `writeTimeline` inside the same transaction as the change
they describe, so the log cannot disagree with the ticket.

Event types: `CREATED`, `STATUS_CHANGED`, `ASSIGNED`, `TRANSFERRED`,
`ESCALATED`, `REPLIED`, `INTERNAL_NOTE`, `ATTACHMENT_ADDED`,
`ATTACHMENT_DOWNLOADED`, `INFO_REQUESTED`, `SLA_WARNING`, `SLA_BREACHED`,
`REOPENED`, `CLOSED`, `FEEDBACK_GIVEN`, `FIELD_UPDATED`.

The actor's name is **snapshotted** on write. If someone is renamed or deleted,
the history still says who did it at the time.

---

## The user lifecycle

Two different questions, deliberately two different endpoints. Collapsing them
into one switch is what made "Activate" on an ex-employee silently erase the
fact that they had left.

| Question | Endpoint | Meaning |
|---|---|---|
| May this account be used at all? | `POST /users/{id}/activate` / `deactivate` | `ACTIVE` ⇄ `INACTIVE` |
| Does this person still work here? | `POST /users/{id}/employment-status` | `ACTIVE` ⇄ `EX_EMPLOYEE` |

### `POST /users/{id}/deactivate` — disable an account

Requires `user.update`, and the caller must outrank the target.

Applies to **agents, partners and employees** alike. The account is set to
`INACTIVE` and every live session is revoked, so the user stops working
immediately rather than when their access token expires. Their tickets,
documents and history are untouched — nothing is deleted and the account can be
enabled again at any time.

A disabled user attempting to sign in receives the ordinary
"credentials are not valid for this portal" message. The API does not
distinguish a disabled account from a wrong password on purpose: the difference
would let an attacker enumerate which accounts exist.

Both directions are written to the audit log as `user.activated` /
`user.deactivated`.

### `POST /users/{id}/employment-status` — Active ⇄ Ex-Employee

Requires `user.update`, and the caller must outrank the target.

```json
{
  "status":           "EX_EMPLOYEE",   // or "ACTIVE"
  "last_working_day": "2026-08-15",    // required leaving, ignored returning
  "agent_id":         "01JB...",       // required in BOTH directions
  "client":           "AMP"         // when staff act with no client selected
}
```

Which fields are required depends on the direction:

| Direction | Last working day | Agent |
|---|---|---|
| Active → Ex-Employee | **required** | **required** |
| Ex-Employee → Active | not asked for; the stored date is **cleared** | **required** |

Leaving does four things in one transaction, because the parts are not
independently valid:

1. sets the status and the last working day,
2. moves the person into the read-only `EX_EMPLOYEES` group, which is what
   carries their access mode,
3. reassigns their **open** tickets to the chosen agent — closed and cancelled
   ones stay where they are, because reassigning finished work rewrites history
   for no benefit, and
4. revokes their sessions.

Returning clears the leaving date and moves them back to `ACTIVE_EMPLOYEES`.

**200**:

```json
{
  "message": "Employee marked as an ex-employee.",
  "status": "EX_EMPLOYEE",
  "last_working_day": "2026-08-15T00:00:00Z",
  "tickets_moved": 3,
  "handling_agent": { "id": "01JB...", "name": "Priya Nair" }
}
```

Audited as `user.employment_changed`, recording the status, the date, the agent
and how many tickets moved.

### `GET /users/assignable-agents?client={ref}`

Who an employee's queries may be handed to: ComplyDesk staff with a live
assignment to the client, plus the client's own administrators. Partners and
employees are excluded — a partner is segmented to one entity and would be given
work they cannot open, and an employee cannot be the person their own queries
escalate to.

### Mandatory fields when creating an employee

`POST /users` enforces, for anyone with the `EMPLOYEE` role (or no role, which
defaults to employee):

| Field | Why |
|---|---|
| `pf_number` | statutory identity; a PF query cannot be answered without it |
| `date_of_joining` | statutory identity and service-length calculations |
| `date_of_birth` | the default first password is derived from it |
| `pan_number` | likewise, and unique per employee within a client |

Creating a user **with** `status: "EX_EMPLOYEE"` additionally requires
`last_working_day`. Every one of these is enforced in the API, not only in the
form.

---

## Dashboard date range

`GET /dashboard/summary`, `GET /dashboard/charts/{key}`

The dashboard defaults to **all time**. It used to default to the last 30 days
while the card was still labelled "All tickets" and still linked to an
unfiltered list, so the board said 32 and the list it opened said 41 — neither
wrong, and no way to tell. Narrowing is now something the reader asks for, and
when they do the KPI's deep link carries the same window as
`created_from` / `created_to`, so the number on the card and the number on the
list always match.

Two shapes are accepted, because the screen has two ways of asking:

```
?range=today|wtd|mtd|last7|last30|last90|ytd|all
?from=2026-08-01&to=2026-08-18
```

Explicit dates win when both are present, so a custom range is never
second-guessed by a stale preset left in the URL. `to` is **inclusive of the
whole day named** — a range ending "today" that stopped at midnight would omit
everything raised since, which is most of what the reader is looking for.

The range is applied to `created_at` — when the ticket was raised — because that
is the question the filter is read as answering: *what came in this week*.
Applying it to `resolved_at` would make "Today" show a board of tickets raised
months ago, and applying it to both would make the counts on the strip disagree
with the list each KPI links to.

It narrows the KPI totals, the trend chart, the grouped charts and the
per-client breakdown together, so the whole screen shows one period. A client
with nothing in the chosen range stays on the breakdown showing zero rather than
disappearing, which is the honest answer.

An unparseable value is **ignored** rather than rejected: a dashboard is a
read-only overview, and failing the whole page because one query parameter is
malformed serves nobody. The unfiltered view is the safe fallback.

---

## Who a ticket may be handed to

### `GET /tickets/assignable`

| Parameter | Effect |
|---|---|
| `ticket={id}` | narrows to that ticket's client **and its department** |
| `client={ref}` | narrows to that client; no department narrowing |
| `department_id={id}` | narrows to that department explicitly — for a transfer dialog asking "who could take this if I moved it to ESIC" |
| `q=` | name or email search |
| *(none)* | the caller's selected client, or unnarrowed |

The population is helpdesk staff whose remit covers the client, plus the
client's own workers (administrators and partners). Employees are excluded:
they raise tickets, they do not work them.

The department narrowing is the half that used to be missing, so an ESIC ticket
offered the PF desk and the choice was then refused on submit. **Who may be
handed a ticket in a department** is one rule, checked identically by this list
and by the assign and transfer endpoints:

- mapped to that department (`department_assignments`), **or**
- posted to it (`users.department_id`), **or**
- holding an unrestricted role (Super Admin, Helpdesk Head, Helpdesk Master
  Administrator), **or**
- holding **no department mapping at all** — a generalist, which is the desk's
  default state before anybody has been allocated, and not the same thing as
  being excluded.

Only somebody mapped exclusively to a *different* department is left out. An
assign naming them is refused with `assignee_id / INVALID`.

`GET /tickets/{id}/transfer-agents?department_id=…` answers the same question
for a destination department, and runs the same query.

---

## Watchers

`GET|POST /tickets/{id}/watchers`, `DELETE /tickets/{id}/watchers/{userId}`

A watcher is somebody kept informed about a ticket they neither raised nor work:
a supervisor following an escalation, a partner tracking their entity's claim.

Gated on read access alone — anybody who can see a ticket may follow it. Adding
or removing *somebody else* additionally needs `ticket.assign` or
`ticket.update`, because it puts a ticket on their radar and sends them its
notifications. You can always stop watching yourself; `can_remove` on each row
is the server's answer to "may I remove this one".

`POST` with no body adds the caller. With `{"user_id": "01J…"}` it adds that
person, resolved inside the ticket's own client so a watcher can never be
somebody unable to open what they are watching. All three verbs return the
resulting list, so the panel never needs a follow-up read.

`DELETE` takes the watcher in the **path**. It also accepts `{"user_id": …}` in
the body for clients that cannot attach one to a DELETE — without that, a body
naming somebody else fell through to "remove me" and quietly took the caller off
the ticket.

### `GET /tickets/{id}/watcher-candidates?q=`

Who may be added. Its own route rather than the assign picker's, which the panel
used to borrow: `/tickets/assignable` is gated on `ticket.assign`, so anybody
without that permission — most of the desk — opened an empty dropdown.

The ticket is loaded first, which is both the access check and the tenant
boundary: the list can only contain people who belong to that ticket's client or
to the desk that covers it, so a name from another tenant cannot appear here to
be picked. Anyone already watching is left out. Employees are excluded — one
employee following another's PF query is a disclosure, not a feature.

Response is a bare array of `{id, name, email, role}`.

## A user's tickets

`GET /users/{id}/tickets`

The Tickets tab on a person's record: the tickets they **raised**. One they are
merely assigned belongs on their queue, not their record.

Deliberately the same query the main list uses with the requester pinned, so it
obeys the caller's scope, paginates and carries the same columns. A
`requester_id` in the query string cannot widen it to somebody else.

## Transfer, escalation and priority

### `POST /tickets/{id}/transfer`

```json
{ "department_id": "01J…", "assignee_id": "01J…", "comment": "why" }
```

Two rules the API enforces rather than trusting the form:

- **The pair has to be real.** The assignee must work the department being
  transferred to — an explicit department assignment, their own posting, or an
  unrestricted staff role. Naming the ESIC line and a PF agent is refused with
  `assignee_id / INVALID`.
- **The transfer has to move something.** Transferring to the department and
  agent the ticket already sits with is refused with `department_id / NO_CHANGE`,
  rather than recording an action, notifying people and changing nothing.

`GET /tickets/{id}/transfer-agents?department_id=…` is the second half of the
picker: who, in that department, can take it.

### `POST /tickets/{id}/escalate`

```json
{ "comment": "Statutory deadline in two days and the member is unpaid." }
```

**A comment is required.** An escalation is a claim on somebody else's
attention, and one with no stated reason gives whoever picks it up nothing to
act on.

Escalation is a **flag beside the status, not a status**. It used to overwrite
the status with `ESCALATED`, which destroyed what the board is actually worked
from — a ticket waiting on the employee and one waiting on the desk both became
"Escalated". `escalation_level` carries the urgency and `is_escalated` on the
response is the indicator to render alongside the real status. Migration
`000021` repairs rows flattened by the old behaviour.

### Priority

A **catalogue**, not an enum: `GET|POST /ticket-priorities`,
`PATCH|DELETE /ticket-priorities/{id}`. Reading is open to anyone who may raise
a ticket (the form renders its dropdown from it); changing it needs
`config.category`.

A row with no tenant is a platform level every client inherits; one with a
tenant is that client's own. Editing an inherited level creates the client's
copy, which shadows the shared row **for that client alone**. `weight` orders
the list — higher first — and `is_default` picks what a ticket is raised at when
nobody chooses.

`tickets.priority` deliberately stores the **key**, not a foreign key: tickets
are the historical record, and a level that is later renamed or retired must not
rewrite what a ticket was raised at. Which is also why a level cannot be deleted
while any ticket uses it, and why a built-in one can only be switched off:

| Attempt | Answer |
|---|---|
| Delete a level a ticket uses | `409` — switch it off instead |
| Delete a built-in level | `id / SYSTEM` |
| Delete an inherited level | `id / INHERITED` — switch it off for this client |
| Raise a ticket at an unknown level | `priority / INVALID` |

## Ticket statuses

Five, and the withdrawal:

| Status | Meaning |
|---|---|
| `NEW` | nobody has picked it up |
| `PENDING_HELPDESK` | waiting on the department |
| `PENDING_EMPLOYEE` | waiting on the client or the employee |
| `CLOSED` | done |
| `REOPENED` | done, and then it wasn't |
| `CANCELLED` | withdrawn — not a stage of the work, so not offered in the status list |

Migration `000024` retired four that were distinctions nobody was making: `OPEN`
and `IN_PROGRESS` both meant "the department has it" and became
`PENDING_HELPDESK`; `RESOLVED` and `CLOSED` both meant "the work is done" and
became `CLOSED`. `ESCALATED` had already become a flag in `000021`.

`resolved_at` is still stamped on close, because it is what the reopen window
counts from and what the satisfaction survey fires on.

Old names still **resolve** rather than matching nothing — a saved filter or an
integration naming `RESOLVED` is read as `CLOSED`. Which moves are legal is
per-category and table-driven: read `allowed_transitions` off the ticket detail
rather than assuming the graph.

## Two-factor authentication

Gated on the workspace's `mfa` feature, which is **on by default**. It shipped
off, which meant the whole enrolment surface was present, reachable, and
answered "not enabled for this workspace" on the first click.

| Step | Endpoint |
|---|---|
| Start enrolment | `POST /auth/mfa/enroll` → secret, `otpauth://` URL, 10 recovery codes |
| Finish enrolment | `POST /auth/mfa/confirm` `{code}` |
| Sign in | `POST /auth/login` → `{mfa_required: true, mfa_token}` and **no access token** |
| Complete sign-in | `POST /auth/mfa/verify` `{mfa_token, code}` or `{mfa_token, recovery_code}` |
| Turn it off | `POST /auth/mfa/disable` `{password}` |

Six-digit TOTP on a 30-second step. Enforcement is entirely server-side: with
2FA on, `POST /auth/login` issues no tokens at all until a valid code is
presented, so a client that ignored the challenge would simply have nothing to
call the API with. Wrong and expired codes are refused; recovery codes are
single-use.

**The recovery codes come from `enroll`, not `confirm`** — `confirm` only
reports that the code matched. Reading them off the confirm response is why the
"save these codes" dialog once opened empty.

## Client code

`PATCH /admin/tenants/{id}` accepts `client_code` **only while the client has no
tickets**.

The code is not decoration: it is part of the portal address employees and
partners sign in through, and it is stamped into every ticket number
(`AMP-PF-2026-000145`). Changing it afterwards would leave those numbers quoting
a code that no longer exists.

Before the first ticket there is nothing to break, so an operator who mistyped
the code during onboarding can correct it. Afterwards the API answers
`client_code / LOCKED` with the reason. `client_code_locked` on the tenant
response is the same answer up front, so the form can disable the field and say
why rather than letting the user discover it on save.

---

## Supporting routes

| Route | Purpose |
|---|---|
| `GET /categories?top_level=true` | the category picker |
| `GET /categories?parent_id={id}` | its subcategories |
| `GET /categories/{id}/form-schema` | custom fields to render |
| `GET /departments?client={ref}` | the departments of one client — step 2 of the picker |
| `GET /entities?department_id={id}` | the entities inside one department — step 3 |
| `GET /entities/for-category/{id}` | **PF vs ESIC establishments** by registration |
| `GET /modules?enabled_only=true` | which modules this client has |
| `GET /karma/clients` | the staff client switcher |

`/entities/for-category/{id}` is what makes selecting PF or ESIC show the right
establishments: a company may be registered for one and not the other, so the
two return genuinely different sets with their own registration numbers.
