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

# Prints the `ate-ant manifest --worker-pools` JSON for the worker nodes that
# match WORKER_NODE_SELECTOR: one WorkerPool per instance type, with one replica
# per node. Each worker requests and is limited to what its node has left after
# the DaemonSet and static pods on it, less a little headroom for DaemonSets
# added later. Only one worker fits per node, and its capacity grows with the
# node.

set -o errexit -o nounset -o pipefail

WORKER_NODE_SELECTOR="${WORKER_NODE_SELECTOR:?WORKER_NODE_SELECTOR must be set, e.g. ate.dev/pool=workers}"
WORKERPOOL_PREFIX="${WORKERPOOL_PREFIX:-ate-ant-workerpool}"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
kubectl get nodes -l "${WORKER_NODE_SELECTOR}" -o json > "${tmp}/nodes.json"
kubectl get pods --all-namespaces --field-selector=status.phase!=Succeeded,status.phase!=Failed -o json > "${tmp}/pods.json"

jq -n --slurpfile nodes "${tmp}/nodes.json" --slurpfile pods "${tmp}/pods.json" \
  --arg selector "${WORKER_NODE_SELECTOR}" --arg prefix "${WORKERPOOL_PREFIX}" '
  def millicores: if endswith("m") then .[:-1] | tonumber else tonumber * 1000 end;
  def bytes:
    capture("^(?<n>[0-9.]+)(?<u>[A-Za-z]*)$") as $q
    | ($q.n | tonumber) * ({"": 1, "k": 1e3, "M": 1e6, "G": 1e9, "Ki": 1024, "Mi": 1048576, "Gi": 1073741824}[$q.u]);
  # A pod'"'"'s request for resource $r as the scheduler counts it: its
  # containers plus sidecar init containers, or its largest other init
  # container if that is bigger, plus the pod overhead.
  def request($r; parse):
    ([.spec.containers[], (.spec.initContainers // [] | .[] | select(.restartPolicy == "Always"))
      | .resources.requests[$r] // "0" | parse] | add // 0) as $run
    | ([.spec.initContainers // [] | .[] | select(.restartPolicy != "Always")
      | .resources.requests[$r] // "0" | parse] | max // 0) as $init
    | ([$run, $init] | max) + (.spec.overhead[$r] // "0" | parse);
  # Requests of the pods every node runs: DaemonSet pods and static pods.
  def overhead($node):
    [$pods[0].items[]
      | select(.spec.nodeName == $node)
      | select(.metadata.ownerReferences // [] | any(.kind == "DaemonSet" or .kind == "Node"))]
    | {cpu: (map(request("cpu"; millicores)) | add // 0), memory: (map(request("memory"; bytes)) | add // 0)};
  ($selector | split(",") | map(split("=") | {(.[0]): .[1]}) | add) as $labels
  | if ($nodes[0].items | length) == 0 then error("no nodes match \($selector)") else . end
  | $nodes[0].items
  | group_by(.metadata.labels["node.kubernetes.io/instance-type"])
  | map(
      .[0] as $n
      | ($n.metadata.labels["node.kubernetes.io/instance-type"]
         // error("node \($n.metadata.name) has no node.kubernetes.io/instance-type label")) as $type
      | overhead($n.metadata.name) as $o
      | {
          name: "\($prefix)-\($type | gsub("[^a-z0-9-]"; "-"))",
          replicas: length,
          node_selector: ($labels + {"node.kubernetes.io/instance-type": $type}),
          cpu_millicores: (($n.status.allocatable.cpu | millicores) - $o.cpu - 100 | floor),
          memory_bytes: (($n.status.allocatable.memory | bytes) - $o.memory - 268435456 | floor)
        })
'
