package engine

import (
	"maps"
	"slices"
	"testing"

	"github.com/opstruss/opstruss/internal/state"
)

// TestProgramState keeps $OPSTRUSS_STATE to the fields that the program
// contract lists in "The state file", which plugins read: a field added
// here is added there too.
func TestProgramState(t *testing.T) {
	rec := &state.Record{Inputs: map[string]any{"a": "1"}, Outputs: map[string]any{"id": "x"},
		Pending:   &state.Attempt{Op: "up", Inputs: map[string]any{"a": "2"}, Outputs: map[string]any{"id": "y"}, RawOutput: "id=y"},
		Abandoned: []*state.Attempt{{Op: "up", Inputs: map[string]any{"a": "3"}, Outputs: map[string]any{"id": "z"}}}}
	s := programState(rec)
	if got, want := slices.Sorted(maps.Keys(s)), []string{"abandoned", "inputs", "outputs", "pending"}; !slices.Equal(got, want) {
		t.Errorf("fields %v, want %v", got, want)
	}
	attempt := []string{"inputs", "op", "outputs"}
	if got := slices.Sorted(maps.Keys(s["pending"].(map[string]any))); !slices.Equal(got, attempt) {
		t.Errorf("fields of pending %v, want %v", got, attempt)
	}
	for _, a := range s["abandoned"].([]any) {
		if got := slices.Sorted(maps.Keys(a.(map[string]any))); !slices.Equal(got, attempt) {
			t.Errorf("fields of abandoned %v, want %v", got, attempt)
		}
	}
}
