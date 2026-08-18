# Claude Managed Agents on Agent Substrate

Deploy [Anthropic Managed Agents Self-Hosted Sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) workers on [Agent Substrate](https://github.com/agent-substrate/substrate).

This worker runs Claude Managed Agents tool calls (bash, file operations, skills) inside isolated, gVisor/microVM sandboxes with durable session orchestration and snapshot persistence.

## Overview

```
┌────────────────────────┐  Poll work event  ┌────────────────────────┐
│ Claude Managed Agents  │ ◄──────────────── │ Poller (`ate-ant poll`)│
└────────────────────────┘                   └───────────┬────────────┘
                                                         │ Enqueue (resume)
                                                         ▼
┌────────────────────────┐   Consume event   ┌───┬───┬───┬────────────┐
│ Controller (`ate-ant   │ ────────────────► │   │   │   │ Event Queue│
│      controller`)      │                   └───┴───┴───┴────────────┘
└───────────┬────────────┘
            │
            ├── Notify (POST /work) ───────► ┌────────────────────────┐
            │                                │  Actor (`ate-ant run`) │
            └── GC & Suspend (idle > 30s) ─► └────────────────────────┘
```

### Components

- **Poller**: An always-on daemon running `ate-ant poll`. It polls the Claude Managed Agents API for new work items and enqueues lifecycle events into the queue.
- **Queue**: An in-cluster Valkey/Redis instance buffering session lifecycle events, providing durable work distribution and decoupling ingestion from execution.
- **Controller**: It consumes lifecycle events from the queue, manages Substrate actor lifecycles (creating, resuming, suspending), and notifies actors of work payloads via `atenet-router`.
- **Actors**: Dedicated, isolated environments assigned to each agent session running `ate-ant run`. They execute tools (bash commands, file operations, skills) via `ant beta:worker run` and preserve session state across turns via snapshot persistence.

### Event Queue

The queue uses Valkey Streams (`stream:events`) to buffer session lifecycle events. It decouples fast API polling from sandbox cold starts, serializes concurrent operations per session, and guarantees at-least-once delivery with consumer group acknowledgments (`XACK`):

```
┌──────────────┐  XADD (resume)  ┌───────────────┐  XREADGROUP  ┌────────────────────┐
│ ate-ant poll │ ──────────────► │ Valkey Stream │ ───────────► │ ate-ant controller │
└──────────────┘                 └───────────────┘ ◄── XACK ─── └────────────────────┘
```

### Lifecycle

1. **Enqueue**: The Poller claims work from Anthropic and enqueues a `resume` event to the Valkey queue.
2. **Notify**: The Controller pulls the event, ensures the actor exists and is resumed via the Substrate API (`api.ate-system`), and notifies the actor with the work payload via `POST /work` through `atenet-router`.
3. **Run**:
   - The runner server inside the actor immediately acknowledges `200 OK` (`{"status":"accepted"}`).
   - In the background, it executes `ant beta:worker run` directly.
   - Process execution is serialized so that only one `ant` process runs at a time within the sandbox actor.
   - Once all tool calls for the current turn are finished and the idle timeout expires, `ant` exits.
4. **Garbage Collection & Suspension**: The controller runs a background sweeper (every `--gc-interval`, default 5s) that queries `/idle` and suspends actors once they exceed `--gc-idle-threshold` (default 30s):

### Idleness

```
┌────────────────────┐        GET /idle         ┌─────────────────┐
│ ate-ant controller │ ───────────────────────► │   ate-ant run   │
│ (GC Loop every 5s) │ ◄─────────────────────── │     (Actor)     │
└─────────┬──────────┘   {"idle_duration":"32s"}└─────────────────┘
          │
          │ SuspendActor (idle > 30s)
          ▼
┌────────────────────┐
│  Substrate Control │
│  Plane (`ateapi`)  │
└────────────────────┘
```

Each runner server exposes `GET /idle` returning activity metrics:

```json
{
  "idle_duration": "32.846s",
  "last_active_at": "2026-08-17T03:35:00Z"
}
```

## Quickstart

### 0. Prerequisites

1. Kubernetes cluster with [Agent Substrate](https://github.com/agent-substrate/substrate).
2. Anthropic credentials: `ANTHROPIC_API_KEY`, `ANTHROPIC_ENVIRONMENT_ID`, and optional `ANTHROPIC_ENVIRONMENT_KEY` (see [Creating a Self-Hosted Environment](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes#create-an-environment)).
3. Google Cloud project (`GOOGLE_CLOUD_PROJECT`) and GCS bucket for snapshots (`gs://$GOOGLE_CLOUD_PROJECT/ate-ant/`).
4. Podman (or Docker) for building container images.

### 1. Push Container Image

```bash
export GOOGLE_CLOUD_PROJECT="your-gcp-project"
make push
```

### 2. Apply Manifests

```bash
go run ./cmd/ate-ant manifest \
  --worker-image      gcr.io/dberkov-gke-dev3/ate-ant@sha256:54b88ede8f54f063500bfd048e181a3d3437f1ce2dfba07a61b39cc2c954ef5e \
  --ateom-image       gcr.io/$GOOGLE_CLOUD_PROJECT/ate-images/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:b0b6e2ad834de42cb2a4c55e83b60243f66cb85ca37575d1a6818e788e0564e0 \
  --snapshots-bucket  gs://$GOOGLE_CLOUD_PROJECT/ate-ant/ \
  --anthropic-api-key "$ANTHROPIC_API_KEY" \
  --anthropic-env-id  "$ANTHROPIC_ENVIRONMENT_ID" \
  --anthropic-env-key "$ANTHROPIC_ENVIRONMENT_KEY" | kubectl apply -f -
```

### 3. Verify Deployment

Check that all worker pool, Valkey, poller, and controller pods are running:

```bash
kubectl get pods -n ate-ant
```

Stream logs from the poller daemon:

```bash
kubectl logs -n ate-ant -l app=ate-ant -f
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
  gcr.io/$GOOGLE_CLOUD_PROJECT/ate-ant:latest
```

Inside the container:
- **Run worker manually**: `ant beta:worker run --work-id "<work-id>" --session-id "<session-id>"`

## Manifest Reference

| Resource | Kind | Description |
| --- | --- | --- |
| `ate-ant` | `Namespace` | Target Kubernetes namespace |
| `ate-ant-creds` | `Secret` | Stores Anthropic credentials and Valkey connection string |
| `ate-ant-creds-reader` | `Role` / `RoleBinding` | RBAC allowing `ate-api-server` to resolve credentials for actor resumption |
| `valkey` | `Service` & `StatefulSet` | Single-replica Valkey instance backing the session lifecycle event queue |
| `ate-ant-workerpool` | `WorkerPool` (`ate.dev/v1alpha1`) | Pool of gVisor/microVM runners for sandbox actors (10 replicas) |
| `ate-ant-actor-template` | `ActorTemplate` (`ate.dev/v1alpha1`) | Sandbox container definition (`ate-ant run`) and GCS snapshot location |
| `ate-ant` | `Deployment` | Always-on poller daemon claiming work items from Anthropic |
| `ate-ant-controller` | `Deployment` | Event controller orchestrating actor lifecycle and routing work items (1 replica) |

## Roadmap

- [ ] **Distribute prebuilt images** so deploying is one `kubectl apply` with no build step.
- [x] **Durable session orchestration** with distributed locking, retries, crash recovery, and auto-ACKing.
- [x] **Enable garbage collection** to automatically suspend idle actors.
