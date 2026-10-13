# agentlite docs

The reference for how agentlite works. `../AGENTS.md` is the short operating
manual (commands, layout, conventions); these documents explain the moving parts
in depth.

| Document | What it covers |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | Components, the life of a run, threads, streaming, and where every piece of state lives (Valkey databases and keys, OpenSearch indices). |
| [AGENT-ARCHITECTURE.md](AGENT-ARCHITECTURE.md) | The roster, the assistant as coordinator, the `subagent` tool and sub-agent threads, todos, connectors, models, the deep-research pipeline. |
| [MODELS.md](MODELS.md) | Model ids (`<provider>/<model>`), strict routing, how discovery, model cards and `models.yaml` merge, per-model overrides, the pass-through proxy, embeddings. |
| [TOOLS.md](TOOLS.md) | Building tools with `toolx`, the HTTP transports, sources and citations, artifacts and S3, tool UI renderers, the MCP tool hooks. |
| [HITL.md](HITL.md) | Approvals and questions: how a run pauses, how the answer resumes it, and how a sub-agent's request reaches the user and its answer reaches the sub-agent. |
| [AG-UI-RENDERING.md](AG-UI-RENDERING.md) | Every AG-UI event the backend emits, which code emits it, and what the UI's reducer and components make of it. |
| [MEMORY.md](MEMORY.md) | Long-term memory: what is stored, how it is searched, how to switch preload on, how to swap the implementation. |

Historical, kept for the reasoning behind decisions; they no longer describe the
code exactly: [PLAN.md](PLAN.md) (the original design, with a "Deviations, as
built" section), [PLAN-Sr-edits.md](PLAN-Sr-edits.md) (review comments on it),
[RESEARCH-streaming.md](RESEARCH-streaming.md) (the survey of opencode, T3 Code,
Codex and others).

## Keeping these current

These documents are part of the code. A change to a protocol, a config schema, the
roster, the run lifecycle, HITL, storage keys or the event mapping updates the
matching document in the same commit. When a document and the code disagree, the
code is right and the document is a bug.
