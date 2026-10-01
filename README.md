# Claude Managed Agents on Agent Substrate

Deploy [Anthropic Managed Agents Self-Hosted Sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) workers on [Agent Substrate](https://github.com/agent-substrate/substrate).

This project runs Claude Managed Agents work (e.g. tool calls like bash, file operations, skills) inside isolated Agent Substrate gVisor/microVM actors with durable orchestration.

## Overview

```
┌────────────────────────┐  Poll work items
│ Claude Managed Agents  │ ◄─────────────────────────────────────────────┐
└────────────────────────┘                                               │
                                                                         ▼
                                                     ┌───────────────────────────────────────┐
                                                     │ Controller (`ate-ant controller`)     │
                                                     │                                       │
                                                     │  ┌─────────────────────────────────┐  │
                                                     │  │ Poller loop                     │  │
                                                     │  └────────────────┬────────────────┘  │
                                                     └───────────────────┼───────────────────┘
                                                       Enqueue (resume)  │
                                                                         ▼
┌────────────────────────┐   Consume event           ┌───┬───┬───┬────────────┐
│ Controller (`ate-ant   │ ◄──────────────────────── │   │   │   │ Event Queue│
│      controller`)      │                           └───┴───┴───┴────────────┘
└───────────┬────────────┘
            │
            ├── Notify (POST /work) ───────► ┌────────────────────────┐
            │                                │  Actor (`ate-ant run`) │
            └── GC & Suspend (idle > 30s) ─► └────────────────────────┘
```

### Components

- **Controller**: A horizontally scalable daemon running `ate-ant controller`. Each replica runs an Anthropic poller loop and an event reconcile loop. Poller loops claim and acknowledge work items from the Anthropic API and enqueue them into Valkey. Controller loops consume lifecycle events from the queue (distributed among replicas via consumer groups), manage Substrate actor lifecycles (creating, resuming, suspending), and notify actors of work payloads via `atenet-router`.
- **Queue**: An in-cluster Valkey/Redis instance buffering session lifecycle events, providing durable work distribution and decoupling ingestion from execution.
- **Actors**: Dedicated, isolated environments assigned to each agent session running `ate-ant run`. They execute tools (bash commands, file operations, skills) with the Anthropic SDK's environment worker in-process and preserve session state across turns via snapshot persistence.

See [DESIGN.md](DESIGN.md) for detailed architecture, event queuing, and actor lifecycle workflows.

## Quickstart

### Prerequisites

1. An [Agent Substrate](https://github.com/agent-substrate/substrate) cluster.
2. Anthropic credentials: `ANTHROPIC_API_KEY`, `ANTHROPIC_ENVIRONMENT_ID`, and optional `ANTHROPIC_ENVIRONMENT_KEY` (see [Creating a Self-Hosted Environment](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes#create-an-environment)). To use a custom Anthropic API URL instead of the public API, also set `ANTHROPIC_BASE_URL` (or pass `--anthropic-base-url`).
3. Google Cloud project (`GOOGLE_CLOUD_PROJECT`) and GCS bucket for snapshots (`gs://$GOOGLE_CLOUD_PROJECT/ate-ant/`).
4. Podman (or Docker) for building container images.

### Push Container Image

```bash
export GOOGLE_CLOUD_PROJECT="your-gcp-project"
make push
```

### 1. Apply Kubernetes Manifests

Update the container images before applying the manifests.

> **Note:** Building your own images is temporary; pre-built images will be distributed in an upcoming release.

```bash
go run ./cmd/ate-ant manifest \
  --worker-image gcr.io/$GOOGLE_CLOUD_PROJECT/ate-ant@sha256:e6d05802b220e9c0976d7a0c2639e54cf263f4e1d4d376ce0f70b520664f9a40 \
  --ateom-image  gcr.io/$GOOGLE_CLOUD_PROJECT/ate-images/ateom-gvisor@sha256:7d8878129e707c2842691cdef1f919450985df3f223dbb65361baca79c789e58 | kubectl apply -f -
```

### 2. Register the ActorTemplate in Substrate

Substrate manages ActorTemplates directly in its control plane rather than Kubernetes CRDs. Use `ate-ant manifest template` to generate the Substrate ActorTemplate manifest:

```bash
go run ./cmd/ate-ant manifest template \
  --worker-image     gcr.io/$GOOGLE_CLOUD_PROJECT/ate-ant@sha256:e6d05802b220e9c0976d7a0c2639e54cf263f4e1d4d376ce0f70b520664f9a40 \
  --snapshots-bucket gs://$GOOGLE_CLOUD_PROJECT/ate-ant/ | kubectl-ate create actor-template -f -
```

### Verify Deployment

Check that all worker pool, Valkey, and controller pods are running:

```bash
kubectl get pods -n ate-ant
```

Stream logs from the controller daemon:

```bash
kubectl logs -n ate-ant -l app=ate-ant-controller -f
```

## Example Agent

```bash
# 1. Set environment variables
export ANTHROPIC_API_KEY="sk-ant-api..."
export ANTHROPIC_ENVIRONMENT_ID="env_..."

# 2. Run example with an initial prompt
go run ./examples/agent --prompt "Run 'uname -a' in bash and show the output."
```

## Running Locally

To shell into the container locally with your Anthropic environment variables:

```bash
podman run -it --rm \
  --entrypoint bash \
  -e ANTHROPIC_ENVIRONMENT_ID="$ANTHROPIC_ENVIRONMENT_ID" \
  -e ANTHROPIC_ENVIRONMENT_KEY="$ANTHROPIC_ENVIRONMENT_KEY" \
  gcr.io/$GOOGLE_CLOUD_PROJECT/ate-ant:latest
```

Inside the container:
- **Run worker manually**: `ant beta:worker run --work-id "<work-id>" --session-id "<session-id>"`

## Roadmap

- [ ] **Distribute prebuilt images** so deploying is one `kubectl apply` with no build step.
- [x] **Durable session orchestration** with distributed locking, retries, crash recovery, and auto-ACKing.
- [x] **Enable garbage collection** to automatically suspend idle actors.
