# AGENTS.md — `agentlite`

An LLM agent server in Go. It reverse-proxies the OpenAI-style endpoints to an upstream
gateway, and runs agents built on [adk-go v2](https://pkg.go.dev/google.golang.org/adk/v2)
behind one WebSocket that speaks [AG-UI 1.0](https://docs.ag-ui.com) events. Runs are
resumable, persisted as the same events, and sub-agents are real threads.

This file is the operating manual. How things work is in `docs/` (start at
`docs/README.md`): ARCHITECTURE (components, run lifecycle, the socket, storage),
AGENT-ARCHITECTURE (roster, the assistant, sub-agents, tools, todos), TOOLS (building
tools, transports, sources, artefacts, tool UI), HITL (approvals and questions),
AG-UI-RENDERING (event → UI mapping), MEMORY. A change to a protocol, config
schema, the roster, the run lifecycle, HITL or storage updates the matching doc in the same
commit.

## Quick start

```bash
make up                     # Valkey :6379, OpenSearch :9200, MinIO :9000, Jaeger :16686
cp .env.example .env.local  # PROXY_PROVIDER, provider keys and base URLs
make run                    # :8000
make vet
make dev web                # adk's launcher (web UI; also cli, api) over the same agents
```

With `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318` every run exports a trace to Jaeger
(UI at :16686, service `agentlite`); adk's `telemetry.New` does the export. `make dev` runs
its own worker loops on the same queues, so it must run the same code as the server.

Unit tests cover the reusable packages (`go test ./...`); end-to-end verification uses a
WebSocket client and `curl`, and those scripts stay out of the repository.

## Layout

```
cmd/server/            the API: bootstrap → handlers → Echo
cmd/dev/               adk's launcher (web UI, cli, api) over the same roster and stores
internal/
  bootstrap/           wires config, logging, tracing, registry, Valkey, OpenSearch, S3,
                       sessions, memory, sources, artefacts, MCP, the runtime and run manager
  config/              env (go-envconfig) + models.yaml, mcp.yaml, connectors.yaml, agents/*.md
  server/              Echo app: middleware, routes, error → status mapping
  handler/             REST reads and thread admin; the socket upgrade
  ws/                  the agent chat socket: frames, requests, subscriptions
  runs/                the run lifecycle: Manager (start, answer, cancel, follow), worker,
                       finalizer, queues, event log, run records
  agents/              the agent runtime: roster → adk agents, running a segment, connectors,
                       MCP tool hooks, compaction, session seeding, answers → adk responses
  hitl/                interrupt records in Valkey
  stream/              AG-UI conventions, adk → AG-UI encoder, storage coalescing
  tools/               world tools (calculate, webfetch, write_note, search_records); the
                       catalog; tools/builtin: subagent, todowrite, question, emit_artifact,
                       report_progress
  sources/             numbers and stores the sources tools return
  artifacts/           artefact records (OpenSearch) and bodies (S3)
  mcp/                 mcp.yaml → adk toolsets
  proxy/               provider clients, model discovery, reverse proxies, embeddings
pkg/
  toolx/               the tool wrapper every function tool is built with
  transport/           HTTP clients for providers, toolsets and MCP servers
  s3/                  S3-compatible object storage (minio-go)
  db/opensearch/       threads, messages, memories (kNN), sources, artefacts
  db/valkey/           client per logical database
  memory/              adk memory.Service on OpenSearch
  session/valkey/      adk session.Service on Valkey
  logging/             zerolog + slog bridge
config/default/        models.yaml, mcp.yaml, connectors.yaml, agents/*.md (the roster)
docs/                  the in-depth documentation
```

Agents are markdown files in `config/default/agents/`; there is no Go per agent.

## Interfaces

**New thread** `POST /v1/agent/chat {messages, agentId?, model?, connectors?}` creates a
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
| GET/PATCH/DELETE | `/v1/threads/{id}` | thread; `PATCH {title?, connectors?}`; delete removes its family (`409` while a run is active) |
| GET | `/v1/threads/{id}/messages?before=&limit=` | `{data: [{id, thread_id, role, seq, created_at, text, events}], cursor}` |
| GET | `/v1/threads/{id}/interrupts` | the paused run's pending interrupts |
| GET | `/v1/threads/{id}/sources`, `/v1/threads/{id}/sources/{n}` | the conversation's sources by number; one source, what `[source:n]` refers to |
| GET | `/v1/threads/{id}/artifacts`, `/v1/threads/{id}/artifacts/{artifactId}/content` | the conversation's artefacts, newest first; one artefact's body |
| POST | `/v1/chat/completions`, `/v1/responses`, `/v1/messages`, `/v1/embeddings` | reverse proxies to the configured provider, bodies untouched |

There is no authentication or identity yet.

## Configuration

Environment (`internal/config/config.go`): `PORT`, `HOST`, `APP_NAME`, `CONFIG_DIR`,
`LOG_LEVEL`, `LOG_JSON`, `ALLOWED_ORIGINS`, `REDIS_HOST/PORT/USER/PASS`,
`REDIS_DB_RUNS/SESSIONS/HITL` (default 1/2/3), `OPENSEARCH_URL/USERNAME/PASSWORD`,
`S3_ENDPOINT/ACCESS_KEY/SECRET_KEY/BUCKET/REGION/USE_SSL` (no endpoint keeps artefact bodies
in OpenSearch), `OTEL_EXPORTER_OTLP_ENDPOINT`, plus what the YAML references (`PROXY_PROVIDER`,
`LITELLM_BASE_URL`, `*_API_KEY`, `OPEN_SLIDE_MCP_URL`, …). `${VAR}` expands in `base_url`,
headers, MCP endpoints and `proxy.provider`.

- `models.yaml`: `providers[]` (`id, base_url, api_key_env, api: chat_completions|responses,
  headers, discover, models[], ssl_*`), `default_model`, `compaction`, `memory`, `proxy`.
  Discovered models merge under YAML entries; the first provider in file order that serves
  an id wins; a provider with an empty `base_url` is skipped.
- `mcp.yaml`: `servers: {name: {endpoint | command, headers, timeout, thread_headers, tools,
  require_confirmation, thread_args, ui, artifacts}}` (TOOLS.md).
  The development server is open-slide-mcp (`~/code/open-slide-mcp`, `make run`, `:8090/mcp`).
- `connectors.yaml`: `connectors[]` (`id, name, description, icon, tools[] | mcp, http`);
  `http: {headers, timeout, thread_headers}` configures the connector's own HTTP client.
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
