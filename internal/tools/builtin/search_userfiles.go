package builtin

import (
	"errors"
	"slices"
	"strings"

	"agentlite/internal/files"
	"agentlite/pkg/toolx"
)

// searchFilesTool searches the user's uploaded files.
const searchFilesTool = "search_userfiles"

type searchFilesArgs struct {
	Query string   `json:"query" jsonschema:"what to look for, in words"`
	Files []string `json:"files,omitempty" jsonschema:"ids of files to search, such as a #id the user named; omit to search all the files of this conversation's workspace"`
}

// mediaNote tells the model what an image, audio or video hit is.
const mediaNote = "Images, audio and video match by what they show or sound like; you can't see or hear them here. " +
	"Name the file to the user; the user can reference an image with #<fileId> for you to look at it."

// searchFiles is search_userfiles: a semantic search over the files the user
// uploaded to the conversation's workspace (internal/files).
func searchFiles(d Deps) (toolx.Tool, error) {
	return toolx.New(toolx.Spec{
		Name: searchFilesTool,
		Description: "Search the files the user uploaded (documents, images, audio, video) by meaning. Returns the best " +
			"matching passages of text files, and the images, audio and video that match. Use it for questions about " +
			"the user's files, and for a file the user named with #id that is too long to read whole. Call files by " +
			"their name, never by their id.",
	}, func(tc toolx.Context, a searchFilesArgs) (map[string]any, error) {
		query := strings.TrimSpace(a.Query)
		if query == "" {
			return nil, errors.New("say what to look for in query")
		}
		hits, err := d.Files.Search(tc, tc.Thread.RootID, query, a.Files)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"hits": hits}
		if slices.ContainsFunc(hits, func(h files.Hit) bool { return h.Text == "" }) {
			out["note"] = mediaNote
		}
		return out, nil
	})
}
