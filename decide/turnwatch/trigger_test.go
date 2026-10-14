package turnwatch_test

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/turnwatch"
	"github.com/deepnoodle-ai/dashi/llm"
)

// TestNotExecutedAgainReminds repeats a call whose arguments the runner
// rejects, so it never runs. The model sees the same error twice, which
// is a failed call repeated, so the second one triggers failed_again.
func TestNotExecutedAgainReminds(t *testing.T) {
	srv, client := judge(t, 0.95, 0.05)
	bad := func(id string) llm.Result {
		return dashitest.CallTool(id, "run_tests", `{"package":5}`)
	}
	model := dashitest.NewClient(bad("c1"), bad("c2"), dashitest.Reply("Stuck."))
	out := run(t, &turnwatch.Watcher{Client: client}, model, dashi.Turn{ID: "t1"}, start(dashi.Agent{}))
	if len(srv.Requests()) != 1 {
		t.Fatalf("%d Decide requests, want 1", len(srv.Requests()))
	}
	if r := watcherResults(out.Turn); len(r) != 1 ||
		!strings.HasPrefix(r[0].Reason, "repeating: failed_again: run_tests, 2 times;") {
		t.Fatalf("hook results = %+v", r)
	}
}
