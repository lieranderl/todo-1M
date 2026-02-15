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
| `GET` | `/healthz` | Public | Liveness probe endpoint |
| `GET` | `/readyz` | Public | Readiness probe (NATS + Postgres checks) |
| `GET` | `/metrics` | Public | Prometheus metrics |

### sse-streamer (`:8081`)
| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/v1/groups` | Bearer | Read groups for current user |
| `GET` | `/api/v1/todos?group_id=...` | Bearer | Read projected todos for a group |
| `GET` | `/ui/workspace?group_id=...` | Bearer | Return Datastar HTML patches (groups + todos) |
| `GET` | `/events?group_id=...` | Bearer | Open SSE stream for selected group |
| `GET` | `/events/disconnect` | Bearer | Cancel active SSE stream for current user |
| `GET` | `/healthz` | Public | Liveness probe endpoint |
| `GET` | `/readyz` | Public | Readiness probe (NATS + Postgres checks) |
| `GET` | `/metrics` | Public | Prometheus metrics |

Notes:
- Group name/role label is non-interactive.
- `Connect` button is the only control that opens `/events` stream for a group.
- Query-token auth for `/events` and `/events/disconnect` remains as temporary fallback for legacy clients.

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
    U[User] --> L["/ and /login"]
    L -->|register/login success| W["/app"]
    W -->|toolbar nav| AR["/architecture"]
    W -->|toolbar nav| ST["/settings"]
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
5. Group label is display-only (no click behavior).
6. Clicking `Connect` sets active group and starts/switches SSE subscription via `/events?group_id=...`.
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
- Selecting a different group in the list updates active group/read model only.
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

Kubernetes runtime resources are Helm-managed:
- Workloads/services/probes: `infrastructure/helm/todo-1m/templates/app.yaml`
- Shared app config: `infrastructure/helm/todo-1m/templates/configmap.yaml`
- Secrets: `infrastructure/helm/todo-1m/templates/secrets.yaml`
- HPA/PDB: `infrastructure/helm/todo-1m/templates/scaling.yaml`

### Kubernetes Cluster Lifecycle

```bash
# status helper
make k8s-status

# Docker Desktop
docker desktop start
docker desktop stop

# kind
kind create cluster --name todo-1m
kind delete cluster --name todo-1m
```

Notes:
- `make k8s-status` only checks reachability/context and does not start/stop clusters.
- Docker Desktop does not provide a reliable CLI command to stop only Kubernetes while keeping the engine fully running; use Docker Desktop Settings -> Kubernetes to disable/enable it.

### Kubernetes Troubleshooting

```bash
# 1) Verify context + cluster reachability
kubectl config current-context
make k8s-status

# 2) Verify app workloads are healthy
kubectl -n todo-1m get deploy,pods,svc -o wide

# 3) Recreate local access tunnels (keep both running)
kubectl -n todo-1m port-forward svc/todo-1m-command-api 18080:8080
kubectl -n todo-1m port-forward svc/todo-1m-sse-streamer 18081:8081

# 4) Quick endpoint checks from your machine
curl http://127.0.0.1:18080/healthz
curl -I http://127.0.0.1:18081/login
```

If a port-forward dies, start it again.  
If `Cross-Origin Request Blocked` appears with `status code: (null)`, treat it as a connectivity issue first (usually a dead port-forward).

### Docker-First Runtime (Containers, No Load Test)

Run the full app + monitoring stack locally in Docker (without synthetic load):

```bash
make docker-monitoring-up
```

Open:
- App UI: `http://localhost:18081`
- API: `http://localhost:18080`
- Prometheus: `http://localhost:9090`
- Grafana: `http://localhost:3000` (`admin` / `admin`)

Stop:
```bash
make docker-monitoring-down
```

### Kubernetes Deployment Quickstart

#### Helm (Full Stack, Recommended)

1. Preflight check chart render (must include `todo-1m-app-config`):
   ```bash
   helm template todo-1m ./infrastructure/helm/todo-1m -n todo-1m | rg "name: todo-1m-app-config"
   ```

2. Install/upgrade the full stack (NATS + Postgres + all app services):
   ```bash
   helm upgrade --install todo-1m ./infrastructure/helm/todo-1m \
     -n todo-1m --create-namespace \
     --set jwtSecret="$(openssl rand -hex 32)" \
     --wait --rollback-on-failure --timeout 10m
   ```

3. If your GHCR images are private, create pull secret and attach it:
   ```bash
   kubectl -n todo-1m create secret docker-registry ghcr-creds \
     --docker-server=ghcr.io \
     --docker-username=<github-username> \
     --docker-password=<github-pat-with-read:packages>

   helm upgrade --install todo-1m ./infrastructure/helm/todo-1m \
     -n todo-1m \
     --reuse-values \
     --wait --rollback-on-failure --timeout 10m \
     --set imagePullSecrets[0].name=ghcr-creds
   ```

4. Verify Helm-created config and wait for workloads:
   ```bash
   kubectl -n todo-1m get configmap todo-1m-app-config
   kubectl -n todo-1m get secret todo-1m-app-secrets todo-1m-db-app
   kubectl -n todo-1m rollout status deploy/todo-1m-command-api
   kubectl -n todo-1m rollout status deploy/todo-1m-sse-streamer
   kubectl -n todo-1m rollout status deploy/todo-1m-domain-engine
   kubectl -n todo-1m rollout status deploy/todo-1m-data-sink
   ```

5. Access the app locally via port-forward:
   ```bash
   kubectl -n todo-1m port-forward svc/todo-1m-command-api 18080:8080
   kubectl -n todo-1m port-forward svc/todo-1m-sse-streamer 18081:8081
   ```

6. (Optional) Install in-cluster monitoring (Prometheus + Grafana):
   ```bash
   helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
   helm repo update
   helm upgrade --install monitoring prometheus-community/kube-prometheus-stack \
     -n monitoring --create-namespace \
	     --set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
	     --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false
	   ```

7. (Optional) Enable chart-managed PodMonitors:
   ```bash
   helm upgrade --install todo-1m ./infrastructure/helm/todo-1m \
     -n todo-1m \
     --reuse-values \
     --wait --rollback-on-failure --timeout 10m \
     --set monitoring.podMonitor.enabled=true \
     --set monitoring.podMonitor.namespace=monitoring
   ```

8. Port-forward monitoring UIs:
   ```bash
   kubectl -n monitoring port-forward svc/monitoring-grafana 3000:80
   kubectl -n monitoring port-forward svc/monitoring-kube-prometheus-prometheus 9090:9090
   ```

Common Helm issue:
- If pods show `CreateContainerConfigError` with `configmap "todo-1m-app-config" not found`, re-apply the chart:
  ```bash
  helm upgrade --install todo-1m ./infrastructure/helm/todo-1m -n todo-1m --reuse-values --wait --rollback-on-failure --timeout 10m
  kubectl -n todo-1m get configmap todo-1m-app-config
  ```
- If you manually created `todo-1m-app-config` before this fix, let Helm take ownership by recreating it via chart:
  ```bash
  kubectl -n todo-1m delete configmap todo-1m-app-config --ignore-not-found
  helm upgrade --install todo-1m ./infrastructure/helm/todo-1m -n todo-1m --reuse-values --wait --rollback-on-failure --timeout 10m
  ```

## Prerequisites
- **Go 1.22+**: [Download](https://go.dev/dl/)
- **Node.js 20+ + npm**: Required for local Tailwind + DaisyUI CSS build.
- **Docker & Docker Compose**: For local infrastructure.
- **kubectl + Helm 3**: Required for Kubernetes deployment/operations.
- **Make**: For running workflow commands.
- **Templ**: `go install github.com/a-h/templ/cmd/templ@latest`

## 🚀 Getting Started (Local Development)

### Environment Variables (optional)
- `NATS_URL` (default: `nats://localhost:4222`)
- `NATS_CONNECT_TIMEOUT` (default: `90s`)
- `DATABASE_URL` (default: `postgres://app:password@localhost:5432/app?sslmode=disable`)
- `DB_MIN_CONNS` (default: `2`)
- `DB_MAX_CONNS` (default: `20`)
- `DB_MAX_CONN_LIFETIME` (default: `30m`)
- `DB_MAX_CONN_IDLE_TIME` (default: `5m`)
- `DB_HEALTH_CHECK_PERIOD` (default: `30s`)

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
5. Confirm group label is display-only (no click behavior).
6. Click `Connect` on any row and verify connection switches to that group.
7. Send todo commands (create, edit, delete) and verify instant board updates.
8. Click `Connect` on a different group and verify selection/read model/realtime stream switch together.
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

## 📈 Load Testing + Monitoring

Use the dedicated compose stack to run the app, simulate hundreds of users, and monitor infra/service health in real time.

Start stack (app + load generator + Prometheus + Grafana + exporters):
```bash
make loadtest-up
```

Tail logs for load and core services:
```bash
make loadtest-logs
```

Stop stack:
```bash
make loadtest-down
```

Run app + monitoring only (without load-generator):
```bash
make docker-monitoring-up
```

Default URLs:
- App UI: `http://localhost:18081`
- API: `http://localhost:18080`
- Prometheus: `http://localhost:9090`
- Grafana: `http://localhost:3000` (`admin` / `admin`)
- Load generator metrics: `http://localhost:9099/metrics`

Load generator defaults (override in `docker-compose.loadtest.yml`):
- `LOADGEN_USERS=200`
- `LOADGEN_DURATION=10m`
- `LOADGEN_RAMP_UP=30s`
- `LOADGEN_ACTIONS_PER_USER_PER_SECOND=0.3`
- `LOADGEN_ENABLE_SSE=true`

Predefined load profiles (restart `load-generator` only):
- `make loadtest-profile-smoke`
  - `LOADGEN_USERS=25`
  - `LOADGEN_SETUP_CONCURRENCY=10`
  - `LOADGEN_STARTUP_WAIT=90s`
  - `LOADGEN_DURATION=3m`
  - `LOADGEN_RAMP_UP=30s`
  - `LOADGEN_ACTIONS_PER_USER_PER_SECOND=0.10`
  - `LOADGEN_REQUEST_TIMEOUT=10s`
  - `LOADGEN_ENABLE_SSE=true`
- `make loadtest-profile-baseline`
  - `LOADGEN_USERS=200`
  - `LOADGEN_SETUP_CONCURRENCY=25`
  - `LOADGEN_STARTUP_WAIT=2m`
  - `LOADGEN_DURATION=10m`
  - `LOADGEN_RAMP_UP=90s`
  - `LOADGEN_ACTIONS_PER_USER_PER_SECOND=0.30`
  - `LOADGEN_REQUEST_TIMEOUT=10s`
  - `LOADGEN_ENABLE_SSE=true`
- `make loadtest-profile-stress`
  - `LOADGEN_USERS=800`
  - `LOADGEN_SETUP_CONCURRENCY=80`
  - `LOADGEN_STARTUP_WAIT=3m`
  - `LOADGEN_DURATION=20m`
  - `LOADGEN_RAMP_UP=3m`
  - `LOADGEN_ACTIONS_PER_USER_PER_SECOND=0.80`
  - `LOADGEN_REQUEST_TIMEOUT=10s`
  - `LOADGEN_ENABLE_SSE=true`

Recommended PromQL checks:
- Online users (server truth): `todo1m_online_users`
- Active SSE fanout subscribers: `todo1m_active_group_subscribers`
- Loadgen VUs / SSE-connected users:
  - `todo1m_loadgen_virtual_users`
  - `todo1m_loadgen_sse_connected_users`
- Request throughput:
  - `sum(rate(todo1m_loadgen_requests_total{outcome="success"}[1m]))`
  - `sum(rate(todo1m_loadgen_requests_total{outcome="error"}[1m]))`
- CPU by container: `sum by (container) (rate(container_cpu_usage_seconds_total{container!=""}[1m]))`
- RAM by container: `sum by (container) (container_memory_working_set_bytes{container!=""})`
- Postgres active connections: `pg_stat_database_numbackends{datname="app"}`
- NATS client connections: `gnatsd_varz_connections`

## 📂 Project Structure
- **/cmd**: Entrypoints for all services.
  - `command-api`: HTTP ingress for actions.
  - `sse-streamer`: SSE egress for UI updates.
  - `domain-engine`: Core logic processor.
  - `data-sink`: Event persistence + read-model projection.
  - `load-generator`: Synthetic user/message load container.
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
- **/infrastructure**: Deployment and observability assets used by this repo.
- **/infrastructure/helm/todo-1m**: Helm chart for full-stack Kubernetes deployment.
- **/infrastructure/monitoring**: Prometheus + Grafana provisioning for load testing.
- **/.runtime**: Local-only runtime state (data, logs, pids).
