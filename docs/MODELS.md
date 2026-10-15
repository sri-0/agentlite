# Models and providers

Which models exist, which provider serves each one, and how a request reaches it:
agent runs, the pass-through proxy and memory embeddings all resolve a model the
same way, and nothing ever falls back to another provider.

## Model ids

A model id is `<provider>/<model>`: the provider's id from `models.yaml`, then the
provider's own name for the model, its **upstream id**.

```
openrouter/openai/gpt-4.1-mini     provider openrouter, upstream openai/gpt-4.1-mini
litellm/claude-sonnet-4-5          provider litellm,    upstream claude-sonnet-4-5
```

The id is split at its first `/`. Provider ids cannot contain `/`. The upstream
id is what is sent to the provider.

## Strict routing

`providers.Registry.Resolve(id)` looks the model up in the named provider's own
list and nowhere else (`internal/providers/registry.go`). Every use resolves through
`ResolveAs(id, types...)`, which also refuses a model of another type
(`ErrWrongType`): the pass-through with its endpoint's types, agents with `llm`,
memory with `embedding`.

| Case | Result |
|---|---|
| the prefix is no configured provider (or one skipped for having no `base_url`) | `unknown provider "<id>"` |
| the provider does not serve that model | `unknown model "<upstream>" for provider "<id>"` |

Two providers serving the same model are two different ids; a request names one
and goes there. A provider whose discovery fails keeps its last successful listing
(only its `models.yaml` models until discovery first succeeds), and a request for
any other model is refused rather than sent elsewhere. Runs
check the model before they are queued (`runs.Start`, a `400` on the thread POST),
`default_model` and `memory.embedding_model` must start with a provider id
(checked at start), and the pass-through refuses with a `400`.

## Where a model's description comes from

Each provider's list is built per model from three layers, lowest precedence
first, merged field by field with `dario.cat/mergo` (`modelcatalog.Merge`):

1. **its model card** (below), matched by upstream id or alias, or else by last
   segment when one card alone ends with it (a provider's `gpt-4.1-mini` or
   `azure/gpt-4.1-mini` finds the card `openai/gpt-4.1-mini`; an exact id or alias
   always wins, and a segment two cards share finds neither: use `aliases`);
2. **what the provider reports** (discovery: OpenAI `/models`, which OpenRouter and
   vLLM extend, or LiteLLM `/model/info`), refreshed on `discover.refresh`;
3. **its `models.yaml` entry**, declared by upstream id.

A later layer replaces a field it sets and leaves the rest; it cannot clear a
value. Discovery outranks the card because it reports what is deployed (a vLLM
server's real context window); `models.yaml` has the last word.

## Model types

Every model has a `type`, which decides the endpoints it may be sent to and whether
agents can run on it (`modelcatalog.Type*`):

| Type | What | Endpoints |
|---|---|---|
| `llm` | chat, completions, responses | `/v1/chat/completions`, `/v1/responses`, `/v1/messages`; agent runs |
| `image` | image generation and editing | `/v1/chat/completions` (vLLM-Omni serves Qwen-Image and Qwen-Image-Edit there, returning images in `message.content`; OpenRouter generates images there) |
| `embedding` | embeddings | `/v1/embeddings`; memory |
| `audio` | speech and audio understanding (Voxtral: transcripts and questions about a recording) | `/v1/chat/completions`, with the audio in an `input_audio` part |
| `decision` | typed answers to questions | `/v1/decisions`, `/v1/systemone` |
| `rerank`, `transcription`, `speech` | relevance scores, speech to text, text to speech | none yet |

A type is taken from, in order, `models.yaml`, the provider's list, the model card;
with none of them saying, a model is an `llm`. What providers report:

- **OpenRouter** lists every kind of model only when asked (`/models?output_modalities=all`,
  as `models.yaml` does) and gives each its output modalities: one that outputs text is
  an `llm`, whatever else it outputs; `embeddings`, `rerank`, `image`, `transcription`,
  `speech` and `decisions` map to the types above.
- **LiteLLM** `/model/info` gives `mode`: `chat`, `completion`, `responses` → `llm`;
  `embedding`; `rerank`; `image_generation`, `image_edit` → `image`;
  `audio_transcription` → `transcription`; `audio_speech` → `speech`; `decisions` →
  `decision`. A deployment without a `mode` is an `llm`, as LiteLLM treats it.
- **vLLM** reports no type, so a vLLM model that is not an `llm` is declared in
  `models.yaml` (an id containing `embed` is taken for an `embedding`).

`audio` is never reported: a speech model that takes text and audio and answers in
text (Voxtral Small) looks like any chat model to OpenRouter (`text+audio->text`) and
LiteLLM (`chat`), and so do general models that also hear (GPT-4o audio, Gemini).
Declare a model used for voice as `audio` in `models.yaml`; agents and the UI's model
picker take `llm` models only, so it stays out of them.

A type no endpoint takes (OpenRouter's `video`, LiteLLM's `ocr`) is kept as reported
and refused everywhere. A run on a model that is not an `llm` is refused when it is
queued (`400`), and memory refuses an embedding model of another type.
`default_model` and `memory.embedding_model`, when already known at start, must be
an `llm` and an `embedding` model.

`models.yaml` entries, and only they, may also set where a model's requests go
(a card's `base_url` or `api` is ignored: cards come from another repository and are
shared by every provider). Per model:

- `base_url`: send this model's requests to another absolute http(s) URL (with
  `${ENV}` expansion; checked at start, and a variable that is unset is an error
  rather than a quiet fall back to the provider's URL).
  The provider's API key and client certificate go with them, so use it only
  within the same trust domain; a model with other credentials is its own
  provider.
- `api`: `chat_completions` or `responses` for this model.

```yaml
providers:
  - id: vllm
    base_url: ${VLLM_BASE_URL}
    api_key_env: VLLM_API_KEY
    discover: { endpoint: /models, refresh: 10m }
    models:
      - id: meta-llama/Llama-3.3-70B-Instruct
        context_length: 32768                       # smaller than the card's
      - id: qwen/qwen3-embedding
        type: embedding
        base_url: ${VLLM_EMBED_URL}                 # served by another vLLM
```

## Credentials

Every call to a provider carries that provider's credentials: its `api_key_env` key,
its headers and its client certificate. The pass-through drops the caller's
`Authorization`, which is the internal user's JWT, and sends the provider's key
instead, so a user's token never reaches a provider.

The development provider `openrouter` takes its base URL from `OPENROUTER_BASE_URL`;
unset (production) it is skipped like any provider without a base URL, so a model id
naming it is refused instead of reaching openrouter.ai. `default_model` and
`memory.embedding_model` must name a provider the environment has: the server
refuses to start otherwise, rather than refusing every run that names no model.

## Model cards

Offline context about each model for an environment with no internet: one
Markdown file per model in a separate repository (`github.com/sri-0/model-cards`),
checked out locally and named by `MODEL_CARDS_DIR`. Empty, there are no cards.

A card's path mirrors the model's OpenRouter id (`anthropic/claude-sonnet-4.5.md`),
so one card serves every provider of that model; `aliases` lists the names other
providers use. The frontmatter holds the model's fields (name, description,
context length, max output tokens, capabilities, architecture, pricing in US
dollars per million tokens, benchmarks, licence, release date, knowledge cutoff,
sources), and the body is notes on what the model is good at and its limits. The
cards are reread on every discovery pass, so a pulled card needs no restart; a
card that cannot be read is logged and skipped. `GET /v1/models` serves the merged
result, and the UI's model view shows it.

## The pass-through proxy

These endpoints are forwarded as they are, each for the model types it takes
(`internal/handler/proxy.go`; serving another upstream endpoint is one more line
there):

| Endpoint | API | Types |
|---|---|---|
| `/v1/chat/completions` | OpenAI | `llm`, `image`, `audio` |
| `/v1/responses` | OpenAI | `llm` |
| `/v1/messages` | Anthropic | `llm` |
| `/v1/embeddings` | OpenAI | `embedding` |
| `/v1/decisions` | OpenAI's decisions API (beta, Oct 2026) | `decision` |
| `/v1/systemone` | TypeSafe's System One API (also vLLM, LiteLLM, DigitalOcean) | `decision` |

The handler (`internal/handler/proxy.go`) reads the whole body (within the server's
body limit, `MAX_BODY_MB`, default 32: Echo's `BodyLimit`, a `413` above it) for its
`model`,
resolves it, checks its type, rewrites it to the upstream id, and forwards the
request to that provider (or the model's own `base_url`) with the provider's key
(`providers.Registry.Forward`), streaming the response back as it arrives. Refusals
are Echo errors (`{"message": ...}`); a provider that cannot be reached is a `502`. A body without a `model` of the form
`<provider>/<model>`, or a model of a type the endpoint does not take, is refused with
a `400` and never sent. Endpoints with no JSON model (multipart uploads: image edits,
transcriptions, files) are not proxied yet. The two decision APIs differ (`input` and
a list of questions, against `state` and questions by name); each is passed as it is
to a provider that serves it. Responses carry the upstream id in their own `model`
field.

## Embeddings

Long-term memory embeds text through `providers.Registry.Embedder(id)`, which
resolves the id on every call (so a model discovery adds after start works) and
calls the provider's `/embeddings` with the openai-go client built once per
provider. It is the server calling a provider itself, not the pass-through.

## Code map

| File | Role |
|---|---|
| `pkg/modelcatalog/model.go` | `Model`, `Merge` |
| `pkg/modelcatalog/discover.go` | parsers for provider model lists |
| `pkg/modelcatalog/cards.go` | the model card loader and lookup |
| `internal/providers/client.go` | one client per provider (pkg/transport, TLS) |
| `internal/providers/registry.go` | per-provider model lists, `Resolve`, discovery |
| `internal/handler/proxy.go` | the pass-through endpoints, their model types, the checks |
| `internal/providers/passthrough.go` | forwarding a pass-through request to its provider |
| `internal/providers/embed.go` | embeddings for memory |
| `internal/config/models.go` | `models.yaml` and its checks |
