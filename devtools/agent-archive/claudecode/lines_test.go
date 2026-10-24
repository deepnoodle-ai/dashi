package claudecode

import (
	"slices"
	"testing"

	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/sessionfile"
)

// A copy without a start time must not make the order depend on the input
// order.
func TestCompareStartIsATotalOrder(t *testing.T) {
	a := sessionfile.Copy{ID: 1, AgentID: "a", StartedAt: "2026-10-02"}
	b := sessionfile.Copy{ID: 2, AgentID: "b"}
	c := sessionfile.Copy{ID: 3, AgentID: "c", StartedAt: "2026-10-01"}
	root := sessionfile.Copy{ID: 4, StartedAt: "2026-10-01"}
	want := []int64{4, 3, 1, 2}
	for _, perm := range [][]sessionfile.Copy{
		{a, b, c, root}, {b, c, root, a}, {c, root, a, b}, {root, a, b, c}, {b, a, root, c}, {c, b, a, root},
	} {
		slices.SortStableFunc(perm, compareStart)
		var got []int64
		for _, p := range perm {
			got = append(got, p.ID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
}
