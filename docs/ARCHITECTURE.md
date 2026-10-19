# Architecture

What agentlite is made of, how a run flows through it, and where every piece of
state lives. Who may do what, and how every OpenSearch call carries a user's
token, is AUTH.md.

## Components

```
browser (agentui)
   │  through a gateway that signs the user in and adds their access token
   │  (Authorization: Bearer) to every request; locally agentlite-infra's dev
   │  gateway on :8000 (AUTH.md)
   │  POST /v1/agent/chat: create a thread with its first turn
   │  one WebSocket /v1/agent/chat?v=1: run, answer, cancel, subscribe, unsubscribe
   │  frames on existing threads; AG-UI events stream back per thread
   │  REST: reads and thread admin (threads, messages, models, agents, connectors)
   ▼
Echo server (internal/server, internal/handler, internal/ws), :8010 locally;
   │  every route but /health checks the token (internal/auth)
   │
   ├─ runs (internal/runs): the run lifecycle. Manager starts runs, answers
   │    interrupts, cancels, and follows threads; the worker loops execute
   │    them and persist each segment, with the conversation's thread token
   │    (internal/tokens). Everything else they share goes through Valkey.
   │      │
   │      └─ agents (internal/agents): the agent runtime. Builds adk agents from
   │           the roster and runs one segment of a run with adk's runner; the
   │           encoder (internal/stream) turns adk events into AG-UI events
   │           written to the thread's log
   │
   ├─ pass-through proxy (internal/passthrough, forwarded by internal/providers):
   │    /v1/chat/completions, /v1/responses, /v1/messages, /v1/embeddings,
   │    /v1/images/generations, /v1/images/edits (a multipart form), /v1/decisions,
   │    /v1/systemone → the provider the
   │    body's model names, for models of the endpoint's types, with the model
   │    rewritten to the provider's own id (MODELS.md)
   │
   ├─ Valkey: event logs, runs, queues, leases, adk sessions, interrupt records
   ├─ OpenSearch: threads, messages (the stored AG-UI events), sources, artifacts,
   │    counters (each conversation's last source number and artifact versions), skills,
   │    workspaces, userfiles, userfile_chunks, memories; every call with a user's
   │    token, under document-level security on each document's control set
   ├─ Keycloak: issues users' access tokens (the server checks them with its
   │    keys) and exchanges them for thread tokens
   └─ S3 (MinIO locally): artifact bodies, skill files, uploaded files
```

`internal/bootstrap` wires all of this from configuration; `cmd/server` serves
it; `cmd/dev` runs adk's own launcher over the same pieces. `runs` defines the
small interface it needs from the runtime (`runs.Runtime`), and `agents`
implements it, so imports run one way.

| Package | Files |
|---|---|
| `internal/runs` | `manager.go` (Manager and the interfaces it needs, a segment's caller), `start.go`, `answer.go`, `cancel.go`, `subscribe.go` (subscribe, wait, statuses), `worker.go` (worker loops, executing a segment), `finalizer.go`, `queue.go`, `log.go`, `store.go` (run records, leases), `run.go` |
| `internal/auth`, `internal/tokens` | checking access tokens, the principal, `X-Control-Set`; thread tokens: exchange, memory cache, refresh (AUTH.md) |
| `internal/agents` | `runtime.go` (Runtime, running a segment), `build.go` (roster → adk agents and tools), `connectors.go` (a client and toolset per connector), `mcptools.go` (the MCP tool hooks), `compaction.go`, `seed.go` (rebuild a session), `resume.go` (answers → adk responses) |
| `internal/sources`, `internal/artifacts` | numbering and storing sources; storing artifacts (TOOLS.md) |
| `internal/images` | `images.go` (making an image, keeping it as an artifact); its tool is `internal/tools/generate_image.go` (TOOLS.md) |
| `internal/rag/userfiles` | `userfiles.go` (keeping a file, what is accepted), `resolve.go` (`#id`), `handler.go` (the routes), `index.go` (what is embedded, chunks), `search.go` (the search behind `search_userfiles`, whose tool is `internal/tools/search_userfiles.go`) (FILES.md) |
| `internal/providers` | provider clients, model resolution, discovery, the pass-through, `embed.go` (the one embedder, memory's and the files', MODELS.md) |
| `internal/voice` | the voice prompt: `voice.go` (a recording to text); apart from the agents; its route, `POST /v1/voice/transcriptions`, is `internal/handler/voice.go` (MODELS.md) |
| `internal/suggest` | next-prompt suggestions: `suggest.go` (from the last turn, typed text or both, one call to a small LLM); apart from the agents; its route, `POST /v1/suggestions`, is `internal/handler/suggestions.go` (MODELS.md) |
| `pkg/toolx`, `pkg/citations`, `pkg/transport`, `pkg/s3`, `pkg/modelcatalog` | the tool wrapper; the source contract; HTTP clients; S3 through minio-go; models and model cards |

## The life of a run

```
client: POST /v1/agent/chat {agentId?, model?, connectors?, imageModel?, messages}   (a new thread)
     or {type: "run", requestId, threadId, agentId?, model?, connectors?, imageModel?, messages}
  │
  Manager.Start (internal/runs/start.go), with the user's own token
  ├─ validate the agent and connectors; for the POST, create the thread in the
  │  control set X-Control-Set names
  ├─ for a user's own turn, make sure a thread token is kept for the
  │  conversation and user, minting it from the request's token (AUTH.md)
  ├─ claim the thread: SET thread:{id}:run = runId NX (a 10 min lease while it
  │  waits in the queue, cut to 60 s once a worker runs it); a second
  │  start while a run owns the thread is `busy` (a retry of the same message id
  │  returns the existing run)
  ├─ save the user message (OpenSearch); rebuild the adk session if it is missing
  ├─ record the run (run:{id}) with the log cursors where its turn starts and
  │  the thread's control set, and its status queued (run:{id}:status)
  └─ enqueue on runs:{agentId}, with the user and their token
                                     → 201 {threadId, runId, seq}, or
                                       {type: "ack", requestId, threadId, runId, seq}

client: {type: "subscribe", threadId, after: seq}

worker (consumer group "workers" on runs:{agentId}, up to 32 at a time)
  ├─ the segment's caller: the thread token kept for the conversation and the
  │  entry's user (minted from the entry's token if none is kept; none to be
  │  had fails the run: sign in again), confined to the thread's control set
  ├─ status queued → running; keep the lease alive; listen for cancel on run:{id}:ctl
  ├─ Runtime.Run: build the agent tree, run adk's runner, encode each adk event
  │  into AG-UI events, append them to thread:{id}:log
  ├─ settle: status done | error | cancelled | awaiting_input
  └─ finalize (finalizer.go, in the same process: only it holds the token):
     fold the turn's events from the log, coalesce deltas, upsert the assistant
     message in OpenSearch; on a terminal status release the lease (only if it
     still names this run) and trim the log
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
carries its user's access token (AUTH.md), so none outlives its run. An entry whose handler
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
whose thread, skill or workspace id is not a UUIDv7 gets 400. Documents that must not be stored twice
(a conversation's sources and artifacts) have name-based UUIDs instead. Each thread has:

- one **event log** in Valkey (`thread:{id}:log`), the live channel and the
  source the finalizer folds from, trimmed to 24 h after each run;
- one **adk session** in Valkey (the model's view of the conversation);
- its **messages** in OpenSearch (one document per turn: the coalesced AG-UI
  events plus the extracted text);
- at most one **active run**, held by the lease `thread:{id}:run`;
- the **connectors** the user switched on, stored on the thread document (connectors.yaml's,
  plus the server's own `images` and `files`, TOOLS.md);
- for a root thread, the **workspace** it is filed under (`workspace_id`), if any, and
  whether it is **pinned** (top of the sidebar) or **archived** (left out of it), set with
  `PATCH /v1/threads/{id}` like the title, connectors and workspace.

A thread is in one control set, fixed when it is created (`X-Control-Set`), and
everything it records is in it too (AUTH.md). Renaming, moving, pinning,
archiving or deleting a thread is its creator's alone.

A workspace (`internal/handler/workspaces.go`, index `workspaces`) is a folder of
conversations, and of uploaded files (FILES.md), with a name, in a control set; deleting one moves its
threads and files out of it rather than deleting them (only those the deleter can
see; a workspace id that names no workspace a user sees is none to them). Renaming
or deleting it is its creator's alone. Later it is what people share; today it only groups the sidebar.

A sub-agent's thread is followed like any other; the parent's agent card also
mirrors its status, progress and pending requests (AGENT-ARCHITECTURE.md).

Deleting a root thread (`DELETE /v1/threads/{id}`; a child thread is refused, 400)
deletes its child threads, their messages and memories, and their logs, counters
and adk sessions in Valkey. Its sources and artifacts stay, as the evidence of what
was said.

## Creating a thread

`POST /v1/agent/chat {messages, agentId?, model?, connectors?, workspaceId?}`, with
`X-Control-Set`, creates a thread in that control set with its first turn and starts the
run (an unknown workspace is 400). It replies `201 {threadId,
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
| `run {requestId, threadId, agentId?, model?, connectors?, imageModel?, messages}` | start a turn on the thread, idle or not yet started | `ack {threadId, runId, seq}` |
| `answer {requestId, threadId, interruptId, approved?, justification?, output?}` | answer an interrupt on that thread | `ack {threadId, runId, seq}` |
| `cancel {requestId, threadId}` | stop the thread's run | `ack {threadId}` |
| `subscribe {threadId, after?}` | follow a thread from a cursor (empty: the active turn's start) | `event` frames, or `idle` |
| `unsubscribe {threadId}` | stop following | — |

Server frames: `event {threadId, seq, event}` (an AG-UI 1.0 event, untouched),
`idle {threadId}`, `ack`, and `error {requestId?, threadId?, code, message}` with
codes `bad_request`, `unauthorized` (no thread token could be had: sign in
again), `forbidden`, `not_found`, `busy`, `not_paused`, `internal`. Runs,
answers and cancels are handled off the read loop, so a slow start never holds
up other frames. AG-UI-RENDERING.md maps every event to the UI.

**The socket's user.** The upgrade request is authenticated like any request;
the socket keeps that principal and closes when its token expires (`1008`): the
client reconnects and its gateway attaches a fresh token. A `run` frame starts
a turn for the socket's user, an `answer` resumes one for them (AUTH.md).

**Thread access.** The first frame a socket sends about a thread, normally its
`subscribe`, looks the thread up in OpenSearch with the user's own token
(`internal/ws/conn.go`, `allow`). A thread that is not found is refused with
`not_found` and the frame does nothing. Under document-level security a thread
the user may not read is not found, so the same lookup is the access check. A
thread found is remembered for the socket's life (which ends with the token):
later frames about it are not checked again, and a reconnect checks again.
Being let in to a thread gives its stream and its requests, nothing else; a
sub-agent's thread, in its root's control set, is checked on its own when the
client first touches it (opening it, or answering its request from the card).
The REST reads of a thread read it as the user too; the two whose data is only
in Valkey (its interrupts and its messages' cursor) look the thread up first.

## Where state lives

Valkey is split into logical databases, set by environment:

| Env | Default | Holds | Keys |
|---|---|---|---|
| `REDIS_DB_RUNS` | 1 | the run lifecycle | `thread:{id}:log` (stream of AG-UI events), `thread:{id}:seq` (message order), `thread:{id}:run` (lease), `run:{id}` (run record with the thread's `controlset_id`, 7 days), `run:{id}:status` (its status, 7 days), `runs:{agentId}` (queues; an entry carries its user's id, name and access token until it is handled), `run:{id}:ctl` (cancel, pub/sub) |
| `REDIS_DB_SESSIONS` | 2 | adk sessions: agent state and history | `session:{app}:{user}:{id}` (metadata), `…:events`, `…:state` (30 days), `state:{app}` and `state:{app}:{user}` (adk's `app:` and `user:` state), `sessions:{app}` (index for adk's launcher); adk's own `sessiontestsuite` runs against it |
| `REDIS_DB_HITL` | 3 | interrupts | `hitl:{interruptId}` (record and decision, 7 days), `hitl:thread:{id}:pending` (open), `hitl:thread:{id}:decided` (answered, waiting for the rest) |

OpenSearch indices: `threads`, `messages` (events stored unindexed), `sources`,
`artifacts`, `counters` (each conversation's source numbers and artifact versions),
`skills` (the skills library, one record per skill; its files are in S3; SKILLS.md),
`workspaces`, `userfiles` (the records of files users upload for RAG; FILES.md),
`userfile_chunks` (the files' chunks, kNN vectors; FILES.md), `memories` (long-term
memory entries, kNN vectors of the same model; MEMORY.md); `models.yaml` can rename these
two (`embedding.index`, `memory.index`).
Every name starts with `OPENSEARCH_INDEX_PREFIX` (default `user_ai_`:
`user_ai_threads`, `user_ai_userfiles`, …), added in one place
(`opensearch.Client.ix`), so deployments can share a cluster; `make indices` uses the
same variable. Every document carries `controlset_id`, `user_created_by` and
`user_updated_by`, and every call is made with a user's token, so document-level
security limits it to the token's control sets (AUTH.md). The service never creates or
changes an index, so users need no rights beyond reading and writing documents: each index's mapping is a file
in `deploy/opensearch`, applied with `make indices` (curl: it creates a missing index,
and adds new fields to an existing one; a changed type needs a reindex). Mappings are
`"dynamic": "strict"`, so a field missing from them fails the write instead of being
guessed. Each operation writes its query out in full next to the call that sends it.
Writes a read follows at once refresh the index (under document-level security a
get is not realtime either); memories, which nothing reads back straight away, do not.

S3 (`S3_ENDPOINT`, required; MinIO in `~/code/agentlite-infra`'s `docker-compose.yaml`): artifact bodies at
`artifacts/{root}/{artifact id}/{version}`, skill files at `skills/{skill id}/{path}`, and
uploaded files at `files/{file id}`.
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
`REDIS_DB_HITL`, `OPENSEARCH_URL` (no credentials: every call carries a user's token),
`OIDC_ISSUER_URL`, `OIDC_JWKS_URL`, `OIDC_AUDIENCE`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`,
`OIDC_TOKEN_URL`, `OIDC_EXCHANGE_CONTROLSET_PARAM` (AUTH.md),
`S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_BUCKET`, `S3_REGION`, `S3_USE_SSL`,
`TRANSPORT_SENSITIVE_HEADERS` (a caller's headers a forward drops),
`TRANSPORT_RESPONSE_HEADER_TIMEOUT` (default 5m: how long a provider, MCP server or tool may
take to answer before its body streams; the databases' clients are their own),
`OTEL_EXPORTER_OTLP_ENDPOINT` (tracing), and whatever the YAML files reference
(provider base URLs and keys, `OPEN_SLIDE_MCP_URL`), and `MODEL_CARDS_DIR`.

Files in `CONFIG_DIR`: `models.yaml` (providers, discovery, `default_model`,
`compaction`, `memory`), `connectors.yaml` (toolx tools and MCP servers),
`agents/*.md`. They are read strictly: an unknown key, an unset `${VAR}` in a header,
an unknown `api`, a tool an agent lists that nothing serves, a pipeline that contains
itself, or a missing default agent (`assistant`) fails the start.

## Observability

Logs are zerolog; Echo and adk log through slog, which `pkg/logging` routes to it.
Each request is logged by Echo's request logger with its real status and request
id, and a panic is logged with its stack. `make lint` runs `go vet` and staticcheck.

With `OTEL_EXPORTER_OTLP_ENDPOINT` set, adk's own telemetry exports traces (Jaeger
in `~/code/agentlite-infra`'s `docker-compose.yaml`, UI on :16686): a `run` span per run segment with adk's
`invoke_agent`, `generate_content` and `execute_tool` spans inside, and
`memory.search`. The trace context travels through the request queue, so a
sub-agent's run nests under its parent's `subagent` call.

## Designed to split later

Everything a worker needs is in Valkey, or in its queue entry (the user's
token, from which it mints the thread token it does not keep), so workers can
move into separate containers: point more consumers at the same queues. A
segment is finalized by the worker that ran it, because thread tokens live only
in that process's memory. The persistence sink (`runs.Sink`) and the event
log (`runs.Log`) are interfaces so OpenSearch writes can become a Kafka producer
and the log can stream elsewhere without touching the run lifecycle.
