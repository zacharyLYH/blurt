# Blurt developer entry points. Docker (compose v2) is the only requirement
# to run the stack; env-init also needs the Go toolchain.
#
#   make env-init           # first run: generate .env with local secrets
#   make start-no-reset     # start, keep existing data
#   make start-reset-full   # wipe SQLite volume, start clean
#   make e2e                # browser suite against the production stack
#   make e2e-real           # opt-in: push through a real push service
WEB_PORT ?= 9998
COMPOSE  ?= docker compose
# A headed browser needs a display. Linux hosts get a virtual one; macOS
# already has one, so the same target works on both.
XVFB     ?= $(shell command -v xvfb-run >/dev/null 2>&1 && echo xvfb-run -a)

.PHONY: env-init start-no-reset start-reset-full stop logs e2e e2e-real test-redis

env-init:
	cd server && go run ./cmd/envgen
	@echo "Now fill BLURT_SMTP_PASSWORD in .env. See README Developer section."

start-no-reset:
	BLURT_WEB_PORT=$(WEB_PORT) $(COMPOSE) up -d --build
	@echo "Blurt: http://localhost:$(WEB_PORT)"

start-reset-full:
	BLURT_WEB_PORT=$(WEB_PORT) $(COMPOSE) down -v --remove-orphans
	BLURT_WEB_PORT=$(WEB_PORT) $(COMPOSE) up -d --build
	@echo "Blurt: http://localhost:$(WEB_PORT) (fresh database)"

stop:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f --tail=100

# Real stack: docker (nginx + Go + SQLite) on a throwaway port, real browser,
# real session cookie. Push is exercised for real in e2e/push.spec.ts.
e2e:
	cd web && npm run test:e2e

# The whole push flow against a real push service: the backend sends, the
# browser's own service relays, the worker shows the notification. Needs
# outbound network, so it is opt-in and never part of the default gate.
e2e-real:
	cd web && $(XVFB) BLURT_E2E_REAL_PUSH=1 npm run test:e2e -- e2e/push.real.spec.ts

# Redis integration tests (server/internal/queue/redis_integration_test.go):
# a throwaway Redis with AOF, the shape production runs. The tests skip without
# BLURT_TEST_REDIS_URL, so the default `go test ./...` stays self-contained.
test-redis:
	@docker rm -f blurt-test-redis >/dev/null 2>&1 || true
	docker run --rm -d --name blurt-test-redis -p 6399:6379 redis:8.10-alpine redis-server --appendonly yes --appendfsync everysec >/dev/null
	@trap 'docker stop blurt-test-redis >/dev/null 2>&1 || true' EXIT; until docker exec blurt-test-redis redis-cli ping >/dev/null 2>&1; do sleep 0.2; done; cd server && BLURT_TEST_REDIS_URL=redis://localhost:6399/0 go test ./internal/queue/ ./internal/flow/ -run Redis -count=1 -v