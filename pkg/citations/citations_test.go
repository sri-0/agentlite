package citations

import (
	"slices"
	"testing"
)

// TestNumbers pins the citation format the model writes and the UI parses.
func TestNumbers(t *testing.T) {
	got := Numbers("A [source:2] and B [source: 10], again [source:2]; not [source:0] or [source:x].")
	if want := []int{2, 10}; !slices.Equal(got, want) {
		t.Errorf("Numbers = %v, want %v", got, want)
	}
}
