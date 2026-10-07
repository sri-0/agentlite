# agentlite

Go agent server: OpenAI-style endpoints reverse-proxied to your gateway, plus agents on
adk-go v2 streamed over one WebSocket as AG-UI events. See `AGENTS.md` to operate it and
`docs/PLAN.md` for the design.

```bash
make up && cp .env.example .env.local && make run
```
