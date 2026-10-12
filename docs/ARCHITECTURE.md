# Architecture

What agentlite is made of, how a run flows through it, and where every piece of
state lives.

## Components

```
browser (agentui)
   │  REST: start runs, decide interrupts, cancel, threads, connectors
   │  WebSocket /v1/agent/chat?v=1: subscribe to threads, receive AG-UI events
   ▼
Echo server (internal/server, internal/handler, internal/ws)
   │
   ├─ run service (internal/agent): start, resume, cancel, subscribe; worker and
   │    finalizer loops; everything it shares goes through Valkey
   │      │
   │      └─ engine (internal/engine): builds adk agents from the roster and runs
   │           them with adk's runner; the encoder (internal/stream) turns adk
   │           events into AG-UI events written to the thread's log
   │
   ├─ pass-through proxies (internal/proxy): /v1/chat/completions, /v1/responses,
   │    /v1/messages, /v1/embeddings → the configured provider, bytes untouched
   │
   ├─ Valkey: event logs, runs, queues, leases, adk sessions, interrupt records
   └─ OpenSearch: threads, messages (the stored AG-UI events), memories
```

`internal/bootstrap` wires all of this from configuration; `cmd/server` serves
it; `cmd/dev` runs adk's own launcher over the same pieces.

## The life of a run

```
POST /v1/agent/chat {messages, thread_id?, agent_id?, model?, connectors?}
  │
  Service.Start
  ├─ claim the thread: SET thread:{id}:run = runId NX (a 60 s lease); a second
  │  start while a run owns the thread is 409 (a retry of the same message id
  │  returns the existing run)
  ├─ create the thread when none is named; save the user message (OpenSearch)
  ├─ rebuild the thread's adk session from stored events if it is missing
  ├─ record the run (run:{id}) with the log cursors where its turn starts
  └─ enqueue on runs:{agentId}                       → 202 {run id, thread id, cursor}

client: subscribe {threadId, after: cursor} on the socket

worker (consumer group "workers" on runs:{agentId}, up to 8 at a time)
  ├─ status running; keep the lease alive; listen for cancel on run:{id}:ctl
  ├─ engine.Run: build the agent tree, run adk's runner, encode each adk event
  │  into AG-UI events, append them to thread:{id}:log
  └─ settle: status done | error | cancelled | awaiting_input; enqueue runs:done

finalizer (consumer group "finalizers" on runs:done)
  ├─ fold the turn's events from the log, coalesce deltas, upsert the assistant
  │  message in OpenSearch
  └─ on a terminal status: release the lease (only if it still names this run),
     trim the log
```

The socket subscription reads `thread:{id}:log` from the cursor and stops at the
segment's `RUN_FINISHED` or `RUN_ERROR`. Because the log is the source of truth,
a client that reconnects, reloads mid-answer or opens the thread on another device
resumes from its cursor and loses nothing.

A run that pauses (see HITL.md) keeps its lease permanently until it is answered
or cancelled; the answer queues a new **segment** of the same run, which streams
into the same assistant message.

Queue entries are acknowledged when the handler returns. An entry whose handler
failed, or whose consumer died, is retried once the queue is idle and the entry
is older than twice the lease. A run found still running on redelivery is left
alone while its lease names it (its worker is alive and the run is just long);
otherwise it is closed with `RUN_ERROR` ("interrupted by a restart"), never
re-executed, because tools have side effects.

## Threads

A thread is a conversation: a root thread the user started, or a child thread a
`task` call started for a sub-agent (`parent_id`, `root_id`). Ids are UUIDv7 and
always the server's. Each thread has:

- one **event log** in Valkey (`thread:{id}:log`), the live channel and the
  source the finalizer folds from, trimmed to 24 h after each run;
- one **adk session** in Valkey (the model's view of the conversation);
- its **messages** in OpenSearch (one document per turn: the coalesced AG-UI
  events plus the extracted text);
- at most one **active run**, held by the lease `thread:{id}:run`;
- the **connectors** the user switched on, stored on the thread document.

Deleting a root thread deletes its child threads, their messages and their
memories.

## Streaming

The socket carries frames `{type: "event", threadId, seq, event}`, where `event`
is an AG-UI 1.0 event untouched, plus `idle` and `error`. One socket per browser
tab serves every thread the tab follows. AG-UI-RENDERING.md maps every event to
the UI.

## Where state lives

Valkey is split into logical databases, set by environment:

| Env | Default | Holds | Keys |
|---|---|---|---|
| `REDIS_DB_RUNS` | 1 | the run lifecycle | `thread:{id}:log` (stream of AG-UI events), `thread:{id}:seq` (message order), `thread:{id}:run` (lease), `run:{id}` (run record, 7 days), `runs:{agentId}` and `runs:done` (queues), `run:{id}:ctl` (cancel, pub/sub) |
| `REDIS_DB_SESSIONS` | 2 | adk sessions: agent state and history | `session:{id}` (metadata), `session:{id}:events`, `session:{id}:state` (30 days), `sessions:{app}` (index for adk's launcher) |
| `REDIS_DB_HITL` | 3 | interrupts | `hitl:{interruptId}` (record and decision, 7 days), `hitl:thread:{id}:pending` |

OpenSearch indices: `threads`, `messages` (events stored unindexed), `memories`
(kNN vectors; created only when memory is configured). Every write refreshes the
index, so a read after a save sees it.

## Configuration

Environment (`internal/config/config.go`): `PORT`, `HOST`, `APP_NAME`,
`CONFIG_DIR`, `LOG_LEVEL`, `LOG_JSON`, `ALLOWED_ORIGINS`, `REDIS_HOST`,
`REDIS_PORT`, `REDIS_USER`, `REDIS_PASS`, `REDIS_DB_RUNS`, `REDIS_DB_SESSIONS`,
`REDIS_DB_HITL`, `OPENSEARCH_URL`, `OPENSEARCH_USERNAME`, `OPENSEARCH_PASSWORD`,
`OTEL_EXPORTER_OTLP_ENDPOINT` (tracing), and whatever the YAML files reference
(`PROXY_PROVIDER`, provider base URLs and keys, `OPEN_SLIDE_MCP_URL`).

Files in `CONFIG_DIR`: `models.yaml` (providers, discovery, `default_model`,
`compaction`, `memory`, `proxy`), `mcp.yaml`, `connectors.yaml`, `agents/*.md`.

## Observability

With `OTEL_EXPORTER_OTLP_ENDPOINT` set, adk's own telemetry exports traces (Jaeger
in `docker-compose.yaml`, UI on :16686): a `run` span per run segment with adk's
`invoke_agent`, `generate_content` and `execute_tool` spans inside, and
`memory.search`. The trace context travels through the request queue, so a
sub-agent's run nests under its parent's `task` call.

## Designed to split later

Everything the worker and finalizer need is in Valkey, and they talk to the rest
only through it, so they can move into separate worker containers: point more
consumers at the same queues. The persistence sink (`agent.Sink`) and the event
log (`agent.Log`) are interfaces so OpenSearch writes can become a Kafka producer
and the log can stream elsewhere without touching the run service.
