# Tools, transports, sources and artifacts

How agentlite builds tools, how it reaches other systems over HTTP, how a tool's
results become citable sources, how artifacts are stored, and how a tool asks
the UI for its own rendering.

## Plan (2026-10-13)

The decisions below were made before building; the sections after them
describe the result.

1. **One transport package for every outbound HTTP client** (`pkg/transport`).
   Each provider's client was built inside `internal/proxy`. It moved to
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
   constructor as a `toolx.Connector` (its id and client); tools take nothing
   else. The alternative,
   toolsets declared by the tools themselves, was rejected: connectors already
   carry the user-facing name, icon and description, and a second place to
   group tools would drift.
4. **Thread information travels in HTTP headers.** The runtime puts the run's
   thread into the context. Toolset and MCP clients that opt in
   (`http.forward`) add `X-Thread-ID`, `X-Root-Thread-ID` and `X-Run-ID` to
   every request made with that context, so a downstream system can scope,
   audit or authorise by thread.
5. **Sources are recorded by the harness, not written by the model.** A tool
   calls `ctx.AddSource(...)` for each item it returns, with the item's system,
   key, URL, title and metadata (row UUIDs, markings, control sets). The
   harness gives each source a number, unique within the conversation, and
   stores the full record in the OpenSearch `sources` index. The model only
   sees the number on the item and cites it as `[source:N]`. When a turn or an
   artifact is saved, the numbers it cites are recorded against the source, so
   every citation traces back to the exact record a tool returned.
6. **Artifacts are stored.** `emit_artifact` writes the metadata to the
   OpenSearch `artifacts` index and the body to S3 (MinIO locally, through the
   `minio-go` client). A `url` artifact has no body: it points at a page to
   embed.
7. **A tool can ask for its own UI.** `toolx.Spec.UI` (and `ui:` per MCP tool in
   an MCP connector) names a renderer. The encoder puts it in the `TOOL_CALL_START`
   metadata, so stored history renders the same way. The UI keeps a registry of
   renderers by name and falls back to the plain tool line.
8. **Open-slide is configuration, not code.** Its connector says which argument
   carries the thread id (the harness fills it), which tools render as
   `slides`, and that `deck_create` records a `url` artifact from its result.
   The `slides` renderer opens the artifact panel on the deck's preview, and a
   later `deck_write` refreshes it.

## Transports (`pkg/transport`)

`transport.New(Config)` returns an `*http.Client` with its own connection pool.

| Field | Meaning |
|---|---|
| `Headers` | set on every request, replacing the caller's (the user agent is one) |
| `ContextHeaders` | a function from the request's context to more headers (`toolx.Headers`) |
| `TLS` | a client certificate for mutual TLS, its key (encrypted or not) and the key's password, trusted CAs, or skip verification |
| `Timeout` | per request, body included; zero for streamed model responses |
| `DisableCompression` | do not ask for gzip; providers set it so streams are not held back |

An encrypted key is read with `github.com/youmark/pkcs8` (PKCS#8, "ENCRYPTED PRIVATE KEY",
what OpenSSL 3 writes) or the standard library (legacy `Proc-Type: 4,ENCRYPTED` PEM, what
OpenSSL 1.x and LibreSSL write), then handed to `tls.X509KeyPair`, which checks the key
belongs to the certificate. An encrypted key without a password, a password for a plain
key, or a key of another certificate fails at start.

Who builds one:

- **Each LLM provider** (`internal/providers`, from its `headers` and `tls` in `models.yaml`).
  The same client carries agent model calls (adk's OpenAI model), the pass-through
  proxies and embeddings.
- **Each connector** (`internal/agents/connectors.go` for toolx tools, `internal/mcp` for
  an MCP server), from the connector's `http:` block. An MCP server's client has no
  timeout, which would cut the session's long-lived event stream; its sessions are
  opened outside any run, so the standalone event stream carries no identity, while
  each request (initialize, tools/list, tools/call) carries that of the run that sent it.

The TLS block is the same for providers and connectors; each `*_env` names a variable,
holding a file path except `key_password_env`, which holds the password itself. A
variable named but not set fails at start, never falls back to plain TLS.

```yaml
tls:
  cert_env: LITELLM_TLS_CERT
  key_env: LITELLM_TLS_KEY
  key_password_env: LITELLM_TLS_KEY_PASSWORD
  ca_env: LITELLM_TLS_CA
```

A connector's `http:` block (`config.HTTP`):

```yaml
http:
  headers: {X-Api-Key: "${RECORDS_API_KEY}"}
  user_agent: records-client/2     # default: connectors.yaml's top-level user_agent
  timeout: 30s                     # tools only
  forward: true                    # send who each call is for (below)
  tls: {...}                       # as above, plus insecure_skip_verify: true (development only)
```

Headers (static and forwarded) are never sent on a redirect that has been on another host
at any hop: they carry credentials, which net/http strips from such a redirect itself.

`forward` sends, on every request made for a tool call: the user's `Authorization`
header, `X-Thread-ID`, `X-Root-Thread-ID`, `X-Run-ID`, and for toolx tools `X-Agent-ID`
and `X-Tool-Name`. It is off by default: a tool such as `webfetch` talks to the public
web. The user's `Authorization` is taken from the request that created the thread
(`POST /v1/agent/chat`) and travels with that run's queue entry to the worker (entries
are deleted once handled); a sub-agent's run inherits it. Runs started or resumed over
the socket carry none yet, so the calls of a resumed run (an approved write, say) go
without the user's identity until the socket carries it. A connector with `forward`
therefore may not also set a static `Authorization` header (refused at start): calls
without a user would silently go out as the service's identity.

An MCP server's session is shared by every run (adk keeps one per toolset), so it was
opened under whichever user's run connected first. A forwarding MCP server must
authorise each request by its own `Authorization`, not the session.

## Connectors and tools (`pkg/toolx`)

A connector (`connectors.yaml`) is one system and one adk toolset, of one of two types:

- `type: tools`: toolx tools from the catalog, by name, each built with the connector;
- `type: mcp`: one MCP server, configured inline (endpoint or command, a tool filter,
  approvals, and the hooks below); adk's `mcptoolset`, named after the connector.

Both reach the agent through `llmagent.Config.Toolsets`, which is how adk-go hands an
agent a group of tools: its docs pass an MCP server's `mcptoolset` there, and adk has no
ready-made toolset for function tools, so `toolx.Toolset` is the small one. A tool name
belongs to one connector at most: a run gets every connector by default, and adk
refuses two tools of one name.

```go
func searchRecords(conn toolx.Connector) (toolx.Tool, error) {
	return toolx.New(toolx.Spec{
		Name:        "search_records",
		Description: "Search the organisation's records. Each hit has a source number to cite.",
		// RequireConfirmation: true pauses for the user's approval (HITL.md)
		// UI: "records" names a UI renderer for the calls
	}, func(tc toolx.Context, a searchRecordsArgs) (map[string]any, error) {
		// conn: the connector's id (conn.ID) and HTTP client (conn.HTTP).
		// tc: adk's tool context plus tc.Thread {ID, RootID, RunID}. Requests made with
		// tc carry the caller's identity when the connector forwards it:
		//   req, _ := http.NewRequestWithContext(tc, ...); conn.HTTP.Do(req)
		n, err := tc.AddSource(citations.Source{System: conn.ID, Key: row.ID, URL: row.URL, Title: row.Title,
			Metadata: map[string]any{"marking": row.Marking}})
		...
	})
}
```

- The input and output types give the schemas, from `json` and `jsonschema` tags, as with
  adk's `functiontool`.
- A world tool is a `tools.Builder`, `func(toolx.Connector) (toolx.Tool, error)`,
  registered by name in `tools.Catalog`; the runtime builds each one with its connector,
  on that connector's one HTTP client. A tool is only offered through a connector.
- `search_userfiles` is a world tool built on the files store
  (`tools.SearchFiles(*userfiles.Store)`, `internal/tools/search_userfiles.go`), with its
  work in `internal/rag/userfiles`; the runtime offers it with the harness tools.
- A harness tool (`internal/tools/builtin`, one file per tool) is built the same way from
  `builtin.Deps`; its work lives in its own package (`emit_artifact` → `internal/artifacts`,
  `write_skill` → `internal/skills`, `generate_image` → `internal/images`).
- Registries and `toolx.Toolset` accept only `toolx.Tool`, and
  `TestOnlyToolxBuildsFunctionTools` fails if any package but `toolx` imports
  `functiontool`.

## Sources

A source is an item a tool returned that an answer may cite. The contract
(`citations.Source`, `citations.Origin`, `citations.Recorder`, and the
`[source:N]` format) lives in `pkg/citations`; a tool records sources through
`tc.AddSource`, because a source must be recorded where the tool reads the data.

```
tool ──tc.AddSource(Source{System, Key, URL, Title, Metadata})──► sources.Recorder
        │                                                          └─ OpenSearch: number + record
        └─ returns the item to the model with "source": N
model ──"… the cables corroded [source:2]."──► encoder ──► runtime: Cite(message:<turn>)
UI    ──[source:2] citation ──► GET /v1/threads/{id}/sources/2
```

- **Numbers belong to the conversation** (the root thread), so a sub-agent's citations
  stay valid in its parent's answer. They come from the conversation's counters
  document (OpenSearch `counters`, incremented by a scripted update, atomic per
  document).
- **The same item keeps its number.** An item's identity is its system plus its `Key`,
  or its URL when there is none; its record's id is derived from that identity, so a
  second add finds it, and two adds of one new item at the same moment collide on the
  id: the second takes the first's number and its own is left unused. An item with
  neither key nor URL always gets a new number.
- **A record is never replaced** (created with `op_type=create`).
- **The record is the tool's, not the model's.** The model sees only the number on the
  item; the system (the tool's connector), key, URL, title and metadata (row ids, data
  markings, control sets) come from the tool's own data, stored as given.
- **Citations are recorded.** Before a segment's closing event, the runtime records every
  `[source:N]` its text showed against the turn (`cited_in: ["message:<turn id>"]`), and an
  artifact's body records its own (`artifact:<id>`). A client that reads the sources when
  the run ends sees them. Each source is updated on its own and retried when two
  citations land at once; a number no source has is skipped. Recording is best effort: a
  failure is logged, never turned into a failed turn or save.
- **The model is told how to cite.** Every agent with tools gets a `<citations>` block in
  its instruction (`internal/agents/build.go`).

Storage, all in OpenSearch, so document-level security covers it:

| Index | What |
|---|---|
| `sources`, id a name-based UUID of `{root, identity}`, or a UUIDv7 when there is no identity | `id, root_id, number, thread_id, run_id, tool, tool_call_id, system, key, url, title, metadata (flat_object), retrieved_at, cited_in` |
| `counters`, id `{root}` | `sources` (the last number), `artifacts` (each artifact's last version) |

`GET /v1/threads/{id}/sources` lists them by number; `GET /v1/threads/{id}/sources/{n}`
returns one. Deleting the root thread deletes the records and the counters.

`search_records` (connector `records`) is the demonstration: it searches a few sample
records and records every hit with the row's id, marking and control set as metadata.
`webfetch` records each page it reads (the final URL, the page title).

## Artifacts

`emit_artifact` publishes a document; `internal/artifacts` stores it:

- the record in OpenSearch `artifacts` (id a name-based UUID of `{root, artifact id}`): `id, root_id,
  thread_id, run_id, title, kind, language, url, media_type, size, version, created_at, updated_at`;
- the body in S3 at `artifacts/{root}/{artifact id}/{version}`.

An artifact id is letters, digits, `.`, `_` and `-`, starting with a letter or digit (it
is part of a document id, an S3 key and a URL); the model may name one, and gets a UUID
otherwise.

Kinds with a body: `markdown`, `code`, `json`, `csv` (there is no HTML kind: model-written
markup is the one kind not worth its risk). Kind `url` has no body: it points at a page
the UI embeds. Kind `image` is an image (see Images below): its body is the image, its
`media_type` the type sniffed from it, and it cites no sources. Publishing the same id again stores a new version under its own key.
Versions come from the conversation's counters document (never below the current
record's version, so an artifact saved before its counter existed keeps counting up),
and the record is written with
OpenSearch external versioning, so it only moves forward: two saves of one artifact at
once both keep their bodies, and the higher version is served. Superseded bodies stay in
S3, unread, until the thread is deleted. The tool's result, and the `artifact` activity
the encoder makes of it, is a summary without the body (`artifacts.Summary`): `{id,
title, kind, version, language?, url?}`. The UI fetches the body from
`GET /v1/threads/{id}/artifacts/{artifactId}/content`, served with the kind's content
type and `Content-Security-Policy: sandbox`; `GET /v1/threads/{id}/artifacts` lists the
conversation's artifacts, and `GET /v1/artifacts?kind=&after=&limit=` lists every
conversation's (of one kind, when set), newest first, with each one's thread title; a full page carries `next`,
an opaque cursor (OpenSearch `search_after` on update time, conversation and id) for the
next page's `after`, so ties never drop an artifact.

## Images

`internal/images` makes images and keeps each as an `image` artifact, so every image is
on the artifacts page. `generate_image {prompt, edit?}` (`internal/tools/builtin/generate_image.go`)
is a harness tool, offered when `models.yaml` has an `images` block: it makes the image, saves
it, and shows it in the thread as an `artifact` activity (its calls carry the UI renderer
`generate_image`). `edit` names an image of the conversation: the result is that
artifact's next version, under its title. The image model is the turn's `imageModel`
(`POST /v1/agent/chat` and the socket's `run`; the run keeps it, and a sub-agent inherits
it), else `images.model`; an unknown or non-image model, no prompt, or an edit of what is
not an image fails the call with the reason. A generation has 3 minutes and is not
retried (each is paid for).

The Images page starts a conversation with the `images` agent
(`config/default/agents/images.md`, whose one tool is `generate_image`) and the image
model picked there; the conversation is an ordinary chat, where a follow-up such as "make
it blue" edits the image. `GET /v1/artifacts?kind=image` lists every conversation's
images for the page. `GET /v1/threads/{id}/artifacts/{artifactId}/content?v=N` serves
version N, so an image shown in a chat keeps showing the version it made.

The `media_type` field is new in `deploy/opensearch/artifacts.json`: run `make indices`.

A new image is made on the OpenAI route `/images/generations`, which OpenRouter and
vLLM-Omni serve, reading `data[].b64_json`. An edit goes to `images.edit_model` on
`/images/edits` (openai-go's `Images.Edit`): a multipart form with the original as
`image`, the prompt and the model's upstream id, as vLLM-Omni takes it, and no
`response_format` (`b64_json` is the default and the only one its edits take; the size
follows the original). vLLM-Omni serves one model per server: Qwen-Image 2.1 makes and
edits, so `edit_model` is `model` again; earlier Qwen-Image models edit with
Qwen-Image-Edit on a second server (`models.yaml` has both setups). Without
`edit_model` (development on OpenRouter, which has no `/images/edits`) an edit goes to
the image model on `/images/generations` with the original in `input_references`. The
model is reached only through the tool call; clients that call the image API themselves
use the pass-through's `/v1/images/generations` and `/v1/images/edits` (MODELS.md).

S3 is any S3-compatible store through `minio-go` (`pkg/s3`); `docker-compose.yaml` runs
MinIO on `:9000` (console `:9001`, `minioadmin`/`minioadmin`). The bucket is created on
start. Deleting the root thread deletes its bodies, then its records.

## Files

`internal/rag/userfiles` keeps the files users upload (FILES.md). `search_userfiles {query,
files?, all?}` (`internal/tools/search_userfiles.go`) is offered to every agent, with the
harness tools, when files are searchable (`models.yaml` has an `embedding` block): a semantic search
over the files it names (from any workspace), else the conversation's workspace's, else
with `all` every workspace's, returning the `search_hits` best chunks (`{hits: [{fileId,
name, mediaType, seq, text?}]}`). A model whose card lists `vision` is shown the images
found, after the result (`tools.ShowFound`, a before-model callback: adk
leaves media out of tool results); audio and video hits are named only.

## Tool UI renderers

A tool can name a renderer: `toolx.Spec.UI` for a function tool, `ui: {tool: name}` in
an MCP connector for an MCP tool. The encoder puts it in `TOOL_CALL_START` as
`metadata: {ui: "<name>"}`, so stored history renders the same way. The UI keeps a
registry of renderers by name (`agentui/components/chat/messages/tools/registry.ts`); a
call with no renderer, or one waiting for approval, renders as the plain tool line. The
renderer receives the tool part: name, input, output, state.

## MCP tool hooks

MCP tools are not built with `toolx`, so the MCP connector says per tool what the harness
does, through adk's before- and after-tool callbacks (`internal/agents/mcptools.go`):

| Key | What the harness does |
|---|---|
| `thread_args: {tool: arg}` | writes the conversation's root thread id into `arg` before the call, so the model never supplies it |
| `ui: {tool: renderer}` | names the tool's UI renderer |
| `artifacts: {tool: {id, title, url}}` | after a successful call, reads those fields of the structured result and records a `url` artifact, shown in the thread at once |

Open-slide uses all three:

```yaml
- id: slides
  type: mcp
  mcp:
    endpoint: ${OPEN_SLIDE_MCP_URL}
    thread_args: {deck_create: thread_id, deck_list: thread_id, thread_cleanup: thread_id}
    ui: {deck_create: slides, deck_write: slides, deck_preview_url: slides}
    artifacts:
      deck_create: {id: deck_id, title: title, url: preview_url}
  http: {forward: true}
```

In the UI the `slides` renderer shows each deck call as one line. When the run creates a
deck, the artifact panel opens on the deck's live preview (the open-slide editor in an
iframe); when it writes slides, the preview reloads. The panel's refresh button reloads it
by hand. The deck stays an artifact of the conversation, listed in the artifact panel when
the thread is opened again; its files stay on the open-slide server.

## Code map

| File | Role |
|---|---|
| `pkg/transport/transport.go` | the HTTP client builder |
| `pkg/toolx/toolx.go`, `context.go` | `New`, `Spec`, `Tool`, `Connector`, `Toolset`; `Context`, `Run`, `WithRun`, `Headers` |
| `pkg/citations/citations.go` | `Source`, `Origin`, `Recorder`, `Numbers` |
| `internal/tools/tools.go` | the catalog of world tools |
| `internal/agents/connectors.go` | one client and toolset per connector |
| `internal/agents/mcptools.go` | the MCP tool hooks |
| `internal/sources/sources.go` | numbering, records, citations |
| `internal/artifacts/artifacts.go` | records and bodies |
| `pkg/s3/s3.go` | S3 through minio-go |
| `pkg/db/opensearch/sources.go`, `artifacts.go`, `counters.go` | the `sources`, `artifacts` and `counters` indices |
| `internal/handler/sources.go`, `artifacts.go` | the read API |
| `internal/stream/encode.go` | `ToolUI` → `TOOL_CALL_START` metadata |
