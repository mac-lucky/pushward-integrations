ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Copy module files for layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY octoprint/go.mod octoprint/go.sum ./octoprint/
RUN cd octoprint && go mod download

# Copy source
COPY shared/ ./shared/
COPY octoprint/ ./octoprint/
RUN cd octoprint && CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-octoprint ./cmd/pushward-octoprint

FROM alpine:3.23

LABEL org.opencontainers.image.title="pushward-octoprint" \
      org.opencontainers.image.description="OctoPrint Live Activity bridge for PushWard" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-integrations"

RUN apk add --no-cache ca-certificates
RUN adduser -D -u 1000 appuser

COPY --from=builder /pushward-octoprint /pushward-octoprint

USER 1000

EXPOSE 8090

ENTRYPOINT ["/pushward-octoprint"]
CMD ["-config", "/config/config.yml"]
