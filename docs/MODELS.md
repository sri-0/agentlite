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
`default_model` and `embedding.model` must start with a provider id
(checked at start), and the pass-through refuses with a `400`.

A thread can change model between turns. The thread record keeps the last turn's
model; a turn that runs on another one starts with a `model` activity (`{from, to}`,
stored with the turn), which the UI shows as "Changed model to …"
(AG-UI-RENDERING.md). Only a turn that ran marks a change: picking a model in the UI
does not.

## Where a model's description comes from

Each provider's list is built per model from three layers, lowest precedence
first, merged field by field with `dario.cat/mergo` (`modelcatalog.Merge`):

1. **its model card** (below), matched by upstream id or alias, or else by last
   segment when one card alone ends with it (a provider's `gpt-4.1-mini` or
   `azure/gpt-4.1-mini` finds the card `openai/gpt-4.1-mini`; an exact id or alias
   always wins, and a segment two cards share finds neither: use `aliases`);
2. **what the provider reports** (discovery: OpenAI `/models`, which OpenRouter and
   vLLM extend, or LiteLLM `/model/info`, whose one entry per deployment counts once):
   listed once during the start, every provider at once, then refreshed in the
   background on `discover.refresh` (checked every 10 minutes). Each refresh also rereads
   the model cards, so a pulled card reaches every provider, discovered or not. A failed
   listing keeps the last one. Requests (`/v1/models` among them) read the list held in
   memory and never call a provider;
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
| `image` | image generation and editing | `images.model` (the `generate_image` tool's default; a turn's `imageModel` picks another; made on `/images/generations`; edited by `images.edit_model` on `/images/edits`, or without it there with the original in OpenRouter's `input_references`; TOOLS.md); `/v1/images/generations` and `/v1/images/edits` passed through; `/v1/chat/completions` (vLLM-Omni serves Qwen-Image and Qwen-Image-Edit there, returning images in `message.content`; OpenRouter generates images there) |
| `embedding` | embeddings | `/v1/embeddings`; `embedding.model`: memory and the files (FILES.md) |
| `audio` | speech and audio understanding (Voxtral: transcripts and questions about a recording) | `/v1/chat/completions`, with the audio in an `input_audio` part |
| `decision` | typed answers to questions | `/v1/decisions`, `/v1/systemone` |
| `transcription` | speech to text (Voxtral's speech-to-text route, Whisper) | the voice prompt, `POST /v1/voice/transcriptions` |
| `rerank`, `speech` | relevance scores, text to speech | none yet |

A type is taken from, in order, `models.yaml`, the provider's list, the model card;
with none of them saying, a model is an `llm`. What providers report:

- **LiteLLM** `/model/info` gives `mode`: `chat`, `completion`, `responses` → `llm`;
  `embedding`; `rerank`; `image_generation`, `image_edit` → `image`;
  `audio_transcription` → `transcription`; `audio_speech` → `speech`; `decisions` →
  `decision`. A deployment without a `mode` is an `llm`, as LiteLLM treats it.
- **vLLM** reports no type. OpenRouter (development only) lists text models by
  default.

Declare `type` in `models.yaml` for any model that is not an `llm` or whose reported
type is wrong; the entry overrides discovery.

`audio` is never reported: a speech model that takes text and audio and answers in
text (Voxtral Small) looks like any chat model to OpenRouter (`text+audio->text`) and
LiteLLM (`chat`), and so do general models that also hear (GPT-4o audio, Gemini).
Declare a model used for voice as `audio` in `models.yaml`; agents and the UI's model
picker take `llm` models only, so it stays out of them.

A type no endpoint takes (LiteLLM's `video_generation` or `ocr`) is kept as reported
and refused everywhere. A run on a model that is not an `llm` is refused when it is
queued (`400`), and memory refuses an embedding model of another type.
`default_model`, `embedding.model`, `voice.model`, `images.model` and `images.edit_model`
must be in the list after the start's first discovery, declared in models.yaml or listed by
their provider, as an `llm`, an `embedding`, a `transcription` and `image` models: the
server refuses to start otherwise. Declaring them keeps them
listed even when their provider is down at the start.

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
its headers and its client certificate. The pass-through drops the caller's sensitive
headers (`transport.Env.Forward`, as the MCP proxy does: `TRANSPORT_SENSITIVE_HEADERS`, by
default `Authorization`, which is the internal user's JWT, `Cookie`, `X-Api-Key` and
`X-Forwarded-Client-Cert`) and sends the provider's key instead, so a user's token never
reaches a provider.

The development provider `openrouter` takes its base URL from `OPENROUTER_BASE_URL`;
unset (production) it is skipped like any provider without a base URL, so a model id
naming it is refused instead of reaching openrouter.ai. `default_model` and
`embedding.model` must name a provider the environment has: the server
refuses to start otherwise, rather than refusing every run that names no model.

## Model cards

Offline context about each model for an environment with no internet: one
Markdown file per model in a separate repository (`github.com/sri-0/model-cards`),
checked out locally and named by `MODEL_CARDS_DIR`. Empty, there are no cards.

A card's path mirrors the model's OpenRouter id (`anthropic/claude-sonnet-4.5.md`),
so one card serves every provider of that model; `aliases` lists the names other
providers use. The frontmatter holds the model's fields (name, description,
context length, max output tokens, capabilities (`tool_calling`, `reasoning`, `vision`
for images and `files` for PDFs as input, two separate things), architecture, pricing in US
dollars per million tokens, benchmarks, licence, release date, knowledge cutoff,
sources), and the body is notes on what the model is good at and its limits. The
cards are reread every refresh (10 minutes), so a pulled card needs no restart; a
card that cannot be read is logged and skipped. `GET /v1/models` serves the merged
result, and the UI's model view shows it.

## The pass-through proxy

These endpoints are forwarded as they are, each for the model types it takes
(`internal/handler/passthrough.go`; serving another upstream endpoint is one more line
there):

| Endpoint | API | Types |
|---|---|---|
| `/v1/chat/completions` | OpenAI | `llm`, `image`, `audio` |
| `/v1/responses` | OpenAI | `llm` |
| `/v1/messages` | Anthropic | `llm` |
| `/v1/embeddings` | OpenAI | `embedding` |
| `/v1/images/generations` | OpenAI (vLLM-Omni) | `image` |
| `/v1/images/edits` | OpenAI, a multipart form (vLLM-Omni) | `image` |
| `/v1/decisions` | OpenAI's decisions API (beta, Oct 2026) | `decision` |
| `/v1/systemone` | TypeSafe's System One API (also vLLM, LiteLLM, DigitalOcean) | `decision` |

The handler (`internal/handler/passthrough.go`) reads the whole body (within the server's
body limit, `MAX_BODY_MB`, default 32: Echo's `BodyLimit`, a `413` above it) for its
`model`: a JSON object's, or a multipart form's `model` field (an image edit's upload,
whose other parts pass as they came, under the same boundary). It
resolves it, checks its type, rewrites it to the upstream id, and forwards the
request to that provider (or the model's own `base_url`) with the provider's key
(`providers.Registry.Forward`), streaming the response back as it arrives. Refusals
are Echo errors (`{"message": ...}`); a provider that cannot be reached is a `502`. A body without a `model` of the form
`<provider>/<model>`, or a model of a type the endpoint does not take, is refused with
a `400` and never sent. vLLM-Omni refuses a `model` that is not the one it serves, so
the upstream id must be its served name. The two decision APIs differ (`input` and
a list of questions, against `state` and questions by name); each is passed as it is
to a provider that serves it. Responses carry the upstream id in their own `model`
field.

## Embeddings

One model embeds for long-term memory and the files: `models.yaml`'s `embedding` block
(`model`, `dimensions`, `query_prefix`, `document_prefix`), behind
`providers.Registry.Embedder` (`internal/providers/embed.go`), which resolves the model
on every call (so one discovery adds after start works). It is the server calling a
provider itself, not the pass-through, and it speaks the OpenAI embeddings API with
openai-go's typed client and nothing per provider:

- texts: `input: [texts]`, `encoding_format: "float"`, and `dimensions` when the block
  sets it (0 leaves it out, for a model whose vectors are the index's size anyway: vLLM
  refuses `dimensions` for EmbeddingGemma 2, vllm#60977);
- an image, audio or video: the API takes only text, so `input` is one chat message
  holding the file as a content part, `[{role: "user", content: [part]}]`, the rest of
  the request as typed. vLLM (main since 2026-06) and OpenRouter's Gemini Embedding 2
  take that. The part is OpenAI's `image_url` or `input_audio`; video, which OpenAI has
  no part for, vLLM's `video_url`.

The prefixes are for a model trained with them (EmbeddingGemma 2: `task: search result |
query: ` and `title: none | text: `); empty, nothing is added. The memory and file
chunk indexes are both sized for 768 dimensions, so the model's vectors must be that
size, and changing the model means re-embedding both.

## Voice

The voice prompt (`internal/voice`) is apart from the agents. The composer records,
converts the recording to 16 kHz mono WAV (what every Voxtral route accepts; browsers
record WebM), and posts it to `POST /v1/voice/transcriptions` as a multipart `file`
(25 MiB at most). The server sends it to `voice.model`'s `/audio/transcriptions`, the
OpenAI route OpenRouter, vLLM and LiteLLM all serve, and replies `{text}`. 25 MiB of
that WAV is about 13 minutes. The text
goes into the composer, and is sent as if typed. Without a `voice` block in
`models.yaml` the route is not registered.

On OpenRouter use `mistralai/voxtral-small-24b-2507-stt`: through that route
`voxtral-mini-3b-2507` answers the recording instead of transcribing it. vLLM serves
Voxtral's weights on `/v1/audio/transcriptions` with its transcription prompt.

## Disconnected deployment

The server itself fetches nothing from the internet: every model call goes to a
provider in `models.yaml`. `openrouter` is for development only: without
`OPENROUTER_BASE_URL` it is skipped, and a model id naming it is refused, never sent.
`config/default/models.yaml` names OpenRouter models for every feature, so a disconnected deployment brings its own config
(`CONFIG_DIR`) that names local models. Run them on `vllm`, or `litellm` in front of
local servers, and declare the non-LLM models under the provider's `models:` with their
`type` (discovery reads `/models`, which doesn't say what a model is):

| Feature | `models.yaml` | Local server |
|---|---|---|
| Agents and chat | `default_model: vllm/<llm>` | vLLM, chat completions (or `/v1/responses` through LiteLLM) |
| Embeddings: long-term memory and file search | `embedding: {model: vllm/google/embeddinggemma-2, dimensions: 0, query_prefix: "task: search result \| query: ", document_prefix: "title: none \| text: "}` (`type: embedding`) | vLLM with EmbeddingGemma 2 support (main since 2026-10-06, so a nightly until the next release): `--runner pooling --dtype bfloat16`; text, images, audio and video. Its vectors are 768, the size of the `memories` and `userfile_chunks` indexes. Avoid sm_86 and sm_120 GPUs until vllm#60847 is fixed |
| Voice prompt | `voice.model: vllm/mistralai/Voxtral-Small-24B-2507` (`type: transcription`) | vLLM serves Voxtral on `/v1/audio/transcriptions` |
| Image generation and edits | `images: {model: vllm/Qwen/Qwen-Image-2.1, edit_model: vllm/Qwen/Qwen-Image-2.1}` (`type: image`, each with the `base_url` of its server) | vLLM-Omni (`vllm serve <model> --omni`, one model per server): makes on `/v1/images/generations`, edits on `/v1/images/edits`. Qwen-Image 2.1 does both; earlier Qwen-Image models edit with Qwen-Image-Edit on a second server, its `edit_model` (TOOLS.md) |
| Model descriptions | `MODEL_CARDS_DIR`: a checkout of the model-cards repository | none |

Leave out a feature's block and the feature is off: no voice route, no
`generate_image`, files kept but not searchable, no memory.

Also check, outside `models.yaml`:
- **Connectors:** the `web` connector (`webfetch`) reads public web pages. Drop it from
  `connectors.yaml`, or limit it to internal sites. MCP connectors (`slides`:
  `OPEN_SLIDE_MCP_URL`) must point at servers inside the network.
- **Tracing:** exported only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set (to the local
  Jaeger, say).
- **Building and running:** Go modules (an internal `GOPROXY`, or `go mod vendor`), the
  container images in `docker-compose.yaml` (OpenSearch, Valkey, MinIO, Jaeger) from an
  internal registry, and the UI's npm packages. The UI's fonts are an npm package
  (sri-0/agentui#28), not Google Fonts.

## Code map

| File | Role |
|---|---|
| `pkg/modelcatalog/model.go` | `Model`, `Merge` |
| `pkg/modelcatalog/discover.go` | parsers for provider model lists |
| `pkg/modelcatalog/cards.go` | the model card loader and lookup |
| `internal/providers/client.go` | one client per provider (pkg/transport, TLS) |
| `internal/providers/registry.go` | per-provider model lists, `Resolve`, discovery |
| `internal/handler/passthrough.go` | the pass-through endpoints, their model types, the checks |
| `internal/providers/reverseproxy.go` | forwarding a pass-through request to its provider |
| `internal/providers/embed.go` | the one embedder: memory's, and the uploaded files' (text, images, audio, video) |
| `config/models.go` | `models.yaml` and its checks |
| `internal/voice/voice.go` | the voice prompt: a recording to text |
