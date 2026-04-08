ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Copy module files for layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY sabnzbd/go.mod sabnzbd/go.sum ./sabnzbd/
RUN cd sabnzbd && go mod download

# Copy source
COPY shared/ ./shared/
COPY sabnzbd/ ./sabnzbd/
RUN cd sabnzbd && CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-sabnzbd ./cmd/pushward-sabnzbd

FROM alpine:3.23

LABEL org.opencontainers.image.title="pushward-sabnzbd" \
      org.opencontainers.image.description="SABnzbd Live Activity bridge for PushWard" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-integrations"

RUN apk add --no-cache ca-certificates && adduser -D -u 1000 appuser

COPY --from=builder /pushward-sabnzbd /pushward-sabnzbd

USER 1000

EXPOSE 8090

ENTRYPOINT ["/pushward-sabnzbd"]
CMD ["-config", "/config/config.yml"]
