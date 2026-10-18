# Files

Users upload files (documents, images, audio, video) to talk about them. A file is
kept once, named in any message with `#id` (the composer shows it as a chip with the
file's name), and, with an embedding model configured, searched by the agents with
`search_userfiles`. Everything files-specific is `internal/files` (the store, the `#`
resolver, the routes, embedding and the tool) and `pkg/db/opensearch/files.go` (the
records and chunks).

## What is accepted

Only kinds a model takes as they are; nothing is converted (no ffmpeg, no PDF
rendering, no Office documents):

| Kind | Media types | What the model gets for `#id` |
|---|---|---|
| text | any `text/*` and `application/json`; `.md`, `.csv` and `.json` refine `text/plain` | the text inline, in a `<file name>` block, up to 20,000 characters; longer, a note to search it |
| image | PNG, JPEG, WebP, GIF | the image itself, up to 10 MiB (an inline part; adk's openaimodel sends it as `image_url`); bigger, a note |
| pdf | `application/pdf` | the PDF itself, up to 10 MiB (an inline part, sent as a `file` part); bigger, a note |
| audio | WAV, MP3 | a note naming the file: adk-go refuses audio parts, so the model cannot listen |
| video | MP4 | a note, as audio |

These are also what the embedding model takes as they are: every kind but PDF is
embedded (below).

The type is sniffed from the first bytes (`http.DetectContentType`); the extension
only refines plain text and names an MP3 without an ID3 tag. Anything else is `415`,
and so is text that is not UTF-8 (a UTF-16 export). A file is at most 25 MiB; an
empty one is `400`.

What goes in as it is stays in the conversation's adk session, and is sent again with
every later turn; hence the 10 MiB cap, so one file a provider refuses can't break a
conversation for good.

Whatever the model gets starts by naming the file: `File "name" (type), which the
message calls #id; call it "name", never by its id`, so answers name files the way the
user does instead of repeating ids. A note then gives the size and, when the file is
searchable, tells the model to call `search_userfiles` with `files: [id]`; otherwise
that it can't be read or searched here.

## Workspaces

A file is filed under a workspace, or none (`workspace_id` absent), like a thread.
Deleting a workspace moves its files out of it, as it does its threads.

## Storage

- the record in the OpenSearch index `files` (`deploy/opensearch/files.json`):
  `{id, name, media_type, size, workspace_id?, status, created_at}`; the id is a UUIDv7;
- the body in S3 at `files/{id}`, served as its media type.

`status` is `ready` once the file is embedded and searchable, `stored` when it is kept
and referenceable but not searchable: a PDF, or every file when `models.yaml` has no
`files` block.

## Embedding and search

`models.yaml`'s `files` block names the embedding model, its vector size and its
server's request format:

```yaml
files:
  embedding_model: deepinfra/google/embeddinggemma-2
  dimensions: 768        # file_chunks.json is sized for it
  format: deepinfra      # or vllm
```

The model is **EmbeddingGemma 2** (open weights, Apache-2.0): text, images, audio and
video in one vector space. In development DeepInfra serves it (the `deepinfra` provider,
skipped without `DEEPINFRA_BASE_URL`, like OpenRouter); offline, vLLM serves the same
weights (`vllm serve google/embeddinggemma-2 --runner pooling --dtype bfloat16`, a vLLM
from after 2026-10-06), so vectors from either match. The two formats
(`internal/files/embed.go`):

- `deepinfra`: `POST /embeddings {model, task: "document" | "search_query", dimensions,
  encoding_format: "float", input: [...]}`, 32 inputs a request; an input is a text or a
  list with one content part: `image_url` and `video_url` with a data URL, `input_audio`
  with `{data, format: wav | mp3}`. DeepInfra adds EmbeddingGemma's task prompts.
- `vllm`: one request per input, `{model, messages: [{role: "user", content: [part]}],
  add_special_tokens: true, encoding_format: "float"}`, the same parts; a text gets the
  task prompt written in (`task: search result | query: …`, `title: none | text: …`).
  `dimensions` is not sent (vLLM caps it at 512 for this model); vectors are cut to
  `dimensions` here, which Matryoshka training allows.

An upload is embedded before anything is kept, and answers when it is done: a text in
chunks of about 2,000 characters (cut at a space, each starting with a whole word up to
200 characters before the last one ended), an image, audio or video whole. A model
refusal (say, audio longer than the model's window) is `502` with the model's message,
and nothing is kept; a failed upload's body and chunks are removed even if the client
has gone. A text is searchable up to 2 MiB; a bigger one is
`400`. The chunks go to the `file_chunks` index (`deploy/opensearch/file_chunks.json`:
`{id: <file id>:<seq>, file_id, name, media_type, workspace_id?, seq, text?, embedding}`,
a 768-dimension kNN vector, cosine, HNSW on Lucene), repeating the file's name, type and
workspace so a search needs no second lookup. Deleting a file deletes its chunks;
deleting a workspace moves them out with the file.

`search_userfiles {query, files?}` is a harness tool, offered when the block is there. It
embeds the query (task `search_query`) and returns the 8 nearest chunks (HNSW looks at
100 candidates: asked for 8, it missed the one differing passage of a long report),
`{hits: [{fileId, name, mediaType, seq, text?}]}`, of the files of the conversation's
workspace (the unfiled ones for a conversation in none), and of those only the files it
names, if it names any: a file of another workspace is never searched.

**Image, audio and video hits** come back by name and id, with a note: the model can't
see or hear them in a tool result (adk-go drops media there), and can tell the user to
reference an image with `#id`, which sends it. Attaching the hits to the next model
request (ADK's `load_artifacts` pattern) would need a request hook in `pkg/toolx`; not
built.

**PDFs** are kept and sent to the model natively with `#id`, but not embedded: no open
embedding model takes a PDF and nothing here converts files. A pipeline that extracts a
PDF's text plugs into one place: `Store.index` in `internal/files/index.go`, the switch
that decides what of a file is embedded. A separate system can instead write a PDF's
chunks to `file_chunks` itself (the fields above, the same model and size) and set the
record's `status` to `ready`.

## API

| Method | Path | |
|---|---|---|
| POST | `/v1/files` | multipart: `file`, and optionally `workspaceId`. `201` with the record, once embedded; `400` for an empty file, an unknown workspace or a text over 2 MiB to embed, `413` over 25 MiB, `415` for a kind not accepted, `502` when the embedding model refuses it |
| GET | `/v1/files` | `{object: "list", data: [record]}`, newest first (capped at 1,000); `?workspace=<id>` one workspace's, `?workspace=none` the unfiled ones |
| GET | `/v1/files/{id}` | the record |
| GET | `/v1/files/{id}/content` | the file, as its media type, sandboxed (`Content-Security-Policy: sandbox`, `nosniff`), `Content-Disposition: attachment` with its name, so it is always downloaded, never opened as a page (an `<img>` ignores it, so thumbnails show); byte ranges |
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
  When workspaces are shared, the `files` and `file_chunks` indices need the
  document-level security the threads index gets (by `workspace_id`).
- **Changing the embedding model** means re-embedding every file; there is no reindex.
  Vectors of two models (or sizes) must not share the index.
