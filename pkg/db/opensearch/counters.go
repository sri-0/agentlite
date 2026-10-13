package opensearch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// IndexCounters holds one document per conversation with its counters: the
// last source number and each artifact's last version. They live with the
// records they number, under the same access rules.
const IndexCounters = "counters"

const countersMapping = `{"mappings":{"dynamic":false,"properties":{
	"root_id":{"type":"keyword"},"sources":{"type":"integer"},"artifacts":{"type":"object","enabled":false}}}}`

// Counter scripts. An update of one document is atomic; a concurrent one is
// retried on conflict.
const (
	nextSourceScript  = `ctx._source.root_id = params.root; ctx._source.sources = (ctx._source.sources ?: 0) + 1;`
	nextVersionScript = `ctx._source.root_id = params.root; if (ctx._source.artifacts == null) { ctx._source.artifacts = [:]; }
		ctx._source.artifacts[params.id] = (ctx._source.artifacts[params.id] ?: 0) + 1;`
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

// NextArtifactVersion returns an artifact's next version.
func (c *Client) NextArtifactVersion(ctx context.Context, rootID, artifactID string) (int, error) {
	var got struct {
		Artifacts map[string]int `json:"artifacts"`
	}
	err := c.increment(ctx, rootID, nextVersionScript, map[string]any{"root": rootID, "id": artifactID}, &got)
	return got.Artifacts[artifactID], err
}

// increment runs a counter script on the conversation's counters document,
// creating it if needed, and decodes the document as it is afterwards.
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
		Index: IndexCounters, ID: rootID, BodyReader: strings.NewReader(string(body)),
		Params: &opensearchapi.UpdateParams{RetryOnConflict: counterRetries, Source: "true", Refresh: "true"},
	})
	if err != nil {
		return fmt.Errorf("count for %s: %w", rootID, err)
	}
	if resp.Get == nil {
		return fmt.Errorf("count for %s: no document returned", rootID)
	}
	return json.Unmarshal(resp.Get.Source, out)
}
