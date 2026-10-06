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

# Runs the 5k-actor benchmark load generator either in-cluster (recommended
# for valkey/direct modes to avoid port-forward bottlenecks) or locally.

set -o errexit -o nounset -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

MODE="${MODE:-valkey}"
SESSIONS="${SESSIONS:-5000}"
CONCURRENCY="${CONCURRENCY:-100}"
TURNS_PER_SESSION="${TURNS_PER_SESSION:-1}"
RATE="${RATE:-0}"
TURN_INTERVAL="${TURN_INTERVAL:-0s}"
ACTIVE_HOLD="${ACTIVE_HOLD:-0s}"
SUSPEND_AFTER_TURN="${SUSPEND_AFTER_TURN:-true}"
WAIT_DRAIN="${WAIT_DRAIN:-true}"
NAMESPACE="${NAMESPACE:-ate-ant}"
TEMPLATE="${TEMPLATE:-ate-ant-actor-template}"
IN_CLUSTER="${IN_CLUSTER:-true}"
JSON_OUT="${JSON_OUT:-}"

LOADGEN_ARGS=(
  "--mode=${MODE}"
  "--sessions=${SESSIONS}"
  "--concurrency=${CONCURRENCY}"
  "--turns-per-session=${TURNS_PER_SESSION}"
  "--rate=${RATE}"
  "--turn-interval=${TURN_INTERVAL}"
  "--active-hold=${ACTIVE_HOLD}"
  "--suspend-after-turn=${SUSPEND_AFTER_TURN}"
  "--wait-drain=${WAIT_DRAIN}"
  "--namespace=${NAMESPACE}"
  "--template=${TEMPLATE}"
)

if [[ $# -gt 0 ]]; then
  LOADGEN_ARGS+=("$@")
fi

if [[ "${IN_CLUSTER}" != "true" ]]; then
  if [[ -n "${JSON_OUT}" ]]; then
    LOADGEN_ARGS+=("--json-out=${JSON_OUT}")
  fi
  echo "--> Running loadgen locally: go run ./benchmarks/loadgen ${LOADGEN_ARGS[*]}"
  exec go run ./benchmarks/loadgen "${LOADGEN_ARGS[@]}"
fi

POD_NAME="ate-ant-loadgen"
RUNNER_IMAGE="${RUNNER_IMAGE:-debian:bookworm-slim}"
TMP_BIN="$(mktemp -t loadgen-linux-amd64.XXXXXX)"

cleanup() {
  rm -f "${TMP_BIN}"
  kubectl delete pod "${POD_NAME}" -n "${NAMESPACE}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "--> Cross-compiling loadgen binary for linux/amd64..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -o "${TMP_BIN}" ./benchmarks/loadgen

echo "--> Launching in-cluster load generator pod '${POD_NAME}' in namespace '${NAMESPACE}'..."
kubectl delete pod "${POD_NAME}" -n "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true

cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: ate-ant-loadgen
spec:
  restartPolicy: Never
  containers:
  - name: loadgen
    image: ${RUNNER_IMAGE}
    command: ["sleep", "infinity"]
    envFrom:
    - secretRef:
        name: ate-ant-creds
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
          expirationSeconds: 3600
          path: ateapi-token
EOF

echo "--> Waiting for pod '${POD_NAME}' to be Ready..."
kubectl wait --for=condition=Ready "pod/${POD_NAME}" -n "${NAMESPACE}" --timeout=120s

echo "--> Copying loadgen binary into '${POD_NAME}'..."
kubectl cp "${TMP_BIN}" "${NAMESPACE}/${POD_NAME}:/tmp/loadgen"
kubectl exec -n "${NAMESPACE}" "${POD_NAME}" -- chmod +x /tmp/loadgen

REMOTE_JSON="/tmp/benchmark-report.json"
if [[ -n "${JSON_OUT}" ]]; then
  LOADGEN_ARGS+=("--json-out=${REMOTE_JSON}")
fi

echo "--> Executing benchmark in cluster: /tmp/loadgen ${LOADGEN_ARGS[*]}"
kubectl exec -n "${NAMESPACE}" "${POD_NAME}" -- /tmp/loadgen "${LOADGEN_ARGS[@]}"

if [[ -n "${JSON_OUT}" ]]; then
  echo "--> Copying JSON report to ${JSON_OUT}..."
  kubectl cp "${NAMESPACE}/${POD_NAME}:${REMOTE_JSON}" "${JSON_OUT}"
fi
