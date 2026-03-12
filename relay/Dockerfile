ARG GO_VERSION=1.25

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

FROM alpine:3.23

LABEL org.opencontainers.image.title="pushward-relay" \
      org.opencontainers.image.description="Multi-tenant webhook gateway for PushWard Live Activities" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-integrations"

RUN apk add --no-cache ca-certificates
RUN adduser -D -u 1000 appuser

COPY --from=builder /pushward-relay /pushward-relay

USER 1000

EXPOSE 8090

ENTRYPOINT ["/pushward-relay"]
CMD ["-config", "/config/config.yml"]
