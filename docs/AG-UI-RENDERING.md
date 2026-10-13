# AG-UI events → view model → components

How every AG-UI event this backend emits reaches the screen in `agentui`.
Written 2026-10-10 from the code as it stands; keep it current when the encoder
(`internal/stream/encode.go`) or the UI reducer (`agentui/lib/chat/reduce.ts`)
changes.

The backend has three emitters:

- the **encoder** (`internal/stream/encode.go`) translates adk session
  events into AG-UI events, one segment per run;
- the **`subagent` tool** (`internal/tools/builtin/subagent.go`) emits the
  sub-agent events while it follows a child run;
- the **run lifecycle** emits the bookkeeping events: the stored user turn
  (`internal/runs/start.go`), the recorded answer on resume
  (`internal/agents/resume.go`), the closing segment of a cancelled paused run
  (`internal/runs/cancel.go`), and the error for a run orphaned by a restart
  (`internal/runs/worker.go`).

On the UI side `agentui/lib/chat/reduce.ts` is the only place events are interpreted. It
folds each event into a part on the assistant message in progress; components
render parts and never look at events. The same function folds stored history
(`GET /v1/threads/{id}/messages`) and the live socket stream.

## Lifecycle events

| Event | Emitted by | Reducer effect | Rendered by |
|---|---|---|---|
| `RUN_STARTED` with metadata `turnId, agentId, model, createdAt` | encoder, once per segment | Opens the assistant message with id `turnId` and stores the metadata. A resumed segment finds the same id and continues that message. Status becomes `streaming`. | `MessageMeta` under the answer (agent name, model). `ThinkingIndicator` shows while the message has no visible part yet. |
| `STEP_STARTED` / `STEP_FINISHED` `{stepName}` | encoder, one per model call per author | Pushes a `step-start` part, then marks it done. | `message-list.tsx` groups parts into stages: consecutive steps by one agent form one stage. If a message has more than one stage and any stage's agent differs from the answering agent, every stage but the last renders as a collapsed `StageCard`, the last inline. |
| `RUN_FINISHED` with `outcome`, `usage`, metadata `durationMs` | encoder | Sums usage onto the message and records the duration. An `interrupt` outcome stamps each interrupt onto the tool part it names (`state: approval`, `approval.id` = interrupt id, `request` from the interrupt metadata for a question) and sets status `awaiting_input`. Any other outcome settles what is still open: leftover `approval` parts become `denied`, and agent cards still `running` or `waiting` become `error` ("stopped"). A `cancelled` outcome sets `metadata.cancelled`. | "Stopped." line; approval controls inside `ToolLine`; `QuestionCard` becomes answerable; `ThreadUsageRing` and `UsageView`. |
| `RUN_ERROR` | encoder on failure; worker for a run orphaned by a restart | Sets `metadata.error` on the message, settles open approvals and agent cards as for `RUN_FINISHED`, and sets the thread's status to `error`. | Red line under the message. (The thread banner is for a refused `run` or `answer` request and for a socket `error` frame.) |

## Content events

| Event | Emitted by | Reducer effect | Rendered by |
|---|---|---|---|
| `TEXT_MESSAGE_START/CONTENT/END` | encoder for the model; `runs` for the stored user turn | A `text` part that accumulates deltas, `done` at `TEXT_MESSAGE_END` (or when the run ends). Role `user` upserts the user message, replacing the optimistic copy the composer added. | `Markdown` (streamdown; streaming mode while the part is open, static once done), which shows each `[source:N]` as a `SourceChip`: the source's system as its connector's icon and name (from the conversation's source list and `GET /v1/connectors`) and the number, opening a popover with the source and the way into the sources panel. |
| `REASONING_START`, `REASONING_MESSAGE_START/CONTENT/END`, `REASONING_END` | encoder, only on providers using the responses API | A `reasoning` part with `startedAt`, then `durationMs` from `REASONING_END`. `REASONING_START` itself is ignored. | `MessageReasoning`, the "Thought for N seconds" dropdown. |
| `TOOL_CALL_START/ARGS/END` | encoder; `TOOL_CALL_START` carries `metadata.ui` for a tool with a UI renderer | A `tool` part in state `running`, with `ui` from the metadata. Args stream into `argsText` and are parsed into `input` at END. The start timestamp is kept. | The renderer the registry names for `ui` (`SlidesCall` for `slides`), else `ToolLine`; `QuestionCard` for a `question` call (the part paused with `request.toolName` = `question`). |
| `TOOL_CALL_RESULT` | encoder | State `done` with `output`, or `error` with `errorText` when the result is exactly `{error}`; computes `durationMs`. A denied call stays `denied`. | `ToolLine` status and expanded output. |
| `ACTIVITY_SNAPSHOT` type `task_list`, messageId `tasks` | encoder from a `todowrite` result | Upserts a `data-task-list` part by id. | `TaskBar` above the composer, which reads the newest list in the thread. Not rendered inline. |
| `ACTIVITY_SNAPSHOT` type `artifact`, messageId the artifact id | encoder from an `emit_artifact` result; the runtime for a `url` artifact an MCP tool recorded | Upserts a `data-artifact` part: `{id, title, kind, version, language?, url?}`, no body. | A button in the message; the artifact panel lists the conversation's artifacts (API and stream merged). `ArtifactView` fetches the body from the API, or embeds a `url` artifact's page, which follows the app's light or dark theme. The `/artifacts` page lists every conversation's artifacts (`GET /v1/artifacts`). |
| `ACTIVITY_SNAPSHOT` type `agent`, messageId the child run id | `subagent` tool, repeatedly as the child progresses | Upserts a `data-agent` part: name, description, status (`running`, `waiting`, `done`, `error`), progress line, log, final text, and while `waiting` the child's pending `interrupts`. | `AgentCard`. While waiting it shows each request with `ToolLine` (approval) or `QuestionCard` (question), built by the reducer's `interruptPart`, and sends the answer to the child's thread as an `answer` frame; the controls stay disabled until the card moves on, and a refused answer shows its error. Clicking opens the child thread live in the side panel through the same reducer. |
| `ACTIVITY_SNAPSHOT` type `compaction`, messageId `compaction:<uuid>` | the runtime, when adk compacts the session (AGENT-ARCHITECTURE.md, Context and memory) | Upserts a `data-compaction` part: `{status, summary?, inputTokens?, outputTokens?, durationMs?, error?}`, `status` one of `running`, `completed`, `failed`, `declined`. | `CompactionDivider`: a full-width rule where it happened, "Compacting context…" while running, then "Context compacted" with tokens and duration, and a collapsible `compact_context` card styled like a tool call holding the summary. `failed` is a muted rule with the error; `declined` renders nothing. |
| `SUBAGENT_STARTED` | `subagent` tool | Ignored; the card comes from the activity. | Nothing. |
| `SUBAGENT_FINISHED` / `SUBAGENT_ERROR` | `subagent` tool | Marks the card done, or failed with the message. | `AgentCard` status badge. |
| `CUSTOM` name `approval` `{toolCallId, approved, justification}` | runtime on resume | Tool part back to `running` (approved) or `denied`, with the justification. | `ToolLine` shield icon and the justification line. |

Events AG-UI defines that the backend never emits, which the reducer drops through
its default branch: `STATE_SNAPSHOT`, `STATE_DELTA`, `MESSAGES_SNAPSHOT`,
`TEXT_MESSAGE_CHUNK`, `TOOL_CALL_CHUNK`, `THINKING_*`, `ACTIVITY_DELTA`, `RAW`.

## Where state lives that is not a part

- **Thread status** (`idle | streaming | awaiting_input | error`) and the
  `error` of a refused request are on `ThreadState`. Pending interrupts are not stored separately:
  each is stamped onto its tool part.
- **Cursor**: every socket frame carries the log id of its event; the socket
  keeps the last one per followed thread and resubscribes from it after a
  reconnect, so no event is applied twice. Frames for a thread no longer
  followed are dropped.
- **Metadata** (`agentId, model, createdAt, durationMs, usage, cancelled,
  error`) is per message, from `RUN_STARTED`, `RUN_FINISHED` and `RUN_ERROR`.

## Findings (2026-10-10)

- The reducer is sound for replay: it is pure, every activity is an upsert by
  id, a resumed segment continues its message, and reconnecting with the stored
  cursor cannot apply an event twice.
- AG-UI's step is one model call, so a pipeline produced one card per call.
  Grouping consecutive steps by agent into a stage fixed the duplicate cards
  without changing the wire format.
- Tool calls carry timing from the event timestamps, like reasoning does.
- `SUBAGENT_STARTED` is unused on purpose: it is redundant with the first
  agent activity.
- Task lists live in the task bar, not in the message, and nothing in a
  sequential pipeline updates them; only the coordinator does. The planner no
  longer writes one.
