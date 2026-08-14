# ⚡ Agent Substrate Anthropic Worker

This project enables Claude Managed Agents to run code execution, bash commands, file operations, and skills inside secure, isolated sandboxes running on your Agent Substrate cluster.

Deploy [Anthropic Managed Agents Self-Hosted Sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) workers on [Agent Substrate](https://github.com/agent-substrate/substrate).

## Overview

The `anthropic-worker` frontend server acts as the bridge between Anthropic's Managed Agents control plane and your Agent Substrate cluster:

1. **Monitors Work Queue**: Inspects Anthropic's environment work queue via `Work.List`.
2. **Resumes Actor**: When a session work item is queued, the controller calls Substrate's `ateapi` to resume (or create) the isolated sandbox `Actor` for that session.
3. **Sandbox Execution**: Inside the Substrate actor container, `ate-ant waker` receives the work item via HTTP `/start` and executes `ant beta:worker run --work-id <id> --session-id <id>` in the sandboxed `/workspace`.
4. **Suspends Actor on Turn Completion**: When Claude finishes its turn, `ant beta:worker run` exits. The controller detects the idle state via `/healthz` and calls `ateapi` to suspend the actor. Substrate snapshots the actor's memory/filesystem to Cloud Storage and releases the worker pool pod back to the cluster.

```
              ╭─────────────────────────────────╮
              │      Claude Managed Agents      │
              ╰─────────────────▲───────────────╯
                                │ Work.List & Session status
                                │ (ANTHROPIC_API_KEY)
╭───────────────────────────────┴─────────────────────────────────╮
│             Agent Substrate on Kubernetes Cluster               │
│                                                                 │
│  ╭──────────────────────────╮       ╭────────────────────────╮  │
│  │ anthropic-worker         │       │ Substrate WorkerPool   │  │
│  │ (Frontend Server)        │       │ (ateom-gvisor runtime) │  │
│  │                          │       ╰───────────┬────────────╯  │
│  │ 1. Lists queued work     │                   │               │
│  │ 2. Resumes session actor ├─ateapi (gRPC)────▶│ Suspends/     │
│  │ 3. Notifies actor session├─/start (HTTP)────▶│ Resumes       │
│  │ 4. Suspends actor on idle├─ateapi (gRPC)────▶│               │
│  ╰──────────────────────────╯                   ▼               │
│                                        ╭─────────────────────╮  │
│                                        │ Substrate Actor     │  │
│                                        │ (ate-ant waker)     │  │
│                                        │ ant beta:worker run │  │
│                                        │ (/workspace)        │  │
│                                        ╰─────────────────────╯  │
╰─────────────────────────────────────────────────────────────────╯
```

## CLI Commands

`ate-ant` provides three subcommands:

1. **`ate-ant manifest`**: Generates Kubernetes deployment YAML for the worker pool, sandbox template, and poller Deployment.
2. **`ate-ant dispatch`**: Hook target for **`ant beta:worker poll --on-work 'ate-ant dispatch'`** (wakes actor $\rightarrow$ executes turn $\rightarrow$ suspends actor).
3. **`ate-ant waker`**: The in-sandbox HTTP daemon running inside each Substrate actor pod supervising `ant beta:worker run`.

---

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

Build the container image with the worker CLI using Podman (or Docker):

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
  --worker-image      gcr.io/dberkov-gke-dev3/anthropic-worker@sha256:73b3f624137c4334c5f351799ec8d943a60beea7b037a958aca9858bb44852d1 \
  --ateom-image       gcr.io/$GOOGLE_CLOUD_PROJECT/ate-images/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:b0b6e2ad834de42cb2a4c55e83b60243f66cb85ca37575d1a6818e788e0564e0 \
  --snapshots-bucket  gs://$GOOGLE_CLOUD_PROJECT/anthropic-worker/ \
  --anthropic-api-key "$ANTHROPIC_API_KEY" \
  --anthropic-env-id  "$ANTHROPIC_ENVIRONMENT_ID" \
  --anthropic-env-key "$ANTHROPIC_ENVIRONMENT_KEY" | kubectl apply -f -
```

### 3. Verify Deployment

Check that the worker poller Deployment and Substrate pre-warmed worker pool are running:

```bash
kubectl get pods -n anthropic-worker
```

Check logs of the worker poller Deployment:

```bash
kubectl logs -n anthropic-worker -l app=anthropic-worker -f
```

## Example Agent

Once deployed, send tasks to Claude referencing your self-hosted `ANTHROPIC_ENVIRONMENT_ID`. The cluster will automatically claim the work, resume the sandbox actor, execute tools inside the sandbox, and suspend the actor when idle.

```bash
# 1. Set environment variables
export ANTHROPIC_API_KEY="sk-ant-api..."
export ANTHROPIC_ENVIRONMENT_ID="env_..."

# 2. Run example
go run ./examples/agent \
  --prompt "Run 'uname -a' in bash and show the output."
```

## Running Locally

To run the poller and dispatcher locally for development or testing:

```bash
# 1. Port-forward ateapi and router
kubectl port-forward svc/api 8443:443 -n ate-system &
kubectl port-forward svc/atenet-router 8080:80 -n ate-system &

# 2. Run poller with ate-ant dispatch
ant beta:worker poll \
  --environment-id "$ANTHROPIC_ENVIRONMENT_ID" \
  --environment-key "$ANTHROPIC_ENVIRONMENT_KEY" \
  --on-work "ate-ant dispatch --ateapi-addr localhost:8443 --atenet-router-addr localhost:8080 --insecure"
```

## Manifest Reference

| Resource | Kind | Purpose |
| --- | --- | --- |
| `anthropic-worker` | `Namespace` | Target Kubernetes namespace |
| `anthropic-environment-creds` | `Secret` | Stores `ANTHROPIC_API_KEY`, `ANTHROPIC_ENVIRONMENT_ID`, and `ANTHROPIC_ENVIRONMENT_KEY` |
| `anthropic-worker-template-workerpool` | `WorkerPool` (`ate.dev/v1alpha1`) | Pre-warmed microVM/gVisor runner pool (50 replicas) |
| `anthropic-worker-template` | `ActorTemplate` (`ate.dev/v1alpha1`) | Sandbox container definition (`ate-ant waker`) and workspace snapshot storage |
| `anthropic-worker` | `Deployment` | Always-on poller running `ant beta:worker poll --on-work "ate-ant dispatch"` |
