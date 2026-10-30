ARG GO_VERSION=1.27.3

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Copy module files for layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY relay/go.mod relay/go.sum ./relay/
RUN cd relay && go mod download

# Copy source
COPY shared/ ./shared/
COPY relay/ ./relay/
RUN cd relay && CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-relay ./cmd/pushward-relay

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

LABEL org.opencontainers.image.title="pushward-relay" \
      org.opencontainers.image.description="Multi-tenant webhook gateway for PushWard Live Activities" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-integrations"

# apk upgrade first: the digest-pinned base can lag Alpine package fixes, and
# upgrading at build picks them up without waiting for a base-image rebuild.
# Stale openssl failed the CI grype gate on 2026-08-27.
# PKG_REFRESH is new on every CI build, so this RUN never comes from the layer cache.
ARG PKG_REFRESH
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates && adduser -D -u 1000 appuser

COPY --from=builder /pushward-relay /pushward-relay

USER 1000

EXPOSE 8090 9090

ENTRYPOINT ["/pushward-relay"]
CMD ["-config", "/config/config.yml"]
