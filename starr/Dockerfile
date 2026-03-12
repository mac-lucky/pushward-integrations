ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Copy module files for layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY starr/go.mod starr/go.sum ./starr/
RUN cd starr && go mod download

# Copy source
COPY shared/ ./shared/
COPY starr/ ./starr/
RUN cd starr && CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-starr ./cmd/pushward-starr

FROM alpine:3.23

LABEL org.opencontainers.image.title="pushward-starr" \
      org.opencontainers.image.description="Radarr and Sonarr webhook bridge for PushWard download Live Activities" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-integrations"

RUN apk add --no-cache ca-certificates
RUN adduser -D -u 1000 appuser

COPY --from=builder /pushward-starr /pushward-starr

USER 1000

EXPOSE 8090

ENTRYPOINT ["/pushward-starr"]
CMD ["-config", "/config/config.yml"]
