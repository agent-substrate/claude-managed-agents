# Design Document

This project runs Claude Managed Agents work (e.g. tool calls like bash, file operations, skills) inside isolated Agent Substrate gVisor/microVM actors with durable orchestration.

## Goals

- **Isolated sessions**: Run each session in its own isolated Agent Substrate actor.
- **Serialized execution**: Work items within the same session are serialized and executed sequentially, preventing any concurrent execution per session.
- **Auto retries**: Automatically retry failed operations (like network partitions) and handle crashes gracefully without losing events.
- **Density**: Automatically pause (suspend) idle actors when they are not in use.

## High-Level Architecture

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

### Event Queue

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

- **Crash resilience**: If a controller pod restarts or crashes, uncompleted events remain in the Pending Entries List (PEL) and are replayed instead of being lost.
- **One active run per session**: The queue ensures all lifecycle events for a given session ID are strictly serialized. Only one event per session ID can be actively processed at any time. If new work arrives for a session while a previous event is still running, it is queued and processed only after the active event finishes.

## Lifecycle

### Work Ingestion

- The `ate-ant` poller runs continuously in a tight polling loop against the Anthropic Managed Agents API.
- When an agent turn generates tool calls (e.g. bash commands, file edits), Anthropic creates a work item containing `session_id` and `work_id`.
- The poller claims the work item and pushes a `resume` event into the Valkey queue.
- The poller does not wait for the sandbox to wake up or execute; it immediately returns to the polling loop to claim subsequent work items.

### Consuming

- The queue receives and buffers lifecycle events. Events for the same session ID are strictly serialized in FIFO order.
- The queue guarantees that only one event per session ID is actively being processed by a controller at any time.
- If a new turn arrives while an earlier turn for the same session is still in flight, the new event waits safely in the queue until the active turn finishes.
- If anything crashes during the execution of the event, the event isn’t acknowledged and retried after some time.

### Actor Resumption

The `ate-ant` controller pulls the next available event from the queue.
It checks the session's actor status:
- **First-time session**: Substrate provisions a new actor container based on the `ActorTemplate` spec using a runner pod claimed from the pre-warmed `WorkerPool`.
- **Dormant session**: Substrate issues a resume.
- **Active session**: If the actor is already awake from a recent turn, it is reused immediately.

### Notifying

- Once the actor container is running and healthy (passing `/healthz` on port 80), the controller sends an HTTP `POST /work` request with the JSON payload `{ "session_id": "...", "work_id": "..." }`.
- The request is routed through Substrate's network router (`atenet-router`), which inspects the session ID and directs the HTTP traffic to the specific actor.

### Running the Work

- Inside the actor sandbox, the `ate-ant run` daemon receives the `POST /work` request on `:80`.
- It immediately acknowledges the notification with `200 OK` (`{"status":"accepted"}`) so intermediate routers and proxies do not time out.
- In the background, it launches the Anthropic worker runtime (`ant beta:worker run`).
- The `ant` process connects to Anthropic, executes all tool calls inside the actor (running commands in bash, modifying files, executing skills), and streams results back to Anthropic.
- When all tool calls for the turn are complete and the idle timeout expires, `ant` exits.
- The `ate-ant run` daemon marks the work as finished.

### Idleness Detection and Suspension

The controller runs a Garbage Collection (GC) loop that periodically inspects all active actors and suspends idle actors. The GC loop runs in the background and identifies the idle actors, once identified the actors are swept and suspended. An actor must be idle for at least 30 seconds to be marked as garbage. If an incoming request arrives after marking, we avoid sweeping.

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

- **Sweep**: The controller queries `ateapi.ListActors()` to discover all active actors in the namespace.
- **Idleness Probing**: The controller sends `GET /idle` through `atenet-router` to query the actor's last activity timestamp.
- **Suspension**: The controller calls `ateapi.SuspendActor()` for the idle actor.
- **Resumption**: On subsequent new events, the controller resumes the actor from the snapshot.

## Improvements

We want to harden reliability by removing our bespoke internal queue in favor of the CMA queue as the source of truth. Currently, managing a duplicate queue leads to state drift with the Anthropic event queue. Relying on CMA directly as a pub/sub style service would drastically simplify our implementation. Some additional capabilities like suspension/sleep signals directly coming from CMA would simplify our idleness detection.

- **Work item acknowledgment**: A mechanism to acknowledge polled items upon receipt and queuing, prior to completion.
- **Completion with error status**: The ability to complete events with an error status so tool calls fail rather than hanging forever.
- **Pollable sleep event**: Delivering an "sleep" event through the polling API alongside work items. Managed Agents should serve as the single source of truth rather than requiring external serialization of these lifecycle events.
- **Allow rescheduling under load**: Similar to model request call reschedules, it would be great if there is a way to automatically reschedule work at a later time.

In addition to this work, we're also considering an official garbage collector for Agent Substrate and will remove our current bespoke implementation once that's live.
