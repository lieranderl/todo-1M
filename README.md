# Todo-1M: CQRS + Event-Driven Todo Architecture

A high-performance, event-driven Todo application designed for 1 million concurrent users, running on Hetzner Bare Metal.

## Architecture Highlights
- **CQRS split**:
  - `command-api` (`:8080`) handles auth + writes (`/api/v1/command`, group mutations).
  - `sse-streamer` (`:8081`) handles reads (`/api/v1/groups`, `/api/v1/todos`, `/ui/workspace`) and SSE.
- **Backbone**: NATS JetStream with deterministic subject sharding (1024 partitions).
- **Compute**: Go microservices.
- **Frontend**: Datastar + Templ.
- **UI access model**:
  - `/` and `/login` are public authentication pages.
  - `/app`, `/architecture`, and `/settings` are token-gated in the browser.
- **Networking target**: Cilium Gateway API (Kubernetes deployment).
- **Persistence**: Postgres read model (`users`, `groups`, `group_members`, `refresh_tokens`, `todo_events`, `todos`, `group_projection_offsets`).
- **Collaboration controls**: Group RBAC (`owner`, `admin`, `member`) with edit/delete moderation rules.

## System Diagram (End-to-End)
```mermaid
flowchart LR
    B[Browser<br/>Templ + Datastar UI] -->|POST /api/v1/auth/*<br/>POST/PATCH/DELETE /api/v1/groups*<br/>POST /api/v1/command| A[command-api :8080]
    B -->|GET /api/v1/groups<br/>GET /api/v1/todos<br/>GET /ui/workspace<br/>SSE /events| S[sse-streamer :8081]

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
5. Membership is enforced on both read (`/api/v1/groups`, `/api/v1/todos`, `/ui/workspace`, `/events`) and write (`/api/v1/command`) paths.

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
- Access token is short-lived.
- Refresh token rotation is supported via `POST /api/v1/auth/refresh`.
- Logout revokes refresh token via `POST /api/v1/auth/logout`.

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
Compile the `.templ` files into Go code.
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
4. Create a group and connect to that group.
5. Send todo commands (create, edit, delete) and verify group visibility.
6. Toggle `Show Realtime Feed` on/off and verify feed panel behavior.
7. Use `Refresh Token` and `Logout` from toolbar.
8. Watch logs of `domain-engine`, `data-sink`, and `sse-streamer` for command -> event -> projection -> query/stream flow.

9. Verify persistence:
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
  - `/` and `/login` authentication page
  - `/app` authenticated workspace
  - `/architecture` authenticated architecture reference
  - `/settings` authenticated RBAC reference
- **/infrastructure**: Kubernetes manifests (Cilium, NATS, CNPG).
- **/.runtime**: Local-only runtime state (data, logs, pids).
