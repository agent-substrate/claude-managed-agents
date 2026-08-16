# Claude Managed Agents on Agent Substrate

Deploy [Anthropic Managed Agents Self-Hosted Sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) workers on [Agent Substrate](https://github.com/agent-substrate/substrate).

This worker runs Claude Managed Agents tool calls (bash, file operations, skills) inside isolated, gVisor/microVM sandboxes with durable session orchestration and cross-turn snapshot persistence.

## Overview

```
Claude Managed Agents  ──►  Poller (`ant beta:worker poll`)
                               │
                               ▼
                          Valkey Queue (Durable session buffer & lock)
                               │
                               ▼
                      Sandbox Actor (`ant beta:worker run`)
                      (gVisor / microVM sandbox on Substrate)
```

### Components

- **Poller**: An always-on service running `ant beta:worker poll --on-work ate-antworker`. It fetches incoming work items and dispatches them.
- **Durable Buffer**: A durable session queue on top of Valkey Streams. It provides per-session distributed locking, automatic heartbeat renewal, crash reclamation, and dead-letter queueing.
- **Sandbox Actors**: Dedicated, isolated environments assigned to each agent session. They execute tools (bash commands, file operations, skills) and preserve session state across turns via snapshots.

### Durable Buffer

Valkey Streams provides a durable buffer for distributed coordination, locking, and crash recovery.

We serialize runs for the same `session_id`, ensuring multiple turns or concurrent poller claims never execute simultaneously on the same actor.

While a handler is running, we automatically renew its claim and lock. If a runner crashes mid-run, another runner reclaims the pending entry. Events that persistently fail are moved to a dead-letter stream after exceeding max deliveries.

### Heartbeat Interception

When `ant beta:worker poll --on-work ate-antworker` claims a work item, the poller process maintains an upstream heartbeat loop with Anthropic to keep the work lease active for the duration of the hook execution.

Inside the sandbox actor, `ant beta:worker run` also attempts to heartbeat the same work item to Anthropic with `expected_last_heartbeat=NO_HEARTBEAT`. Because the poller has already heartbeated the work item, sending this request upstream would result in a `412 Precondition Failed` conflict error, causing `ant beta:worker run` to terminate prematurely.

The local `ate-antworker worker` HTTP server intercepts `/v1/environments/.../work/.../heartbeat` requests inside the sandbox and returns `200 OK` locally without forwarding upstream.

### Maximum Idle Timeout (`--max-idle`)

When `ate-antworker worker` receives a `/work` request, it starts `ant beta:worker run` with `--max-idle 30s`. Once all tool calls for the current work item are executed and results are streamed to Anthropic, the CLI detects session inactivity and exits after the 30-second idle period.

Without `--max-idle`, `ant beta:worker run` remains running indefinitely waiting for subsequent turns on the same session, preventing the actor from suspending and releasing its worker pool slot.

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
  --worker-image      gcr.io/dberkov-gke-dev3/ate-antworker@sha256:029c5c516d64630de3f154379b5749c773a29554d6813c6cfccdb6f6b9ace923 \
  --ateom-image       gcr.io/$GOOGLE_CLOUD_PROJECT/ate-images/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:b0b6e2ad834de42cb2a4c55e83b60243f66cb85ca37575d1a6818e788e0564e0 \
  --snapshots-bucket  gs://$GOOGLE_CLOUD_PROJECT/ate-antworker/ \
  --anthropic-api-key "$ANTHROPIC_API_KEY" \
  --anthropic-env-id  "$ANTHROPIC_ENVIRONMENT_ID" \
  --anthropic-env-key "$ANTHROPIC_ENVIRONMENT_KEY" | kubectl apply -f -
```

### 3. Verify Deployment

Check that the worker poller Deployment and Substrate worker pool are running:

```bash
kubectl get pods -n ate-antworker
```

Check logs of the worker poller Deployment:

```bash
kubectl logs -n ate-antworker -l app=ate-antworker -f
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
| `ate-antworker` | `Atespace` (`ate.dev/v1alpha1`) | Substrate tenant atespace for actors |
| `ate-antworker` | `Namespace` | Target Kubernetes namespace |
| `ate-antworker-creds` | `Secret` | Stores `ANTHROPIC_API_KEY`, `ANTHROPIC_ENVIRONMENT_ID`, `ANTHROPIC_ENVIRONMENT_KEY` |
| `valkey` | `Service` | Internal ClusterIP service exposing Valkey queue on port 6379 |
| `valkey` | `Deployment` | Dedicated Valkey instance managing durable streams and consumer groups |
| `ate-antworker-workerpool` | `WorkerPool` (`ate.dev/v1alpha1`) | Pool of gVisor/microVM runners for sandbox actors (10 replicas) |
| `ate-antworker-actor-template` | `ActorTemplate` (`ate.dev/v1alpha1`) | Sandbox container definition (`ate-antworker worker`) and workspace snapshot storage |
| `ate-antworker` | `Deployment` | Always-on poller running `ant beta:worker poll --on-work ate-antworker` (5 replicas) |

## Roadmap

- [ ] **Distribute prebuilt images** so deploying is one `kubectl apply` with no build step.
- [x] **Durable session orchestration & buffering** with distributed locking, crash recovery, and auto-ACKing.
