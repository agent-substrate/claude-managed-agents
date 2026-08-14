# ⚡ Agent Substrate Anthropic Worker

This project enables Claude Managed Agents to run code execution, bash commands, file operations, and skills inside secure, isolated sandboxes running on your Agent Substrate cluster.

Deploy [Anthropic Managed Agents Self-Hosted Sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) workers on [Agent Substrate](https://github.com/agent-substrate/substrate).

## Overview

The `anthropic-worker` frontend server acts as the bridge between Anthropic's Managed Agents control plane and your Agent Substrate cluster:

1. **Monitors Work Queue**: Inspects Anthropic's environment work queue via `Work.List` without claiming items.
2. **Resumes Actor**: When a session work item is queued, the frontend server calls Substrate's `ateapi` to resume (or create) the isolated sandbox `Actor` for that session.
3. **Sandbox Execution**: Inside the Substrate actor container, `ant beta:worker poll` claims the work item and executes the session tools (bash, file read/write, etc.) in the sandboxed `/workspace`.
4. **Suspends Actor on Completion**: The frontend server monitors the work item / session until execution finishes and the work item is stopped, then calls `ateapi` to suspend the actor. Substrate snapshots the actor's memory/filesystem to Cloud Storage and releases the worker pool pod back to the cluster.

```
              ╭─────────────────────────────────╮
              │      Claude Managed Agents      │
              ╰────────────────▲────────────────╯
                                │ Work.List & Session status
                                │ (ANTHROPIC_API_KEY)
                                │
╭───────────────────────────────┴─────────────────────────────────╮
│             Agent Substrate on Kubernetes Cluster               │
│                                                                 │
│  ╭──────────────────────────╮       ╭────────────────────────╮  │
│  │ anthropic-worker         │       │ Substrate WorkerPool   │  │
│  │ (Frontend Server)        │       │ (ateom-gvisor runtime) │  │
│  │                          │       ╰───────────┬────────────╯  │
│  │ 1. Lists queued work     │                   │               │
│  │ 2. Resumes session actor ├─ateapi (gRPC)────▶│ Suspends/     │
│  │ 3. Monitors work status  │                   │ Resumes       │
│  │ 4. Suspends actor on end ├─ateapi (gRPC)────▶│               │
│  ╰──────────────────────────╯                   ▼               │
│                                        ╭─────────────────────╮  │
│                                        │ Substrate Actor     │  │
│                                        │ (ant beta:worker)   │  │
│                                        │ (/workspace)        │  │
│                                        ╰─────────────────────╯  │
╰─────────────────────────────────────────────────────────────────╯
```

## Prerequisites

1. Kubernetes cluster with [Agent Substrate](https://github.com/agent-substrate/substrate) installed.
2. Anthropic API credentials:
   - `ANTHROPIC_API_KEY` (e.g. `sk-ant-api...`)
   - `ANTHROPIC_ENVIRONMENT_ID` (e.g. `env_01abc...`)
   - `ANTHROPIC_ENVIRONMENT_KEY` (optional, e.g. `sk-ant-oat...`)
3. A Google Cloud project provided by `GOOGLE_CLOUD_PROJECT` environment variable.
4. An object storage bucket for actor snapshots (e.g. `gs://$GOOGLE_CLOUD_PROJECT/anthropic-worker/`).
5. [Podman](https://podman.io/) (or Docker) for building container images.

## Quickstart

### 1. Build and Push Container Image

Build the container image with the frontend worker server using Podman (or Docker):

```bash
export GOOGLE_CLOUD_PROJECT="your-gcp-project"
make push
```

### 2. Generate and Apply Deployment Manifests

Using the CLI:

```bash
# Install CLI tool
go install github.com/agent-substrate/anthropic/cmd/ate-ant@latest

# Generate deployment YAML and apply to Kubernetes
ate-ant manifest \
  --worker-image      gcr.io/dberkov-gke-dev3/anthropic-worker@sha256:efa8f019e8f3b005a572f25cc665ed64ab7c934770acac736d0b6ff6ad73a3b1 \
  --ateom-image       gcr.io/$GOOGLE_CLOUD_PROJECT/ate-images/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:b0b6e2ad834de42cb2a4c55e83b60243f66cb85ca37575d1a6818e788e0564e0 \
  --snapshots-bucket  gs://$GOOGLE_CLOUD_PROJECT/anthropic-worker/ \
  --anthropic-api-key $ANTHROPIC_API_KEY \
  --anthropic-env-id  $ANTHROPIC_ENVIRONMENT_ID \
  --anthropic-env-key $ANTHROPIC_ENVIRONMENT_KEY | kubectl apply -f -
```

### 3. Verify Deployment

Check that the worker frontend server and Substrate pre-warmed worker pool are running:

```bash
kubectl get pods -n anthropic-worker
```

Check logs of the frontend worker server:

```bash
kubectl logs -n anthropic-worker -l app=anthropic-worker -f
```

## Example Agent

Once deployed, send tasks to Claude referencing your self-hosted `ANTHROPIC_ENVIRONMENT_ID`. The worker in your Kubernetes cluster will automatically claim the work, resume the sandbox actor, execute tools inside the sandbox, and suspend the actor when idle.

```bash
# 1. Set environment variables
export ANTHROPIC_API_KEY="sk-ant-api..."
export ANTHROPIC_ENVIRONMENT_ID="env_..."

# 2. Run example
go run ./examples/agent \
  --prompt "Run 'uname -a' in bash and show the output."
```

## Running Locally

To run the frontend worker server locally for development or testing:

```bash
# Port-forward ateapi if connecting to a remote Substrate cluster
kubectl port-forward svc/api 8443:443 -n ate-system &

# Run worker server
ate-ant worker \
  --anthropic-api-key $ANTHROPIC_API_KEY \
  --anthropic-env-id  $ANTHROPIC_ENVIRONMENT_ID \
  --anthropic-env-key $ANTHROPIC_ENVIRONMENT_KEY \
  --ateapi-addr       localhost:8443
```

## Manifest Reference

| Resource | Kind | Purpose |
| --- | --- | --- |
| `anthropic-worker` | `Namespace` | Target Kubernetes namespace |
| `anthropic-environment-creds` | `Secret` | Stores `ANTHROPIC_API_KEY` and `ANTHROPIC_ENVIRONMENT_ID` |
| `anthropic-worker-template-workerpool` | `WorkerPool` (`ate.dev/v1alpha1`) | Pre-warmed microVM/gVisor runner pool |
| `anthropic-worker-template` | `ActorTemplate` (`ate.dev/v1alpha1`) | Sandbox container definition (`ant beta:worker poll`) and workspace snapshot storage |
| `anthropic-worker` | `Deployment` | Always-on frontend server polling work queue, resuming/suspending session actors |
