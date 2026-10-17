# Architecture

What agentlite is made of, how a run flows through it, and where every piece of
state lives.

## Components

```
browser (agentui)
   │  POST /v1/agent/chat: create a thread with its first turn
   │  one WebSocket /v1/agent/chat?v=1: run, answer, cancel, subscribe, unsubscribe
   │  frames on existing threads; AG-UI events stream back per thread
   │  REST: reads and thread admin (threads, messages, models, agents, connectors)
   ▼
Echo server (internal/server, internal/handler, internal/ws)
   │
   ├─ runs (internal/runs): the run lifecycle. Manager starts runs, answers
   │    interrupts, cancels, and follows threads; the worker and finalizer loops
   │    execute and persist them. Everything they share goes through Valkey.
   │      │
   │      └─ agents (internal/agents): the agent runtime. Builds adk agents from
   │           the roster and runs one segment of a run with adk's runner; the
   │           encoder (internal/stream) turns adk events into AG-UI events
   │           written to the thread's log
   │
   ├─ pass-through proxy (internal/providers): /v1/chat/completions, /v1/responses,
   │    /v1/messages, /v1/embeddings, /v1/decisions, /v1/systemone → the provider the
   │    body's model names, for models of the endpoint's types, with the model
   │    rewritten to the provider's own id (MODELS.md)
   │
   ├─ Valkey: event logs, runs, queues, leases, adk sessions, interrupt records
   ├─ OpenSearch: threads, messages (the stored AG-UI events), memories, sources, artifacts,
   │    counters (each conversation's last source number and artifact versions), skills
   └─ S3 (MinIO locally): artifact bodies, skill files
```

`internal/bootstrap` wires all of this from configuration; `cmd/server` serves
it; `cmd/dev` runs adk's own launcher over the same pieces. `runs` defines the
small interface it needs from the runtime (`runs.Runtime`), and `agents`
implements it, so imports run one way.

| Package | Files |
|---|---|
| `internal/runs` | `manager.go` (Manager and the interfaces it needs), `start.go`, `answer.go`, `cancel.go`, `subscribe.go` (subscribe, wait, statuses), `worker.go` (worker loops, executing a segment), `finalizer.go`, `queue.go`, `log.go`, `store.go` (run records, leases), `run.go` |
| `internal/agents` | `runtime.go` (Runtime, running a segment), `build.go` (roster → adk agents and tools), `connectors.go` (a client and toolset per connector), `mcptools.go` (the MCP tool hooks), `compaction.go`, `seed.go` (rebuild a session), `resume.go` (answers → adk responses) |
| `internal/sources`, `internal/artifacts` | numbering and storing sources; storing artifacts (TOOLS.md) |
| `internal/providers` | provider clients, model resolution, discovery, the pass-through, embeddings (MODELS.md) |
| `pkg/toolx`, `pkg/citations`, `pkg/transport`, `pkg/s3`, `pkg/modelcatalog` | the tool wrapper; the source contract; HTTP clients; S3 through minio-go; models and model cards |

## The life of a run

```
client: POST /v1/agent/chat {agentId?, model?, connectors?, messages}   (a new thread)
     or {type: "run", requestId, threadId, agentId?, model?, connectors?, messages}
  │
  Manager.Start (internal/runs/start.go)
  ├─ validate the agent and connectors; for the POST, create the thread
  ├─ claim the thread: SET thread:{id}:run = runId NX (a 10 min lease while it
  │  waits in the queue, cut to 60 s once a worker runs it); a second
  │  start while a run owns the thread is `busy` (a retry of the same message id
  │  returns the existing run)
  ├─ save the user message (OpenSearch); rebuild the adk session if it is missing
  ├─ record the run (run:{id}) with the log cursors where its turn starts, and
  │  its status queued (run:{id}:status)
  └─ enqueue on runs:{agentId}       → 201 {threadId, runId, seq}, or
                                       {type: "ack", requestId, threadId, runId, seq}

client: {type: "subscribe", threadId, after: seq}

worker (consumer group "workers" on runs:{agentId}, up to 32 at a time)
  ├─ status queued → running; keep the lease alive; listen for cancel on run:{id}:ctl
  ├─ Runtime.Run: build the agent tree, run adk's runner, encode each adk event
  │  into AG-UI events, append them to thread:{id}:log
  └─ settle: status done | error | cancelled | awaiting_input; enqueue runs:done

finalizer (consumer group "finalizers" on runs:done)
  ├─ fold the turn's events from the log, coalesce deltas, upsert the assistant
  │  message in OpenSearch
  └─ on a terminal status: release the lease (only if it still names this run),
     trim the log
```

The subscription reads `thread:{id}:log` from the cursor and stops at the
segment's `RUN_FINISHED` or `RUN_ERROR`. Because the log is the source of truth,
a client that reconnects, reloads mid-answer or opens the thread on another device
resumes from its cursor and loses nothing.

A run's status is a key of its own, and every change is one compare-and-set
(`SET … IFEQ`): queued → running for the worker that takes it, queued or
awaiting_input → cancelled for a cancel, awaiting_input → queued for the last
answer. Whoever moves it first wins, and the other side falls back (a cancel
that loses to a worker stops the run as a running one). There is no Lua: the
lease is released with `DELIFEQ` (Valkey 9), so a late notice never releases a
newer run.

A run that pauses (see HITL.md) keeps its lease as long as its record (7 days)
until it is answered or cancelled; once every pending interrupt is answered
(`answer` frames), a new **segment** of the same run is queued, which streams into
the same assistant message.

Each consumer executes up to 32 entries at once. A parent waiting on its
sub-agents gives its slot back for the wait (`runs.Yield`), so waiting parents,
even on children that wait for the user, never fill an agent's slots.

Queue entries are acknowledged and deleted when the handler returns; an entry
may carry the user's `Authorization` (TOOLS.md), so none outlives its run. An entry whose handler
failed, or whose consumer died, is retried once the queue is idle and the entry
is older than twice the lease. A run found still running on redelivery is left
alone while its lease names it (its worker is alive and the run is just long);
otherwise it is closed with `RUN_ERROR` ("interrupted by a restart"), never
re-executed, because tools have side effects; when a newer run already holds the
thread, it is only marked as an error, and nothing is written into the newer run's
log. Acknowledgements are written even during a shutdown. Consumer groups are
created at start, before any request can arrive, and read from the stream's
beginning.

A shutdown cancels the worker loops and waits for their runs to settle and for
memory writes in flight; sockets are closed as going away (1001), so clients
reconnect and resume from their cursors.

## Threads

A thread is a conversation: a root thread the user started, or a child thread a
`subagent` call started for a sub-agent (`parent_id`, `root_id`). Ids are UUIDv7, like
every id the server makes (adk's event ids too), and always the server's; a path or frame
whose thread or skill id is not a UUIDv7 gets 400. Documents that must not be stored twice
(a conversation's sources and artifacts) have name-based UUIDs instead. Each thread has:

- one **event log** in Valkey (`thread:{id}:log`), the live channel and the
  source the finalizer folds from, trimmed to 24 h after each run;
- one **adk session** in Valkey (the model's view of the conversation);
- its **messages** in OpenSearch (one document per turn: the coalesced AG-UI
  events plus the extracted text);
- at most one **active run**, held by the lease `thread:{id}:run`;
- the **connectors** the user switched on, stored on the thread document.

A sub-agent's thread is followed like any other; the parent's agent card also
mirrors its status, progress and pending requests (AGENT-ARCHITECTURE.md).

Deleting a root thread (`DELETE /v1/threads/{id}`; a child thread is refused, 400)
deletes its child threads, their messages and memories, and their logs, counters
and adk sessions in Valkey. Its sources and artifacts stay, as the evidence of what
was said.

## Creating a thread

`POST /v1/agent/chat {messages, agentId?, model?, connectors?}` creates a
thread with its first turn and starts the run. It replies `201 {threadId,
runId, seq}` only after the thread document is written (with a refresh), so the
subscribe the client sends next finds it. Creation is HTTP rather than a socket
frame so a gateway or middleware can check the request's headers before the
thread exists; a browser cannot add headers to a WebSocket. Every later turn
goes over the socket.

## The socket

One WebSocket per browser tab, shared by every thread the UI shows, carries
everything between the client and its agents on existing threads. The socket
only reads the threads' logs in Valkey, never the model, so any server
instance can serve any socket. Each frame names its thread:

| Client frame | Purpose | Reply |
|---|---|---|
| `run {requestId, threadId, agentId?, model?, connectors?, messages}` | start a turn on the thread, idle or not yet started | `ack {threadId, runId, seq}` |
| `answer {requestId, threadId, interruptId, approved?, justification?, output?}` | answer an interrupt on that thread | `ack {threadId, runId, seq}` |
| `cancel {requestId, threadId}` | stop the thread's run | `ack {threadId}` |
| `subscribe {threadId, after?}` | follow a thread from a cursor (empty: the active turn's start) | `event` frames, or `idle` |
| `unsubscribe {threadId}` | stop following | — |

Server frames: `event {threadId, seq, event}` (an AG-UI 1.0 event, untouched),
`idle {threadId}`, `ack`, and `error {requestId?, threadId?, code, message}` with
codes `bad_request`, `not_found`, `busy`, `not_paused`, `internal`. Runs,
answers and cancels are handled off the read loop, so a slow start never holds
up other frames. AG-UI-RENDERING.md maps every event to the UI.

**Thread access.** The first frame a socket sends about a thread, normally its
`subscribe`, looks the thread up in OpenSearch (`internal/ws/conn.go`, `allow`).
A thread that is not found is refused with `not_found` and the frame does
nothing. With document-level security on, a thread the user may not read is
not found, so the same lookup is the access check. A thread found is remembered
for the socket's life: later frames about it are not checked again, and a
reconnect checks again. Being let in to a thread gives its stream and its
requests, nothing else; a sub-agent's thread is checked on its own when the
client first touches it (opening it, or answering its request from the card).

For the check to mean anything in production, the lookup must run as the user:
the OpenSearch client today uses the service account, which document-level
security does not restrict. Child threads must also carry whatever fields the
security rules match on, copied from the root. The REST reads of a thread
(`/v1/threads/{id}/messages` and the rest) are not covered by this check.

## Where state lives

Valkey is split into logical databases, set by environment:

| Env | Default | Holds | Keys |
|---|---|---|---|
| `REDIS_DB_RUNS` | 1 | the run lifecycle | `thread:{id}:log` (stream of AG-UI events), `thread:{id}:seq` (message order), `thread:{id}:run` (lease), `run:{id}` (run record, 7 days), `run:{id}:status` (its status, 7 days), `runs:{agentId}` and `runs:done` (queues), `run:{id}:ctl` (cancel, pub/sub) |
| `REDIS_DB_SESSIONS` | 2 | adk sessions: agent state and history | `session:{app}:{user}:{id}` (metadata), `…:events`, `…:state` (30 days), `state:{app}` and `state:{app}:{user}` (adk's `app:` and `user:` state), `sessions:{app}` (index for adk's launcher); adk's own `sessiontestsuite` runs against it |
| `REDIS_DB_HITL` | 3 | interrupts | `hitl:{interruptId}` (record and decision, 7 days), `hitl:thread:{id}:pending` (open), `hitl:thread:{id}:decided` (answered, waiting for the rest) |

OpenSearch indices: `threads`, `messages` (events stored unindexed), `memories`
(kNN vectors), `sources`, `artifacts`, `counters` (each conversation's source
numbers and artifact versions), `skills` (the skills library, one record per skill;
its files are in S3; SKILLS.md). The service never creates or changes an index, so it
needs no rights beyond reading and writing documents: each index's mapping is a file
in `deploy/opensearch`, applied with `make indices` (curl: it creates a missing index,
and adds new fields to an existing one; a changed type needs a reindex). Mappings are
`"dynamic": "strict"`, so a field missing from them fails the write instead of being
guessed. Each operation writes its query out in full next to the call that sends it.
Writes a read follows at once refresh the index;
memories, which nothing reads back straight away, do not.

S3 (`S3_ENDPOINT`, required; MinIO in `docker-compose.yaml`): artifact bodies at
`artifacts/{root}/{artifact id}/{version}`, and skill files at `skills/{skill id}/{path}`.
OpenSearch keeps only their records.

## Configuration

Environment (`config/config.go`; each storage package declares its own settings,
such as `valkey.Config`): `PORT`, `HOST`, `APP_NAME`, `APP_URL`
(this server as clients reach it, through any gateway), `CONFIG_DIR`, `LOG_LEVEL`
(debug, info, warn or error; empty or unknown fails the start), `LOG_JSON`, `ALLOWED_ORIGINS` (CORS),
`WS_ALLOWED_ORIGINS` (the page origins a browser may open the socket from; `*`
allows any, and clients that send no `Origin` are not checked),
`MAX_BODY_MB` (the largest request body, default 32), `REDIS_HOST`,
`REDIS_PORT`, `REDIS_USER`, `REDIS_PASS`, `REDIS_DB_RUNS`, `REDIS_DB_SESSIONS`,
`REDIS_DB_HITL`, `OPENSEARCH_URL`, `OPENSEARCH_USERNAME`, `OPENSEARCH_PASSWORD`,
`S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_BUCKET`, `S3_REGION`, `S3_USE_SSL`,
`TRANSPORT_SENSITIVE_HEADERS` (a caller's headers a forward drops),
`OTEL_EXPORTER_OTLP_ENDPOINT` (tracing), and whatever the YAML files reference
(provider base URLs and keys, `OPEN_SLIDE_MCP_URL`), and `MODEL_CARDS_DIR`.

Files in `CONFIG_DIR`: `models.yaml` (providers, discovery, `default_model`,
`compaction`, `memory`), `connectors.yaml` (toolx tools and MCP servers),
`agents/*.md`. They are read strictly: an unknown key, an unset `${VAR}` in a header,
an unknown `api`, a tool an agent lists that nothing serves, a pipeline that contains
itself, or a missing default agent (`assistant`) fails the start.

## Observability

With `OTEL_EXPORTER_OTLP_ENDPOINT` set, adk's own telemetry exports traces (Jaeger
in `docker-compose.yaml`, UI on :16686): a `run` span per run segment with adk's
`invoke_agent`, `generate_content` and `execute_tool` spans inside, and
`memory.search`. The trace context travels through the request queue, so a
sub-agent's run nests under its parent's `subagent` call.

## Designed to split later

Everything the worker and finalizer need is in Valkey, and they talk to the rest
only through it, so they can move into separate worker containers: point more
consumers at the same queues. The persistence sink (`runs.Sink`) and the event
log (`runs.Log`) are interfaces so OpenSearch writes can become a Kafka producer
and the log can stream elsewhere without touching the run lifecycle.
