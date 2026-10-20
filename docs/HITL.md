# Human in the loop

How a run stops to ask the user something, how the answer gets back, and where
each piece of that state is kept. Pausing and resuming are adk-go's own tool
confirmation; agentlite adds durable records, the socket frames that carry
answers, and the agent card that shows a sub-agent's requests on its parent's
thread.

## One kind of pause

Every pause is adk's **tool confirmation**. A tool either has adk raise it
(`RequireConfirmation` on the tool, or `require_confirmation` for an MCP tool) or
raises it itself with `ctx.RequestConfirmation(hint, payload)`. Either way adk
emits a function call named `adk_request_confirmation` wrapping the real call,
records the request on the session event (`Actions.RequestedToolConfirmations`),
and stops the agent loop. When the answer arrives as that function's response,
adk calls the real tool again with `ctx.ToolConfirmation()` set.

Two things pause a run:

| What | Raised by | The user sees | Answered with |
|---|---|---|---|
| A tool that needs approval (`write_note`, MCP tools listed in `require_confirmation`) | adk, from the tool's config | the call's arguments, Approve / Deny, a justification field | `{approved, justification?}` |
| The `question` tool | the tool itself, with `request: {toolName: "question", args: {questions}}` | the questions in place of the composer (shadcn questionnaire) | `{output: {answers, skipped?}}`, which becomes the tool's result: one list of chosen labels or typed text per question, empty for one skipped, and `skipped: true` when the user skipped them all, which the tool's description tells the model to take as "go on with your best judgement" |

`question` uses a confirmation rather than adk's long-running tools because adk
always stops its loop on a confirmation, even when the pausing call shares a
turn with other tool calls; a long-running tool that returns nothing does not,
and the model is then called with an unanswered call, which providers reject.

## The flow for one thread

```
run executes ─► tool raises confirmation ─► adk stops the loop
   │
   ├─ encoder (internal/stream/encode.go): sees adk_request_confirmation, emits the
   │  real tool call, and records an interrupt {id, reason: "confirmation",
   │  toolCallId, message: hint, metadata: {toolName, args, request?}}
   ├─ RUN_FINISHED {outcome: {type: "interrupt", interrupts: [...]}}
   ├─ worker (internal/runs/worker.go): status awaiting_input; hitl.Add stores each
   │  interrupt (internal/hitl); the thread's lease lives as long as the run record
   │  while paused (7 days); a failure to record them fails the run instead
   └─ the worker finalizes: it persists the turn so far, with the run's thread token

user answers over the socket:
   {type: "answer", requestId, threadId, interruptId, approved, justification} | {…, output}
   │
   ├─ ws (internal/ws/conn.go) ─► Manager.Answer (internal/runs/answer.go): the run
   │  must be paused and the interrupt pending on that thread; hitl.Decide marks
   │  the record resolved with a compare-and-set on it (SET … IFEQ the JSON just
   │  read), then adds it to the decided set before taking it out of the pending
   │  one, so whoever sees none pending finds every answer decided
   ├─ more interrupts still pending: ack, and the run stays paused
   ├─ the last one answered: the run is moved back to queued (a compare-and-set
   │  on awaiting_input, so two last answers cannot both resume it), every
   │  decided answer is taken, and the next segment is queued with all of them,
   │  for the user who gave the last answer, with their access token (AUTH.md)
   ├─ ack {threadId, runId, seq}: the client subscribes from seq to follow it
   └─ worker ─► Runtime.content (internal/agents/resume.go) turns the answer into the
      adk_request_confirmation response {confirmed, payload} for each answer, emits
      CUSTOM approval, and adk calls each tool again with ctx.ToolConfirmation():
        approved ─► the tool runs     denied ─► adk returns "rejected" to the model
        question ─► the tool returns the answers
```

The continuation's `RUN_STARTED` carries the same `turnId`, so it folds into the
same assistant message. A turn that paused on several calls (an approval and a
question in one model turn) resumes once, after the last of them is answered,
because adk expects every confirmation of the turn in the same request.

A thread takes one run at a time, so a message sent while the run waits is refused
as busy. The UI cancels the paused run first (a paused run closes at once, before the
cancel is acknowledged) and then sends the message, which starts the next turn; the
question or approval it passed over shows as not answered.

## Sub-agents: shown on the parent, answered on the child

A sub-agent is a separate run on its own child thread, started by the parent's
`subagent` call. Its pause is an ordinary pause of the child run, recorded on the
child thread. The parent is not involved in answering it:

```
parent run                               child run (child thread)
──────────                               ────────────────────────
subagent(report-writer, prompt) ───────► starts, works…
  follows the child's log until it ends   write_note ─► confirmation ─► paused
  (the parent's run keeps running, its     (hitl record on the child thread,
   worker slot given back; its model is     RUN_FINISHED interrupt on its log)
   not called)
  the call sees the pause on the child's
  log and puts the requests on the
  agent card ──────────────────────────► parent thread: card "Needs you", with
                                           the request and Approve / Deny
user answers on the card:
  {type: "answer", threadId: <child>, interruptId, approved…}  ─► child resumes,
                                                                    keeps working
  the call sees the child's next segment start and clears the request
  the child ends ─► subagent returns {result} to the parent's model
```

This is how opencode works: the child's request blocks only the child, the user
sees it from the top-level view and answers the child directly, and the parent's
call simply waits. The same request also shows on the child's own thread (open
it from the card); answering in either place is the same frame to the same
thread and interrupt, and the second answer gets `not_paused`.

- **Siblings keep running.** Each `subagent` call waits for its own child; adk
  runs a turn's calls concurrently. The parent's next model turn starts once
  every call in the turn has returned.
- **Cancel.** Cancelling the parent cancels its children, paused or running: the
  waiting `subagent` call sees its context end with the user's cancel as the
  cause and cancels the child, which closes the child's interrupts. A run
  cancelled while it is pausing is closed as cancelled, not left paused.
- **Durability.** The child's pause is durable: it survives a server restart and
  can be answered afterwards from its own thread. The parent's wait is a running
  run that has given its worker slot back. A shutdown ends the parent's turn as cancelled
  but leaves the child alone, so its request stays answerable; its result just
  no longer reaches the parent. (opencode keeps neither across a restart.)

## Storage

| Where | What | Lifetime |
|---|---|---|
| adk session (Valkey DB `REDIS_DB_SESSIONS`, `session:{thread}:*`) | the pending `adk_request_confirmation` call and `RequestedToolConfirmations` | 30 days |
| Valkey DB `REDIS_DB_HITL`: `hitl:{interruptId}` | the interrupt record: thread, run, reason, tool name, args (the questions for a question), message, status (`pending`, `resolved`, `cancelled`), decision, timestamps | 7 days |
| Valkey DB `REDIS_DB_HITL`: `hitl:thread:{thread}:pending` | the set of a thread's open interrupt ids | removed as each is decided or cancelled |
| Valkey DB `REDIS_DB_HITL`: `hitl:thread:{thread}:decided` | the ids answered while others are still open | taken when the last is answered; deleted on cancel |
| Valkey DB `REDIS_DB_RUNS`: `run:{id}:status`, `thread:{id}:run` | run status `awaiting_input`; the lease, kept as long as the record while paused | 7 days |
| Event log and OpenSearch message | the `RUN_FINISHED` interrupt outcome, the later `CUSTOM approval`, and the agent card's snapshots | as the thread |

adk's session is what actually resumes the run; the `hitl` records are the
queryable account of what was asked and how it was decided.

## Protocol

All of it travels on the one socket (`GET /v1/agent/chat?v=1`). Answering a
sub-agent's request from its card is the socket's first frame about the child
thread, so it checks access to that thread (ARCHITECTURE.md, "Thread access"):

| Frame | Direction | Purpose |
|---|---|---|
| `{type: "answer", requestId, threadId, interruptId, approved?, justification?, output?}` | client → server | answer one interrupt on that thread (a sub-agent's thread for a request on a card) |
| `{type: "cancel", requestId, threadId}` | client → server | cancel the thread's run, closing its interrupts |
| `{type: "ack", requestId, threadId, runId, seq}` | server → client | accepted; subscribe from `seq` to follow the continuation |
| `{type: "error", requestId, threadId, code, message}` | server → client | `not_paused` (not waiting, or already answered), `not_found`, `bad_request`, `internal` |

`GET /v1/threads/{id}/interrupts` lists a thread's pending interrupts for a
client that wants them without folding the stream.

## Code map

| File | Role |
|---|---|
| `internal/tools/write_note.go` | the demo approval tool (`RequireConfirmation`) |
| `internal/tools/builtin/question.go` | the question tool (`RequestConfirmation` with its questions) |
| `internal/tools/builtin/subagent.go` | sub-agent dispatch; `follow` puts a paused child's requests on the card |
| `internal/mcp/mcp.go` | MCP tools needing approval (`require_confirmation`) |
| `internal/stream/encode.go` | `call`, `interrupt`, `confirmation`: adk events → the AG-UI interrupt outcome |
| `internal/runs/worker.go` | records interrupts on pause, holds the lease for it |
| `internal/runs/answer.go` | `Answer`: validate, record, resume once all are answered |
| `internal/runs/cancel.go` | `Cancel`: closes interrupts and writes a cancelled segment |
| `internal/hitl/hitl.go` | the interrupt records |
| `internal/agents/resume.go` | answers → adk confirmation responses; `CUSTOM approval` |
| `internal/ws/conn.go` | the `answer` and `cancel` frames |
| agentui `lib/chat/reduce.ts`, `components/chat/messages/tool-line.tsx`, `question-card.tsx`, `agent-card.tsx` | the UI side; see AG-UI-RENDERING.md |

## Limits

- Sub-agents in the default roster cannot pause: they list their tools, and none
  of those need approval or ask questions. A sub-agent given such a tool or a
  connector pauses as described.
- Two clients answering the same interrupt at the same moment: one wins, the
  other gets `not_paused` and changes nothing. An answer and a cancel at the same
  moment: one wins too, by the same compare-and-set on the record and on the run's
  status, so an interrupt is never both answered and cancelled.
