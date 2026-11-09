# syntax=docker/dockerfile:1

# --- Editor (React) ---------------------------------------------------------
FROM --platform=$BUILDPLATFORM node:26-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

# --- Server (Go) ------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY modules ./modules
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/g2f ./cmd/g2f

# --- Runtime ----------------------------------------------------------------
# distroless/static: no shell, no package manager, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=server /out/g2f /app/g2f
COPY --from=web /src/web/dist /app/web
COPY modules /app/modules
ENV G2F_LISTEN=:8080 \
    G2F_STATIC_DIR=/app/web \
    G2F_MODULES_DIR=/app/modules
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 CMD ["/app/g2f", "healthcheck"]
ENTRYPOINT ["/app/g2f"]
