.PHONY: dev-db dev-server dev-web build test web image up down plugin-sample

# Development-only encryption key for secrets. Never use it in production.
DEV_SECRET_KEY ?= ZGV2LWtleS1kZXYta2V5LWRldi1rZXktZGV2LWtleS0=

# Local development without Keycloak (auth disabled, user "dev" in org "default").
dev-server:
	G8G_AUTH_MODE=none G8G_LISTEN=:8080 G8G_SECRET_KEY=$(DEV_SECRET_KEY) go run ./cmd/g8g

dev-web:
	cd web && npm install && npm run dev

dev-db:
	docker run -d --name g8g-postgres -p 5432:5432 \
		-e POSTGRES_USER=g8g -e POSTGRES_PASSWORD=g8g -e POSTGRES_DB=g8g postgres:16-alpine

test:
	go vet ./...
	go test ./...
	cd web && npm run typecheck

web:
	cd web && npm ci && npm run build

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/g8g ./cmd/g8g

image:
	docker build -t g8g:latest .

up:
	docker compose up --build -d

down:
	docker compose down

# Build the sample Telegram WebAssembly plugin -> examples/plugins/telegram/telegram.zip
plugin-sample:
	$(MAKE) -C examples/plugins/telegram go bundle
