# Improvement plan, round 3 (for approval)

Seventeen requested changes, grouped into four phases. Each item says what the
code does today, what changes, and how it is tested end to end. Questions that
need an answer before building are collected at the end, each with a
recommended default; "approved" means the defaults.

Research behind this plan (adk-go and AG-UI sources and docs, mTLS key formats
proven against a client-certificate server, the proxy and model registry,
open-slide's theme handling) was done by sub-agents and is cited inline where
it decides something.

---

## Phase A — connectors, transports, identity

### 1. MCP servers become a connector type

**Why it was split.** MCP servers were configured first, in `mcp.yaml`;
connectors came later as the user-facing grouping and pointed at servers by
name. Nothing needs two files.

**Change.** `mcp.yaml` goes away. A connector has a `type`: `tools` (a set of
toolx tools) or `mcp` (one MCP server, configured inline).

```yaml
connectors:
  - id: web
    name: Web
    type: tools
    tools: [webfetch]
  - id: slides
    name: Slides
    type: mcp
    mcp:
      endpoint: ${OPEN_SLIDE_MCP_URL}      # or command: [...]
      tools: [...]                         # optional filter
      require_confirmation: [...]
      thread_args: {deck_create: thread_id}
      ui: {deck_create: slides}
      artifacts: {deck_create: {id: deck_id, title: title, url: preview_url}}
    http: {forward: true}                  # the shared HTTP block (items 3, 4)
```

**adk-go confirms the shape.** `llmagent.Config.Toolsets []tool.Toolset`
(adk `agent/llmagent/llmagent.go:330`); `mcptoolset.New` returns a
`tool.Toolset` and its doc says to pass it in `Toolsets` (`tool/mcptoolset/set.go:35-51`);
the official Go MCP example does exactly that (adk-docs
`tools-custom/mcp-tools`). adk has no ready-made toolset for function tools, so
`toolx.Toolset` stays (the docs' own example is a hand-written toolset). Every
connector, of either type, reaches the agent as one `tool.Toolset`, which is
what happens today; only the configuration moves.

Also: give each MCP toolset its connector's name (adk's is always
`mcp_tool_set`, which makes errors unreadable), and reject at bind time any two
enabled connectors that expose the same tool name (adk fails the model call on
a duplicate, `toolutils.go:47`).

### 2. Tests

Two test files exist (about 100 lines). Only one earns its place:
`TestOnlyToolxBuildsFunctionTools` is how "every tool must use the wrapper" is
enforced. The transport header test and the citation-parsing test are removed;
both behaviours are covered by the end-to-end runs. No new unit tests: mTLS,
routing, compaction and the rest are proven with curl, a WebSocket client and
the browser against running services.

### 3. mTLS with encrypted client keys

**Today.** A provider names env vars holding a cert and key path; the key must
be unencrypted. An encrypted key fails with an ASN.1 error.

**Change.** Load the key with `github.com/youmark/pkcs8` (MIT, small: encrypted
PKCS#8/PBES2, PBKDF2-SHA1/SHA256 or scrypt, AES or 3DES — OpenSSL 3's default
output), falling back to the standard library's `x509.DecryptPEMBlock` for
legacy `Proc-Type: 4,ENCRYPTED` keys (what OpenSSL 1.x and LibreSSL write).
This is the same pairing the MongoDB Go driver uses. The decrypted key is
re-encoded and handed to `tls.X509KeyPair`, so the standard library still
checks that key and certificate match. A sub-agent proved it: RSA, EC and
Ed25519 keys in 11 encrypted variants got HTTP 200 from a server requiring
client certificates; wrong passwords and mismatched pairs fail clearly.

One TLS block, the same everywhere (providers, tool connectors, MCP
connectors), naming env vars:

```yaml
tls:
  cert_env: LITELLM_TLS_CERT                  # client certificate (PEM; intermediates may follow)
  key_env: LITELLM_TLS_KEY                    # private key (PEM, encrypted or not)
  key_password_env: LITELLM_TLS_KEY_PASSWORD  # holds the password itself
  ca_env: LITELLM_TLS_CA                      # trusted CA bundle (replaces system roots)
  insecure_skip_verify: false
```

Startup fails, naming the variable or file (never the password), when: a named
variable is unset; a cert has no key or a key no cert; a key is encrypted and no
password is configured; a password is configured for a plain key. The provider's
existing `ssl_*` fields are replaced by this block (models.yaml changes).

**Test.** A throwaway mTLS gateway (outside the repo) that requires a client
certificate and forwards to OpenRouter; a provider pointing at it; curl a chat
through the server with an encrypted PKCS#8 key and with a legacy key (200),
without the cert and with a wrong password (startup error).

### 4. Tool HTTP: user agent, the user's JWT, agent and tool names

**Today.** `webfetch` hard-codes `agentlite/1.0`; no JWT is captured anywhere;
thread headers are opt-in per connector.

**Change.**
- `user_agent` set in `connectors.yaml` (a top-level default, overridable per
  connector); the hard-coded one goes.
- `http.forward: true` on a connector (replacing `thread_headers`) sends the
  caller's identity with every request the connector's tools make:
  `Authorization: Bearer <the user's JWT>`, `X-Thread-ID`, `X-Root-Thread-ID`,
  `X-Run-ID`, `X-Agent-ID`, `X-Tool-Name`. Off by default, so nothing reaches
  the public web (`webfetch`). Works for MCP connectors too.
- The agent and tool name come from toolx: `toolx.Context` answers context
  lookups for the call, so any request made with it carries them.
- The JWT is taken from the request that queued each run segment (the thread
  POST, the socket's upgrade request for `run` and `answer` frames) and travels
  with that segment to the worker; a sub-agent's run uses its parent's. See
  questions Q1–Q2.

### 5. A connector concept

**Recommendation.** Keep connectors in YAML as the one place that defines a
system (name, icon, type, tools or MCP server, HTTP/TLS, user agent). In Go, a
tool's builder receives a `toolx.Connector` instead of a bare `*http.Client`:

```go
type Connector struct {
	ID, Name string
	HTTP     *http.Client // the connector's own transport
	DN       string       // see Q3
}
```

so a tool knows which system it belongs to, and `AddSource` can stamp the
connector's id as the source's system without the tool repeating it. No Go code
per connector. What the system DN is for decides where it goes (Q3).

---

## Phase B — models and the proxy

### 11. Types in `internal/proxy`

`openai-go`'s `Model` carries only id, created, owned_by; every field we use
(context length, architecture, supported parameters) would come out of raw
`ExtraFields`, which is worse than a struct. There is no maintained Go package
for LiteLLM's `/model/info`. So: move `Model`, the upstream response shapes, the
merge and the model-card loader into `pkg/modelcatalog` (no agentlite imports);
`config` keeps only the YAML override shape. Also fixes a bug found on the way:
vLLM's `max_model_len` is ignored, so vLLM models have no context length.

### 12. `embed.go` and `overlay`

`embed.go` exists because long-term memory embeds text server-side; there is
no incoming request to proxy, so a reverse proxy cannot replace it. It is
already the openai-go client. Fixes: build that client once per provider (today
it is built per call); `pkg/memory` gets an `Embedder` interface instead of
importing `internal/proxy`; it sends the upstream model id (item 14).

`overlay` moves to `dario.cat/mergo` with `WithOverride`: a set field in a
higher layer wins, field by field, as today. One difference: an empty value
can never clear a lower layer's value (today `capabilities: []` clears). That
is the trade for not hand-copying every field as model cards add more.

### 13. A generic pass-through

The route constants, `proxy.provider`, `proxy.overrides` and `PROXY_PROVIDER`
go. The handler lists pass-through paths, one line each:

```go
var passThrough = []string{"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/embeddings"}
```

Every one reads `model` from the JSON body, resolves it (item 14), rewrites it
to the upstream id, and forwards the request untouched otherwise to that
provider (one reverse proxy per provider). Adding `/v1/decisions` is one line.
A request without a `model` field (multipart uploads, files) gets a 400 (Q7).

### 14. Provider-prefixed model ids, strict routing

Every model id is `<provider>/<upstream id>`: `openrouter/openai/gpt-5-4` is
provider `openrouter`, upstream `openai/gpt-5-4` (split on the first `/`).
Resolution is a lookup in that provider's own list, nothing else: an unknown
prefix is `unknown provider`, a model the provider does not serve is
`unknown model`. No scan across providers, no fallback. (Today the first
provider that lists an id wins, so if LiteLLM discovery fails at start,
`openai/gpt-4.1-mini` silently goes to OpenRouter — the spill this removes.)

Everything that resolves a model goes through it: agent runs, compaction, the
proxy, memory embeddings, `default_model` and `memory.embedding_model`
(prefixed in models.yaml, checked at load), and `runs.Start` (an unknown model
is refused before the run is queued). The upstream id is what is sent to the
provider. Inside a provider block in models.yaml, models keep their upstream
ids; the prefix is added. Provider ids may not contain `/`. The UI keeps
treating ids as opaque; brand icons read a new `upstream_id` field.

Existing data: stored threads keep working (the model is chosen per turn). Runs
queued or paused for approval at deploy time with an unprefixed model fail with
`unknown provider`; drain them first.

### 15. Per-model overrides with discovery

Overrides already work field by field (context length included). Missing:
`base_url` and `api` per model. Added to the model override (with `${ENV}`
expansion); the resolved target carries them; agent runs, the proxy and
embeddings call that URL with the provider's HTTP client and key. A model with
different credentials should be its own provider (the provider's key and client
certificate are sent to the per-model URL).

### 16. Model cards

A separate GitHub repo of cards, checked out locally; `MODEL_CARDS_DIR` points
at it (empty: no cards). One card per model, Markdown with YAML frontmatter
(the same format as `agents/*.md`): frontmatter holds the model's fields plus
pricing, max output tokens, knowledge cutoff, release date, links, aliases; the
body is the long description the UI already renders. File path mirrors the
model id (Q5). Precedence, lowest to highest: card, discovery, models.yaml —
discovery reports what is actually deployed, the card fills gaps (pricing,
description), models.yaml has the last word. Cards reload on each discovery
tick; a malformed card is logged and skipped.

---

## Phase C — sources and artifacts

### 6. Sourcing in toolx; `[source:system:N]`?

**Why it is there.** A source must be recorded by the tool at the moment it
reads the data (that is what makes it deterministic), so the hook,
`AddSource`, is on the tool's context. Proposed tidy-up: `Source`, the citation
parser and the recorder interface move to `pkg/citations`; toolx keeps only
the hook.

**Citation format: keep `[source:N]`.** The model only copies a number it was
given; putting the system in the citation adds a token the model can get
wrong, and duplicates what the record already says. The UI already fetches the
conversation's sources once per thread (one cached call), so each chip looks up
its number and renders the system's icon and a short name inline, with no
per-chip request. With item 5 the system is the connector, whose icon is
already in connectors.yaml.

### 7. HTML artifacts

**Recommendation: drop the `html` kind.** Markdown covers documents, code
artifacts cover markup, slides have open-slide. HTML is rendered sandboxed with
scripts off and served with `Content-Security-Policy: sandbox`, but it is the
one kind that is model-written markup, for little gain (Q6).

### 8. Valkey out of sources and artifacts

**What Valkey does there today.** Two counters: a conversation's next source
number (plus key → number for de-duplication) and each artifact's next
version. Both move to OpenSearch, so every source and artifact datum sits
under document-level security:

- A `counters` document per conversation, incremented with a scripted update
  (`retry_on_conflict`), which is atomic per document.
- Sources: the document id is derived from the item's key (system + key), and
  it is created with `op_type=create`; a second add of the same item gets a
  conflict and reads the existing number. Two adds of the same new item at the
  same moment can leave a gap in the numbering (numbers are ids, not counts).
  Lookup by number becomes a search on `(root_id, number)`.
- Artifacts: the version comes from the same counters document; the record is
  written with external versioning (only moves forward), as today.

Valkey then holds only run lifecycle, sessions and HITL state.

### 9. An all-artifacts page

`GET /v1/artifacts?before=&limit=` lists artifacts across conversations, newest
first, with their thread's title. A new `/artifacts` page in the UI: a list
(title, kind, thread, updated), a kind filter, and the artifact opened in place
(the same views as the panel) with a link to its thread. Under document-level
security later, the list is naturally filtered.

---

## Phase D — UI

### 10. Light/dark in the open-slide embed

Open-slide reads only its own `localStorage.theme` and the OS preference; it has
no query parameter and no message listener, and its app comes from the npm
package `@open-slide/core`. The hook goes in **open-slide-mcp's Go proxy**
(your repo, `~/code/open-slide-mcp`), see Q4:

- `/preview/<deck>` keeps a `theme` parameter through its redirect.
- A small script is injected after `<head>` in HTML responses: it stores
  `?theme=` before first paint, and on a `postMessage` of
  `{type: "open-slide:theme", theme}` stores it and fires the `storage` event
  next-themes already listens to — so the editor switches without reloading.

agentui's embedded page adds `?theme=<current>` on load and posts each change.
Only the editor chrome follows the app; slides keep their authored colours.

### 17. Compaction in the UI

**Why it is invisible today.** adk deliberately does not yield its compaction
event to the caller (adk `runner.go:261`), and the token-threshold pass emits
nothing at all; both only append to the session.

**Change.**
- Wrap the session service the runner uses: when an event with
  `Actions.Compaction` is appended, emit to the run's stream. A wrapped
  summarizer emits the start. Both compaction kinds go through it.
- AG-UI has no compaction event, so the event is an `ACTIVITY_SNAPSHOT` of a new
  type `compaction` (one id per compaction): `running`, then `done` with the
  summary, the range it covers, and the summarizer's token usage. Activities
  are the AG-UI mechanism for UI-only state that persists with the message and
  is never sent back to the model.
- The UI renders a "Context compacted" divider where it happened, followed by a
  collapsible card styled like a tool call (`compact_context`, the summary as
  its output) — see Q8 on why not a real tool-call event.
- Fixes found on the way: a failure of the post-answer compaction currently
  marks an answered run as failed (it must log and continue); the summarizer
  gets a timeout (adk only bounds its own default one).

**Test.** A config with a low threshold, so compaction triggers in a couple of
turns: curl starts the thread, a WebSocket client and curl on `/messages` show
the activity live and stored, the browser shows the divider and the card.

---

## Order of work and testing

1. Phase A (1, 2, 3, 4, 5), then B (11–16), then C (6–9), then D (10, 17).
2. After each phase: end-to-end with curl, the WebSocket client and headless
   Chrome against the real services (OpenSearch, Valkey, MinIO, open-slide,
   OpenRouter), including the failure cases each item names.
3. Then code review by sub-agents (Go design and quality; React), fixes, and a
   full retest; repeat until clean. Docs updated in the same commits; commits on
   new branches in each repo; pushed only when you ask.

---

## Questions (recommended default first)

- **Q1. Where does the user's JWT come from?** (a) The `Authorization` header on
  `POST /v1/agent/chat` and on the socket's upgrade request, set by a gateway in
  front of the server (browsers cannot set headers on a WebSocket); or (b) a
  cookie; or (c) something else.
- **Q2. May the JWT be stored with a queued run segment?** Runs execute in
  workers through the Valkey queue, so the token has to travel with the run.
  Default: it is stored in the queue entry and run record with the run's TTL,
  and each segment uses the token of the request that queued it (a resumed run
  uses the answering request's). Alternative: keep tokens only in the process's
  memory (works only while workers run in the server process).
- **Q3. What is the system DN for?** (a) The expected subject DN of the
  system's server certificate, checked during the TLS handshake (pinning); (b)
  an identity sent to the system (a header); (c) data attached to each source
  from that system; (d) other.
- **Q4. May I change `~/code/open-slide-mcp`** (theme parameter and injected
  script, committed on a branch there)? Default: yes.
- **Q5. Model card naming.** Default: one card per upstream model, path
  mirroring its id (`openai/gpt-5-4.md`), shared by every provider that serves
  it, with optional `providers/<provider>/<id>.md` for provider-specific
  differences. Alternative: one card per full prefixed id
  (`openrouter/openai/gpt-5-4.md`). And: should the new GitHub repo
  (`sri-0/model-cards`) be private (default) or public, and seeded with cards
  for the models in models.yaml only (default)?
- **Q6. Drop the `html` artifact kind?** Default: yes.
- **Q7. Pass-through requests without a JSON `model`** (multipart audio, files).
  Default: not proxied (400) until needed; then an explicit
  `/v1/providers/{provider}/…` route.
- **Q8. Compaction "tool call".** Default: an activity rendered to look like a
  tool call. A real `TOOL_CALL_*` event would be stored as a call the model made
  and could be replayed to the model when a session is rebuilt; it can be done
  and filtered out of rebuilding if you prefer.
- **Q9. Encrypted key formats.** PKCS#8 (encrypted or not) and legacy PEM are
  covered. Do you also need PKCS#12 bundles (`.p12`/`.pfx`)? Default: no (one
  more small package, `software.sslmate.com/src/go-pkcs12`, if yes).
- **Q10. Env vars hold file paths** (the mounted cert files), as today, or the
  PEM content itself? Default: paths.
- **Q11. Go skills.** No Go review skills are installed in this session (the
  last review round found none either), so reviewers work from Effective Go and
  the Go Code Review Comments. If you have a skill to add, name it.
