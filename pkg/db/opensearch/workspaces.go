package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// indexWorkspaces holds the workspaces threads are filed under.
const indexWorkspaces = "workspaces"

// Workspace is a folder of threads, the unit people will share.
type Workspace struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SaveWorkspace upserts a workspace.
func (c *Client) SaveWorkspace(ctx context.Context, w *Workspace) error {
	return c.index(ctx, indexWorkspaces, w.ID, w)
}

// Workspace fetches one workspace; ErrNotFound when there is none.
func (c *Client) Workspace(ctx context.Context, id string) (*Workspace, error) {
	return get[Workspace](ctx, c, indexWorkspaces, id)
}

// Workspaces lists the workspaces by name.
func (c *Client) Workspaces(ctx context.Context) ([]Workspace, error) {
	return search[Workspace](ctx, c, indexWorkspaces, map[string]any{
		"size": listCap, "sort": []any{map[string]any{"name": "asc"}},
	})
}

// DeleteWorkspace removes a workspace. Its threads stay, outside any
// workspace; they are moved out first, so a failure can be retried. A thread
// being written at that moment can keep the id (writes replace the whole
// document); clients treat an unknown workspace as none.
func (c *Client) DeleteWorkspace(ctx context.Context, id string) error {
	q, _ := json.Marshal(map[string]any{
		"query":  map[string]any{"term": map[string]any{"workspace_id": id}},
		"script": map[string]any{"lang": "painless", "source": "ctx._source.remove('workspace_id')"},
	})
	_, err := c.api.UpdateByQuery(ctx, &opensearchapi.UpdateByQueryReq{
		Indices: []string{indexThreads}, BodyReader: bytes.NewReader(q),
		Params: &opensearchapi.UpdateByQueryParams{Refresh: "true", Conflicts: opensearchapi.ConflictsProceed},
	})
	if err != nil {
		return fmt.Errorf("move threads out of workspace %s: %w", id, err)
	}
	return c.delete(ctx, indexWorkspaces, id)
}
