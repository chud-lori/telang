# syntax=docker/dockerfile:1.7

# Build stage: produce a static-ish binary. modernc.org/sqlite is pure Go so
# we can disable cgo and build a fully static binary.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/telang ./cmd/telang

# Runtime: small base with TLS certs (Telegram is HTTPS) and tini for PID 1.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tini && \
    addgroup -S telang && adduser -S -G telang telang && \
    mkdir -p /etc/telang /var/lib/telang && \
    chown -R telang:telang /etc/telang /var/lib/telang
COPY --from=build /out/telang /usr/local/bin/telang
USER telang
EXPOSE 9000
VOLUME ["/etc/telang", "/var/lib/telang"]
ENTRYPOINT ["/sbin/tini", "--", "telang"]
CMD ["serve", "--config", "/etc/telang/config.toml"]
