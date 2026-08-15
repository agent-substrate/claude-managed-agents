# Claude Managed Agents on Agent Substrate

This project enables Claude Managed Agents to run code execution, bash commands, file operations, and skills inside secure, isolated sandboxes running on your Agent Substrate cluster.

Deploy [Anthropic Managed Agents Self-Hosted Sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) workers on [Agent Substrate](https://github.com/agent-substrate/substrate).

## Overview

This project connects Claude Managed Agents to secure, isolated sandboxes running on your Agent Substrate cluster.

### Key Components

- **Poller**: An always-on service in your cluster that fetches incoming events and appends them to the work queue.
- **Work Queue**: A durable Redis queue holding polled events until a turn has actually completed.
- **Consumers**: Deployment that drains the queue, resumes the sandbox actor for each event, and acknowledges the event when the turn is done.
- **Worker Pool**: A set of pre-warmed microVM/gVisor runners ready to launch sandboxes.
- **Sandbox Actors**: Dedicated, isolated environments assigned to each agent session. They execute tools (bash commands, file operations, skills) and preserve session state across turns.

### How It Works

1. **Poll a new Task**: The poller fetches a new task queued by Claude Managed Agents.
2. **Queue**: The poller appends the event to the durable queue and returns immediately.
3. **Dispatch**: A consumer claims the event and dispatches the work to a dedicated sandbox actor.
4. **Run**: Inside the session-tenant sandbox, the worker runs the requested work.
5. **Suspend**: When the execution completes, Substrate suspends the actor and persists its snapshot, and the consumer acknowledges the event.

```
              ╭─────────────────────────────────╮
              │      Claude Managed Agents      │
              ╰─────────────────▲───────────────╯
                                │ 1. Poll Work
╭───────────────────────────────┴─────────────────────────────────────╮
│                Agent Substrate on Kubernetes Cluster                │
│                                                                     │
│  ╭──────────────────────────╮        ╭────────────────────────────╮ │
│  │ ate-antworker Poller     │ 2. LPUSH  Redis Work Queue          │ │
│  │ (Poller Deployment)      ├───────►│ (durable, append-only)     │ │
│  ╰──────────────────────────╯        ╰──────────────┬─────────────╯ │
│                                                     │ 3. Claim      │
│  ╭──────────────────────────╮                       ▼               │
│  │ ate-antworker Consumers  │◄──────────────────────╯               │
│  │ (Consumer Deployment)    │ 4. Dispatches Session                 │
│  ╰──────────────────────────╯─────────────────────╮                 │
│                                                   ▼                 │
│  ╭──────────────────────╮                ╭───────────────────────╮  │
│  │ Substrate WorkerPool ├─ 5. Resumes ──►│ Sandbox Actor         │  │
│  ╰──────────────────────╯                │ (ate-antworker)       │  │
│                                          │ • Runs worker task    │  │
│                                          │ • Persists snapshot   │  │
│                                          ╰───────────────────────╯  │
╰─────────────────────────────────────────────────────────────────────╯
```

## Commands

`ate-antworker` provides four subcommands:

1. **`ate-antworker manifest`**: Generates Kubernetes deployment YAML for Redis, the worker pool, sandbox template, poller Deployment, and consumer Deployment.
2. **`ate-antworker dispatch`**: Hook target for **`ant beta:worker poll --on-work 'ate-antworker dispatch'`**. Appends the work item to the durable queue and returns.
3. **`ate-antworker consume`**: Drains the queue (claims item $\rightarrow$ locks session $\rightarrow$ wakes actor $\rightarrow$ executes turn $\rightarrow$ suspends actor $\rightarrow$ acknowledges item).
4. **`ate-antworker worker`**: The in-sandbox HTTP daemon running inside each Substrate actor pod supervising `ant beta:worker run`.

---

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
  --worker-image      gcr.io/dberkov-gke-dev3/anthropic-worker@sha256:73b3f624137c4334c5f351799ec8d943a60beea7b037a958aca9858bb44852d1 \
  --ateom-image       gcr.io/$GOOGLE_CLOUD_PROJECT/ate-images/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:b0b6e2ad834de42cb2a4c55e83b60243f66cb85ca37575d1a6818e788e0564e0 \
  --snapshots-bucket  gs://$GOOGLE_CLOUD_PROJECT/ate-antworker/ \
  --anthropic-api-key "$ANTHROPIC_API_KEY" \
  --anthropic-env-id  "$ANTHROPIC_ENVIRONMENT_ID" \
  --anthropic-env-key "$ANTHROPIC_ENVIRONMENT_KEY" | kubectl apply -f -
```

By default this also generates a single-replica Redis with append-only
persistence for the work queue. To use an existing Redis instead, pass
`--redis-addr` (and `--redis-password` if it requires auth):

```bash
ate-antworker manifest ... \
  --redis-addr      redis.example.com:6379 \
  --redis-password  "$REDIS_PASSWORD"
```

### 3. Verify Deployment

Check that the worker poller Deployment and Substrate pre-warmed worker pool are running:

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

# 2. Run example
go run ./examples/agent \
  --prompt "Run 'uname -a' in bash and show the output."
```

## Work Queue

Polled events are persisted before any actor is resumed, so a work item is not
lost if the poller pod is evicted, the consumer crashes mid-turn, or the actor
cannot be woken. A claimed item stays in `<prefix>:inflight` under a lease until
its turn completes; when renewals stop, maintenance reclaims and retries it, or
moves it to `<prefix>:dead` once `--max-attempts` is used up. Expiry is the only
retry path, so a failed turn waits out its lease (5 minutes) instead of retrying
immediately.

Each consumer runs one turn at a time and a claim takes the whole session, so
concurrency is the replica count and no two turns ever share a sandbox. Set the
pool with `--consumer-replicas` (8 by default, under the pre-warmed WorkerPool
size). On SIGTERM a consumer stops claiming and finishes the turn it is on, so
give the pod a `terminationGracePeriodSeconds` longer than a typical turn if you
want it to finish rather than be reclaimed.

Inspect the queue with `redis-cli`:

```bash
kubectl exec -n ate-antworker ate-antworker-redis-0 -- \
  redis-cli llen ate-antworker:pending
kubectl exec -n ate-antworker ate-antworker-redis-0 -- \
  redis-cli lrange ate-antworker:dead 0 -1
```

The generated Redis is a single replica: it survives a pod restart through its
persistent volume, but not the loss of its node. Point `--redis-addr` at a
managed or replicated Redis for a stronger guarantee.

## Running Locally

To run the poller, queue, and consumer locally for development or testing:

```bash
# 1. Port-forward ateapi and router
kubectl port-forward svc/api 8443:443 -n ate-system &
kubectl port-forward svc/atenet-router 8080:80 -n ate-system &

# 2. Start a local Redis
podman run -d --name ate-antworker-redis -p 6379:6379 redis:7.4-alpine \
  redis-server --appendonly yes

# 3. Run poller; the hook only queues work
ant beta:worker poll \
  --environment-id "$ANTHROPIC_ENVIRONMENT_ID" \
  --environment-key "$ANTHROPIC_ENVIRONMENT_KEY" \
  --on-work "ate-antworker dispatch --redis-addr localhost:6379"

# 4. In another shell, run the consumer
ate-antworker consume \
  --redis-addr localhost:6379 \
  --ateapi-addr localhost:8443 \
  --atenet-router-addr localhost:8080 \
  --insecure
```

Every polled event goes through the queue: `ate-antworker dispatch` never
contacts an actor itself, so the poller cannot be the thing that loses work.

## Manifest Reference

| Resource | Kind | Purpose |
| --- | --- | --- |
| `ate-antworker` | `Atespace` (`ate.dev/v1alpha1`) | Substrate tenant atespace for actors |
| `ate-antworker` | `Namespace` | Target Kubernetes namespace |
| `ate-antworker-creds` | `Secret` | Stores `ANTHROPIC_API_KEY`, `ANTHROPIC_ENVIRONMENT_ID`, `ANTHROPIC_ENVIRONMENT_KEY`, and `REDIS_PASSWORD` |
| `ate-antworker-redis` | `Service` + `StatefulSet` | Durable work queue, with append-only persistence (omitted when `--redis-addr` points at an existing Redis) |
| `ate-antworker-workerpool` | `WorkerPool` (`ate.dev/v1alpha1`) | Pre-warmed microVM/gVisor runner pool (10 replicas) |
| `ate-antworker-actor-template` | `ActorTemplate` (`ate.dev/v1alpha1`) | Sandbox container definition (`ate-antworker worker`) and workspace snapshot storage |
| `ate-antworker` | `Deployment` | Always-on poller running `ant beta:worker poll --on-work "ate-antworker dispatch"` |
| `ate-antworker-consumer` | `Deployment` | Queue consumers running `ate-antworker consume` (2 replicas) |
