# Files

Users upload files (documents, images, audio, video) for RAG, to talk about them. A
file is kept once, named in any message with `#id` (the composer shows it as a chip
with the file's name), and, with an embedding model configured, searched by the
agents with `search_userfiles`. Everything files-specific is `internal/rag/userfiles` (the
store, the `#` resolver, the routes under `/v1/rag/files`, embedding and the search)
and `pkg/db/opensearch/files.go` (the records, in the `userfiles` index, and the
chunks); the tool's wrapper is `internal/tools/builtin/search_userfiles.go`, like
the other harness tools.

## What is accepted

Only kinds a model takes as they are; nothing is converted (no ffmpeg, no PDF
rendering, no Office documents):

| Kind | Media types | What the model gets for `#id` |
|---|---|---|
| text | any `text/*` and `application/json` | the text inline, in a `<file name>` block, up to `inline_text_kb` (20 KB); bigger, a note |
| image | PNG, JPEG, WebP, GIF | to a model whose card lists `vision`, the image itself, up to `inline_media_mb` (10 MiB; an inline part, which adk's openaimodel sends as `image_url`); bigger, or to another model, a note |
| pdf | `application/pdf` | to a model whose card lists `files`, the PDF itself, up to `inline_media_mb` (an inline part, sent as a `file` part); bigger, or to another model, a note |
| audio | WAV, MP3 | a note naming the file: adk-go refuses audio parts, so the model cannot listen |
| video | MP4 | a note, as audio |

The type is detected from the content by `github.com/gabriel-vasile/mimetype` (CSV
and JSON included; Markdown and code are `text/plain`); the name plays no part.
Anything else is `415`, and so is text that is not UTF-8 (a UTF-16 export). A file is
at most `max_upload_mb` (25 MiB); an empty one is `400`.

These are also what the embedding model takes as they are: every kind but PDF is
embedded (below).

The limits are the `files` block of `models.yaml`, every one with a default:
`max_upload_mb` 25, `inline_text_kb` 20, `inline_media_mb` 10, `max_tags` 10,
`max_tag_chars` 40.

What goes in as it is stays in the conversation's adk session, and is sent again with
every later turn; hence the `inline_media_mb` cap, so one file a provider refuses
can't break a conversation for good.

Whatever the model gets starts with a line naming the file, its type and size, and
telling the model to call it by its name, never by its id, so answers name files the
way the user does. Anything not sent whole gets a note that it can't be read here,
and, when the file is searchable, to call `search_userfiles` with `files: [id]`.

## Workspaces

A file is filed under a workspace, or none (`workspace_id` absent), like a thread.
Deleting a workspace moves its files out of it, as it does its threads.

## Storage

- the record in the OpenSearch index `userfiles` (`deploy/opensearch/userfiles.json`;
  with the deployment's `OPENSEARCH_INDEX_PREFIX`, `user_ai_userfiles`):
  `{id, name, media_type, size, workspace_id?, tags?, status, created_at}`; the id is a UUIDv7;
- the body in S3 at `files/{id}`, served as its media type.

`status` is `ready` once the file is embedded and searchable, `stored` when it is kept
and referenceable but not searchable: a PDF, or every file when `models.yaml` has no
`embedding` block.

## Embedding and search

Files are embedded with `models.yaml`'s `embedding` block, the one model long-term memory
uses too (MODELS.md, Embeddings), through the OpenAI embeddings API with openai-go's
typed client (`internal/providers/embed.go`): no request format per provider.

- **Text** goes as the API has it, `input: [texts]`, 64 a request, each after the
  block's `document_prefix` (a query after its `query_prefix`; both empty unless the
  model was trained with them, like EmbeddingGemma 2).
- **An image, audio or video** has no place in that API, so `input` holds one chat
  message with the file as a content part, the way chat requests carry media; the rest
  of the request is the same typed call. vLLM (main since 2026-06) and OpenRouter's
  Gemini Embedding 2 both take that and embed it into one vector. An image is OpenAI's
  `image_url` (a data URL), audio OpenAI's `input_audio` (`{data, format: wav | mp3}`),
  video vLLM's `video_url` (OpenAI has none).

In development the block names OpenRouter's **Gemini Embedding 2** (768 dimensions): it
embeds text and images; it refuses `input_audio` without a data URL and wants
`input_video` for video, so there audio and video uploads are refused (`502`).
Offline, **EmbeddingGemma 2** on vLLM (open weights, Apache-2.0; a vLLM from after
2026-10-06, `--runner pooling --dtype bfloat16`) takes all four. Vectors of different
models don't mix: switching model means re-embedding (Gaps).

An upload is embedded before anything is kept, and answers when it is done: a text in
chunks of about `chunk_chars` characters overlapping by `chunk_overlap` (langchaingo's
`textsplitter.RecursiveCharacter`, which splits at paragraphs, then lines, then words),
an image, audio or video whole. A model refusal is `502` with the model's message, and
nothing is kept; a failed upload's body and chunks are removed even if the client has
gone. A text is searchable up to `searchable_text_mb`; a bigger one is `400`.

The chunks go to the `userfile_chunks` index (`deploy/opensearch/userfile_chunks.json`):
a chunk is `{id: <file id>:<seq>, file_id, name, media_type, workspace_id?, seq, text?,
embedding}`, a
768-dimension kNN vector (cosine, HNSW on Lucene), repeating the file's name, type and
workspace so a search needs no second lookup. Deleting a file deletes its chunks;
deleting a workspace moves them out with the file.

`search_userfiles {query, files?, all?}` (`internal/tools/builtin/search_userfiles.go`,
`Store.Search` behind it) is offered when files are searchable. It embeds the query and
returns the `search_hits` nearest chunks (HNSW looks at 100 candidates: asked for 8, it
missed the one differing passage of a long report), `{hits: [{fileId, name, mediaType,
seq, text?}]}`:
- of the files it names, from any workspace (a `#id` the user picked);
- else, by default, of the conversation's workspace's files (the unfiled ones for a
  conversation in none);
- else, with `all`, of every workspace's: the agent can look more broadly when its own
  workspace has nothing.

**Image hits are shown to the model** when its card lists `vision`
(`modelcatalog.Model.HasVision`; a PDF is never a hit, as none is embedded): adk leaves
media out of tool results, so a before-model callback (`builtin.ShowFound`) adds them,
up to `search_media` files each within `inline_media_mb`, as a user message after the
result, on the call that reads it; the next call no longer ends with the result, so they
are sent once. A model without vision gets a note to name them to the user. Audio and
video hits are named only: adk-go refuses audio and video parts.

**PDFs** are kept and sent natively with `#id` to a model that takes files (its card's
`files`, which discovery sets when OpenRouter's input modalities name `file`; images are
`vision`, a separate capability: vLLM takes images but has no PDF part), but not
embedded: no open embedding model takes a PDF and nothing here converts files. A
pipeline that extracts a PDF's text plugs into one place: `Store.index` in
`internal/rag/userfiles/index.go`, the switch that decides what of a file is embedded. A
separate system can instead write a PDF's chunks to the `userfile_chunks` index itself
(the fields above, the same model and size) and set the record's `status` to `ready`.

## API

| Method | Path | |
|---|---|---|
| POST | `/v1/rag/files` | multipart: `file`, optionally `workspaceId`, and `tags` repeated (trimmed; blanks and repeats dropped, sorted; at most `max_tags` of at most `max_tag_chars`). `201` with the record, once embedded; `400` for an empty file, an unknown workspace, bad tags or a text over `searchable_text_mb` to embed, `413` over `max_upload_mb`, `415` for a kind not accepted, `502` when the embedding model refuses it |
| GET | `/v1/rag/files` | `{object: "list", data: [record]}`, newest first (capped at 1,000); `?workspace=<id>` one workspace's, `?workspace=none` the unfiled ones |
| GET | `/v1/rag/files/{id}` | the record |
| GET | `/v1/rag/files/{id}/content` | the file, as its media type, sandboxed (`Content-Security-Policy: sandbox`, `nosniff`), `Content-Disposition: attachment` with its name, so it is always downloaded, never opened as a page (an `<img>` ignores it, so thumbnails show); byte ranges |
| DELETE | `/v1/rag/files/{id}` | its body and chunks, then its record; `204` |

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
  file. When workspaces are shared, the `userfiles` and `userfile_chunks` indices need the
  document-level security the threads index gets (by `workspace_id`), and `#` and the
  list must only offer what the user may read.
- **Changing the embedding model** means re-embedding every file and memory; there is
  no reindex. Vectors of two models (or sizes) must not share the index.
