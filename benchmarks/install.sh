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

# Installs Agent Substrate (ate-system) and the ate-ant stack
# configured for the 50-node / 5,000-actor benchmark.

set -o errexit -o nounset -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

GOOGLE_CLOUD_PROJECT="${GOOGLE_CLOUD_PROJECT:-kubecon-na-26-keynote-demo}"
CLUSTER_NAME="${CLUSTER_NAME:-ate-ant-bench}"
ZONE="${ZONE:-us-west1-a}"
CLUSTER_VERSION="${CLUSTER_VERSION:-1.37.0-gke.3503000}"

NAMESPACE="${NAMESPACE:-ate-ant}"
TEMPLATE="${TEMPLATE:-ate-ant-actor-template}"

# Default warm WorkerPool size (50 warm runners multiplexed across 5k sessions)
# and controller replicas for high-throughput event reconciliation.
WORKERPOOL_REPLICAS="${WORKERPOOL_REPLICAS:-50}"
CONTROLLER_REPLICAS="${CONTROLLER_REPLICAS:-20}"

# Container images and snapshot storage.
BUILD_IMAGE="${BUILD_IMAGE:-false}"
WORKER_IMAGE="${WORKER_IMAGE:-gcr.io/${GOOGLE_CLOUD_PROJECT}/ate-ant@sha256:2f5b931584f80ccbd72851058a597af39e706eeaa20dfd86a7bc2fcd7410b416}"
ATEOM_IMAGE="${ATEOM_IMAGE:-gcr.io/${GOOGLE_CLOUD_PROJECT}/ate-images/ateworker-gvisor@sha256:c97cf56ec19ef37895a0e7cc5c6dada677cb3824c5e93fcf7f142053cfe19a85}"
ATE_IMAGE_REPO="${ATE_IMAGE_REPO:-gcr.io/${GOOGLE_CLOUD_PROJECT}/ate-images}"
ATE_IMAGE_TAG="${ATE_IMAGE_TAG:-latest}"
SNAPSHOTS_BUCKET="${SNAPSHOTS_BUCKET:-gs://${GOOGLE_CLOUD_PROJECT}-ate-ant-snapshots/ate-ant/}"
BUCKET_URI="gs://$(echo "${SNAPSHOTS_BUCKET#gs://}" | cut -d/ -f1)"

# Optional Agent Substrate control-plane installation overrides.
INSTALL_SUBSTRATE="${INSTALL_SUBSTRATE:-true}"
SUBSTRATE_MANIFEST="${SUBSTRATE_MANIFEST:-}"
SUBSTRATE_HELM_CHART="${SUBSTRATE_HELM_CHART:-}"
SUBSTRATE_NAMESPACE="${SUBSTRATE_NAMESPACE:-ate-system}"

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

# 1. Ensure gcr.io Artifact Registry repository and IAM permissions are in place.
if ! gcloud artifacts repositories describe gcr.io --location=us --project="${GOOGLE_CLOUD_PROJECT}" >/dev/null 2>&1; then
  echo "--> Creating gcr.io Artifact Registry repository in ${GOOGLE_CLOUD_PROJECT}..."
  gcloud artifacts repositories create gcr.io \
    --repository-format=docker \
    --location=us \
    --project="${GOOGLE_CLOUD_PROJECT}"
fi

PROJECT_NUMBER="$(gcloud projects describe "${GOOGLE_CLOUD_PROJECT}" --format="value(projectNumber)")"
NODE_SA="serviceAccount:${PROJECT_NUMBER}-compute@developer.gserviceaccount.com"
WI_PREFIX="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${GOOGLE_CLOUD_PROJECT}.svc.id.goog/subject/ns/${SUBSTRATE_NAMESPACE}/sa"
ATELET_PRINCIPAL="${WI_PREFIX}/atelet"
ATE_API_PRINCIPAL="${WI_PREFIX}/ate-api-server"

echo "--> Ensuring IAM permissions for GKE nodes and Workload Identity..."
gcloud projects add-iam-policy-binding "${GOOGLE_CLOUD_PROJECT}" \
  --member="${NODE_SA}" --role="roles/storage.objectViewer" --condition=None --quiet >/dev/null
gcloud projects add-iam-policy-binding "${GOOGLE_CLOUD_PROJECT}" \
  --member="${NODE_SA}" --role="roles/artifactregistry.reader" --condition=None --quiet >/dev/null
gcloud projects add-iam-policy-binding "${GOOGLE_CLOUD_PROJECT}" \
  --member="${ATELET_PRINCIPAL}" --role="roles/storage.objectAdmin" --condition=None --quiet >/dev/null
gcloud projects add-iam-policy-binding "${GOOGLE_CLOUD_PROJECT}" \
  --member="${ATELET_PRINCIPAL}" --role="roles/artifactregistry.reader" --condition=None --quiet >/dev/null

for MEMBER in "${ATELET_PRINCIPAL}" "${ATE_API_PRINCIPAL}"; do
  gcloud storage buckets add-iam-policy-binding "${BUCKET_URI}" \
    --member="${MEMBER}" --role="roles/storage.objectAdmin" --condition=None --quiet >/dev/null || true
  gcloud storage buckets add-iam-policy-binding "${BUCKET_URI}" \
    --member="${MEMBER}" --role="roles/storage.bucketViewer" --condition=None --quiet >/dev/null || true
done

# 2. Ensure the GKE control plane serves certificates.k8s.io/v1beta1 (required by ate-system).
if ! kubectl api-versions 2>/dev/null | grep -q "certificates.k8s.io/v1beta1"; then
  echo "--> Enabling certificates.k8s.io/v1beta1 APIs on '${CLUSTER_NAME}'..."
  gcloud beta container clusters update "${CLUSTER_NAME}" \
    --project="${GOOGLE_CLOUD_PROJECT}" \
    --zone="${ZONE}" \
    --enable-kubernetes-unstable-apis="certificates.k8s.io/v1beta1/podcertificaterequests,certificates.k8s.io/v1beta1/clustertrustbundles" \
    --quiet || true
fi

# 3. Install or verify Agent Substrate control plane (ate-system).
if [[ -n "${SUBSTRATE_MANIFEST}" ]]; then
  echo "--> Applying Agent Substrate manifest from ${SUBSTRATE_MANIFEST}..."
  kubectl apply -f "${SUBSTRATE_MANIFEST}"
elif [[ -n "${SUBSTRATE_HELM_CHART}" ]]; then
  echo "--> Installing Agent Substrate Helm chart from ${SUBSTRATE_HELM_CHART}..."
  helm upgrade --install ate-system "${SUBSTRATE_HELM_CHART}" \
    --namespace "${SUBSTRATE_NAMESPACE}" \
    --create-namespace \
    --wait
elif [[ "${INSTALL_SUBSTRATE}" == "true" ]]; then
  SUBSTRATE_MOD_DIR="${SUBSTRATE_DIR:-}"
  if [[ -z "${SUBSTRATE_MOD_DIR}" ]]; then
    if [[ -d "${HOME}/substrate/cmd/ate-setup" ]]; then
      SUBSTRATE_MOD_DIR="${HOME}/substrate"
    else
      SUBSTRATE_MOD_DIR="$(go list -m -f '{{.Dir}}' go.ate.dev/substrate)"
    fi
  fi
  echo "--> Deploying Agent Substrate (ate-system) via ate-setup from ${SUBSTRATE_MOD_DIR}..."
  (
    cd "${SUBSTRATE_MOD_DIR}"
    BUCKET_NAME="${BUCKET_URI#gs://}" \
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

# 4. Optionally build and push a fresh worker image.
if [[ "${BUILD_IMAGE}" == "true" ]]; then
  echo "--> Building and pushing worker image via 'make push'..."
  push_output="$(GOOGLE_CLOUD_PROJECT="${GOOGLE_CLOUD_PROJECT}" make push | tee /dev/stderr)"
  pushed_ref="$(echo "${push_output}" | grep -Eo "gcr\.io/[^[:space:]]+@sha256:[a-f0-9]{64}" | tail -n1 || true)"
  if [[ -n "${pushed_ref}" ]]; then
    WORKER_IMAGE="${pushed_ref}"
    echo "--> Using newly pushed worker image: ${WORKER_IMAGE}"
  fi
fi

# 5. Generate and apply Kubernetes manifests (Namespace, Secret, RBAC, Valkey, WorkerPool, Controller).
echo "--> Applying ate-ant Kubernetes manifests..."
go run ./cmd/ate-ant manifest \
  --namespace="${NAMESPACE}" \
  --template="${TEMPLATE}" \
  --worker-image="${WORKER_IMAGE}" \
  --ateom-image="${ATEOM_IMAGE}" \
  --replicas="${WORKERPOOL_REPLICAS}" \
  --controller-replicas="${CONTROLLER_REPLICAS}" | kubectl apply -f -

# 6. Wait for Valkey and Controller pods to become Ready.
echo "--> Waiting for Valkey StatefulSet to be ready..."
kubectl rollout status statefulset/valkey -n "${NAMESPACE}" --timeout=300s

echo "--> Waiting for ate-ant-controller Deployment (${CONTROLLER_REPLICAS} replicas) to be ready..."
kubectl rollout status deployment/ate-ant-controller -n "${NAMESPACE}" --timeout=300s

# 7. Register Atespace and ActorTemplate in Substrate control plane.
echo "--> Registering Atespace '${NAMESPACE}' and ActorTemplate '${TEMPLATE}'..."
KUBECTL_ATE_BIN="$(command -v kubectl-ate || echo "${HOME}/go/bin/kubectl-ate")"
if [[ -x "${KUBECTL_ATE_BIN}" ]]; then
  "${KUBECTL_ATE_BIN}" create atespace "${NAMESPACE}" 2>/dev/null || true
  # ate-api enforces authorization: grant the controller and dashboard roles.
  NAMESPACE="${NAMESPACE}" KUBECTL_ATE_BIN="${KUBECTL_ATE_BIN}" ./benchmarks/access-policy.sh
  "${KUBECTL_ATE_BIN}" delete actor-template "${TEMPLATE}" --atespace="${NAMESPACE}" 2>/dev/null || true
  go run ./cmd/ate-ant manifest template \
    --namespace="${NAMESPACE}" \
    --template="${TEMPLATE}" \
    --worker-image="${WORKER_IMAGE}" \
    --snapshots-bucket="${SNAPSHOTS_BUCKET}" | "${KUBECTL_ATE_BIN}" create actor-template --preview -f - || \
    "${KUBECTL_ATE_BIN}" get actor-template "${TEMPLATE}" --atespace="${NAMESPACE}"
else
  echo "WARNING: 'kubectl-ate' binary not found. To register the ActorTemplate manually, run:"
  echo "  kubectl-ate create atespace ${NAMESPACE}"
  echo "  go run ./cmd/ate-ant manifest template --worker-image=\"${WORKER_IMAGE}\" --snapshots-bucket=\"${SNAPSHOTS_BUCKET}\" | kubectl-ate create actor-template --preview -f -"
fi

echo "--> Deployment summary in namespace '${NAMESPACE}':"
kubectl get statefulsets,deployments,workerpools -n "${NAMESPACE}" || kubectl get pods -n "${NAMESPACE}"
echo "Benchmark stack installation complete."
