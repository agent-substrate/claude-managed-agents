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

# Rebuilds container images, pushes them to gcr.io/${GOOGLE_CLOUD_PROJECT},
# and updates benchmarks/install.sh (and README.md) with the new @sha256 digests.

set -o errexit -o nounset -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

GOOGLE_CLOUD_PROJECT="${GOOGLE_CLOUD_PROJECT:-kubecon-na-26-keynote-demo}"
CONTAINER_ENGINE="${CONTAINER_ENGINE:-podman}"
PLATFORM="${PLATFORM:-linux/amd64}"

# Rebuilds Agent Substrate images (ateapi, atecontroller, atelet, atenet,
# podcertcontroller, ateom-gvisor) from SUBSTRATE_DIR (defaults to ~/substrate)
# unless BUILD_SUBSTRATE_IMAGES=false is set.
BUILD_SUBSTRATE_IMAGES="${BUILD_SUBSTRATE_IMAGES:-true}"
SUBSTRATE_DIR="${SUBSTRATE_DIR:-${HOME}/substrate}"
SKIP_IMAGES="${SKIP_IMAGES:-./cmd/ateom-microvm}"
UPDATE_README="${UPDATE_README:-true}"

INSTALL_SCRIPT="${ROOT_DIR}/benchmarks/install.sh"
ROOT_README="${ROOT_DIR}/README.md"

echo "=================================================================="
echo "Building and Pushing Benchmark Images"
echo "  Project:                ${GOOGLE_CLOUD_PROJECT}"
echo "  Container Engine:       ${CONTAINER_ENGINE} (${PLATFORM})"
echo "  Build Substrate Images: ${BUILD_SUBSTRATE_IMAGES}"
echo "=================================================================="

# 1. Ensure gcr.io Artifact Registry repository exists and authenticate with ADC token.
if ! gcloud artifacts repositories describe gcr.io --location=us --project="${GOOGLE_CLOUD_PROJECT}" >/dev/null 2>&1; then
  echo "--> Creating gcr.io Artifact Registry repository in ${GOOGLE_CLOUD_PROJECT}..."
  gcloud artifacts repositories create gcr.io \
    --repository-format=docker \
    --location=us \
    --project="${GOOGLE_CLOUD_PROJECT}"
fi

echo "--> Authenticating ${CONTAINER_ENGINE} to gcr.io using Application Default Credentials..."
ACCESS_TOKEN="$(gcloud auth application-default print-access-token 2>/dev/null || gcloud auth print-access-token)"
printf '%s' "${ACCESS_TOKEN}" | "${CONTAINER_ENGINE}" login -u oauth2accesstoken --password-stdin https://gcr.io >/dev/null

# 2. Build and push the ate-ant worker/controller image.
echo "--> Building and pushing ate-ant image via 'make push'..."
push_output="$(
  GOOGLE_CLOUD_PROJECT="${GOOGLE_CLOUD_PROJECT}" \
  CONTAINER_ENGINE="${CONTAINER_ENGINE}" \
  PLATFORM="${PLATFORM}" \
  make push | tee /dev/stderr
)"

WORKER_SHA="$(echo "${push_output}" | grep -Eo "sha256:[a-f0-9]{64}" | tail -n1 || true)"
if [[ -z "${WORKER_SHA}" ]]; then
  WORKER_SHA="$(gcloud container images describe "gcr.io/${GOOGLE_CLOUD_PROJECT}/ate-ant:latest" --format="value(image_summary.digest)")"
fi

if [[ -z "${WORKER_SHA}" ]]; then
  echo "ERROR: Could not determine sha256 digest for gcr.io/${GOOGLE_CLOUD_PROJECT}/ate-ant:latest" >&2
  exit 1
fi
echo "--> New ate-ant digest: ${WORKER_SHA}"

# 3. Optionally rebuild Agent Substrate images (including ateom-gvisor).
ATEOM_SHA=""
if [[ "${BUILD_SUBSTRATE_IMAGES}" == "true" ]]; then
  if [[ ! -d "${SUBSTRATE_DIR}" ]]; then
    echo "ERROR: SUBSTRATE_DIR '${SUBSTRATE_DIR}' not found." >&2
    exit 1
  fi
  echo "--> Building and pushing Agent Substrate images from ${SUBSTRATE_DIR}..."
  TMP_DOCKER_CONFIG="$(mktemp -d)"
  trap 'rm -rf "${TMP_DOCKER_CONFIG}"' EXIT
  printf '%s' "${ACCESS_TOKEN}" | "${CONTAINER_ENGINE}" login --authfile="${TMP_DOCKER_CONFIG}/config.json" -u oauth2accesstoken --password-stdin https://gcr.io >/dev/null

  substrate_output="$(
    cd "${SUBSTRATE_DIR}" && \
    DOCKER_CONFIG="${TMP_DOCKER_CONFIG}" \
    PROJECT_ID="${GOOGLE_CLOUD_PROJECT}" \
    KO_DOCKER_REPO="gcr.io/${GOOGLE_CLOUD_PROJECT}/ate-images" \
    SKIP_IMAGES="${SKIP_IMAGES}" \
    make build-images | tee /dev/stderr
  )"
  ATEOM_SHA="$(echo "${substrate_output}" | grep "ateom-gvisor" | grep -Eo "sha256:[a-f0-9]{64}" | tail -n1 || true)"
  if [[ -z "${ATEOM_SHA}" ]]; then
    ATEOM_SHA="$(gcloud container images describe "gcr.io/${GOOGLE_CLOUD_PROJECT}/ate-images/ateom-gvisor:latest" --format="value(image_summary.digest)")"
  fi
  echo "--> New ateom-gvisor digest: ${ATEOM_SHA}"
fi

# 4. Update benchmarks/install.sh (and README.md) with the new digests.
echo "--> Updating ${INSTALL_SCRIPT} with ate-ant@${WORKER_SHA}..."
perl -pi -e "s|(ate-ant\@)sha256:[a-f0-9]{64}|\${1}${WORKER_SHA}|g" "${INSTALL_SCRIPT}"

if [[ -n "${ATEOM_SHA}" ]]; then
  echo "--> Updating ${INSTALL_SCRIPT} with ateom-gvisor@${ATEOM_SHA}..."
  perl -pi -e "s|(ateom-gvisor\@)sha256:[a-f0-9]{64}|\${1}${ATEOM_SHA}|g" "${INSTALL_SCRIPT}"
fi

if [[ "${UPDATE_README}" == "true" && -f "${ROOT_README}" ]]; then
  echo "--> Updating ${ROOT_README} with new image digest(s)..."
  perl -pi -e "s|(ate-ant\@)sha256:[a-f0-9]{64}|\${1}${WORKER_SHA}|g" "${ROOT_README}"
  if [[ -n "${ATEOM_SHA}" ]]; then
    perl -pi -e "s|(ateom-gvisor\@)sha256:[a-f0-9]{64}|\${1}${ATEOM_SHA}|g" "${ROOT_README}"
  fi
fi

echo "=================================================================="
echo "Image Build & Script Update Complete"
echo "  WORKER_IMAGE: gcr.io/\${GOOGLE_CLOUD_PROJECT}/ate-ant@${WORKER_SHA}"
if [[ -n "${ATEOM_SHA}" ]]; then
  echo "  ATEOM_IMAGE:  gcr.io/\${GOOGLE_CLOUD_PROJECT}/ate-images/ateom-gvisor@${ATEOM_SHA}"
fi
echo "=================================================================="
