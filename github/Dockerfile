ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Copy module files for layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY github/go.mod github/go.sum ./github/
RUN cd github && go mod download

# Copy source
COPY shared/ ./shared/
COPY github/ ./github/
RUN cd github && CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-github ./cmd/pushward-github

FROM alpine:3.23

LABEL org.opencontainers.image.title="pushward-github" \
      org.opencontainers.image.description="GitHub Actions Live Activity bridge for PushWard" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-integrations"

RUN apk add --no-cache ca-certificates
RUN adduser -D -u 1000 appuser

COPY --from=builder /pushward-github /pushward-github

ENV PUSHWARD_URL="https://api.pushward.app"

USER 1000

ENTRYPOINT ["/pushward-github"]
CMD ["-config", "/config/config.yml"]
