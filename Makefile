.PHONY: all tidy css test test-integration build predeploy predeploy-full run-infra run-services clean clean-local templ verify prepare-runtime wait-infra stop-services run-all stop-all

all: tidy css templ test

RUNTIME_DIR := .runtime
LOG_DIR := $(RUNTIME_DIR)/logs
PID_DIR := $(RUNTIME_DIR)/pids
DATA_DIR := $(RUNTIME_DIR)/data

COMMAND_PID := $(PID_DIR)/command.pid
STREAMER_PID := $(PID_DIR)/streamer.pid
ENGINE_PID := $(PID_DIR)/engine.pid
SINK_PID := $(PID_DIR)/sink.pid

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
