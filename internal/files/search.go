package files

import (
	"errors"
	"strings"

	"agentlite/pkg/toolx"
)

// SearchToolName is the tool that searches the user's uploaded files.
const SearchToolName = "search_userfiles"

// searchHits is how many chunks a search returns.
const searchHits = 8

type searchArgs struct {
	Query string   `json:"query" jsonschema:"what to look for, in words"`
	Files []string `json:"files,omitempty" jsonschema:"ids of files to search, such as a #id the user named; omit to search all the files of this conversation's workspace"`
}

// hit is a search result as the model sees it.
type hit struct {
	FileID    string `json:"fileId"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Seq       int    `json:"seq"`            // the chunk's place in its file
	Text      string `json:"text,omitempty"` // a text file's chunk
}

// mediaNote tells the model what an image, audio or video hit is.
const mediaNote = "Images, audio and video match by what they show or sound like; you can't see or hear them here. " +
	"Name the file to the user; the user can reference an image with #<fileId> for you to look at it."

// Tool is search_userfiles: a semantic search over the files the user
// uploaded to the conversation's workspace (or the unfiled ones, for a
// conversation in none), or over the files it names.
func (s *Store) Tool() (toolx.Tool, error) {
	return toolx.New(toolx.Spec{
		Name: SearchToolName,
		Description: "Search the files the user uploaded (documents, images, audio, video) by meaning. Returns the best " +
			"matching passages of text files, and the images, audio and video that match. Use it for questions about " +
			"the user's files, and for a file the user named with #id that is too long to read whole.",
	}, func(tc toolx.Context, a searchArgs) (map[string]any, error) {
		query := strings.TrimSpace(a.Query)
		if query == "" {
			return nil, errors.New("say what to look for in query")
		}
		workspaceID := ""
		if len(a.Files) == 0 {
			t, err := s.records.Thread(tc, tc.Thread.RootID)
			if err != nil {
				return nil, err
			}
			workspaceID = t.WorkspaceID
		}
		vectors, err := s.embedder.embed(tc, taskQuery, []input{{text: query}})
		if err != nil {
			return nil, err
		}
		chunks, err := s.records.SearchFileChunks(tc, vectors[0], searchHits, workspaceID, a.Files)
		if err != nil {
			return nil, err
		}
		hits := make([]hit, len(chunks))
		media := false
		for i, c := range chunks {
			hits[i] = hit{FileID: c.FileID, Name: c.Name, MediaType: c.MediaType, Seq: c.Seq, Text: c.Text}
			media = media || kind(c.MediaType) != kindText
		}
		out := map[string]any{"hits": hits}
		if media {
			out["note"] = mediaNote
		}
		return out, nil
	})
}
