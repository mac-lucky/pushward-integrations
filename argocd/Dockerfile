ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Copy module files for layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY argocd/go.mod argocd/go.sum ./argocd/
RUN cd argocd && go mod download

# Copy source
COPY shared/ ./shared/
COPY argocd/ ./argocd/
RUN cd argocd && CGO_ENABLED=0 go build -ldflags="-s -w" -o /pushward-argocd ./cmd/pushward-argocd

FROM alpine:3.23

LABEL org.opencontainers.image.title="pushward-argocd" \
      org.opencontainers.image.description="ArgoCD webhook bridge for PushWard sync Live Activities" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-docker"

RUN apk add --no-cache ca-certificates
RUN adduser -D -u 1000 appuser

COPY --from=builder /pushward-argocd /pushward-argocd

USER appuser

EXPOSE 8090

ENTRYPOINT ["/pushward-argocd"]
CMD ["-config", "/config/config.yml"]
