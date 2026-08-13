# ⚡ Agent Substrate Anthropic Worker

This project enables Claude Managed Agents to run code execution, bash commands, file operations, and skills inside secure, isolated sandboxes running on your Agent Substrate cluster.

Deploy [Anthropic Managed Agents Self-Hosted Sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) workers on [Agent Substrate](https://github.com/agent-substrate/substrate).

## Overview

The `anthropic-worker` frontend server acts as the bridge between Anthropic's Managed Agents control plane and your Agent Substrate cluster:

1. **Polls**: Long-polls the self-hosted environment queue for claimed work items.
2. **Resumes Actor**: When a session work item is claimed, the frontend server contacts Substrate's `ateapi` to resume (or create) the isolated sandbox `Actor` for that session.
3. **Executes Session Worker**: Runs the Anthropic tool execution loop against the active sandbox.
4. **Suspends Actor**: Once the turn or session work finishes, the frontend server suspends the actor, taking a snapshot of its memory/filesystem to Cloud Storage and releasing the worker pool pod back to the cluster.

```
              ╭─────────────────────────────────╮
              │      Claude Managed Agents      │
              ╰────────────────▲────────────────╯
                                │ Polls with
                                │ ANTHROPIC_ENVIRONMENT_ID,
                                │ ANTHROPIC_ENVIRONMENT_KEY
                                │
╭───────────────────────────────┴─────────────────────────────────╮
│             Agent Substrate on Kubernetes Cluster               │
│                                                                 │
│  ╭──────────────────────────╮       ╭────────────────────────╮  │
│  │ anthropic-worker         │       │ Substrate WorkerPool   │  │
│  │ (Frontend Server)        │       │ (ateom-gvisor runtime) │  │
│  │                          │       ╰───────────┬────────────╯  │
│  │ 1. Polls work item       │                   │               │
│  │ 2. Resumes session actor ├─ateapi (gRPC)────▶│ Suspends/     │
│  │ 3. Executes tool runner  │                   │ Resumes       │
│  │ 4. Suspends actor on end ├─ateapi (gRPC)────▶│               │
│  ╰──────────────────────────╯                   ▼               │
│                                        ╭─────────────────────╮  │
│                                        │ Substrate Actor     │  │
│                                        │ (session sandbox)   │  │
│                                        │ (/workspace)        │  │
│                                        ╰─────────────────────╯  │
╰─────────────────────────────────────────────────────────────────╯
```

## Prerequisites

1. Kubernetes cluster with [Agent Substrate](https://github.com/agent-substrate/substrate) installed.
2. Anthropic Managed Agents environment credentials:
   - `ANTHROPIC_ENVIRONMENT_ID` (e.g. `env_01abc...`)
   - `ANTHROPIC_ENVIRONMENT_KEY` (e.g. `sk-ant-env...`)
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
  --worker-image      gcr.io/$GOOGLE_CLOUD_PROJECT/anthropic-worker@sha256:1dc73cf7abaf091cfb20c88cd6111d5b35692869b81f1a1f08162c6ac07ad6d8 \
  --ateom-image       gcr.io/$GOOGLE_CLOUD_PROJECT/ate-images/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:b0b6e2ad834de42cb2a4c55e83b60243f66cb85ca37575d1a6818e788e0564e0 \
  --snapshots-bucket  gs://$GOOGLE_CLOUD_PROJECT/anthropic-worker/ \
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

Once deployed, send tasks to Claude referencing your self-hosted `ANTHROPIC_ENVIRONMENT_ID`. The worker in your Kubernetes cluster will automatically claim the work, resume the sandbox actor, execute tools, and suspend the actor when idle.

```bash
# 1. Set environment variables
export ANTHROPIC_API_KEY="sk-ant-api..."
export ANTHROPIC_ENVIRONMENT_ID="env_..."

# 2. Run example.
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
  --anthropic-env-id $ANTHROPIC_ENVIRONMENT_ID \
  --anthropic-env-key $ANTHROPIC_ENVIRONMENT_KEY \
  --ateapi-addr localhost:8443
```

## Manifest Reference

| Resource | Kind | Purpose |
| --- | --- | --- |
| `anthropic-worker` | `Namespace` | Target Kubernetes namespace |
| `anthropic-environment-creds` | `Secret` | Stores `ANTHROPIC_ENVIRONMENT_ID` and `ANTHROPIC_ENVIRONMENT_KEY` |
| `anthropic-worker-template-workerpool` | `WorkerPool` (`ate.dev/v1alpha1`) | Pre-warmed microVM/gVisor runner pool |
| `anthropic-worker-template` | `ActorTemplate` (`ate.dev/v1alpha1`) | Sandbox container definition and workspace snapshot storage |
| `anthropic-worker` | `Deployment` | Always-on frontend server polling work queue, resuming/suspending session actors |
