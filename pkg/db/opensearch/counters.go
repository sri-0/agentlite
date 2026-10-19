package opensearch

import (
	"context"
	"fmt"
)

// indexCounters holds one document per conversation with its counters: the
// last source number and each artifact's last version. They live with the
// records they number, under the same access rules.
const indexCounters = "counters"

// counters is a conversation's counters document. Each count is a read and
// a conditional write (update): concurrent ones retry, so none is given out
// twice.
type counters struct {
	RootID    string         `json:"root_id"`
	Sources   int            `json:"sources,omitempty"`
	Artifacts map[string]int `json:"artifacts,omitempty"`
	Marks
}

// NextSourceNumber returns the conversation's next source number.
func (c *Client) NextSourceNumber(ctx context.Context, rootID string) (int, error) {
	got, err := update(ctx, c, indexCounters, rootID, true, func(d *counters) bool {
		d.RootID = rootID
		d.Sources++
		return true
	})
	if err != nil {
		return 0, fmt.Errorf("count for %s: %w", rootID, err)
	}
	return got.Sources, nil
}

// NextArtifactVersion returns an artifact's next version, above floor (the
// version of its current record): an artifact saved before its counter
// existed keeps counting up.
func (c *Client) NextArtifactVersion(ctx context.Context, rootID, artifactID string, floor int) (int, error) {
	got, err := update(ctx, c, indexCounters, rootID, true, func(d *counters) bool {
		if d.Artifacts == nil {
			d.Artifacts = map[string]int{}
		}
		d.RootID, d.Artifacts[artifactID] = rootID, max(d.Artifacts[artifactID], floor)+1
		return true
	})
	if err != nil {
		return 0, fmt.Errorf("count for %s: %w", rootID, err)
	}
	return got.Artifacts[artifactID], nil
}
