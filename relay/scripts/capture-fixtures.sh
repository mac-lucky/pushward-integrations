#!/usr/bin/env bash
# capture-fixtures.sh — Spin up Grafana, ArgoCD, Radarr, and Sonarr on minikube,
# point their webhooks at a capture server, trigger events, and save payloads.
#
# Prerequisites: minikube, helm, kubectl, docker, go
#
# Usage: ./relay/scripts/capture-fixtures.sh

set -euo pipefail

CLUSTER_NAME="relay-fixtures"
CAPTURE_PORT=9999
TESTDATA_DIR="relay/testdata"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

cd "$REPO_ROOT"

cleanup() {
    echo "==> Cleaning up..."
    kill "$CAPTURE_PID" 2>/dev/null || true
    minikube delete --profile "$CLUSTER_NAME" 2>/dev/null || true
}
trap cleanup EXIT

# ─── 1. Start minikube ───────────────────────────────────────────────────────
echo "==> Starting minikube cluster '$CLUSTER_NAME'..."
minikube start --profile "$CLUSTER_NAME" --cpus=2 --memory=4096 --wait=all

KUBECTL="kubectl --context $CLUSTER_NAME"

# ─── 2. Start capture server (runs on host) ─────────────────────────────────
echo "==> Building and starting capture server..."
mkdir -p "$TESTDATA_DIR"
go build -o /tmp/capture-server ./relay/cmd/capture-server
/tmp/capture-server &
CAPTURE_PID=$!
sleep 1

# Get host IP reachable from minikube
HOST_IP=$(minikube ssh --profile "$CLUSTER_NAME" -- "route -n | grep '^0\.0\.0\.0' | awk '{print \$2}'" 2>/dev/null || echo "host.minikube.internal")
CAPTURE_URL="http://${HOST_IP}:${CAPTURE_PORT}"
echo "    Capture URL from inside minikube: $CAPTURE_URL"

# ─── 3. Deploy Grafana ──────────────────────────────────────────────────────
echo "==> Deploying Grafana..."
helm repo add grafana https://grafana.github.io/helm-charts 2>/dev/null || true
helm repo update grafana

cat <<GRAFANA_VALUES > /tmp/grafana-values.yaml
persistence:
  enabled: false
datasources:
  datasources.yaml:
    apiVersion: 1
    datasources:
      - name: TestData
        type: testdata
        access: proxy
        isDefault: true
alerting:
  contactpoints.yaml:
    apiVersion: 1
    contactPoints:
      - orgId: 1
        name: capture-webhook
        receivers:
          - uid: capture-1
            type: webhook
            settings:
              url: "${CAPTURE_URL}/grafana"
              httpMethod: POST
  policies.yaml:
    apiVersion: 1
    policies:
      - orgId: 1
        receiver: capture-webhook
  rules.yaml:
    apiVersion: 1
    groups:
      - orgId: 1
        name: test-alerts
        folder: test
        interval: 10s
        rules:
          - uid: test-cpu-alert
            title: TestCPUAlert
            condition: A
            data:
              - refId: A
                datasourceUid: __expr__
                model:
                  type: math
                  expression: "1"
            for: 0s
            labels:
              severity: critical
            annotations:
              summary: "Test CPU alert"
GRAFANA_VALUES

helm upgrade --install grafana grafana/grafana \
    --kube-context "$CLUSTER_NAME" \
    --namespace monitoring --create-namespace \
    -f /tmp/grafana-values.yaml \
    --wait --timeout 120s

echo "    Waiting for Grafana alert to fire..."
sleep 30

# ─── 4. Deploy ArgoCD ───────────────────────────────────────────────────────
echo "==> Deploying ArgoCD..."
$KUBECTL create namespace argocd --dry-run=client -o yaml | $KUBECTL apply -f -
$KUBECTL apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
echo "    Waiting for ArgoCD to be ready..."
$KUBECTL wait --for=condition=available deployment/argocd-server -n argocd --timeout=180s

# Configure ArgoCD notification webhook to capture server
$KUBECTL apply -n argocd -f - <<ARGOCD_CM
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-notifications-cm
  namespace: argocd
data:
  service.webhook.capture: |
    url: ${CAPTURE_URL}/argocd
    headers:
      - name: Content-Type
        value: application/json
  trigger.on-sync-running: |
    - when: app.status.operationState.phase in ['Running']
      send: [sync-event]
  trigger.on-sync-succeeded: |
    - when: app.status.operationState.phase in ['Succeeded']
      send: [sync-event]
  trigger.on-deployed: |
    - when: app.status.health.status == 'Healthy' && app.status.operationState.phase in ['Succeeded']
      send: [sync-event]
  trigger.on-sync-failed: |
    - when: app.status.operationState.phase in ['Error', 'Failed']
      send: [sync-event]
  trigger.on-health-degraded: |
    - when: app.status.health.status == 'Degraded'
      send: [sync-event]
  template.sync-event: |
    webhook:
      capture:
        method: POST
        body: |
          {
            "event": "{{.app.status.operationState.phase}}",
            "app": "{{.app.metadata.name}}",
            "revision": "{{.app.status.sync.revision}}",
            "health": "{{.app.status.health.status}}",
            "syncStatus": "{{.app.status.sync.status}}",
            "message": "{{.app.status.operationState.message}}",
            "repoURL": "{{.app.spec.source.repoURL}}"
          }
  subscriptions: |
    - recipients:
        - capture
      triggers:
        - on-sync-running
        - on-sync-succeeded
        - on-deployed
        - on-sync-failed
        - on-health-degraded
ARGOCD_CM

# Create a test application for ArgoCD to sync
$KUBECTL apply -n argocd -f - <<ARGOCD_APP
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: test-app
  namespace: argocd
  annotations:
    notifications.argoproj.io/subscribe.on-sync-running.capture: ""
    notifications.argoproj.io/subscribe.on-sync-succeeded.capture: ""
    notifications.argoproj.io/subscribe.on-deployed.capture: ""
    notifications.argoproj.io/subscribe.on-sync-failed.capture: ""
    notifications.argoproj.io/subscribe.on-health-degraded.capture: ""
spec:
  project: default
  source:
    repoURL: https://github.com/argoproj/argocd-example-apps
    path: guestbook
    targetRevision: HEAD
  destination:
    server: https://kubernetes.default.svc
    namespace: test-app
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
ARGOCD_APP

echo "    Waiting for ArgoCD sync events..."
sleep 60

# ─── 5. Deploy Radarr & Sonarr ──────────────────────────────────────────────
echo "==> Deploying Radarr and Sonarr..."

# Radarr
$KUBECTL create namespace media --dry-run=client -o yaml | $KUBECTL apply -f -
$KUBECTL apply -n media -f - <<RADARR_DEPLOY
apiVersion: apps/v1
kind: Deployment
metadata:
  name: radarr
spec:
  replicas: 1
  selector:
    matchLabels:
      app: radarr
  template:
    metadata:
      labels:
        app: radarr
    spec:
      containers:
        - name: radarr
          image: lscr.io/linuxserver/radarr:latest
          ports:
            - containerPort: 7878
          env:
            - name: PUID
              value: "1000"
            - name: PGID
              value: "1000"
            - name: TZ
              value: "UTC"
---
apiVersion: v1
kind: Service
metadata:
  name: radarr
spec:
  selector:
    app: radarr
  ports:
    - port: 7878
RADARR_DEPLOY

# Sonarr
$KUBECTL apply -n media -f - <<SONARR_DEPLOY
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sonarr
spec:
  replicas: 1
  selector:
    matchLabels:
      app: sonarr
  template:
    metadata:
      labels:
        app: sonarr
    spec:
      containers:
        - name: sonarr
          image: lscr.io/linuxserver/sonarr:latest
          ports:
            - containerPort: 8989
          env:
            - name: PUID
              value: "1000"
            - name: PGID
              value: "1000"
            - name: TZ
              value: "UTC"
---
apiVersion: v1
kind: Service
metadata:
  name: sonarr
spec:
  selector:
    app: sonarr
  ports:
    - port: 8989
SONARR_DEPLOY

echo "    Waiting for Radarr and Sonarr to start..."
$KUBECTL wait --for=condition=available deployment/radarr -n media --timeout=180s
$KUBECTL wait --for=condition=available deployment/sonarr -n media --timeout=180s
sleep 10

# Get API keys from Radarr and Sonarr configs
echo "    Configuring Radarr webhook..."
RADARR_POD=$($KUBECTL get pods -n media -l app=radarr -o jsonpath='{.items[0].metadata.name}')
RADARR_API_KEY=$($KUBECTL exec -n media "$RADARR_POD" -- cat /config/config.xml | grep -oP '(?<=<ApiKey>)[^<]+' || echo "")

if [ -n "$RADARR_API_KEY" ]; then
    # Port-forward Radarr and configure webhook via API
    $KUBECTL port-forward -n media svc/radarr 7878:7878 &
    PF_RADARR_PID=$!
    sleep 3
    curl -s -X POST "http://localhost:7878/api/v3/notification" \
        -H "X-Api-Key: $RADARR_API_KEY" \
        -H "Content-Type: application/json" \
        -d "{
            \"name\": \"capture\",
            \"implementation\": \"Webhook\",
            \"configContract\": \"WebhookSettings\",
            \"fields\": [
                {\"name\": \"url\", \"value\": \"${CAPTURE_URL}/radarr/webhook\"},
                {\"name\": \"method\", \"value\": 1}
            ],
            \"onGrab\": true,
            \"onDownload\": true,
            \"onUpgrade\": true
        }" || echo "    (Radarr webhook setup via API failed, will use Test button)"

    # Trigger test notification
    curl -s -X POST "http://localhost:7878/api/v3/notification/test" \
        -H "X-Api-Key: $RADARR_API_KEY" \
        -H "Content-Type: application/json" \
        -d "{
            \"name\": \"capture-test\",
            \"implementation\": \"Webhook\",
            \"configContract\": \"WebhookSettings\",
            \"fields\": [
                {\"name\": \"url\", \"value\": \"${CAPTURE_URL}/radarr/webhook\"},
                {\"name\": \"method\", \"value\": 1}
            ],
            \"onGrab\": true,
            \"onDownload\": true,
            \"onUpgrade\": true
        }" || echo "    (Radarr test notification failed)"
    kill "$PF_RADARR_PID" 2>/dev/null || true
else
    echo "    Could not get Radarr API key, skipping webhook config"
fi

echo "    Configuring Sonarr webhook..."
SONARR_POD=$($KUBECTL get pods -n media -l app=sonarr -o jsonpath='{.items[0].metadata.name}')
SONARR_API_KEY=$($KUBECTL exec -n media "$SONARR_POD" -- cat /config/config.xml | grep -oP '(?<=<ApiKey>)[^<]+' || echo "")

if [ -n "$SONARR_API_KEY" ]; then
    $KUBECTL port-forward -n media svc/sonarr 8989:8989 &
    PF_SONARR_PID=$!
    sleep 3
    curl -s -X POST "http://localhost:8989/api/v3/notification/test" \
        -H "X-Api-Key: $SONARR_API_KEY" \
        -H "Content-Type: application/json" \
        -d "{
            \"name\": \"capture-test\",
            \"implementation\": \"Webhook\",
            \"configContract\": \"WebhookSettings\",
            \"fields\": [
                {\"name\": \"url\", \"value\": \"${CAPTURE_URL}/sonarr/webhook\"},
                {\"name\": \"method\", \"value\": 1}
            ],
            \"onGrab\": true,
            \"onDownload\": true,
            \"onUpgrade\": true,
            \"onSeriesAdd\": true
        }" || echo "    (Sonarr test notification failed)"
    kill "$PF_SONARR_PID" 2>/dev/null || true
else
    echo "    Could not get Sonarr API key, skipping webhook config"
fi

# ─── 6. Collect results ─────────────────────────────────────────────────────
echo ""
echo "==> Captured fixtures:"
find "$TESTDATA_DIR" -name "*.json" -newer /tmp/capture-server -type f | sort
echo ""
echo "==> Done! Fixtures saved to $TESTDATA_DIR/"
echo "    Review and rename files, then commit."
