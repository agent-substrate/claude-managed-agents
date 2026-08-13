# ⚡ Substrate Anthropic Worker

Deploy [Anthropic Managed Agents Self-Hosted Sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) workers on [Agent Substrate](https://github.com/agent-substrate/substrate).

This project enables Claude Managed Agents to run code execution, bash commands, file operations, and custom tools inside secure, isolated sandboxes running on your Agent Substrate Kubernetes cluster.

## Architecture

```
 ╭─────────────────────────╮
 │ Anthropic Control Plane │ (Claude Managed Agents)
 ╰────────────┬────────────╯
              │ Poll / Webhook (HTTPS with ANTHROPIC_ENVIRONMENT_KEY)
              ▼
 ╭─────────────────────────╮             ╭─────────────────────────╮
 │ substrate-anthropic-    │  launches   │ Agent Substrate         │
 │ worker                  ├────────────▶│ WorkerPool              │
 │ (always-on daemon)      │  in-sandbox │ (ateom-gvisor runtime)  │
 ╰────────────┬────────────╯             ╰────────────┬────────────╯
              │                                       │
              │ executes tools                        ▼
              │ in /workspace            ╭─────────────────────────╮
              ╰─────────────────────────▶│ Isolated Guest Sandbox │
                                         │ (/workspace + skills)   │
                                         ╰─────────────────────────╯
```

- **`cmd/substrate-anthropic`**: CLI tool to generate Kubernetes and Substrate deployment manifests.
- **`cmd/substrate-anthropic-worker`**: Worker daemon that polls Anthropic's queue, handles tool execution (`agent_toolset_20260401`), and serves health probes.
- **`internal/worker`**: Go implementation using `github.com/anthropics/anthropic-sdk-go/lib/environments` and `agenttoolset`.
- **`internal/deploy`**: Programmatic Kubernetes and Substrate manifest generator (`Namespace`, `Secret`, `WorkerPool`, `ActorTemplate`, `Deployment`, `Service`).

## Prerequisites

1. Kubernetes cluster with [Agent Substrate](https://github.com/agent-substrate/substrate) installed.
2. Anthropic Managed Agents environment credentials:
   - `ANTHROPIC_ENVIRONMENT_ID` (e.g. `env_01abc...`)
   - `ANTHROPIC_ENVIRONMENT_KEY` (e.g. `sk-ant-env...`)
3. A cloud storage bucket for actor snapshots (e.g. `gs://my-bucket/snapshots/`).
4. [ko](https://github.com/ko-build/ko) or Docker for building container images.

## Quickstart

### 1. Build and Push Container Images

Set your container repository and build the worker image with `ko`:

```bash
export SUBSTRATE_ANTHROPIC_IMAGE_REPO=us-docker.pkg.dev/your-project/substrate-anthropic
make images
```

### 2. Generate and Apply Deployment Manifests

Using the CLI:

```bash
# Install CLI tool
go install ./cmd/substrate-anthropic

# Generate deployment YAML and apply to Kubernetes
substrate-anthropic deploy \
  --worker-image    us-docker.pkg.dev/your-project/substrate-anthropic/substrate-anthropic-worker@sha256:... \
  --ateom-image     gcr.io/dberkov-gke-dev3/ate-images/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:b0b6e2ad834de42cb2a4c55e83b60243f66cb85ca37575d1a6818e788e0564e0 \
  --snapshots-bucket gs://my-substrate-snapshots-bucket/substrate-anthropic/ \
  --env-id          env_01abc... \
  --env-key         sk-ant-env-... | kubectl apply -f -
```

Alternatively, edit `deployment.yaml` with your credentials and images, then run:

```bash
kubectl apply -f deployment.yaml
```

### 3. Verify Deployment

Check that the worker pods and Substrate pre-warmed worker pool are running:

```bash
kubectl get pods -n substrate-anthropic
```

Check logs of the worker:

```bash
kubectl logs -n substrate-anthropic -l app=substrate-anthropic-worker -f
```

## Running Locally

To run the worker locally for testing against your Anthropic environment:

```bash
export ANTHROPIC_ENVIRONMENT_ID="env_..."
export ANTHROPIC_ENVIRONMENT_KEY="sk-ant-env-..."
export WORK_DIR="/tmp/workspace"

go run ./cmd/substrate-anthropic-worker
```

The worker will expose `/healthz` and `/readyz` on `http://127.0.0.1:8080` while polling Anthropic for sessions.

## Testing

Run unit tests:

```bash
make test
```

## Manifest Reference

| Resource | Kind | Purpose |
| --- | --- | --- |
| `substrate-anthropic` | `Namespace` | Target Kubernetes namespace |
| `anthropic-environment-creds` | `Secret` | Stores `ANTHROPIC_ENVIRONMENT_ID` and `ANTHROPIC_ENVIRONMENT_KEY` |
| `substrate-anthropic-workerpool` | `WorkerPool` (`ate.dev/v1alpha1`) | Pre-warmed microVM/gVisor runner pool |
| `substrate-anthropic-template` | `ActorTemplate` (`ate.dev/v1alpha1`) | Sandbox container definition and `/readyz` probe |
| `substrate-anthropic-worker` | `Deployment` | Always-on worker daemon polling Anthropic queue |
| `substrate-anthropic-worker` | `Service` | HTTP health/metrics endpoint |
