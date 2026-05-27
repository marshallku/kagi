# Multi-stage build for kagi serve. The container is meant to live as a
# sidecar next to consumers (e.g. sssup/irang-api) so they can call kagi
# over localhost / cluster-internal HTTP without bundling the kagi binary.
#
# Auth in containers: KAGI_SESSION env (if present) or KAGI_EMAIL +
# KAGI_PASSWORD auto-login. Keyring writes are best-effort and fail
# silently on Alpine (no libsecret/dbus) — fine because the session
# stays in-memory for the container's lifetime.

FROM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/kagi ./cmd/kagi

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/kagi /usr/local/bin/kagi

EXPOSE 8921
ENTRYPOINT ["/usr/local/bin/kagi"]
CMD ["serve", "-addr", "0.0.0.0:8921"]
