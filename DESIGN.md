# Design Document

This project runs [Claude Managed Agents](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) sandboxes on [Agent Substrate](https://github.com/agent-substrate/substrate).

## Goals

- **Isolated session**: Run each session in its own isolated Substrate actor.
- **Fast wake-up**: Use a pool of pre-warmed runners so sandboxes start and resume quickly without long wait times.
- **Serialized execution**: Work items associated with the same session is serialized and executed in order.
No two work item are executed concurrently if they belong to the same session.
- **Durable buffering**: Use a durable queue between different components so a crash doesn't cause an event to be dropped.
- **Auto retries**: Automatically retry failed operations (like network partitions) and handle crashes gracefully without losing events.
- **Density**: Automatically pause (suspend) idle sandboxes when they are not in use to free up cluster memory and CPU.

## Architecture

```
  ┌─────────────────────────┐
  │  Claude Managed Agents  │
  └─────────────────────────┘
             ▲
             │ Poll work
   ┌─────────┴──────────────┐
   │ Poller (`ate-ant poll`)│
   └─────────┬──────────────┘
             │ Enqueue lifecycle events
             ▼
   ┌───────────────────┐
   │ Event Queue (Redis│
   │    / Valkey)      │
   └───────────────────┘
             ▲
             │ Consume events
   ┌─────────┴─────────┐      Resume / Suspend calls
   │    Controller     ├──────────────────────────────────────┐
   │(`ate-ant controller`)                                    │
   └─────────┬─────────┘                                      ▼
             │ POST /work                       ┌────────────────────────┐
             ▼                                  │    Agent Substrate     │
   ┌───────────────────┐                        │        (ateapi)        │
   │   atenet-router   │                        └────────────────────────┘
   └─────────┬─────────┘
             │ Route by Session ID
             ▼
   ┌────────────────────────────────────────────────────────┐
   │ Actor (`ate-ant run`)                                  │
   │  ┌──────────────┐         ┌─────────────────────────┐  │
   │  │ HTTP Server  │ ──────► │ `ant beta:worker run`   │  │
   │  │  (:80)       │ (forks) │ (Tool execution)        │  │
   │  └──────────────┘         └─────────────────────────┘  │
   │                                                        │
   │  Isolated session actor                                │
   └────────────────────────────────────────────────────────┘
```

### Components

- **Poller**: An always-on daemon running `ate-ant poll`. It polls the Claude Managed Agents API for new work items and enqueues lifecycle events into the queue.
- **Queue**: An in-cluster Valkey/Redis instance buffering session lifecycle events, providing durable work distribution and decoupling ingestion from execution.
- **Controller**: It consumes lifecycle events from the queue, manages Substrate actor lifecycles (creating, resuming, suspending), and notifies actors of work payloads via `atenet-router`.
- **Actors**: Dedicated, isolated environments assigned to each agent session running `ate-ant run`. They execute tools (bash commands, file operations, skills) via `ant beta:worker run` and preserve session state across turns via snapshot persistence.

#### Event Queue

An in-cluster Valkey/Redis instance acts as the durable buffer between the poller and the controller:

```
┌───────────────────────┐
│     ate-ant poll      │
│ (Work Ingestion Loop) │
└──────────┬────────────┘
           │ XADD stream:events *
           │ data: {action, session_id, work_id}
           ▼
┌───────────────────────────────────────┐
│             Valkey Stream             │
│           (`stream:events`)           │
└──────────┬─────────────────┬──────────┘
           │                 │
           │ XREADGROUP      │ XREADGROUP
           │ (exclusive)     │ (exclusive)
           ▼                 ▼
┌────────────────────┐ ┌────────────────────┐
│ ate-ant controller │ │ ate-ant controller │
│      (Pod #1)      │ │      (Pod #2)      │
└──────────┬─────────┘ └─────────┬──────────┘
           │                     │
           │ XACK stream:events  │ XACK stream:events
           ▼                     ▼
```

```
┌─────────────────────────────────────────────────────────┐
│                 Stream: `stream:events`                 │
├───────────────────┬───────────────────┬─────────────────┤
│ ID: 1723901000-0  │ ID: 1723901005-0  │ ID: 1723901010-0│
│ action: "resume"  │ action: "resume"  │ action:"suspend"│
│ session: sesn_1   │ session: sesn_2   │ session: sesn_1 │
│ work: work_101    │ work: work_102    │                 │
└─────────┬─────────┴─────────┬─────────┴────────┬────────┘
          │                   │                  │
       [ACKed]           [In-Flight]         [Queued]
          ▼                   ▼                  ▼
        XACK                 PEL                Next
     (Completed)       (Crash Recovery)      (In Line)
```

The event queue provides:

- **Decoupling ingestion from execution**: Polling Anthropic's API needs to be fast and continuous. Creating or waking up sandboxes and restoring snapshots can take seconds. The queue absorbs traffic bursts and ensures the poller never stalls or misses work.
- **Crash resilience**: If a controller pod restarts or crashes, uncompleted events remain in the Pending Entries List (PEL) and are replayed (`ID 0`) instead of being lost in memory.
- **Single stateful environment**: Each session ID represents a single, stateful sandbox with its own filesystem and memory state. Running multiple operations on the same session concurrently would cause race conditions and snapshot corruption.
- **One active run per session**: The queue ensures all lifecycle events for a given session ID are strictly serialized. Only **one event per session ID** can be actively processed at any time. If new work arrives for a session while a previous event is still running, it is queued and processed only after the active event finishes.

## Lifecycle

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
- It checks the session's actor status:
  - **First-time session**: Substrate provisions a new actor container based on the `ActorTemplate` spec using a runner pod claimed from the pre-warmed `WorkerPool`.
  - **Dormant session**: Substrate issues a resume.
  - **Active session**: If the actor is already awake from a recent turn, it is reused immediately.

### 4. Notifying
- Once the actor container is running and healthy (passing `/healthz` on port 80), the controller sends an HTTP `POST /work` request with the JSON payload `{ "session_id": "...", "work_id": "..." }`.
- The request is routed through Substrate's network router (`atenet-router`), which inspects the session ID and directs the HTTP traffic to the specific actor.

### 5. Runs
- Inside the actor sandbox, the **`ate-ant run`** daemon receives the `POST /work` request on `:80`.
- It immediately acknowledges the notification with `200 OK` (`{"status":"accepted"}`) so intermediate routers and proxies do not time out.
- In the background, it launches the Anthropic worker runtime (`ant beta:worker run`).
- The `ant` process connects to Anthropic, executes all tool calls inside the actor (running commands in bash, modifying files, executing skills), and streams results back to Anthropic.
- When all tool calls for the turn are complete and the idle timeout expires, `ant` exits.
- The `ate-ant run` daemon marks the work as finished.

### 6. Idleness Detection and Suspension

The controller runs a Garbage Collection (GC) loop that periodically inspects all active actors
and suspends idle actors:

```
┌───────────────────────────────────────────────┐
│     ate-ant controller (Garbage Collector)    │
└────────┬──────────────────────┬───────────────┘
         │ 1. ListActors (5s)   │ 2. GET /idle
         ▼                      ▼
┌──────────────────┐   ┌────────────────────────┐
│  ateapi Server   │   │ Actor (:80)            │
│ (Control Plane)  │   │ idle: 32.8s > 30s      │
└────────▲─────────┘   └────────┬───────────────┘
         │                      │
         │ 3. SuspendActor()    │ (idle >= 30s)
         │                      │
┌────────┴──────────────────────┴───────────────┐
│              Agent Substrate Engine           │
└──────────────────────┬────────────────────────┘
                       │ 4. Snapshot
                       ▼
            ┌─────────────────────┐
            │     GCS Bucket      │
            │ (gs://.../snapshots)│
            └─────────────────────┘
```

#### Garbage Collection

```
Controller            atenet-router        Actor (:80)                 ateapi
    │                       │                       │                     │
    │── GET /idle ─────────►│                       │                     │
    │                       │── GET /idle ─────────►│                     │
    │                       │◄── 200 {"idle":"12s"}─│                     │
    │◄── 200 {"idle":"12s"}─│                       │                     │
    │    [12s < 30s: Active]│                       │                     │
    │                       │                       │                     │
    │    ... (time passes) ...                      │                     │
    │                       │                       │                     │
    │── GET /idle ─────────►│                       │                     │
    │                       │── GET /idle ─────────►│                     │
    │                       │◄── 200 {"idle":"32s"}─│                     │
    │◄── 200 {"idle":"32s"}─│                       │                     │
    │    [32s >= 30s: SUSPEND]                      │                     │
    │                                                                     │
    │── SuspendActor(actorRef) ──────────────────────────────────────────►│
    │                                                                     │ (Checkpoint)
    │◄── Actor Suspended ─────────────────────────────────────────────────│ (Free pod to pool)
```

1. **Sweep**: The controller queries `ateapi.ListActors()` to discover all active actors in the namespace.
2. **Idleness Probing**: The controller sends `GET /idle` through `atenet-router` to query the actor's last activity timestamp.
4. **Suspension**: The controller calls `ateapi.SuspendActor()` for the idle actor.
5. **Resumption**: On subsequent new events, the controller resumes the actor from the snapshot.
