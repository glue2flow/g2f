# syntax=docker/dockerfile:1

# --- Editor (React) ---------------------------------------------------------
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

# --- Server (Go) ------------------------------------------------------------
FROM golang:1.25-alpine AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/g8g ./cmd/g8g

# --- Runtime ----------------------------------------------------------------
# distroless/static: no shell, no package manager, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=server /out/g8g /app/g8g
COPY --from=web /src/web/dist /app/web
COPY modules /app/modules
ENV G8G_LISTEN=:8080 \
    G8G_STATIC_DIR=/app/web \
    G8G_MODULES_DIR=/app/modules
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 CMD ["/app/g8g", "healthcheck"]
ENTRYPOINT ["/app/g8g"]
