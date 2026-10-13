// Package citations is the contract for sources between tools, the harness,
// the model and the UI: what a tool records about an item it returned, the
// call it came from, and how an answer cites it, [source:N].
package citations

import (
	"context"
	"regexp"
	"slices"
	"strconv"
)

// Source is an item a tool returned that the model may cite. The tool fills
// it from the system's own data and the harness stores it as given; the
// model only ever sees its number.
type Source struct {
	System string // the system it came from: the tool's connector
	// Key is the item's identity in the system, such as a row's UUID. The
	// same key keeps the same number within a conversation; empty uses URL.
	Key   string
	URL   string
	Title string
	// Metadata is stored with the source and never shown to the model: ids,
	// data markings, control sets, whatever the system attaches to the item.
	Metadata map[string]any
}

// Origin is the tool call a source came from.
type Origin struct {
	ThreadID string
	RootID   string // the conversation; numbers belong to it
	RunID    string
	Tool     string
	CallID   string
}

// Recorder records sources and numbers them within a conversation.
type Recorder interface {
	Add(ctx context.Context, from Origin, s Source) (int, error)
}

var citation = regexp.MustCompile(`\[source:\s*(\d+)\]`)

// Numbers returns the source numbers a text cites as [source:N], each once,
// in order of first appearance.
func Numbers(text string) []int {
	out := []int{}
	for _, m := range citation.FindAllStringSubmatch(text, -1) {
		n, err := strconv.Atoi(m[1])
		if err == nil && n > 0 && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}
