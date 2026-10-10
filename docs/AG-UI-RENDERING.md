# AG-UI events → view model → components

How every AG-UI event this backend emits reaches the screen in `agentui`.
Written 2026-10-10 from the code as it stands; keep it current when the encoder
(`internal/stream/encode.go`) or the UI reducer (`agentui/lib/chat/reduce.ts`)
changes.

The backend has three emitters:

- the **encoder** (`internal/stream/encode.go`) translates adk session
  events into AG-UI events, one segment per run;
- the **`task` tool** (`internal/tools/builtin/task.go`) emits the
  sub-agent events while it follows a child run;
- the **engine and worker** emit the bookkeeping events: the route decision,
  the recorded answers on resume, and the error for a run orphaned by a restart.

On the UI side `agentui/lib/chat/reduce.ts` is the only place events are interpreted. It
folds each event into a part on the assistant message in progress; components
render parts and never look at events. The same function folds stored history
(`GET /v1/threads/{id}/messages`) and the live socket stream.

## Lifecycle events

| Event | Emitted by | Reducer effect | Rendered by |
|---|---|---|---|
| `RUN_STARTED` with metadata `turnId, agentId, model, createdAt` | encoder, once per segment | Opens the assistant message with id `turnId` and stores the metadata. A resumed segment finds the same id and continues that message. Status becomes `streaming`. | `MessageMeta` under the answer (agent name, model). `ThinkingIndicator` shows while the message has no visible part yet. |
| `STEP_STARTED` / `STEP_FINISHED` `{stepName}` | encoder, one per model call per author | Pushes a `step-start` part, then marks it done. | `message-list.tsx` groups parts into stages: consecutive steps by one agent form one stage. If a message has more than one stage and any stage's agent differs from the answering agent, every stage but the last renders as a collapsed `StageCard`, the last inline. |
| `RUN_FINISHED` with `outcome`, `usage`, metadata `durationMs` | encoder | Sums usage onto the message and records the duration. An `interrupt` outcome stamps each interrupt onto the tool part it names (`state: approval`, `approval.id` = interrupt id, `request` from the interrupt metadata when it forwards a question or a sub-agent's request) and sets status `awaiting_input`. Any other outcome turns leftover `approval` parts into `denied`. A `cancelled` outcome sets `metadata.cancelled`. | "Stopped." line; approval controls inside `ToolLine`; `QuestionCard` becomes answerable; `ThreadUsageRing` and `UsageView`. |
| `RUN_ERROR` | encoder on failure; worker for a run orphaned by a restart | Sets `metadata.error` on the message and the thread's status to `error`. | Red line under the message. (The thread banner is only for REST failures to start or resume a run.) |

## Content events

| Event | Emitted by | Reducer effect | Rendered by |
|---|---|---|---|
| `TEXT_MESSAGE_START/CONTENT/END` | encoder for the model; the run service for the stored user turn | A `text` part that accumulates deltas. Role `user` upserts the user message, replacing the optimistic copy the composer added. | `MessageResponse` (streamdown markdown). |
| `REASONING_START`, `REASONING_MESSAGE_START/CONTENT/END`, `REASONING_END` | encoder, only on providers using the responses API | A `reasoning` part with `startedAt`, then `durationMs` from `REASONING_END`. `REASONING_START` itself is ignored. | `MessageReasoning`, the "Thought for N seconds" dropdown. |
| `TOOL_CALL_START/ARGS/END` | encoder | A `tool` part in state `running`. Args stream into `argsText` and are parsed into `input` at END. The start timestamp is kept. | `ToolLine`; `QuestionCard` for a `question` call, and for any part paused on a request whose `toolName` is `question` (a sub-agent's question forwarded through `task`). |
| `TOOL_CALL_RESULT` | encoder; the engine on resume for a recorded answer | State `done` with `output`, or `error` with `errorText` when the result is exactly `{error}`; computes `durationMs`. A denied call stays `denied`. | `ToolLine` status and expanded output. |
| `ACTIVITY_SNAPSHOT` type `task_list`, messageId `tasks` | encoder from a `todowrite` result | Upserts a `data-task-list` part by id. | `TaskBar` above the composer, which reads the newest list in the thread. Not rendered inline. |
| `ACTIVITY_SNAPSHOT` type `artifact`, messageId the artifact id | encoder from an `emit_artifact` result | Upserts a `data-artifact` part. | A button in the message; `ArtifactView` in the side panel. |
| `ACTIVITY_SNAPSHOT` type `agent`, messageId the child run id | `task` tool, repeatedly as the child progresses | Upserts a `data-agent` part: name, description, status, progress line, log, final text. | `AgentCard`. Clicking it opens the child thread live in the side panel through the same reducer. |
| `SUBAGENT_STARTED` | `task` tool | Ignored; the card comes from the activity. | Nothing. |
| `SUBAGENT_FINISHED` / `SUBAGENT_ERROR` | `task` tool | Marks the card done, or failed with the message. | `AgentCard` status badge. |
| `CUSTOM` name `approval` `{toolCallId, approved, justification}` | engine on resume | Tool part back to `running` (approved) or `denied`, with the justification. | `ToolLine` shield icon and the justification line. |
| `CUSTOM` name `route` `{agent, reason}` | engine after a routed start | Ignored. | Nothing; the routed agent already arrives in `RUN_STARTED` metadata. |

Events AG-UI defines that the backend never emits, which the reducer drops through
its default branch: `STATE_SNAPSHOT`, `STATE_DELTA`, `MESSAGES_SNAPSHOT`,
`TEXT_MESSAGE_CHUNK`, `TOOL_CALL_CHUNK`, `THINKING_*`, `ACTIVITY_DELTA`, `RAW`.

## Where state lives that is not a part

- **Thread status** (`idle | streaming | awaiting_input | error`) and a REST
  `error` are on `ThreadState`. Pending interrupts are not stored separately:
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
- Two events are unused on purpose: the route decision has no UI yet (the
  classifier's reason could go into `MessageMeta`), and `SUBAGENT_STARTED` is
  redundant with the first agent activity.
- Task lists live in the task bar, not in the message, and nothing in a
  sequential pipeline updates them; only the coordinator does. The planner no
  longer writes one.
