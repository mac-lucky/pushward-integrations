ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Copy module files for layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY grafana/go.mod grafana/go.sum ./grafana/
RUN cd grafana && go mod download

# Copy source
COPY shared/ ./shared/
COPY grafana/ ./grafana/
RUN cd grafana && CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-grafana ./cmd/pushward-grafana

FROM alpine:3.23

LABEL org.opencontainers.image.title="pushward-grafana" \
      org.opencontainers.image.description="Grafana webhook bridge for PushWard alert Live Activities" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-docker"

RUN apk add --no-cache ca-certificates
RUN adduser -D -u 1000 appuser

COPY --from=builder /pushward-grafana /pushward-grafana

USER 1000

EXPOSE 8090

ENTRYPOINT ["/pushward-grafana"]
CMD ["-config", "/config/config.yml"]
