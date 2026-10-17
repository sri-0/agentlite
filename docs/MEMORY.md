# Long-term memory

How agentlite remembers across threads, what it costs, and how to swap it.

## What it is

adk defines long-term memory as one interface, `memory.Service`, with two
methods: `AddSessionToMemory(ctx, session)` stores a finished conversation, and
`SearchMemory(ctx, {AppName, UserID, Query})` returns the entries that match a
query. adk ships the tools that use it; agentlite ships one implementation of
the interface, `pkg/memory`, on OpenSearch.

There is no identity yet, so every memory belongs to the one adk user
`default`; memories are not scoped by user until identity exists.

## What gets stored, and when

After a **user-started** run finishes with status done (not a sub-agent's run,
not a paused one), the runtime calls `AddSessionToMemory` with the thread's adk
session in a detached goroutine, so storing never delays the answer
(`internal/agents/runtime.go`, `remember`).

`pkg/memory.Service.AddSessionToMemory` takes the session's latest turn (the
last user message and everything after it) and, for each text event in it
(user or model; tool calls and reasoning are skipped), embeds the text with the
configured embedding model and indexes one document in the OpenSearch
`memories` index:

```
{ id, thread_id, author, text, embedding (kNN vector), created_at }
```

The document id is the event's id (a UUIDv7), so re-storing a session
is idempotent. Embedding goes through the provider's `/embeddings` endpoint
with a 15 s timeout.

## How it is searched

A search embeds the query with the same model and runs a kNN query (lucene
HNSW, cosine) for the five nearest entries, **excluding the asking thread's own
turns**, which the model already has in its context. The exclusion is why the
runtime hands each run a per-thread view: `Memory.ForThread(threadID)` returns
an adk `memory.Service` whose searches carry that filter.

Search is fail-soft: if embedding or OpenSearch fails, the turn proceeds with
no memories and a warning is logged. Memory is an enhancement, never a
dependency of answering.

## How agents use it

Two adk tools, added only to **user-started** runs of agents (sub-agents work
from their prompt alone):

- `load_memory` — a function tool the model calls with a query when it decides
  it needs to recall something. Always on when memory is configured.
- `preload_memory` — a request processor that searches with the user's latest
  message before **every** model call of the turn and injects the hits into the
  system instruction. On only when `memory.preload: true` in `models.yaml`.

Preload is off by default because of its cost: one embedding round trip plus a
kNN query per model call, about a second each on a hosted embedding model, and
a turn with tool calls makes several model calls. The `memory.search` span in
Jaeger shows it. With preload off, recall costs nothing until the agent asks.

## Configuration

```yaml
# models.yaml
memory:
  embedding_model: openai/text-embedding-3-small   # must be served by a provider
  preload: false
```

Omit the block and there is no memory: no tools, no storing. The `memories` index
is created with the others (`make indices`, ARCHITECTURE.md) for 1536-dimension
vectors (`deploy/opensearch/memories.json`); a model with another size needs that
file changed and the index recreated.

## Swapping it

The runtime depends on one small interface (`internal/agents`):

```go
type Memory interface {
	ForThread(threadID string) adkmemory.Service
}
```

Anything that returns an adk `memory.Service` per thread can stand in: adk's
in-memory service for tests, a Vertex memory bank, another vector store. Wire it
in `internal/bootstrap`, where `pkg/memory` is built today. The tools and the
storing hook do not change.

## Known limits

- Entries are never deleted except with their thread (`DELETE /v1/threads/{id}`
  removes the thread family's memories).
- Every text turn is stored verbatim; there is no summarisation or
  deduplication across threads, so a fact repeated in many threads is stored
  many times and recalled many times.
- No user scoping until identity exists.
