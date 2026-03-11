ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Copy module files for layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY bambulab/go.mod bambulab/go.sum ./bambulab/
RUN cd bambulab && go mod download

# Copy source
COPY shared/ ./shared/
COPY bambulab/ ./bambulab/
RUN cd bambulab && CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-bambulab ./cmd/pushward-bambulab

FROM alpine:3.23

LABEL org.opencontainers.image.title="pushward-bambulab" \
      org.opencontainers.image.description="BambuLab 3D printer Live Activity bridge for PushWard" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-integrations"

RUN apk add --no-cache ca-certificates
RUN adduser -D -u 1000 appuser

COPY --from=builder /pushward-bambulab /pushward-bambulab

USER 1000

ENTRYPOINT ["/pushward-bambulab"]
CMD ["-config", "/config/config.yml"]
