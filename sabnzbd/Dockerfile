ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-sabnzbd ./cmd/pushward-sabnzbd

FROM alpine:3.23

LABEL org.opencontainers.image.title="pushward-sabnzbd" \
      org.opencontainers.image.description="SABnzbd Live Activity bridge for PushWard" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-docker"

RUN apk add --no-cache ca-certificates
RUN adduser -D -u 1000 appuser

COPY --from=builder /pushward-sabnzbd /pushward-sabnzbd

USER appuser

EXPOSE 8090

ENTRYPOINT ["/pushward-sabnzbd"]
CMD ["-config", "/config/config.yml"]
