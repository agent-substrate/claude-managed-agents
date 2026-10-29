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

# Builds the Agent Substrate gVisor worker image (ateworker-gvisor) with ko,
# pushes it to ATE_IMAGES_REPO, and prints the digest-pinned image to stdout.
# Progress is logged to stderr.
#
# Unless SUBSTRATE_DIR is set, the worker is built from a git worktree of
# SUBSTRATE_REPO checked out at the go.ate.dev/substrate version pinned in
# go.mod, so it matches the API this repo is built against. (The Go module cache
# cannot be used: it omits the nested hack/tools module ate-setup needs to
# locate ko.)

set -o errexit -o nounset -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

: "${GOOGLE_CLOUD_PROJECT:?GOOGLE_CLOUD_PROJECT must be set}"
PLATFORM="${PLATFORM:-linux/amd64}"
SUBSTRATE_REPO="${SUBSTRATE_REPO:-${HOME}/substrate}"
SUBSTRATE_DIR="${SUBSTRATE_DIR:-}"
ATE_IMAGES_REPO="${ATE_IMAGES_REPO:-gcr.io/${GOOGLE_CLOUD_PROJECT}/ate-images}"

if [[ -z "${SUBSTRATE_DIR}" ]]; then
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
    echo "--> Creating Agent Substrate worktree at ${SUBSTRATE_REV} in ${SUBSTRATE_DIR}..." >&2
    git -C "${SUBSTRATE_REPO}" fetch --tags origin >&2
    mkdir -p "$(dirname "${SUBSTRATE_DIR}")"
    git -C "${SUBSTRATE_REPO}" worktree add --detach "${SUBSTRATE_DIR}" "${SUBSTRATE_REV}" >&2
  fi
fi

if [[ ! -d "${SUBSTRATE_DIR}" ]]; then
  echo "ERROR: SUBSTRATE_DIR '${SUBSTRATE_DIR}' not found." >&2
  exit 1
fi

echo "--> Building and pushing Agent Substrate worker images from ${SUBSTRATE_DIR}..." >&2
substrate_output="$(
  cd "${SUBSTRATE_DIR}" && \
  go run ./cmd/ate-setup publish worker-images \
    --no-dev-env \
    --ko-docker-repo="${ATE_IMAGES_REPO}" \
    --ko-default-platforms="${PLATFORM}" | tee /dev/stderr
)"
ATEWORKER_SHA="$(echo "${substrate_output}" | grep "^ateworker-gvisor:" | grep -Eo "sha256:[a-f0-9]{64}" | tail -n1 || true)"
if [[ -z "${ATEWORKER_SHA}" ]]; then
  echo "ERROR: Could not determine sha256 digest for ateworker-gvisor" >&2
  exit 1
fi

echo "${ATE_IMAGES_REPO}/ateworker-gvisor@${ATEWORKER_SHA}"
