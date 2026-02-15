.PHONY: all tidy css test test-integration build predeploy predeploy-full run-infra run-services clean clean-local templ verify prepare-runtime wait-infra stop-services run-all stop-all k8s-status image-build image-push image-build-push image-print docker-monitoring-up docker-monitoring-down loadtest-up loadtest-down loadtest-logs loadtest-profile-smoke loadtest-profile-baseline loadtest-profile-stress

all: tidy css templ test

RUNTIME_DIR := .runtime
LOG_DIR := $(RUNTIME_DIR)/logs
PID_DIR := $(RUNTIME_DIR)/pids
DATA_DIR := $(RUNTIME_DIR)/data

COMMAND_PID := $(PID_DIR)/command.pid
STREAMER_PID := $(PID_DIR)/streamer.pid
ENGINE_PID := $(PID_DIR)/engine.pid
SINK_PID := $(PID_DIR)/sink.pid
SERVICES := command-api sse-streamer domain-engine data-sink
IMAGE_REGISTRY ?= ghcr.io
IMAGE_OWNER ?= lieranderl
IMAGE_PREFIX ?= todo-1m
IMAGE_TAG ?= latest
IMAGE_PLATFORM ?= linux/arm64
LOADTEST_COMPOSE := docker-compose -f docker-compose.loadtest.yml

tidy:
	go mod tidy

css:
	npm run build:css

templ:
	# Installs templ if not present (requires GOBIN to be in PATH)
	# go install github.com/a-h/templ/cmd/templ@latest
	go run -mod=mod github.com/a-h/templ/cmd/templ generate

test:
	go test ./...

test-integration: build
	go test -count=1 -tags=integration ./integration -v

build:
	@mkdir -p bin
	go build -o bin/command-api ./cmd/command-api
	go build -o bin/sse-streamer ./cmd/sse-streamer
	go build -o bin/domain-engine ./cmd/domain-engine
	go build -o bin/data-sink ./cmd/data-sink

prepare-runtime:
	@mkdir -p $(LOG_DIR) $(PID_DIR) $(DATA_DIR)/nats $(DATA_DIR)/postgres

predeploy: test build

predeploy-full: predeploy test-integration

verify: test
	@echo "Verifying project structure..."
	@test -d cmd || (echo "Missing cmd/ directory" && exit 1)
	@test -f docker-compose.yml || (echo "Missing docker-compose.yml" && exit 1)

clean:
	rm -rf bin
	rm -rf $(RUNTIME_DIR)

clean-local: clean
	@docker compose down -v >/dev/null 2>&1 || true

run-infra: prepare-runtime
	docker-compose up -d

wait-infra:
	@echo "Waiting for NATS and Postgres..."
	@for i in `seq 1 60`; do \
		nc -z localhost 4222 >/dev/null 2>&1 && nc -z localhost 5432 >/dev/null 2>&1 && exit 0; \
		sleep 1; \
	done; \
	echo "Timed out waiting for infra readiness." && exit 1

run-command:
	go run cmd/command-api/main.go

run-streamer:
	go run cmd/sse-streamer/main.go

run-engine:
	go run cmd/domain-engine/main.go

run-sink:
	go run cmd/data-sink/main.go

stop-services:
	@echo "Stopping app services..."
	@for pid_file in $(COMMAND_PID) $(STREAMER_PID) $(ENGINE_PID) $(SINK_PID); do \
		if [ -f $$pid_file ]; then \
			kill `cat $$pid_file` 2>/dev/null || true; \
		fi; \
	done
	@pkill -f "/bin/command-api" >/dev/null 2>&1 || true
	@pkill -f "/bin/sse-streamer" >/dev/null 2>&1 || true
	@pkill -f "/bin/domain-engine" >/dev/null 2>&1 || true
	@pkill -f "/bin/data-sink" >/dev/null 2>&1 || true
	@pkill -f "cmd/command-api/main.go" >/dev/null 2>&1 || true
	@pkill -f "cmd/sse-streamer/main.go" >/dev/null 2>&1 || true
	@pkill -f "cmd/domain-engine/main.go" >/dev/null 2>&1 || true
	@pkill -f "cmd/data-sink/main.go" >/dev/null 2>&1 || true
	@rm -f $(COMMAND_PID) $(STREAMER_PID) $(ENGINE_PID) $(SINK_PID)

run-all: stop-services run-infra wait-infra css templ build
	@echo "Starting all services..."
	@nohup ./bin/command-api > $(LOG_DIR)/command.log 2>&1 & echo $$! > $(COMMAND_PID)
	@nohup ./bin/sse-streamer > $(LOG_DIR)/streamer.log 2>&1 & echo $$! > $(STREAMER_PID)
	@nohup ./bin/domain-engine > $(LOG_DIR)/engine.log 2>&1 & echo $$! > $(ENGINE_PID)
	@nohup ./bin/data-sink > $(LOG_DIR)/sink.log 2>&1 & echo $$! > $(SINK_PID)
	@sleep 2
	@kill -0 `cat $(COMMAND_PID)` `cat $(STREAMER_PID)` `cat $(ENGINE_PID)` `cat $(SINK_PID)` 2>/dev/null || \
	 (echo "One or more services failed to start. Check $(LOG_DIR)/*.log files." && exit 1)
	@for i in `seq 1 20`; do \
		nc -z localhost 8080 >/dev/null 2>&1 && nc -z localhost 8081 >/dev/null 2>&1 && break; \
		sleep 1; \
	done
	@nc -z localhost 8080 >/dev/null 2>&1 && nc -z localhost 8081 >/dev/null 2>&1 || \
	 (echo "HTTP services are not reachable on :8080/:8081. Check $(LOG_DIR)/*.log files." && exit 1)
	@echo "Services started."
	@echo "PIDs: $(PID_DIR)"
	@echo "Logs: $(LOG_DIR)"
	@echo "Frontend: http://localhost:8081"
	@echo "API:      http://localhost:8080"

stop-all: stop-services
	@echo "Stopping infrastructure..."
	@docker-compose down
	@echo "All stopped."

docker-monitoring-up:
	@mkdir -p $(DATA_DIR)/loadtest/nats $(DATA_DIR)/loadtest/postgres $(DATA_DIR)/loadtest/prometheus $(DATA_DIR)/loadtest/grafana
	@$(LOADTEST_COMPOSE) stop load-generator >/dev/null 2>&1 || true
	@$(LOADTEST_COMPOSE) rm -f load-generator >/dev/null 2>&1 || true
	$(LOADTEST_COMPOSE) up -d --build nats postgres command-api sse-streamer domain-engine data-sink prometheus grafana cadvisor node-exporter postgres-exporter nats-exporter
	@echo "Docker monitoring stack is starting (without load-generator)..."
	@echo "App:        http://localhost:18081"
	@echo "API:        http://localhost:18080"
	@echo "Prometheus: http://localhost:9090"
	@echo "Grafana:    http://localhost:3000 (admin/admin)"

docker-monitoring-down:
	$(LOADTEST_COMPOSE) down

loadtest-up:
	@mkdir -p $(DATA_DIR)/loadtest/nats $(DATA_DIR)/loadtest/postgres $(DATA_DIR)/loadtest/prometheus $(DATA_DIR)/loadtest/grafana
	$(LOADTEST_COMPOSE) up -d --build
	@echo "Load test stack is starting..."
	@echo "App:        http://localhost:18081"
	@echo "API:        http://localhost:18080"
	@echo "Prometheus: http://localhost:9090"
	@echo "Grafana:    http://localhost:3000 (admin/admin)"
	@echo "Loadgen metrics: http://localhost:9099/metrics"

loadtest-logs:
	$(LOADTEST_COMPOSE) logs -f --tail=100 load-generator command-api sse-streamer domain-engine data-sink

loadtest-down:
	$(LOADTEST_COMPOSE) down

loadtest-profile-smoke:
	@echo "Applying load profile: smoke"
	@LOADGEN_USERS=25 \
	LOADGEN_SETUP_CONCURRENCY=10 \
	LOADGEN_STARTUP_WAIT=90s \
	LOADGEN_DURATION=3m \
	LOADGEN_RAMP_UP=30s \
	LOADGEN_ACTIONS_PER_USER_PER_SECOND=0.10 \
	LOADGEN_REQUEST_TIMEOUT=10s \
	LOADGEN_ENABLE_SSE=true \
	$(LOADTEST_COMPOSE) up -d --no-deps --force-recreate load-generator

loadtest-profile-baseline:
	@echo "Applying load profile: baseline"
	@LOADGEN_USERS=200 \
	LOADGEN_SETUP_CONCURRENCY=25 \
	LOADGEN_STARTUP_WAIT=2m \
	LOADGEN_DURATION=10m \
	LOADGEN_RAMP_UP=90s \
	LOADGEN_ACTIONS_PER_USER_PER_SECOND=0.30 \
	LOADGEN_REQUEST_TIMEOUT=10s \
	LOADGEN_ENABLE_SSE=true \
	$(LOADTEST_COMPOSE) up -d --no-deps --force-recreate load-generator

loadtest-profile-stress:
	@echo "Applying load profile: stress"
	@LOADGEN_USERS=800 \
	LOADGEN_SETUP_CONCURRENCY=80 \
	LOADGEN_STARTUP_WAIT=3m \
	LOADGEN_DURATION=20m \
	LOADGEN_RAMP_UP=3m \
	LOADGEN_ACTIONS_PER_USER_PER_SECOND=0.80 \
	LOADGEN_REQUEST_TIMEOUT=10s \
	LOADGEN_ENABLE_SSE=true \
	$(LOADTEST_COMPOSE) up -d --no-deps --force-recreate load-generator

k8s-status:
	@echo "Current context: $$(kubectl config current-context 2>/dev/null || echo 'none')"
	@kubectl get nodes -o wide >/dev/null 2>&1 && kubectl get nodes -o wide || echo "Kubernetes API is not reachable (cluster may be stopped)."

image-print:
	@for svc in $(SERVICES); do \
		echo "$(IMAGE_REGISTRY)/$(IMAGE_OWNER)/$(IMAGE_PREFIX)-$$svc:$(IMAGE_TAG)"; \
	done

image-build:
	@echo "Building images for platform $(IMAGE_PLATFORM)..."
	@for svc in $(SERVICES); do \
		echo "==> $$svc"; \
		docker buildx build --platform $(IMAGE_PLATFORM) -f Dockerfile --build-arg SERVICE=$$svc -t $(IMAGE_REGISTRY)/$(IMAGE_OWNER)/$(IMAGE_PREFIX)-$$svc:$(IMAGE_TAG) --load . || exit 1; \
	done

image-push:
	@echo "Pushing images..."
	@for svc in $(SERVICES); do \
		echo "==> $$svc"; \
		docker push $(IMAGE_REGISTRY)/$(IMAGE_OWNER)/$(IMAGE_PREFIX)-$$svc:$(IMAGE_TAG) || exit 1; \
	done

image-build-push:
	@echo "Building and pushing images for platform $(IMAGE_PLATFORM)..."
	@for svc in $(SERVICES); do \
		echo "==> $$svc"; \
		docker buildx build --platform $(IMAGE_PLATFORM) -f Dockerfile --build-arg SERVICE=$$svc -t $(IMAGE_REGISTRY)/$(IMAGE_OWNER)/$(IMAGE_PREFIX)-$$svc:$(IMAGE_TAG) --push . || exit 1; \
	done
