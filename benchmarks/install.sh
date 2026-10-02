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
# Installs Agent Substrate (ate-system) and the ate-ant stack configured for
# the 50-node / 5,000-actor benchmark.
#
# Run cluster-create.sh first: it owns the GCP resources (cluster, snapshot
# bucket, registry, IAM bindings) that this script assumes already exist.

set -o errexit -o nounset -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

GOOGLE_CLOUD_PROJECT="${GOOGLE_CLOUD_PROJECT:-kubecon-na-26-keynote-demo}"
CLUSTER_NAME="${CLUSTER_NAME:-ate-ant-bench}"
ZONE="${ZONE:-us-west1-a}"

NAMESPACE="${NAMESPACE:-ate-ant}"
TEMPLATE="${TEMPLATE:-ate-ant-actor-template}"

# Default warm WorkerPool size (50 warm runners multiplexed across 5k sessions)
# and controller replicas for high-throughput event reconciliation.
WORKERPOOL_REPLICAS="${WORKERPOOL_REPLICAS:-50}"
CONTROLLER_REPLICAS="${CONTROLLER_REPLICAS:-50}"

# Container images and snapshot storage. Digests are pinned by build-images.sh.
WORKER_IMAGE="${WORKER_IMAGE:-gcr.io/${GOOGLE_CLOUD_PROJECT}/ate-ant@sha256:3fae8baea92eee9c90089c131f97369c24e26797e8b3945e78221a0123676ad2}"
ATEOM_IMAGE="${ATEOM_IMAGE:-gcr.io/${GOOGLE_CLOUD_PROJECT}/ate-images/ateom-gvisor@sha256:7d8878129e707c2842691cdef1f919450985df3f223dbb65361baca79c789e58}"
ATE_IMAGE_REPO="${ATE_IMAGE_REPO:-gcr.io/${GOOGLE_CLOUD_PROJECT}/ate-images}"
ATE_IMAGE_TAG="${ATE_IMAGE_TAG:-latest}"
SNAPSHOTS_BUCKET="${SNAPSHOTS_BUCKET:-gs://${GOOGLE_CLOUD_PROJECT}-ate-ant-snapshots/ate-ant/}"
BUCKET_NAME="${SNAPSHOTS_BUCKET#gs://}"
BUCKET_NAME="${BUCKET_NAME%%/*}"

# Optional Agent Substrate control-plane installation overrides.
INSTALL_SUBSTRATE="${INSTALL_SUBSTRATE:-true}"
SUBSTRATE_MANIFEST="${SUBSTRATE_MANIFEST:-}"
SUBSTRATE_NAMESPACE="${SUBSTRATE_NAMESPACE:-ate-system}"
SUBSTRATE_DIR="${SUBSTRATE_DIR:-${HOME}/substrate}"

echo "=================================================================="
echo "Installing Benchmark Stack"
echo "  Project:             ${GOOGLE_CLOUD_PROJECT}"
echo "  Namespace:           ${NAMESPACE}"
echo "  WorkerPool Replicas: ${WORKERPOOL_REPLICAS} (warm runners for 5k actors)"
echo "  Controller Replicas: ${CONTROLLER_REPLICAS}"
echo "  Substrate Repo:      ${ATE_IMAGE_REPO}:${ATE_IMAGE_TAG}"
echo "  Worker Image:        ${WORKER_IMAGE}"
echo "  Ateom Image:         ${ATEOM_IMAGE}"
echo "  Snapshots Bucket:    ${SNAPSHOTS_BUCKET}"
echo "=================================================================="

# 1. Install or verify the Agent Substrate control plane (ate-system).
if [[ -n "${SUBSTRATE_MANIFEST}" ]]; then
  echo "--> Applying Agent Substrate manifest from ${SUBSTRATE_MANIFEST}..."
  kubectl apply -f "${SUBSTRATE_MANIFEST}"
elif [[ "${INSTALL_SUBSTRATE}" == "true" ]]; then
  if [[ ! -d "${SUBSTRATE_DIR}" ]]; then
    SUBSTRATE_DIR="$(go list -m -f '{{.Dir}}' github.com/agent-substrate/substrate)"
  fi
  echo "--> Deploying Agent Substrate (ate-system) via ate-setup from ${SUBSTRATE_DIR}..."
  (
    cd "${SUBSTRATE_DIR}"
    BUCKET_NAME="${BUCKET_NAME}" \
    PROJECT_ID="${GOOGLE_CLOUD_PROJECT}" \
    CLUSTER_NAME="${CLUSTER_NAME}" \
    CLUSTER_LOCATION="${ZONE}" \
    go run ./cmd/ate-setup deploy ate-system \
      --no-dev-env \
      --image-repo="${ATE_IMAGE_REPO}" \
      --image-tag="${ATE_IMAGE_TAG}" \
      --rollout-timeout="600s"
  )
fi

kubectl label nodes --all "ate.dev/substrate-version=${ATE_IMAGE_TAG}" --overwrite >/dev/null

echo "--> Waiting for Agent Substrate control plane deployments in '${SUBSTRATE_NAMESPACE}'..."
kubectl wait --for=condition=Available deployment --all -n "${SUBSTRATE_NAMESPACE}" --timeout=300s

# 2. Generate and apply Kubernetes manifests (Namespace, Secret, RBAC, Valkey, WorkerPool, Controller).
echo "--> Applying ate-ant Kubernetes manifests..."
go run ./cmd/ate-ant manifest \
  --namespace="${NAMESPACE}" \
  --template="${TEMPLATE}" \
  --worker-image="${WORKER_IMAGE}" \
  --ateom-image="${ATEOM_IMAGE}" \
  --replicas="${WORKERPOOL_REPLICAS}" \
  --controller-replicas="${CONTROLLER_REPLICAS}" | kubectl apply -f -

echo "--> Waiting for Valkey StatefulSet to be ready..."
kubectl rollout status statefulset/valkey -n "${NAMESPACE}" --timeout=300s

echo "--> Waiting for ate-ant-controller Deployment (${CONTROLLER_REPLICAS} replicas) to be ready..."
kubectl rollout status deployment/ate-ant-controller -n "${NAMESPACE}" --timeout=300s

# 3. Register Atespace and ActorTemplate in the Substrate control plane.
echo "--> Registering Atespace '${NAMESPACE}' and ActorTemplate '${TEMPLATE}'..."
KUBECTL_ATE_BIN="$(command -v kubectl-ate || echo "${HOME}/go/bin/kubectl-ate")"
if [[ -x "${KUBECTL_ATE_BIN}" ]]; then
  "${KUBECTL_ATE_BIN}" create atespace "${NAMESPACE}" 2>/dev/null || true
  "${KUBECTL_ATE_BIN}" delete actor-template "${TEMPLATE}" --atespace="${NAMESPACE}" 2>/dev/null || true
  go run ./cmd/ate-ant manifest template \
    --namespace="${NAMESPACE}" \
    --template="${TEMPLATE}" \
    --worker-image="${WORKER_IMAGE}" \
    --snapshots-bucket="${SNAPSHOTS_BUCKET}" | "${KUBECTL_ATE_BIN}" create actor-template -f - || \
    "${KUBECTL_ATE_BIN}" get actor-template "${TEMPLATE}" --atespace="${NAMESPACE}"
else
  echo "WARNING: 'kubectl-ate' binary not found. To register the ActorTemplate manually, run:"
  echo "  kubectl-ate create atespace ${NAMESPACE}"
  echo "  go run ./cmd/ate-ant manifest template --worker-image=\"${WORKER_IMAGE}\" --snapshots-bucket=\"${SNAPSHOTS_BUCKET}\" | kubectl-ate create actor-template -f -"
fi

echo "--> Deployment summary in namespace '${NAMESPACE}':"
kubectl get statefulsets,deployments,workerpools -n "${NAMESPACE}" || kubectl get pods -n "${NAMESPACE}"
echo "Benchmark stack installation complete."
