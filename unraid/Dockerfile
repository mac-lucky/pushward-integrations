ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Copy module files for layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY unraid/go.mod unraid/go.sum ./unraid/
RUN cd unraid && go mod download

# Copy source
COPY shared/ ./shared/
COPY unraid/ ./unraid/
RUN cd unraid && CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-unraid ./cmd/pushward-unraid

FROM alpine:3.23

LABEL org.opencontainers.image.title="pushward-unraid" \
      org.opencontainers.image.description="Unraid Live Activity bridge for PushWard" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-integrations"

RUN apk add --no-cache ca-certificates && adduser -D -u 1000 appuser

COPY --from=builder /pushward-unraid /pushward-unraid

USER 1000

ENTRYPOINT ["/pushward-unraid"]
CMD ["-config", "/config/config.yml"]
