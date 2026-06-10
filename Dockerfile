# syntax=docker/dockerfile:1

# ─────────────────────────────────────────────────────────────────────────────
# Stage 1 — builder: static, stripped, reproducible-ish binary.
# CGO is disabled so the result runs on distroless/static (no libc).
# ─────────────────────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache the module graph independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /app/engine ./cmd/engine

# ─────────────────────────────────────────────────────────────────────────────
# Stage 2 — final: distroless static. No shell, no package manager, no root.
# The binary embeds its own SQL migrations, so nothing else ships.
# ─────────────────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12

COPY --from=builder /app/engine /app/engine

USER nonroot:nonroot

# 8080 = operator API, 9090 = admin API (separate gin.Engine).
EXPOSE 8080 9090

ENTRYPOINT ["/app/engine"]
