# AGENTS.md — `agentlite`

An LLM agent server in Go. It reverse-proxies the OpenAI-style endpoints to an upstream
gateway, and runs agents built on [adk-go v2](https://pkg.go.dev/google.golang.org/adk/v2)
behind one WebSocket that speaks [AG-UI 1.0](https://docs.ag-ui.com) events. Runs are
resumable, persisted as the same events, and sub-agents are real threads.

This file is the operating manual. How things work is in `docs/` (start at
`docs/README.md`): ARCHITECTURE (components, run lifecycle, the socket, storage),
AGENT-ARCHITECTURE (roster, the assistant, sub-agents, tools, todos), HITL (approvals and
questions), AG-UI-RENDERING (event → UI mapping), MEMORY. A change to a protocol, config
schema, the roster, the run lifecycle, HITL or storage updates the matching doc in the same
commit.

## Quick start

```bash
make up                     # Valkey :6379, OpenSearch :9200, Jaeger :16686
cp .env.example .env.local  # PROXY_PROVIDER, provider keys and base URLs
make run                    # :8000
make vet
make dev web                # adk's launcher (web UI; also cli, api) over the same agents
```

With `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318` every run exports a trace to Jaeger
(UI at :16686, service `agentlite`); adk's `telemetry.New` does the export. `make dev` runs
its own worker loops on the same queues, so it must run the same code as the server.

There are no tests yet; development verification uses a WebSocket client and `curl`, and
those scripts stay out of the repository.

## Layout

```
cmd/server/            the API: bootstrap → handlers → Echo
cmd/dev/               adk's launcher (web UI, cli, api) over the same roster and stores
internal/
  bootstrap/           wires config, logging, tracing, registry, Valkey, OpenSearch, sessions,
                       memory, MCP, the agent runtime and the run manager
  config/              env (go-envconfig) + models.yaml, mcp.yaml, connectors.yaml, agents/*.md
  server/              Echo app: middleware, routes, error → status mapping
  handler/             REST reads and thread admin; the socket upgrade
  ws/                  the agent chat socket: frames, requests, subscriptions
  runs/                the run lifecycle: Manager (start, answer, cancel, follow), worker,
                       finalizer, queues, event log, run records
  agents/              the agent runtime: roster → adk agents, running a segment, connectors,
                       compaction, session seeding, answers → adk responses
  hitl/                interrupt records in Valkey
  stream/              AG-UI conventions, adk → AG-UI encoder, storage coalescing
  tools/               world tools (calculate, webfetch, write_note); tools/builtin: subagent,
                       todowrite, question, emit_artifact, report_progress
  mcp/                 mcp.yaml → adk toolsets
  proxy/               provider clients (mTLS, headers), model discovery, reverse proxies, embeddings
pkg/
  db/opensearch/       threads, messages, memories (kNN)
  db/valkey/           client per logical database
  memory/              adk memory.Service on OpenSearch
  session/valkey/      adk session.Service on Valkey
  logging/             zerolog + slog bridge
config/default/        models.yaml, mcp.yaml, connectors.yaml, agents/*.md (the roster)
docs/                  the in-depth documentation
```

Agents are markdown files in `config/default/agents/`; there is no Go per agent.

## Interfaces

**Socket** `GET /v1/agent/chat?v=1` (`426` on another version) carries everything between a
client and its agents: `run`, `answer`, `cancel`, `subscribe`, `unsubscribe` frames in;
`ack`, `event` (AG-UI), `idle`, `error` frames out. ARCHITECTURE.md has the frames.

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
| POST | `/v1/chat/completions`, `/v1/responses`, `/v1/messages`, `/v1/embeddings` | reverse proxies to the configured provider, bodies untouched |

There is no authentication or identity yet.

## Configuration

Environment (`internal/config/config.go`): `PORT`, `HOST`, `APP_NAME`, `CONFIG_DIR`,
`LOG_LEVEL`, `LOG_JSON`, `ALLOWED_ORIGINS`, `REDIS_HOST/PORT/USER/PASS`,
`REDIS_DB_RUNS/SESSIONS/HITL` (default 1/2/3), `OPENSEARCH_URL/USERNAME/PASSWORD`,
`OTEL_EXPORTER_OTLP_ENDPOINT`, plus what the YAML references (`PROXY_PROVIDER`,
`LITELLM_BASE_URL`, `*_API_KEY`, `OPEN_SLIDE_MCP_URL`, …). `${VAR}` expands in `base_url`,
headers, MCP endpoints and `proxy.provider`.

- `models.yaml`: `providers[]` (`id, base_url, api_key_env, api: chat_completions|responses,
  headers, discover, models[], ssl_*`), `default_model`, `compaction`, `memory`, `proxy`.
  Discovered models merge under YAML entries; the first provider in file order that serves
  an id wins; a provider with an empty `base_url` is skipped.
- `mcp.yaml`: `servers: {name: {endpoint | command, headers, tools, require_confirmation}}`.
  The development server is open-slide-mcp (`~/code/open-slide-mcp`, `make run`, `:8090/mcp`).
- `connectors.yaml`: `connectors[]` (`id, name, description, icon, tools[] | mcp`).
- `agents/*.md`: frontmatter `id, name, description, kind, mode, tools, subagents,
  max_output_tokens` and the instruction (AGENT-ARCHITECTURE.md).

## Conventions

- Writes between a client and its agents go over the socket; REST is for reads and admin.
- One wire format (AG-UI), one encoder, one coalescer; the only reducer lives in the UI.
- `runs` decides when and where work happens; `agents` decides what the model does. `runs`
  defines the interface it needs (`runs.Runtime`) and `agents` implements it.
- The worker and finalizer talk to each other and to `runs.Manager` only through Valkey, so
  they can move to separate processes.
- A run is executed at most once; tools have side effects.
- Agents never name a model; the request's model flows to every step and sub-agent.
- Errors are logged at `warn`/`error`, never swallowed; sentinel errors map to statuses and
  socket codes in one place each (`server.errorHandler`, `ws.code`).
- Measure before tuning: Jaeger shows where a slow turn goes.
- `gofmt`, `go vet` clean before committing; docs updated in the same commit.
