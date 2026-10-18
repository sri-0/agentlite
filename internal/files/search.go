package files

import "context"

// searchHits is how many chunks a search returns.
const searchHits = 8

// Hit is a chunk a search found, as the model sees it.
type Hit struct {
	FileID    string `json:"fileId"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Seq       int    `json:"seq"`            // the chunk's place in its file
	Text      string `json:"text,omitempty"` // a text file's chunk; empty for an image, audio or video
}

// Search is a semantic search over the files the user uploaded to a
// conversation's workspace (the unfiled ones, for a conversation in none),
// or over those of fileIDs among them; rootID is the conversation's root
// thread. It needs the embedder: search_userfiles exists only with one.
func (s *Store) Search(ctx context.Context, rootID, query string, fileIDs []string) ([]Hit, error) {
	root, err := s.records.Thread(ctx, rootID)
	if err != nil {
		return nil, err
	}
	vectors, err := s.embedder.embed(ctx, taskQuery, []input{{text: query}})
	if err != nil {
		return nil, err
	}
	chunks, err := s.records.SearchFileChunks(ctx, vectors[0], searchHits, root.WorkspaceID, fileIDs)
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, len(chunks))
	for i, c := range chunks {
		hits[i] = Hit{FileID: c.FileID, Name: c.Name, MediaType: c.MediaType, Seq: c.Seq, Text: c.Text}
	}
	return hits, nil
}
