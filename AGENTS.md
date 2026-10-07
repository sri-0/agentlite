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
make up                     # Valkey :6379, OpenSearch :9200
cp .env.example .env.local  # set PROXY_PROVIDER and provider keys / base URLs
make run                    # :8000
make vet
```

Development verification is done with `curl` and a WebSocket client; there are no tests yet
(deliberate, see PLAN §7). Scripts used for that stay out of the repository.

## 2. Repository layout

```
cmd/server/            entrypoint: config → clients → engine → run service → Echo
internal/
  config/              env (go-envconfig) + models.yaml + mcp.yaml + agents/*.md
  server/              Echo app: middleware and routes
  handler/             HTTP + WebSocket handlers (threads, models, agents, proxy, chat socket)
  agent/               run lifecycle: Service, Valkey log, request/done queues, worker, finalizer
  stream/              AG-UI conventions, adk→AG-UI encoder, storage compaction, context sink
  tools/               function tools (one file each) and the task tool
  mcp/                 mcp.yaml → adk toolsets
  proxy/               provider clients (mTLS, headers), model discovery, reverse proxies, embeddings
agents/                roster → adk agents, run engine, Valkey session service, session seeding
pkg/
  db/opensearch/       threads, messages, memories (kNN)
  db/valkey/           client
  memory/              adk memory.Service on OpenSearch
  logging/             zerolog + slog bridge
config/default/        models.yaml, mcp.yaml, agents/*.md
docs/                  PLAN.md, RESEARCH-streaming.md
```

## 3. Request flow

**Pass-through** (`POST /v1/chat/completions | /v1/responses | /v1/messages | /v1/embeddings`):
`httputil.ReverseProxy` to the provider named by `proxy.provider` in `models.yaml` (per-route
`overrides`). Bodies are untouched; `Authorization` is replaced; streams flush as they arrive.

**Agent chat** (`GET /v1/agent/chat?v=1`, WebSocket; `426` on another version):

```
client → { "type":"run", "threadId", "message":{"id","content"}, "agentId"?, "model"? }
client → { "type":"run", "threadId", "resume":[{ "interruptId", "status":"resolved", "payload" }] }
client → { "type":"subscribe", "threadId", "after"? }   { "type":"unsubscribe", "threadId" }   { "type":"cancel", "threadId" }
server → { "type":"event", "threadId", "seq", "event": <AG-UI event> }   { "type":"idle", "threadId" }   { "type":"error", "threadId", "code", "message" }
```

One socket per client, any number of thread subscriptions. A `run` streams the new segment; a
subscription ends at the segment's `RUN_FINISHED`/`RUN_ERROR`; `subscribe` without `after`
replays the active turn from its start. Error codes: `busy`, `not_paused`, `not_found`,
`bad_request`.

Opening a thread in a client: `GET /v1/threads/{id}/messages` returns `{data, cursor}`; fold
each message's `events`, then `subscribe{after: cursor}` (`cursor` is `""` when idle).

**Run lifecycle** (`internal/agent`): `Start` persists the user message, rebuilds the thread's
adk session from history if it is missing, records the log cursors, claims the thread
(`thread:{id}:run`, a lease) and enqueues on `runs:{agentId}`. The in-process worker loop
consumes it, runs the engine, appends AG-UI events to `thread:{id}:log`, and writes status plus
`result` to `run:{id}`. A `runs:done` consumer (the finalizer) folds the turn's events from the
log, compacts deltas, upserts the assistant message document and only then releases the
thread, so a client never reads a thread as idle before its answer is persisted (`Start` waits
up to five seconds for that release). Both queues are Valkey consumer groups; stale entries are
reclaimed and a reclaimed `running` run is marked `error`, never re-executed. Cancel is a
pub/sub message on `run:{id}:ctl`.

## 4. REST API

| Method | Path | Purpose |
|---|---|---|
| GET | `/health` | `{status, agents}` |
| GET | `/v1/models` | merged model list (`object: "model"`, `provider_id`, `capabilities`, …) |
| GET | `/v1/agents` | primary agents with their definitions |
| GET | `/v1/threads` | root threads, newest first, with `run_status` when a run is active |
| GET/PATCH/DELETE | `/v1/threads/{id}` | thread; `PATCH {title}`; delete removes its messages |
| GET | `/v1/threads/{id}/messages?before=&limit=` | `{data: [{id, thread_id, role, seq, created_at, text, events}], cursor}` |
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

Interrupts: an approval tool pauses with `reason: "confirmation"` (`interruptId` ≠ the tool
call id); the `question` tool pauses with `reason: "input_required"` (`interruptId` = the tool
call id). Resume payloads: `{approved, justification?}` and the tool's output respectively.

Storage keeps the same events, compacted (`stream.Compact` merges contiguous deltas), one
OpenSearch document per message with the extracted `text`.

## 6. Agents

One markdown file per agent in `config/default/agents/`: YAML frontmatter + the instruction.

```yaml
id, name, description
kind: llm | sequential | parallel | loop | graph      # default llm
mode: primary | subagent | all                        # default primary; subagent hides it from the picker
model, provider                                       # provider optional: the model id is looked up across providers
tools: [...]                                          # omitted ⇒ every function tool and MCP tool; [] ⇒ none
subagents: [...]                                      # task-tool targets (llm) or steps (workflow kinds)
edges: [{from, to, route?}]                           # graph kind
max_iterations, max_output_tokens, output_key
compaction: {interval, overlap, token_threshold}      # adk compaction; threshold is a fraction of context_length
```

The default `assistant` decides per message whether to answer, use tools, or dispatch
`subagents` through the `task` tool (several calls in one turn run in parallel). A dispatched
sub-agent runs on its own child thread (`parent_id`); the parent carries an `agent` activity
card with status, bounded progress lines (from the child's tool calls and `report_progress`)
and the final text. Primary agents get `preload_memory` and `load_memory` when `memory:` is
configured in `models.yaml`.

Function tools: `calculate`, `read_url`, `todowrite`, `emit_artifact`, `write_note`
(approval demo), `question` (client-answered), `report_progress`, `task`. MCP servers from
`mcp.yaml` (`endpoint` or `command`, `headers`, `tools`, `require_confirmation`).

## 7. Configuration

Environment (`internal/config/config.go`): `PORT`, `HOST`, `APP_NAME`, `CONFIG_DIR`,
`LOG_LEVEL`, `LOG_JSON`, `ALLOWED_ORIGINS`, `REDIS_HOST/PORT/USER/PASS`,
`OPENSEARCH_URL/USERNAME/PASSWORD`, plus whatever `models.yaml` references (`PROXY_PROVIDER`,
`LITELLM_BASE_URL`, `*_API_KEY`, …). `${VAR}` expands in `base_url`, headers and
`proxy.provider`; `*_env` fields name the variable holding a secret or, for TLS, a file path.

`models.yaml`: `providers[]` (`id, base_url, api_key_env, api: chat_completions|responses,
headers, discover: {endpoint, refresh}, models[], ssl_*`), `memory: {embedding_model,
dimensions}`, `proxy: {provider, overrides}`. Discovery reads `/models` (OpenAI shape, with
OpenRouter/vLLM extras) or `/model/info` (LiteLLM); YAML `models` entries override discovered
ones field by field. A provider whose `base_url` is empty in an environment is skipped.

## 8. Storage

Valkey: `thread:{id}:log` (events, trimmed to 24 h after each finalise), `thread:{id}:seq`,
`thread:{id}:run` (lease), `run:{id}`, `runs:{agentId}`, `runs:done`, `runs:active`,
`session:{threadId}` (+`:events`, `:state`; the adk session, 30-day TTL, rebuilt from
OpenSearch if missing). OpenSearch: `threads`, `messages`, `memories` (kNN, created on boot).

## 9. Conventions

- One wire format, one encoder, one compactor; the only reducer lives in the UI.
- The engine and tools read and write Valkey only; they never import `pkg/db/opensearch`.
- Nothing in a run waits on OpenSearch; persistence happens in the finalizer.
- A run is executed at most once; tools have side effects.
- Named constants for cross-file strings; errors are logged at `warn`/`error`, never swallowed.
- Agents are built per run; there is no global registry of built agents.
- `gofmt`, `go vet` clean before committing.
