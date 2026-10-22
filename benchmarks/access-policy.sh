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

# Creates or replaces the ate-api access policy of the ate-ant atespace.
# ate-api enforces authorization on every call, so each in-cluster client needs
# a role:
#   - router-client     (controller, loadgen): editor, to create, resume, and
#                                              suspend session actors.
#   - ate-ant-dashboard (dashboard):           viewer, to list actors.
#
# Usage: NAMESPACE=ate-ant ./benchmarks/access-policy.sh

set -o errexit -o nounset -o pipefail

NAMESPACE="${NAMESPACE:-ate-ant}"
KUBECTL_ATE_BIN="${KUBECTL_ATE_BIN:-$(command -v kubectl-ate || echo "${HOME}/go/bin/kubectl-ate")}"

ACCESS_POLICY="$(cat <<EOF
bindings:
- role: editor
  members:
  - user:system:serviceaccount:${NAMESPACE}:router-client
- role: viewer
  members:
  - user:system:serviceaccount:${NAMESPACE}:ate-ant-dashboard
EOF
)"

if ! echo "${ACCESS_POLICY}" | "${KUBECTL_ATE_BIN}" create access-policy -a "${NAMESPACE}" -f - 2>/dev/null; then
  # Updates must carry the existing policy's metadata.uid/version preconditions.
  { echo "${ACCESS_POLICY}"
    "${KUBECTL_ATE_BIN}" get access-policy -a "${NAMESPACE}" -o yaml 2>/dev/null | sed -n '/^metadata:/,/^[^ ]/{/^metadata:/p;/^  /p;}'
  } | "${KUBECTL_ATE_BIN}" update access-policy -a "${NAMESPACE}" -f -
fi
