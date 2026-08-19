# ComplyDesk — technical documentation

ComplyDesk is a multi-tenant statutory-compliance helpdesk: employees of a
client company raise PF, ESIC and related queries, ComplyDesk's staff work them,
and the client's partners oversee the entities they are accountable for.

| Document | Answers |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | How the system is built — layers, multi-tenancy, auth, RBAC, storage, realtime, audit, error handling, deployment |
| [WORKFLOWS.md](WORKFLOWS.md) | What a person or a ticket goes through — onboarding, login, ticket lifecycle, assignment, escalation, watchers, documents, notifications, reporting |
| [DATABASE.md](DATABASE.md) | The schema — ER diagrams, every major table, keys, relationships, constraints, migrations |
| [API.md](API.md) | The wire contract — routes, payloads, permissions, error codes |
| [TEST_LOGINS.md](TEST_LOGINS.md) | Credentials for the seeded dataset, and what each account can do |

Diagrams are [Mermaid](https://mermaid.js.org/) so they render on GitHub and in
most editors, and — more importantly — so they are diffable and reviewable in
the pull request that changes them.

---

## Running it

```bash
# Backend (this repository)
go run ./cmd/cli migrate up
go run ./cmd/cli seed --demo
go run ./cmd/api                        # :8090

# Frontend (../complydesk_frontend)
npm install && npm run dev              # :5173, proxies /api/v1 to the backend

# Both, on Windows
.\run-dev.ps1
```

| Portal | URL |
|---|---|
| Admin | `http://localhost:5173/admin` |
| Agents | `http://localhost:5173/agents` |
| Partner | `http://localhost:5173/AMP/partners` |
| Employee | `http://localhost:5173/AMP/user` |

---

## Keeping the documentation true

Update the documentation in the **same change** that alters what it describes:

| If you change… | Update |
|---|---|
| a table, a column, a foreign key, a constraint | [DATABASE.md](DATABASE.md) |
| a route, a request or response shape, an error code | [API.md](API.md) |
| a process a person follows | [WORKFLOWS.md](WORKFLOWS.md) |
| a layer, a boundary, or a security control | [ARCHITECTURE.md](ARCHITECTURE.md) |

Documentation that is updated in a later pass is documentation that stops being
trusted, and an untrusted diagram is worse than none — people read it, act on
it, and are wrong.
