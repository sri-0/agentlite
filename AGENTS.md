# AGENTS.md — `agentlite`

An LLM agent server in Go. It reverse-proxies the OpenAI-style endpoints to an upstream
gateway, and runs agents built on [adk-go v2](https://pkg.go.dev/google.golang.org/adk/v2)
behind one WebSocket that speaks [AG-UI 1.0](https://docs.ag-ui.com) events. Runs are
resumable, persisted as the same events, and sub-agents are real threads.

Read `docs/PLAN.md` for the design and its reasoning; this file is the operating manual.
Keep both current when you change protocols, config schemas or the agent roster.

---

## 1. Quick start

```bash
make up                     # Valkey :6379, OpenSearch :9200, Jaeger :16686 (docker-compose.yml)
cp .env.example .env.local  # set PROXY_PROVIDER and provider keys / base URLs
make run                    # :8000
make vet
```

**Tracing.** Set `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318` and every run exports an
OpenTelemetry trace to Jaeger (UI at http://localhost:16686, service `agentlite`): a `run`
root span (thread, run, agent) with `memory.search` and adk's own `invoke_agent`, `generate_content` and `execute_tool` spans
nested under it. The trace context travels through the Valkey request queue, so a sub-agent's
child run nests under its parent's `task` call. The export itself is adk's (`telemetry.New`
reads the OTLP environment); agentlite only adds the `run` and `memory.search` spans and the
propagator. Unset, nothing is exported.

`make dev web` (or `cli`, `api`) runs adk's own launcher over the same agents, sessions and
memory, for adk's event and trace views. It runs its own worker loops on the same queues, so
it must run the same code as the server; runs made there bypass the thread store.

Development verification is done with `curl` and a WebSocket client; there are no tests yet
(deliberate, see PLAN §7). Scripts used for that stay out of the repository.

## 2. Repository layout

```
cmd/server/            the API: bootstrap → handlers → Echo
cmd/dev/               adk's launcher (web UI, cli, api) over the same roster and stores
internal/
  bootstrap/           wires config, logging, tracing, registry, Valkey, OpenSearch, sessions, memory, MCP, engine, run service
  config/              env (go-envconfig) + models.yaml + mcp.yaml + connectors.yaml + agents/*.md
  server/              Echo app: middleware and routes
  handler/             HTTP handlers (threads, runs, interrupts, connectors, models, agents, proxy) and the socket upgrade
  ws/                  the agent chat socket: frames and the per-connection subscription loop
  agent/               run lifecycle: Service, Valkey log, request/done queues, worker, finalizer
  engine/              roster → adk agents, run execution, connectors as toolsets, compaction, session seeding
  hitl/                interrupt records (what a run paused on, how it was decided) in Valkey
  stream/              AG-UI conventions, adk→AG-UI encoder, storage coalescing, context sink
  tools/               world tools (one file each); tools/builtin holds the harness tools (task, todowrite, …)
  mcp/                 mcp.yaml → adk toolsets, with a way to list a server's tools
  proxy/               provider clients (mTLS, headers), model discovery, reverse proxies, embeddings
pkg/
  db/opensearch/       threads, messages, memories (kNN)
  db/valkey/           client
  memory/              adk memory.Service on OpenSearch (docs/MEMORY.md)
  session/valkey/      adk session.Service on Valkey
  logging/             zerolog + slog bridge
config/default/        models.yaml, mcp.yaml, connectors.yaml, agents/*.md (the roster)
docs/                  PLAN.md, AG-UI-RENDERING.md, MEMORY.md, RESEARCH-streaming.md
```

Agents are markdown files in `config/default/agents/`, not Go; the Go that builds and runs
them is `internal/engine`.

## 3. Request flow

**Pass-through** (`POST /v1/chat/completions | /v1/responses | /v1/messages | /v1/embeddings`):
`httputil.ReverseProxy` to the provider named by `proxy.provider` in `models.yaml` (per-route
`overrides`). Bodies are untouched; `Authorization` is replaced; streams flush as they arrive.

**Agent chat.** REST writes, the socket streams. A turn is two calls:

```
POST /v1/agent/chat                   { "model", "messages":[{"role":"user","content","id"?}],
                                        "thread_id"?, "agent_id"?, "connectors"? }      → 202 { id, thread_id, agent_id, model, status, cursor }
GET  /v1/agent/chat?v=1  (WebSocket)  { "type":"subscribe", "threadId", "after": cursor }
```

The run request is the chat-completions shape plus agentlite's fields. The last user message
is the turn and its optional `id` makes a retry idempotent. Without `thread_id` the server
creates the thread and names it on the run; with one, the thread must exist (ids are always
the server's). Socket frames:

```
client → { "type":"subscribe", "threadId", "after"? }   { "type":"unsubscribe", "threadId" }
server → { "type":"event", "threadId", "seq", "event": <AG-UI event> }   { "type":"idle", "threadId" }   { "type":"error", "threadId", "code", "message" }
```

The `event` is an AG-UI event, untouched. AG-UI defines events for one run over one
connection, so the envelope adds only what that leaves out: `threadId` (one socket carries
many threads) and `seq` (the log cursor to reconnect from); the run and agent are in
`RUN_STARTED.metadata`. One socket per client, any number of thread subscriptions; a
subscription ends at the segment's `RUN_FINISHED`/`RUN_ERROR`, and `subscribe` without
`after` replays the active turn from its start. Error codes: `bad_request`, `internal`.
Decisions on a paused run and cancels are REST too (§4).

Opening a thread in a client: `GET /v1/threads/{id}/messages` returns `{data, cursor}`; fold
each message's `events`, then `subscribe{after: cursor}` (`cursor` is `""` when idle).

**Run lifecycle** (`internal/agent`): `Start` claims the thread (`thread:{id}:run`, a lease),
persists the user message, rebuilds the thread's adk session from history if it is missing,
records the log cursors and enqueues on `runs:{agentId}`; a failure after the claim releases
it. The worker loop consumes the queue (up to eight entries at once per queue) and runs the engine, appends AG-UI
events to `thread:{id}:log`, records interrupts in `hitl`, and writes status plus `result` to
`run:{id}`. A `runs:done` consumer (the finalizer) folds the turn's events from the log,
coalesces deltas, upserts the assistant message document and only then releases the thread
(only if it still holds it), so a client never reads a thread as idle before its answer is
persisted (`Start` waits up to five seconds for that release). Both queues are Valkey consumer
groups: entries whose handler failed, or whose consumer died, are retried once idle; a
reclaimed `running` run is closed with `RUN_ERROR`, never re-executed. Cancel closes a queued
run directly, publishes on `run:{id}:ctl` for a running one, and closes a paused run's
interrupts. Shutdown waits for runs in flight; closing writes use a context that outlives the
run.

## 4. REST API

| Method | Path | Purpose |
|---|---|---|
| GET | `/health` | `{status, agents}` |
| GET | `/v1/models` | merged model list (`object: "model"`, `provider_id`, `capabilities`, …) |
| GET | `/v1/agents` | primary agents with their definitions |
| GET | `/v1/connectors` | the tool groups a thread can switch on: `{id, name, description, icon, type: tools \| mcp, url?, tools?: [{name, description}]}`; an MCP connector's tools are not listed here |
| GET | `/v1/connectors/{id}` | one connector in full; an MCP connector's `tools` are listed live from the server with `status: ok \| unreachable` and `error` |
| GET | `/v1/threads` | root threads, newest first (capped at 200), with `run_status` when a run is active |
| GET/PATCH/DELETE | `/v1/threads/{id}` | thread; `PATCH {title?, connectors?}`; delete removes it, its sub-agent threads and all their messages (`409` while a run is active) |
| POST | `/v1/agent/chat` | start a turn (§3), creating the thread when none is named → `202`; `404` unknown thread, `409` busy, `400` invalid |
| POST | `/v1/threads/{id}/cancel` | stop the active run → `202`; `404` when idle |
| GET | `/v1/threads/{id}/messages?before=&limit=` | `{data: [{id, thread_id, role, seq, created_at, text, events}], cursor}` |
| GET | `/v1/threads/{id}/interrupts` | the paused run's pending interrupts: `{id, run_id, reason, tool_name, args, message, status, created_at}` |
| POST | `/v1/threads/{id}/interrupts/{iid}` | decide one: `{approved, justification?}` or `{output}`; `202 {run_id, cursor}`, `404`, `400`, `409 not paused` |
| GET | `/v1/agent/chat?v=1` | the WebSocket above |

There is no authentication or identity in v1. Records carry an empty `user_id` for later.

## 5. Events (AG-UI 1.0)

Emitted by `internal/stream/encode.go` from adk session events, one segment per run:
`RUN_STARTED{metadata: turnId, agentId, model, createdAt}` · `STEP_STARTED/FINISHED{stepName}`
around each model call (and each workflow step, named after its agent) · `TEXT_MESSAGE_*` ·
`REASONING_*` (only with `api: responses` providers) · `TOOL_CALL_START/ARGS/END` and
`TOOL_CALL_RESULT` · `ACTIVITY_SNAPSHOT{activityType: task_list | artifact | agent}` ·
`SUBAGENT_STARTED/FINISHED/ERROR` · `CUSTOM{name: "approval"}` on resume ·
`RUN_FINISHED{outcome: success | interrupt{interrupts} | cancelled, usage, metadata.durationMs}` ·
`RUN_ERROR`.

Interrupts: every pause is adk's tool confirmation (`reason: "confirmation"`, `interruptId`
≠ the tool call id), with `metadata: {toolName, args, request?}`. Three things pause:

- a tool that needs approval (`write_note`, MCP tools in `require_confirmation`): answered
  `{approved, justification?}`;
- the `question` tool, which calls `RequestConfirmation` itself with `request: {toolName:
  "question", args: {questions}}`: answered `{output: {answers, text?}}`, which becomes its result;
- a `task` call whose sub-agent paused: the task tool keeps the child in session state and
  raises a confirmation whose `request` is the child's (`subagent`, `childThreadId`,
  `toolName`, `args`). The answer, approve, deny or output, is relayed to the child, which
  resumes; the task call keeps waiting and returns the child's result as usual. Cancelling the
  paused parent cancels the paused child.

Using confirmation for all three keeps adk's loop stopping reliably even when a pausing call
shares its turn with other calls. Every pause is recorded in `internal/hitl` (`hitl:{id}`,
`hitl:thread:{id}:pending`) with its decision; `POST /v1/threads/{id}/interrupts/{iid}` decides
one and resumes the run. Cancelling a paused run marks its interrupts `cancelled` and writes a
cancelled segment.

Storage keeps the same events, coalesced (`stream.Coalesce` merges contiguous deltas), one
OpenSearch document per message with the extracted `text`. `docs/AG-UI-RENDERING.md` tables
every event, which code emits it, and what the UI's reducer and components make of it.

## 6. Agents

One markdown file per agent in `config/default/agents/`: YAML frontmatter + the instruction.

```yaml
id, name, description                                 # the description is what a parent reads when delegating
kind: llm | sequential                                # default llm; sequential runs its subagents in order
mode: primary | subagent | all                        # default primary; subagent hides it from the picker
tools: [...]                                          # omitted ⇒ harness tools plus the thread's connectors; [] ⇒ none
subagents: [...]                                      # task-tool targets (llm) or steps (sequential)
max_output_tokens
```

Agents never name a model: a run uses the model the request names (`default_model` from
`models.yaml` when it names none), and that model flows to every step, to the compaction
summarizer and to child runs started by the `task` tool. Definitions are validated at boot.

The roster works the way opencode and Claude Code do: there is no classifier. `assistant` is
the default agent and the coordinator. It answers directly, uses tools and connectors, keeps a
task list with `todowrite` for multi-step work, and delegates through `task` to `deep-research`
(a sequential planner → researcher → report-writer pipeline), `researcher`, `data-analyst`,
`evaluator`, `planner` and `report-writer`. Several `task` calls in one turn run in parallel,
and each sub-agent's result returns as the tool result, so research can precede building in
the same turn. Sub-agents do the work they are given: without listing them they get neither
`task`, `todowrite` nor `question`, and cancelling a parent cancels its running children.
`chat` is the plain model; `deep-research` and `data-analyst` can also be picked directly.

**Connectors** (`config/default/connectors.yaml`) are the tool groups a user switches on: each
is a set of world tools or one MCP server, with an id, name, description and a lucide `icon`
name. Each connector
is one adk `tool.Toolset` (`internal/engine/connectors.go`; an MCP connector is the server's
`mcptoolset`), handed to the agent whole, so switching a connector off removes every tool in
it for that run. A thread with no choice has every connector on; a choice sent with a run or
a `PATCH` is stored on the thread (`connectors`, `[]` for none) and copied to the run and to
the child threads its sub-agents run on. An agent whose `tools:` are listed keeps exactly
those; one with `tools:` omitted gets the harness tools plus the enabled connectors' toolsets.

`task` dispatches a sub-agent on its own child thread (`parent_id`, same model and
connectors); several calls
in one turn run in parallel. The parent carries an `agent` activity card with status, bounded
progress lines (from the child's tool calls and `report_progress`) and the final text; the
child sees only its prompt. User-started runs get the `load_memory` tool when `memory:` is
configured, and `preload_memory` too when `memory.preload` is on; sub-agent runs get neither.
`docs/MEMORY.md` has the whole story.

World tools (`internal/tools`): `calculate`, `read_url` (public hosts only), `write_note`
(the approval demo; it only logs). Harness tools (`internal/tools/builtin`): `task`,
`todowrite`, `emit_artifact`, `question` (answered by the user), `report_progress`. MCP servers from
`mcp.yaml` (`endpoint` or `command`, `headers`, `tools`, `require_confirmation`; tools keep
their MCP names; a server that cannot list its tools costs a warning, not the run).

## 7. Configuration

Environment (`internal/config/config.go`): `PORT`, `HOST`, `APP_NAME`, `CONFIG_DIR`,
`LOG_LEVEL`, `LOG_JSON`, `ALLOWED_ORIGINS`, `REDIS_HOST/PORT/USER/PASS`,
`OPENSEARCH_URL/USERNAME/PASSWORD`, plus whatever `models.yaml` references (`PROXY_PROVIDER`,
`LITELLM_BASE_URL`, `*_API_KEY`, …). `${VAR}` expands in `base_url`, headers and
`proxy.provider`; `*_env` fields name the variable holding a secret or, for TLS, a file path.

`connectors.yaml`: `connectors[]` (`id, name, description, tools[] | mcp`); the file is
optional and every connector is validated at boot against the world tools and `mcp.yaml`.
`mcp.yaml`: `servers: {name: {endpoint | command, headers, tools, require_confirmation}}`;
the development server is open-slide-mcp (`OPEN_SLIDE_MCP_URL`, streamable HTTP on
`:8090/mcp`, run from `~/code/open-slide-mcp` with `make run`), exposed as the `slides`
connector. A stdio server gets a fresh process on every connect so adk can reconnect.

`models.yaml`: `providers[]` (`id, base_url, api_key_env, api: chat_completions|responses,
headers, discover: {endpoint, refresh}, models[], ssl_*`), `default_model`, `compaction:
{interval, overlap, token_threshold}` (adk compaction for every run; the threshold is a
fraction of the model's `context_length` and is skipped when that is unknown; omit the block
to disable), `memory: {embedding_model, dimensions, preload}`, `proxy: {provider, overrides}`.
Discovery reads `/models` (OpenAI shape, with OpenRouter/vLLM extras) or `/model/info`
(LiteLLM); YAML `models` entries override discovered ones field by field, and the first
provider in file order that serves an id wins. A provider whose `base_url` is empty in an
environment is skipped.

## 8. Storage

Valkey: `thread:{id}:log` (events, trimmed to 24 h after each finalise), `thread:{id}:seq`,
`thread:{id}:run` (lease), `run:{id}`,
`runs:{agentId}`, `runs:done`, `run:{id}:ctl` (cancel), `hitl:{id}` + `hitl:thread:{id}:pending`,
`session:{threadId}` (+`:events`, `:state`; the adk session, 30-day TTL, rebuilt from
OpenSearch if missing). OpenSearch: `threads`, `messages`, `memories` (kNN, created on boot);
every write forces a refresh so reads after a save see it. Deleting a thread removes its
family by `root_id`; its Valkey keys expire on their own.

## 9. Conventions

- One wire format, one encoder, one coalescer; the only reducer lives in the UI.
- REST writes (threads, runs, decisions, cancels); the socket only streams.
- Measure before tuning: `make up` brings Jaeger; the `memory.search` and `generate_content` spans
  show where a slow start goes.
- The engine and tools read and write Valkey only; they never import `pkg/db/opensearch`.
- Nothing in a run waits on OpenSearch; persistence happens in the finalizer.
- A run is executed at most once; tools have side effects.
- Named constants for cross-file strings; errors are logged at `warn`/`error`, never swallowed.
- Agents are built per run; there is no global registry of built agents.
- `gofmt`, `go vet` clean before committing.
