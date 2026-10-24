GO ?= go
BIN ?= bin/onboardd
VERSION ?= 0.1.0

# CGO is required by the SQLite driver (github.com/mattn/go-sqlite3).
export CGO_ENABLED ?= 1

.PHONY: help build test test-race vet fmt fmt-check verify run migrate healthcheck \
	contract security e2e-sandbox sandbox-up sandbox-down sandbox-status sandbox-verify \
	ops-test verify-backup load-smoke docker-build compose-config backup restore rotate scan clean

help:
	@echo "account-onboarding v$(VERSION)"
	@echo ""
	@echo "  make build           build ./bin/onboardd"
	@echo "  make test            go test ./... (no race detector)"
	@echo "  make test-race       go test ./... -race -count=1"
	@echo "  make vet             go vet ./..."
	@echo "  make fmt / fmt-check gofmt -w / verify formatting"
	@echo "  make verify          fmt-check + vet + test-race + build"
	@echo "  make run             run the service (expects ONBOARD_* env)"
	@echo "  make migrate         apply migrations and exit"
	@echo "  make healthcheck     probe the local /healthz"
	@echo "  make contract        provider contract tests (offline)"
	@echo "  make security        API/security/redaction tests"
	@echo "  make e2e-sandbox     local Keycloak+Mailpit 5-identity E2E (see tests/sandbox)"
	@echo "  make sandbox-up/down/status/verify  local Keycloak + Mailpit fixture"
	@echo "  make docker-build    docker build (NOT_RUN when no daemon)"
	@echo "  make compose-config  validate deploy/compose.yaml (no daemon needed)"
	@echo "  make backup/restore/rotate/scan  operator tooling (see scripts/)"
	@echo "  make verify-backup SNAPSHOT=...  verify a snapshot (support encrypted .enc)"
	@echo "  make load-smoke      offline 5 concurrent + 30 sequential load evidence"
	@echo "  make ops-test        internal/ops reliability tests"
	@echo "  make clean           remove build artifacts"

build:
	@mkdir -p $(dir $(BIN))
	$(GO) build -trimpath -o $(BIN) ./cmd/onboardd

test:
	$(GO) test ./... -count=1

test-race:
	$(GO) test ./... -race -count=1

vet:
	$(GO) vet ./...

fmt:
	gofmt -w $$(find cmd internal -name '*.go')

fmt-check:
	@out="$$(gofmt -l $$(find cmd internal -name '*.go'))"; \
	if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi
	@echo "gofmt: clean"

verify: fmt-check vet test-race build

run: build
	./$(BIN)

migrate: build
	./$(BIN) migrate

healthcheck: build
	./$(BIN) healthcheck

contract:
	$(GO) test ./internal/mailx/... ./internal/provider/... -race -count=1

security:
	$(GO) test ./internal/api/... ./internal/security/... ./internal/ops/... -race -count=1

ops-test:
	$(GO) test ./internal/ops/... -race -count=1

verify-backup:
	@if [ -n "$(SNAPSHOT)" ] && [ -x scripts/verify_backup.sh ]; then scripts/verify_backup.sh "$(SNAPSHOT)"; \
	else echo "usage: make verify-backup SNAPSHOT=backups/state-<UTC>.sqlite[.enc] [KEYRING=...]"; exit 2; fi

load-smoke:
	@if [ -x scripts/load_smoke.sh ]; then scripts/load_smoke.sh; \
	else echo "NOT_RUN: scripts/load_smoke.sh not present"; fi

e2e-sandbox:
	@if [ -x tests/e2e/run.sh ]; then bash tests/e2e/run.sh; \
	elif [ -d tests/e2e ]; then ONBOARD_SANDBOX_E2E=1 $(GO) test ./tests/e2e/... -race -count=1; \
	else echo "NOT_RUN: tests/e2e not present yet (R2 pending)"; fi

sandbox-up:
	bash tests/sandbox/up.sh

sandbox-down:
	bash tests/sandbox/down.sh

sandbox-status:
	bash tests/sandbox/status.sh

sandbox-verify:
	bash tests/sandbox/verify.sh --count 5

docker-build:
	docker build -t account-onboarding:local .

compose-config:
	docker compose -f deploy/compose.yaml config

backup:
	@if [ -x scripts/backup.sh ]; then scripts/backup.sh; else echo "NOT_RUN: scripts/backup.sh not present"; fi

restore:
	@if [ -x scripts/restore.sh ]; then scripts/restore.sh; else echo "NOT_RUN: scripts/restore.sh not present"; fi

rotate:
	@if [ -x scripts/rotate_key.sh ]; then scripts/rotate_key.sh; else echo "NOT_RUN: scripts/rotate_key.sh not present"; fi

scan:
	@if [ -x scripts/redact_scan.sh ]; then scripts/redact_scan.sh; else echo "NOT_RUN: scripts/redact_scan.sh not present"; fi

clean:
	rm -rf bin
