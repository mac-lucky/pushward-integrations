ARG GO_VERSION=1.25
FROM golang:${GO_VERSION}-alpine AS builder
WORKDIR /src
COPY shared/go.mod shared/go.sum ./shared/
COPY mqtt/go.mod mqtt/go.sum ./mqtt/
RUN cd mqtt && go mod download
COPY shared/ ./shared/
COPY mqtt/ ./mqtt/
RUN cd mqtt && CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-mqtt ./cmd/pushward-mqtt
FROM alpine:3.23
LABEL org.opencontainers.image.title="pushward-mqtt" \
      org.opencontainers.image.description="MQTT bridge for PushWard Live Activities" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-integrations"
RUN apk add --no-cache ca-certificates
RUN adduser -D -u 1000 appuser
COPY --from=builder /pushward-mqtt /pushward-mqtt
USER 1000
ENTRYPOINT ["/pushward-mqtt"]
CMD ["-config", "/config/config.yml"]
