package runs

import (
	"reflect"
	"testing"
	"time"
)

// TestRunFields: a run read back from its Valkey fields is the run written,
// so the worker runs it with what Start decided.
func TestRunFields(t *testing.T) {
	connectors := []string{"web"}
	want := &Run{
		ID: "r", ThreadID: "t", RootID: "t", AgentID: "images", Model: "m", PreviousModel: "p", ImageModel: "img",
		MessageID: "msg", TurnID: "turn", TurnSeq: 3, TurnFrom: "1-0", SegmentFrom: "2-0", Connectors: &connectors,
		Result: "done", Error: "failed", StartedAt: time.UnixMilli(1_700_000_000_000),
	}
	if got := runFromFields(want.fields()); !reflect.DeepEqual(got, want) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
	}
}
