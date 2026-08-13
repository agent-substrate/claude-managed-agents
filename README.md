# ⚡ Substrate Anthropic Worker

Deploy [Anthropic Managed Agents Self-Hosted Sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) workers on [Agent Substrate](https://github.com/agent-substrate/substrate).

This project enables Claude Managed Agents to run code execution, bash commands, file operations, and skills inside secure, isolated sandboxes running on your Agent Substrate Kubernetes cluster.

## Architecture

```
 ╭─────────────────────────╮
 │ Anthropic Control Plane │ (Claude Managed Agents)
 ╰────────────┬────────────╯
              │ Long-Poll (HTTPS with ANTHROPIC_ENVIRONMENT_KEY)
              ▼
 ╭─────────────────────────╮             ╭─────────────────────────╮
 │ anthropic-worker        │  executes   │ Agent Substrate         │
 │ (ant CLI poller)        ├────────────▶│ WorkerPool              │
 │ (Deployment)            │  in-sandbox │ (ateom-gvisor runtime)  │
 ╰────────────┬────────────╯             ╰────────────┬────────────╯
              │                                       │
              │ tool executions                       ▼
              │ in /workspace            ╭─────────────────────────╮
              ╰─────────────────────────▶│ Isolated Sandbox Guest  │
                                         │ (/workspace + skills)   │
                                         ╰─────────────────────────╯
```

- **`cmd/substrate-anthropic`**: CLI tool to generate Kubernetes and Agent Substrate deployment manifests.
- **`Dockerfile`**: Debian-based container image packaging Anthropic's official prebuilt `ant` CLI worker.
- **`deployment.yaml`**: Complete Kubernetes & Substrate manifest template.

## Prerequisites

1. Kubernetes cluster with [Agent Substrate](https://github.com/agent-substrate/substrate) installed.
2. Anthropic Managed Agents environment credentials:
   - `ANTHROPIC_ENVIRONMENT_ID` (e.g. `env_01abc...`)
   - `ANTHROPIC_ENVIRONMENT_KEY` (e.g. `sk-ant-env...`)
3. An object storage bucket for actor snapshots (e.g. `gs://my-bucket/snapshots/`).
4. [Podman](https://podman.io/) (or Docker) for building container images.

## Quickstart

### 1. Build and Push Container Image

Build the Debian-based container image with the official Anthropic `ant` CLI preinstalled using Podman:

```bash
export ANTHROPIC_WORKER_IMAGE=gcr.io/your-project/anthropic-worker
make image
# push to your registry:
make push
```

### 2. Generate and Apply Deployment Manifests

Using the CLI:

```bash
# Install CLI tool
go install github.com/agent-substrate/substrate-anthropic/cmd/substrate-anthropic@latest

# Generate deployment YAML and apply to Kubernetes
substrate-anthropic deploy \
  --worker-image     gcr.io/your-project/anthropic-worker@sha256:... \
  --ateom-image      gcr.io/dberkov-gke-dev3/ate-images/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:b0b6e2ad834de42cb2a4c55e83b60243f66cb85ca37575d1a6818e788e0564e0 \
  --snapshots-bucket gs://my-substrate-snapshots-bucket/anthropic-worker/ \
  --anthropic-env-id env_01abc... \
  --anthropic-env-key sk-ant-env-... | kubectl apply -f -
```

Alternatively, edit [`deployment.yaml`](deployment.yaml) with your credentials and images, then run:

```bash
kubectl apply -f deployment.yaml
```

### 3. Verify Deployment

Check that the worker pods and Substrate pre-warmed worker pool are running:

```bash
kubectl get pods -n anthropic-worker
```

Check logs of the worker:

```bash
kubectl logs -n anthropic-worker -l app=anthropic-worker -f
```

## Running Locally

To run the containerized worker locally for testing against your Anthropic environment:

```bash
podman run --rm -it \
  -e ANTHROPIC_ENVIRONMENT_ID="env_..." \
  -e ANTHROPIC_ENVIRONMENT_KEY="sk-ant-env-..." \
  gcr.io/your-project/anthropic-worker:latest
```

The `ant` worker will poll Anthropic for claimed sessions and execute tool calls in `/workspace`.

## Testing

Run unit tests:

```bash
make test
```

## Manifest Reference

| Resource | Kind | Purpose |
| --- | --- | --- |
| `anthropic-worker` | `Namespace` | Target Kubernetes namespace |
| `anthropic-environment-creds` | `Secret` | Stores `ANTHROPIC_ENVIRONMENT_ID` and `ANTHROPIC_ENVIRONMENT_KEY` |
| `anthropic-worker-template-workerpool` | `WorkerPool` (`ate.dev/v1alpha1`) | Pre-warmed microVM/gVisor runner pool |
| `anthropic-worker-template` | `ActorTemplate` (`ate.dev/v1alpha1`) | Sandbox container definition and workspace mount |
| `anthropic-worker` | `Deployment` | Always-on Anthropic `ant` worker daemon polling work queue |
| `anthropic-worker` | `Service` | Worker network endpoint |
