# Human in the loop

How a run stops to ask the user something, how the answer gets back to the right
place, and where each piece of that state is kept. Everything here is built on
adk-go's own tool confirmation; agentlite adds the records, the REST API and the
relaying between threads.

## One kind of pause

Every pause is adk's **tool confirmation**. A tool either has adk raise it
(`RequireConfirmation` on the tool, or `require_confirmation` for an MCP tool) or
raises it itself with `ctx.RequestConfirmation(hint, payload)`. Either way adk
emits a function call named `adk_request_confirmation` wrapping the real call,
records the request on the session event (`Actions.RequestedToolConfirmations`),
and stops the agent loop. When the answer arrives as that function's response,
adk calls the real tool again with `ctx.ToolConfirmation()` set.

Three things pause a run:

| What | Raised by | The user sees | Answered with |
|---|---|---|---|
| A tool that needs approval (`write_note`, MCP tools listed in `require_confirmation`) | adk, from the tool's config | the call's arguments, Approve / Deny, a justification field | `{approved, justification?}` |
| The `question` tool | the tool itself, with `request: {toolName: "question", args: {questions}}` | a question card | `{output: {answers, text?}}`, which becomes the tool's result |
| A `task` call whose sub-agent paused | the task tool, with `request: {subagent, childThreadId, toolName, args}` | the sub-agent's request: an approval line, or its question card | whatever the sub-agent's request needs; it is relayed to the sub-agent |

Using confirmation for all three matters: adk always stops its loop on a
confirmation, even when the pausing call shares a turn with other tool calls. (A
long-running tool that returns nothing does not stop the loop in that case, and
the model is then called with an unanswered call, which providers reject.)

## The flow for one thread

```
run executes ─► tool raises confirmation ─► adk stops the loop
   │
   ├─ encoder (internal/stream/encode.go): sees adk_request_confirmation, emits the
   │  real tool call, and records an interrupt {id, reason: "confirmation",
   │  toolCallId, message: hint, metadata: {toolName, args, request?}}
   ├─ RUN_FINISHED {outcome: {type: "interrupt", interrupts: [...]}}
   ├─ worker (internal/agent/worker.go): status awaiting_input; hitl.Add stores each
   │  interrupt (internal/hitl); the thread lease is made permanent while paused
   └─ finalizer persists the turn so far

user answers: POST /v1/threads/{id}/interrupts/{interruptId}  {approved…} | {output…}
   │
   ├─ Service.Decide (internal/agent/service.go): checks the interrupt belongs to the
   │  thread, records the decision (hitl.Decide), and calls Resume
   ├─ Resume queues a new segment of the same run with the resume entries
   └─ worker ─► engine.content (internal/engine/engine.go) turns each entry into the
      adk_request_confirmation response {confirmed, payload}, emits CUSTOM approval,
      and adk calls the tool again with ctx.ToolConfirmation():
        approval approved ─► the tool runs      denied ─► adk returns "rejected"
        question          ─► the tool returns the answers
        task              ─► relays to the sub-agent (below)
```

The continuation's `RUN_STARTED` carries the same `turnId`, so it folds into the
same assistant message; the UI never shows two messages for one paused turn.

## Sub-agents: the request goes up, the answer goes down

A sub-agent is a separate run on its own child thread, started by the parent's
`task` call. When the sub-agent pauses, the user should see the request on the
thread they are looking at (the parent), and the answer must reach the sub-agent,
which then carries on with its own work. That is how it works:

```
parent run                          child run (child thread)
──────────                          ────────────────────────
task(report-writer, prompt) ──────► starts, works…
  waits for the child               write_note ─► confirmation ─► child paused
  sees child awaiting_input           (hitl record on the child thread)
  reads the child's pending
  interrupt, keeps
  {childThread, runId, interruptId}
  in session state ("task:<callId>")
  RequestConfirmation(hint, request)
parent paused (hitl record on the
parent thread, child_thread_id set)

user answers on the parent thread
parent resumes ─► adk calls task again
  with the answer ─────────────────► Decide(child thread, child interrupt)
                                      child resumes, write_note runs, child
                                      keeps working and finishes
  waits again ◄──────────────────── result
task returns the child's result;
the parent's model continues
```

So the parent thread does handle the resume, but only as a relay: the parent's
model is not called with the approval, it only ever sees the `task` call's final
result. The work after the approval happens in the child.

Details that make this robust:

- **Approve or deny both reach the child.** adk rejects a confirmation answered
  "not confirmed" without calling the tool again, so for `task` (and `question`)
  the engine always marks the response confirmed and carries the real decision in
  the payload. A denial therefore reaches the sub-agent, which reports it.
- **Answer anywhere.** The child's own thread (open it from the agent card) shows
  the same request with the same buttons. Answering there resumes the child, and
  `Service.releaseParent` resumes the parent's waiting `task` call with the same
  answer; the task tool finds the child already answered and goes back to
  waiting. Either way the parent ends with the child's result and nothing is left
  pending.
- **Several pauses in a row.** If the child pauses again, the cycle repeats: each
  pause becomes one parent interrupt.
- **Cancel.** Cancelling a paused parent cancels the paused child it is waiting on
  (the parent's interrupt names the child thread). Cancelling a running parent
  cancels its running children. A paused or queued run that is cancelled gets a
  cancelled segment written, so the stored turn ends "cancelled", not "waiting".
- **Model and connectors** are inherited by the child; the parent's state key is
  per tool call, so parallel `task` calls each relay their own child.

## Storage

| Where | What | Lifetime |
|---|---|---|
| adk session (Valkey DB `REDIS_DB_SESSIONS`, `session:{thread}:*`) | the pending `adk_request_confirmation` call and `RequestedToolConfirmations`; the task tool's `task:<callId>` state | 30 days |
| Valkey DB `REDIS_DB_HITL`: `hitl:{interruptId}` | the interrupt record: thread, run, reason, tool name, args, message, child thread, status (`pending`, `resolved`, `cancelled`), decision, timestamps | 7 days |
| Valkey DB `REDIS_DB_HITL`: `hitl:thread:{thread}:pending` | the set of a thread's open interrupt ids | removed as each is decided or cancelled |
| Valkey DB `REDIS_DB_RUNS`: `run:{id}`, `thread:{id}:run` | run status `awaiting_input`; the lease, made permanent while paused | run: 7 days |
| Event log and OpenSearch message | the `RUN_FINISHED` interrupt outcome and the later `CUSTOM approval` | as the thread |

adk's session is what actually resumes the run; the `hitl` records are the
queryable, durable account of what was asked and how it was decided, and what the
REST endpoints read and write. Nothing about a pause lives only in memory, so a
paused run survives a server restart and can be answered afterwards.

## API

| Call | Purpose |
|---|---|
| `GET /v1/threads/{id}/interrupts` | the thread's pending interrupts: `{id, run_id, reason, tool_name, args, message, child_thread_id?, status, created_at}` |
| `POST /v1/threads/{id}/interrupts/{interruptId}` | decide one: `{approved, justification?}` or `{output}` → `202 {run_id, cursor}`; `404` unknown, `400` no answer, `409` not paused or already decided |
| `POST /v1/threads/{id}/cancel` | cancel the run, closing its interrupts |

The UI subscribes from the returned `cursor` to follow the continuation.

## Code map

| File | Role |
|---|---|
| `internal/tools/write_note.go` | the demo approval tool (`RequireConfirmation`) |
| `internal/tools/builtin/question.go` | the question tool (`RequestConfirmation` with its questions) |
| `internal/tools/builtin/task.go` | sub-agent dispatch, `forward` (raise the child's request), `decisionFor` (relay the answer) |
| `internal/mcp/mcp.go` | MCP tools needing approval (`require_confirmation`) |
| `internal/stream/encode.go` | `call`, `interrupt`, `confirmation`: adk events → AG-UI interrupt outcome |
| `internal/agent/worker.go` | records interrupts on pause, persists the lease |
| `internal/agent/service.go` | `Decide`, `Resume`, `releaseParent`, `Cancel` |
| `internal/hitl/hitl.go` | the interrupt records |
| `internal/engine/engine.go` | `content`: resume entries → adk confirmation responses; `CUSTOM approval` |
| `internal/handler/hitl.go` | the REST endpoints |
| agentui `lib/chat/reduce.ts`, `components/chat/messages/tool-line.tsx`, `question-card.tsx` | the UI side; see AG-UI-RENDERING.md |

## Limits

- Only the first pending interrupt of a paused child is forwarded at a time; a
  child that pauses on two calls at once gets the second forwarded after the
  first is answered.
- An approval decided on the child's thread and the parent's thread at the same
  moment: one wins, the other gets `409` and changes nothing.
- Sub-agents in the default roster cannot pause (they list their tools, and none
  of those need approval); forwarding matters once a sub-agent is given a
  connector or tool that does.
