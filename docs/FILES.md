# Files

Users upload files (documents, images, audio, video) to talk about them. A file is
kept once and named in any message with `#id`; the composer shows it as a chip with
the file's name. Everything files-specific is `internal/files` (the store, the `#`
resolver and the routes) and `pkg/db/opensearch/files.go` (the records).

## What is accepted

Only kinds a model takes as they are; nothing is converted (no ffmpeg, no PDF
rendering, no Office documents):

| Kind | Media types | What the model gets for `#id` |
|---|---|---|
| text | any `text/*` and `application/json`; `.md`, `.csv` and `.json` refine `text/plain` | the text inline, in a `<file id name>` block, up to 20,000 characters; longer, a note |
| image | PNG, JPEG, WebP, GIF | the image itself, up to 10 MiB (an inline part; adk's openaimodel sends it as `image_url`); bigger, a note |
| pdf | `application/pdf` | the PDF itself, up to 10 MiB (an inline part, sent as a `file` part); bigger, a note |
| audio | WAV, MP3 | a note naming the file: adk-go refuses audio parts, so the model cannot listen |
| video | MP4 | a note, as audio |

The type is sniffed from the first bytes (`http.DetectContentType`); the extension
only refines plain text and names an MP3 without an ID3 tag. Anything else is `415`,
and so is text that is not UTF-8 (a UTF-16 export). A file is at most 25 MiB; an
empty one is `400`.

What goes in as it is stays in the conversation's adk session, and is sent again with
every later turn; hence the 10 MiB cap, so one file a provider refuses can't break a
conversation for good.

A note says what the file is (`#id is the file "name" (type, size)`) and that it can't
be read here.

## Workspaces

A file is filed under a workspace, or none (`workspace_id` absent), like a thread.
Deleting a workspace moves its files out of it, as it does its threads.

## Storage

- the record in the OpenSearch index `files` (`deploy/opensearch/files.json`):
  `{id, name, media_type, size, workspace_id?, status, created_at}`; the id is a UUIDv7;
- the body in S3 at `files/{id}`, served as its media type.

`status` is `stored` today: kept and referenceable, not searchable.

## API

| Method | Path | |
|---|---|---|
| POST | `/v1/files` | multipart: `file`, and optionally `workspaceId`. `201` with the record; `400` for an empty file or an unknown workspace, `413` over 25 MiB, `415` for a kind not accepted |
| GET | `/v1/files` | `{object: "list", data: [record]}`, newest first (capped at 1,000); `?workspace=<id>` one workspace's, `?workspace=none` the unfiled ones |
| GET | `/v1/files/{id}` | the record |
| GET | `/v1/files/{id}/content` | the file, as its media type, sandboxed (`Content-Security-Policy: sandbox`, `nosniff`), `Content-Disposition: inline` with its name; byte ranges, so audio and video seek |
| DELETE | `/v1/files/{id}` | its body, then its record; `204` |

## References: `#id`

`#` is a sigil of `internal/refs` (SKILLS.md, References), resolved by
`files.Store.Resolve`: a word `#<uuid>` that names a file **of the conversation's
workspace** (the unfiled ones for a conversation in none) gives the turn's content
what the table above says, ahead of the message, and the turn a `file` activity
(`{id, name, mediaType}`, messageId `file:<turn id>:<file id>`), which the UI shows as
a chip. Any other `#word` stays text, a file of another workspace too. The runtime
puts the run in the context (`toolx.WithRun`) before it resolves references, so the
resolver knows the conversation. The message itself is stored as typed, with the
ids. An image or PDF part stays in the adk session like the rest of the turn; a
session rebuilt from OpenSearch (`agents/seed.go`) has the text only.

## Gaps

- **A workspace deleted during an upload into it**: the upload checks the workspace
  first, so a file saved while the workspace goes can keep its id. Clients treat an
  unknown workspace as none, as they do for threads.
- **Access control.** Files are not scoped to a user; a workspace is the only scope.
  When workspaces are shared, the `files` index needs the document-level security
  the threads index gets (by `workspace_id`).
