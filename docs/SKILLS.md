# Skills

The skills library: [Agent Skills](https://agentskills.io/specification), each a
`SKILL.md` (YAML frontmatter with a `name` and a `description`, then instructions in
Markdown) and any other files, kept in OpenSearch. Agents read them through adk's
skill toolset and write them with `write_skill`; a user loads one into a turn by
starting a message with `/name`; `npx skills` installs them into local agents.

Scripts never run. adk-go has no script tool (only adk-python has
`run_skill_script`), agentlite adds none, and the instruction tells the model that a
skill's scripts are for reading only.

## Storage

One OpenSearch index, `skills`, one document per skill with its name as id
(`pkg/db/opensearch/skills.go`):

| Field | |
|---|---|
| `name` (keyword), `description` (text, Markdown) | from SKILL.md's frontmatter |
| `title` (text) | a name to show people; the UI shows `name` when there is none |
| `category`, `source` (keyword) | what the library files it under, free-form: a category such as Writing, and where it comes from, such as Internal or Open source |
| `files` (object, not indexed) | every file, `SKILL.md` among them: `path`, and `content` as base64 |
| `created_at`, `updated_at` (date) | |

Categories and sources are values on the skills, not records of their own: the list to
choose from is the values the library has (a terms aggregation), and a new one exists
once a skill has it. Saving a skill replaces any of the same name, keeping when it was
created and its title, category and source unless it brings its own
(`Library.Save`, `internal/skills/meta.go`). A listing leaves out the files' content.

## Adding a skill

`POST /v1/skills` as a multipart form: `file`, a zip of the skill or its `SKILL.md`
alone (`internal/skills/skill.go`), and optional `title`, `description`, `category` and
`source`; an empty field keeps the SKILL.md's description, or what a replaced skill had.

- The zip is read with `archive/zip` as an `fs.FS`. `SKILL.md` is at its root or in its
  one top folder (a zip of the skill's folder, as Finder, GitHub and skill-creator make
  them). Hidden files and `__MACOSX` are left out; a link is refused.
- At most 1000 files and 25 MiB unzipped, as `npx skills` allows; the total is checked
  against each file's stated size before reading, and `archive/zip` refuses to read
  past it. The server's body limit (`MAX_BODY_MB`) bounds the zip itself.
- SKILL.md's frontmatter is read leniently (`adrg/frontmatter`): fields the spec does
  not have, such as Claude Code's `argument-hint`, are allowed and kept in the file.
  `name` and `description` are checked with adk's `skill.Validate` (lowercase letters,
  digits and single hyphens, at most 64; a description of at most 1024). adk's own
  parser is not used: it refuses any field outside the spec, which most published
  Claude Code skills have.
- A refused skill is a `400` with the reason.

`write_skill` (below) adds or changes a skill from a run.

## Changing a skill's details

`PATCH /v1/skills/{name}` with JSON `{title?, description?, category?, source?}`: a field
left out stays, `""` clears a title, category or source, and a body that sets none is a
`400` (`Library.Update`). The description is SKILL.md's, so setting it rewrites
SKILL.md's frontmatter: only that value changes in the YAML's syntax tree
(`goccy/go-yaml`'s parser and `$.description` path), so the other fields keep their
order, comments and formatting, the body stays as it was, and a multi-line description
is written as a literal block. A description that only differs by surrounding space or
CRLF line ends, as a browser's form sends it, changes nothing. The result is checked as
any upload is.

## The API

| Method | Path | |
|---|---|---|
| GET | `/v1/skills?q=&category=&source=` | the library by name, or best match first for `q` (names, titles and descriptions, every word matching, as you type: `bool_prefix`), each skill with its file paths; and `categories` and `sources`, each value with its count. A category's count is under the chosen source and the source counts under the chosen category (filter aggregations; the choices filter the skills through a `post_filter`), so each count is what choosing it would show |
| POST | `/v1/skills` | add a skill (above), `201` with the skill |
| PATCH | `/v1/skills/{name}` | change its title, description, category or source (above) |
| DELETE | `/v1/skills/{name}` | `204`; `404` when there is none |
| GET | `/v1/skills/{name}/files/{path}` | one file as written, sandboxed (`Content-Security-Policy: sandbox`, `nosniff`) |
| GET | `/.well-known/agent-skills/index.json` | the discovery index for `npx skills` |
| GET | `/.well-known/agent-skills/{name}/{path}` | one file, for `npx skills` |

## Agents

Agents that take every harness tool (no `tools:` list) get adk's skill toolset over the
library (`skilltoolset`, `internal/agents/skills.go`): `list_skills`, `load_skill` (a
skill's instructions) and `load_skill_resource` (one of its files), and before each
model call the list of skills, names and descriptions, at the end of the system
instruction. Our instruction replaces adk's, which says scripts can be run. The list
goes in the system instruction rather than the agent's `Instruction`, which adk treats
as a template that `{...}` in a description would break. `load_skill_resource` refuses
a binary file, since the model would read it as text.

`write_skill` (a harness tool, `internal/tools/builtin/skills.go`) creates a skill or
changes one after the user approves (HITL.md): `name`, `description`, `instructions`
(SKILL.md is built from them with adk's `skill.Build`), any other files as
`{path, content}`, and optionally `title`, `category` and `source`, which the tool tells
the model to ask the user for. A skill keeps the files, title, category and source the
call does not give.

The library is an adk `skill.Source` (`internal/skills/library.go`), which is what the
toolset and `/name` read.

## `/name`

A message that starts with `/name`, where `name` is a skill, carries the skill into
that turn (`Library.Expand`, `internal/skills/invoke.go`), as opencode does: the server
puts the skill's instructions in a `<skill_content name="...">` part ahead of the rest of
the message. The instructions name the skill's other files, which an agent with the
skill toolset reads with `load_skill_resource`. The client sends only what the user
typed, and the stored message stays as typed. A session rebuilt from history
(`Runtime.Seed`) expands it again, with the skill as the library has it then. The turn
starts with a `skill` activity (`{name, description}`), which the UI shows as the skill
loaded (AG-UI-RENDERING.md). A `/word` that is no skill is sent as typed; a skill that
cannot be read (the store is down) fails the turn rather than sending it without the
skill.

## npx skills

`npx skills add https://<server>` lists the library and installs the chosen skills
into a local agent; `-s <name>` picks one. The CLI reads
`/.well-known/agent-skills/index.json`. agentlite serves the draft's per-file form
(v0.1.0 of [the discovery draft](https://github.com/cloudflare/agent-skills-discovery-rfc),
no `$schema`): each skill's name, description and file paths, each file fetched from
`/.well-known/agent-skills/<name>/<path>`. The CLI sends no credentials.

The draft's v0.2.0 serves each skill as one archive with a digest instead. It is not
used: it needs an archive built for every skill on every listing, and the CLI's tar
reader ignores PAX headers, so long file names arrive cut short. The CLI reads both
forms. opencode's remote skills (`skills.urls`) read the same per-file shape.

## MCP servers that expose skills

Not built. An MCP server's skills are written against its own tools, so copying them
into the library forks them, and they drift on every upgrade of the server. The
choice is to proxy them. A connector's skills would be an adk `skill.Source` over the
server's skill resources (the MCP Skills extension, SEP-2640: `skill://` resources),
merged with the library (`skill.NewMergedSource`) for threads with that connector on.
The server's own skill tool would then be left out of its toolset.

## Code map

| File | Role |
|---|---|
| `pkg/db/opensearch/skills.go` | the `skills` index |
| `internal/skills/skill.go` | reading an upload, checking SKILL.md |
| `internal/skills/library.go` | `Library`: the store, as an adk `skill.Source` |
| `internal/skills/invoke.go` | `/name` |
| `internal/agents/skills.go` | adk's skill toolset and its instruction |
| `internal/tools/builtin/skills.go` | `write_skill` |
| `internal/handler/skills.go` | the API and the discovery index |
