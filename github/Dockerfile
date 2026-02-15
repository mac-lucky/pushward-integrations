ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-github ./cmd/pushward-github

FROM alpine:3.23

LABEL org.opencontainers.image.title="pushward-github" \
      org.opencontainers.image.description="GitHub Actions Live Activity bridge for PushWard" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-docker"

RUN apk add --no-cache ca-certificates
RUN adduser -D -u 1000 appuser

COPY --from=builder /pushward-github /pushward-github

USER appuser

ENTRYPOINT ["/pushward-github"]
CMD ["-config", "/config/config.yml"]
