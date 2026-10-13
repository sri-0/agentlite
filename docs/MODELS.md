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
list and nowhere else (`internal/providers/registry.go`):

| Case | Result |
|---|---|
| the prefix is no configured provider (or one skipped for having no `base_url`) | `unknown provider "<id>"` |
| the provider does not serve that model | `unknown model "<upstream>" for provider "<id>"` |

Two providers serving the same model are two different ids; a request names one
and goes there. A provider whose discovery fails keeps only its `models.yaml`
models, and a request for any other is refused rather than sent elsewhere. Runs
check the model before they are queued (`runs.Start`, a `400` on the thread POST),
`default_model` and `memory.embedding_model` must start with a provider id
(checked at start), and the pass-through refuses with a `400`.

## Where a model's description comes from

Each provider's list is built per model from three layers, lowest precedence
first, merged field by field with `dario.cat/mergo` (`modelcatalog.Merge`):

1. **its model card** (below), matched by upstream id, by an alias, or by the
   upstream id's last segment (for providers that drop the vendor prefix);
2. **what the provider reports** (discovery: OpenAI `/models`, which OpenRouter and
   vLLM extend, or LiteLLM `/model/info`), refreshed on `discover.refresh`;
3. **its `models.yaml` entry**, declared by upstream id.

A later layer replaces a field it sets and leaves the rest; it cannot clear a
value. Discovery outranks the card because it reports what is deployed (a vLLM
server's real context window); `models.yaml` has the last word.

`models.yaml` entries may also set, per model:

- `base_url`: send this model's requests to another URL (with `${ENV}` expansion).
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

`/v1/chat/completions`, `/v1/responses`, `/v1/messages` and `/v1/embeddings` are
forwarded as they are (`internal/handler/proxy.go` lists them; serving another
upstream endpoint is one more line there). `providers.Registry.Passthrough` reads
the body's `model`, resolves it, rewrites it to the upstream id, and forwards the
request to that provider (or the model's own `base_url`) with the provider's key,
streaming the response back as it arrives. A body without a `model` of the form
`<provider>/<model>` is refused with a `400`, so endpoints with no JSON model
(multipart uploads, files) are not proxied. Responses carry the upstream id in
their own `model` field.

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
| `internal/providers/passthrough.go` | the pass-through proxy |
| `internal/providers/embed.go` | embeddings for memory |
| `internal/config/models.go` | `models.yaml` and its checks |
