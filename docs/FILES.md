# Files

Users upload files (documents, images, audio, video) for RAG, to talk about them. A
file is kept once and named in any message with `#id`; the composer shows it as a
chip with the file's name. Everything files-specific is `internal/files` (the store,
the `#` resolver and the routes, under `/v1/rag/files`) and
`pkg/db/opensearch/files.go` (the records, in the `userfiles` index).

## What is accepted

Only kinds a model takes as they are; nothing is converted (no ffmpeg, no PDF
rendering, no Office documents):

| Kind | Media types | What the model gets for `#id` |
|---|---|---|
| text | any `text/*` and `application/json` | the text inline, in a `<file name>` block, up to `inline_text_kb` (20 KB); bigger, a note |
| image | PNG, JPEG, WebP, GIF | the image itself, up to `inline_media_mb` (10 MiB; an inline part, which adk's openaimodel sends as `image_url`); bigger, a note |
| pdf | `application/pdf` | the PDF itself, up to `inline_media_mb` (an inline part, sent as a `file` part); bigger, a note |
| audio | WAV, MP3 | a note naming the file: adk-go refuses audio parts, so the model cannot listen |
| video | MP4 | a note, as audio |

The type is detected from the content by `github.com/gabriel-vasile/mimetype` (CSV
and JSON included; Markdown and code are `text/plain`); the name plays no part.
Anything else is `415`, and so is text that is not UTF-8 (a UTF-16 export). A file is
at most `max_upload_mb` (25 MiB); an empty one is `400`.

The limits are the `files` block of `models.yaml`, every one with a default:
`max_upload_mb` 25, `inline_text_kb` 20, `inline_media_mb` 10, `max_tags` 10,
`max_tag_chars` 40.

What goes in as it is stays in the conversation's adk session, and is sent again with
every later turn; hence the `inline_media_mb` cap, so one file a provider refuses
can't break a conversation for good.

Whatever the model gets starts with a line naming the file, its type and size, and
telling the model to call it by its name, never by its id, so answers name files the
way the user does. Anything not sent whole gets a note that it can't be read here.

## Workspaces

A file is filed under a workspace, or none (`workspace_id` absent), like a thread.
Deleting a workspace moves its files out of it, as it does its threads.

## Storage

- the record in the OpenSearch index `userfiles` (`deploy/opensearch/userfiles.json`;
  with the deployment's `OPENSEARCH_INDEX_PREFIX`, `user_ai_userfiles`):
  `{id, name, media_type, size, workspace_id?, tags?, status, created_at}`; the id is a UUIDv7;
- the body in S3 at `files/{id}`, served as its media type.

`status` is `stored` today: kept and referenceable, not searchable.

## API

| Method | Path | |
|---|---|---|
| POST | `/v1/rag/files` | multipart: `file`, optionally `workspaceId`, and `tags` repeated (trimmed; blanks and repeats dropped, sorted; at most `max_tags` of at most `max_tag_chars`). `201` with the record; `400` for an empty file, an unknown workspace or bad tags, `413` over `max_upload_mb`, `415` for a kind not accepted |
| GET | `/v1/rag/files` | `{object: "list", data: [record]}`, newest first (capped at 1,000); `?workspace=<id>` one workspace's, `?workspace=none` the unfiled ones |
| GET | `/v1/rag/files/{id}` | the record |
| GET | `/v1/rag/files/{id}/content` | the file, as its media type, sandboxed (`Content-Security-Policy: sandbox`, `nosniff`), `Content-Disposition: attachment` with its name, so it is always downloaded, never opened as a page (an `<img>` ignores it, so thumbnails show); byte ranges |
| DELETE | `/v1/rag/files/{id}` | its body, then its record; `204` |

## References: `#id`

`#` is a sigil of `internal/refs` (SKILLS.md, References), resolved by
`files.Store.Resolve`: a word `#<uuid>` that names a file, of any workspace (the
composer's `#` menu lists every workspace's files, the chat's own first, so a user can
pick any), gives the turn's content what the table above says, ahead of the message,
and the turn a `file` activity (`{id, name, mediaType}`, messageId
`file:<turn id>:<file id>`), which the UI shows as a chip. Any other `#word` stays
text, and so does a file whose body is gone. The message itself is stored as typed,
with the ids. An image or PDF part stays in the adk session like the rest of the turn; a
session rebuilt from OpenSearch (`agents/seed.go`) has the text only.

## Gaps

- **A workspace deleted during an upload into it**: the upload checks the workspace
  first, so a file saved while the workspace goes can keep its id. Clients treat an
  unknown workspace as none, as they do for threads.
- **Access control.** Files are not scoped to a user, and `#` reaches any workspace's
  file. When workspaces are shared, the `userfiles` index needs the document-level
  security the threads index gets (by `workspace_id`), and `#` and the list must
  only offer what the user may read.
