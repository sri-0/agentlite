# Tools, transports, sources and artefacts

How agentlite builds tools, how it reaches other systems over HTTP, how a tool's
results become citable sources, how artefacts are stored, and how a tool asks
the UI for its own rendering.

## Plan (2026-10-13)

The decisions below were made before building; the sections after them
describe the result.

1. **One transport package for every outbound HTTP client** (`pkg/transport`).
   Today each provider's client is built inside `internal/proxy`. It moves to
   `pkg/transport`, which builds an `*http.Client` from a small config: static
   headers, headers taken from the request's context, TLS files, a timeout.
   Every LLM provider gets its own instance, used by agent model calls, the
   pass-through proxies and embeddings alike. Every connector (toolset) and
   every MCP server gets its own instance too.
2. **Every function tool is built with `pkg/toolx`.** `toolx.New(spec, fn)`
   wraps adk's `functiontool`. The tool body receives a `toolx.Context`: adk's
   tool context plus the thread the call belongs to and `AddSource`. Registries
   and toolsets only accept `toolx.Tool`, and a test fails if any other package
   imports adk's `functiontool`, so no tool can skip the wrapper.
3. **Toolsets come from configuration; tools stay independent.** A connector in
   `connectors.yaml` is one system: its tools and an optional `http:` block.
   The runtime builds one HTTP client per connector and passes it to each tool's
   constructor. Tools take the client and nothing else. The alternative,
   toolsets declared by the tools themselves, was rejected: connectors already
   carry the user-facing name, icon and description, and a second place to
   group tools would drift.
4. **Thread information travels in HTTP headers.** The runtime puts the run's
   thread into the context. Toolset and MCP clients that opt in
   (`thread_headers`) add `X-Thread-ID`, `X-Root-Thread-ID` and `X-Run-ID` to
   every request made with that context, so a downstream system can scope,
   audit or authorise by thread.
5. **Sources are recorded by the harness, not written by the model.** A tool
   calls `ctx.AddSource(...)` for each item it returns, with the item's system,
   key, URL, title and metadata (row UUIDs, markings, control sets). The
   harness gives each source a number, unique within the conversation, and
   stores the full record in the OpenSearch `sources` index. The model only
   sees the number on the item and cites it as `[source:N]`. When a turn or an
   artefact is saved, the numbers it cites are recorded against the source, so
   every citation traces back to the exact record a tool returned.
6. **Artefacts are stored.** `emit_artifact` writes the metadata to the
   OpenSearch `artifacts` index and the body to S3 (MinIO locally, through the
   `minio-go` client). Without S3 configured the body stays in the OpenSearch
   document. A `url` artefact has no body: it points at a page to embed.
7. **A tool can ask for its own UI.** `toolx.Spec.UI` (and `ui:` per MCP tool in
   `mcp.yaml`) names a renderer. The encoder puts it in the `TOOL_CALL_START`
   metadata, so stored history renders the same way. The UI keeps a registry of
   renderers by name and falls back to the plain tool line.
8. **Open-slide is configuration, not code.** `mcp.yaml` says which argument
   carries the thread id (the harness fills it), which tools render as
   `slides`, and that `deck_create` records a `url` artefact from its result.
   The `slides` renderer opens the artefact panel on the deck's preview, and a
   later `deck_write` refreshes it.

## Transports (`pkg/transport`)

`transport.New(Config)` returns an `*http.Client` with its own connection pool.

| Field | Meaning |
|---|---|
| `Headers` | set on every request, replacing the caller's |
| `ContextHeaders` | a function from the request's context to more headers |
| `TLS` | client certificate, key and trusted CAs (PEM files), or skip verification |
| `Timeout` | per request, body included; zero for streamed model responses |
| `DisableCompression` | do not ask for gzip; providers set it so streams are not held back |

Who builds one:

- **Each LLM provider** (`internal/proxy`, from `models.yaml`: `headers`, `ssl_*`). The
  same client carries agent model calls (adk's OpenAI model), the pass-through proxies and
  embeddings.
- **Each connector of function tools** (`internal/agents/connectors.go`, from the
  connector's `http:` block). Every tool of the connector gets it.
- **Each MCP server reached over HTTP** (`internal/mcp`, from the server's `headers` and
  `thread_headers`). It has no timeout, which would also cut the session's long-lived event
  stream. A session is opened outside any run, so only each tool call's own request
  carries thread headers.

`config.HTTP` is the shared shape of those blocks:

```yaml
http:
  headers: {Authorization: "Bearer ${RECORDS_TOKEN}"}
  timeout: 30s
  thread_headers: true   # X-Thread-ID, X-Root-Thread-ID, X-Run-ID on every request
```

Thread headers are off by default: thread ids are internal, and a tool such as `webfetch`
talks to the public web.

## Building a tool (`pkg/toolx`)

```go
func searchRecords(client *http.Client) (toolx.Tool, error) {
	return toolx.New(toolx.Spec{
		Name:        "search_records",
		Description: "Search the organisation's records. Each hit has a source number to cite.",
		// RequireConfirmation: true pauses for the user's approval (HITL.md)
		// UI: "records" names a UI renderer for the calls
	}, func(tc toolx.Context, a searchRecordsArgs) (map[string]any, error) {
		// tc is adk's tool context plus tc.Thread {ID, RootID, RunID}.
		// Requests made with tc carry the thread headers when the connector sends them:
		//   http.NewRequestWithContext(tc, ...); client.Do(req)
		n, err := tc.AddSource(toolx.Source{System: "records", Key: row.ID, URL: row.URL, Title: row.Title,
			Metadata: map[string]any{"marking": row.Marking}})
		...
	})
}
```

- The input and output types give the schemas, from `json` and `jsonschema` tags, as with
  adk's `functiontool`.
- A world tool is a `tools.Builder`, `func(*http.Client) (toolx.Tool, error)`, registered
  by name in `tools.Catalog`. A connector lists tool names; the runtime builds each tool
  with the connector's client. A tool belongs to one connector at most (`connectors.yaml`
  is rejected otherwise: a run gets every connector by default, and adk refuses two tools
  of one name); one in no connector is built on a plain client for agents that list it.
- A harness tool (`internal/tools/builtin`) is built the same way from `builtin.Deps`.
- Registries and `toolx.Toolset` accept only `toolx.Tool`, and
  `TestOnlyToolxBuildsFunctionTools` fails if any package but `toolx` imports
  `functiontool`.

## Sources

A source is an item a tool returned that an answer may cite.

```
tool ──tc.AddSource(Source{System, Key, URL, Title, Metadata})──► sources.Recorder
        │                                                          ├─ Valkey: the number
        │                                                          └─ OpenSearch: the record
        └─ returns the item to the model with "source": N
model ──"… the cables corroded [source:2]."──► encoder ──► runtime: Cite(message:<turn>)
UI    ──[source:2] chip ──► GET /v1/threads/{id}/sources/2
```

- **Numbers belong to the conversation** (the root thread), so a sub-agent's citations
  stay valid in its parent's answer. `numberScript` (`internal/sources`) gives an item the
  number its key already has, or the next one: the same row returned twice keeps its
  number. The key is `Key`, or the URL when there is none; an item with neither always
  gets a new number.
- **A number is never reused.** A record is created, never overwritten. If Valkey ever
  loses the count, the next record collides with a stored number; the recorder then
  resumes the count after the highest stored number and numbers the source again.
- **The record is the tool's, not the model's.** The model sees only the number on the
  item; the system, key, URL, title and metadata (row ids, data markings, control sets)
  come from the tool's own data, stored as given.
- **Citations are recorded.** Before a segment's closing event, the runtime records every
  `[source:N]` its text showed against the turn (`cited_in: ["message:<turn id>"]`), and an
  artefact's body records its own (`artifact:<id>`). A client that reads the sources when
  the run ends sees them. Each source is updated on its own and retried when two
  citations land at once; a number no source has is skipped. Recording is best effort: a
  failure is logged, never turned into a failed turn or save.
- **The model is told how to cite.** Every agent with tools gets a `<citations>` block in
  its instruction (`internal/agents/build.go`).

Storage:

| Where | What |
|---|---|
| Valkey `REDIS_DB_RUNS`: `thread:{root}:sources` | hash, item key → number |
| Valkey `REDIS_DB_RUNS`: `thread:{root}:sources:seq` | the last number given |
| OpenSearch `sources`, id `{root}:{n}` | `root_id, number, thread_id, run_id, tool, tool_call_id, system, key, url, title, metadata (flat_object), retrieved_at, cited_in` |

`GET /v1/threads/{id}/sources` lists them by number; `GET /v1/threads/{id}/sources/{n}`
returns one. Deleting the root thread deletes the records and the numbering.

`search_records` (connector `records`) is the demonstration: it searches a few sample
records and records every hit with the row's id, marking and control set as metadata.
`webfetch` records each page it reads (system `web`, the final URL, the page title).

## Artefacts

`emit_artifact` publishes a document; `internal/artifacts` stores it:

- the record in OpenSearch `artifacts` (id `{root}:{artifact id}`): `id, root_id,
  thread_id, run_id, title, kind, language, url, size, stored, version, created_at,
  updated_at`;
- the body in S3 at `artifacts/{root}/{artifact id}/{version}` (`stored: s3`), or in the
  record's `content` when `S3_ENDPOINT` is not set (`stored: inline`).

An artefact id is letters, digits, `.`, `_` and `-` (it is part of a document id, an S3
key and a URL); the model may name one, and gets a UUID otherwise.

Kinds with a body: `markdown`, `code`, `html`, `json`, `csv`. Kind `url` has no body: it
points at a page the UI embeds. Publishing the same id again stores a new version under
its own key. Versions are numbered atomically in Valkey (`thread:{root}:artifacts`, a hash
of artefact id → last version), and the record is written with OpenSearch external
versioning, so it only moves forward: two saves of one artefact at once both keep their
bodies, and the higher version is served. The tool's result, and the `artifact` activity the encoder makes of it,
is a summary without the body (`artifacts.Summary`): `{id, title, kind, version,
language?, url?}`. The UI fetches the body from
`GET /v1/threads/{id}/artifacts/{artifactId}/content`, served with the kind's content
type, with `Content-Security-Policy: sandbox` so a body opened directly never runs as the
API's origin; `GET /v1/threads/{id}/artifacts` lists the conversation's artefacts.

S3 is any S3-compatible store through `minio-go` (`pkg/s3`); `docker-compose.yaml` runs
MinIO on `:9000` (console `:9001`, `minioadmin`/`minioadmin`). The bucket is created on
start. Deleting the root thread deletes its bodies, then its records.

## Tool UI renderers

A tool can name a renderer: `toolx.Spec.UI` for a function tool, `ui: {tool: name}` in
`mcp.yaml` for an MCP tool. The encoder puts it in `TOOL_CALL_START` as
`metadata: {ui: "<name>"}`, so stored history renders the same way. The UI keeps a
registry of renderers by name (`agentui/components/chat/messages/tools/registry.ts`); a
call with no renderer, or one waiting for approval, renders as the plain tool line. The
renderer receives the tool part: name, input, output, state.

## MCP tool hooks

MCP tools are not built with `toolx`, so `mcp.yaml` says per tool what the harness does,
through adk's before- and after-tool callbacks (`internal/agents/mcptools.go`):

| Key | What the harness does |
|---|---|
| `thread_args: {tool: arg}` | writes the conversation's root thread id into `arg` before the call, so the model never supplies it |
| `ui: {tool: renderer}` | names the tool's UI renderer |
| `artifacts: {tool: {id, title, url}}` | after a successful call, reads those fields of the structured result and records a `url` artefact, shown in the thread at once |

Open-slide uses all three:

```yaml
open-slide:
  endpoint: ${OPEN_SLIDE_MCP_URL}
  thread_headers: true
  thread_args: {deck_create: thread_id, deck_list: thread_id, thread_cleanup: thread_id}
  ui: {deck_create: slides, deck_write: slides, deck_preview_url: slides}
  artifacts:
    deck_create: {id: deck_id, title: title, url: preview_url}
```

In the UI the `slides` renderer shows each deck call as one line. When the run creates a
deck, the artefact panel opens on the deck's live preview (the open-slide editor in an
iframe); when it writes slides, the preview reloads. The panel's refresh button reloads it
by hand. The deck stays an artefact of the conversation, listed in the artefact panel when
the thread is opened again; its files stay on the open-slide server.

## Code map

| File | Role |
|---|---|
| `pkg/transport/transport.go` | the HTTP client builder |
| `pkg/toolx/toolx.go`, `context.go`, `source.go` | `New`, `Spec`, `Tool`, `Toolset`; `Context`, `Thread`, `WithRun`, `Headers`; `Source`, `Citations` |
| `internal/tools/tools.go` | the catalog of world tools |
| `internal/agents/connectors.go` | one client and toolset per connector |
| `internal/agents/mcptools.go` | the MCP tool hooks |
| `internal/sources/sources.go` | numbering, records, citations |
| `internal/artifacts/artifacts.go` | records and bodies |
| `pkg/s3/s3.go` | S3 through minio-go |
| `pkg/db/opensearch/sources.go`, `artifacts.go` | the `sources` and `artifacts` indices |
| `internal/handler/sources.go`, `artifacts.go` | the read API |
| `internal/stream/encode.go` | `ToolUI` → `TOOL_CALL_START` metadata |
