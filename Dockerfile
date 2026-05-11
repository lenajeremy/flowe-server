FROM golang:1.26-alpine AS base
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

# ── dev stage (hot-reload with Air) ──────────────────────────
FROM base AS dev
RUN go install github.com/air-verse/air@latest
COPY . .
CMD ["air", "-c", ".air.toml"]

# ── prod stage ────────────────────────────────────────────────
FROM base AS builder
COPY . .
RUN go build -ldflags="-s -w" -o /server .

FROM alpine:3.21 AS prod
WORKDIR /app
COPY --from=builder /server ./server
EXPOSE 8080
CMD ["./server"]
