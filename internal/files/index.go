package files

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"agentlite/pkg/db/opensearch"
)

// Chunking of text: a chunk is about chunkSize characters and starts
// chunkOverlap characters before the last one ended, so a passage cut in two
// is whole in one of them.
const (
	chunkSize    = 2000
	chunkOverlap = 200
	// maxTextIndexed bounds a text file that is embedded: a bigger one would
	// take minutes to upload.
	maxTextIndexed = 2 << 20
)

// index embeds what a file holds, for search: a text in chunks, an image,
// audio or video whole. Without an embedder nothing is.
//
// This is the one place a kind of file becomes searchable. A PDF is kept but
// not embedded: no open embedding model takes one, and nothing here converts
// files. A pipeline that extracts a PDF's text would hand its chunks to this
// switch (or write them to the file_chunks index itself, docs/FILES.md).
func (s *Store) index(ctx context.Context, f *opensearch.File, body []byte) ([]opensearch.FileChunk, error) {
	if s.embedder == nil {
		return nil, nil
	}
	var inputs []input
	switch kind(f.MediaType) {
	case kindText:
		if len(body) > maxTextIndexed {
			return nil, fmt.Errorf("%w: a text file is searchable up to 2 MiB; this one is %d bytes", ErrInvalid, len(body))
		}
		for _, c := range chunk(string(body)) {
			inputs = append(inputs, input{text: c})
		}
	case kindImage, kindAudio, kindVideo:
		inputs = []input{{data: body, mediaType: base(f.MediaType)}}
	default: // kindPDF, see above
		return nil, nil
	}
	vectors, err := s.embedder.embed(ctx, taskDocument, inputs)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEmbed, err)
	}
	chunks := make([]opensearch.FileChunk, len(inputs))
	for i, in := range inputs {
		chunks[i] = opensearch.FileChunk{
			ID: fmt.Sprintf("%s:%d", f.ID, i), FileID: f.ID, Name: f.Name, MediaType: f.MediaType,
			WorkspaceID: f.WorkspaceID, Seq: i, Text: in.text, Embedding: vectors[i],
		}
	}
	return chunks, nil
}

// chunk splits a text into chunks of about chunkSize characters, each cut at
// a space where one is near the end, and each after the first starting with
// the whole word at or after chunkOverlap characters before the last one
// ended. A blank text has none.
func chunk(text string) []string {
	r := []rune(strings.TrimSpace(text))
	var out []string
	for start := 0; start < len(r); {
		end := min(start+chunkSize, len(r))
		if end < len(r) {
			// Back to the last space in the chunk's final stretch, if any.
			for i := end; i > end-chunkOverlap; i-- {
				if unicode.IsSpace(r[i]) {
					end = i
					break
				}
			}
		}
		out = append(out, strings.TrimSpace(string(r[start:end])))
		if end == len(r) {
			break
		}
		start = end - chunkOverlap
		for i := start; i < end; i++ { // on to the next word, so no chunk starts mid-word
			if unicode.IsSpace(r[i]) {
				start = i + 1
				break
			}
		}
	}
	return out
}
