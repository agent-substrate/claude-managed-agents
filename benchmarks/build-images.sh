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
#
# Images built:
#   - ate-ant           (this repo, via `make push`)
#   - ateworker-gvisor  (Agent Substrate gVisor worker, via `ate-setup publish worker-images`)
#
# The Agent Substrate control plane images (ateapi, atecontroller, atelet, ...)
# are built and deployed by `ate-setup deploy ate-system`; see install.sh.

set -o errexit -o nounset -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

GOOGLE_CLOUD_PROJECT="${GOOGLE_CLOUD_PROJECT:-kubecon-na-26-keynote-demo}"
CONTAINER_ENGINE="${CONTAINER_ENGINE:-podman}"
PLATFORM="${PLATFORM:-linux/amd64}"

# Rebuilds the Agent Substrate worker image (ateworker-gvisor) unless
# BUILD_SUBSTRATE_IMAGES=false. Unless SUBSTRATE_DIR is set, the worker is built
# from a git worktree of SUBSTRATE_REPO checked out at the go.ate.dev/substrate
# version pinned in go.mod, so it matches the API this repo is built against.
# (The Go module cache cannot be used: it omits the nested hack/tools module
# ate-setup needs to locate ko.)
BUILD_SUBSTRATE_IMAGES="${BUILD_SUBSTRATE_IMAGES:-true}"
SUBSTRATE_REPO="${SUBSTRATE_REPO:-${HOME}/substrate}"
SUBSTRATE_DIR="${SUBSTRATE_DIR:-}"
ATE_IMAGE_REPO="${ATE_IMAGE_REPO:-gcr.io/${GOOGLE_CLOUD_PROJECT}/ate-images}"
UPDATE_README="${UPDATE_README:-true}"

INSTALL_SCRIPT="${ROOT_DIR}/benchmarks/install.sh"
ROOT_README="${ROOT_DIR}/README.md"

if [[ "${BUILD_SUBSTRATE_IMAGES}" == "true" && -z "${SUBSTRATE_DIR}" ]]; then
  SUBSTRATE_VERSION="$(go list -m -f '{{.Version}}' go.ate.dev/substrate)"
  # Pseudo-versions (v0.5.1-0.20261022032710-ab4f3dbec79b) end in a commit hash;
  # anything else is a tag.
  if [[ "${SUBSTRATE_VERSION}" =~ [.-][0-9]{14}-([0-9a-f]{12})$ ]]; then
    SUBSTRATE_REV="${BASH_REMATCH[1]}"
  else
    SUBSTRATE_REV="${SUBSTRATE_VERSION}"
  fi
  SUBSTRATE_DIR="${HOME}/.cache/ate-ant/substrate-${SUBSTRATE_REV}"
  if [[ ! -d "${SUBSTRATE_DIR}" ]]; then
    echo "--> Creating Agent Substrate worktree at ${SUBSTRATE_REV} in ${SUBSTRATE_DIR}..."
    git -C "${SUBSTRATE_REPO}" fetch --tags origin
    mkdir -p "$(dirname "${SUBSTRATE_DIR}")"
    git -C "${SUBSTRATE_REPO}" worktree add --detach "${SUBSTRATE_DIR}" "${SUBSTRATE_REV}"
  fi
fi

echo "=================================================================="
echo "Building and Pushing Benchmark Images"
echo "  Project:                ${GOOGLE_CLOUD_PROJECT}"
echo "  Container Engine:       ${CONTAINER_ENGINE} (${PLATFORM})"
echo "  Build Substrate Images: ${BUILD_SUBSTRATE_IMAGES} (${SUBSTRATE_DIR})"
echo "  Substrate Image Repo:   ${ATE_IMAGE_REPO}"
echo "=================================================================="

# 1. Ensure gcr.io Artifact Registry repository exists.
if ! gcloud artifacts repositories describe gcr.io --location=us --project="${GOOGLE_CLOUD_PROJECT}" >/dev/null 2>&1; then
  echo "--> Creating gcr.io Artifact Registry repository in ${GOOGLE_CLOUD_PROJECT}..."
  gcloud artifacts repositories create gcr.io \
    --repository-format=docker \
    --location=us \
    --project="${GOOGLE_CLOUD_PROJECT}"
fi

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

# 3. Optionally rebuild the Agent Substrate gVisor worker image with ko.
ATEWORKER_SHA=""
if [[ "${BUILD_SUBSTRATE_IMAGES}" == "true" ]]; then
  if [[ ! -d "${SUBSTRATE_DIR}" ]]; then
    echo "ERROR: SUBSTRATE_DIR '${SUBSTRATE_DIR}' not found." >&2
    exit 1
  fi
  echo "--> Building and pushing Agent Substrate worker images from ${SUBSTRATE_DIR}..."
  substrate_output="$(
    cd "${SUBSTRATE_DIR}" && \
    go run ./cmd/ate-setup publish worker-images \
      --no-dev-env \
      --ko-docker-repo="${ATE_IMAGE_REPO}" \
      --ko-default-platforms="${PLATFORM}" | tee /dev/stderr
  )"
  ATEWORKER_SHA="$(echo "${substrate_output}" | grep "^ateworker-gvisor:" | grep -Eo "sha256:[a-f0-9]{64}" | tail -n1 || true)"
  if [[ -z "${ATEWORKER_SHA}" ]]; then
    echo "ERROR: Could not determine sha256 digest for ateworker-gvisor" >&2
    exit 1
  fi
  echo "--> New ateworker-gvisor digest: ${ATEWORKER_SHA}"
fi

# 4. Update benchmarks/install.sh (and README.md) with the new digests.
update_digests() {
  local file="$1"
  perl -pi -e "s|(ate-ant\@)sha256:[a-f0-9]{64}|\${1}${WORKER_SHA}|g" "${file}"
  if [[ -n "${ATEWORKER_SHA}" ]]; then
    perl -pi -e "s|(ateworker-gvisor\@)sha256:[a-f0-9]{64}|\${1}${ATEWORKER_SHA}|g" "${file}"
  fi
}

echo "--> Updating ${INSTALL_SCRIPT} with new image digest(s)..."
update_digests "${INSTALL_SCRIPT}"

if [[ "${UPDATE_README}" == "true" && -f "${ROOT_README}" ]]; then
  echo "--> Updating ${ROOT_README} with new image digest(s)..."
  update_digests "${ROOT_README}"
fi

echo "=================================================================="
echo "Image Build & Script Update Complete"
echo "  WORKER_IMAGE: gcr.io/\${GOOGLE_CLOUD_PROJECT}/ate-ant@${WORKER_SHA}"
if [[ -n "${ATEWORKER_SHA}" ]]; then
  echo "  ATEOM_IMAGE:  ${ATE_IMAGE_REPO}/ateworker-gvisor@${ATEWORKER_SHA}"
fi
echo "=================================================================="
