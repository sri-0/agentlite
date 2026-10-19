package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// indexCounters holds one document per conversation with its counters: the
// last source number and each artifact's last version. They live with the
// records they number, under the same access rules.
const indexCounters = "counters"

// Counter scripts. An update of one document is atomic; a concurrent one is
// retried on conflict. A version never falls below the floor the caller
// read, so an artifact saved before its counter existed keeps counting up.
const (
	nextSourceScript  = `ctx._source.root_id = params.root; ctx._source.sources = (ctx._source.sources ?: 0) + 1;`
	nextVersionScript = `ctx._source.root_id = params.root; if (ctx._source.artifacts == null) { ctx._source.artifacts = [:]; }
		def cur = ctx._source.artifacts[params.id]; int last = cur == null ? 0 : (int) cur; int floor = (int) params.floor;
		ctx._source.artifacts[params.id] = (last > floor ? last : floor) + 1;`
	counterRetries = 10
)

// NextSourceNumber returns the conversation's next source number.
func (c *Client) NextSourceNumber(ctx context.Context, rootID string) (int, error) {
	var got struct {
		Sources int `json:"sources"`
	}
	err := c.increment(ctx, rootID, nextSourceScript, map[string]any{"root": rootID}, &got)
	return got.Sources, err
}

// NextArtifactVersion returns an artifact's next version, above floor (the
// version of its current record).
func (c *Client) NextArtifactVersion(ctx context.Context, rootID, artifactID string, floor int) (int, error) {
	var got struct {
		Artifacts map[string]int `json:"artifacts"`
	}
	params := map[string]any{"root": rootID, "id": artifactID, "floor": floor}
	err := c.increment(ctx, rootID, nextVersionScript, params, &got)
	return got.Artifacts[artifactID], err
}

// increment runs a counter script on the conversation's counters document,
// creating it if needed, and decodes the document as it is afterwards (the
// update returns it, so no refresh is needed).
func (c *Client) increment(ctx context.Context, rootID, script string, params map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{
		"scripted_upsert": true,
		"upsert":          map[string]any{},
		"script":          map[string]any{"lang": "painless", "source": script, "params": params},
	})
	if err != nil {
		return err
	}
	resp, err := c.api.Update(ctx, opensearchapi.UpdateReq{
		Index: indexCounters, ID: rootID, BodyReader: bytes.NewReader(body),
		Params: &opensearchapi.UpdateParams{RetryOnConflict: counterRetries, Source: "true"},
	})
	if err != nil {
		return fmt.Errorf("count for %s: %w", rootID, err)
	}
	if resp.Get == nil {
		return fmt.Errorf("count for %s: no document returned", rootID)
	}
	return json.Unmarshal(resp.Get.Source, out)
}
