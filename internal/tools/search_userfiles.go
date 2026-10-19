package tools

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"agentlite/internal/rag/userfiles"
	"agentlite/pkg/toolx"
)

// searchFilesTool searches the user's uploaded files.
const searchFilesTool = "search_userfiles"

type searchFilesArgs struct {
	Query string   `json:"query" jsonschema:"what to look for, in words"`
	Files []string `json:"files,omitempty" jsonschema:"ids of files to search, such as a #id the user named, from any workspace; omit to search by workspace"`
	All   bool     `json:"all,omitempty" jsonschema:"search every workspace's files, not only this conversation's; for when its own have nothing"`
}

// mediaNote tells the model what an image, audio or video hit is when it
// can't see it (ShowFound shows images to a model that can).
const mediaNote = "Images, audio and video match by what they show or sound like. You are shown the images " +
	"found if you can see images; otherwise name the file to the user."

// SearchFiles is search_userfiles: a semantic search over the files the user
// uploaded (internal/rag/userfiles), the conversation's workspace's unless it says
// otherwise.
func SearchFiles(store *userfiles.Store) (toolx.Tool, error) {
	return toolx.New(toolx.Spec{
		Name: searchFilesTool,
		Description: "Search the files the user uploaded (documents, images, audio, video) by meaning: by default " +
			"this conversation's workspace's, every workspace's with all. Returns the best matching passages of text " +
			"files, and the images, audio and video that match. Use it for questions about the user's files, and for " +
			"a file the user named with #id that is too long to read whole. Call files by their name, never by their id.",
	}, func(tc toolx.Context, a searchFilesArgs) (map[string]any, error) {
		query := strings.TrimSpace(a.Query)
		if query == "" {
			return nil, errors.New("say what to look for in query")
		}
		hits, err := store.Search(tc, tc.Thread.RootID, query, a.Files, a.All)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"hits": hits}
		if slices.ContainsFunc(hits, func(h userfiles.Hit) bool { return h.Text == "" }) {
			out["note"] = mediaNote
		}
		return out, nil
	})
}

// ShowFound shows a model that sees images the images that search_userfiles
// found: adk leaves media out of tool results, so they go
// as a user message after the result, on the call that reads it (the next
// call no longer ends with the result, so they are sent once).
func ShowFound(store *userfiles.Store) llmagent.BeforeModelCallback {
	return func(ctx agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		if len(req.Contents) == 0 {
			return nil, nil
		}
		var hits []userfiles.Hit
		for _, p := range req.Contents[len(req.Contents)-1].Parts {
			if r := p.FunctionResponse; r != nil && r.Name == searchFilesTool {
				hits = append(hits, foundIn(r.Response)...)
			}
		}
		if parts := store.Shown(ctx, hits); len(parts) > 0 {
			req.Contents = append(req.Contents, genai.NewContentFromParts(parts, genai.RoleUser))
		}
		return nil, nil
	}
}

// foundIn is the hits of a search_userfiles result, as the tool returned them
// or as they read back from the session (JSON).
func foundIn(response map[string]any) []userfiles.Hit {
	raw, err := json.Marshal(response["hits"])
	if err != nil {
		return nil
	}
	var hits []userfiles.Hit
	_ = json.Unmarshal(raw, &hits) // a result without hits (an error) has none
	return hits
}
