# Claude Managed Agents on Agent Substrate

Deploy [Anthropic Managed Agents Self-Hosted Sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) workers on [Agent Substrate](https://github.com/agent-substrate/substrate).

This worker runs Claude Managed Agents tool calls (bash, file operations, skills) inside isolated, gVisor/microVM sandboxes with durable session orchestration and cross-turn snapshot persistence.

## Overview

```
Claude Managed Agents  ──►  Poller (`ate-antworker`)
                                │
                                ▼
                       Sandbox Actor (`ant beta:worker run`)
                       (gVisor / microVM sandbox on Substrate)
```

### Components

- **Poller**: An always-on service running `ate-antworker`. It fetches incoming work items and dispatches them to sandbox actors.
- **Sandbox Actors**: Dedicated, isolated environments assigned to each agent session. They execute tools (bash commands, file operations, skills) and preserve session state across turns via snapshots.

### Turn Lifecycle

When `ate-antworker worker` receives a `/work` request via the Substrate router:
1. It immediately acknowledges `200 OK` (`{"status":"accepted"}`).
2. In the background, it executes `ant beta:worker run` directly against Anthropic API with `--max-idle 1s`.
3. Process execution is serialized so that only one `ant` process runs at a time within the sandbox actor.
4. Once all tool calls for the current turn are finished and the idle timeout expires, `ant` exits.

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
  --worker-image      gcr.io/dberkov-gke-dev3/ate-antworker@sha256:a0587dde12efd3c9b51ffbace2e50cf7b9585d98da94b358b381e6340774ec91 \
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
| `ate-antworker-workerpool` | `WorkerPool` (`ate.dev/v1alpha1`) | Pool of gVisor/microVM runners for sandbox actors (10 replicas) |
| `ate-antworker-actor-template` | `ActorTemplate` (`ate.dev/v1alpha1`) | Sandbox container definition (`ate-antworker worker`) and workspace snapshot storage |
| `ate-antworker` | `Deployment` | Always-on poller running `ate-antworker` (1 replica) |

## Roadmap

- [ ] **Distribute prebuilt images** so deploying is one `kubectl apply` with no build step.
- [x] **Durable session orchestration & buffering** with distributed locking, crash recovery, and auto-ACKing.
