ARG GO_VERSION=1.27.1

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Copy module files for layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY bambulab/go.mod bambulab/go.sum ./bambulab/
RUN cd bambulab && go mod download

# Copy source
COPY shared/ ./shared/
COPY bambulab/ ./bambulab/
RUN cd bambulab && CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-bambulab ./cmd/pushward-bambulab

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

LABEL org.opencontainers.image.title="pushward-bambulab" \
      org.opencontainers.image.description="BambuLab 3D printer Live Activity bridge for PushWard" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-integrations"

# apk upgrade first: the digest-pinned base can lag Alpine package fixes, and
# upgrading at build picks them up without waiting for a base-image rebuild.
# Stale openssl failed the CI grype gate on 2026-08-27.
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates && adduser -D -u 1000 appuser

COPY --from=builder /pushward-bambulab /pushward-bambulab

USER 1000

ENTRYPOINT ["/pushward-bambulab"]
CMD ["-config", "/config/config.yml"]
