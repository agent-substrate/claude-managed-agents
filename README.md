# Claude Managed Agents on Agent Substrate

Deploy [Anthropic Managed Agents Self-Hosted Sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) workers on [Agent Substrate](https://github.com/agent-substrate/substrate).

This project enables Claude Managed Agents to run bash commands, file operations, code execution, and skills inside secure, isolated sandboxes running on your Agent Substrate cluster.

## Overview

### Components

- **Poller**: An always-on service in your cluster running `ant beta:worker poll --on-work ate-antworker`. It fetches incoming work items and dispatches them.
- **Durable Buffer**: A durable session queue on top of Valkey Streams. It provides per-session distributed locking, automatic heartbeat renewal, crash reclamation (`XAUTOCLAIM`), and dead-letter queueing.
- **Sandbox Actors**: Dedicated, isolated environments assigned to each agent session. They execute tools (bash commands, file operations, skills) and preserve session state across turns via snapshots.

### How It Works

```
                ╭───────────────────────────────────╮
                │       Claude Managed Agents       │
                ╰─────────────────┬─────────────────╯
                                  │ 1. poll work item
╭─────────────────────────────────┼──────────────────────────────────╮
│ Agent Substrate on Kubernetes   ▼                                  │
│    ╭──────────────────────────────────────────────────────────╮    │
│    │ Poller                                                   │    │
│    │   ant beta:worker poll --on-work ate-antworker           │    │
│    ╰────────────────────────────┬─────────────────────────────╯    │
│                                 │ 2. enqueue & coordinate          │
│                                 ▼                                  │
│    ╭──────────────────────────────────────────────────────────╮    │
│    │ Durable Buffer                                           │    │
│    │   Distributed session locking + automatic heartbeats     │    │
│    ╰────────────────────────────┬─────────────────────────────╯    │
│                                 │ 3. notify via router             │
│                                 ▼                                  │
│    ╭─────────────────────────╮  ╭─────────────────────────────╮    │
│    │ Substrate WorkerPool    ├─►│ Sandbox Actor (per session) │    │
│    ╰─────────────────────────╯  │   ate-antworker worker      │    │
│                                 │   ant beta:worker run       │    │
│                                 ╰─────────────────────────────╯    │
│                                 4. run tools, stream results       │
│                                 5. suspend actor & ACK event       │
╰────────────────────────────────────────────────────────────────────╯
```

### Durable Buffer

Valkey Streams provides a durable buffer for distributed coordination, locking, and crash recovery.

We serialize runs for the same `session_id`, ensuring multiple turns or concurrent poller claims never execute simultaneously on the same actor.

While a handler is running, we automatically renew its claim and lock. If a runner crashes mid-run, another runner reclaims the pending entry via `XAUTOCLAIM`. Events that persistently
fail are moved to a dead-letter stream after exceeding max deliveries.

### Heartbeat Interception

When `ant beta:worker poll --on-work ate-antworker` claims a work item, the poller process maintains an upstream heartbeat loop with Anthropic to keep the work lease active for the duration of the hook execution.

Inside the sandbox actor, `ant beta:worker run` also attempts to heartbeat the same work item to Anthropic with `expected_last_heartbeat=NO_HEARTBEAT`. Because the poller has already heartbeated the work item, sending this request upstream would result in a `412 Precondition Failed` conflict error, causing `ant beta:worker run` to terminate prematurely.

The local `ate-antworker worker` HTTP server intercepts `/v1/environments/.../work/.../heartbeat` requests inside the sandbox and returns `200 OK` locally without forwarding upstream.

### Maximum Idle Timeout (`--max-idle`)

When `ate-antworker worker` receives a `/work` request, it starts `ant beta:worker run` with `--max-idle 30s`. Once all tool calls for the current work item are executed and results are streamed to Anthropic, the CLI detects session inactivity and exits after the 30-second idle period.

Without `--max-idle`, `ant beta:worker run` remains running indefinitely waiting for subsequent turns on the same session, preventing the actor from suspending and releasing its worker pool slot.

## Prerequisites

1. Kubernetes cluster with [Agent Substrate](https://github.com/agent-substrate/substrate) installed.
2. Anthropic API credentials:
   - `ANTHROPIC_API_KEY` (e.g. `sk-ant-api...`)
   - `ANTHROPIC_ENVIRONMENT_ID` (e.g. `env_01abc...`)
   - `ANTHROPIC_ENVIRONMENT_KEY` (optional, e.g. `sk-ant-oat...`)
3. A Google Cloud project provided by `GOOGLE_CLOUD_PROJECT` environment variable.
4. An object storage bucket for actor snapshots (e.g. `gs://$GOOGLE_CLOUD_PROJECT/ate-antworker/`).
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
go install github.com/agent-substrate/claude-managed-agents/cmd/ate-antworker@latest

# Generate deployment YAML and apply to Kubernetes
ate-antworker manifest \
  --worker-image      gcr.io/dberkov-gke-dev3/ate-antworker@sha256:c184286aaf020a9d6e986d78f047eb9e1a9d00529bfd8ce8ce7d57f164b9ac2e \
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

Once deployed, send tasks to Claude referencing your self-hosted `ANTHROPIC_ENVIRONMENT_ID`. The cluster will automatically claim the work, resume the sandbox actor, execute tools inside the sandbox, and suspend the actor when idle.

```bash
# 1. Set environment variables
export ANTHROPIC_API_KEY="sk-ant-api..."
export ANTHROPIC_ENVIRONMENT_ID="env_..."

# 2. Run example with an initial prompt (prints session ID & trace URL)
go run ./examples/agent \
  --prompt "Run 'uname -a' in bash and show the output."

# 3. Reuse an existing agent, instead of creating one on every run
go run ./examples/agent \
  --agent "agent_..." \
  --prompt "Show the kernel version."
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
| `ate-antworker-workerpool` | `WorkerPool` (`ate.dev/v1alpha1`) | Pool of microVM/gVisor runners for sandbox actors (10 replicas) |
| `ate-antworker-actor-template` | `ActorTemplate` (`ate.dev/v1alpha1`) | Sandbox container definition (`ate-antworker worker`) and workspace snapshot storage |
| `ate-antworker` | `Deployment` | Always-on poller running `ant beta:worker poll --on-work ate-antworker` (5 replicas) |

## Roadmap

- **Distribute prebuilt images** so deploying is one `kubectl apply` with no build step.
- [x] **Durable session orchestration & buffering** backed by [`sessionflight`](https://github.com/rakyll/sessionflight) with distributed locking, crash recovery, and auto-ACKing.
