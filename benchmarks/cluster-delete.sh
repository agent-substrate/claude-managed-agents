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

# Tears down the 200-node benchmark GKE cluster and optionally deletes the
# GCS snapshot bucket when PURGE_SNAPSHOTS=true.

set -o errexit -o nounset -o pipefail

GOOGLE_CLOUD_PROJECT="${GOOGLE_CLOUD_PROJECT:-kubecon-na-26-keynote-demo}"
CLUSTER_NAME="${CLUSTER_NAME:-ate-ant-bench}"
ZONE="${ZONE:-us-west1-a}"
SNAPSHOTS_BUCKET="${SNAPSHOTS_BUCKET:-gs://${GOOGLE_CLOUD_PROJECT}-ate-ant-snapshots}"
PURGE_SNAPSHOTS="${PURGE_SNAPSHOTS:-false}"

echo "--> Deleting GKE benchmark cluster '${CLUSTER_NAME}' in ${ZONE}..."
gcloud container clusters delete "${CLUSTER_NAME}" \
  --project="${GOOGLE_CLOUD_PROJECT}" \
  --zone="${ZONE}" \
  --quiet

if [[ "${PURGE_SNAPSHOTS}" == "true" ]]; then
  BUCKET_URI="${SNAPSHOTS_BUCKET%/}"
  echo "--> Purging snapshot bucket ${BUCKET_URI}..."
  gcloud storage rm --recursive "${BUCKET_URI}" --project="${GOOGLE_CLOUD_PROJECT}" || true
fi

echo "Benchmark teardown complete."
