#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Builds and deploys the live benchmark dashboard web UI directly inside the
# Kubernetes cluster (namespace ate-ant) and exposes it via a LoadBalancer Service.

set -o errexit -o nounset -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

NAMESPACE="${NAMESPACE:-ate-ant}"
POD_NAME="${POD_NAME:-ate-ant-dashboard}"
SERVICE_NAME="${SERVICE_NAME:-ate-ant-dashboard}"
SERVICE_TYPE="${SERVICE_TYPE:-LoadBalancer}"
RUNNER_IMAGE="${RUNNER_IMAGE:-debian:bookworm-slim}"

TMP_BIN="$(mktemp -t dashboard-linux-amd64.XXXXXX)"
cleanup() {
  rm -f "${TMP_BIN}"
}
trap cleanup EXIT

echo "--> Cross-compiling benchmark dashboard for linux/amd64..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -buildvcs=false -o "${TMP_BIN}" ./benchmarks/dashboard

echo "--> Granting the dashboard viewer access to atespace '${NAMESPACE}' in ate-api..."
NAMESPACE="${NAMESPACE}" ./benchmarks/access-policy.sh

echo "--> Applying RBAC and Service (${SERVICE_TYPE}) in namespace '${NAMESPACE}'..."
kubectl delete pod "${POD_NAME}" -n "${NAMESPACE}" --ignore-not-found --grace-period=0 --force --wait=true >/dev/null 2>&1 || true

cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ServiceAccount
metadata:
  # Its token authenticates the dashboard to ate-api (viewer on the atespace,
  # granted by access-policy.sh) and to Kubernetes (deployments reader below).
  name: ate-ant-dashboard
  namespace: ${NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ate-ant-dashboard-reader
  namespace: ${NAMESPACE}
rules:
- apiGroups: ["apps"]
  resources: ["deployments"]
  verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ate-ant-dashboard-reader
  namespace: ${NAMESPACE}
subjects:
- kind: ServiceAccount
  name: ate-ant-dashboard
  namespace: ${NAMESPACE}
roleRef:
  kind: Role
  name: ate-ant-dashboard-reader
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: v1
kind: Service
metadata:
  name: ${SERVICE_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: ate-ant-dashboard
spec:
  type: ${SERVICE_TYPE}
  selector:
    app: ate-ant-dashboard
  ports:
  - name: http
    port: 80
    targetPort: 8080
---
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: ate-ant-dashboard
spec:
  restartPolicy: Always
  serviceAccountName: ate-ant-dashboard
  containers:
  - name: dashboard
    image: ${RUNNER_IMAGE}
    command:
    - sh
    - -c
    - |
      while [ ! -x /tmp/ate-ant-dashboard ]; do
        sleep 0.5
      done
      exec /tmp/ate-ant-dashboard --addr=:8080 --namespace=${NAMESPACE}
    ports:
    - containerPort: 8080
      name: http
    envFrom:
    - secretRef:
        name: ate-ant-creds
    readinessProbe:
      httpGet:
        path: /healthz
        port: 8080
      initialDelaySeconds: 1
      periodSeconds: 3
    volumeMounts:
    - name: ateapi-token
      mountPath: /var/run/secrets/tokens
      readOnly: true
    - name: servicedns-ca
      mountPath: /run/servicedns-ca
      readOnly: true
  volumes:
  - name: ateapi-token
    projected:
      sources:
      - serviceAccountToken:
          audience: api.ate-system.svc
          expirationSeconds: 86400
          path: ateapi-token
  # Verifies ate-api's serving certificate.
  - name: servicedns-ca
    projected:
      sources:
      - clusterTrustBundle:
          signerName: servicedns.podcert.ate.dev/identity
          labelSelector:
            matchLabels:
              podcert.ate.dev/canarying: live
          path: trust-bundle.pem
EOF

echo "--> Waiting for pod '${POD_NAME}' container to start..."
for _ in $(seq 1 60); do
  phase="$(kubectl get pod "${POD_NAME}" -n "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  if [[ "${phase}" == "Running" ]]; then
    break
  fi
  sleep 1
done

echo "--> Uploading dashboard binary into '${POD_NAME}'..."
kubectl cp "${TMP_BIN}" "${NAMESPACE}/${POD_NAME}:/tmp/ate-ant-dashboard.bin"
kubectl exec -n "${NAMESPACE}" "${POD_NAME}" -- sh -c "chmod +x /tmp/ate-ant-dashboard.bin && mv /tmp/ate-ant-dashboard.bin /tmp/ate-ant-dashboard"

echo "--> Waiting for dashboard readiness probe..."
kubectl wait --for=condition=Ready "pod/${POD_NAME}" -n "${NAMESPACE}" --timeout=60s

if [[ "${SERVICE_TYPE}" == "LoadBalancer" ]]; then
  echo "--> Waiting for LoadBalancer external IP on service/${SERVICE_NAME}..."
  EXTERNAL_IP=""
  for _ in $(seq 1 60); do
    EXTERNAL_IP="$(kubectl get svc "${SERVICE_NAME}" -n "${NAMESPACE}" -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [[ -n "${EXTERNAL_IP}" ]]; then
      break
    fi
    sleep 2
  done
  if [[ -n "${EXTERNAL_IP}" ]]; then
    echo ""
    echo "=========================================================="
    echo "Dashboard is live at: http://${EXTERNAL_IP}/"
    echo "JSON Stats API:       http://${EXTERNAL_IP}/api/stats"
    echo "=========================================================="
  else
    echo "LoadBalancer IP is still provisioning. Check with:"
    echo "  kubectl get svc ${SERVICE_NAME} -n ${NAMESPACE} -w"
  fi
fi

echo ""
echo "You can also access via port-forward:"
echo "  kubectl port-forward -n ${NAMESPACE} svc/${SERVICE_NAME} 8080:80"
