# Auth and document-level security

Who a request is made by, what they may read and write, and how the server
keeps a conversation to its own control set. There is no service identity:
every OpenSearch call carries a user's token, the caller's own or a thread
token for them, and OpenSearch's document-level security (DLS) limits it to the
control sets the token names. The server has no OpenSearch credentials.

The local stack lives in `~/code/agentlite-infra`: its `docker-compose.yaml`
(Keycloak on :8180, OpenSearch with the security plugin on :9200), the realm
(`keycloak/gen-realm.py` → `keycloak/agentlite-realm.json`), the OpenSearch
security config (`opensearch-security/*.yml`, pushed to a running cluster with
`make security` there) and the dev gateway (`devgateway`, `make gateway` there).

The code is in two layers. `pkg/auth` is generic: it knows an OpenID Connect
issuer, bearer tokens, a control-set header and claim, roles, and how to
exchange and refresh tokens as the service's own client; it imports nothing of
agentlite. `internal/sts` is agentlite's thread-token policy on top of it, and
`internal/server` and `internal/runs` turn a principal into the store's caller.

## Identity

Users sign in at Keycloak, realm `agentlite`. The server never does OAuth with a
browser: a gateway in front of it signs the user in and sends their access token
as `Authorization: Bearer …` on every request, WebSocket upgrades included. In
production that is the Istio gateway (client certificate, OPA, a direct-grant
proxy); locally it is agentlite-infra's dev gateway, which listens on :8000,
signs `DEV_USER` in through the client `agentlite-gateway` and forwards to the
server on :8010 (`GET /__dev/as/{user}` switches user).

An access token names its user (`sub`, `preferred_username`) and their control
sets: `controlset_ids` (`OIDC_CONTROLSET_CLAIM`), a JSON array of client roles of
the client `controlsets`, granted through groups `/CONTROLSET/Cxxxx`. The array is
unordered; it is absent, not empty, for a user with none. `aud` includes
`agentlite` and `opensearch`. Dev users (password = username): `alice`
C1000–C1010, `bob` C1010–C1020, `carol` all 21.

## Verifying tokens

`pkg/auth` checks a token with the issuer's keys (go-oidc for discovery, the
key set and verification; x/oauth2 for the token endpoint: both are needed):
`iss` must be `OIDC_ISSUER_URL` exactly, the signature one of the issuer's keys
(from its discovery document, or `OIDC_JWKS_URL` in a container, where the
issuer's own URL is not reachable), `exp` in the future, `aud` must contain
`OIDC_AUDIENCE`, `typ` must be `Bearer` (not a refresh or ID token), and `sub`
and `preferred_username` must be set. A token without the control-set claim is
valid and holds none; one whose claim is not an array is refused.

Its middleware (`Verifier.Middleware`) runs on every route but `/health`: the
API, the socket's upgrade, `/.well-known/*` (the gateway attaches a token for
opencode and `npx skills` too), `/mcp/{id}` and the pass-through. A request
without a valid token gets `401` with `WWW-Authenticate: Bearer`. CORS answers a
preflight before the check. A request let in carries its `auth.Principal`:
subject, username, control sets, the request's control set (below), roles,
groups, the token and its expiry.

**Roles and groups.** The principal's `Roles` are the token's `realm_access.roles`
as they are and its `resource_access.*.roles` as `client:role`; `Groups` is the
`groups` claim when the token carries one (the realm emits none today).
`auth.RequireRole(role)` guards a route with one (`403` without it):

```go
e.GET("/v1/admin/usage", h.Usage, auth.RequireRole("agentlite:operator"))
```

For a claim the access token does not carry, `Verifier.UserInfo(ctx, principal)`
fetches the issuer's userinfo claims with the user's token and keeps them per
subject for `OIDC_USERINFO_TTL` (5 minutes), so a gated feature can read, say,
`claims["department"]` on each request without a round trip each time. It needs
discovery (no `OIDC_JWKS_URL`).

## Control sets

**One control set per thread**, fixed when the thread is created. **Every POST
names it in `X-Control-Set`** (`AUTH_CONTROLSET_HEADER`; `auth.Controlset`, one
middleware on every route): exactly one value, one of the caller's: none, a
blank, two headers or a list is `400`; one that is not theirs is `403`. The
value becomes `Principal.Controlset`, and the control set of the documents the
request creates: `POST /v1/agent/chat` (a new thread), `/v1/workspaces`,
`/v1/rag/files`, `/v1/skills`. POSTs that create nothing need it all the same,
the pass-through (`/v1/chat/completions`, …), `/v1/images/edits`, the MCP proxy
(`POST /mcp/{id}`), `/v1/suggestions` and `/v1/voice/transcriptions` included:
the UI sends it on every POST. A route that must do without it (an opencode
client's MCP proxy, say) goes in `AUTH_CONTROLSET_EXEMPT_PATHS`, by request path
or route pattern (`/mcp/:id`); the list is empty by default. GET, PATCH, PUT
and DELETE never need it: what they touch has its control set already.

Everything a thread produces inherits its control set: its messages, sub-agent
threads, sources, artifacts, counters and memories. A file's chunks have the
file's. Socket `run` frames carry no control set: a turn on an existing thread
runs in the thread's.

**Marks.** Every document of every index carries five fields, set by the store
on every write (`opensearch.Marks`, `stamp`; `created_*` only when the document
has none):

| Field | Set |
|---|---|
| `controlset_id` (keyword) | when created: the thread's, or `X-Control-Set` |
| `user_created_by` (keyword) | when created: the creator's `preferred_username` |
| `user_updated_by` (keyword) | on every write |
| `created_at` (date) | when created (a memory's is the remembered event's time; an assistant message's, its turn's start) |
| `updated_at` (date) | on every write |

The mappings are in `deploy/opensearch` (`make indices`); `make backfill`
marks older documents (`C1000` / `alice`; `created_at` from a source's own
`retrieved_at`, else when the backfill ran; `updated_at` = `created_at`). A
document that changes in place through `_update_by_query` (a thread moved out
of a deleted workspace) is marked by the query itself.

OpenSearch's role `abac_controlset_id` applies
`{"terms": {"controlset_id": [${user.roles}]}}` to every `user_ai_*` index, the
roles being the token's `controlset_ids`.

## The store

`pkg/db/opensearch` makes every call for the `Caller` in its context
(`access.go`): a token source, the username, the control set new documents get
and, inside a run only, the control sets reads and writes are confined to.

- **One hook authenticates every request.** The client's `Signer` sets
  `Authorization: Bearer` from the caller's token source on each attempt. A call
  with no caller fails before it is sent (`ErrNoCaller`); nothing goes out
  unauthenticated. There are no `OPENSEARCH_USERNAME`/`PASSWORD` settings (a
  stray one would have been all-access), a URL with credentials is refused, and
  node discovery and health checks, which would call without a user, are off.
- **A user's own request is governed by DLS alone.** `internal/server`'s
  `caller` middleware makes the request's caller from its principal: their own
  token, their username, the control set the request named, and no confinement.
  OpenSearch returns what the token may see; a token OpenSearch gives nothing
  (no control sets) is `ErrForbidden`.
- **A run is confined** to its thread's control set, on top of DLS (below).
- **Writes are marked** (`stamp`): a new document gets the caller's control set
  for new documents and is created by them, now; every write is updated by
  them, now. The control set a user's request labels with is the one it named,
  checked against their token by the middleware, or that of a document DLS let
  them read (a thread they add a turn to); inside a run, a label outside the
  thread's control set is refused (`ErrForbidden`), since OpenSearch stores
  whatever label it is given.
- **DLS does not filter a delete or an index by id**: a user can delete, or
  overwrite, a document they cannot see. So every delete by id reads the
  document first, as the caller (not found when hidden), and checks its owner;
  every replace of a document follows a read of it; and the one id a client
  chooses, a user message's, is written with `op_type=create` (a taken id is
  `400`).
- **OpenSearch refuses `_update` under DLS** (scripted or partial, `500`), and a
  `_bulk` with any update item fails whole. Documents that change in place, the
  counters (`counters.go`) and citations (`Cite`), are read and written back
  with `if_seq_no`/`if_primary_term` (`update`), again after a random pause when
  another write came first; twenty writers at once all land.
  `_update_by_query` and `_delete_by_query` work (they touch only documents the
  token can see).
- Writes refresh the index (`refresh=true`): under DLS a get is not realtime.

**Ownership.** Deleting, renaming or editing a thread, workspace, file or skill
is its creator's alone (`user_created_by` = the caller's username), whoever else
can see it: `403` otherwise (`opensearch.CheckOwner`). That includes replacing a
skill by uploading or writing one of the same name; the replaced skill keeps its
control set and creator. A run adding a turn to a thread someone else created is
not an edit.

Endpoints whose data is in Valkey, which knows nothing of control sets, read
the thread first: `GET /v1/threads/{id}/interrupts` and the cursor of
`/v1/threads/{id}/messages`.

## Thread tokens

A run cannot use the user's own access token: it expires in minutes, and a
resumed segment, a sub-agent's run, the turn's memory write and its finalizing
may come long after the request. A run uses a **thread token** instead
(`internal/sts`): one per conversation (the root thread) and user, so a user
taking a turn in a conversation someone shares with them uses their own.

**Minting** is an RFC 8693 token exchange by the server's own confidential client
(`OIDC_CLIENT_ID`/`OIDC_CLIENT_SECRET`, HTTP Basic; `auth.Client`, x/oauth2's
`Config.Exchange` with the exchange's parameters) at the token endpoint
(`OIDC_TOKEN_URL`, or the issuer's from discovery):

```
grant_type=urn:ietf:params:oauth:grant-type:token-exchange
subject_token=<the user's access token>
subject_token_type=urn:ietf:params:oauth:token-type:access_token
requested_token_type=urn:ietf:params:oauth:token-type:refresh_token
X-Control-Set: <the thread's control set>      (a header, STS_CONTROLSET_HEADER)
```

Keycloak (standard token exchange, "allow refresh token: same session") returns
an access token with the user's full `controlset_ids` and a refresh token bound
to the user's Keycloak session; it ignores the header (and the empty `code=`
x/oauth2 adds). It cannot issue offline tokens by exchange, so a thread token
lives as long as the user's session: each refresh counts as activity, and
logout, the session's idle or maximum lifetime, or a Keycloak restart ends it.
No `scope` is sent: there are no per-control-set scopes. An STS that narrows a
token to the thread's control set can take Keycloak's place: point
`OIDC_TOKEN_URL` at it, and it reads the control set from the header.

**When.** A user's own turn (`POST /v1/agent/chat`, a socket `run`) makes sure
there is one before it is queued, minting from that request's token when none is
kept or the kept one's refresh token has under `STS_MIN_LIFE` (5 minutes) left,
so a token that cannot be exchanged fails the request (`401`, socket code
`unauthorized`) rather than the run. The queue entry carries the user's token,
and the worker that executes the segment, possibly on another replica, mints
from it when it keeps none. A resume entry carries the token of the socket that
gave the last answer, and the resumed segment runs for that user.

**Kept** in memory only, never in Valkey (`auth.Memory`): the newest refresh
token and the current access token, renewed `STS_EARLY_EXPIRY` (30 s) before it
expires (`auth.Client.Source`, one exchange or refresh per key at a time). A
refresh that is refused drops the token; the run fails with a `RUN_ERROR`,
"authentication failed" (`sts.ErrSignIn`), and nothing falls back to another
token; the user's next turn mints a new one. A restart loses every thread token:
runs in flight die anyway, and later turns and resumes mint from the token they
come with. The cache is an interface (`auth.Cache`), so one that survives
restarts (Valkey with AES-GCM, Vault) can take its place.

## Runs

Every OpenSearch call of a run is made with the thread token of the user who
queued its segment, as them, **confined to the thread's control set** (the run
record's `controlset_id`; `runs.Manager.caller`, `caller.go`): the run reads and
writes only that control set, even for a user who holds more. The confinement
is the app's own filter on top of DLS: every search ANDs
`terms controlset_id` into its query, inside a k-NN query's own `filter`
(memories, file chunks), so the nearest are found among the thread's documents
rather than filtered out after; a get by id of another control set's document
is not found; a write labelled with another is refused. That covers the run
itself, its tools (sources, artifacts, `search_userfiles`, `#id`,
`write_skill`), the skills toolset (adk lists the skills before every model
call), memory search and the turn's memory write (`remember`, after the run),
sub-agents (their runs are queued with the parent's user and token, and find the
thread token kept for the conversation), and the finalizer. The finalizer runs
in the worker's process, straight after the segment settles, since only that
process holds the token; a queued or paused run that a user cancels is
finalized with the canceller's own token. Until an STS narrows thread tokens,
this confinement is what keeps a thread to its control set; once it does, DLS
enforces it too and the app's filter is the second layer.

Connectors that forward identity (`http.forward`, TOOLS.md) send the
`Authorization` of the run's user (`toolx.Run.User`): the access token the
segment was queued with, which may have expired during a long run.

## The socket

The upgrade request is authenticated like any other. The socket keeps its
principal and closes when the token expires (`1008`, "access token expired;
reconnect"): the UI reconnects, and its gateway attaches a fresh token. Runs
started from it are unaffected. The first frame about a thread looks the thread
up with the user's own token (DLS): a thread they cannot read is `not_found`.
Socket errors add the codes `unauthorized` (no thread token could be had: sign
in again) and `forbidden`.

## Audit log

Who did what is logged at the level `audit` (`pkg/logging.Audit`), above every
built-in level so `LOG_LEVEL` never hides it, with `agent_id`, `user_id`,
`username`, `run_id`, `thread_id`, `root_id` and `controlset_id` where they
apply: a run started (`resumed` for a later segment) and ended (its status), a
sub-agent started (with its parent's run and agent), a thread token minted,
refreshed or refused, and a request refused: a control set that is not the
user's, a change to someone else's document, a label a run may not write
(`request refused`, with the method and path, or the socket frame).

adk's session user stays `default` for every user (`runs.SessionUser`): a
thread's session is the thread's, and a shared thread has one history. A
session per user would split it, so each user saw only their own turns of the
conversation.

## Configuration

| Env | Default | |
|---|---|---|
| `OIDC_ISSUER_URL` | required | every token's `iss` exactly; locally `http://localhost:8180/realms/agentlite` |
| `OIDC_JWKS_URL` | discovery | the signing keys, when the issuer's URLs are not reachable (in a container: `http://keycloak:8080/realms/agentlite/protocol/openid-connect/certs`) |
| `OIDC_AUDIENCE` | `agentlite` | must be one of a token's `aud` |
| `OIDC_CONTROLSET_CLAIM` | `controlset_ids` | the token claim that lists the user's control sets |
| `OIDC_CLIENT_ID` | `agentlite` | the server's own client: token exchange and refresh |
| `OIDC_CLIENT_SECRET` | required | its secret (locally `agentlite-dev-secret`) |
| `OIDC_TOKEN_URL` | discovery | the exchange and refresh endpoint; set it in a container, or to an STS |
| `OIDC_USERINFO_TTL` | `5m` | how long userinfo claims are kept per user |
| `AUTH_CONTROLSET_HEADER` | `X-Control-Set` | the header every POST names its control set in |
| `AUTH_CONTROLSET_EXEMPT_PATHS` | none | POST paths, or route patterns, that need none (comma-separated) |
| `STS_CONTROLSET_HEADER` | `X-Control-Set` | the header of the exchange request that carries the thread's control set |
| `STS_EARLY_EXPIRY` | `30s` | how long before its expiry a thread token's access token is renewed |
| `STS_MIN_LIFE` | `5m` | the refresh lifetime a kept thread token needs for a run to use it rather than mint again |

`OPENSEARCH_USERNAME`/`OPENSEARCH_PASSWORD` are for `make indices` and
`make backfill` only (the internal admin user); the server ignores them.

## Limits

- `cmd/dev` (adk's own launcher) runs agents outside the server's requests, with
  no user: whatever reads OpenSearch there (the skills toolset, memory, files)
  fails closed.
- A run queued or paused before control sets existed has no `controlset_id` on
  its record: it fails before it starts ("the run has no control set"), rather
  than run with none. Start a new turn on the thread.
- A sub-agent's run, or a resumed segment, executed by a replica that keeps no
  thread token for the conversation mints one from the token in its queue entry,
  which may have expired by then; with one replica, as locally, the kept token
  is always found.
- Skill names are unique among the skills a user can see, not across control
  sets: two control sets can each hold a skill of one name, and a user who sees
  both gets either by name.
- `Verifier.UserInfo` needs discovery: with `OIDC_JWKS_URL` set there is no
  userinfo endpoint to call.
