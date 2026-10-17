# Agents and sub-agents

How agentlite's agents are defined, how the assistant coordinates work, and how
sub-agents run. The model is opencode's and Claude Code's: one primary agent that
answers, uses tools, keeps a todo list, and delegates self-contained work to
sub-agents through a `subagent` tool. There is no classifier in front of it.

## Agents are configuration

Each agent is one markdown file in `config/default/agents/`: YAML frontmatter and
the instruction as the body. `config/agents.go` loads and validates them
at boot; `internal/agents/build.go` turns a definition into an adk agent for each
run. There is no Go per agent.

```yaml
id, name, description   # the description is what a parent reads when delegating
kind: llm | sequential  # default llm; sequential runs its subagents in order in one run
mode: primary | subagent | all   # primary: pickable; subagent: only via the subagent tool; all: both
tools: [...]            # omitted: harness tools + the thread's connectors; [] : none
subagents: [...]        # llm: who the subagent tool may run; sequential: the steps
max_output_tokens       # default: the model's own limit up to 32768, or 8192 when it states none
```

No agent names a model. A run uses the model the request names, or
`default_model` from `models.yaml`; that one model is used by every step, by the
compaction summarizer, and by every sub-agent the run starts.

## The roster

| Agent | Kind | Mode | Tools | Role |
|---|---|---|---|---|
| `assistant` | llm | primary (the default) | harness tools + connectors | answers, uses tools, keeps todos, delegates to the six below |
| `chat` | llm | primary | none | the plain model ("No agent" in the UI) |
| `deep-research` | sequential | all | its steps' | planner → researcher → report-writer, in one run |
| `data-analyst` | llm | all | `calculate`, `webfetch`, `report_progress` | numbers and comparisons with exact figures |
| `researcher` | llm | subagent | `webfetch`, `report_progress` | facts for one focused question, with sources |
| `evaluator` | llm | subagent | none | scores work against its goal, lists findings |
| `planner` | llm | subagent | none | a short concrete plan |
| `report-writer` | llm | subagent | `emit_artifact` | a polished deliverable, or an artifact |

## Tools

- **World tools** (`internal/tools`): `calculate`, `webfetch` (public hosts only; each
  page is a source), `write_note` (the approval demo; it only logs), `search_records`
  (the sources demo over sample records).
- **Harness tools** (`internal/tools/builtin`): `subagent`, `todowrite`, `question`,
  `emit_artifact` (stored; TOOLS.md), `report_progress`, `write_skill` (asks for
  approval; SKILLS.md).
- **Skills** (SKILLS.md): adk's skill toolset over the skills library (`list_skills`,
  `load_skill`, `load_skill_resource`), which lists the skills at the end of the system
  instruction. Agents with no `tools:` list get it; a list names it as `skills`. A
  skill's files are read as text; nothing runs its scripts.
- Every function tool is built with `pkg/toolx` (TOOLS.md): it gets the thread its call
  belongs to and records the sources it returns.
- **Connectors** (`config/default/connectors.yaml`): user-toggled groups of world
  tools or one MCP server each, every one an adk `tool.Toolset`
  (`internal/agents/connectors.go`). Switching one off removes all its tools. A
  connector of world tools is one system: its tools share one HTTP client, configured by
  its `http:` block.
- **MCP**: a connector of type `mcp` is one MCP server, an adk `mcptoolset`; a server
  that cannot list its tools costs a warning, not the run.

What an agent gets:

| Agent's `tools:` | It gets |
|---|---|
| listed | exactly those (harness, world or MCP names); validated at boot |
| omitted, user-started run | every harness tool (`subagent` only if it has subagents) + the toolsets of the thread's enabled connectors + `load_memory` when memory is on |
| omitted, sub-agent run | the same, minus `subagent`, `todowrite` and `question`: sub-agents do the work they are given; they do not delegate, keep lists or ask the user unless they list those tools |

## The assistant as coordinator

The assistant's instruction (`config/default/agents/assistant.md`) tells it to do
the work itself when it can, to delegate with `subagent` when a piece is
self-contained or when several pieces can run at once, to research before
building, to keep a todo list for anything with three or more steps, and to ask
the user only for decisions that are theirs. The runtime appends an
`<available_subagents>` block listing each sub-agent's description and tools, so
the assistant knows what each can do, and gives every agent with tools a
`<citations>` block: cite numbered tool results as `[source:N]` (TOOLS.md).

A typical turn: write the todo list; dispatch deep-research or researchers (in
parallel when independent); take their results; build the deliverable with its
own tools or connectors (for example the slides MCP); mark todos complete;
answer. All of it happens in one turn of one run.

## The subagent tool

`internal/tools/builtin/subagent.go`, named after opencode's. A call
`subagent(subagent_type, description, prompt)`:

1. checks the sub-agent is in the caller's `subagents`;
2. starts a **child run** on a new **child thread** (`parent_id` = the caller's
   thread, same root, same model and connectors) with the prompt as the only
   message: the child sees nothing of the parent's conversation;
3. emits `SUBAGENT_STARTED` and an `agent` activity card on the parent, then
   follows the child's event log, across the child's segments, to keep the card
   current: each tool call becomes a progress line (`report_progress` messages
   verbatim), a pause sets the card to `waiting` with the child's pending
   requests, and the child's next segment clears them;
4. waits for the child to end, and returns `{result: <the child's final text>}`
   to the parent's model; the card shows done or error.

While the child is paused the call just keeps waiting: the user answers the
request from the card (or the child's own thread) and the answer goes to the
child's thread (HITL.md). Several `subagent` calls in one turn run in parallel
(adk runs a turn's tool calls concurrently), and each child is a real run in the
request queue `runs:<subagent>`. Because children are separate runs on separate
threads:

- the child's full transcript, tools and reasoning live on the child thread,
  which the UI opens from the card; the parent's transcript carries only the card;
- the child survives as a record: it is persisted, listed under its parent, and
  deleted with it;
- cancelling the parent cancels its running or paused children.

Sub-agents of the default roster do not get `subagent`, so nesting is one level
deep, as in opencode by default. An agent could list `subagent` and its own
subagents to go deeper.

## Todos versus sub-agents

`subagent` and `todowrite` do different jobs. `subagent` runs work: it hands a
self-contained piece to another agent and returns that agent's result.
`todowrite` runs nothing: it is the coordinator's checklist, the plan of the
whole job and how far along it is, shown to the user in the task bar. One todo
can be done by the assistant itself, by one sub-agent, or by several in
parallel; the list is what tells the user which steps are pending, in progress
or done while those calls run. Claude Code keeps the same split (Task and
TodoWrite).

## The todo list

`todowrite` replaces the caller's whole list each call (`id`, `content`,
`status` of `pending | in_progress | completed | cancelled`; other statuses are
rejected). The encoder turns its result into a `task_list` activity, which the UI
shows in the task bar above the composer. Only the agent that writes the list
keeps it current; pipelines and sub-agents do not get `todowrite` by default.

## Workflow agents

`kind: sequential` runs its subagents in order inside one run and one adk session:
each step sees the conversation so far, so later steps build on earlier output.
`deep-research` is the one in the roster. In the UI each step is a stage card and
the last stage is the answer. A sequential agent can be used directly (picked in
the UI) or as a sub-agent through `subagent`; as a sub-agent its result is its last
step's final text.

## Context and memory

- **Compaction**: one global `compaction` block in `models.yaml` configures adk's
  session compaction (`runner.Config.Compaction`) for every run, summarised with
  the run's own model (`internal/agents/compaction.go`). The main strategy is tail
  retention: before a model call whose prompt passes `token_threshold` of the
  model's context window (from `GET /v1/models`; with no known window it is off),
  adk summarises every event but the newest `keep_events` into one summary event
  and sends the model that summary plus the tail. The stored AG-UI history is never
  changed; only the model's view of it shrinks. `interval`/`overlap` add adk's
  sliding window, which summarises every `interval` user turns after the answer.
  `summarizer_timeout` (default 60s) bounds each summary. adk does not yield its
  compaction event to the runner's caller, so the run wraps the summarizer (start,
  failure, decline) and the session service (the stored summary) and streams the
  outcome as a `compaction` activity: `running`, then `completed` with the summary
  and its token usage and duration, `failed` with the error, or `declined` (the
  summarizer had nothing to summarise, or adk discarded the summary because the
  session changed while it was written; the UI drops it). adk compacts between the
  events it yields, so a summary still unstored at the next event was discarded
  and is closed then. A failed summary leaves the run going with its full history
  (adk logs it and carries on); a run that ends mid-summary closes the activity as
  `failed`. adk's later correction of a stored record is not shown again.
- **Session**: each thread's adk session lives in Valkey and is rebuilt from the
  stored AG-UI events if it is missing.
- **Memory**: see MEMORY.md. User-started runs get `load_memory`; sub-agents work
  from their prompt alone.

## Building agents in Go

`cmd/dev` runs adk's own launcher (`make dev web | cli | api`) over the same
roster, sessions and memory, for adk's event and trace views. It runs its own
worker loops on the same queues, so it must run the same code as the server.
