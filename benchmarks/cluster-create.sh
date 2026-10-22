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

# Creates a 50-node GKE cluster configured to run Agent Substrate and
# serve 5,000 multiplexed Claude Managed Agents actors.

set -o errexit -o nounset -o pipefail

GOOGLE_CLOUD_PROJECT="${GOOGLE_CLOUD_PROJECT:-kubecon-na-26-keynote-demo}"
CLUSTER_NAME="${CLUSTER_NAME:-ate-ant-bench}"
REGION="${REGION:-us-west1}"
ZONE="${ZONE:-us-west1-a}"
RELEASE_CHANNEL="${RELEASE_CHANNEL:-rapid}"
# GKE 1.37+ serves certificates.k8s.io/v1beta1 (podcertificaterequests and
# clustertrustbundles) by default, which Agent Substrate requires.
CLUSTER_VERSION="${CLUSTER_VERSION:-1.37.0-gke.3503000}"

# System node pool: hosts ate-system control plane, Valkey, ate-ant-controller, and loadgen.
SYSTEM_POOL_NAME="${SYSTEM_POOL_NAME:-default-pool}"
SYSTEM_NODES="${SYSTEM_NODES:-3}"
SYSTEM_MACHINE_TYPE="${SYSTEM_MACHINE_TYPE:-e2-standard-4}"
SYSTEM_DISK_SIZE="${SYSTEM_DISK_SIZE:-100GB}"

# Worker node pool: 50 gVisor nodes hosting the warm WorkerPool for 5k actors.
# Note: e2-standard-2 (2 vCPU, 8 GB RAM) is the smallest GKE machine type supporting gVisor.
WORKER_POOL_NAME="${WORKER_POOL_NAME:-gvisor-pool}"
WORKER_NODES="${WORKER_NODES:-50}"
WORKER_MACHINE_TYPE="${WORKER_MACHINE_TYPE:-e2-standard-2}"
WORKER_DISK_SIZE="${WORKER_DISK_SIZE:-100GB}"
WORKER_DISK_TYPE="${WORKER_DISK_TYPE:-pd-balanced}"
MAX_PODS_PER_NODE="${MAX_PODS_PER_NODE:-64}"

# Networking CIDRs sized for 50+ nodes and 5k+ pods/services.
CLUSTER_IPV4_CIDR="${CLUSTER_IPV4_CIDR:-/14}"
SERVICES_IPV4_CIDR="${SERVICES_IPV4_CIDR:-/18}"

SNAPSHOTS_BUCKET="${SNAPSHOTS_BUCKET:-gs://${GOOGLE_CLOUD_PROJECT}-ate-ant-snapshots}"

echo "=================================================================="
echo "Creating 50-node Benchmark Cluster for 5k Actors"
echo "  Project:            ${GOOGLE_CLOUD_PROJECT}"
echo "  Cluster:            ${CLUSTER_NAME} (${ZONE}, v${CLUSTER_VERSION})"
echo "  System Pool:        ${SYSTEM_NODES} x ${SYSTEM_MACHINE_TYPE}"
echo "  gVisor Worker Pool: ${WORKER_NODES} x ${WORKER_MACHINE_TYPE} (gVisor)"
echo "  Snapshots Bucket:   ${SNAPSHOTS_BUCKET}"
echo "=================================================================="

# 1. Ensure gcr.io Artifact Registry repository and GCS snapshot bucket exist.
if ! gcloud artifacts repositories describe gcr.io --location=us --project="${GOOGLE_CLOUD_PROJECT}" >/dev/null 2>&1; then
  echo "--> Creating gcr.io Artifact Registry repository in ${GOOGLE_CLOUD_PROJECT}..."
  gcloud artifacts repositories create gcr.io \
    --repository-format=docker \
    --location=us \
    --project="${GOOGLE_CLOUD_PROJECT}"
fi

BUCKET_URI="${SNAPSHOTS_BUCKET%/}"
if ! gcloud storage buckets describe "${BUCKET_URI}" --project="${GOOGLE_CLOUD_PROJECT}" >/dev/null 2>&1; then
  echo "--> Creating GCS snapshot bucket ${BUCKET_URI} in ${REGION}..."
  gcloud storage buckets create "${BUCKET_URI}" \
    --project="${GOOGLE_CLOUD_PROJECT}" \
    --location="${REGION}" \
    --uniform-bucket-level-access
else
  echo "--> Snapshot bucket ${BUCKET_URI} already exists."
fi

# 2. Create the GKE cluster with the system node pool and required k8s beta APIs.
if ! gcloud container clusters describe "${CLUSTER_NAME}" --zone="${ZONE}" --project="${GOOGLE_CLOUD_PROJECT}" >/dev/null 2>&1; then
  echo "--> Creating GKE cluster '${CLUSTER_NAME}' with system pool (${SYSTEM_NODES} nodes)..."
  gcloud container clusters create "${CLUSTER_NAME}" \
    --project="${GOOGLE_CLOUD_PROJECT}" \
    --zone="${ZONE}" \
    --release-channel="${RELEASE_CHANNEL}" \
    --cluster-version="${CLUSTER_VERSION}" \
    --machine-type="${SYSTEM_MACHINE_TYPE}" \
    --num-nodes="${SYSTEM_NODES}" \
    --disk-type="pd-ssd" \
    --disk-size="${SYSTEM_DISK_SIZE}" \
    --enable-ip-alias \
    --cluster-ipv4-cidr="${CLUSTER_IPV4_CIDR}" \
    --services-ipv4-cidr="${SERVICES_IPV4_CIDR}" \
    --default-max-pods-per-node="${MAX_PODS_PER_NODE}" \
    --workload-pool="${GOOGLE_CLOUD_PROJECT}.svc.id.goog" \
    --enable-kubernetes-unstable-apis="certificates.k8s.io/v1beta1/podcertificaterequests,certificates.k8s.io/v1beta1/clustertrustbundles" \
    --addons="HorizontalPodAutoscaling,HttpLoadBalancing,GcsFuseCsiDriver" \
    --scopes="https://www.googleapis.com/auth/cloud-platform" \
    --node-labels="ate.dev/pool=system"
else
  echo "--> GKE cluster '${CLUSTER_NAME}' already exists."
  if ! gcloud beta container clusters describe "${CLUSTER_NAME}" \
    --zone="${ZONE}" \
    --project="${GOOGLE_CLOUD_PROJECT}" \
    --format="value(enableK8sBetaApis.enabledApis)" | grep -q "clustertrustbundles"; then
    echo "--> Enabling k8s beta APIs on '${CLUSTER_NAME}'..."
    gcloud beta container clusters update "${CLUSTER_NAME}" \
      --project="${GOOGLE_CLOUD_PROJECT}" \
      --zone="${ZONE}" \
      --enable-kubernetes-unstable-apis="certificates.k8s.io/v1beta1/podcertificaterequests,certificates.k8s.io/v1beta1/clustertrustbundles" \
      --quiet
  fi
fi

# 3. Create the 50-node worker node pool for actor sandboxes (ateworker-gvisor runs runsc in-container).
if ! gcloud container node-pools describe "${WORKER_POOL_NAME}" \
  --cluster="${CLUSTER_NAME}" \
  --zone="${ZONE}" \
  --project="${GOOGLE_CLOUD_PROJECT}" >/dev/null 2>&1; then
  echo "--> Creating ${WORKER_NODES}-node worker pool '${WORKER_POOL_NAME}'..."
  gcloud container node-pools create "${WORKER_POOL_NAME}" \
    --project="${GOOGLE_CLOUD_PROJECT}" \
    --cluster="${CLUSTER_NAME}" \
    --zone="${ZONE}" \
    --machine-type="${WORKER_MACHINE_TYPE}" \
    --num-nodes="${WORKER_NODES}" \
    --disk-type="${WORKER_DISK_TYPE}" \
    --disk-size="${WORKER_DISK_SIZE}" \
    --node-taints="ate.dev/sandboxClass=gvisor:NoSchedule" \
    --no-enable-autoupgrade \
    --max-pods-per-node="${MAX_PODS_PER_NODE}" \
    --scopes="https://www.googleapis.com/auth/cloud-platform" \
    --node-labels="ate.dev/pool=workers"
else
  echo "--> Node pool '${WORKER_POOL_NAME}' already exists. Ensuring size=${WORKER_NODES}..."
  gcloud container clusters resize "${CLUSTER_NAME}" \
    --project="${GOOGLE_CLOUD_PROJECT}" \
    --zone="${ZONE}" \
    --node-pool="${WORKER_POOL_NAME}" \
    --num-nodes="${WORKER_NODES}" \
    --quiet
fi

# 4. Fetch kubeconfig credentials.
echo "--> Fetching kubeconfig credentials for '${CLUSTER_NAME}'..."
gcloud container clusters get-credentials "${CLUSTER_NAME}" \
  --zone="${ZONE}" \
  --project="${GOOGLE_CLOUD_PROJECT}"

# 5. Configure IAM and Workload Identity bindings for Agent Substrate and GKE nodes.
PROJECT_NUMBER="$(gcloud projects describe "${GOOGLE_CLOUD_PROJECT}" --format="value(projectNumber)")"
NODE_SA="serviceAccount:${PROJECT_NUMBER}-compute@developer.gserviceaccount.com"
WI_PREFIX="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${GOOGLE_CLOUD_PROJECT}.svc.id.goog/subject/ns/ate-system/sa"
ATELET_PRINCIPAL="${WI_PREFIX}/atelet"
ATE_API_PRINCIPAL="${WI_PREFIX}/ate-api-server"

echo "--> Configuring IAM permissions for GKE nodes and Agent Substrate Workload Identity..."
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
    --member="${MEMBER}" --role="roles/storage.objectAdmin" --condition=None --quiet >/dev/null
  gcloud storage buckets add-iam-policy-binding "${BUCKET_URI}" \
    --member="${MEMBER}" --role="roles/storage.bucketViewer" --condition=None --quiet >/dev/null
done

echo "--> Verifying cluster nodes..."
kubectl get nodes -l ate.dev/pool=system
echo "Total worker nodes ready: $(kubectl get nodes -l ate.dev/pool=workers --no-headers 2>/dev/null | wc -l | tr -d ' ')"
echo "Cluster '${CLUSTER_NAME}' is ready for benchmark installation."
