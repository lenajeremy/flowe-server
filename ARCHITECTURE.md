# Architecture: Current vs Proposed

## Overview

This document describes the current architecture of the workflow AI server, the problems with it at scale, and the proposed event-driven redesign.

---

## Current Architecture

### How It Works

The server is a single Go monolith built on Gin. When a workflow run is triggered, the HTTP handler directly calls the workflow runner, sets up an SSE stream, and forwards events inline to the connected client. Everything happens in one process, in memory.

```
Client
  │
  ▼
HTTP Handler (Gin)
  ├─ Creates WorkflowRun record in PostgreSQL
  ├─ Calls executor.RunWorkflow() directly
  ├─ Emit callback:
  │   ├─ Publishes event to hub.Global (in-memory)
  │   └─ Writes event to SSE stream (if direct connection)
  └─ After run: persists all events to PostgreSQL, clears hub buffer
```

### Problems

1. **Tight coupling** — HTTP handler, runner, and SSE are tangled together
2. **In-memory hub doesn't survive restarts** — all in-flight events are lost on crash
3. **Can't scale horizontally** — hub is per-process; two instances can't share it
4. **Execution and delivery are coupled** — runner must be aware of whether a client is connected
5. **Redis is in the stack but does nothing useful**

---

## Proposed Architecture

### Design Goals

- Decouple the workflow runner from event delivery entirely
- Use Kafka as the event backbone for fan-out, durability, and replay
- Use Redis for ephemeral state (workflow node state, connected clients, run status)
- Keep the system as a single deployable Go binary with clearly separated internal modules
- Make the workflow runner an injectable dependency so it can be swapped out or moved to a separate service later without restructuring the rest of the system

### High-Level Flow

```
Client
  ├─ 1. Connect to Client Event Manager (SSE) with userID + workflowID
  └─ 2. POST /workflow/:id/run
              │
              ▼
            Server → fetches workflow JSON from PostgreSQL
              │
              ▼
         Workflow Runner
              ├─ Reads/writes node state to Redis
              ├─ Executes nodes
              └─ Publishes ExecutionEvents to Kafka
                           │
              ─────────────┴─────────────
              │                         │
              ▼                         ▼
       Log DB Consumer         Client Message Reader
              │                         │
              ▼                         ▼
         PostgreSQL             Client Event Manager → Client (SSE)
```

### Components

---

#### Workflow Runner (injectable module)

The runner is structured as an internal module with a defined interface. The server injects it at startup. This means the runner can be replaced with a different implementation (e.g. a remote gRPC runner, a sandboxed executor) without touching the rest of the system.

**Responsibilities:**
- Accept a workflow definition and a run context
- Topologically sort the DAG and execute nodes in order
- Read and write per-node state to Redis, namespaced by `workflowID:runID`
- Publish `ExecutionEvent` messages to the Kafka topic `workflow-events`
- Clear its Redis state when the run completes, fails, or is stopped

**What it does NOT do:**
- Know about clients or SSE connections
- Write to the log database
- Manage HTTP concerns

**Interface (conceptual):**
```go
type Runner interface {
    Run(ctx context.Context, run RunContext) error
}

type RunContext struct {
    RunID      string
    WorkflowID string
    Workflow   WorkflowDefinition
    Input      map[string]string
}
```

---

#### Kafka Topic: `workflow-events`

Single topic that carries all execution events from the runner. Each message includes:
- `runID`
- `workflowID`
- `userID`
- `ExecutionEvent` payload (type, nodeID, output, timestamp, etc.)

Two independent consumer groups read from this topic:

1. **`log-writer` group** — persists every event to the log database
2. **`client-reader` group** — forwards relevant events to the Client Event Manager

Because they use different consumer groups, both process every message independently.

---

#### Log DB Consumer

**Responsibilities:**
- Consume all events from `workflow-events`
- Write them to PostgreSQL (`workflow_run_events` table, one row per event)
- This is the source of truth for run history and replay of finished runs

---

#### Client Message Reader

**Responsibilities:**
- Consume events from `workflow-events`
- For each event, check the Redis connections map: is `userID:workflowID` currently connected?
- If yes, forward the event to the Client Event Manager
- If no, discard (the log DB consumer already persisted it)

This check prevents wasted work when no client is watching a run.

When a new client connects or disconnects, the Client Event Manager signals the reader so it can update its local view of connected clients (backed by Redis).

---

#### Client Event Manager

**Responsibilities:**
- Manage SSE connections from clients
- Clients connect with `userID + workflowID` as the subscription key
- On connect:
  1. Add `userID:workflowID` to the Redis connections map
  2. Check the Redis workflow status cache: is this workflow currently running?
  3. If running: fetch event history from the log DB for this run and stream it to the client, then switch to live events
  4. If idle: wait for the runner to start a run
- On disconnect:
  1. Remove `userID:workflowID` from the Redis connections map
  2. Signal the Client Message Reader

---

#### Redis (three distinct uses)

**1. Workflow execution state**
- Key: `state:{workflowID}:{runID}:{nodeID}`
- Value: node output (string)
- Written by the workflow runner during execution
- Cleared by the runner on completion/failure

**2. Workflow status cache**
- Key: `workflow:status:{workflowID}`
- Value: `idle | running | failed | completed`
- Written by the runner on state transitions
- Used by the Client Event Manager on client connect to decide whether to replay history

**3. Connections map**
- Key: `connections:{userID}:{workflowID}`
- Value: presence flag (TTL-based or removed on disconnect)
- Written by the Client Event Manager on connect/disconnect
- Read by the Client Message Reader to filter Kafka events

---

#### PostgreSQL

Single database for all persistent storage:
- Workflow definitions (nodes + edges as JSONB) — read at run start
- Every `ExecutionEvent` as a row, written by the Log DB Consumer

Used for:
- Run history (list of past runs, event details)
- Replay when a client connects mid-run or after a run finishes
- Audit trail

---

### Handling Late Subscribers

When a client connects after a run has already started:

1. Client Event Manager checks Redis workflow status → `running`
2. Queries log DB for all events emitted so far for this run
3. Streams those events to the client immediately
4. Switches to live forwarding from the Client Message Reader

This replaces the current `hub.go` buffer approach. The log DB is the replay source rather than in-memory state.

---

### Workflow Runner as Injectable Dependency

The runner is defined behind an interface and injected into the server at startup:

```go
// Server wires this up at boot
runner := executor.NewRunner(redisClient, kafkaProducer)
server := api.NewServer(db, runner, ...)
```

To swap the runner (e.g. move it to a remote service), only the injection site changes. All handler code calls `runner.Run(ctx, runCtx)` and is otherwise unaware of the implementation.

---

### Deployment

Single deployable Go binary. External dependencies:

| Dependency | Role |
|---|---|
| **Kafka** | Event backbone — decouples runner from consumers |
| **Redis** | Ephemeral state — execution state, connections, workflow status |
| **PostgreSQL** | Persistent storage — workflow definitions, run history, events |

Single database (PostgreSQL) for everything persistent. No separate NoSQL store needed — workflow definitions are already stored as JSONB columns and that continues to work fine.

---

### Failure Points & Mitigations

| Component | Failure Impact | Mitigation |
|---|---|---|
| **Kafka** | Events lost in transit; log DB and client both stop receiving | Kafka replication (min 2 brokers); producer retries with idempotency |
| **Redis** | Execution state lost mid-run; client routing breaks | Redis Sentinel or Cluster for automatic failover |
| **Log DB Consumer crash** | Events not persisted during downtime | Kafka offset not committed until write succeeds; consumer restarts and replays |
| **Client Message Reader crash** | Clients stop receiving live events | Restart re-subscribes to Kafka from last committed offset |
| **Client Event Manager crash** | All SSE connections drop | Clients reconnect; connections map rebuilds automatically |
| **Workflow Runner crash mid-run** | Run orphaned; Redis state left dirty; status stuck at `running` | Heartbeat + watchdog: if runner stops emitting events, mark run as `failed`, clean Redis |

---

## Summary Table

| Concern | Current | Proposed |
|---|---|---|
| Event delivery | In-memory hub, SSE inline | Kafka → two consumers |
| Execution state | In-memory `map[string]string` | Redis, namespaced per run |
| Run persistence | JSONB blob after run completes | Per-event rows written in real time |
| Late subscriber replay | In-memory hub buffer | Log DB query on connect |
| Client routing | Direct SSE from handler | Event Manager + Redis connections map |
| Runner coupling | Tightly coupled to HTTP handler | Interface-based, injected at startup |
| Horizontal scaling | Not supported | Supported (Kafka + Redis are shared) |
| Restart resilience | Events lost on restart | Kafka retains; consumers replay from offset |
