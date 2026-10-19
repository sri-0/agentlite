# AGENTS.md — `agentlite`

An LLM agent server in Go. It reverse-proxies the OpenAI-style endpoints to an upstream
gateway, and runs agents built on [adk-go v2](https://pkg.go.dev/google.golang.org/adk/v2)
behind one WebSocket that speaks [AG-UI 1.0](https://docs.ag-ui.com) events. Runs are
resumable, persisted as the same events, and sub-agents are real threads.

This file is the operating manual. How things work is in `docs/` (start at
`docs/README.md`): ARCHITECTURE (components, run lifecycle, the socket, storage),
AGENT-ARCHITECTURE (roster, the assistant, sub-agents, tools, todos), TOOLS (building
tools, connectors, transports, sources, artifacts, tool UI), MODELS (model ids, strict
routing, overrides, model cards, the pass-through), HITL (approvals and questions),
AG-UI-RENDERING (event → UI mapping), MEMORY. A change to a protocol, config
schema, the roster, the run lifecycle, HITL or storage updates the matching doc in the same
commit.

## Quick start

```bash
make up                     # Valkey 9 :6379, OpenSearch :9200, MinIO :9000, Jaeger :16686
make indices                # the OpenSearch indices, from deploy/opensearch
cp .env.example .env.local  # provider keys and base URLs, MODEL_CARDS_DIR
make run                    # :8000
make lint                   # go vet and staticcheck
make dev web                # adk's launcher (web UI; also cli, api) over the same agents
```

With `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318` every run exports a trace to Jaeger
(UI at :16686, service `agentlite`); adk's `telemetry.New` does the export. `make dev` runs
its own worker loops on the same queues, so it must run the same code as the server.

Unit tests (`go test ./...`) cover what breaks quietly: config loading and its checks, the
encoder and coalescing (checked with the ag-ui SDK's own `ValidateSequence`), seeding a
session back from stored events, the model catalog, and the Valkey session store against
adk's own `sessiontestsuite` (skipped without Valkey). End-to-end verification uses a
WebSocket client and `curl`, and those scripts stay out of the repository.

## Layout

```
cmd/server/            the API: bootstrap → handlers → Echo
cmd/dev/               adk's launcher (web UI, cli, api) over the same roster and stores
internal/
  bootstrap/           wires config, logging, tracing, registry, Valkey, OpenSearch, S3,
                       sessions, memory, sources, artifacts, MCP, the runtime and run manager
  server/              Echo app: middleware, routes, error → status mapping
  handler/             REST reads and thread admin; the socket upgrade; handler/wellknown:
                       /.well-known/opencode and the skills discovery index
  ws/                  the agent chat socket: frames, requests, subscriptions
  runs/                the run lifecycle: Manager (start, answer, cancel, follow), worker,
                       finalizer, queues, event log, run records
  agents/              the agent runtime: roster → adk agents, running a segment, connectors,
                       MCP tool hooks, compaction, session seeding, answers → adk responses
  hitl/                interrupt records in Valkey
  stream/              AG-UI conventions, adk → AG-UI encoder, storage coalescing
  tools/               world tools (calculate, webfetch, write_note, search_records); the
                       catalog; tools/builtin: subagent, todowrite, question, emit_artifact,
                       report_progress, write_skill
  skills/              the skills library: records in OpenSearch, files in S3, uploads, adk's
                       skill toolset, /name
  refs/                references in a message (/name; later @agent, #id), by sigil
  sources/             numbers and stores the sources tools return
  artifacts/           artifact records (OpenSearch) and bodies (S3)
  mcp/                 mcp connectors → adk toolsets
  providers/           provider clients, model resolution and discovery, the pass-through
                       proxy, embeddings
pkg/
  toolx/               the tool wrapper every function tool is built with
  citations/           the source contract: Source, Origin, Recorder, [source:N]
  modelcatalog/        the Model type, provider list parsers, model cards, the merge
  transport/           HTTP clients for providers, toolsets and MCP servers; the reverse
                       proxy the pass-through and the MCP proxy share
  opencode/            what opencode reads from /.well-known/opencode
  s3/                  S3-compatible object storage (minio-go)
  db/opensearch/       threads, messages, memories (kNN), sources, artifacts, skills; typed queries
  db/valkey/           Valkey settings and a client per logical database
  memory/              adk memory.Service on OpenSearch
  session/valkey/      adk session.Service on Valkey
  logging/             zerolog + slog bridge
config/                env (go-envconfig, with each pkg's own settings) + models.yaml,
                       connectors.yaml, agents/*.md, all read strictly
config/default/        models.yaml, connectors.yaml, agents/*.md (the roster)
deploy/opensearch/     the index mappings, applied with make indices
docs/                  the in-depth documentation
```

Agents are markdown files in `config/default/agents/`; there is no Go per agent.

## Interfaces

**New thread** `POST /v1/agent/chat {messages, agentId?, model?, connectors?, workspaceId?}` creates a
thread with its first turn and starts the run; `201 {threadId, runId, seq}` once the thread
is stored. It is HTTP so the request can be checked before the thread exists.

**Socket** `GET /v1/agent/chat?v=1` (`426` on another version), one per client, carries
everything on existing threads: `run`, `answer`, `cancel`, `subscribe`, `unsubscribe`
frames in; `ack`, `event` (AG-UI), `idle`, `error` frames out. The first frame about a
thread (normally its subscribe) looks the thread up in OpenSearch; not found, or hidden by
document-level security, is `not_found`. ARCHITECTURE.md has the frames.

**REST** (reads and admin):

| Method | Path | Purpose |
|---|---|---|
| GET | `/health` | `{status, agents}` |
| GET | `/v1/models`, `/v1/agents` | merged model list; pickable agents with their definitions |
| GET | `/v1/connectors`, `/v1/connectors/{id}` | tool groups; one in full, an MCP server's tools listed live |
| GET | `/v1/threads` | root threads, newest first (capped at 200), with `run_status` from each thread's lease |
| GET/PATCH/DELETE | `/v1/threads/{id}` | thread; `PATCH {title?, connectors?, workspace_id?}` (`""` takes it out of its workspace); delete removes its family (`409` while a run is active) |
| GET/POST | `/v1/workspaces` | the workspaces threads are filed under, by name; `POST {name}` |
| PATCH/DELETE | `/v1/workspaces/{id}` | rename (`{name}`); delete, which moves its threads out of it |
| GET | `/v1/threads/{id}/messages?before=&limit=` | `{data: [{id, thread_id, role, seq, created_at, text, events}], cursor}` |
| GET | `/v1/threads/{id}/interrupts` | the paused run's pending interrupts |
| GET | `/v1/threads/{id}/sources`, `/v1/threads/{id}/sources/{n}` | the conversation's sources by number; one source, what `[source:n]` refers to |
| GET | `/v1/threads/{id}/artifacts`, `/v1/threads/{id}/artifacts/{artifactId}/content` | the conversation's artifacts, newest first; one artifact's body |
| GET | `/v1/artifacts?after=&limit=` | artifacts across every conversation, newest first, with thread titles; a full page carries `next`, the cursor for `after` |
| GET/POST | `/v1/skills` | the skills library (needs S3), filtered by `?q=&category=&source=`, with category and source counts; `POST` a multipart form: `file` (a skill's zip or its SKILL.md) and optional `title`, `description`, `category`, `source` (SKILLS.md) |
| GET/PATCH/DELETE | `/v1/skills/{id}` | one skill by its uuid; change its title, description, category or source; remove it and its files |
| GET/PUT | `/v1/skills/{id}/files/{path}` | one file of a skill, sandboxed; replace or add one |
| GET | `/.well-known/agent-skills/index.json`, `/.well-known/agent-skills/{name}/{path}` | the skills discovery index and files `npx skills add <server>` reads (no credentials) |
| GET | `/.well-known/opencode` | what `opencode auth login <server>` configures: models, exposed MCP servers, skills (docs/OPENCODE.md) |
| any | `/mcp/{id}` | an MCP connector with `mcp.expose: true`, proxied for clients such as opencode |
| POST | `/v1/chat/completions`, `/v1/responses`, `/v1/messages`, `/v1/embeddings`, `/v1/decisions`, `/v1/systemone` | pass-through to the provider the body's `model` names, with the model rewritten to the provider's own id; each endpoint takes only models of its types (MODELS.md) |

There is no authentication yet. The `Authorization` header on `POST /v1/agent/chat`
is kept with that run and forwarded to connectors that opt in (TOOLS.md); turns
started or resumed over the socket carry none yet.

## Configuration

Environment (`config/config.go`): `PORT`, `HOST`, `APP_NAME`, `APP_URL` (this
server as clients reach it), `CONFIG_DIR`,
`LOG_LEVEL`, `LOG_JSON`, `ALLOWED_ORIGINS` (CORS), `WS_ALLOWED_ORIGINS` (the socket's browser
origins; `*` allows any), `MAX_BODY_MB`
(the largest request body, default 32), `REDIS_HOST/PORT/USER/PASS`,
`REDIS_DB_RUNS/SESSIONS/HITL` (default 1/2/3), `OPENSEARCH_URL/USERNAME/PASSWORD`,
`S3_ENDPOINT/ACCESS_KEY/SECRET_KEY/BUCKET/REGION/USE_SSL` (required: artifact bodies and
skill files), `TRANSPORT_SENSITIVE_HEADERS` (a caller's headers a forward drops),
`TRANSPORT_RESPONSE_HEADER_TIMEOUT` (default 5m),
`MODEL_CARDS_DIR` (a checkout of the model cards), `OTEL_EXPORTER_OTLP_ENDPOINT`,
plus what the YAML references (
`LITELLM_BASE_URL`, `*_API_KEY`, `OPEN_SLIDE_MCP_URL`, …). `${VAR}` expands in `base_url`,
headers and MCP endpoints.

- `models.yaml`: `providers[]` (`id, base_url, api_key_env, api: chat_completions|responses,
  headers, discover, models[], tls`), `default_model`, `compaction`, `memory`. A model id is
  `<provider>/<model>` and routes only to that provider (MODELS.md); models are declared by
  the provider's own id and override, field by field, discovery and the model cards, with
  an optional per-model `base_url` and `api`. A provider with an empty `base_url` is skipped.
- `connectors.yaml`: `user_agent` and `connectors[]` (`id, name, description, icon, type:
  tools | mcp, tools[], mcp: {endpoint | command, tools, require_confirmation, thread_args,
  ui, artifacts, expose}, http: {headers, user_agent, timeout, forward, tls}`). A tool belongs to
  one connector at most. The development MCP server is open-slide-mcp
  (`~/code/open-slide-mcp`, `make run`, `:8090/mcp`).
- `agents/*.md`: frontmatter `id, name, description, kind, mode, tools, subagents,
  max_output_tokens` and the instruction (AGENT-ARCHITECTURE.md).

## Conventions

- A new thread is a POST; everything after it between a client and its agents goes over
  the socket; the rest of REST is for reads and admin.
- One wire format (AG-UI), one encoder, one coalescer; the only reducer lives in the UI.
- `runs` decides when and where work happens; `agents` decides what the model does. `runs`
  defines the interface it needs (`runs.Runtime`) and `agents` implements it.
- The worker and finalizer talk to each other and to `runs.Manager` only through Valkey, so
  they can move to separate processes.
- A run is executed at most once; tools have side effects.
- Every function tool is built with `toolx.New`; a test fails if anything else imports adk's
  `functiontool`. A tool that returns citable items records each with `ctx.AddSource` and
  shows the model only the number.
- Every outbound HTTP client is built with `pkg/transport`, one per provider, connector and
  MCP server.
- Agents never name a model; the request's model flows to every step and sub-agent.
- Errors are logged at `warn`/`error`, never swallowed; sentinel errors map to statuses and
  socket codes in one place each (`server.errorHandler`, `ws.code`).
- Measure before tuning: Jaeger shows where a slow turn goes.
- `gofmt`, `go vet` clean before committing; docs updated in the same commit.
