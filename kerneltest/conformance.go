package kerneltest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel"
	"github.com/deepnoodle-ai/kernel/llm"
)

// Run checks a recorder against the commit contract Engine.Apply relies on.
// newStore must return an empty store for each subtest; subtests do not share
// turns. Every commit in the suite carries an owner token claimed first, so a
// recorder that requires a lease for every write can still be tested.
//
// Each check follows from the engine's use of the recorder:
//
//   - Steps append in sequence, and Load returns each one with the JSON
//     meaning it was committed with. Key order, whitespace, string escaping,
//     and the spelling of numerically equal numbers may change, and a
//     duplicate key resolves to its last value, so a Postgres jsonb column
//     conforms; the engine compares recorded values by canonical JSON. Step
//     times use microsecond precision, which timestamp columns preserve.
//   - Load of a turn with no steps returns an error matching
//     kernel.ErrTurnNotFound or an empty turn with the requested ID. Apply
//     treats any other Load error as an unreadable record and stops.
//   - A commit whose ExpectedSeq is not the turn's current revision is
//     rejected with kernel.ErrRevisionConflict, including a different step
//     at an occupied sequence number. A step that differs only in its time
//     is different: a worker retrying from an old snapshot builds new steps,
//     and this rejection is what stops it before it repeats a model call or
//     tool execution.
//   - A commit from an owner other than the one last claimed is rejected.
//   - A retry of an acknowledged step with the same JSON meaning succeeds
//     without adding a step, so a commit whose acknowledgement was lost can
//     be retried.
//   - A step with an already recorded command ID succeeds without adding a
//     step when its kind and digest match, and is rejected with
//     kernel.ErrCommandConflict when they differ. Command IDs are scoped to
//     one turn.
//   - Turns are independent: a step for one turn never depends on another
//     turn's revision.
//   - Of concurrent commits at the same revision, exactly one succeeds.
//
// "Rejected" means a *kernel.CommitError with kernel.PersistenceRejected,
// after which Load must show the record unchanged. Run does not check
// recorder behavior Kernel does not rely on, such as validating step payloads.
func Run(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()
	tests := []struct {
		name string
		run  func(*testing.T, Store)
	}{
		{"AppendsInOrder", testAppendsInOrder},
		{"LoadWithoutSteps", testLoadWithoutSteps},
		{"RejectsStaleRevision", testRejectsStaleRevision},
		{"RejectsRetimedStep", testRejectsRetimedStep},
		{"RejectsRevisionGap", testRejectsRevisionGap},
		{"RejectsStaleOwner", testRejectsStaleOwner},
		{"RetriesIdenticalStep", testRetriesIdenticalStep},
		{"RetriesCommand", testRetriesCommand},
		{"RejectsCommandConflict", testRejectsCommandConflict},
		{"IsolatesTurns", testIsolatesTurns},
		{"SerializesConcurrentCommits", testSerializesConcurrentCommits},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, newStore(t))
		})
	}
}

const (
	turnID = "kerneltest-turn"
	owner  = "kerneltest-owner"
)

func testAppendsInOrder(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 3)
	requireRecord(t, s, turnID, steps)
}

func testLoadWithoutSteps(t *testing.T, s Store) {
	turn, err := s.Load(t.Context(), "kerneltest-missing")
	switch {
	case errors.Is(err, kernel.ErrTurnNotFound):
	case err != nil:
		t.Fatalf(
			"load of a turn without steps returned %v; want "+
				"kernel.ErrTurnNotFound, or Apply cannot start the turn",
			err,
		)
	case turn.ID != "kerneltest-missing" || len(turn.Steps) != 0:
		t.Fatalf(
			"load of a turn without steps returned id=%q steps=%d; want "+
				"kernel.ErrTurnNotFound or an empty turn with the requested ID",
			turn.ID,
			len(turn.Steps),
		)
	}
}

func testRejectsStaleRevision(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 2)
	stale := contextStep(turnID, 2, "a different second step")
	err := commit(t, s, stale, owner)
	requireRejected(t, err, kernel.ErrRevisionConflict)
	requireRecord(t, s, turnID, steps)
}

func testRejectsRetimedStep(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 2)
	retimed := steps[1]
	retimed.At = retimed.At.Add(time.Second)
	err := commit(t, s, retimed, owner)
	requireRejected(t, err, kernel.ErrRevisionConflict)
	requireRecord(t, s, turnID, steps)
}

func testRejectsRevisionGap(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 2)
	err := commit(t, s, contextStep(turnID, 4, "skips a revision"), owner)
	requireRejected(t, err, kernel.ErrRevisionConflict)
	requireRecord(t, s, turnID, steps)
}

func testRejectsStaleOwner(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 1)
	claim(t, s, turnID, "kerneltest-takeover")
	next := contextStep(turnID, 2, "from the old owner")
	requireRejected(t, commit(t, s, next, owner), nil)
	requireRecord(t, s, turnID, steps)
	if err := commit(t, s, next, "kerneltest-takeover"); err != nil {
		t.Fatalf("new owner could not commit after claim: %v", err)
	}
	requireRecord(t, s, turnID, append(steps, next))
}

func testRetriesIdenticalStep(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 2)
	if err := commit(t, s, steps[1], owner); err != nil {
		t.Fatalf("identical retry of an acknowledged step failed: %v", err)
	}
	requireRecord(t, s, turnID, steps)
}

func testRetriesCommand(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 1)
	retry := steps[0]
	retry.At = retry.At.Add(time.Second)
	if err := commit(t, s, retry, owner); err != nil {
		t.Fatalf("retry of a recorded command failed: %v", err)
	}
	requireRecord(t, s, turnID, steps)
}

func testRejectsCommandConflict(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 1)
	conflict := steps[0]
	conflict.CommandDigest = "kerneltest-different-digest"
	err := commit(t, s, conflict, owner)
	requireRejected(t, err, kernel.ErrCommandConflict)
	requireRecord(t, s, turnID, steps)
}

func testIsolatesTurns(t *testing.T, s Store) {
	const other = "kerneltest-other-turn"
	steps := seed(t, s, turnID, 2)
	claim(t, s, other, owner)
	err := commit(t, s, contextStep(other, 3, "wrong turn"), owner)
	requireRejected(t, err, kernel.ErrRevisionConflict)
	start := startStep(other)
	start.CommandDigest = "kerneltest-other-digest"
	if err := commit(t, s, start, owner); err != nil {
		t.Fatalf("command ID reused in another turn was not accepted: %v", err)
	}
	requireRecord(t, s, other, []kernel.Step{start})
	requireRecord(t, s, turnID, steps)
}

func testSerializesConcurrentCommits(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 1)
	const writers = 8
	candidates := make([]kernel.Step, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		candidates[i] = contextStep(turnID, 2, fmt.Sprintf("writer %d", i))
		wg.Go(func() {
			errs[i] = commit(t, s, candidates[i], owner)
		})
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		if err != nil {
			continue
		}
		if winner >= 0 {
			t.Fatalf("writers %d and %d both committed seq 2", winner, i)
		}
		winner = i
	}
	if winner < 0 {
		t.Fatalf("no concurrent writer committed seq 2: %v", errs)
	}
	requireRecord(t, s, turnID, append(steps, candidates[winner]))
}

// seed claims the turn and commits n steps: a start followed by context.
func seed(t *testing.T, s Store, id string, n int) []kernel.Step {
	t.Helper()
	claim(t, s, id, owner)
	steps := []kernel.Step{startStep(id)}
	for seq := int64(2); seq <= int64(n); seq++ {
		steps = append(steps, contextStep(id, seq, fmt.Sprintf("step %d", seq)))
	}
	for _, step := range steps {
		if err := commit(t, s, step, owner); err != nil {
			t.Fatalf("commit of seq %d failed: %v", step.Seq, err)
		}
	}
	return steps
}

func claim(t *testing.T, s Store, id, by string) {
	t.Helper()
	if err := s.Claim(t.Context(), id, by); err != nil {
		t.Fatalf("claim %q for %q: %v", id, by, err)
	}
}

func commit(t *testing.T, s Store, step kernel.Step, by string) error {
	return s.Commit(t.Context(), kernel.CommitRequest{
		Step:        step,
		ExpectedSeq: step.Seq - 1,
		Owner:       by,
	})
}

// stepTime has microsecond precision, which timestamp columns preserve.
var stepTime = time.Date(2026, 1, 2, 3, 4, 5, 678901000, time.UTC)

func startStep(id string) kernel.Step {
	def := kernel.Definition{
		ID:       "kerneltest",
		Revision: "r1",
		Provider: "test",
		API:      "test.chat",
		Model:    "fixture",
		Limits:   kernel.DefaultLimits(),
	}
	return kernel.Step{
		Version:       1,
		TurnID:        id,
		Seq:           1,
		At:            stepTime,
		Kind:          kernel.StepStarted,
		CommandID:     "start",
		CommandDigest: "kerneltest-digest",
		Started: &kernel.StartFact{
			Definition: def,
			// Raw JSON that a meaning-preserving store may respell: keys out
			// of jsonb order, extra whitespace, and an exponent number.
			OutputSchema: json.RawMessage(
				`{"required": ["z"], "type": "object", ` +
					`"properties": {"z": {"maxLength": 1e2}}}`,
			),
			Input: []llm.Message{llm.UserMessage(llm.Text("begin <now> & go"))},
		},
	}
}

func contextStep(id string, seq int64, text string) kernel.Step {
	return kernel.Step{
		Version: 1,
		TurnID:  id,
		Seq:     seq,
		At:      stepTime.Add(time.Duration(seq) * time.Millisecond),
		Kind:    kernel.StepDelivered,
		Context: &kernel.ContextFact{
			Items: []kernel.ContextItem{{
				Origin:  "user",
				Message: llm.UserMessage(llm.Text(text)),
			}},
		},
	}
}

func requireRejected(t *testing.T, err error, target error) {
	t.Helper()
	var commitErr *kernel.CommitError
	switch {
	case err == nil:
		t.Fatal("commit succeeded; want a rejected commit")
	case !errors.As(err, &commitErr):
		t.Fatalf("commit error %v is not a *kernel.CommitError", err)
	case commitErr.Persistence != kernel.PersistenceRejected:
		t.Fatalf("commit persistence is %q; want rejected", commitErr.Persistence)
	case target != nil && !errors.Is(err, target):
		t.Fatalf("commit error %v does not wrap %v", err, target)
	}
}

func requireRecord(t *testing.T, s Store, id string, want []kernel.Step) {
	t.Helper()
	turn, err := s.Load(t.Context(), id)
	if err != nil {
		t.Fatalf("load %q: %v", id, err)
	}
	if turn.ID != id {
		t.Fatalf("loaded turn ID %q; want %q", turn.ID, id)
	}
	if len(turn.Steps) != len(want) {
		t.Fatalf("loaded %d steps; want %d", len(turn.Steps), len(want))
	}
	for i := range want {
		got, err := json.Marshal(turn.Steps[i])
		if err != nil {
			t.Fatalf("encode loaded seq %d: %v", i+1, err)
		}
		expected, err := json.Marshal(want[i])
		if err != nil {
			t.Fatalf("encode committed seq %d: %v", i+1, err)
		}
		same, err := sameJSON(got, expected)
		if err != nil {
			t.Fatalf("decode seq %d: %v", i+1, err)
		}
		if !same {
			t.Fatalf(
				"loaded seq %d differs in meaning from the committed step:\n got %s\nwant %s",
				i+1,
				got,
				expected,
			)
		}
	}
}

// sameJSON reports whether a and b have the same JSON meaning: the same
// structure, names, strings, and literals, with numerically equal numbers.
// It matches Kernel's canonical JSON comparison without importing it.
func sameJSON(a, b []byte) (bool, error) {
	x, err := decodeJSON(a)
	if err != nil {
		return false, err
	}
	y, err := decodeJSON(b)
	if err != nil {
		return false, err
	}
	return sameValue(x, y), nil
}

// decodeJSON keeps numbers as their literal text so no precision is lost;
// Go's decoder already resolves a duplicate name to its last value.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func sameValue(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !sameValue(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameValue(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		p, okP := new(big.Rat).SetString(string(x))
		q, okQ := new(big.Rat).SetString(string(y))
		return okP && okQ && p.Cmp(q) == 0
	default:
		return a == b
	}
}
