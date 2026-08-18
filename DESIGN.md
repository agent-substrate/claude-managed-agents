# Design Document: Claude Managed Agents on Agent Substrate

## 1. Overview

This project runs [Claude Managed Agents](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) sandboxes on [Agent Substrate](https://github.com/agent-substrate/substrate).

---

## 2. Goals

- **Isolated sandbox**: Run each session in its own isolated actor.
- **Fast wake-up**: Use a pool of pre-warmed runners so sandboxes start and resume quickly without long wait times.
- **Durable buffering**: Use a durable queue between different components so a crash doesn't cause an event to be dropped.
- **Auto retries**: Automatically retry failed operations (like network partitions) and handle crashes gracefully without losing events.
- **Density**: Automatically pause (suspend) idle sandboxes when they are not in use to free up cluster memory and CPU.

---

## 3. System Architecture

```
┌─────────────────────────┐
│  Claude Managed Agents  │
└────────────┬────────────┘
             │ Poll for work
             ▼
   ┌────────────────────────┐
   │ Poller (`ate-ant poll`)│
   └─────────┬──────────────┘
             │ Enqueue lifecycle events
             ▼
   ┌───────────────────┐
   │ Event Queue (Redis│
   │    / Valkey)      │
   └─────────┬─────────┘
             │ Consume events
             ▼
   ┌───────────────────┐      Substrate API (Actor Lifecycle)
   │    Controller     ├─────────────────────────────────────────┐
   │(`ate-ant controller`)                                       │
   └─────────┬─────────┘                                         ▼
             │ POST /work                           ┌─────────────────────────┐
             ▼                                      │    Substrate Control    │
   ┌───────────────────┐                            │      Plane (ateapi)     │
   │   atenet-router   │                            └─────────────────────────┘
   └─────────┬─────────┘
             │ Route by Session ID
             ▼
   ┌────────────────────────────────────────────────────────┐
   │ Sandbox Actor (`ate-ant run`)                          │
   │  ┌──────────────┐         ┌─────────────────────────┐  │
   │  │ HTTP Server  │ ──────► │ `ant beta:worker run`   │  │
   │  │  (:80)       │ (forks) │ (Tool execution engine) │  │
   │  └──────────────┘         └─────────────────────────┘  │
   │                                                        │
   │  Isolated gVisor / microVM container                   │
   │  Snapshot storage: gs://bucket/snapshots/              │
   └────────────────────────────────────────────────────────┘
```

### Overview

Claude Managed Agents allow polling 

### Components

All functionality is packaged into a single binary (`ate-ant`) with subcommands for each operational role:

1. **`ate-ant poll`**:
   - Always-on daemon that polls the Anthropic Managed Agents API for new work items.
   - Pushes session lifecycle events (`resume`) into the queue without waiting for sandboxes to be ready.

2. **`ate-ant controller`**:
   - Consumes lifecycle events from the queue.
   - Manages Substrate actor lifecycles (creating new actors, resuming suspended ones, and suspending idle ones) via the Substrate API (`ateapi`).
   - Manages actor idleness detection and actor garbage collection.
   - Dispatches work notifications (`POST /work`) to the appropriate actor via `atenet-router`.

3. **`ate-ant run` (in-sandbox helper)**:
   - Runs as the entrypoint process inside each sandbox actor container.
   - Exposes HTTP endpoints (`/healthz` for readiness, `/work` for turn execution, `/idle` for activity metrics).
   - Handles `/work` to run `ant beta:worker run` to run incoming work items.

#### Durable Buffer

An in-cluster Redis/Valkey acts as the durable buffer between the poller and the controller.
A durable buffer provides many benefits such as:

- **Decoupling ingestion from execution**: Polling Anthropic's API needs to be fast and continuous. Creating or waking up sandboxes and restoring snapshots can take seconds. The queue absorbs traffic bursts and ensures the poller never stalls or misses work.

- **Crash resilience**: If a controller pod restarts or crashes, uncompleted events remain safely in the queue instead of being lost in memory.

- **Single stateful environment**: Each session ID represents a single, stateful sandbox with its own filesystem and memory state. Running multiple operations on the same session at the same time (such as overlapping tool executions or simultaneous resume/suspend requests) would cause race conditions, conflicting file writes, or snapshot corruption.

- **One active run per session**: The queue ensures all lifecycle events for a given session ID are strictly serialized. Only **one event per session ID** can be actively processed at any time. If new work arrives for a session while a previous event is still running, it is queued and processed only after the active event finishes.

---

## 4. Lifecycle

### Work Ingestion
- The **`ate-ant`** poller runs continuously in a tight polling loop against the Anthropic Managed Agents API.
- When an agent turn generates tool calls (e.g. bash commands, file edits), Anthropic creates a work item containing `session_id` and `work_id`.
- The poller claims the work item and pushes a `resume` event into the Valkey queue.
- The poller does not wait for the sandbox to wake up or execute; it immediately returns to the polling loop to claim subsequent work items.

### 2. Queueing
- The **Valkey queue** receives and buffers lifecycle events.
- Events for the same session ID are strictly serialized in FIFO order.
- The queue guarantees that **only one event per session ID** is actively being processed by a controller at any time.
- If a new turn arrives while an earlier turn for the same session is still in flight, the new event waits safely in the queue until the active turn finishes.

### 3. Actor lifecycle
- The **`ate-ant controller`** pulls the next available event from the queue.
- It checks the session's actor status with the Substrate API server (`ateapi`):
  - **First-time session**: Substrate provisions a new actor container based on the `ActorTemplate` spec using a runner pod claimed from the pre-warmed `WorkerPool`.
  - **Dormant session**: Substrate issues a resume command. It claims a pre-warmed runner pod and downloads the actor's incremental memory and disk snapshot from object storage (`gs://bucket/snapshots/`). The session is restored to its exact previous state in sub-seconds.
  - **Active session**: If the actor is already awake from a recent turn, it is reused immediately without snapshot restoration.

### 4. Dispatching
- Once the actor container is running and healthy (passing `/healthz` on port 80), the controller sends an HTTP `POST /work` request with the JSON payload `{ "session_id": "...", "work_id": "..." }`.
- The request is routed through Substrate's network router (`atenet-router`), which inspects the session ID and directs the HTTP traffic to the specific actor container.

### 5. Runs
- Inside the actor sandbox, the **`ate-ant run`** daemon receives the `POST /work` request on `:80`.
- It immediately acknowledges the dispatch with `200 OK` (`{"status":"accepted"}`) so intermediate routers and proxies do not time out.
- In the background, it launches the Anthropic worker runtime (`ant beta:worker run`).
- The `ant` process connects to Anthropic, executes all tool calls inside the sandbox (running commands in bash, modifying files, executing skills), and streams results back to Anthropic.
- When all tool calls for the turn are complete and the idle timeout expires, `ant` exits.
- The `ate-ant run` daemon marks the process as finished, updates its last active timestamp, and begins tracking idle duration.

### 6. Idleness Detection, Checkpointing, and Suspension
- The controller monitors active actors by querying their `/idle` endpoint.
- When an actor has remained idle beyond a configured timeout (and after a cooldown period), the controller issues a suspend request to Substrate.
- Substrate suspends the actor.
- When the user sends another message in this session, the lifecycle repeats from Step 1, restoring from this snapshot.
