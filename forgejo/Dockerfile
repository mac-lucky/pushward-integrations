ARG GO_VERSION=1.26.5

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Copy module files for layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY forgejo/go.mod forgejo/go.sum ./forgejo/
RUN cd forgejo && go mod download

# Copy source
COPY shared/ ./shared/
COPY forgejo/ ./forgejo/
RUN cd forgejo && CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-forgejo ./cmd/pushward-forgejo

FROM alpine:3.23

LABEL org.opencontainers.image.title="pushward-forgejo" \
      org.opencontainers.image.description="Forgejo Actions Live Activity bridge for PushWard" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-integrations"

RUN apk add --no-cache ca-certificates && adduser -D -u 1000 appuser

COPY --from=builder /pushward-forgejo /pushward-forgejo

ENV PUSHWARD_URL="https://api.pushward.app"

USER 1000

ENTRYPOINT ["/pushward-forgejo"]
CMD ["-config", "/config/config.yml"]
