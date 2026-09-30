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
             │ Poll work items
   ┌─────────┴───────────────────────────────────────┐
   │ Controller (`ate-ant controller`)               │
   │                                                 │
   │   ┌─────────────────────────────────────────┐   │
   │   │ Poller loop                             │   │
   │   └────────────────────┬────────────────────┘   │
   └────────────────────────┼────────────────────────┘
                            │ Enqueue lifecycle events
                            ▼
                  ┌───────────────────┐
                  │ Event Queue (Redis│
                  │    / Valkey)      │
                  └─────────┬─────────┘
                            │
                            │ Consume events
   ┌────────────────────────┴────────────────────────┐
   │ Controller (`ate-ant controller`)               │
   │                                                 │
   │   ┌─────────────────────────────────────────┐   │
   │   │ Reconcile loop                          │   │
   │   └────────────────────┬────────────────────┘   │
   └─────────┬──────────────┴─────┬──────────────────┘
             │                    │ Resume / Suspend calls
             │ POST /work         ▼
             │          ┌────────────────────────┐
             ▼          │    Agent Substrate     │
   ┌───────────────────┐│        (ateapi)        │
   │   atenet-router   │└────────────────────────┘
   └─────────┬─────────┘
             │ Route by Session ID
             ▼
   ┌────────────────────────────────────────────────────────┐
   │ Actor (`ate-ant run`)                                  │
   │  ┌──────────────┐         ┌─────────────────────────┐  │
   │  │ HTTP Server  │ ──────► │ Environment worker      │  │
   │  │  (:80)       │ (in-    │ (Tool execution +       │  │
   │  │              │ process)│  lease heartbeat)       │  │
   │  └──────────────┘         └─────────────────────────┘  │
   │                                                        │
   │  Isolated session actor                                │
   └────────────────────────────────────────────────────────┘
```

### Components

- **Controller**: A horizontally scalable daemon running `ate-ant controller`. It runs the Anthropic poller loop to ingest work items and queue lifecycle events into Valkey, as well as the event reconcile loop that consumes events from Valkey, manages Substrate actor lifecycles (creating, resuming, suspending), and notifies actors of work payloads via `atenet-router`.
- **Queue**: An in-cluster Valkey/Redis instance buffering session lifecycle events, providing durable work distribution and decoupling ingestion from execution.
- **Actors**: Dedicated, isolated environments assigned to each agent session running `ate-ant run`. They execute tools (bash commands, file operations, skills) with the SDK's environment worker running in-process, continue the lease the poller took, and preserve session state across turns via snapshot persistence.

### Event Queue

An in-cluster Valkey/Redis instance acts as the durable buffer between the poller and the controller:

```
┌────────────────────┐ ┌────────────────────┐
│ ate-ant controller │ │ ate-ant controller │
│  (Pod #1 Poller)   │ │  (Pod #2 Poller)   │
└──────────┬─────────┘ └─────────┬──────────┘
           │ XADD stream:events  │ XADD stream:events
           ▼                     ▼
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
│ (Pod #1 Reconciler)│ │ (Pod #2 Reconciler)│
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
- The poller claims the work item, acknowledges it, sends its first heartbeat (see [Lease hand-off](#lease-hand-off)), and pushes a `resume` event into the Valkey queue carrying the heartbeat token.
- The poller does not wait for the sandbox to wake up or execute; it immediately returns to the polling loop to claim subsequent work items.

### Lease hand-off

The work queue's contract, as the API exposes it, has three steps a worker must take on every item it polls. A poll claims the item and returns it with a per-delivery `secret`. An item that is not acknowledged is re-delivered to a later poll once it is older than `reclaim_older_than_ms` (5 seconds by default). `POST .../work/{id}/ack` confirms the claim, and `POST .../work/{id}/heartbeat` keeps the item's lease alive: the first heartbeat sends `expected_last_heartbeat=NO_HEARTBEAT`, and every later one must echo the `last_heartbeat` string the previous response returned, or the API answers `412 Precondition Failed`. An item that has been acknowledged but never heartbeated is never reclaimed by the API.

Those rules shape the bridge in two ways. First, the poller must acknowledge every item it enqueues; otherwise the item stays reclaimable and the poller's own next polls re-deliver it after the reclaim window, which produces duplicate `resume` events. Second, acknowledging alone is not enough: if the controller crashed between the ack and the actor start, the acknowledged item would never be heartbeated and the session would wait forever. So the poller also establishes the lease before the hand-off, and the in-actor worker continues that lease instead of claiming a new one:

1. The poller polls and receives the item (and its `secret`).
2. The poller sends `ack`.
3. The poller sends the first heartbeat with `expected_last_heartbeat=NO_HEARTBEAT` and `desired_ttl_seconds` set to `--handoff-ttl-seconds` (default 120). The response's `last_heartbeat` is the lease token.
4. The poller enqueues `{action: resume, session_id, work_id, last_heartbeat}`.
5. The controller restores the actor and sends `POST /work` with `{session_id, work_id, last_heartbeat}`.
6. The worker inside the actor sends its first heartbeat with `expected_last_heartbeat` set to that token, then heartbeats on its own schedule, echoing each response as usual.

The hand-off TTL is the time the controller has to bring the actor up. If the worker starts late, one of three things happens on its first heartbeat:

- The lease is still held: the heartbeat succeeds and the worker continues normally.
- The lease lapsed but nobody has re-claimed the item: the API answers `200` with `lease_extended: false` and `state: "stopped"`, and the worker exits.
- The lease lapsed and the item was requeued and picked up by a later poll: the API answers `412`, because the token no longer matches. The worker exits without force-stopping the item, since the item now belongs to the queue or to another actor.

The poller sets `reclaim_older_than_ms` to the API default of 5000 explicitly; since every enqueued item is acknowledged right after the poll, that window only needs to cover the poll-to-ack round trip. An item whose ack fails is skipped and left for the API to re-deliver. An item whose ack succeeded but whose first heartbeat failed is force-stopped by the poller, because nothing else would ever release it.

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

- Once the actor container is running and healthy (passing `/healthz` on port 80), the controller sends an HTTP `POST /work` request with the JSON payload `{ "session_id": "...", "work_id": "...", "last_heartbeat": "..." }`.
- The request is routed through Substrate's network router (`atenet-router`), which inspects the session ID and directs the HTTP traffic to the specific actor.

### Running the Work

- Inside the actor sandbox, the `ate-ant run` daemon receives the `POST /work` request on `:80`.
- It immediately acknowledges the notification with `200 OK` (`{"status":"accepted"}`) so intermediate routers and proxies do not time out.
- In the background, it runs the Anthropic SDK's environment worker in-process, quoting the `last_heartbeat` token so the worker continues the lease the poller took (see [Lease hand-off](#lease-hand-off)).
- The worker connects to Anthropic, executes all tool calls inside the actor (running commands in bash, modifying files, executing skills), streams results back to Anthropic, and heartbeats the lease while it runs.
- When all tool calls for the turn are complete and the idle timeout expires, the worker stops the work item and returns.
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

###! Idleness

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

- **Work item acknowledgment**: Done; see [Lease hand-off](#lease-hand-off). With the CMA queue holding the claim and the lease, the Valkey queue could be removed.
- **Completion with error status**: The ability to complete events with an error status so tool calls fail rather than hanging forever.
- **Pollable sleep event**: Delivering an "sleep" event through the polling API alongside work items. Managed Agents should serve as the single source of truth rather than requiring external serialization of these lifecycle events.
- **Allow rescheduling under load**: Similar to model request call reschedules, it would be great if there is a way to automatically reschedule work at a later time.

In addition to this work, we're also considering an official garbage collector for Agent Substrate and will remove our current bespoke implementation once that's live.
