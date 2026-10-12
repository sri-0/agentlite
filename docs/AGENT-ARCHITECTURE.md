# Agents and sub-agents

How agentlite's agents are defined, how the assistant coordinates work, and how
sub-agents run. The model is opencode's and Claude Code's: one primary agent that
answers, uses tools, keeps a todo list, and delegates self-contained work to
sub-agents through a `task` tool. There is no classifier in front of it.

## Agents are configuration

Each agent is one markdown file in `config/default/agents/`: YAML frontmatter and
the instruction as the body. `internal/config/agents.go` loads and validates them
at boot; `internal/engine/build.go` turns a definition into an adk agent for each
run. There is no Go per agent.

```yaml
id, name, description   # the description is what a parent reads when delegating
kind: llm | sequential  # default llm; sequential runs its subagents in order in one run
mode: primary | subagent | all   # primary: pickable; subagent: only via task; all: both
tools: [...]            # omitted: harness tools + the thread's connectors; [] : none
subagents: [...]        # llm: who task may dispatch; sequential: the steps
max_output_tokens
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
| `data-analyst` | llm | all | `calculate`, `read_url`, `report_progress` | numbers and comparisons with exact figures |
| `researcher` | llm | subagent | `read_url`, `report_progress` | facts for one focused question, with sources |
| `evaluator` | llm | subagent | none | scores work against its goal, lists findings |
| `planner` | llm | subagent | none | a short concrete plan |
| `report-writer` | llm | subagent | `emit_artifact` | a polished deliverable, or an artifact |

## Tools

- **World tools** (`internal/tools`): `calculate`, `read_url` (public hosts only),
  `write_note` (the approval demo; it only logs).
- **Harness tools** (`internal/tools/builtin`): `task`, `todowrite`, `question`,
  `emit_artifact`, `report_progress`.
- **Connectors** (`config/default/connectors.yaml`): user-toggled groups of world
  tools or one MCP server each, every one an adk `tool.Toolset`
  (`internal/engine/connectors.go`). Switching one off removes all its tools.
- **MCP** (`config/default/mcp.yaml`): servers become adk `mcptoolset`s; a server
  that cannot list its tools costs a warning, not the run.

What an agent gets:

| Agent's `tools:` | It gets |
|---|---|
| listed | exactly those (harness, world or MCP names); validated at boot |
| omitted, user-started run | every harness tool (`task` only if it has subagents) + the toolsets of the thread's enabled connectors + `load_memory` when memory is on |
| omitted, sub-agent run | the same, minus `task`, `todowrite` and `question`: sub-agents do the work they are given; they do not delegate, keep lists or ask the user unless they list those tools |

## The assistant as coordinator

The assistant's instruction (`config/default/agents/assistant.md`) tells it to do
the work itself when it can, to delegate with `task` when a piece is
self-contained or when several pieces can run at once, to research before
building, to keep a todo list for anything with three or more steps, and to ask
the user only for decisions that are theirs. The engine appends an
`<available_subagents>` block listing each sub-agent's description and tools, so
the assistant knows what each can do.

A typical turn: write the todo list; dispatch deep-research or researchers (in
parallel when independent); take their results; build the deliverable with its
own tools or connectors (for example the slides MCP); mark todos complete;
answer. All of it happens in one turn of one run.

## The task tool

`internal/tools/builtin/task.go`. A call `task(subagent_type, description,
prompt)`:

1. checks the sub-agent is in the caller's `subagents`;
2. starts a **child run** on a new **child thread** (`parent_id` = the caller's
   thread, same root, same model and connectors) with the prompt as the only
   message: the child sees nothing of the parent's conversation;
3. emits `SUBAGENT_STARTED` and an `agent` activity card on the parent, then tails
   the child's event log to add progress lines to the card (each tool call, and
   `report_progress` messages verbatim);
4. waits for the child to finish, and returns `{result: <the child's final
   text>}` to the parent's model; the card shows done or error.

Several `task` calls in one turn run in parallel (adk runs a turn's tool calls
concurrently), and each child is a real run in the request queue
`runs:<subagent>`. Because children are separate runs on separate threads:

- the child's full transcript, tools and reasoning live on the child thread,
  which the UI opens from the card; the parent's transcript carries only the card;
- the child survives as a record: it is persisted, listed under its parent, and
  deleted with it;
- a child can pause for an approval or a question; the request is forwarded to the
  parent and the answer relayed back (see HITL.md);
- cancelling the parent cancels its running or paused children.

Sub-agents of the default roster do not have `task`, so nesting is one level
deep. An agent could list `task` and its own subagents to go deeper.

## Todos

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
the UI) or as a sub-agent through `task`; as a sub-agent its result is its last
step's final text.

## Context and memory

- **Compaction**: one global block in `models.yaml` (`interval`, `overlap`,
  `token_threshold`) configures adk's session compaction for every run, summarised
  with the run's own model.
- **Session**: each thread's adk session lives in Valkey and is rebuilt from the
  stored AG-UI events if it is missing.
- **Memory**: see MEMORY.md. User-started runs get `load_memory`; sub-agents work
  from their prompt alone.

## Building agents in Go

`cmd/dev` runs adk's own launcher (`make dev web | cli | api`) over the same
roster, sessions and memory, for adk's event and trace views. It runs its own
worker loops on the same queues, so it must run the same code as the server.
