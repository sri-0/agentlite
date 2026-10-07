# How other agent systems stream to their UIs

Research notes, 2026-10-08. Note: `PLAN.md` v3 (same day, after owner review) moved the wire
and storage format from the AI SDK chunk vocabulary to AG-UI 1.0 events with a single
client-side reducer (no `useChat`). The findings below still hold; the "agentlite (plan)" column and §4
describe the v2 design and should be read with that change in mind. Source-level audits of five open systems, done to pressure-test the
`agentlite` design in `PLAN.md`. Each was read from a shallow clone at the commit noted; claims
below cite the files that were read.

| System | What was read | Commit |
|---|---|---|
| opencode (`anomalyco/opencode`, ex `sst/opencode`) | `packages/opencode/src/{server,session,tool,permission,question}`, `packages/core/src/{event,session}`, `packages/schema/src`, `packages/tui`, `packages/app` | `ecc4916` (2026-10-06) |
| T3 Code (`pingdotgg/t3code`) | `apps/server/src/{ws.ts,orchestration-v2/*}`, `packages/contracts/src/orchestrationV2.ts`, `packages/client-runtime/src/{rpc,state}`, `docs/internals/*`, `docs/orchestration-v2/*` | `611132c` (2026-10-07) |
| Codex app-server (`openai/codex`) | `codex-rs/app-server-protocol/src/protocol/{common.rs,v2/*}`, `codex-rs/app-server/src/*`, `codex-rs/rollout`, `codex-rs/history` | `95ec468` (2026-10-07) |
| Agent Client Protocol (`agentclientprotocol/agent-client-protocol`) | `docs/protocol/v2/*`, `docs/rfds/*`, `schema/v2/schema.json` | `4d75b46` (2026-10-07) |
| Goose (`block/goose`) | `crates/goose/src/{acp,session}`, `crates/goose-provider-types`, `ui/desktop/src/acp` | `8c0c409` (2026-10-07) |

---

## 1. What each one does, in a paragraph

**opencode.** A Bun/Node server (now Effect-based, Hono is gone) exposes HTTP for commands and
one Server-Sent-Events stream per directory (`GET /event`) or per server (`GET /global/event`)
for everything: message and part updates, session status, permissions, questions, file watcher,
LSP. WebSockets are used only for terminals. Text streams as a non-durable
`message.part.delta {partID, field, delta}` per provider token; every part lifecycle boundary
(`text-start`, `text-end`, tool state change, step finish) publishes the **whole part** as a
durable `message.part.updated`, which a projector upserts into SQLite in the same transaction
that assigns a per-session `seq`. Persisted shape equals wire shape equals client store shape.
The live SSE carries no cursor (`id:` is explicitly undefined), so on reconnect the TUI and web
app refetch, and the web app needed roughly 1,400 lines of merge logic to reconcile fetched
pages with events that arrived during the fetch. Sub-agents are child sessions
(`session.parent_id`) linked from the parent's `task` tool part via `metadata.sessionId`; the
TUI subscribes to the child session's store to render its live tool calls. Permissions and
questions are in-memory deferreds, recovered on reconnect by polling list endpoints.

**T3 Code.** A Node server (Effect-TS) wraps other harnesses (Codex app-server over stdio
JSON-RPC, Claude Agent SDK, Cursor, OpenCode, ACP agents) and serves web, Electron and mobile
clients over **one WebSocket per environment** carrying Effect RPC with Ack-based flow control.
Every provider event is normalised into its own domain events; raw provider frames never reach
storage. The durable event log is a single SQLite table whose autoincrement `sequence` is the
only cursor; events and per-entity projection rows are written in one transaction, then
published in-process. Nothing on the wire is a token delta: text arrives as
`turn-item.updated` with the full accumulated text, coalesced at 50 ms, and tool rows are
whole-entity upserts. A thread opens with a bounded HTTP snapshot (last ~75 rows plus an opaque
history cursor), then `subscribeThread{afterSequence}`; the server replays the gap if it is
under 128 events and 1 MiB (measured before decoding), otherwise sends a fresh snapshot.
Large bodies are stripped on the wire (`outputOmitted: true`) and fetched on demand.
Per-subscription buffers are capped (1,000 items / 8 MiB) and overflow is a typed error that
makes the client resubscribe from its cursor. Sub-agents are child threads with lineage; child
narration is never rendered in the parent, only a `subagent` row with prompt, progress and
result. Approvals are durable rows with a `responseCapability` (`live | message |
not_resumable`); a restart expires orphans.

**Codex app-server.** JSON-RPC 2.0 over stdio, WebSocket, in-process, or a unix control
socket. A turn is `turn/start` (returns immediately) followed by notifications:
`item/started` (skeleton), typed deltas on a stable `itemId` (`item/agentMessage/delta`,
`item/reasoning/textDelta`, `item/commandExecution/outputDelta`), then `item/completed` with
the **authoritative full item** (the plan item's doc comment says outright that the completed
item may not equal the concatenated deltas). No sequence numbers on the wire. Storage is an
append-only rollout JSONL per thread (model-visible items and UI events together, with an
`ordinal`), projected into turns and items and served paginated with opaque and anchor cursors;
full-history hydration is deprecated. Reconnect is `thread/resume`, which merges the live turn's
snapshot into the history; missed deltas are not replayed. The daemon keeps running without
clients. Approvals are server-to-client JSON-RPC requests with an `approvalId` distinct from
the `itemId`, typed decision enums including `decline` versus `cancel`, and a
`serverRequest/resolved` broadcast so other clients dismiss. Sub-agents are threads with
`parent_thread_id`; parent subscribers are auto-attached to children; the parent stream carries
`collabAgentToolCall` and `subAgentActivity` items. Slow consumers are disconnected.

**Agent Client Protocol (v2 draft, Goose as reference implementation).** JSON-RPC over stdio,
with an HTTP RFD (POST returns 202; SSE or WebSocket for the stream). v2 changed
`session/prompt` to return `{messageId}` at insertion instead of pending for the whole turn,
explicitly because the v1 shape "entangled prompt acceptance with turn completion, which made
replay, multi-client sessions, background work, and queued messages awkward". All updates are
**upserts keyed by id** with uniform patch semantics: chunks append, a full update replaces,
omitted means unchanged, `null` clears. Load is replay: `session/resume{replayFrom: start}`
replays `session/update` notifications; before replaying a message from chunks the agent must
send that message with `content: []` to clear it. The HTTP RFD states "In-flight messages are
not replayed" for v1 and plans `Last-Event-ID`-style resumption. Permissions are
server-to-client requests with an option set whose `kind` is `allow_once | allow_always |
reject_once | reject_always`. Sub-agents (draft) are announced on the parent with
`subagent_update` before any child traffic; child events carry the child's session id. Goose
persists a reduced `Message[]` in SQLite and re-derives notifications on load; its in-flight run
is cancelled when the carrying connection drops, which is the one anti-pattern in the set.

**AG-UI 1.0 and AI SDK UI Message Stream.** Covered in `PLAN.md` §2 and §0. Both are event
vocabularies rather than full client protocols. Neither specifies reconnect; AG-UI has native
sub-agent events, AI SDK has native tool approval and a parts model that doubles as the
persistence format.

---

## 2. Matrix

| | opencode | T3 Code | Codex app-server | ACP v2 / Goose | agentlite (plan) |
|---|---|---|---|---|---|
| Transport | HTTP commands + SSE events (WS for pty only) | One WS per environment, Effect RPC, Ack flow control | JSON-RPC over stdio / WS | JSON-RPC over stdio; HTTP+SSE or WS | One WS per client, JSON frames, AI SDK chunks |
| Connection granularity | Global or per-directory firehose | One socket, per-thread subscriptions | One connection, per-thread subscriptions | One connection, per-session streams | One socket, per-thread subscriptions (amended, was one socket per thread) |
| Cursor on the wire | None (durable `seq` exists in DB; experimental `/sync/history` uses it) | `sequence` on every event | None (rollout `ordinal` in storage only) | None (planned "last replay ID") | Valkey stream id on every chunk |
| Streaming text | Delta per token (not durable) + full part at boundaries | Full accumulated text, coalesced 50 ms | Delta per token on item id + `item/completed` snapshot | Chunks append; full message replaces | Delta per token (AI SDK) + folded `UIMessage` snapshot |
| Tool calls | Full part upsert per state change (4 states, timestamps) | Whole-row upsert per change | `item/started` → typed deltas → `item/completed` | `tool_call_update` upsert (kind, status, content, locations) | `tool-input-*` / `tool-output-*` upsert by `toolCallId` |
| Persisted form | Part rows (`data` JSON) = wire shape | Event log + projection tables = wire shape | Rollout JSONL → projected items | Reduced `Message[]` (Goose); unspecified (ACP) | Reduced `UIMessage` = wire shape |
| Cold load | Full or paginated `GET /session/:id/message` | Bounded HTTP snapshot + opaque history cursor | Paginated turns/items with anchor cursors | Replay notifications from start (or tail) | Paginated `GET /threads/{id}/messages` (amended) |
| Reconnect mid-run | Refetch everything, merge | Replay gap if small, else snapshot | Resume merges live-turn snapshot; no delta replay | Not replayed (v1) | Replay from cursor; snapshot-as-chunks when no cursor (amended) |
| Run outlives connection | Yes | Yes | Yes (daemon) | ACP: yes over HTTP; Goose: **no**, run cancelled | Yes |
| Approvals | In-memory deferred, poll lists on reconnect | Durable row with `responseCapability`, expired on restart | Server→client RPC request, option enums, `approvalId` ≠ `itemId`, `resolved` broadcast | Server→client RPC request with option kinds | `tool-approval-request` part persisted at pause; resume survives restart |
| Sub-agents | Child session + parent tool part `metadata.sessionId` | Child thread with lineage; compact `subagent` row in parent | Child thread (`parent_thread_id`); attribution items in parent; auto-attach subscribers | Draft `subagent_update` announced on parent | Child thread per sub-agent + compact `data-agent` card; client subscribes to the child thread (amended) |
| Multi-client | Any number of SSE subscribers | Per-thread subscribers | Per-thread subscriber sets, broadcast lifecycle | Implementation-defined | Any number of subscribers via the shared log |
| Backpressure | Unbounded queue (v1); 256-slot dropping queue (v2) | Ack-based + 1,000 items / 8 MiB cap → typed overflow | Disconnect slow consumer | Per-session streams | Each subscriber reads Valkey at its own pace; no shared buffer |

---

## 3. Where the five agree

1. **Stable ids for every streamed thing, and an authoritative snapshot at the end.** Codex
   says it in a doc comment, ACP makes it a protocol rule, opencode and T3 implement it as
   upserts. Deltas are for the screen; snapshots are for storage and for anyone who joins late.
2. **Persist a reduced model, not the token stream.** Codex projects its log into items and
   deprecated full-history hydration; Goose and opencode store the reduced shape directly; T3
   stores both but reads the projection. Nobody serves raw deltas as history.
3. **The persisted shape is the wire shape is the client store shape.** opencode's
   `{...row.data, id, sessionID}`, T3's projection, AI SDK's `UIMessage`. Every conversion layer
   someone added later (opencode's V1/V2 shims, T3's wire projection) was for size, not shape.
4. **Runs outlive connections.** All but Goose, and Goose's behaviour is the one everyone else
   avoided.
5. **One connection per client, many subscriptions.** T3, Codex and ACP all multiplex threads
   over one connection; opencode's single firehose is the degenerate version and it costs the
   client routing and filtering work.
6. **Approvals are correlated requests with their own id, and other clients are told when one
   is resolved.** Codex and ACP via RPC request ids and option sets; T3 via durable rows; Goose
   and AI SDK by persisting the request inside the message so a reload can re-issue it.
7. **Sub-agents are child threads.** Four of four systems that have sub-agents give each one its
   own session or thread with a parent link, and keep the parent's view compact. T3 refuses to
   render child narration inline at all.
8. **Nobody has cursor-based replay of missed deltas except T3**, and T3 falls back to a
   snapshot when the gap is large. ACP calls resumption future work; Codex merges a snapshot;
   opencode refetches.

---

## 4. What this changes in `PLAN.md`

Each item is applied in the plan; this is the reasoning.

1. **One WebSocket per client, not per thread** (§3.4). Every frame carries `threadId`; the
   transport is a singleton shared by all `Chat` instances. Enables child-thread subscriptions
   and live sidebar status later without more sockets. Cost: one field per frame and a map of
   subscriptions per socket.
2. **Cursorless attach sends a snapshot-as-chunks, cursor attach replays** (§3.4, §3.5). A page
   reload must not replay thousands of deltas; the server folds the in-flight segment from the
   log once and emits it as a compact chunk sequence (full text per part, open parts left
   unclosed), then tails live. A mid-stream drop, where the transport still holds the state,
   replays from the cursor. This is T3's "replay if small, else snapshot" rule with the decision
   made by who is asking rather than by measuring, and it keeps the wire vocabulary unchanged.
3. **Sub-agents are child threads run through the same run service** (§3.8). The `task` tool
   creates a thread with `parent_id`, starts an ordinary run on it, waits for it, and updates
   one compact `data-agent` card on the parent. Nothing is mirrored between streams: a client
   that wants the child's live detail subscribes to the child thread on the same socket, and
   the child's transcript is persisted like any thread. This deletes the special
   child-translation path from the original plan, gives sub-agents durability and cancellation
   for free, and matches what Codex, T3, opencode and Goose all converged on.
4. **Runs are idempotent on the user message id** (§3.5). Codex echoes `client_user_message_id`,
   T3 stores command receipts. A `run` frame whose message id already exists in the thread
   re-subscribes instead of starting a second run, so transport retries cannot double-post.
5. **History is paginated from day one** (§3.7, §3.12). Codex deprecated full hydration; T3
   needed a bounded cold open. `GET /v1/threads/{id}/messages?before=<seq>&limit=50`.
6. **Protocol version on the socket URL** (§3.4). T3 answers 426 on mismatch; ACP and Codex gate
   per connection. One query parameter now avoids a painful migration later.
7. **Reasoning duration in `providerMetadata`** (§3.3). opencode keeps `time.start/end` on
   reasoning parts so "thought for 4 s" survives reload. AI SDK's sanctioned slot for that is
   `providerMetadata` on `reasoning-end`; the plan originally dropped the timing entirely.
8. **Tool outputs are capped by the tool** (§3.9). T3 strips bodies on the wire and fetches
   on demand; for v1 the cheaper rule is that no tool returns more than 32 KiB, with a detail
   endpoint noted for later.
9. **Backpressure is stated, not assumed** (§3.5). Each subscriber goroutine reads the Valkey
   stream at its client's pace and writes synchronously to the socket, so a slow client slows
   only itself and the producer never blocks. Codex and T3 both had to add explicit policies
   because they fan out from memory.

---

## 5. What we deliberately do not copy

- **T3's execution graph** (runs, attempts, nodes, provider turns, checkpoints, context
  transfers; an 11,000-line orchestrator and 6,000-line projection store). It exists to steer,
  fork, roll back and switch providers across eight harnesses. We have one harness and a chat.
- **Whole-entity upserts for text** (T3). AI SDK deltas are smaller and the client reducer
  already exists; snapshots are reserved for late joiners.
- **A global firehose** (opencode). Per-thread subscriptions on one socket are the same cost
  server-side and spare the client the filtering.
- **JSON-RPC framing** (Codex, ACP). Our client is `useChat`, which wants a chunk stream, not
  request ids; approvals ride on AI SDK's own parts. If a non-browser client ever needs
  request/response semantics, JSON-RPC can wrap the same chunks.
- **Two overlapping protocols** (opencode's V1 `message.part.*` beside V2 `session.next.*`).
  The whole point of the rebuild is one vocabulary.
- **Codex's twenty item types.** They encode a coding agent's domain (command execution, file
  change, review mode). Our tool parts plus three data parts cover a chat with tools; add
  typed parts when a real renderer needs them, not before.
