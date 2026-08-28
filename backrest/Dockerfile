ARG GO_VERSION=1.26.5

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Copy module files for layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY backrest/go.mod backrest/go.sum ./backrest/
RUN cd backrest && go mod download

# Copy source
COPY shared/ ./shared/
COPY backrest/ ./backrest/
RUN cd backrest && CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-backrest ./cmd/pushward-backrest

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

LABEL org.opencontainers.image.title="pushward-backrest" \
      org.opencontainers.image.description="Backrest backup progress Live Activity bridge for PushWard" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-integrations"

# apk upgrade first: the digest-pinned base can lag Alpine package fixes, and
# upgrading at build picks them up without waiting for a base-image rebuild.
# Stale openssl failed the CI grype gate on 2026-08-27.
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates && adduser -D -u 1000 appuser

COPY --from=builder /pushward-backrest /pushward-backrest

ENV PUSHWARD_URL="https://api.pushward.app"

USER 1000

ENTRYPOINT ["/pushward-backrest"]
CMD ["-config", "/config/config.yml"]
