# agentlite

Go agent server: OpenAI-style endpoints reverse-proxied to your gateway, plus agents on
adk-go v2 streamed over one WebSocket as AG-UI events. See `AGENTS.md` to operate it and
`docs/PLAN.md` for the design.

```bash
(cd ../agentlite-infra && make up) && make indices && cp .env.example .env.local && make run
(cd ../agentlite-infra && make gateway)   # :8000: signs in as DEV_USER and forwards to the server (docs/AUTH.md)
```
