# Claude Managed Agents on Agent Substrate

Deploy [Anthropic Managed Agents Self-Hosted Sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) workers on [Agent Substrate](https://github.com/agent-substrate/substrate).

This worker runs Claude Managed Agents tool calls (bash, file operations, skills) inside isolated, gVisor/microVM sandboxes with durable session orchestration and cross-turn snapshot persistence.

## Overview

```
Claude Managed Agents ──► Poller (`ate-antworker`)
                                │
                                ▼
                       Queue (`valkey` / Redis)
                                │
                                ▼
                      Consumer (`ate-antworker consumer`)
                                │
                                ▼
                      Substrate Router (`atenet-router`)
                                │
                                ▼
                     Sandbox Actor (`ate-antworker worker`)
                     (gVisor / microVM sandbox on Substrate)
```

### Components

- **Poller**: An always-on daemon running `ate-antworker`. It polls the Claude Managed Agents API for new work items and enqueues lifecycle events into the queue.
- **Queue**: An in-cluster Valkey/Redis instance buffering session lifecycle events, providing durable work distribution and decoupling ingestion from execution.
- **Consumer**: It consumes lifecycle events from the queue, manages Substrate actor lifecycles (creating, resuming, suspending), and dispatches work payloads to actors via `atenet-router`.
- **Sandbox Actors**: Dedicated, isolated environments assigned to each agent session running `ate-antworker worker`. They execute tools (bash commands, file operations, skills) via `ant beta:worker run` and preserve session state across turns via snapshot persistence.

### Lifecycle

1. **Enqueue**: The Poller claims work from Anthropic and enqueues a `resume` event to the Valkey queue.
2. **Dispatch**: The Consumer pulls the event, ensures the actor exists and is resumed via the Substrate API (`api.ate-system`), and dispatches the work payload via `POST /work` through `atenet-router`.
3. **Run**:
   - The worker server inside the sandbox actor immediately acknowledges `200 OK` (`{"status":"accepted"}`).
   - In the background, it executes `ant beta:worker run` directly.
   - Process execution is serialized so that only one `ant` process runs at a time within the sandbox actor.
   - Once all tool calls for the current turn are finished and the idle timeout expires, `ant` exits.
4. **Garbage collect**: When sessions are idle, actors are suspended, checkpointing memory and disk state to GCS for instant resumption on subsequent turns.

### Idle Status (`/idle`)

Each worker server exposes an HTTP `GET /idle` endpoint returning idle metrics:

```json
{
  "idle_duration": "12.401s",
  "last_active_at": "2026-08-17T03:35:00Z"
}
```

## Prerequisites

1. Kubernetes cluster with [Agent Substrate](https://github.com/agent-substrate/substrate).
2. Anthropic API credentials: `ANTHROPIC_API_KEY`, `ANTHROPIC_ENVIRONMENT_ID`, and optional `ANTHROPIC_ENVIRONMENT_KEY`.
3. Google Cloud project (`GOOGLE_CLOUD_PROJECT`) and GCS bucket for snapshots (`gs://$GOOGLE_CLOUD_PROJECT/ate-antworker/`).
4. Podman (or Docker) for building container images.

## Quickstart

### 1. Build and Push Container Image

```bash
export GOOGLE_CLOUD_PROJECT="your-gcp-project"
make push
```

### 2. Generate and Apply Manifests

```bash
go run ./cmd/ate-antworker manifest \
  --worker-image      gcr.io/dberkov-gke-dev3/ate-antworker@sha256:4ac69dab7524458bdd4c01e55d4dff7f26db5c7e6b09e0ee13bf19825482350a \
  --ateom-image       gcr.io/$GOOGLE_CLOUD_PROJECT/ate-images/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:b0b6e2ad834de42cb2a4c55e83b60243f66cb85ca37575d1a6818e788e0564e0 \
  --snapshots-bucket  gs://$GOOGLE_CLOUD_PROJECT/ate-antworker/ \
  --anthropic-api-key "$ANTHROPIC_API_KEY" \
  --anthropic-env-id  "$ANTHROPIC_ENVIRONMENT_ID" \
  --anthropic-env-key "$ANTHROPIC_ENVIRONMENT_KEY" | kubectl apply -f -
```

### 3. Verify Deployment

Check that all worker pool, Valkey, poller, and consumer pods are running:

```bash
kubectl get pods -n ate-antworker
```

Stream logs from the poller daemon:

```bash
kubectl logs -n ate-antworker -l app=ate-antworker -f
```

Stream logs from the consumer daemon:

```bash
kubectl logs -n ate-antworker -l app=ate-antworker-consumer -f
```

## Example Agent

```bash
# 1. Set environment variables
export ANTHROPIC_API_KEY="sk-ant-api..."
export ANTHROPIC_ENVIRONMENT_ID="env_..."

# 2. Run example with an initial prompt
go run ./examples/agent --prompt "Run 'uname -a' in bash and show the output."

# 3. Reuse an existing agent
go run ./examples/agent --agent "agent_..." --prompt "Show the kernel version."
```

## Running Locally

To shell into the container locally with your Anthropic environment variables:

```bash
podman run -it --rm \
  --entrypoint bash \
  -e ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY" \
  -e ANTHROPIC_ENVIRONMENT_ID="$ANTHROPIC_ENVIRONMENT_ID" \
  -e ANTHROPIC_ENVIRONMENT_KEY="$ANTHROPIC_ENVIRONMENT_KEY" \
  gcr.io/$GOOGLE_CLOUD_PROJECT/ate-antworker:latest
```

Inside the container:
- **Run worker manually**: `ant beta:worker run --work-id "<work-id>" --session-id "<session-id>"`

## Manifest Reference

| Resource | Kind | Description |
| --- | --- | --- |
| `ate-antworker` | `Namespace` | Target Kubernetes namespace |
| `ate-antworker-creds` | `Secret` | Stores Anthropic credentials and Valkey connection string |
| `ate-antworker-creds-reader` | `Role` / `RoleBinding` | RBAC allowing `ate-api-server` to resolve credentials for actor resumption |
| `valkey` | `Service` & `StatefulSet` | Single-replica Valkey instance backing the session lifecycle event queue |
| `ate-antworker-workerpool` | `WorkerPool` (`ate.dev/v1alpha1`) | Pool of gVisor/microVM runners for sandbox actors (10 replicas) |
| `ate-antworker-actor-template` | `ActorTemplate` (`ate.dev/v1alpha1`) | Sandbox container definition (`ate-antworker worker`) and GCS snapshot location |
| `ate-antworker` | `Deployment` | Always-on poller daemon claiming work items from Anthropic |
| `ate-antworker-consumer` | `Deployment` | Event consumer orchestrating actor lifecycle and routing work items (1 replica) |

## Roadmap

- [ ] **Distribute prebuilt images** so deploying is one `kubectl apply` with no build step.
- [ ] **Durable session orchestration & buffering** with distributed locking, crash recovery, and auto-ACKing.
- [ ] **Enable garbage collection** to automatically suspend idle actors.
