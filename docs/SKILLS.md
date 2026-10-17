# Skills

The skills library: [Agent Skills](https://agentskills.io/specification), each a
`SKILL.md` (YAML frontmatter with a `name` and a `description`, then instructions in
Markdown) and any other files. Each skill's record is in OpenSearch and its files are in
S3. Agents read skills through adk's skill toolset and write them with `write_skill`; a
user loads one into a turn with `/name`; `npx skills` installs them into local agents.
All of it is `internal/skills`.

Scripts never run. adk-go has no script tool (only adk-python has
`run_skill_script`), agentlite adds none, and the instruction tells the model that a
skill's scripts are for reading only. A script is stored, served and installed like any
other file, so local harnesses that run scripts can use it.

## Storage

The record is one document per skill in the OpenSearch index `skills`
(`pkg/db/opensearch/skills.go`):

| Field | |
|---|---|
| `id` (keyword) | a uuid, the document's id; the API and the UI address a skill by it |
| `name` (keyword), `description` (text, Markdown) | from SKILL.md's frontmatter; the name is unique in the library |
| `title` (text) | a name to show people; the UI shows `name` when there is none |
| `category`, `source` (keyword) | what the library files it under, free-form: a category such as Writing, and where it comes from, such as Internal or Open source |
| `files` (object, not indexed) | every file, `SKILL.md` among them: `path` and `size` |
| `created_at`, `updated_at` (date) | |

The files are in S3 (`S3_ENDPOINT`; MinIO in `docker-compose.yaml`) at
`skills/<id>/<path>`, in the bucket artifact bodies use. Without S3 there is no skills
library: no routes, no toolset, no `write_skill`.

Categories and sources are values on the skills, not records of their own: the list to
choose from is the values the library has (a terms aggregation), and a new one exists
once a skill has it.

Saving a skill (`Library.Save`) replaces any of the same name: it keeps its id, when it
was created, and its title, category and source unless the save brings its own. Its new
files are written, then the record, then the files it no longer has are deleted, so a
reader never finds the record listing a file that is gone. A save that fails part way
leaves a replaced skill with some of its new files, and a new skill's files are deleted.
A file left behind by a failed delete is logged and goes with the skill's others when it
is deleted. Two first saves of one name at the same moment could make two skills; the
library is written by people, and by agents a person approves, one at a time.

## Adding a skill

`POST /v1/skills` as a multipart form: `file`, a zip of the skill or its `SKILL.md`
alone (`internal/skills/parse.go`), and optional `title`, `description`, `category` and
`source`; an empty field keeps the SKILL.md's description, or what a replaced skill had.

- The zip is read with `archive/zip` as an `fs.FS`. `SKILL.md` is at its root or in its
  one top folder (a zip of the skill's folder, as Finder, GitHub and skill-creator make
  them). Hidden files and `__MACOSX` are left out; a link is refused, and so is a zip
  with a path that leaves it (`../`, `/`, `\`): `go.mod` sets `godebug
  zipinsecurepath=0`, and `archive/zip` checks it.
- At most 1000 files and 25 MiB unzipped, as `npx skills` allows; every save is held to
  them, `write_skill`'s too. A zip's total is checked against each file's stated size
  before anything is read, and `archive/zip` refuses to read past a stated size, so a zip
  bomb stops at the limit. The route's body limit is 26 MiB.
- SKILL.md's frontmatter is read leniently (`adrg/frontmatter`): fields the spec does
  not have, such as Claude Code's `argument-hint`, are allowed and kept in the file.
  `name` and `description` are checked with adk's `skill.Validate` (lowercase letters,
  digits and single hyphens, at most 64; a description of at most 1024). adk's own
  parser is not used: it refuses any field outside the spec, which most published
  Claude Code skills have.
- A refused skill is a `400` with the reason.

`write_skill` (below) adds or changes a skill from a run.

## Changing a skill's details

`PATCH /v1/skills/{id}` with JSON `{title?, description?, category?, source?}`: a field
left out stays, `""` clears a title, category or source, and a body that sets none is a
`400` (`Library.Update`). The description is SKILL.md's, as the spec has it, so
setting it rewrites SKILL.md's frontmatter (`internal/skills/meta.go`): only that value
changes in the YAML's syntax tree (`goccy/go-yaml`'s parser and `$.description` path),
so the other fields keep their order, comments and formatting, the body stays as it
was, and a multi-line description is written as a literal block. A description that
only differs by surrounding space or CRLF line ends, as a browser's form sends it,
changes nothing. The result is checked as any upload is.

## The API

| Method | Path | |
|---|---|---|
| GET | `/v1/skills?q=&category=&source=` | the library by name, or best match first for `q` (names, titles and descriptions, every word matching, as you type: `bool_prefix`), each skill with its files; and `categories` and `sources`, each value with its count. A category's count is under the chosen source and the source counts under the chosen category (filter aggregations; the choices filter the skills through a `post_filter`), so each count is what choosing it would show |
| POST | `/v1/skills` | add a skill (above), `201` with the skill |
| GET | `/v1/skills/{id}` | one skill |
| PATCH | `/v1/skills/{id}` | change its title, description, category or source (above) |
| DELETE | `/v1/skills/{id}` | the record and its files; `204`, `404` when there is none |
| GET | `/v1/skills/{id}/files/{path}` | one file as written, sandboxed (`Content-Security-Policy: sandbox`, `nosniff`: Echo's `Secure` middleware) |
| GET | `/.well-known/agent-skills/index.json` | the discovery index for `npx skills` |
| GET | `/.well-known/agent-skills/{name}/{path}` | one file, for `npx skills`, sandboxed; the only place a skill is addressed by name |

## Agents

The library is an adk `skill.Source` (`internal/skills/source.go`), and agents get
adk's skill toolset over it (`skilltoolset`, `Library.Toolset`): `list_skills`,
`load_skill` (a skill's instructions) and `load_skill_resource` (one of its files), and
before each model call the list of skills, names and descriptions, at the end of the
system instruction. The toolset is a tool like any other: an agent with no `tools:`
list gets it with the rest, and an agent with a list gets it by naming `skills`
(`report-writer` does; `chat`, which has no tools, does not).

- Our instruction replaces adk's, which says scripts can be run.
- The list goes in the system instruction rather than the agent's `Instruction`, which
  adk treats as a template that `{...}` in a description would break.
- `load_skill_resource` returns a script's text like any other file's; a binary file is
  refused, since the model would read it as text.
- Every call has the run's context. OpenSearch is read as the service today; once it
  is read with the user's credentials, the listing holds only the skills they may see.

`write_skill` (a harness tool, `internal/tools/builtin/skills.go`) creates a skill or
changes one after the user approves (HITL.md): `name`, `description`, `instructions`
(SKILL.md is built from them with adk's `skill.Build`), any other files as
`{path, content}`, and optionally `title`, `category` and `source`, which the tool tells
the model to ask the user for. A skill keeps the files, title, category and source the
call does not give.

## References: `/name`

A word of a message that is a sigil and a name is a reference (`internal/refs`).
`/name`, where `name` is a skill, is the only kind so far; the same seam is meant for
`/compact`, `@agent` and `#id`: each sigil has a `refs.Resolver`, and the library is
the one for `/` (`Library.Resolve`). For each reference, once each and at most ten words
looked up per message, the turn's content gets what it resolves to ahead of the
message, and the turn starts with an activity that shows it:

- A skill gives its instructions in a `<skill_content name="...">` part, as opencode
  does. They name the skill's other files, which the agent reads with
  `load_skill_resource`. The activity is `skill` (`{id, name, title, description}`), and
  the UI shows it as the skill loaded, linking to its page (AG-UI-RENDERING.md).
- The message itself goes to the model and is stored as typed. A `/word` that is no
  skill stays plain text, and one that cannot be a skill's name (`/usr/bin`) is not looked
  up; a skill that cannot be read (the store is down) fails the turn rather than sending
  it without the skill.

A session rebuilt from history (`Runtime.Seed`, when the adk session is gone from
Valkey) has the messages as typed, without the skills' instructions; the instruction
tells the model to load a skill named that way if it still needs it.

## npx skills

`npx skills add https://<server>` lists the library and installs the chosen skills
into a local agent; `-s <name>` picks one. The CLI reads
`/.well-known/agent-skills/index.json`. agentlite serves the draft's per-file form
(v0.1.0 of [the discovery draft](https://github.com/cloudflare/agent-skills-discovery-rfc),
no `$schema`): each skill's name, description and file paths, each file fetched from
`/.well-known/agent-skills/<name>/<path>` (`internal/skills/index.go`). The CLI sends
no credentials.

The draft's v0.2.0 serves each skill as one archive with a digest instead. It is not
used: it needs an archive built for every skill on every listing, and the CLI's tar
reader ignores PAX headers, so long file names arrive cut short. The CLI reads both
forms. opencode's remote skills (`skills.urls`) read the same per-file shape.

## MCP servers that expose skills

The library keeps its own copy of an MCP server's skills, and the server's skill tool is
left out of its connector's `tools:`. open-slide-mcp's skills (category Slides, source
open-slide) were copied in once, each SKILL.md with the preface open-slide's
`skill_read` adds (how its filesystem instructions map onto the deck tools), and the
slides connector lists every tool but `skill_read`. Copying again after an upgrade of
the server replaces them in place.

## Code map

| File | Role |
|---|---|
| `pkg/db/opensearch/skills.go` | the `skills` index: records, search, categories and sources |
| `internal/skills/library.go` | `Library`: saving, updating, deleting, reading files from S3 |
| `internal/skills/parse.go` | reading an upload, checking SKILL.md |
| `internal/skills/meta.go` | a skill's details, rewriting SKILL.md's description |
| `internal/skills/source.go` | the library as an adk `skill.Source` |
| `internal/skills/agents.go` | adk's skill toolset and its instruction; `/name` |
| `internal/skills/index.go` | the discovery index for `npx skills` |
| `internal/refs/refs.go` | references in a message, by sigil |
| `internal/tools/builtin/skills.go` | `write_skill` |
| `internal/handler/skills.go` | the API |
