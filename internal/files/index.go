package files

import (
	"context"
	"fmt"

	"github.com/tmc/langchaingo/textsplitter"

	"agentlite/pkg/db/opensearch"
)

// index embeds what a file holds, for search: a text in chunks, an image,
// audio or video whole. Without an embedder nothing is.
//
// This is the one place a kind of file becomes searchable. A PDF is kept but
// not embedded: no open embedding model takes one, and nothing here converts
// files. A pipeline that extracts a PDF's text would hand its chunks to this
// switch (or write them to the embeddings index itself, docs/FILES.md).
func (s *Store) index(ctx context.Context, f *opensearch.File, body []byte) ([]opensearch.FileChunk, error) {
	if s.embedder == nil {
		return nil, nil
	}
	var texts []string
	var vectors [][]float32
	var err error
	switch kind(f.MediaType) {
	case kindText:
		if limit := s.limits.SearchableTextMB << 20; len(body) > limit {
			return nil, fmt.Errorf("%w: a text file is searchable up to %d MB; this one is %d bytes",
				ErrInvalid, s.limits.SearchableTextMB, len(body))
		}
		splitter := textsplitter.NewRecursiveCharacter(
			textsplitter.WithChunkSize(s.limits.ChunkChars), textsplitter.WithChunkOverlap(s.limits.ChunkOverlap))
		if texts, err = splitter.SplitText(string(body)); err != nil {
			return nil, err
		}
		vectors, err = s.embedder.Documents(ctx, texts)
	case kindImage, kindAudio, kindVideo:
		var v []float32
		v, err = s.embedder.Media(ctx, body, f.MediaType)
		texts, vectors = []string{""}, [][]float32{v}
	default: // kindPDF, see above
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEmbed, err)
	}
	chunks := make([]opensearch.FileChunk, len(texts))
	for i, text := range texts {
		chunks[i] = opensearch.FileChunk{
			ID: fmt.Sprintf("%s:%d", f.ID, i), FileID: f.ID, Name: f.Name, MediaType: f.MediaType,
			WorkspaceID: f.WorkspaceID, Seq: i, Text: text, Embedding: vectors[i],
		}
	}
	return chunks, nil
}
