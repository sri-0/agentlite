package files

import (
	"context"
	"fmt"
	"io"

	"google.golang.org/genai"
)

// Hit is a chunk a search found, as the model sees it.
type Hit struct {
	FileID    string `json:"fileId"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Seq       int    `json:"seq"`            // the chunk's place in its file
	Text      string `json:"text,omitempty"` // a text file's chunk; empty for an image, audio or video
}

// Search is a semantic search over the files the user uploaded: those of
// fileIDs, from any workspace, when it names some; else every workspace's
// with all; else the files of the conversation's workspace (the unfiled
// ones, for a conversation in none). rootID is the conversation's root
// thread. It needs the embedder: search_userfiles exists only with one.
func (s *Store) Search(ctx context.Context, rootID, query string, fileIDs []string, all bool) ([]Hit, error) {
	var workspaceID *string
	if !all {
		root, err := s.records.Thread(ctx, rootID)
		if err != nil {
			return nil, err
		}
		workspaceID = &root.WorkspaceID
	}
	vector, err := s.embedder.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	chunks, err := s.records.SearchFileChunks(ctx, vector, s.limits.SearchHits, workspaceID, fileIDs)
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, len(chunks))
	for i, c := range chunks {
		hits[i] = Hit{FileID: c.FileID, Name: c.Name, MediaType: c.MediaType, Seq: c.Seq, Text: c.Text}
	}
	return hits, nil
}

// Shown is what the model sees of the images among hits (a PDF is never one:
// none is embedded, see index.go): each file once, up to the search_media limit and each within inline_media_mb, as it
// is after a line naming it. A model that sees images gets them after
// search_userfiles' result (adk leaves media out of tool results). A file
// whose record or body is gone is left out.
func (s *Store) Shown(ctx context.Context, hits []Hit) []*genai.Part {
	var parts []*genai.Part
	seen := map[string]bool{}
	for _, h := range hits {
		if kind(h.MediaType) != kindImage || seen[h.FileID] || len(seen) == s.limits.SearchMedia {
			continue
		}
		seen[h.FileID] = true
		f, err := s.records.File(ctx, h.FileID)
		if err != nil || f.Size > s.limits.InlineMediaMB<<20 {
			continue
		}
		body, err := s.Body(ctx, f)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(body)
		body.Close()
		if err != nil {
			continue
		}
		parts = append(parts, genai.NewPartFromText(fmt.Sprintf("search_userfiles found %q:", f.Name)), genai.NewPartFromBytes(data, f.MediaType))
	}
	return parts
}
