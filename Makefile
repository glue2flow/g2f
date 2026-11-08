.PHONY: dev-db dev-server dev-web build test test-db web image up down plugin-sample plugin-mistral

# Development-only encryption key for secrets. Never use it in production.
DEV_SECRET_KEY ?= ZGV2LWtleS1kZXYta2V5LWRldi1rZXktZGV2LWtleS0=

# Local development without Keycloak (auth disabled, user "dev" in org "default").
dev-server:
	G2F_AUTH_MODE=none G2F_LISTEN=:8080 G2F_SECRET_KEY=$(DEV_SECRET_KEY) go run ./cmd/g2f

dev-web:
	cd web && npm install && npm run dev

dev-db:
	docker run -d --name g2f-postgres -p 5432:5432 \
		-e POSTGRES_USER=g2f -e POSTGRES_PASSWORD=g2f -e POSTGRES_DB=g2f postgres:16-alpine

test:
	go vet ./...
	go test ./...
	cd web && npm run typecheck

# Also runs the tests that need PostgreSQL (execution recovery), against the
# dev database from `make dev-db`. Each run creates and drops its own database.
G2F_TEST_DATABASE_URL ?= postgres://g2f:g2f@localhost:5432/g2f?sslmode=disable
test-db:
	G2F_TEST_DATABASE_URL='$(G2F_TEST_DATABASE_URL)' go test -race ./...

web:
	cd web && npm ci && npm run build

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/g2f ./cmd/g2f

image:
	docker build -t g2f:latest .

up:
	docker compose up --build -d

down:
	docker compose down

# Build the sample Telegram WebAssembly plugin -> examples/plugins/telegram/telegram.zip
plugin-sample:
	$(MAKE) -C examples/plugins/telegram go bundle

# Build the Mistral AI WebAssembly plugin -> examples/plugins/mistral/mistral.zip
plugin-mistral:
	$(MAKE) -C examples/plugins/mistral go bundle
