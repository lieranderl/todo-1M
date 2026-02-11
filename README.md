# Todo-1M: CQRS + Event-Driven Todo Architecture

A high-performance, event-driven Todo application designed for 1 million concurrent users, running on Hetzner Bare Metal.

## Architecture Highlights
- **CQRS split**:
  - `command-api` (`:8080`) handles auth + writes (`/api/v1/command`, group mutations).
  - `sse-streamer` (`:8081`) handles reads (`/api/v1/groups`, `/api/v1/todos`, `/ui/workspace`) and SSE.
- **Backbone**: NATS JetStream with deterministic subject sharding (1024 partitions).
- **Compute**: Go microservices.
- **Frontend**: Datastar + Templ + Tailwind CSS v4 + DaisyUI v5.5.18.
- **UI access model**:
  - `/` and `/login` are public authentication pages.
  - `/app`, `/architecture`, and `/settings` are token-gated in the browser.
- **Networking target**: Cilium Gateway API (Kubernetes deployment).
- **Persistence**: Postgres read model (`users`, `groups`, `group_members`, `refresh_tokens`, `todo_events`, `todos`, `group_projection_offsets`).
- **Collaboration controls**: Group RBAC (`owner`, `admin`, `member`) with edit/delete moderation rules.

## API Endpoints (Quick Reference)

### command-api (`:8080`)
| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/api/v1/auth/register` | Public | Create user + issue access/refresh tokens |
| `POST` | `/api/v1/auth/login` | Public | Login + issue access/refresh tokens |
| `POST` | `/api/v1/auth/refresh` | Public (refresh token in body) | Rotate refresh token + issue new access token |
| `POST` | `/api/v1/auth/logout` | Public (refresh token in body) | Revoke refresh token |
| `POST` | `/api/v1/groups` | Bearer | Create group (creator becomes `owner`) |
| `DELETE` | `/api/v1/groups/{groupID}` | Bearer | Delete group (`owner` only) |
| `POST` | `/api/v1/groups/{groupID}/members` | Bearer | Add group member (`owner/admin`) |
| `PATCH` | `/api/v1/groups/{groupID}/members/role` | Bearer | Change member role (`owner` only) |
| `POST` | `/api/v1/command` | Bearer | Submit todo commands (`create/update/delete`) |

### sse-streamer (`:8081`)
| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/v1/groups` | Bearer | Read groups for current user |
| `GET` | `/api/v1/todos?group_id=...` | Bearer | Read projected todos for a group |
| `GET` | `/ui/workspace?group_id=...` | Bearer | Return Datastar HTML patches (groups + todos) |
| `GET` | `/events?group_id=...&token=...` | Query token | Open SSE stream for selected group |
| `GET` | `/events/disconnect?token=...` | Query token | Cancel active SSE stream for current user |

Notes:
- Group row click in UI performs selection + `/ui/workspace` refresh and opens `/events`.
- `Connect` button also opens `/events` stream for the target group.

## System Diagram (End-to-End)
```mermaid
flowchart LR
    B[Browser<br/>Templ + Datastar UI] -->|POST /api/v1/auth/*<br/>POST/PATCH/DELETE /api/v1/groups*<br/>POST /api/v1/command| A[command-api :8080]
    B -->|GET /api/v1/groups<br/>GET /api/v1/todos<br/>GET /ui/workspace<br/>SSE /events<br/>GET /events/disconnect| S[sse-streamer :8081]

    A -->|publish app.command.<shard>.group.<group_id>| N[(NATS JetStream)]
    N -->|consume app.command.>| D[domain-engine]
    D -->|publish app.event.<shard>| N

    N -->|consume app.event.>| K[data-sink]
    K -->|append todo_events + update todos projection| P[(Postgres)]

    N -->|consume app.event.*.group.<group_id>| S
    S -->|SSE: datastar-patch-elements + todo-event| B
```

## Workflow Diagrams

### 1. UI Access and Navigation
```mermaid
flowchart LR
    U[User] --> L[/ and /login]
    L -->|register/login success| W[/app]
    W -->|toolbar nav| AR[/architecture]
    W -->|toolbar nav| ST[/settings]
    W -->|logout| L
    AR -->|logout| L
    ST -->|logout| L
```

### 2. Authentication and Session Lifecycle
```mermaid
sequenceDiagram
    participant Browser
    participant API as command-api
    participant DB as Postgres

    Browser->>API: POST /api/v1/auth/register or /login
    API->>DB: create/find user + issue refresh token
    API-->>Browser: access_token + refresh_token

    Browser->>API: POST /api/v1/auth/refresh
    API->>DB: validate old refresh token, rotate token
    API-->>Browser: new access_token + new refresh_token

    Browser->>API: POST /api/v1/auth/logout
    API->>DB: revoke refresh token
    API-->>Browser: 204 No Content
```

### 3. Group and RBAC Workflow
1. Authenticated user creates a group via `POST /api/v1/groups`.
2. Creator is auto-added as `owner`.
3. `owner/admin` can add members (`POST /api/v1/groups/{groupID}/members`).
4. Only `owner` can promote/demote role (`PATCH /api/v1/groups/{groupID}/members/role`).
5. Clicking a group row sets active + connected group and opens SSE subscription (`/events?group_id=...`).
6. Clicking `Connect` explicitly connects to the target group as well.
7. Membership is enforced on both read (`/api/v1/groups`, `/api/v1/todos`, `/ui/workspace`, `/events`) and write (`/api/v1/command`) paths.

### RBAC Matrix
| Capability | Owner | Admin | Member |
|---|---|---|---|
| Create group | ✅ | ✅ | ✅ |
| Add member to group as `member` | ✅ | ✅ | ❌ |
| Add member to group as `admin` | ✅ | ❌ | ❌ |
| Change member role (`member`/`admin`) | ✅ | ❌ | ❌ |
| Create todo/message | ✅ | ✅ | ✅ |
| Edit/delete own todo/message | ✅ | ✅ | ✅ |
| Edit/delete other users' todo/message | ✅ | ✅ | ❌ |
| Read group todos + SSE stream | ✅ | ✅ | ✅ |

Auth/session notes:
- Access token TTL is **15 minutes**.
- Refresh token TTL is **30 days** with rotation on refresh.
- Refresh token rotation is supported via `POST /api/v1/auth/refresh`.
- Logout revokes refresh token via `POST /api/v1/auth/logout`.
- Client does **not** auto-refresh access tokens; user can refresh from toolbar.
- Browser enforces session expiry by clearing local auth state and redirecting to `/login` when access token expires.

### 4. Todo Command/Event CQRS Workflow
```mermaid
sequenceDiagram
    participant Browser
    participant API as command-api
    participant NATS as NATS JetStream
    participant DE as domain-engine
    participant DS as data-sink
    participant DB as Postgres
    participant SSE as sse-streamer

    Browser->>API: POST /api/v1/command (create/update/delete)
    API->>API: validate JWT + group membership + RBAC
    API->>NATS: publish app.command.<shard>.group.<id>
    NATS->>DE: deliver command
    DE->>NATS: publish todo.created/updated/deleted event
    NATS->>DS: deliver event
    DS->>DB: append todo_events + project todos table
    NATS->>SSE: deliver app.event.*.group.<id>
    SSE-->>Browser: SSE feed patch + todo-event payload
    Browser->>SSE: GET /api/v1/todos?group_id=...
    Browser->>SSE: GET /ui/workspace?group_id=...
```

### 5. Realtime Feed and Board Consistency
- Feed updates are pushed immediately from event stream.
- Todo board reads from projected read model (`todos` table).
- SSE subscribes to group-scoped NATS subjects (`app.event.*.group.<group_id>`).
- Selecting a different group in the list switches active + connected group and rebinds stream.
- `Connect` explicitly establishes/switches the stream for the target group.
- `/events/disconnect` cancels the current user stream when switching groups or clearing a connected group.
- UI consumes `todo-event` and performs short retry-based reconciliation with projection offsets, so newly created/updated/deleted items appear consistently even under projection lag.
- Todo board refresh from SSE is debounced to reduce read pressure during event bursts.
- Realtime feed panel is optional in `/app` using the `Show Realtime Feed` checkbox (preference stored in localStorage).

### Kubernetes Edge Flow (target deployment)
```mermaid
flowchart LR
    U[User] --> G[Cilium Gateway]
    G --> R[HTTPRoute]
    R --> A[command-api Service]
    R --> S[sse-streamer Service]
```

## Prerequisites
- **Go 1.22+**: [Download](https://go.dev/dl/)
- **Node.js 20+ + npm**: Required for local Tailwind + DaisyUI CSS build.
- **Docker & Docker Compose**: For local infrastructure.
- **Make**: For running workflow commands.
- **Templ**: `go install github.com/a-h/templ/cmd/templ@latest`

## 🚀 Getting Started (Local Development)

### Environment Variables (optional)
- `NATS_URL` (default: `nats://localhost:4222`)
- `DATABASE_URL` (default: `postgres://app:password@localhost:5432/app?sslmode=disable`)

### 1. Start Infrastructure
Spin up the local NATS JetStream and Postgres instances using Docker Compose.
```bash
make run-infra
```
Local state is stored in `.runtime/data`.

### 2. Generate Frontend Code
Install frontend dependencies and build the local Tailwind + DaisyUI stylesheet:
```bash
npm install
make css
```

Then compile the `.templ` files into Go code:
```bash
make templ
```

### 3. Run Microservices
You can run each service in a separate terminal window:

**Terminal 1: SSE Streamer (Frontend & Events)**
Serves the UI at http://localhost:8081
```bash
make run-streamer
```

**Terminal 2: Command API (Ingress)**
Accepts commands at http://localhost:8080
```bash
make run-command
```

**Terminal 3: Domain Engine (Logic)**
Processes incoming commands and generates events.
```bash
make run-engine
```

**Terminal 4: Data Sink (Persistence)**
Persists events to Postgres.
```bash
make run-sink
```

Or start everything together:
```bash
make run-all
```
Logs are written to `.runtime/logs` and service PID files to `.runtime/pids`.

### 4. Interactive Testing
1. Open [http://localhost:8081](http://localhost:8081).
2. Register or login from `/login`.
3. You are redirected to `/app` (authenticated workspace).
4. Create a group.
5. Click a group row and verify it becomes the connected realtime group.
6. Click `Connect` on any row and verify connection switches to that group.
7. Send todo commands (create, edit, delete) and verify instant board updates.
8. Switch to another group row and verify active + connected group switch together.
9. Toggle `Show Realtime Feed` on/off and verify feed panel behavior.
10. Use `Refresh Token` and `Logout` from toolbar.
11. Watch logs of `domain-engine`, `data-sink`, and `sse-streamer` for command -> event -> projection -> query/stream flow.

12. Verify persistence:
   ```bash
   docker compose exec -T postgres psql -U app -d app -c \
     "select event_id, group_id, actor_user_id, actor_name, event_type, title, shard_id from todo_events order by inserted_at desc limit 10;"
   ```

## 🧪 Running Tests
Run the unit tests, specifically verifying the deterministic sharding logic.
```bash
make test
```

Run end-to-end integration test (Docker required):
```bash
make test-integration
```

Pre-deployment gate (tests + build):
```bash
make predeploy
```

Full pre-deployment gate (unit + integration + build):
```bash
make predeploy-full
```

Clean local artifacts (binaries + `.runtime` + Docker volumes):
```bash
make clean-local
```

## 📂 Project Structure
- **/cmd**: Entrypoints for all services.
  - `command-api`: HTTP ingress for actions.
  - `sse-streamer`: SSE egress for UI updates.
  - `domain-engine`: Core logic processor.
- **/internal/app**: Service use-cases and domain logic (testable, framework-light).
  - `commandapi`: request validation + command publishing use-case.
  - `domainengine`: command->event transformation use-case.
  - `datasink`: event decode + persistence use-case.
- **/internal/platform**: Infrastructure adapters (env, NATS wiring).
- **/internal/sharding**: Deterministic shard strategy.
- **/services/frontend**: Templ UI definitions.
  - `head.templ`: shared head, Datastar bootstrap, session-expiry guard.
  - `layout.templ`: shell navbar/sidebar/auth persistence helpers.
  - `components_ui.templ`: reusable UI fragments.
  - `page_login.templ`: `/` and `/login`.
  - `page_workspace.templ`: `/app`.
  - `page_architecture.templ`: `/architecture`.
  - `page_settings.templ`: `/settings`.
  - `/static/styles.input.css`: Tailwind/Daisy source.
  - `/static/styles.css`: generated stylesheet (embedded by `assets.go`).
- **/infrastructure**: Kubernetes manifests (Cilium, NATS, CNPG).
- **/.runtime**: Local-only runtime state (data, logs, pids).
