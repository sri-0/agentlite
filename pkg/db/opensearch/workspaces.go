package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// indexWorkspaces holds the workspaces threads are filed under.
const indexWorkspaces = "workspaces"

// Workspace is a folder of threads and files: public, so everyone who holds
// its control set can file under it, or the creator's alone.
type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Sharing
	Marks
}

// SaveWorkspace upserts a workspace.
func (c *Client) SaveWorkspace(ctx context.Context, w *Workspace) error {
	return c.index(ctx, indexWorkspaces, w.ID, w)
}

// ErrUnknownWorkspace is a workspace id that names no workspace.
var ErrUnknownWorkspace = errors.New("unknown workspace")

// CheckWorkspace refuses a workspace id that names no workspace; "" is none.
func (c *Client) CheckWorkspace(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	_, err := c.Workspace(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w %q", ErrUnknownWorkspace, id)
	}
	return err
}

// Workspace fetches one workspace; ErrNotFound when there is none.
func (c *Client) Workspace(ctx context.Context, id string) (*Workspace, error) {
	return get[Workspace](ctx, c, indexWorkspaces, id)
}

// WorkspaceThreads lists the ids of the conversations filed under a
// workspace (never nil).
func (c *Client) WorkspaceThreads(ctx context.Context, id string) ([]string, error) {
	found, err := search[Thread](ctx, c, indexThreads, map[string]any{
		"size": listCap, "_source": []string{"id"}, "query": map[string]any{"term": map[string]any{"workspace_id": id}},
	})
	return ids(found), err
}

// Workspaces lists the workspaces the caller can see, their own and the
// public ones, by name.
func (c *Client) Workspaces(ctx context.Context) ([]Workspace, error) {
	return search[Workspace](ctx, c, indexWorkspaces, map[string]any{
		"size": listCap, "sort": []any{map[string]any{"name": "asc"}},
	})
}

// DeleteWorkspace removes a workspace the caller created. Its threads and
// files stay, outside any workspace; they are moved out first (marked as
// updated by the caller, now), so a failure can be retried. Only those the
// caller's token may touch move (its control sets; others' private ones
// among them): the rest, and a thread being written at that moment (writes
// replace the whole document), keep the id, and clients treat an unknown
// workspace as none.
func (c *Client) DeleteWorkspace(ctx context.Context, id string) error {
	w, err := c.Workspace(ctx, id)
	if err != nil {
		return err
	}
	if err := CheckOwner(ctx, w.Marks); err != nil {
		return err
	}
	caller, _ := CallerFrom(ctx)
	body := map[string]any{
		"query": map[string]any{"term": map[string]any{"workspace_id": id}},
		"script": map[string]any{"lang": "painless", "params": map[string]any{"user": caller.Username, "now": time.Now()},
			"source": "ctx._source.remove('workspace_id'); ctx._source.user_updated_by = params.user; ctx._source.updated_at = params.now"},
	}
	restrict(body, confinement(caller)...)
	q, _ := json.Marshal(body)
	_, err = c.api.UpdateByQuery(ctx, &opensearchapi.UpdateByQueryReq{
		Indices: []string{c.ix(indexThreads), c.ix(indexFiles), c.ix(c.chunks)}, BodyReader: bytes.NewReader(q),
		Params: &opensearchapi.UpdateByQueryParams{Refresh: "true", Conflicts: opensearchapi.ConflictsProceed},
	})
	if err != nil {
		return fmt.Errorf("move threads and files out of workspace %s: %w", id, err)
	}
	return c.delete(ctx, indexWorkspaces, id)
}
