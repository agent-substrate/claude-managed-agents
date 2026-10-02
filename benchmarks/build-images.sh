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
# and pins the new @sha256 digests in benchmarks/install.sh and README.md.
#
# Assumes cluster-create.sh has already created the gcr.io registry.

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

# Files that carry digest-pinned image references.
DIGEST_FILES=("${ROOT_DIR}/benchmarks/install.sh" "${ROOT_DIR}/README.md")

# Prints the sha256 digest of the :latest tag of $1 (e.g. ate-ant) in the project registry.
digest_of() {
  gcloud container images describe "gcr.io/${GOOGLE_CLOUD_PROJECT}/$1:latest" \
    --format="value(image_summary.digest)"
}

# Rewrites every "$1@sha256:..." reference in DIGEST_FILES to digest $2.
pin_digest() {
  echo "--> Pinning $1@$2 in: ${DIGEST_FILES[*]#"${ROOT_DIR}"/}"
  perl -pi -e "s|(\Q$1\E\@)sha256:[a-f0-9]{64}|\${1}$2|g" "${DIGEST_FILES[@]}"
}

echo "=================================================================="
echo "Building and Pushing Benchmark Images"
echo "  Project:                ${GOOGLE_CLOUD_PROJECT}"
echo "  Container Engine:       ${CONTAINER_ENGINE} (${PLATFORM})"
echo "  Build Substrate Images: ${BUILD_SUBSTRATE_IMAGES}"
echo "=================================================================="

# 1. Build and push the ate-ant image (make push authenticates to gcr.io itself).
echo "--> Building and pushing ate-ant image via 'make push'..."
GOOGLE_CLOUD_PROJECT="${GOOGLE_CLOUD_PROJECT}" \
CONTAINER_ENGINE="${CONTAINER_ENGINE}" \
PLATFORM="${PLATFORM}" \
make push
WORKER_SHA="$(digest_of ate-ant)"
pin_digest ate-ant "${WORKER_SHA}"

# 2. Optionally rebuild Agent Substrate images (including ateom-gvisor).
ATEOM_SHA=""
if [[ "${BUILD_SUBSTRATE_IMAGES}" == "true" ]]; then
  if [[ ! -d "${SUBSTRATE_DIR}" ]]; then
    echo "ERROR: SUBSTRATE_DIR '${SUBSTRATE_DIR}' not found." >&2
    exit 1
  fi
  echo "--> Building and pushing Agent Substrate images from ${SUBSTRATE_DIR}..."
  TMP_DOCKER_CONFIG="$(mktemp -d)"
  trap 'rm -rf "${TMP_DOCKER_CONFIG}"' EXIT
  (gcloud auth application-default print-access-token 2>/dev/null || gcloud auth print-access-token) | \
    "${CONTAINER_ENGINE}" login --authfile="${TMP_DOCKER_CONFIG}/config.json" \
      -u oauth2accesstoken --password-stdin https://gcr.io >/dev/null
  (
    cd "${SUBSTRATE_DIR}"
    DOCKER_CONFIG="${TMP_DOCKER_CONFIG}" \
    PROJECT_ID="${GOOGLE_CLOUD_PROJECT}" \
    KO_DOCKER_REPO="gcr.io/${GOOGLE_CLOUD_PROJECT}/ate-images" \
    SKIP_IMAGES="${SKIP_IMAGES}" \
    make build-images
  )
  ATEOM_SHA="$(digest_of ate-images/ateom-gvisor)"
  pin_digest ateom-gvisor "${ATEOM_SHA}"
fi

echo "=================================================================="
echo "Image Build & Script Update Complete"
echo "  WORKER_IMAGE: gcr.io/\${GOOGLE_CLOUD_PROJECT}/ate-ant@${WORKER_SHA}"
if [[ -n "${ATEOM_SHA}" ]]; then
  echo "  ATEOM_IMAGE:  gcr.io/\${GOOGLE_CLOUD_PROJECT}/ate-images/ateom-gvisor@${ATEOM_SHA}"
fi
echo "=================================================================="
