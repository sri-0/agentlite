# agentlite — design and migration plan

Status: v3, 2026-10-08. Phases 0–5 are implemented and verified with curl and a socket client
(2026-10-08); see the deviations list at the end of §6. The UI port (§4) is in progress. Supersedes v1 (2026-10-07) and v2 (research revisions). v3 folds in the review
comments from `PLAN-Sr-edits.md` and switches the wire format to AG-UI.
Companion: `RESEARCH-streaming.md` (how opencode, T3 Code, Codex, ACP and Goose do this).

The brief: Go 1.27, adk-go v2, Echo, a WebSocket agent stream, OpenAI-style endpoints as pure
reverse proxies, one protocol on the wire and in the database, resumable streams, MCP, memory,
compaction, per-message sub-agent dispatch, custom workflow agents, and a swarm. Simple,
readable code that does
exactly what is needed, structured like `agentic` so the migration is easy.

---

## 0. Decisions at a glance

| Decision | Choice | Why (short) |
|---|---|---|
| Toolchain | Go **1.27.1**, module `agentlite` | adk v2.5.0 needs ≥1.26.6, opensearch-go v5 needs ≥1.26; 1.27 has `encoding/json/v2` and a stdlib `uuid`. |
| Agent framework | `google.golang.org/adk/v2` **v2.5.0** | In-tree OpenAI-compatible model, `mcptoolset`, compaction, memory service + tools, `transfer_to_agent` routing, parallel tool execution, HITL confirmation. |
| HTTP | `github.com/labstack/echo/v5` **v5.4.0** | Stable, graceful shutdown, struct context. Logs go through a zerolog-backed `slog.Handler`. |
| WebSocket | `github.com/coder/websocket` **v1.8.15** | Context-first, safe concurrent writes, already an adk dependency. |
| Wire protocol | **AG-UI 1.0 events** over one WebSocket per client (`/v1/agent/chat`), each frame tagged with `threadId` and a cursor | Vendor-neutral spec with native sub-agent, activity, reasoning and interrupt vocabulary; Go types from the AG-UI SDK; the server becomes an AG-UI server other clients can use. See §3.3 for the trade. |
| Client | `agentui` keeps its components; `useChat` and the AI SDK runtime go. A pure AG-UI reducer (`@ag-ui/core` types) folds events into the view model; a `useThread` hook over the existing zustand store exposes messages, status, send, approve, answer, stop | AI Elements and streamdown are presentational and take props; the view model keeps their part shapes as types only, so no component changes. One conversion step and no impedance mismatch with `useChat`'s one-message-per-response rule. |
| Storage format | **Compacted AG-UI events**, one OpenSearch document per message, plus extracted `text` | "Store the protocol." History folds through the same reducer as the live stream; nothing is converted on the server. |
| Live log / resume | **Valkey stream per thread** (`thread:{id}:log`), cursor = entry id, trimmed by age | Lines up with how the client thinks (thread + cursor). Works across instances. |
| Durable store | **OpenSearch** via `opensearch-go/v5`, written only through `store.Sink` from a background goroutine | Per-message documents paginate; the sink is the seam for a Kafka producer later. |
| Kafka | Not in v1 | `run.Log` (chunk stream) and `store.Sink` (persistence) are the two interfaces a Kafka implementation would satisfy. |
| LLM context | **One durable ADK session per thread** in Valkey; rebuilt from persisted events if missing | Makes adk's compaction, memory and routing stickiness work natively (§3.9). |
| Agents | One file per agent; kinds `llm \| sequential \| parallel \| loop \| graph`; the default `assistant` is the router: on every message it decides to answer, use tools, or dispatch typed sub-agents via `task` onto child threads | The Claude Code / opencode model: no classifier, no handoff; the primary agent decides per message whether the swarm is needed. Pipelines (deep research) are selectable directly or dispatchable as sub-agents. |
| Providers | Dedicated `http.Client` per provider (base transport + mTLS + static headers); model list discovered from `/v1/models` or `/model/info` and merged with YAML overrides | Some providers need custom headers; LiteLLM exposes richer model info than YAML. |
| OpenAI endpoints | `httputil.ReverseProxy` with `Rewrite`; one default upstream, optional per-path override | No body parsing; `/v1/messages` is a plain proxy too (vLLM/LiteLLM serve it). |
| Logging / config | **zerolog** (slog bridge for Echo and adk); `sethvargo/go-envconfig`; `goccy/go-yaml` | Owner preference; goccy has the best error messages and is maintained. |
| Identity | **None in v1.** No `X-User-ID`, no anonymous user. Records keep an empty `user_id` for later | Owner decision; auth is a later phase. |
| Distribution | Engine talks only to Valkey; runs are requested, executed and finalised through Valkey streams; v1 runs the worker loop in-process | Separate containers per agent later is a deployment change (§3.15). |
| Testing | **Deferred.** Checkpoints are verified with `curl` and `websocat`; development scripts are not committed | Owner decision; the tests worth writing later are listed in §7. |

---

## 1. Goals and non-goals

**Goals (v1)**

1. Pure reverse proxies for `/v1/chat/completions`, `/v1/responses`, `/v1/messages`, `/v1/embeddings`.
2. One WebSocket per client at `/v1/agent/chat`, AG-UI events, per-thread subscriptions, resumable from a cursor.
3. Every message persisted as compacted AG-UI events; thread load returns them and the client translates.
4. Agents: `chat` (plain model), `assistant` (default; all tools, dispatches `researcher`, `writer` and the pipeline through `task`), one `sequential` or `graph` pipeline (deep research shape), tools, MCP servers from YAML.
5. Native HITL: approval with justification, and a client-answered `question` tool.
6. Long-term memory (adk memory service on OpenSearch) and compaction (adk), in phase 5.
7. `agentui` talks only to this server through one base URL and one socket.

**Non-goals for v1**: RAG retrieval tools, skills manifest, suggestions, prompt templates (the UI
has no prompts screen), attachments, regenerate, office artifacts, Confluence, OAuth for MCP,
Kafka, authentication, the bubbletea CLI.

---

## 2. Research summary

Verified against module sources and installed packages on 2026-10-07/08.

**adk-go v2.5.0** (`$GOMODCACHE/google.golang.org/adk/v2@v2.5.0`)
- `llmagent.New(Config{Name, Instruction, Model, Tools, Toolsets, SubAgents, OutputKey, DisallowTransferToParent/Peers, IncludeContents, Mode})`. An LLM agent with `SubAgents` gets a `transfer_to_agent` tool (`internal/llminternal/agent_transfer.go`); this is adk's routing primitive. The runner routes a new user message to the agent that produced the session's last event, so a transfer is sticky for the thread until the agent transfers again.
- Workflow agents: `sequentialagent`, `parallelagent` (sub-agents run in goroutines, events stream live with `Branch` set), `loopagent`; graph workflows via `workflowagent.New(Config{Edges})` with `workflow.EdgeBuilder.Add/AddRoute/AddRoutes/AddFanOut/AddFanIn`. There is no dedicated router agent.
- `runner.New(Config{AppName, Agent, SessionService, MemoryService, Compaction, AutoCreateSession})`; `Run(ctx, userID, sessionID, *genai.Content, RunConfig{StreamingMode: SSE}) iter.Seq2[*session.Event, error]`. Tool calls in one model turn execute concurrently (`base_flow.go:1306`).
- `session.Service` (`Create/Get/List/Delete/AppendEvent`); in-tree: memory, GORM, Vertex; `session/sessiontestsuite` for custom ones. `session.Event` embeds `model.LLMResponse` and carries `Author`, `Branch`, `LongRunningToolIDs`, `Actions.RequestedToolConfirmations`.
- Compaction: `runner.Config.Compaction = &compaction.Config{CompactionInterval, OverlapSize, TokenThreshold}` + `compaction.NewLLMSummarizer`. Sliding window after N invocations and/or tail retention when the prompt exceeds a token threshold. Needs a durable session.
- Memory: `memory.Service{AddSessionToMemory(ctx, session.Session), SearchMemory(ctx, *SearchRequest)}`; tools `preloadmemorytool.New()` (injects relevant memories every turn) and `loadmemorytool.New()` (agent-initiated search).
- MCP: `mcptoolset.New(Config{Endpoint | Transport, Auth, RequireConfirmation, RequireConfirmationProvider})` returns a `tool.Toolset`; filter with `tool.FilterToolset`. Streamable HTTP by endpoint; stdio via `mcp.CommandTransport` from `modelcontextprotocol/go-sdk`.
- HITL: `functiontool.Config{RequireConfirmation}` or `ctx.RequestConfirmation(hint, payload)` emits `FunctionCall{Name: "adk_request_confirmation", ID: <approvalId>}` and pauses; resume by `Run` with `FunctionResponse{ID: approvalId, Name: "adk_request_confirmation", Response: {"confirmed": bool, "payload": any}}`. The tool reads `ctx.ToolConfirmation().Payload`. Long-running tools (`IsLongRunning`) returning nil pause the same way and resume with a `FunctionResponse` for their own call id.
- Model: `openaimodel.NewModel(ctx, name, &ClientConfig{APIKey, BaseURL, HTTPClient, API: APIChatCompletions | APIResponses})`. Chat Completions mode ignores reasoning deltas; Responses mode maps them to thought parts. No Anthropic model in tree.

**AG-UI 1.0** (`@ag-ui/core@1.0.2`; Go SDK `github.com/ag-ui-protocol/ag-ui/sdks/community/go` at `v0.0.0-20261007133343-903a9ab9a154`)
- Events: `RUN_STARTED/FINISHED/ERROR`, `STEP_STARTED/FINISHED{stepName}`, `TEXT_MESSAGE_START/CONTENT/END{messageId, role}`, `REASONING_START/END` + `REASONING_MESSAGE_START/CONTENT/END`, `TOOL_CALL_START/ARGS/END/RESULT{toolCallId, parentMessageId}`, `STATE_SNAPSHOT/DELTA`, `MESSAGES_SNAPSHOT`, `ACTIVITY_SNAPSHOT/DELTA{messageId, activityType, content, replace}`, `SUBAGENT_STARTED/FINISHED/ERROR{subagentRunId, name, parentToolCallId}`, `CUSTOM{name, value}`, `RAW`. `subagentRunId` is allowed on most events for attribution.
- `RUN_FINISHED.outcome` is `success | interrupt{interrupts: [{id, reason, toolCallId?, message?, responseSchema?}]} | cancelled`; `RUN_FINISHED.usage` is a list of per-model token usage. A new run with `RunAgentInput.resume: [{interruptId, status, payload}]` continues after an interrupt.
- Transports: HTTP+SSE and protobuf bindings; "implementations MAY carry AG-UI over other channels — WebSockets". No stream resumption in the spec; we add a cursor in our frame envelope.
- Go SDK: typed events with JSON (de)serialisation, `Validate`, `ValidateSequence`, an SSE writer. Community-maintained, pseudo-versioned; it still ships the retired `THINKING_*` constants beside the 1.0 `REASONING_*` ones. Pin the commit; be ready to vendor `pkg/core`.

**AI SDK `ai@6.0.204`** (installed in `agentui`): `ChatTransport{sendMessages, reconnectToStream}` returns a `ReadableStream<UIMessageChunk>`; `readUIMessageStream` folds chunks into a `UIMessage` on the client; `tool-approval-request` → `addToolApprovalResponse({id, approved, reason})`; `addToolOutput` for client-answered tools; `data-*` parts reconcile by `id`; one assistant message per response stream. `streamdown` renders markdown text and has no protocol dependency.

**Other**: Echo v5.4.0 (`func(c *echo.Context) error`, `websocket.Accept(c.Response(), c.Request(), …)` works); Go 1.27.1 (`uuid.NewV7()`, `httputil.ReverseProxy.Rewrite`); opensearch-go **v5.0.0**; valkey-go v1.0.78 (streams, consumer groups, pub/sub); zerolog v1.35.1; goccy/go-yaml v1.19.2.

---

## 3. Architecture

### 3.1 Overview

```
 browser (agentui: AG-UI reducer + useThread over zustand; AI Elements + streamdown unchanged)
   │  REST: /v1/threads, /v1/agents, /v1/models              WS: /v1/agent/chat (all threads)
   ▼
 ┌──────────────────────────── agentlite (Echo v5, one binary) ───────────────────────────┐
 │ handler/proxy ──► upstream providers (chat/completions, responses, messages, embeddings) │
 │ handler/ws   ──► run.Service: Start / Resume / Cancel / Subscribe / Wait                │
 │                    │ requests → runs:{agentId} ─► worker loop (in-process in v1)        │
 │                    │                                 │ adk runner + session (Valkey)     │
 │                    │                                 ▼                                   │
 │                    │                              encode(session.Event) → AG-UI events   │
 │                    │                                 │ XADD thread:{id}:log              │
 │                    └── Subscribe tails the thread log from a cursor ◄────────┘          │
 │                    runs:done ─► finalizer: compact(events) ─► store.Sink ─► OpenSearch   │
 │ agents/  roster (one file per agent) · kinds (llm/sequential/parallel/loop/graph)        │
 │ tools/   calculate, read_url, todowrite, emit_artifact, write_note, question, task,      │
 │          report_progress, memory (adk) · mcp/ (mcptoolset from mcp.yaml)                 │
 └──────────────────────────────────────────────────────────────────────────────────────────┘
 Valkey: thread logs, run hashes, request/done streams, ADK sessions, leases, ctl pub/sub
 OpenSearch: threads, messages (compacted events), memories
```

### 3.2 Domain model

| Entity | Lives in | Shape |
|---|---|---|
| **Thread** | OpenSearch `threads` | `{id, parent_id?, root_id, title, agent_id, model, created_at, updated_at, user_id: ""}`. Id is client-generated; the server upserts on first run. `parent_id` marks a sub-agent's thread; the thread list shows roots only. |
| **Message** | OpenSearch `messages`, one document each | `{id, thread_id, root_id, role, seq, created_at, text, events: [AG-UI event…]}`. `seq` is a per-thread integer from `INCR thread:{id}:seq`, assigned before the write; it is the sort key. `events` is the compacted event sequence of that message (user: one text message; assistant: every event of its turn with deltas merged). `text` is extracted for search and titles. |
| **Run** | Valkey hash `run:{id}` | `{id, thread_id, root_id, agent_id, model, turn_id, status, segment_start, result, started_at, updated_at, error}`; `status ∈ queued \| running \| awaiting_input \| done \| error \| cancelled`. `result` is the final text (capped at 64 KiB). |
| **Turn** | implicit | One user message and the assistant message that answers it. A turn may span several AG-UI runs when it pauses for input; `turn_id` is the assistant message id and is what the client folds into one assistant message in its view model. |
| **Event** | Valkey stream `thread:{id}:log` | One AG-UI event per entry (`e` = JSON). The entry id is the cursor. |

Ids: user messages keep the id the client generated; assistant message ids (`turn_id`) are
`uuid.NewV7()` minted at `Start`; AG-UI `messageId`s inside a turn are minted per text block.
The user message id is the idempotency key for `Start`: a repeat subscribes to the existing run.

### 3.3 The protocol

**Why AG-UI, given the earlier recommendation for the AI SDK stream.** The AI SDK format won on
one constraint: load without conversion. That constraint is now relaxed to "a lightweight
converter on the frontend is fine", and with it the balance moves: AG-UI is a versioned,
vendor-neutral spec with native sub-agent, activity, reasoning and interrupt vocabulary and
typed Go structs; storing it literally satisfies "store the protocol"; and the server becomes
something CopilotKit or any AG-UI client can talk to. The cost is one reducer, and the
design below makes sure it is exactly one.

**Where the reducer lives.** In the browser, as a pure function `reduce(state, event)` over
`@ag-ui/core` event types. It builds the view model directly: one assistant message per turn
holding an ordered parts array (text, reasoning, tool with its state, step, activity data),
plus thread status and pending interrupts. History uses the same function:
`GET /v1/threads/{id}/messages` returns each message's compacted events and the client folds
them. No Go reducer exists and no second protocol exists; the Go side encodes adk events into
AG-UI and compacts contiguous deltas for storage. `useChat` and the AI SDK runtime are not
used; the AI SDK part types are kept as types only so the existing components keep their props.

**streamdown and performance.** `streamdown` renders markdown text; it is fed by a text part
and never sees a protocol. AI Elements components take props (`text`, `state`, `input`,
`output`, `isStreaming`) and do not care how they were produced. The reducer is a switch over
event types, O(events); frames are folded as they arrive and the store is published to React
once per animation frame (the same 50 ms-class throttle `useChat` applied). History is a few
thousand reductions for a long thread, well under a frame. The only wire cost is slightly
larger frames, because AG-UI repeats `messageId` on every content event.

**Events we emit** (Go SDK types in brackets):

| Event | When |
|---|---|
| `RUN_STARTED {threadId, runId, metadata: {turnId, agentId, model, createdAt}}` | first event of every segment |
| `STEP_STARTED / STEP_FINISHED {stepName}` | around each model call of the output agent, and around each workflow step (`stepName` = agent name) |
| `TEXT_MESSAGE_START {messageId, role: assistant}` / `CONTENT {delta}` / `END` | text; one `messageId` per text block |
| `REASONING_START {messageId}` + `REASONING_MESSAGE_START/CONTENT/END` + `REASONING_END` | reasoning; `REASONING_END.timestamp - START.timestamp` gives the duration |
| `TOOL_CALL_START {toolCallId, toolCallName, parentMessageId}` / `ARGS {delta: full JSON}` / `END` | tool call (adk gives whole args) |
| `TOOL_CALL_RESULT {toolCallId, messageId, content}` | tool result |
| `ACTIVITY_SNAPSHOT {messageId, activityType, content, replace: true}` | `task_list` (messageId `tasks`), `artifact` (messageId = artifact id), `agent` (messageId = subagentRunId), `step` (messageId = step id); reconciled by id |
| `SUBAGENT_STARTED / FINISHED / ERROR {subagentRunId, name, parentToolCallId}` | task-tool children; the `agent` activity carries progress and the final text |
| `RUN_FINISHED {outcome: success \| interrupt{…} \| cancelled, usage, metadata: {durationMs}}` | last event of a segment |
| `RUN_ERROR {message, code}` | run failed; replaces `RUN_FINISHED` |

Interrupts: an approval pauses the run with `RUN_FINISHED{outcome: interrupt, interrupts:
[{id: <approvalId>, reason: "confirmation", toolCallId, message: <hint>}]}`; a client-answered
tool with `reason: "input_required"` and `responseSchema`. The resume is a `run` frame carrying
`resume: [{interruptId, status: "resolved", payload}]` (§3.11).

**Reducer** (Appendix B has the full table): `RUN_STARTED` opens or continues the turn named by
`metadata.turnId` and sets status `streaming`; `STEP_*` appends a step marker; `TEXT_MESSAGE_*`
and `REASONING_MESSAGE_*` open, append to and close a text or reasoning part keyed by
`messageId` (reasoning gets `durationMs` from the timestamps); `TOOL_CALL_START/ARGS/END` and
`TOOL_CALL_RESULT` drive a tool part keyed by `toolCallId` through `input-streaming →
input-available → output-available | output-error`; `ACTIVITY_SNAPSHOT` upserts a data part
keyed by `(activityType, messageId)`; `SUBAGENT_*` updates the agent card's status;
`RUN_FINISHED` sets status `ready`, or `awaiting_input` with each interrupt recorded on its tool
part (`approval-requested`), and merges usage; `RUN_ERROR` sets status `error` with the message.
Unknown event types are ignored and the cursor still advances.

Conventions the reducer relies on: one `RUN_STARTED` per segment with the same `turnId` across
segments of a turn; text and reasoning ids are AG-UI `messageId`s; tool ids are `toolCallId`s;
activity ids are stable per thing; sub-agent children never appear as events on the parent
beyond `SUBAGENT_*` and the `agent` activity.

### 3.4 The WebSocket transport

One socket per client: `GET /v1/agent/chat?v=1` (upgrade; `426` on an unknown version). It
multiplexes any number of thread subscriptions; the client subscribes and unsubscribes as
threads open and close in the UI.

Client → server frames:

```jsonc
{ "type": "run",         "threadId": "th_1", "message": { "id": "m_9", "role": "user", "content": "…" },
                         "agentId": "assistant", "model": "openai/gpt-4.1", "reasoningEffort": "medium" }
{ "type": "run",         "threadId": "th_1", "resume": [{ "interruptId": "adk_c7", "status": "resolved", "payload": { "approved": true, "justification": "looks right" } }] }
{ "type": "subscribe",   "threadId": "th_1", "after": "1759840000000-0" }     // after is optional
{ "type": "unsubscribe", "threadId": "th_1" }
{ "type": "cancel",      "threadId": "th_1" }
```

Server → client frames:

```jsonc
{ "type": "event", "threadId": "th_1", "seq": "1759840000012-0", "event": { "type": "TEXT_MESSAGE_CONTENT", "messageId": "…", "delta": "Hel" } }
{ "type": "idle",  "threadId": "th_1" }                                         // subscribe on a thread with no active run
{ "type": "error", "threadId": "th_1", "code": "busy | not_found | bad_request", "message": "…" }
```

Semantics:
- `run` with a `message` starts a turn (or, if that message id already exists, subscribes to
  the run it started). `run` with `resume` continues the paused run (§3.11). `busy` while a run
  is `running`.
- After `run` the server streams the new segment from its first entry.
- Opening a thread in the UI is: `GET /v1/threads/{id}/messages` (history + `cursor`), then
  `subscribe{after: cursor}`. The `cursor` is the log id of the last event that is already
  inside the returned history, so the stream delivers exactly what is newer. If the gap is
  large (a reload mid-answer), the server may substitute a compacted replay of the in-flight
  segment for the raw one; same events, fewer frames.
- A subscription ends at the segment's `RUN_FINISHED`/`RUN_ERROR` event; the socket and the
  other subscriptions stay.
- `cancel` cancels the run on whichever instance runs it (Valkey pub/sub `run:{id}:ctl`).

Client (sketch; `lib/chat/`):

```ts
// socket.ts     one module-level socket with reconnect; per-thread cursor; frames routed to the store
// reduce.ts     export function reduce(thread: ThreadState, event: AgUiEvent): ThreadState   // pure
// store.ts      zustand: threads[threadId] = { messages, status, interrupts, cursor }; published once per animation frame
// use-thread.ts
export function useThread(threadId) {
  const t = useStore(s => s.threads[threadId]);
  return {
    messages: t.messages, status: t.status, interrupts: t.interrupts,
    send:    (text)              => socket.send({ type: 'run', threadId, message: userMessage(text), ...selection() }),
    approve: (id, approved, why) => socket.send({ type: 'run', threadId, resume: [{ interruptId: id, status: 'resolved', payload: { approved, justification: why } }] }),
    answer:  (id, output)        => socket.send({ type: 'run', threadId, resume: [{ interruptId: id, status: 'resolved', payload: output }] }),
    stop:    ()                  => socket.send({ type: 'cancel', threadId }),
  };
}
// open thread: fetch history → fold each message's events with reduce → subscribe{after: cursor}
```

Sequences:

```
open thread         : GET messages → {data, cursor} ─► reduce(history) ─► subscribe{after: cursor} ─► idle | events
new turn            : run(message) ──► RUN_STARTED … RUN_FINISHED                 (status: streaming → ready)
mid-stream drop     : socket closes ─► reconnect ─► subscribe{after: last seq} for every open thread ─► remaining events
approval            : approve(id, true, why) ─► run(resume) ─► RUN_STARTED{turnId same} TOOL_CALL_RESULT … RUN_FINISHED
sub-agent detail    : subscribe{threadId: childThreadId} on the same socket ─► the child's events, same components
```

### 3.5 Run lifecycle

```
Start(thread, message, agent, model)                              // control plane
  ├─ reject busy (one run per thread); if message id exists → return that run
  ├─ upsert thread; seq := INCR thread:{id}:seq; Sink.SaveMessage(user message as events)
  ├─ ensure the thread's ADK session exists in Valkey (rebuild from persisted events if missing, §3.9)
  ├─ create run hash {status: queued, turn_id}, set thread:{id}:run = runId (lease), SADD runs:active
  └─ XADD runs:{agentId} {runId, message}

worker loop (in-process in v1; a separate container later; same code)
  XREADGROUP runs:{agentId} → load run hash; status := running
  for event in engine.Run(run, message | resume):                 // encoder output
      seq := log.Append(threadID, event)                          // XADD thread:{id}:log; first seq of the segment → segment_start
      refresh lease every 20 s
  write status + result to the run hash; XACK; XADD runs:done {runId, status}; PUBLISH run:{id}:done

finalizer (control plane; consumer group on runs:done; same process in v1)
  events := XRANGE thread:{id}:log from the turn's first segment_start      // never from worker memory
  Sink.SaveMessage({id: turn_id, seq, text: extract(events), events: compact(events)}); XACK
  if terminal: DEL thread:{id}:run, SREM runs:active, XTRIM thread:{id}:log MINID <now - retention>

Resume(thread, resume entries)   → status must be awaiting_input → XADD runs:{agentId} {runId, resume}
Cancel(thread)                   → PUBLISH run:{id}:ctl cancel → worker cancels ctx → RUN_FINISHED{outcome: cancelled}
Subscribe(thread, after)         → active run or idle → XREAD from after (or from segment_start) until a terminal event
Wait(runID)                      → SUBSCRIBE run:{id}:done, then check the hash → status + result   (used by the task tool)
```

Valkey keys:

| Key | Type | Content | Lifetime |
|---|---|---|---|
| `thread:{id}:log` | stream | AG-UI events for every run of the thread, newest runs at the tail | trimmed by age after each finalise (`XTRIM MINID`, 24 h); key expires 7 days after the last append |
| `thread:{id}:seq` | string | per-thread message sequence | with the thread |
| `thread:{id}:run` | string | active run id | lease: TTL 60 s refreshed while `running`; `PERSIST` while `awaiting_input`; deleted at terminal |
| `runs:{agentId}` | stream + group `workers` | run requests; `XAUTOCLAIM` reclaims from crashed workers, which are then marked `error`, never re-run | trimmed on `XACK` |
| `runs:done` | stream + group `finalizers` | `{runId, status}` at each pause or terminal; at-least-once, finalisation is idempotent (upsert by message id) | trimmed on `XACK` |
| `runs:active` | set | active run ids, for the thread list's status column | members removed at terminal |
| `run:{id}` | hash | run record incl. `result` | 7 days |
| `run:{id}:ctl`, `run:{id}:done` | pub/sub | cancel; completion wake-up | — |
| `session:{threadId}` (+`:events`, `:state`) | hash/list/hash | the thread's ADK session (§3.9) | 30 days after last use |

During a run the log is the source of truth; at finalise the persisted message is. Orphan rule:
a run whose hash says `running` but whose lease is gone was executing on a dead process; the
first subscriber or finaliser that notices marks it `error`, appends `RUN_ERROR`, and persists
the partial turn.

Backpressure: one worker per run writes the log; each subscriber reads with its own `XREAD`
and writes its socket synchronously, so a slow client slows only itself and the producer
never buffers per subscriber.

### 3.6 Kafka

Not in v1. Two interfaces are the seams, and both exist from day one:

- `run.Log{Append, Read(after)}`: the chunk stream. A Kafka implementation keys by `root_id`
  (a parent and its children stay ordered in one partition) and maps the cursor to a partition
  offset; `Read` becomes a consumer that seeks to the offset. Live fan-out then means each API
  node consumes the topic and forwards to its subscribers.
- `store.Sink{SaveThread, SaveMessage}`: persistence. A Kafka implementation produces the same
  documents keyed by id; the Aiven OpenSearch Connect sink (v4.1.0, upsert) lands them in the
  same indices; reads stay on OpenSearch.

Swapping either is a configuration choice and touches no caller.

### 3.7 Persistence

**One document per message, written in the background.** The finaliser writes each assistant
message when its turn pauses or ends, and `Start` writes the user message; both go through
`store.Sink` from goroutines outside the request path. Nothing in the run waits on OpenSearch.

Indices (created on boot if missing):

```
threads  : id, parent_id, root_id (keyword), title (text), agent_id, model (keyword), created_at, updated_at (date), user_id (keyword, empty)
messages : id, thread_id, root_id (keyword), role (keyword), seq (integer), created_at (date),
           text (text), events (object, enabled: false)
memories : id, text (text), embedding (knn_vector 1024, cosine), created_at, user_id (keyword, empty)
```

`enabled: false` on `events` means OpenSearch keeps the JSON in `_source` and returns it
verbatim but builds no inverted index over its nested fields. Each message is still its own
document; the per-thread document alternative was rejected because every update would rewrite
the whole thread and long threads could not be paginated.

Why not one document per thread with all messages inside: whole-document rewrites on every
turn, no pagination, and no per-message Kafka key. Per-message documents with `seq` give both.

Reads: thread list where `parent_id` is unset, sorted by `updated_at desc`, with `run_status`
joined from `runs:active`; messages by `thread_id` sorted by `seq`, paginated
(`?before=<seq>&limit=50`) and returned with `cursor` (§3.4). The history response is
`{data: [{id, role, seq, events}], cursor}`; the client translates.

```go
// internal/store
type Sink interface {
    SaveThread(ctx context.Context, t *Thread) error     // upsert by id
    SaveMessage(ctx context.Context, m *Message) error   // upsert by id; m.Seq already assigned
}
```

Coalescing for storage (`stream.Coalesce`): merge contiguous `TEXT_MESSAGE_CONTENT` for the
same `messageId` into one, likewise `REASONING_MESSAGE_CONTENT` and `TOOL_CALL_ARGS`; keep every
other event. Only contiguous deltas within a start/end pair are merged, so the result is a
valid event sequence (`ValidateSequence` is run on it in development) and the client's
reducer does not know the difference.

### 3.8 Agents

**Roster: one file per agent.** `config/default/agents/<id>.md`, YAML frontmatter + body as
instruction. Agents reference each other by id; nothing is nested.

```yaml
---
id: assistant
name: Assistant
description: Default agent. Answers directly, uses tools, or dispatches sub-agents when a task needs them.
kind: llm                     # llm | sequential | parallel | loop | graph
mode: primary                 # primary | subagent | all
model: openai/gpt-4.1
# tools omitted ⇒ every registered tool and MCP tool; tools: [] ⇒ none
subagents: [researcher, writer, deep-research]   # ids the task tool may dispatch (llm) / steps (sequential, parallel, loop) / nodes (graph)
---
You are the user's assistant. <available_subagents> is appended by the builder.
```

Kinds map to adk: `llm` → `llmagent`; `sequential`/`parallel`/`loop` → the workflow agents over
the listed ids (`loop` takes `max_iterations`); `graph` → `workflowagent` with `edges:` in the
frontmatter (`from: a, to: b, route?: <state key>`), which is how a deep-research pipeline with a
critic loop is declared. There is no separate router. The default agent, `assistant`, is an `llm` agent with the full
tool set and `subagents: [researcher, writer, deep-research]`: on every message the model
decides whether to answer, use tools, or dispatch children through `task`. This is how Claude
Code and opencode work (one primary agent with a task tool and a typed roster; no classifier,
no handoff); it costs nothing when the swarm is not needed, and the thread's context stays in
one place. The picker still lets a user choose a specific agent (`chat` for a plain model, a
pipeline directly). adk's `transfer_to_agent` remains available to agents that declare
`subagents` without the `task` tool, but nothing in v1 uses it.

Agents are built per run from the definition and the request's model, which every step, the
router's classifier, the compaction summarizer and child runs use; agents never name a model.
There is no global registry of built agents.

**In-run sub-agents (workflow kinds).** Steps of a sequential, parallel, loop or graph agent run
inside the one run and session. The encoder wraps each adk sub-agent's events in `STEP_STARTED
/ STEP_FINISHED {stepName: <agent id>}` (adk sets `Author` on every event; steps are tracked
per author, so a parallel agent's interleaved sub-agents each get their own step) and the UI
groups a step's text, reasoning and tools by those markers. Nothing is hidden and nothing is
duplicated.

**Dynamic sub-agents (swarm): a child thread per sub-agent.** The `task` tool
(`task(subagent_type, description, prompt)`):

1. validates `subagent_type` against the parent's `subagents`; any kind can be dispatched
   (`researcher` is an `llm` agent, `deep-research` a `graph` pipeline);
2. creates a thread with `parent_id` (and `root_id`) and starts an ordinary run on it through
   `run.Service.Start` with the child's definition and the prompt as the user message;
3. emits `SUBAGENT_STARTED {subagentRunId, name, parentToolCallId}` and an `agent` activity
   `{subagentRunId, childThreadId, name, status: running, progress, log}` plus a `task_list` row;
4. tails the child's log read-only while it waits: each child `TOOL_CALL_START` becomes a
   progress line (`report_progress` calls contribute their `message` verbatim, other tools
   "<tool>: <short input>"), and each line re-emits the `agent` activity with the last ten lines;
5. on the child's terminal status (`Wait`) emits `SUBAGENT_FINISHED` (or `ERROR`), the final
   `agent` activity with `text`, and the `task_list` update, and returns the child's `result`.

The tool never copies child events into the parent's log. A client that wants the child's
live detail subscribes to the child thread on the same socket; afterwards it loads the child
thread like any other. adk runs all `task` calls of a model turn concurrently, so parallelism is
the model's choice; a semaphore caps concurrent children (4); children do not get `task`
(no nesting) nor HITL tools in v1; cancelling the parent cancels its children.

### 3.9 Context management

What each agent sees, where it comes from, and how it is kept bounded.

**One durable ADK session per thread.** `session:{threadId}` in Valkey (a `session.Service`
implementation: hash for metadata, list of JSON events, hash for state; conformance-tested with
`sessiontestsuite`). adk reads the session to build every model request and appends every
non-partial event to it, so the LLM-visible history is adk's own, and adk's compaction, memory
and routing stickiness work unchanged. The persisted AG-UI events in OpenSearch are the durable
record; if a session is missing (TTL, Valkey restore), `run.Service` rebuilds it from them
(`agents/seed.go`: user text → user content; assistant text → model content; tool call + result
→ function call + response; reasoning and activities dropped) before the run starts.

**Root agent.** Sees the thread's session (all prior turns, compacted as below), its
instruction and its tools; from phase 5, memories preloaded for the turn. It decides per
message whether to dispatch children; children never share this session.

**Workflow steps** (sequential/parallel/loop/graph). Share the run's session, so a later step
sees an earlier step's output in the conversation. This is adk's native data flow and needs
nothing from us.

**Swarm children** (task tool). A child has its own thread and session and sees only the
prompt the parent wrote for it, its instruction and its tools. The parent sees the
child's `result` as the tool result. Children see no parent history (decided 2026-10-08); if a
task needs context, the parent puts it in the prompt and its instruction says so. Parent and
child contexts therefore never bloat each other.

**Compaction.** adk's, configured per agent in the roster:

```yaml
compaction: { interval: 10, overlap: 1, token_threshold: 0.75 }   # interval in user turns; threshold as a fraction of context_length
```

`interval` triggers the sliding-window summary after N user turns; `token_threshold` (resolved
against the model's `context_length` from `models.yaml`) triggers tail-retention compaction
before a model call. The summariser uses the smallest configured model. Summaries live in the
session as events; the UI is unaffected because the UI reads persisted messages, not the
session.

**Memory (phase 5; off in phases 2–4).** adk's `memory.Service` implemented on OpenSearch
(`memories` index, kNN with text fallback; a port of `agentic/pkg/memory`). Two tools:
`preloadmemorytool` runs every turn and injects relevant memories into the instruction;
`loadmemorytool` lets the agent search on demand. Both, and the `AddSessionToMemory` post-run
hook, apply to primary agents only; swarm children stay stateless. Explicit
`add_memory`/`delete_memory` tools stay as our own function tools.

**Budgets.** `RUN_FINISHED.usage` carries per-model token usage; the UI shows it against
`context_length`. Tool outputs are capped by each tool (32 KiB for `read_url`).

Decided 2026-10-08: children receive only the prompt the parent writes; there is no router,
the assistant decides per message; memory preload is off until phase 5 and then applies to
primary agents only. Compaction defaults (10 turns, 75%) are configuration and can change.

### 3.10 Tools and MCP

| Tool | Kind | Purpose |
|---|---|---|
| `calculate` | function | deterministic demo tool |
| `read_url` | function | fetch a URL and return readable text; capped at 32 KiB |
| `todowrite` | function | task list → `task_list` activity |
| `emit_artifact` | function | → `artifact` activity |
| `write_note` | function, `RequireConfirmation: true` | the approval demo; reads `justification` from the confirmation payload |
| `question` | long-running | structured questions answered by the user; schema from `agentic/internal/tools/question.go` |
| `task` | function, agents that declare `subagents` | child-thread dispatch (§3.8) |
| `report_progress` | function, sub-agents only | no side effect; its call is what the parent lifts into the agent card |
| `load_memory`, `preload_memory` | adk | §3.9 |
| `add_memory`, `delete_memory` | function | explicit memory writes |

Tool chunks carry `title` and a `kind` (`read | fetch | execute | write | other`) in
`TOOL_CALL_START` metadata so the UI picks an icon without parsing names.

**MCP.** `config/default/mcp.yaml`:

```yaml
servers:
  office:
    endpoint: http://localhost:8090/mcp          # streamable HTTP; or command: [python, server.py] for stdio
    headers: { Authorization: "Bearer ${OFFICE_TOKEN}" }
    require_confirmation: [create_pptx]           # or true for all
    tools: [create_pptx, render_report_docx]      # omitted ⇒ all tools the server offers
```

Each server becomes one `mcptoolset.New(Config{Endpoint | Transport, RequireConfirmationProvider})`
with a dedicated `http.Client` (headers applied by a RoundTripper over the base transport),
filtered with `tool.FilterToolset` when `tools` is set. Tool names are exposed as
`<server>_<tool>` so agents can list them in `tools:`. A server that is down at build time is
logged at `error` and contributes no tools; the run proceeds.

### 3.11 Human-in-the-loop

adk's native mechanism, end to end, with state in Valkey and no side table.

**Approval.** A tool with `RequireConfirmation` (or an MCP server flagged in `mcp.yaml`) makes
adk emit `adk_request_confirmation{ID: approvalId, originalFunctionCall, hint}` and end the
invocation. The encoder emits the original call as `TOOL_CALL_START/ARGS/END`, then
`RUN_FINISHED{outcome: interrupt, interrupts: [{id: approvalId, reason: "confirmation",
toolCallId, message: hint}]}`. The run is `awaiting_input`; the lease is persisted; the ADK
session (which holds the pending confirmation) is in Valkey. The reducer marks the tool part
`approval-requested` with the interrupt id and message, so the UI shows the approval card.

The user approves or denies with an optional justification: `approve(id, approved, why)` from
`useThread` sends `run{resume: [{interruptId: id, status: "resolved", payload: {approved,
justification: why}}]}` → the control plane enqueues the resume → the worker calls `Run` with
`FunctionResponse{ID: approvalId, Name: "adk_request_confirmation", Response: {"confirmed":
approved, "payload": {"justification": …}}}`. The tool reads
`ctx.ToolConfirmation().Payload["justification"]`. The resumed segment starts with
`RUN_STARTED{metadata.turnId same}` and either `TOOL_CALL_RESULT` or, on denial, a
`TOOL_CALL_RESULT` carrying the error and a `CUSTOM{name: "approval", value: {toolCallId,
approved, justification}}` that the reducer turns into `output-denied` on the tool part plus
the recorded decision, so it is visible after reload.

**Questions.** `question` is a long-running tool: adk pauses after the call; the encoder emits
`RUN_FINISHED{outcome: interrupt, interrupts: [{id: toolCallId, reason: "input_required",
toolCallId, responseSchema}]}`. The question card calls `answer(toolCallId, {answers, text})`,
which sends the same `resume` shape; the worker resumes with `FunctionResponse{ID: toolCallId,
Name: "question", Response: output}`.

**Properties.** Other tabs see the resolution because the resumed segment is on the shared
thread log. A restart between request and answer is fine because the session, run hash and
lease are all in Valkey. A pending approval survives for the run record's lifetime (7 days);
`cancel` resolves it as cancelled. Children do not get HITL tools in v1.

### 3.12 Providers, model discovery, proxy

```yaml
providers:
  - id: litellm
    base_url: https://llm.internal/v1
    api_key_env: LITELLM_API_KEY
    headers: { X-Team: ${TEAM_ID} }              # static headers, ${ENV} expanded, applied per provider
    ssl_client_cert_env: …                       # optional mTLS; env var names, never values
    ssl_client_key_env: …
    ssl_trust_store_env: …
    discover: { endpoint: /model/info, refresh: 10m }   # or /v1/models; optional
    models:                                      # optional overrides, merged by id over discovered entries
      - { id: openai/gpt-4.1, context_length: 1047576, capabilities: [tools, reasoning] }
proxy:
  provider: litellm                              # upstream for all pass-through routes
  overrides: { messages: anthropic }             # optional per-route override
```

Each provider gets a dedicated `*http.Client`: the base transport (timeouts, HTTP/2,
`DisableCompression` for streams) wrapped with mTLS when configured and with a RoundTripper that
adds the static headers. The same client is given to `openaimodel.ClientConfig.HTTPClient`, the
reverse proxy and any MCP server on that host. Headers are never stubbed; a provider without
headers simply has an empty map.

Model discovery: when `discover` is set, the provider's model list is fetched at boot and every
`refresh` from `/v1/models` (OpenAI shape) or `/model/info` (LiteLLM shape, which carries
`max_input_tokens`, `supports_function_calling`, `supports_reasoning`, pricing). YAML `models`
entries are merged over discovered ones by id: only the fields present in YAML win, the rest
are inherited. `GET /v1/models` serves the merged list.

Proxy: `httputil.ReverseProxy{Rewrite: setURL + replace Authorization + add provider headers,
Transport: provider client transport, FlushInterval: -1, ErrorHandler: ignore
context.Canceled}` per route. Bodies are never read; `/v1/messages` is the same pass-through.
The `proxy` block exists only to say which upstream answers the pass-through routes; nothing
else is configured there.

### 3.13 Identity, CORS, timeouts

- No authentication or identity in v1. Records carry `user_id: ""` so adding scoping later is
  a filter, not a migration. There is no anonymous user and no `X-User-ID` header.
- CORS via Echo middleware with origins from `ALLOWED_ORIGINS`; the same list feeds
  `websocket.AcceptOptions.OriginPatterns`.
- `WriteTimeout = 0` (sockets and proxied SSE are long-lived); `ReadHeaderTimeout = 10s`; proxy
  `ResponseHeaderTimeout = 60s`.

### 3.14 Repository layout and key interfaces

Structured like `agentic` so the migration reads as a move, not a rewrite. Old → new:

| `agentic` | `agentlite` | Notes |
|---|---|---|
| `cmd/server` | `cmd/server` | one binary; worker loop and finalizer run inside it in v1 |
| `cmd/dev`, `cmd/cli` | later: `cmd/dev` mounts adk's `adkrest`; `cmd/cli` is a bubbletea client | phase 6 |
| `internal/config` | `internal/config` | env (go-envconfig), `models.yaml`, `mcp.yaml`, agents loader; one `Config` |
| `internal/server` | `internal/server` | Echo router, middleware, zerolog request log |
| `internal/handler` | `internal/handler` | `proxy.go`, `ws.go`, `threads.go`, `agents.go`, `models.go` |
| `internal/agent` (Core, coordinator, stream loop) | `internal/agent` | `run.go`, `service.go`, `worker.go`, `finalizer.go`, `log.go`, `valkey.go` (the run lifecycle) |
| `internal/stream` (encoders) | `internal/stream` | `encode.go` (adk event → AG-UI event), `coalesce.go`, `events.go` (our conventions over the AG-UI SDK types) |
| `agents/` (implementations), `internal/roster` | `agents/` | `roster.go` (definitions from files), `build.go` (kinds → adk agents), `seed.go` (events → session), `session_valkey.go` |
| `internal/tools` | `internal/tools` | one file per tool; `task.go`, `question.go`, `report_progress.go` |
| `internal/mcp` | `internal/mcp` | `mcp.yaml` → `mcptoolset`s |
| `internal/proxy` | `internal/proxy` | provider clients and reverse proxies |
| `internal/hitl` | removed | state lives in the ADK session and the run hash |
| `pkg/db/opensearch`, `pkg/db/valkey` | `pkg/db/opensearch` (client v5 + `Sink` + reads), `pkg/db/valkey` | |
| `pkg/memory` | `pkg/memory` | adk `memory.Service` on OpenSearch |
| `pkg/session/valkey` | `agents/session_valkey.go` | rewritten, conformance-tested later |
| `pkg/genai/*` | removed | adk `openaimodel`; `internal/proxy/provider.go` builds the per-provider clients |
| `config/default` | `config/default` | `models.yaml`, `mcp.yaml`, `agents/*.md` |

Seams (the only interfaces; everything else is concrete):

```go
// internal/stream
type Event = events.Event                           // AG-UI Go SDK
func Encode(ev *session.Event, ctx EncodeContext) []Event   // adk → AG-UI; EncodeContext carries turnId, output agent, step state
func Coalesce(evs []Event) []Event

// internal/agent
type Log interface {
    Append(ctx context.Context, threadID string, e stream.Event) (seq string, err error)
    Read(ctx context.Context, threadID, after string) iter.Seq2[Entry, error]   // replay, then block-tail until a terminal event
}
type Service struct { /* log Log; runs (valkey hashes); sink store.Sink; sessions session.Service; builder *agents.Builder */ }
func (s *Service) Start(ctx, StartInput) (*Run, error)
func (s *Service) Resume(ctx, threadID string, entries []ResumeEntry) (*Run, error)
func (s *Service) Cancel(ctx, threadID string) error
func (s *Service) Subscribe(ctx, threadID, after string) (iter.Seq2[Entry, error], error)   // nil, nil when idle
func (s *Service) Wait(ctx, runID string) (*Run, error)

// internal/store
type Sink interface { SaveThread(ctx, *Thread) error; SaveMessage(ctx, *Message) error }

// agents — imports adk, stream, agent (record types); never store or handler
func (b *Builder) Build(def Definition, modelOverride string) (adkagent.Agent, error)
func (e *Engine) Run(ctx context.Context, in Input, emit func(stream.Event)) (agent.Status, string /* result */, error)
```

Coding rules (the reasons the old code got messy):

1. One wire format, one encoder, one compactor; the only reducer is in the browser.
2. Named constants for cross-file strings (activity types, status values, stream keys).
3. No fail-open: a degraded path logs at `warn` and, where the client can act, sends an
   `error` frame. Never swallow errors into defaults.
4. All configuration through `config.Config`; no `os.Getenv` elsewhere.
5. Use adk directly; do not wrap it. Build agents per run.
6. Packages are nouns with one responsibility. No `shared`, `util`, `common`.
7. The engine and tools read and write Valkey only (session, log, run record). They never
   import `store`.
8. Development scripts and curl collections stay out of the repository.

### 3.15 Running agents in separate containers (future, designed for now)

Topology when it happens: API nodes (Echo, proxies, socket, `Start`, `Subscribe`, finalizer),
worker pools consuming `runs:{agentId}` (one deployment per agent id or group, with that
agent's secrets), Valkey as the only shared dependency (streams, hashes, sessions, leases,
pub/sub), OpenSearch written only through `store.Sink` on API nodes.

Contracts v1 fixes so the split is a deployment change:

1. The engine talks only to Valkey (rule 7). Given a run id it has the record, the session and
   the request.
2. Everything crosses the seam as data: `runs:{agentId}` in, `thread:{id}:log` and the run hash
   out, `runs:done` to finalise. No Go channel or shared memory between `agent.Service` and the
   engine. v1 runs the worker loop in-process for every agent id; v2 runs the same loop in a
   container for a subset.
3. Sessions live in Valkey and are rebuilt by the control plane when missing; workers never
   touch OpenSearch.
4. Persistence folds from the log, never from worker memory.
5. The `task` tool is a client of `agent.Service` and `agent.Log`: start a child, tail its log
   read-only for progress, `Wait`, read `result`. It never copies child events.
6. Runs execute at most once; a request reclaimed after a dead lease is marked `error`.
7. Ids are opaque; lineage (`parent_id`, `root_id`) is data; `root_id` is the Kafka key.

Forbidden in v1: the engine importing `store`; in-memory sessions; in-memory sinks between
parent and child; folding from memory; HITL state outside Valkey; the socket handler calling
the engine directly. Agents in another language attach later as adk remote agents over A2A
(`agent/remoteagent/v2`), an orthogonal path.

---

## 4. UI changes (`agentui`)

The components (`components/messages/*`, `right-panel/*`, `composer/*`, `ai-elements/*`), the
zustand UI store and the TanStack hooks for threads, agents and models stay.

1. **One base URL** (`NEXT_PUBLIC_API_URL`); drop the second streaming URL and the
   `X-User-ID` header everywhere.
2. **`lib/chat/socket.ts`**: one module-level socket to `/v1/agent/chat?v=1` with reconnect, a
   map of thread subscriptions and a per-thread cursor; frames are routed to the store.
3. **`lib/chat/reduce.ts`**: the pure reducer (Appendix B) over `@ag-ui/core` event types,
   producing the view model `{messages, status, interrupts}`. The same function folds history.
4. **`lib/chat/store.ts` + `use-thread.ts`**: thread state in zustand (published once per
   animation frame) and the hook exposing `messages, status, interrupts, send, approve, answer,
   stop` (§3.4). This replaces `useChat`; `ai` and `@ai-sdk/react` leave the runtime and `ai`
   stays as a types-only dependency for the part shapes the components already use (or those
   types are copied into `lib/chat/types.ts`).
5. **Delete** `use-agent-chat.ts`, `use-session-reconnect.ts`, `use-interrupt-resolver.ts`,
   `interrupt-context.ts`, `sse-to-chunks.ts`, `from-history.ts`, the `{data, live}` branching
   and delayed refetch in `lib/api/threads.ts`, `lib/api/sessions.ts`, and the `liveHeadSeq`,
   `stopped`, `cancelSession` plumbing in `thread-view.tsx`.
6. **Approvals and questions**: `ToolCard` calls `approve(interruptId, approved, justification)`
   with a justification field; `QuestionCard` reads the tool part's `input.questions` and calls
   `answer(toolCallId, output)`.
7. **Agent cards** read the `agent` data part (status, progress, log, text); the panel calls
   `useThread(childThreadId)` for the child's live detail (same components, no composer).
   **Steps** (`step` data parts) group the parts of workflow sub-agents into collapsible cards;
   the output agent's step is the main answer.
8. **Usage** from the assistant message's metadata; the sidebar reads `run_status` from
   `GET /v1/threads`.

---

## 5. What to keep, port, or drop from `agentic`

| Area | Today | v1 | Notes |
|---|---|---|---|
| AI-SDK encoder | clean | **reference only**; replaced by `stream.Encode` to AG-UI | the part shapes now live in the browser reducer's view model |
| OpenAI/ag_ui encoder, `internal/sse`, non-stream paths | unused | **drop** | |
| Event log, pump, projector | right idea, 3 copies of shapes | **redesign** into `agent.Log` + `stream.Coalesce` | |
| Run coordinator | in-process maps | **redesign** as `agent.Service` on Valkey | second turn while busy → `busy` |
| HITL store, resume handler | fragile | **replace** with adk-native + `resume` frame (§3.11) | |
| Chat handler (routing, RAG, templates, memory injection) | messy | **redesign**; routing is the assistant's per-message dispatch; memory via adk tools; RAG and templates later | |
| Roster, markdown defs, permissions | clean | **port**; one file per agent; `tools` omitted ⇒ all | |
| `task` tool + child stream translation | salvageable | **redesign**: children are threads run by `agent.Service` | |
| `question` tool | clean | **port** as a long-running tool | |
| `todowrite`, `emit_artifact`, task clamping | clean | **port** | |
| Demo/stub tools, triage, basic | stubs | **drop** | |
| `pkg/genai/openai`, `pkg/genai/anthropic`, Anthropic `/v1/messages` adapter | clean, now unnecessary | **drop**; `/v1/messages` is a proxy | keep `pkg/genai/openai` only if the reasoning spike (§8) fails |
| Valkey ADK session service | messy | **rewrite** as the per-thread session | |
| OpenSearch client, indices | clean | **port** on opensearch-go v5 | |
| Memory (`pkg/memory`) | clean | **port** as adk `memory.Service` + adk tools | v1 |
| Compaction | dead code | **replace** with adk compaction | v1 |
| MCP manager, resilient toolset | salvageable | **replace** with adk `mcptoolset` from `mcp.yaml` | v1; office artifact decorator and OAuth later |
| Prompts, skills, suggestions, RAG | mixed | **later** | the UI has no prompts screen |
| Sessions API | clean | **fold** into `GET /v1/threads` `run_status` + socket `cancel` | |
| Threads/messages handlers | ungated | **rewrite** small; no gating in v1 | |
| `cmd/dev`, `cmd/cli` | useful | **later**: `adkrest` mount and a bubbletea CLI | |
| Config | clean, strays | **port** the provider schema; add headers, discovery, compaction | |
| Tests | mixed | **later** (§7) | |

---

## 6. Delivery phases

Each phase ends with a checkpoint demonstrated with `curl`/`websocat`, not just compiled.

**Phase 0 — Scaffold.** `go.mod` (`go 1.27.1`), `cmd/server`, config (env + YAML), zerolog with
the slog bridge, Echo with request-id/recover/CORS/request-log, `/health`, `Makefile`
(`run`, `vet`), `docker-compose.yaml` (Valkey, OpenSearch), `AGENTS.md`, `.env.example`.
Checkpoint: `make run` serves `/health`.

**Phase 1 — Providers and proxy.** `models.yaml` loader, per-provider clients (mTLS, headers),
model discovery and merge, reverse proxies for the four routes, `GET /v1/models`.
Checkpoint: a streamed chat completion and an Anthropic `/v1/messages` call through the proxy
arrive unbuffered with the upstream key substituted.

**Phase 2 — Stream, runs, single agent, socket, persistence.** `stream` (encode, compact,
conventions), `agent` (service, Valkey log, request/done streams, worker loop, finalizer, lease,
orphan rule, idempotent start, cursor replay), `agents` (roster, `llm` kind via `openaimodel`,
Valkey session, seed), `store` (sink, OpenSearch v5, indices, paginated reads with cursor),
`handler` (threads, messages, agents, socket). UI: socket, reducer, store, `useThread`, history
loading.
Checkpoint: chat with `chat`; reload shows history; close the tab mid-answer and reopen → the
answer continues; restart the server → history intact; two tabs on one thread both stream.

**Phase 3 — Tools, MCP, HITL.** Function tools; `mcp.yaml` → `mcptoolset`s; `write_note`
approval with justification; `question`; resume path. UI: approval and question cards wired to `approve` / `answer`.
Checkpoint: approve with a justification the tool echoes back; deny; answer a question; restart
the server between request and approval and it still resumes; an MCP tool from a YAML-configured
server runs.

**Phase 4 — Multi-agent.** Roster kinds (`sequential`, `parallel`, `loop`, `graph`) with step
activities; `assistant` dispatching `researcher`, `writer` and the pipeline onto child threads,
`SUBAGENT_*`, progress, task list, `report_progress`. UI: agent cards, child panel, step cards.
Checkpoint: a simple question is answered directly; a research request makes the assistant
spawn parallel children whose live detail opens in the panel; the pipeline run directly shows
its steps as cards; reload restores all of it.

**Phase 5 — Memory and compaction.** adk `memory.Service` on OpenSearch, preload and load
tools for primary agents, `AddSessionToMemory` hook, explicit memory tools; compaction config
per agent; LLM titles as a post-run hook.
Checkpoint: a fact stated in one thread is recalled in a new thread; a 30-turn thread compacts
and the next answer still references early context; thread titles appear.

**Deviations from the text above, as built.** Every interrupt is adk's tool confirmation;
there is no `input_required` reason. The `question` tool raises a confirmation carrying its
questions and is answered with `{output}`; a `task` call raises one carrying its sub-agent's
request and relays the answer to the child (see AGENTS.md §5). The `runs:active` set and the `run:{id}:done`
channel are gone: a thread's status is read from its run lease, and `Wait` polls the run
record. There is no cap on concurrent children; the request queue's in-flight limit bounds
work. Runs start over REST (`POST /v1/agent/chat`, the
chat-completions shape plus `thread_id`, `agent_id`, `connectors`) on threads the server
created on demand from the first run; decisions and cancels are REST too, and the socket only
subscribes. Connectors (`connectors.yaml`) are the user-facing tool groups, stored per thread
and inherited by sub-agents; agents with explicit `tools:` ignore them. `auto` is a `router`
kind rather than the assistant deciding per message. `task` emits `SUBAGENT_*` plus the `agent` card
but no `task_list` rows (the task list stays the `todowrite` tool's). Workflow steps are
delimited by `STEP_STARTED/FINISHED{stepName}` only; there is no `step` activity. The answer
to a client-answered tool is recorded as `TOOL_CALL_RESULT` on resume, and approvals as
`CUSTOM{approval}`. Memory, compaction and the `graph` kind are implemented; LLM titles are
not (titles are the first line of the first message). MCP tools keep the names their server
reports; there is no `<server>_` prefix. `graph` and MCP are implemented but were not exercised
against a live server in this session.

**Phase 6 — Later.** RAG tools, skills, suggestions, attachments, regenerate, office artifacts,
MCP OAuth, `cmd/dev` (`adkrest`), bubbletea `cmd/cli`, authentication, Kafka `Log` and `Sink`,
worker containers (§3.15).

---

## 7. Verification

Tests are deferred by decision. Each checkpoint above is exercised with `curl` for REST and the
proxies and `websocat` for the socket (`websocat ws://localhost:8000/v1/agent/chat?v=1`, then
paste `run` / `subscribe` frames). Development scripts stay uncommitted.

When tests are added, these are the ones worth having, in order: `stream.Coalesce` keeps
`ValidateSequence` green; reducer golden fixtures in the UI repo (recorded event sequences →
expected view model); `agent.Service` lifecycle
with the memory log (start, subscribe-after, pause/resume, cancel, orphan, idempotent start);
finalizer idempotency under duplicate `runs:done`; the Valkey session service against
`sessiontestsuite`; proxy SSE pass-through and header rewrite.

---

## 8. Risks and open questions

1. **Reasoning deltas through `openaimodel`.** Resolved 2026-10-08: with `api: responses` on
   the provider, `REASONING_*` events stream from the in-tree model (verified against
   OpenRouter with gpt-5-mini). Chat Completions mode ignores reasoning, so gateways that
   support `/v1/responses` (LiteLLM does) should set `api: responses`. No custom provider.
2. **AG-UI 1.0 is new** (1.0.2 on 2026-10-05) and the Go SDK is community-maintained and
   unversioned. Pin the commit; vendor `pkg/core` if it drifts. Emit only 1.0 event types
   (`REASONING_*`, not `THINKING_*`).
3. **`openaimodel` is marked experimental**; pin adk v2.5.0.
4. **OpenRouter-style gateways require `max_tokens`**; set `MaxOutputTokens` per agent from
   config and verify it is forwarded.
5. **AI Elements prop types** come from the `ai` package. Keep it as a types-only dependency or
   copy the handful of part types into `lib/chat/types.ts`; either way nothing from it runs.
6. **`RUN_STARTED.metadata`**: confirmed, the Go SDK's `BaseEvent` carries `Metadata` on every
   event, so turn metadata rides on `RUN_STARTED` and `RUN_FINISHED` as written.
7. **At-least-once streams** need idempotent consumers: finalisation upserts by message id;
   the request stream never re-executes (lease check on claim).
8. **Crash mid-segment** loses nothing persisted but the partial turn is finalised as `error`
   by the orphan rule; periodic persistence of partial turns is a small later addition.
9. **Child detail lives on the child thread**; the parent shows the card and progress. One
   subscription per visible child is the cost of never duplicating events.
10. **The reducer is ours.** Tool-state transitions and interleaved message ids are where bugs
    hide; `ValidateSequence` on the server bounds the input, and the reducer golden fixtures
    (§7) are the first tests to add when testing starts.

---

## Appendix A — wire example

An assistant run that dispatches one sub-agent, then an approval pause and resume. Frames are
`{type: "event", threadId, seq, event}`; only `event` is shown.

```jsonc
{"type":"RUN_STARTED","threadId":"th_1","runId":"run_1","metadata":{"turnId":"0192…","agentId":"assistant","model":"openai/gpt-4.1","createdAt":1759840000000}}
{"type":"STEP_STARTED","stepName":"assistant"}
{"type":"REASONING_START","messageId":"r1"} {"type":"REASONING_MESSAGE_START","messageId":"r1"} {"type":"REASONING_MESSAGE_CONTENT","messageId":"r1","delta":"Need a search first."} {"type":"REASONING_MESSAGE_END","messageId":"r1"} {"type":"REASONING_END","messageId":"r1"}
{"type":"TOOL_CALL_START","toolCallId":"call_1","toolCallName":"task","parentMessageId":"a1"}
{"type":"TOOL_CALL_ARGS","toolCallId":"call_1","delta":"{\"subagent_type\":\"researcher\",\"description\":\"Find sources\",\"prompt\":\"…\"}"}
{"type":"TOOL_CALL_END","toolCallId":"call_1"}
{"type":"ACTIVITY_SNAPSHOT","messageId":"tasks","activityType":"task_list","replace":true,"content":{"tasks":[{"id":"call_1","title":"Find sources","status":"in_progress","subagentRunId":"sa_1"}]}}
{"type":"SUBAGENT_STARTED","subagentRunId":"sa_1","name":"researcher","parentToolCallId":"call_1"}
{"type":"ACTIVITY_SNAPSHOT","messageId":"sa_1","activityType":"agent","replace":true,"content":{"subagentRunId":"sa_1","childThreadId":"th_1.sa_1","name":"researcher","status":"running","startedAt":1759840000500,"progress":"read_url: example.com/report","log":[{"at":1759840001200,"message":"read_url: example.com/report"}]}}
// the child's own stream (subscribe{threadId:"th_1.sa_1"}) carries its text, reasoning and tool events; nothing is mirrored here
{"type":"ACTIVITY_SNAPSHOT","messageId":"sa_1","activityType":"agent","replace":true,"content":{"…":"…","status":"done","durationMs":4210,"text":"Three sources: …"}}
{"type":"SUBAGENT_FINISHED","subagentRunId":"sa_1","outcome":{"type":"success"}}
{"type":"TOOL_CALL_RESULT","toolCallId":"call_1","messageId":"t1","content":"Three sources: …"}
{"type":"ACTIVITY_SNAPSHOT","messageId":"tasks","activityType":"task_list","replace":true,"content":{"tasks":[{"id":"call_1","title":"Find sources","status":"completed","subagentRunId":"sa_1"}]}}
{"type":"STEP_FINISHED","stepName":"assistant"}
{"type":"STEP_STARTED","stepName":"assistant"}
{"type":"TOOL_CALL_START","toolCallId":"call_2","toolCallName":"write_note","parentMessageId":"a2"}
{"type":"TOOL_CALL_ARGS","toolCallId":"call_2","delta":"{\"title\":\"Summary\",\"body\":\"…\"}"} {"type":"TOOL_CALL_END","toolCallId":"call_2"}
{"type":"STEP_FINISHED","stepName":"assistant"}
{"type":"RUN_FINISHED","threadId":"th_1","runId":"run_1","outcome":{"type":"interrupt","interrupts":[{"id":"adk_c7","reason":"confirmation","toolCallId":"call_2","message":"Save the summary to your notes?"}]},"usage":[{"model":"openai/gpt-4.1","inputTokens":1800,"outputTokens":420}],"metadata":{"durationMs":6100}}
// run status: awaiting_input. Client approves with a justification → run{resume} → new segment:
{"type":"RUN_STARTED","threadId":"th_1","runId":"run_2","metadata":{"turnId":"0192…","agentId":"assistant","model":"openai/gpt-4.1"}}
{"type":"CUSTOM","name":"approval","value":{"toolCallId":"call_2","approved":true,"justification":"looks right"}}
{"type":"TOOL_CALL_RESULT","toolCallId":"call_2","messageId":"t2","content":"{\"ok\":true}"}
{"type":"STEP_STARTED","stepName":"assistant"}
{"type":"TEXT_MESSAGE_START","messageId":"a3","role":"assistant"} {"type":"TEXT_MESSAGE_CONTENT","messageId":"a3","delta":"Saved. Here is the summary…"} {"type":"TEXT_MESSAGE_END","messageId":"a3"}
{"type":"STEP_FINISHED","stepName":"assistant"}
{"type":"RUN_FINISHED","threadId":"th_1","runId":"run_2","outcome":{"type":"success"},"usage":[{"model":"openai/gpt-4.1","inputTokens":2300,"outputTokens":380}],"metadata":{"durationMs":9800}}
```

Stored: one `messages` document for the turn with `events` = the above with contiguous deltas
merged, and `text` = "Saved. Here is the summary…". The client's reducer folds it into one
assistant message with parts `[step, reasoning, tool(task), data(task_list), data(agent), step,
tool(write_note, output-available, approval), step, text]`.

## Appendix B — reducer: AG-UI event → view model

View model per thread: `{messages: Message[], status: idle | streaming | awaiting_input |
error, interrupts: Interrupt[], cursor}`. `Message = {id, role, parts: Part[], metadata}`;
parts keep the AI SDK shapes (`text`, `reasoning`, `dynamic-tool` with `state`, `step-start`,
`data-<name>` with `id`) so the components are unchanged.

| AG-UI event | State change | Notes |
|---|---|---|
| `RUN_STARTED{metadata.turnId}` | if the last message's id ≠ `turnId`, append an assistant message `{id: turnId, parts: [], metadata}`; set `status: streaming` | same `turnId` across segments ⇒ the resumed segment continues the same message |
| `STEP_STARTED{stepName}` | append `step-start` and a `data-step{id, data: {agent: stepName, status: running}}` | |
| `STEP_FINISHED` | update that `data-step` to `done` | |
| `TEXT_MESSAGE_START{messageId, role}` | append `text{id: messageId, text: "", state: streaming}`; a `user` role in history starts a user message instead | |
| `TEXT_MESSAGE_CONTENT{delta}` | append to the text part with that id | error if unknown id |
| `TEXT_MESSAGE_END` | `state: done` | |
| `REASONING_MESSAGE_START/CONTENT/END` | same as text on a `reasoning` part | |
| `REASONING_END` | set `durationMs` on the reasoning part from the timestamps | |
| `TOOL_CALL_START{toolCallId, toolCallName}` | append `dynamic-tool{toolCallId, toolName, state: input-streaming, title?, kind?}` | |
| `TOOL_CALL_ARGS{delta}` | buffer args; set `input` to the partial parse | |
| `TOOL_CALL_END` | `state: input-available`, `input: JSON.parse(args)` | |
| `TOOL_CALL_RESULT{toolCallId, content}` | `state: output-available, output` or `output-error, errorText` when the content is an error envelope | |
| `ACTIVITY_SNAPSHOT{messageId, activityType, content}` | upsert `data-<activityType>{id: messageId, data: content}` in place | `task_list`, `agent`, `artifact`, `step` |
| `SUBAGENT_STARTED/FINISHED/ERROR` | no change beyond the `agent` activity | kept on the wire for AG-UI clients |
| `CUSTOM{name: "approval", value}` | set `approval: {approved, justification}` on the tool part; `state: output-denied` when `approved: false` | |
| `RUN_FINISHED{outcome: success, usage}` | `status: idle`; merge `usage`, `durationMs` into the message metadata | usage summed across the list |
| `RUN_FINISHED{outcome: interrupt}` | `status: awaiting_input`; per interrupt: `confirmation` → tool part `state: approval-requested, approval: {id, message}`; `input_required` → tool part stays `input-available` and the interrupt is recorded with its `responseSchema` | `interrupts` is what `approve` / `answer` resolve |
| `RUN_FINISHED{outcome: cancelled}` | `status: idle`; metadata `cancelled: true` | |
| `RUN_ERROR{message}` | `status: error`; metadata `error` | |
| anything else | ignored | cursor still advances |

History folds with the same function: each stored message's compacted events are reduced in
order into an empty thread state, then the live subscription continues from the cursor.

## Appendix C — adk v2 signatures used

```go
llmagent.New(llmagent.Config{Name, Description, Instruction string, Model model.LLM, Tools []tool.Tool, Toolsets []tool.Toolset,
    SubAgents []agent.Agent, OutputKey string, DisallowTransferToParent, DisallowTransferToPeers bool,
    GenerateContentConfig *genai.GenerateContentConfig}) (agent.Agent, error)
sequentialagent.New / parallelagent.New (Config{AgentConfig: agent.Config{Name, SubAgents}}); loopagent.New(Config{AgentConfig, MaxIterations})
workflowagent.New(workflowagent.Config{Name, SubAgents, Edges: workflow.NewEdgeBuilder().Add(a, b).AddRoute(b, c, route).Build()})
openaimodel.NewModel(ctx, modelName, &openaimodel.ClientConfig{APIKey, BaseURL, HTTPClient, API}) (model.LLM, error)
runner.New(runner.Config{AppName, Agent, SessionService, MemoryService, Compaction *compaction.Config, AutoCreateSession}) (*runner.Runner, error)
(*runner.Runner).Run(ctx, userID, sessionID string, msg *genai.Content, agent.RunConfig{StreamingMode: agent.StreamingModeSSE}) iter.Seq2[*session.Event, error]
functiontool.New[TArgs, TResults](functiontool.Config{Name, Description, IsLongRunning, RequireConfirmation}, func(agent.Context, TArgs) (TResults, error)) (tool.Tool, error)
mcptoolset.New(mcptoolset.Config{Endpoint string | Transport mcp.Transport, Auth, RequireConfirmation bool, RequireConfirmationProvider}) (tool.Toolset, error)
tool.FilterToolset(ts, tool.AllowedToolsPredicate(names))
memory.Service{AddSessionToMemory(ctx, session.Session) error; SearchMemory(ctx, *memory.SearchRequest) (*memory.SearchResponse, error)}
preloadmemorytool.New(); loadmemorytool.New()
compaction.Config{CompactionInterval, OverlapSize, TokenThreshold int}; compaction.NewLLMSummarizer(compaction.LLMSummarizerConfig{…})
agent.Context: FunctionCallID(), ToolConfirmation() *toolconfirmation.ToolConfirmation, RequestConfirmation(hint string, payload any) error
toolconfirmation.FunctionCallName == "adk_request_confirmation"; toolconfirmation.OriginalCallFrom(*genai.FunctionCall)
session.Service{Create, Get, List, Delete, AppendEvent}; session.ErrNotFound; session/sessiontestsuite
session.Event{LLMResponse{Content, Partial, UsageMetadata, FinishReason}, ID, InvocationID, Author, Branch, LongRunningToolIDs, Actions{StateDelta, RequestedToolConfirmations}}
```
