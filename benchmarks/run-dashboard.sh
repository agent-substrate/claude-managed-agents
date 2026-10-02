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
# Deploys the live benchmark dashboard web UI inside the Kubernetes cluster
# (namespace ate-ant) and exposes it via a LoadBalancer Service.
#
# The dashboard binary ships in the ate-ant worker image. Rebuild and
# reinstall (build-images.sh, install.sh) to pick up local changes to
# benchmarks/dashboard.

set -o errexit -o nounset -o pipefail

NAMESPACE="${NAMESPACE:-ate-ant}"
POD_NAME="${POD_NAME:-ate-ant-dashboard}"
SERVICE_NAME="${SERVICE_NAME:-ate-ant-dashboard}"
SERVICE_TYPE="${SERVICE_TYPE:-LoadBalancer}"
# Default to the worker image install.sh deployed; it contains /usr/local/bin/dashboard.
RUNNER_IMAGE="${RUNNER_IMAGE:-$(kubectl get deployment ate-ant-controller -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}')}"

echo "--> Applying RBAC, Service (${SERVICE_TYPE}), and dashboard Pod in namespace '${NAMESPACE}'..."
kubectl delete pod "${POD_NAME}" -n "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true

cat <<EOF | kubectl apply -f -
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
  name: default
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
  containers:
  - name: dashboard
    image: ${RUNNER_IMAGE}
    command: ["dashboard", "--addr=:8080", "--namespace=${NAMESPACE}"]
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
  volumes:
  - name: ateapi-token
    projected:
      sources:
      - serviceAccountToken:
          audience: api.ate-system.svc
          expirationSeconds: 86400
          path: ateapi-token
EOF

echo "--> Waiting for dashboard pod '${POD_NAME}' to be Ready..."
kubectl wait --for=condition=Ready "pod/${POD_NAME}" -n "${NAMESPACE}" --timeout=120s

if [[ "${SERVICE_TYPE}" == "LoadBalancer" ]]; then
  echo "--> Waiting for LoadBalancer external IP on service/${SERVICE_NAME}..."
  if kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' "svc/${SERVICE_NAME}" -n "${NAMESPACE}" --timeout=120s >/dev/null; then
    EXTERNAL_IP="$(kubectl get svc "${SERVICE_NAME}" -n "${NAMESPACE}" -o jsonpath='{.status.loadBalancer.ingress[0].ip}')"
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
