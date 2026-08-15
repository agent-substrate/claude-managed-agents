# Claude Managed Agents on Agent Substrate

Deploy [Anthropic Managed Agents Self-Hosted Sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) workers on [Agent Substrate](https://github.com/agent-substrate/substrate).

This project enables Claude Managed Agents to run bash commands, file operations, code execution, and skills inside secure, isolated sandboxes running on your Agent Substrate cluster.


## Overview

### Components

- **Poller**: An always-on service in your cluster that fetches incoming events and appends them to the work queue.
- **Work Queue**: A durable Redis queue holding polled events until the work has actually completed.
- **Dispatchers**: Deployment that drains the queue, resumes the sandbox actor for each event, and acknowledges the event when the work is done.
- **Worker Pool**: A set of microVM/gVisor runners that sandbox actors are scheduled onto.
- **Sandbox Actors**: Dedicated, isolated environments assigned to each agent session. They execute tools (bash commands, file operations, skills) and preserve session state across work items.

### How It Works

1. **Poll**: The poller fetches a work item queued by Claude Managed Agents.
2. **Queue**: The poller appends it to the durable Redis queue and returns immediately, without touching an actor.
3. **Claim**: A dispatcher takes the work item together with its session, so no other dispatcher can enter that sandbox.
4. **Run**: The dispatcher resumes the sandbox actor, the worker runs the work item, Substrate suspends the actor and persists its snapshot, and only then is the item acknowledged.

If anything interrupts step 4, the claim lapses and the work item returns to `ate-antworker:pending` to be retried; after 3 attempts it lands on `ate-antworker:dead`.

```
                ╭───────────────────────────────────╮
                │       Claude Managed Agents       │
                ╰─────────────────┬─────────────────╯
                                  │ 1. poll work item
╭─────────────────────────────────┼──────────────────────────────────╮
│ Agent Substrate on Kubernetes   ▼                                  │
│    ╭──────────────────────────────────────────────────────────╮    │
│    │ Poller Deployment                                        │    │
│    │   ant beta:worker poll --on-work "ate-antworker queue"   │    │
│    ╰────────────────────────────┬─────────────────────────────╯    │
│                                 │ 2. enqueue, then return          │
│                                 ▼                                  │
│    ╭──────────────────────────────────────────────────────────╮    │
│    │ Redis work queue   (StatefulSet, appendonly)             │    │
│    │                                                          │    │
│    │   pending ──claim──► inflight ──ack──► done              │    │
│    │      ▲                   │                               │    │
│    │      ╰── lease expired ──┴── attempts used ──► dead      │    │
│    ╰────────────────────────────┬─────────────────────────────╯    │
│                                 │ 3. claim work item + its session │
│                                 ▼                                  │
│    ╭──────────────────────────────────────────────────────────╮    │
│    │ Dispatcher Deployment (N replicas, one work item each)   │    │
│    │   ate-antworker dispatch                                 │    │
│    ╰────────────────────────────┬─────────────────────────────╯    │
│                                 │ 4. resume, run, suspend, ack     │
│                                 ╰──────────────╮                   │
│                                                ▼                   │
│    ╭─────────────────────────╮  ╭─────────────────────────────╮    │
│    │ Substrate WorkerPool    │  │ Sandbox Actor (per session) │    │
│    │ microVM / gVisor        ├─►│ ate-antworker worker        │    │
│    │ runners                 │  │ ant beta:worker run         │    │
│    ╰─────────────────────────╯  ╰─────────────────────────────╯    │
╰────────────────────────────────────────────────────────────────────╯
```

## Commands

`ate-antworker` provides the following:

1. **`ate-antworker queue`**: Hook target for **`ant beta:worker poll --on-work 'ate-antworker queue'`**. Appends the work item to the durable queue and returns.
2. **`ate-antworker dispatch`**: Drains the queue (claims item and its session $\rightarrow$ wakes actor $\rightarrow$ runs the work $\rightarrow$ suspends actor $\rightarrow$ acknowledges item).
3. **`ate-antworker worker`**: The in-sandbox HTTP daemon running inside each Substrate actor pod supervising `ant beta:worker run`.

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
  --worker-image      gcr.io/dberkov-gke-dev3/ate-antworker@sha256:31b8bf93926ac28208b2659e3aa98b0199b695f241a823df2b9df511095a4c29 \
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

# 2. Run example
go run ./examples/agent \
  --prompt "Run 'uname -a' in bash and show the output."

# 3. Reuse the agent it created, instead of creating one on every run
go run ./examples/agent \
  --agent "agent_..." \
  --prompt "Show the kernel version."
```

## Work Queue

Polled events are persisted before any actor is resumed, so a work item is not
lost if the poller pod is evicted, the dispatcher crashes mid-run, or the actor
cannot be woken. A claimed item stays in `ate-antworker:inflight`, owned by a
single key — `ate-antworker:session:<id>`, whose value names the item holding it
and whose TTL the dispatcher renews while it works. When renewals stop,
maintenance requeues the item, or moves it to `ate-antworker:dead` after three
attempts. Expiry is the only retry
path, so a failed work item waits out its lease (5 minutes) instead of retrying
immediately.

Each dispatcher runs one work item at a time and a claim takes the whole
session, so concurrency is the replica count and no two work items ever share a
sandbox. Set the pool with `--dispatcher-replicas` (8 by default, under the
WorkerPool size). On SIGTERM a dispatcher stops claiming and finishes the item
it is on, so give the pod a `terminationGracePeriodSeconds` longer than a
typical work item if you want it to finish rather than be reclaimed.

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

To shell into the container locally with your Anthropic environment variables:

```bash
podman run -it --rm \
  --entrypoint bash \
  -e ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY" \
  -e ANTHROPIC_ENVIRONMENT_ID="$ANTHROPIC_ENVIRONMENT_ID" \
  -e ANTHROPIC_ENVIRONMENT_KEY="$ANTHROPIC_ENVIRONMENT_KEY" \
  gcr.io/dberkov-gke-dev3/ate-antworker@sha256:31b8bf93926ac28208b2659e3aa98b0199b695f241a823df2b9df511095a4c29
```

Inside the container:
- **Run worker manually**: `ant beta:worker run --work-id "<work-id>" --session-id "<session-id>"`

## Manifest Reference

| Resource | Kind | Description |
| --- | --- | --- |
| `ate-antworker` | `Atespace` (`ate.dev/v1alpha1`) | Substrate tenant atespace for actors |
| `ate-antworker` | `Namespace` | Target Kubernetes namespace |
| `ate-antworker-creds` | `Secret` | Stores `ANTHROPIC_API_KEY`, `ANTHROPIC_ENVIRONMENT_ID`, `ANTHROPIC_ENVIRONMENT_KEY`, and `REDIS_PASSWORD` |
| `ate-antworker-redis` | `Service` + `StatefulSet` | Durable work queue, with append-only persistence (omitted when `--redis-addr` points at an existing Redis) |
| `ate-antworker-workerpool` | `WorkerPool` (`ate.dev/v1alpha1`) | Pool of microVM/gVisor runners for sandbox actors (10 replicas) |
| `ate-antworker-actor-template` | `ActorTemplate` (`ate.dev/v1alpha1`) | Sandbox container definition (`ate-antworker worker`) and workspace snapshot storage |
| `ate-antworker` | `Deployment` | Always-on poller running `ant beta:worker poll --on-work "ate-antworker queue"` |
| `ate-antworker-dispatcher` | `Deployment` | Dispatchers running `ate-antworker dispatch` (8 replicas) |

## Roadmap

- **Distribute prebuilt images** so deploying is one `kubectl apply` with no build step.
- **Expose metrics** for queue depth, retries, and dead-lettered work items.
