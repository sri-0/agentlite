# agentlite — design and migration plan

Status: proposal (2026-10-07). This is a clean rebuild of `~/code/agentic`, not a refactor.
Audience: the engineer or coding agent who will implement it, phase by phase.

The brief: Go 1.27, adk-go v2, Echo, WebSockets for the agent stream, OpenAI-style
endpoints as pure reverse proxies, one protocol on the wire and in the database, resumable
streams, and a small but real multi-agent demo. Simple, readable code that does exactly what
is needed.
---
## 0. Decisions at a glance

| Decision | Choice | Why (short) |
|---|---|---|
| Toolchain | Go **1.27.1**, module `agentlite` | adk v2.5.0 needs ≥1.26.6, opensearch-go v4.8 needs ≥1.26.8; 1.27 graduates `encoding/json/v2` and adds a stdlib `uuid` package. |
| Agent framework | `google.golang.org/adk/v2` **v2.5.0** | Current line (v1 is backports only). In-tree OpenAI-compatible model, parallel tool execution, HITL confirmation, `sessiontestsuite`. |
| HTTP | `github.com/labstack/echo/v5` **v5.4.0** | Stable since Jan 2026, slog-native, struct context, graceful shutdown built in. |
| WebSocket | `github.com/coder/websocket` **v1.8.15** | Context-first API, safe concurrent writes, zero deps, already an adk dependency. gorilla has had no release in 28 months. No stdlib WebSocket exists. |
| Wire protocol | **AI SDK UI Message Stream** (the `ai@6` chunk vocabulary) over a thin WebSocket envelope | The UI is on `useChat`; the persisted `UIMessage` is the render model; the reducer is a documented reference implementation; HITL is a first-class client flow. AG-UI has better sub-agent vocabulary but its message model is OpenAI-shaped, it specifies no resume, and there is no AG-UI→`useChat` adapter. We borrow AG-UI's attribution field names inside our data parts. |
| Storage format | The reduced **`UIMessage`** (`{id, role, parts, metadata}`) | Store what the UI renders. No conversion on load. |
| Live log / resume | **Valkey Streams**, one stream per run, cursor = stream entry id [[ one valkey stream should line up with a thread ]] | Cheap append, blocking tail reads, per-client cursors, TTL. Works across server instances. |
| Durable store | **OpenSearch** (`threads`, `messages`) | Already in the stack; `parts` stored unindexed. [[what do you mean parts are stored unindexed, each message should get its own document? The entire thread will persist in opensearch, this will happen in the background now though, you dont need to write each message to openseach, we will push it to kafka later on ]] |
| Kafka | **Not in v1.** Optional secondary sink later (§3.6) | Live fan-out with per-connection cursors is a poor fit; a connector can only persist raw chunks, not the reduced message. [[ ignore kafka for now, as long as the design is easy for me to stream the agent(s) response stream into kafka instead of redis in future ]] |
| LLM context [[ lets expand this if possible, ask me questions if u need clarification, but lets plan how we want context management to work with mutliple agent/subagent/swarm architecture]] | ADK sessions are **run-scoped**, seeded from persisted messages at run start | One durable truth (messages). Sessions live in memory (phase 2) then Valkey (phase 3) so HITL resume survives restarts. |
| Multi-agent [[ multi-agent (agent swarm) is one pattern, we still want the auto router pattern (adk has a new router we should consider for this, and we still want custom agents/subagents for things like deep research, data analyst, etc ]] | Coordinator = ordinary `llmagent` + a `task` tool; typed roster from markdown | adk v2 executes parallel tool calls concurrently (verified in source), so N `task` calls = N parallel sub-agents with no extra machinery. |
| Providers | adk `model/openaimodel` (`APIChatCompletions` or `APIResponses`, `BaseURL`, custom `HTTPClient` for mTLS) [[ each provider will need its own HTTPClient instantiated, as some providers will have custom headers required (dont stub the headers out, just ensure each provided gets a dedicated client transport, based off the base transport )]] | Deletes ~800 lines of custom model code. One open question on reasoning deltas (§8). |
| OpenAI endpoints | `httputil.ReverseProxy` with `Rewrite`, one transport per provider [[ this makes sense, ideally use the adk straight LLM or ]] | No body parsing, SSE passes through, `Director` is deprecated. |
| Logging / config | `log/slog`; `sethvargo/go-envconfig`; `go.yaml.in/yaml/v3` | stdlib logging matches Echo and adk; `gopkg.in/yaml.v3` is unmaintained. [[ use zerolog for logging, and whater yaml pkg is best.]] | 
| UI | `agentui` stays. New WebSocket `ChatTransport`; delete ~700 lines of workaround hooks [[ this is good, does the socket get created when a tread is opened? or is it a single socket for the whole app, and the client subscribes/unsubs to threads as they open/close them in the UI. Opening a thread should always hit opensearch first for the historical messages, then subscribe for the new ones being streamed, using a cursor to get where the last message was and consume from there using redis stream ]] | The render layer already consumes `UIMessage.parts`; only the transport and a few part types change. |

---

## 1. Goals and non-goals

**Goals (v1, phases 0–4)**

1. Pure reverse proxies for `/v1/chat/completions`, `/v1/responses`, `/v1/messages`, `/v1/embeddings`.
2. One agent chat WebSocket per thread [[ or a single websocket for the app, and sub/unsub? ]], speaking AI SDK UI message chunks, resumable from a cursor.
3. Every run's reduced `UIMessage` persisted to OpenSearch; thread load returns it unchanged.
4. A `chat` agent, a `coordinator` with parallel typed sub-agents, five or six real tools, one
   approval tool, one client-answered tool (`question`).
5. `agentui` talks only to this server, through one base URL, with native AI SDK approvals.

**Non-goals for v1** (kept in the migration matrix, §5, as later phases): RAG, long-term memory,
skills, prompts, MCP [[ ensure we have mcp support too now please, these should use adk's mcptool, and allow us to configure them via a yaml]], Confluence, compaction, LLM-generated titles, the `auto` router [[ include auto classifier router, look at adk options for this too]],
attachments, regenerate, Anthropic native model, office artifacts, OAuth, Kafka.
[[ the /v1/messages anthropic can just be a reverse proxy too, since vllm supports it.]]
---

## 2. Research summary

Facts that shaped the design (verified against module sources and installed packages on
2026-10-07; full reports are in the session that produced this plan).

**adk-go v2.5.0** (`/Users/sri/go/pkg/mod/google.golang.org/adk/v2@v2.5.0`)
- `agent.Agent` cannot be implemented by hand (unexported method); custom agents use `agent.New(agent.Config{Run: ...})`.
- `llmagent.New(llmagent.Config{Name, Model, Instruction, Tools, SubAgents, OutputKey, Mode, ...})`. Root LLM agents must be `ModeChat` (default).
- `runner.New(runner.Config{AppName, Agent, SessionService, AutoCreateSession, Compaction})`;
  `Run(ctx, userID, sessionID, *genai.Content, agent.RunConfig{StreamingMode: StreamingModeSSE}) iter.Seq2[*session.Event, error]`.
  Partial events are yielded but not persisted; non-partial events are appended to the session.
- `session.Event` embeds `model.LLMResponse` (`Content`, `Partial`, `UsageMetadata`, `FinishReason`) plus `Author`, `Branch`, `InvocationID`, `LongRunningToolIDs`, `Actions{StateDelta, RequestedToolConfirmations, ...}`. JSON tags exist now.
- `session.Service` = `Create/Get/List/Delete/AppendEvent`; in-tree impls: in-memory, GORM database, Vertex. **No Valkey impl**; `session/sessiontestsuite` is a conformance suite for custom ones.
- Tool execution: `handleFunctionCalls` runs every function call in the model turn as a concurrent goroutine (`base_flow.go:1306-1325`). Long-running tools (`IsLongRunning()`) that return `nil` produce no `FunctionResponse`; the event becomes final and the run pauses until the next `Run` carries a matching `FunctionResponse`.
- HITL: `functiontool.Config{RequireConfirmation: true}` or `ctx.RequestConfirmation(hint, payload)` emits an agent event with `FunctionCall{Name: "adk_request_confirmation", ID: <approvalId>, Args: {originalFunctionCall, toolConfirmation}}` and pauses. Resume = `Run` again with a user content containing `FunctionResponse{ID: <approvalId>, Name: "adk_request_confirmation", Response: {"confirmed": bool, "payload": ...}}`. `toolconfirmation.OriginalCallFrom(fc)` extracts the wrapped call.
- `model/openaimodel.NewModel(ctx, name, &ClientConfig{APIKey, BaseURL, HTTPClient, API})`. Marked experimental. Chat Completions mode reads text, refusals and tool calls only (no OpenRouter `reasoning` field); Responses mode maps reasoning to thought parts. No Anthropic model in tree.
- Callbacks (`BeforeModel`, `AfterTool`, ...) all take the unified `agent.Context`. `plugin.OnEventCallback` can observe every event globally.

**Echo v5.4.0**: `func(c *echo.Context) error`; `c.Response()` is an `http.ResponseWriter` that hijacks, so `websocket.Accept(c.Response(), c.Request(), opts)` works directly; `echo.StartConfig{Address, GracefulTimeout, BeforeServeFunc}.Start(ctx, e)`.

**Go 1.27.1**: `encoding/json/v2` and `jsontext` stable, stdlib `uuid`, generic methods, `testing/synctest`. `httputil.ReverseProxy.Director` deprecated in 1.26 (use `Rewrite`); `FlushInterval` auto-switches to `-1` for `text/event-stream`.

**AI SDK `ai@6.0.204`** (installed in `agentui`; npm latest is 7.0.130, see §8)
- 27 chunk types. The ones we emit are listed in §3.3. No `reset-step`, `custom`, `reasoning-file`, `tool-approval-response` in v6.
- `ChatTransport` = `sendMessages({trigger, chatId, messageId, messages, abortSignal, headers?, body?, metadata?}) → ReadableStream<UIMessageChunk>` and `reconnectToStream({chatId, ...}) → ReadableStream | null`. `useChat({resume: true})` calls `reconnectToStream` once on mount; nothing else is automatic.
- One `sendMessages` stream = one assistant message. If the client's last message is an assistant message, the stream continues into it in place (same `messageId`); a second `start` with a different id corrupts state.
- Reducer rules (`processUIMessageStream`) are documented in Appendix B. `data-*` parts reconcile by `type`+`id`; `transient: true` parts only reach `onData`. An `error` chunk hard-fails the request (`status: 'error'`).
- Approval: `tool-approval-request{approvalId, toolCallId}` → part state `approval-requested` → `addToolApprovalResponse({id, approved, reason})` → with `sendAutomaticallyWhen: lastAssistantMessageIsCompleteWithApprovalResponses` the client calls `sendMessages` again with the assistant message last, containing a part in state `approval-responded`.
- Client-answered tools: `addToolOutput({tool, toolCallId, output})` + `lastAssistantMessageIsCompleteWithToolCalls` does the same round-trip with the part in state `output-available`.

**AG-UI 1.0** (`@ag-ui/core@1.0.2`): native `SUBAGENT_STARTED/FINISHED/ERROR` with `subagentRunId` on every attributable event; explicitly "no stream resumption" in the HTTP+SSE binding; messages are OpenAI-shaped; community Go SDK only; no adapter to `useChat`.

**Current `agentic`/`agentui`** (what the rewrite must honour; details in §5)
- The UI already calls the Go server directly (no Next route handlers), with `X-User-ID` for identity and two base-URL env vars that point at the same server.
- The live encoder, the event-log replay and the OpenSearch projection are three hand-synced copies of the part shapes; sub-agent deltas are persisted one part per token; run records are never persisted; ADK sessions run as user `"default"`.
- Worth keeping as ideas: the typed roster with markdown definitions, the `task`-tool dispatch model, the `question` tool schema, task-board clamping, provider mTLS config, ownership checks that return 404.

---

## 3. Architecture

### 3.1 Overview

```
 browser (agentui, useChat + WsChatTransport)
   │  REST: /v1/threads, /v1/agents, /v1/models            WS: /v1/threads/{id}/stream
   ▼
 ┌───────────────────────────── agentlite (Echo v5) ─────────────────────────────┐
 │ httpapi      proxy ──────────────────────────► upstream providers (OpenRouter, │
 │   │          /v1/chat/completions, /v1/responses, /v1/messages, /v1/embeddings)│
 │   │ ws                                                                         │
 │   ▼                                                                            │
 │ run.Service ── Start / Resume / Cancel / Subscribe                             │
 │   │  │                                                                         │
 │   │  └──► run.Log (Valkey stream per run)  ◄── Subscribe tails from a cursor   │
 │   ▼                                                                            │
 │ engine (adk v2 runner) ──► translate(session.Event) ──► protocol.Chunk         │
 │   │  tools: calculate, read_url, todowrite, emit_artifact, write_note,         │
 │   │         question, task(→ child runner, attributed chunks)                  │
 │   ▼                                                                            │
 │ protocol.Reduce(chunks) ──► UIMessage ──► store (OpenSearch threads/messages)  │
 └────────────────────────────────────────────────────────────────────────────────┘
```

One process does everything; nothing is pinned to a process except the goroutine that is
executing a run. Run state, the chunk log and the ADK session live in Valkey, so a client may
reconnect to any instance and a paused run may be resumed from any instance.

### 3.2 Domain model

| Entity | Lives in | Shape |
|---|---|---|
| **Thread** | OpenSearch `threads` | `{id, user_id, title, agent_id, model, created_at, updated_at}`. Id is client-generated (the UI already mints a nanoid); the server upserts on first run. |
| **Message** | OpenSearch `messages` | A `UIMessage` plus `{thread_id, user_id, seq, created_at}`. `seq` is a per-thread integer assigned at persist time; it is the sort key. `parts` and `metadata` are stored unindexed. |
| **Run** | Valkey hash `run:{id}` | `{id, thread_id, user_id, agent_id, model, message_id, status, segment_start, started_at, updated_at, error}`. `status ∈ running \| awaiting_input \| done \| error \| cancelled`. One run produces exactly one assistant message. |
| **Chunk** | Valkey stream `run:{id}:log` | One `UIMessageChunk` per entry, JSON in field `c`. Entry id is the cursor. |
| **Segment** | implicit | The chunks between a `start` and the next `finish`/`error`. A run has one segment unless it pauses for input; each pause ends a segment and each resume starts one. `segment_start` on the run is the cursor of the current segment's first entry. |

Message ids: the user message keeps the id the client generated (so live and reloaded ids
agree without a naming scheme); the assistant message id is minted by the server with
`uuid.NewV7()` at run start and sent in the `start` chunk.

### 3.3 The protocol

The wire and storage format is the AI SDK v6 UI Message Stream. We emit this subset:

| Chunk | When |
|---|---|
| `start {messageId, messageMetadata}` | first chunk of every segment; metadata carries `threadId, runId, agentId, model, createdAt` |
| `start-step` / `finish-step` | around every model call of the output agent (required for the SDK's approval helpers) |
| `text-start/delta/end {id}` | output agent text |
| `reasoning-start/delta/end {id}` | output agent reasoning |
| `tool-input-start {toolCallId, toolName, dynamic:true, title?}` + `tool-input-available {input}` | output agent tool call (adk gives whole args; no `tool-input-delta`) |
| `tool-output-available {toolCallId, output, dynamic:true}` / `tool-output-error {errorText}` / `tool-output-denied` | tool result |
| `tool-approval-request {approvalId, toolCallId}` | approval tool paused the run |
| `data-task-list`, `data-artifact`, `data-agent`, `data-agent-delta` | custom parts, below |
| `message-metadata {messageMetadata}` | optional mid-run metadata updates |
| `finish {finishReason, messageMetadata}` | last chunk of a segment; metadata adds `durationMs, usage` |
| `error {errorText}` | run failed; replaces `finish` and ends the segment |

Everything is `dynamic: true` because tools are defined at runtime; the UI's `ToolCard`
already renders `dynamic-tool` parts.

**Custom data parts** (the `ChatDataParts` type in the UI). Field names for attribution are
AG-UI's so a future AG-UI encoder is a mechanical mapping.

```ts
"agent": {                           // one part per sub-agent invocation, id = subagentRunId, replaced on change
  subagentRunId: string; name: string; description?: string;
  parentToolCallId: string;          // the coordinator's `task` call that spawned it
  status: "running" | "done" | "error"; startedAt: number; durationMs?: number; error?: string;
  text: string;                      // folded final text (empty until done)
  tools: { toolCallId: string; toolName: string; state: "input-available" | "output-available" | "output-error";
           input?: unknown; output?: unknown; errorText?: string }[];
}
"agent-delta": { subagentRunId: string; kind: "text" | "reasoning"; delta: string }   // transient: true, never persisted
"task-list":   { tasks: { id: string; title: string; status: "pending" | "in_progress" | "completed" | "cancelled" | "error";
                         subagentRunId?: string }[] }                                   // id = "tasks", replaced
"artifact":    { id: string; title: string; kind: "markdown" | "code" | "html" | "json" | "csv" | "file";
                 content?: string; language?: string; url?: string; filename?: string; mime?: string; size?: number }  // id = artifact id
```

Rules:
- A `data-agent` snapshot is emitted at start, at every tool boundary of the sub-agent, and at
  done/error. Never per token. Per-token sub-agent output goes out as transient
  `data-agent-delta`; the client accumulates deltas in component state and discards them when
  the next snapshot for that id arrives. After reload only snapshots exist, so persisted equals
  rendered.
- `data-task-list` is a snapshot; statuses never regress (clamp as today's `tasks.Clamp`), and
  identical snapshots are not re-sent.
- Message metadata: `{threadId, runId, agentId, model, createdAt, durationMs, finishReason?,
  usage?: {inputTokens, outputTokens, totalTokens, contextWindow}}`. Usage replaces today's
  `data-usage` part.
- Dropped from today's contract: `data-agent-step`, `data-agent-delta` (as persisted parts),
  `data-agent-progress`, `data-tool-interrupt`, `data-usage`, and the non-standard
  `startedMs/endedMs` on reasoning parts (the UI can compute thinking duration from
  `reasoning-start`/`reasoning-end` arrival and store it in a `data-agent`-style part if it
  wants it after reload; v1 shows duration live only).

### 3.4 The WebSocket transport

One socket per thread: `GET /v1/threads/{threadId}/stream` (upgrade). [[ maybe the websocket is simply at `/v1/agent/chat`]] The socket is a
long-lived control channel; each `useChat` request maps to one segment of chunks.

Client → server frames:

```jsonc
{ "type": "run",       "message": <UIMessage>, "agentId": "coordinator", "model": "openai/gpt-4.1", "reasoningEffort": "medium" }
{ "type": "subscribe", "after": "<cursor>" }    // after is optional
{ "type": "cancel" }
```

Server → client frames:

```jsonc
{ "type": "chunk", "seq": "1759840000000-0", "chunk": { "type": "text-delta", "id": "t1", "delta": "Hel" } }
{ "type": "idle" }                              // reply to subscribe when the thread has no active run
{ "type": "error", "message": "thread belongs to another user" }   // protocol-level error, socket closes
```

Semantics:
- `run` with a **user** message starts a new run. `run` with an **assistant** message (the
  SDK sends the last message after `addToolApprovalResponse` / `addToolOutput`) resumes the
  paused run: each tool part in state `approval-responded` becomes an
  `adk_request_confirmation` FunctionResponse, each pending `output-available` part becomes a
  FunctionResponse for that tool call. The transport does not need to know the difference; the
  server dispatches on `message.role`. Rejected while a run is `running` (one run per thread).
- After `run` the server streams the new segment from its first entry. After `subscribe` the
  server streams from `after` if given, otherwise from `segment_start` of the active run. The
  stream for a request ends at the segment's `finish`/`error` chunk; the socket stays open.
- `cancel` cancels the run goroutine (any instance: it is a Valkey pub/sub notification or a
  flag the goroutine polls between events; v1 = pub/sub channel `run:{id}:ctl`).
- Only one subscription per socket is active; a new `run`/`subscribe` replaces it.

Client transport (sketch; full code in phase 2):

```ts
class WsChatTransport implements ChatTransport<ChatMessage> {
  // one socket per chatId, lazy connect, reconnect with backoff; cursor = last seq seen
  async sendMessages({ messages, abortSignal, body }) {
    send({ type: 'run', message: messages.at(-1), ...body });
    abortSignal?.addEventListener('abort', () => send({ type: 'cancel' }));
    return this.readable();      // closes on finish/error chunk; on socket drop, resubscribes from cursor into the same stream
  }
  async reconnectToStream() {
    send({ type: 'subscribe' });
    return (await this.firstFrame()).type === 'idle' ? null : this.readable();
  }
}
```

Sequences:

```
new turn           : run(user msg) ──► start … finish          (UI status: submitted → streaming → ready)
mid-stream drop    : socket closes ─► transport reconnects ─► subscribe{after: cursor} ─► remaining chunks (SDK never notices)
page reload, running: GET messages (persisted, excludes in-flight segment) ─► resume:true ─► subscribe ─► replay segment from start
page reload, paused : GET messages (includes the partial assistant message with approval-requested part) ─► subscribe ─► idle
approval           : addToolApprovalResponse ─► run(assistant msg) ─► start{same messageId} tool-output-available … finish
```

Why the "page reload while running" case works: the in-flight segment has not been persisted
yet, so the persisted message is either absent (first segment) or the state at the last pause
(later segments); replaying the current segment from its first chunk onto that state is exactly
what the SDK's reducer expects (`start` with the same id continues the last assistant message).

### 3.5 Run lifecycle and resumability

```
Start(thread, user, message, agent, model)
  ├─ reject if thread:{id}:run exists (409)
  ├─ upsert thread; persist user message (seq N)
  ├─ create run hash, set thread:{id}:run = runId (lease, see below), SADD user:{id}:runs
  └─ go execute(run, segment 1)

execute(run, segment):
  msg := persisted partial message or empty assistant message
  for chunk in engine.Run(...):           // translator output
      seq := log.Append(run.ID, chunk)    // XADD
      protocol.Reduce(&msg, chunk)
      if first chunk of segment: run.segment_start = seq
  persist msg (seq N+1, upsert by message id)
  status := done | awaiting_input | error | cancelled
  update run hash; if terminal: DEL thread:{id}:run, SREM user:{id}:runs, EXPIRE log 24h

Resume(thread, assistant message)        // status must be awaiting_input
  └─ build FunctionResponses from the message's responded tool parts; go execute(run, next segment)

Cancel(thread)  → publish run:{id}:ctl "cancel" → goroutine cancels its context → finish{finishReason:"other"}, status cancelled
Subscribe(thread, after) → run := active run or nil → XREAD loop from after|segment_start until a finish/error entry
```

Valkey keys:

| Key | Type | Content | Lifetime |
|---|---|---|---|
| `run:{id}` | hash | run record | 7 days |
| `run:{id}:log` | stream | chunks, no `MAXLEN` (runs are bounded; trimming silently breaks replay, which is a bug in today's code) | 24 h after terminal |
| `thread:{id}:run` | string | active run id | lease: TTL 60 s refreshed every 20 s while `running`; `PERSIST` while `awaiting_input`; deleted at terminal |
| `user:{id}:runs` | set | active run ids for the sidebar | members removed at terminal |
| `run:{id}:ctl` | pub/sub | `cancel` | — |
| `adk:session:{runId}` (+`:events`, `:state`) | hash/list/hash | run-scoped ADK session (phase 3) | 7 days |

Orphan rule: if a subscriber finds a run whose hash says `running` but whose lease key is gone,
the process that executed it died. The subscriber marks the run `error`, appends
`error{errorText:"run was interrupted"}` to the log and persists the partial message. This is
the only crash-recovery path and it is one small function.

Concurrency: one goroutine per run writes the log; N subscribers read it. Child agents emit
through the same `Sink` as the parent (the `task` tool holds the parent's sink), serialised by a
mutex in the sink, so the log has one writer per run.

### 3.6 Valkey Streams vs Kafka

| Concern | Valkey Streams | Kafka + Connect |
|---|---|---|
| Live fan-out to browsers with per-client cursors | `XREAD BLOCK` from any id; one blocking connection per subscriber | A consumer per browser is unworkable; you need a bridge process that consumes once and fans out in memory, which is what Valkey already is |
| Resume after reconnect | cursor = entry id, replay with `XRANGE` | seek by offset; same capability, more client code |
| Persistence of the reduced message | the run goroutine writes it at segment end (one index call) | a connector persists **raw chunks**; reduction then happens on every read, which is the conversion the brief wants to avoid |
| Retention | `EXPIRE` per run | topic retention; fine |
| Ops | one Valkey (already required) | brokers + Connect + schema + DLQ |
| Where it shines | hot path | audit trail, analytics, multiple downstream consumers |

Decision: Valkey for the hot path. The run goroutine already has every chunk in hand, so the
log writer is an interface (`run.Log`) with two implementations (memory for tests, Valkey for
real). If an audit copy in Kafka becomes a requirement, add a third `Log` that tees to a
franz-go producer with the Aiven OpenSearch sink (v4.1.0) on the far side; it never sits on the
live path and nothing else changes.

### 3.7 Persistence

**OpenSearch indices** (created on boot if missing):

```
threads  : id (keyword), user_id (keyword), title (text), agent_id, model (keyword), created_at, updated_at (date)
messages : id (keyword), thread_id, user_id, role (keyword), seq (integer), created_at (date),
           parts (object, enabled: false), metadata (object, enabled: false)
```

Reads: thread list by `user_id` sorted by `updated_at desc`; messages by `thread_id` + `user_id`
sorted by `seq asc`. Every endpoint filters by the caller's user id and returns 404 for another
user's thread (never 403), on reads and writes alike.

**The reducer** (`protocol.Reduce`) is a line-by-line port of the SDK's
`processUIMessageStream` (Appendix B), about 200 lines. It runs in the run goroutine as chunks
are emitted, so persistence never re-reads the log. Golden tests feed recorded chunk sequences
and compare against the `UIMessage` produced by the TypeScript `readUIMessageStream` for the
same input (fixtures generated once by a small node script in the UI repo).

**ADK sessions are run-scoped and seeded.** At run start the engine converts the thread's
persisted messages into session events (the Go equivalent of AI SDK's `convertToModelMessages`):

- user message: text parts → `genai.Content{Role: "user"}`;
- assistant message: text parts → model text; tool parts with `output-available` → a model
  `FunctionCall` plus a user `FunctionResponse`; denied → `FunctionResponse{error}`; reasoning
  and data parts are dropped. Assistant events are authored as the agent that is about to run.

This keeps one durable copy of the conversation. The cost is one short conversion per run
(hundreds of pipelined writes at most). What it gives up: cross-turn ADK session state and
ADK's built-in compaction, neither of which v1 needs. If either becomes necessary, switch the
seeding to a durable `session.Service` on OpenSearch; the engine interface does not change.

Phase 2 uses `session.InMemoryService()` per run. Phase 3 adds a Valkey `session.Service`
(hash for metadata, list of JSON events, hash for state; `AppendEvent` = `RPUSH` + `HSET`)
tested with `session/sessiontestsuite`, so a paused run can resume after a restart or on another
instance.

### 3.8 Agents

**Roster.** Agent definitions are markdown files in `config/agents/*.md` with YAML frontmatter
(the format `agentic` already uses) and the body as the instruction:

```yaml
---
id: coordinator
name: Coordinator
description: Breaks a request into tasks and dispatches typed sub-agents in parallel.
mode: primary            # primary | subagent | all
model: openai/gpt-4.1
tools: [task, todowrite] [[ if no tools specified, assume the agent can access any tool ]]
subagents: [researcher, writer] [[ i want to get rid of this pattern, allow each agent/subagent to be defined in its own file. and then top level agents can specify the name of the subagents. dont nest them underneath in the yaml]]
---
You coordinate ... <available_subagents> is appended by the builder.
```

The registry holds **definitions** only. Agents are built per run (`llmagent.New` is cheap),
which makes per-request model override trivial and removes global mutable agent state.

**Builder.** `definition + model override → llmagent.New(Config{Name, Instruction, Model:
openaimodel, Tools, GenerateContentConfig{MaxOutputTokens}})`. The anti-loop reminder ("call
any single tool at most three times") stays as a one-line instruction suffix because it exists
for a documented reason (weak models looping on tools).

**Coordinator = llmagent + `task` tool.** The `task` tool
(`task(subagent_type, description, prompt)`):

1. validates `subagent_type` against the coordinator's `subagents` list;
2. builds the child agent, a fresh in-memory session and a child `runner`;
3. mints `subagentRunId`, emits `data-agent{status: running}` and a `data-task-list` row, and
   translates every child event into attributed chunks on the parent's sink (`data-agent-delta`
   per token, `data-agent` snapshot per tool boundary);
4. returns the child's final text as the tool result and emits the final `data-agent{done}`
   snapshot and the task-list update.

adk runs all `task` calls of one model turn concurrently, so parallelism is the model's choice
and needs no `background`/`join` tools. A semaphore caps concurrent children (4). Children do
not receive `task`, `question` or approval tools in v1 (no nested dispatch, no HITL inside a
child); a child that errors becomes `data-agent{status: error}` plus a `tool-output-error` on
the parent's `task` call, never an `error` chunk.

**Translator rules** (`engine/translate.go`, the only ADK→protocol conversion):

- Root-agent events (`Author == root name`): partial text → `text-*`, partial thought →
  `reasoning-*`; a text block closes an open reasoning block and vice versa; a `FunctionCall`
  closes both. A non-partial event whose text was already streamed is ignored; if no partials
  were seen (non-streaming model) its text is emitted whole.
- `FunctionCall` → `tool-input-start` + `tool-input-available`. `FunctionResponse` →
  `tool-output-available` (plus `data-artifact` for `emit_artifact`, `data-task-list` for
  `todowrite`). `finish-step` after a function-response event and `start-step` before the next
  model event.
- `adk_request_confirmation` → emit the **original** call as `tool-input-start/available`
  (`title` = hint), then `tool-approval-request{approvalId: <confirmation call id>, toolCallId:
  <original id>}`, `finish-step`, `finish{finishReason: "tool-calls"}`; run → `awaiting_input`.
- A long-running tool call (`question`) → `tool-input-start/available`, `finish-step`,
  `finish{"tool-calls"}`; run → `awaiting_input`. On resume the first chunks are `start{same
  messageId}`, `tool-output-available{output}` (the client's answer), `start-step`.
- `UsageMetadata` accumulates per model call → `finish.messageMetadata.usage`
  (`contextWindow` from `models.yaml`).
- Events from other authors (adk workflow sub-agents, later) are attributed by `Author`
  exactly like task children; v1 has none.

### 3.9 Tools

| Tool | Kind | Purpose in v1 |
|---|---|---|
| `calculate` | function | deterministic demo tool |
| `read_url` | function | fetch a URL and return readable text (real, no API key) |
| `todowrite` | function | task list → `data-task-list` |
| `emit_artifact` | function | → `data-artifact` |
| `write_note` | function, `RequireConfirmation: true` | the approval (HITL) demo; writes to a per-user notes index later, a log line in v1 |
| `question` | long-running | asks the user structured questions; schema and summary format copied from `agentic/internal/tools/question.go` |
| `task` | function, coordinator only | typed sub-agent dispatch (§3.8) |

All tools are `functiontool.New` with struct-tagged schemas; no registry indirection beyond a
`map[string]tool.Tool` built from config names. Anything that needs OpenSearch, Confluence,
memory or MCP arrives in phase 5 with the same pattern (adk has `mcptoolset` in tree).

### 3.10 Providers and the proxy
[[ can we include an option to use_v1_models endpoint or model info endpoint (more detail usually with litellm) to get all the model info, then if we have specified here too, it overwrites the config but only with the fields that we specify, it still inherits the rest. ]]
`config/models.yaml` keeps today's provider schema because the mTLS fields are a real
requirement:

```yaml
providers:
  - id: openrouter
    base_url: https://openrouter.ai/api/v1
    api_key_env: OPENROUTER_API_KEY
    ssl_client_cert_env: ...        # optional mTLS, env var names not values
    ssl_client_key_env: ...
    ssl_trust_store_env: ...
    models:
      - { id: openai/gpt-4.1, context_length: 1047576, capabilities: [tools, reasoning] }
proxy:                              # path → provider [[ unsure what this proxy thing is, these dont need defining at the proxy provider/proxy level?]]
  chat_completions: openrouter
  responses: openrouter
  messages: anthropic
  embeddings: openrouter
```

`provider.HTTPClient()` builds one `*http.Transport` per provider (mTLS if configured,
`ForceAttemptHTTP2`, `DisableCompression` for streams). The same transport feeds both the adk
model (`openaimodel.ClientConfig.HTTPClient`) and the reverse proxy.

Proxy handler per route: `httputil.ReverseProxy{Rewrite: setURL + replace Authorization +
strip X-User-ID, Transport: provider transport, FlushInterval: -1, ErrorHandler: ignore
context.Canceled}`. Bodies are never read. `/v1/models` and `/v1/agents` are served from
config, not proxied.

### 3.11 Identity, CORS, timeouts

- Identity is one function, `httpapi.UserID(c)`: `X-User-ID` header, else the `user_id` query. [[ dont worry about identity here, we will add that later. dont use fake identity/anon either]]
  parameter (browsers cannot set headers on WebSocket upgrades), else `anonymous`. Replace the
  body of that function when real auth arrives.
- CORS via Echo middleware with origins from `ALLOWED_ORIGINS`; the same list feeds
  `websocket.AcceptOptions.OriginPatterns`.
- `http.Server.WriteTimeout = 0` (WebSockets and proxied SSE are long-lived);
  `ReadHeaderTimeout = 10s`; proxy `ResponseHeaderTimeout = 60s`.

### 3.12 Repository layout and key interfaces
[[ if possible, please keep a similar repo structure where possible, we really dont want an entirely new structure here. i need this to be an easy migration. however this doesnt mean just copy the old code.]]
```
agentlite/
  cmd/agentlite/main.go        config → clients → services → echo
  internal/
    config/                    env (go-envconfig) + models.yaml + agents/*.md loaders; one Config struct
    protocol/                  chunk.go (Chunk + constructors), message.go (Message, Part), reduce.go, parts.go (custom data structs)
    run/                       run.go (Run, Status), service.go (Start/Resume/Cancel/Subscribe), log.go (Log iface + memory impl), valkey.go
    engine/                    engine.go (Run(ctx, Input, Sink)), build.go (definition → llmagent), translate.go, seed.go (messages → session events), session_valkey.go (phase 3)
    engine/tools/              one file per tool; task.go holds the child-run logic
    store/                     opensearch.go (client, indices), threads.go, messages.go
    httpapi/                   server.go (echo, middleware), proxy.go, threads.go, agents.go, ws.go, identity.go
  config/                      models.yaml, agents/*.md
  docs/                        PLAN.md (this), PROTOCOL.md (wire contract, kept in sync with agentui/lib/chat/types.ts)
  AGENTS.md                    kept current from phase 0
```

Seams (the only interfaces; everything else is concrete):

```go
// internal/protocol
type Chunk struct { Type string `json:"type"`; /* optional fields, omitzero, see chunk.go */ }
type Message struct { ID string; Role string; Parts []Part; Metadata map[string]any }
func Reduce(m *Message, c Chunk)

// internal/run
type Log interface {
    Append(ctx context.Context, runID string, c protocol.Chunk) (seq string, err error)
    Read(ctx context.Context, runID, after string) iter.Seq2[Entry, error]   // replay, then block-tail until a terminal chunk
}
type Service struct { /* log Log; runs *Runs (valkey hashes); store *store.Store; engine *engine.Engine */ }
func (s *Service) Start(ctx, StartInput) (*Run, error)
func (s *Service) Resume(ctx, threadID string, m protocol.Message) (*Run, error)
func (s *Service) Cancel(ctx, threadID string) error
func (s *Service) Subscribe(ctx, threadID, after string) (iter.Seq2[Entry, error], error)   // nil, nil when idle

// internal/engine
type Sink interface { Emit(protocol.Chunk) }
type Input struct { Run *run.Run; History []protocol.Message; Message *protocol.Message /* new user message, or nil */; Responses []ToolResponse /* on resume */ }
func (e *Engine) Run(ctx context.Context, in Input, sink Sink) (run.Status, error)   // returns when the segment ends
```

Coding rules for the implementer (they are the reason the old code got messy):

1. One wire format, one reducer, one translator. No second encoder, ever.
2. Named constants for every cross-file string (`"tasks"`, part type names, status values).
3. No fail-open: a degraded path logs at `warn` and, where the client can act on it, surfaces
   an `error` frame. Never swallow errors into defaults.
4. All configuration through `config.Config`; no `os.Getenv` elsewhere.
5. Use adk directly; do not wrap it. Build agents per run; no global agent registry of built
   objects.
6. Tests where the behaviour is subtle (reducer golden tests, translator, run lifecycle,
   lease/orphan rule, proxy SSE passthrough, Valkey session conformance). Not elsewhere.
7. Packages are nouns with one responsibility. No `shared`, `util`, `common`.

---

## 4. UI changes (`agentui`)

The render layer, stores and TanStack hooks stay. Changes, in order:

1. **One base URL.** Collapse `NEXT_PUBLIC_BACKEND_URL` and `NEXT_PUBLIC_AGENTIC_URL` into
   `NEXT_PUBLIC_API_URL` (`lib/api/client.ts`, `lib/chat/api.ts`). Drop the "no Content-Type on
   GET" workaround once Echo answers preflights.
2. **`lib/chat/ws-transport.ts`** implementing `ChatTransport<ChatMessage>` (§3.4), passed to
   `new Chat()` in `components/chat/use-agent-chat.ts` instead of `DefaultChatTransport`; set
   `useChat({chat, resume: true, experimental_throttle: 50})` and
   `sendAutomaticallyWhen: m => lastAssistantMessageIsCompleteWithApprovalResponses(m) || lastAssistantMessageIsCompleteWithToolCalls(m)`.
3. **Delete** `use-session-reconnect.ts`, `use-interrupt-resolver.ts`, `interrupt-context.ts`,
   `sse-to-chunks.ts`, the `{data, live}` branching and delayed refetch in `lib/api/threads.ts`,
   `liveHeadSeq`/`stopped`/`cancelSession` plumbing in `thread-view.tsx`, and `lib/api/sessions.ts`
   (run status now comes on each thread from `GET /v1/threads`).
4. **`lib/chat/from-history.ts`** becomes a pass-through (optionally `safeValidateUIMessages`).
   Messages arrive as `UIMessage`s.
5. **`lib/chat/types.ts`**: replace the data parts with §3.3 (`agent`, `agent-delta`,
   `task-list`, `artifact`); metadata gains `threadId, runId, createdAt, usage`.
6. **Approvals and questions natively.** `ToolCard` calls `addToolApprovalResponse({id:
   part.approval.id, approved})` and renders `output-denied`; `QuestionCard` reads
   `part.input.questions` and calls `addToolOutput({tool: 'question', toolCallId, output})`.
   Remove the `interrupts` map and `HIDDEN_TOOL_CARDS` special cases that exist only for the
   side channel.
7. **Agent cards and panel** read `data-agent` snapshots; `onData` accumulates `data-agent-delta`
   into a small per-message map keyed by `subagentRunId` (cleared on snapshot). `TaskBar` keys
   rows by `subagentRunId` and `lib/chat/tasks.ts#taskAgentKey` goes away.
8. **Usage** from `message.metadata.usage`; delete the token-estimate fallback and the 128k
   placeholder.
9. Sidebar: `GET /v1/threads` already carries `run.status`; keep the 5 s poll, drop
   `useSessions`/`useSessionStatus`.
10. Stay on AI SDK v6 for this migration (`ai@6.0.x`); the chunk vocabulary we emit is
    identical in v7 except the deprecations noted in §8, so upgrading later is a UI-only task.

Net effect: roughly 700 lines removed, one 150-line transport added, and `useChat.status` is
finally correct during reconnects and approvals (thinking indicator, Stop, `onFinish`).

---

## 5. What to keep, port, or drop from `agentic`

| Area | Today | v1 | Notes |
|---|---|---|---|
| AI-SDK encoder (`internal/stream/aisdk`) | clean | **port as reference** for chunk shapes; code is replaced by `protocol` constructors | |
| OpenAI/ag_ui encoder, `internal/sse`, non-stream paths | unused by UI | **drop** | |
| Event log (`internal/eventlog`), pump, projector | right idea, 3 copies of shapes | **redesign** into `run.Log` + `protocol.Reduce` | keep the Redis Lua atomic-append idea only if XADD alone proves insufficient (it does not; single writer) |
| Run coordinator (900 lines, in-process maps) | salvageable | **redesign** as `run.Service` with Valkey-backed records | queueing of a second turn is dropped: reject with 409 while running |
| HITL store, resume handler | works, fragile | **replace** by the WS `run` frame + adk-native resume | no separate store: the pending state lives in the ADK session and the paused run record  [[ this HITL needs more planning, we should look at what ADK go provides now, and try use the native HITL. Consider storing HITL state in redis, also we may need HITL to include additional info from the user when they click approve - eg. justification ]]|
| Chat handler (routing, RAG, templates, memory injection) | messy | **redesign**; v1 does agent + model only | RAG/templates/memory return in phase 5 as engine hooks |
| Roster (`internal/roster`), markdown defs, permissions | clean | **port** | permissions reduce to the tool list per definition in v1 |
| `task` tool + child stream translation | salvageable | **port with cleanup** (§3.8) | drop `task_join`/background; adk runs calls concurrently |
| `question` tool | clean | **port** as a long-running tool | |
| `todowrite`, `emit_artifact`, task clamping | clean | **port** | |
| Demo/stub tools (`query_*_db`, incident, alert, triage) | stubs | **drop** | |
| `pkg/genai/openai` | clean, 788 lines | **drop** in favour of `openaimodel`, unless the reasoning spike (§8) fails | |
| `pkg/genai/anthropic` | clean, unused | **later** (phase 5) if native Anthropic is wanted | |
| Valkey ADK session service | messy | **rewrite** (phase 3), conformance-tested | |
| OpenSearch client, indices | clean | **port** the two indices we need; client via opensearch-go v4 [[ use opensearch go v5 only please! ]] | |
| Memory, RAG, skills, prompts, compaction, suggestions | mixed, partly dead | **later** (phase 5) | compaction via adk `runner.Config.Compaction` once sessions are durable  [[ get memory, compaction working, prompts if the UI has it already port over. skills and suggestions can come later ]]|
| MCP manager, resilient toolset, office decorator, OAuth | salvageable / inert | **later**; prefer adk `mcptoolset` [[ not later, get MCP working too. ]] | |
| Sessions API (`/v1/sessions*`) | clean | **fold** into `GET /v1/threads` `run` field + WS `cancel` | |
| Threads/messages handlers | ungated writes | **rewrite** small, all endpoints gated | |
| Anthropic `/v1/messages` adapter | clean, unused | **drop**; `/v1/messages` is a raw proxy now [[ good so we can remove the anthropic parsing/genai encoder]] | |
| `cmd/dev`, `cmd/cli` | useful | **later**; adk's `adk web` launcher covers most of it [[ ideally just have a way to use the adk-go stuff natively. consider using bubbletea go lib as a local cli to configure things ]] | |
| Config (`internal/config`) | clean, strays | **port** the provider/model schema; single surface | |
| Tests | mixed | **port ideas**: eventlog seq/replay, task/question round-trips, cross-user 404 | |

---

## 6. Delivery phases

Each phase is one branch and ends with a checkpoint that is demonstrated, not just compiled.

**Phase 0 — Scaffold (½ day)**
`go.mod` (`go 1.27.1`), `cmd/agentlite`, `config` (env + yaml), slog, Echo server with
request-id/recover/CORS/request-logger, `/health`, identity function, `Makefile`
(`run`, `test`, `vet`), `docker-compose.yaml` (Valkey, OpenSearch), `AGENTS.md`, `.env.example`.
Checkpoint: `make run` serves `/health`.

**Phase 1 — Proxy (½ day)**
`models.yaml` loader, provider transports (mTLS), reverse proxy for the four routes,
`GET /v1/models`. Test: httptest upstream streaming SSE; assert chunks arrive unbuffered and
`Authorization` is replaced.
Checkpoint: `curl` a streamed chat completion through the proxy.

**Phase 2 — Protocol, runs, single agent, WebSocket, persistence (3–4 days)**
`protocol` (types, constructors, reducer + golden fixtures), `run` (Run, memory + Valkey log,
service with lease and orphan rule, subscribe), `engine` (build `chat` agent from a markdown
definition via `openaimodel`, seed session, translate), `store` (indices, threads, messages),
`httpapi` (threads, messages, agents, ws). UI: transport, single URL, pass-through history.
Checkpoint: chat with the `chat` agent; reload shows history; close the tab mid-answer and
reopen → the answer continues; restart the server → history intact; two tabs on one thread
both stream.

**Phase 3 — Tools and HITL (2 days)**
`calculate`, `read_url`, `todowrite`, `emit_artifact`; `write_note` with confirmation;
`question` long-running; Valkey ADK session service (`sessiontestsuite`); resume path. UI:
native approvals and question output.
Checkpoint: approve and deny; answer a question; restart the server between the request and
the approval and it still resumes.

**Phase 4 — Multi-agent (2–3 days)**
Roster with modes, `coordinator`, `researcher`, `writer`; `task` tool with attributed chunks;
task list from task calls; semaphore. UI: agent cards/panel from `data-agent`, delta
accumulation, task bar keyed by `subagentRunId`.
Checkpoint: "research X and write a summary" spawns two or three parallel sub-agents with live
deltas and tool calls visible; reload shows the final snapshots; persisted message has one
`data-agent` part per sub-agent.

**Phase 5 — Later, in priority order**
LLM titles (post-run hook with the smallest model); attachments (file parts → `InlineData`);
regenerate; `auto` router; long-term memory tools; RAG hook; MCP via `mcptoolset` + office
artifacts; skills manifest; compaction via adk; Anthropic native model; Kafka tee; real auth.

---

## 7. Testing strategy
[[ ONLY TEST LATER, use curl for testing for now, or use test scripts for your own development but do not commit them or put them in a scripts folder. ]]
| Area | Test | Why |
|---|---|---|
| `protocol.Reduce` | golden fixtures: recorded chunk sequences → expected `UIMessage` generated by `readUIMessageStream` in TS | the whole persisted==rendered promise rests on this |
| `engine/translate` | fake `session.Event` sequences (partial text, thought, tool call, confirmation, long-running) → expected chunks | the only ADK-specific logic |
| `run.Service` | memory log + fake engine: start, subscribe-after, pause/resume, cancel, orphan rule, 409 on concurrent run | lifecycle correctness |
| `run` Valkey log | integration (skipped without Valkey): append/read/tail/expire | |
| Valkey `session.Service` | `sessiontestsuite` | conformance |
| proxy | httptest SSE upstream, header rewrite, cancel propagation | |
| `httpapi` | cross-user 404 on every thread route | security invariant |

No end-to-end browser tests in v1; the phase checkpoints are manual.

---

## 8. Risks and open questions

1. **Reasoning deltas through `openaimodel`.** In Chat Completions mode the in-tree model
   ignores OpenRouter's `reasoning` field; Responses mode maps reasoning to thought parts.
   Phase 2 starts with a one-hour spike: point `openaimodel{API: APIResponses}` at OpenRouter
   and confirm streamed reasoning for the models you use. If it fails, port `pkg/genai/openai`
   (interface unchanged in v2 apart from `agent.Context`) and keep it as `engine/model_openai.go`.
2. **`openaimodel` is marked experimental.** Pin v2.5.0; read release notes on upgrade.
3. **OpenRouter requires `max_tokens`** (402 otherwise). Set `GenerateContentConfig.MaxOutputTokens`
   from config per agent and verify the model forwards it.
4. **Tool-call id length.** The old provider clamped ids to 40 chars for some upstreams; check
   whether `openai-go`-generated ids need it with your providers.
5. **AI SDK v7 drift.** The UI is on v6; v7 adds `reasoning-file`, `reset-step`, a
   `tool-approval-response` chunk and deprecates `needsApproval`. Nothing we emit breaks, but
   upgrade deliberately and re-run the golden fixtures against v7's reducer.
6. **WebSocket identity.** `user_id` as a query parameter is a placeholder; it is one function [[ dont use user_id stuff, for now just do no auth]]
   to replace. Decide the real mechanism (cookie or bearer token on upgrade) before exposing the
   server beyond localhost.
7. **Crash mid-segment loses the partial answer** (the user message stays; the client sees an
   `error` frame on resubscribe via the orphan rule). Periodic persistence of the partial is a
   ten-line addition if this matters.
8. **Run-scoped sessions drop cross-turn state.** Fine for v1; see §3.7 for the switch.
9. **Kafka.** If the target environment already runs Kafka and wants an audit stream, the tee
   `Log` is a day of work; otherwise leave it out.

---

## Appendix A — wire example

A coordinator run with one sub-agent, then an approval pause and resume. Frames are
`{type:"chunk", seq, chunk}`; only `chunk` is shown.

```jsonc
{"type":"start","messageId":"0192…","messageMetadata":{"threadId":"th_1","runId":"run_1","agentId":"coordinator","model":"openai/gpt-4.1","createdAt":1759840000000}}
{"type":"start-step"}
{"type":"reasoning-start","id":"r1"} {"type":"reasoning-delta","id":"r1","delta":"Need a search first."} {"type":"reasoning-end","id":"r1"}
{"type":"tool-input-start","toolCallId":"call_1","toolName":"task","dynamic":true}
{"type":"tool-input-available","toolCallId":"call_1","toolName":"task","dynamic":true,"input":{"subagent_type":"researcher","description":"Find sources","prompt":"…"}}
{"type":"data-task-list","id":"tasks","data":{"tasks":[{"id":"call_1","title":"Find sources","status":"in_progress","subagentRunId":"sa_1"}]}}
{"type":"data-agent","id":"sa_1","data":{"subagentRunId":"sa_1","name":"researcher","parentToolCallId":"call_1","status":"running","startedAt":1759840000500,"text":"","tools":[]}}
{"type":"data-agent-delta","transient":true,"data":{"subagentRunId":"sa_1","kind":"text","delta":"Searching"}}
{"type":"data-agent","id":"sa_1","data":{"…":"…","tools":[{"toolCallId":"c9","toolName":"read_url","state":"input-available","input":{"url":"…"}}]}}
{"type":"data-agent","id":"sa_1","data":{"…":"…","status":"done","durationMs":4210,"text":"Three sources: …","tools":[{"toolCallId":"c9","toolName":"read_url","state":"output-available","output":{"…":"…"}}]}}
{"type":"tool-output-available","toolCallId":"call_1","dynamic":true,"output":{"result":"Three sources: …"}}
{"type":"data-task-list","id":"tasks","data":{"tasks":[{"id":"call_1","title":"Find sources","status":"completed","subagentRunId":"sa_1"}]}}
{"type":"finish-step"}
{"type":"start-step"}
{"type":"tool-input-start","toolCallId":"call_2","toolName":"write_note","dynamic":true,"title":"Save the summary to your notes?"}
{"type":"tool-input-available","toolCallId":"call_2","toolName":"write_note","dynamic":true,"input":{"title":"Summary","body":"…"}}
{"type":"tool-approval-request","approvalId":"adk_c7","toolCallId":"call_2"}
{"type":"finish-step"}
{"type":"finish","finishReason":"tool-calls","messageMetadata":{"durationMs":6100,"usage":{"inputTokens":1800,"outputTokens":420,"totalTokens":2220,"contextWindow":1047576}}}
// run status: awaiting_input. Client approves → run(assistant message) → new segment:
{"type":"start","messageId":"0192…"}
{"type":"tool-output-available","toolCallId":"call_2","dynamic":true,"output":{"ok":true}}
{"type":"start-step"}
{"type":"text-start","id":"t1"} {"type":"text-delta","id":"t1","delta":"Saved. Here is the summary…"} {"type":"text-end","id":"t1"}
{"type":"finish-step"}
{"type":"finish","finishReason":"stop","messageMetadata":{"durationMs":9800,"usage":{"…":"…"}}}
```

Persisted result: one assistant `UIMessage` with parts `[step-start, reasoning, dynamic-tool(task,
output-available), data-task-list, data-agent(sa_1, done), step-start, dynamic-tool(write_note,
output-available, approval{approved:true}), step-start, text]` and metadata from `start` merged
with both `finish` chunks.

## Appendix B — reducer rules (port of `processUIMessageStream`, `ai@6.0.204`)

- `start`: set `message.id` if `messageId` present; merge `messageMetadata`. Parts are not reset.
- `start-step`: append `{type: "step-start"}`. `finish-step`: close all open text/reasoning ids.
- `text-start {id}`: append `{type:"text", text:"", state:"streaming"}` and remember it by id;
  `text-delta`: append to that part (error if unknown id); `text-end`: `state:"done"`, forget id.
  Same for `reasoning-*`.
- `tool-input-start`: upsert tool part by `toolCallId` (`dynamic-tool` with `toolName` when
  `dynamic`, else `tool-<name>`) with `state:"input-streaming"`; `tool-input-available`:
  `state:"input-available"`, `input`; `tool-input-error`: `state:"output-error"`, `errorText`.
- `tool-approval-request`: part → `state:"approval-requested"`, `approval:{id}` (error if the
  part does not exist).
- `tool-output-available`: `state:"output-available"`, `output` (replaced wholesale),
  `preliminary`; `tool-output-error`: `state:"output-error"`, `errorText`; `tool-output-denied`:
  `state:"output-denied"`.
- `data-*`: if `transient`, do nothing; else if a part with the same `type` and `id` exists,
  replace its `data` in place; else append `{type, id?, data}`.
- `file`, `source-url`, `source-document`: always append.
- `message-metadata` and `finish`: merge `messageMetadata`; `finish` also records `finishReason`
  (we store it in metadata).
- `error`, `abort`: no change to the message.

Upsert of tool parts matches on `toolCallId`; text and reasoning ids are only meaningful while
open, so concurrent blocks with different ids become separate parts in arrival order.

## Appendix C — adk v2 signatures used

```go
llmagent.New(llmagent.Config{Name, Description, Instruction string, Model model.LLM, Tools []tool.Tool,
    GenerateContentConfig *genai.GenerateContentConfig, DisallowTransferToPeers bool}) (agent.Agent, error)
openaimodel.NewModel(ctx, modelName string, &openaimodel.ClientConfig{APIKey, BaseURL string, HTTPClient *http.Client,
    API openaimodel.API /* APIChatCompletions | APIResponses */}) (model.LLM, error)
runner.New(runner.Config{AppName string, Agent agent.Agent, SessionService session.Service, AutoCreateSession bool}) (*runner.Runner, error)
(*runner.Runner).Run(ctx, userID, sessionID string, msg *genai.Content, agent.RunConfig{StreamingMode: agent.StreamingModeSSE}) iter.Seq2[*session.Event, error]
functiontool.New[TArgs, TResults](functiontool.Config{Name, Description string, IsLongRunning, RequireConfirmation bool},
    func(agent.Context, TArgs) (TResults, error)) (tool.Tool, error)
agent.Context: FunctionCallID(), ToolConfirmation() *toolconfirmation.ToolConfirmation, RequestConfirmation(hint string, payload any) error
toolconfirmation.FunctionCallName == "adk_request_confirmation"; toolconfirmation.OriginalCallFrom(*genai.FunctionCall) (*genai.FunctionCall, error)
session.Service: Create, Get, List, Delete, AppendEvent(ctx, session.Session, *session.Event) error; session.ErrNotFound; session.InMemoryService()
session.Event{LLMResponse{Content *genai.Content, Partial bool, UsageMetadata, FinishReason}, ID, InvocationID, Author, Branch string,
    LongRunningToolIDs []string, Actions session.EventActions{StateDelta, RequestedToolConfirmations}}
```
