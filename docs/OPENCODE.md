# opencode

`opencode auth login https://<server>` points [opencode](https://opencode.ai) at
agentlite. It then gets this server's models, its exposed MCP servers and its skills
library, with no configuration of its own. opencode reads `GET /.well-known/opencode`
at login, and again on every start (`internal/handler/wellknown/opencode.go`, its types in
`pkg/opencode`). With `default_model: openrouter/openai/gpt-4.1-mini` in models.yaml:

```json
{
  "auth": {"command": ["true"], "env": "AGENTLITE_TOKEN"},
  "config": {
    "model": "agentlite/openrouter/openai/gpt-4.1-mini",
    "provider": {
      "agentlite": {
        "npm": "@ai-sdk/openai-compatible",
        "name": "agentlite",
        "options": {"baseURL": "https://<server>/v1"},
        "models": {"openrouter/openai/gpt-4.1-mini": {"name": "GPT-4.1 Mini", "tool_call": true, "attachment": true}}
      }
    },
    "mcp": {"slides": {"type": "remote", "url": "https://<server>/mcp/slides", "oauth": false}},
    "skills": {"urls": ["https://<server>/.well-known/agent-skills/"]}
  }
}
```

opencode puts `config` under the user's own configuration, so a user's settings win.
The URLs start with `APP_URL` (default `http://localhost:8000`): this server as clients
reach it, through whatever gateway is in front. Every route needs the user's access
token, which that gateway attaches, and every POST a control set in `X-Control-Set`
(AUTH.md). opencode sends none: for it, exempt the pass-through and the MCP proxy
with `AUTH_CONTROLSET_EXEMPT_PATHS=/v1/chat/completions,/mcp/:id` (the control set
matters only to POSTs that create documents, which these are not).

## Login

Clients prove who they are with their certificate at the gateway (mutual TLS), so there
is no token. opencode still needs a login command, whose output it keeps as a token in
`env`; `true` succeeds and prints nothing. The command runs on the user's machine, so it
must exist there (macOS and Linux have `true`).

## Models

- **The provider** is named after the app (`APP_NAME`, default `agentlite`). It is the
  pass-through (MODELS.md) as an OpenAI-compatible provider, so a model is
  `agentlite/<provider>/<model>`.
- **Its models** are every `llm` model of the registry, as `/v1/models` lists them:
  models.yaml's providers, with the models their discovery finds.
  `reasoning` and `attachment` (vision) are set when the model's capabilities say so.
  `tool_call` is set when its capabilities are known, so a model without tool calls
  says so; opencode assumes it has them otherwise. `limit` is set only when both the
  context window and the maximum output are known, since opencode needs both.
- **`model`**, opencode's default, is models.yaml's `default_model`, which the start
  checks is in the list (MODELS.md): opencode refuses a default its provider does not
  list.

## MCP servers

An MCP connector with `mcp.expose: true` is published at `/mcp/{id}`
(`mcp.Server.Forward`, `internal/mcp/proxy.go`). It is `transport.Env.Forward`
(`pkg/transport`, `httputil.ReverseProxy`; the one place requests are forwarded, which
the provider pass-through uses too) over
the connector's own HTTP client, so requests carry the connector's headers and client
certificate, to the connector's endpoint as configured. Streamed responses pass through
as they come. A server that cannot be reached is a `502`, through the server's error
handler. It publishes the server as it is:

- every tool, whatever `mcp.tools` allows the agents;
- no harness hooks: no `require_confirmation`, `thread_args`, `ui` or `artifacts`; tool
  approval is whatever opencode's own `permission` settings say;
- the client's sensitive headers dropped, like every forward's (`transport.Env.Forward`,
  which the model pass-through uses too): `TRANSPORT_SENSITIVE_HEADERS`, by default
  `Authorization`, `Cookie`, `X-Api-Key` and `X-Forwarded-Client-Cert`. The client's
  `Authorization`, the user's, goes on only when the connector forwards the user's
  identity (`http.forward`, TOOLS.md). The client's other headers go through, run
  headers (`X-Thread-ID`, …) among them.

Only a server reached over HTTP can be exposed: a connector with a `command` (stdio) and
`expose` fails the start. Any other id is a `404`.

## Skills

`skills.urls` points opencode at the skills library's discovery index (SKILLS.md), the
same one `npx skills` reads. opencode downloads every skill into its cache. An entry's
`version`, the skill's `updated_at`, lets opencode releases that read it download a
changed skill again. opencode 1.15.1 does not read it, and keeps its first download until
its cache (`~/.cache/opencode/skills`) is cleared.

## Trying it

```bash
opencode auth login http://localhost:8000
opencode models agentlite
opencode mcp list
opencode run -m agentlite/openrouter/openai/gpt-4.1-mini "Use the report-style skill: ..."
```

## Code map

| File | Role |
|---|---|
| `internal/handler/wellknown/opencode.go` | `/.well-known/opencode` |
| `pkg/opencode` | the manifest's types; what a model's card tells opencode |
| `internal/handler/connectors.go` | the `/mcp/{id}` route |
| `internal/mcp/proxy.go` | the MCP proxy (`Server.Forward`) |
| `pkg/transport/forward.go` | the reverse proxy it and the pass-through share |
| `internal/agents/connectors.go` | `Runtime.ExposedMCP` |
| `config/connectors.go` | `mcp.expose` |
