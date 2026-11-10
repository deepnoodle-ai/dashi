package delegate

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// testSteps returns the events of n steps of Turn id: a model call started
// at Seq 2 and finished at Seq 3, then plain steps, ending in a stop.
func testSteps(id string, n int) []dashi.Event {
	var out []dashi.Event
	for seq := int64(1); seq <= int64(n); seq++ {
		s := dashi.Step{TurnID: id, Seq: seq, Kind: dashi.StepMessagesAdded}
		switch {
		case seq == 2:
			s.Kind, s.Model = dashi.StepModelCallStarted, &dashi.ModelFact{ModelCallID: "c1", MessageID: "m1"}
		case seq == 3:
			s.Kind, s.Model = dashi.StepModelCallFinished, &dashi.ModelFact{ModelCallID: "c1", MessageID: "m1"}
		case seq == int64(n):
			s.Kind = dashi.StepTurnStopped
		}
		out = append(out, dashi.Event{TurnID: id, Seq: seq, Step: &s})
	}
	return out
}

func delta(id, text string) dashi.Event {
	return dashi.Event{TurnID: id, Seq: 2, ModelCallID: "c1", MessageID: "m1", Delta: &llm.Delta{Kind: llm.DeltaText, Text: text}}
}

// trace is a compact record of what a watch yielded: "T<id>:<n steps>",
// "<id>#<seq>", "<id>~<delta text>", "<id>gap", or "missing <id>".
func trace(updates []TaskUpdate) string {
	var parts []string
	for _, u := range updates {
		switch u.Kind {
		case TaskUpdateTurn:
			parts = append(parts, fmt.Sprintf("T%s:%d", u.Turn.ID, len(u.Turn.Steps)))
		case TaskUpdateRecordEnd:
			parts = append(parts, "record_end")
		case TaskUpdateMissingSteps:
			parts = append(parts, "missing "+u.TurnID)
		case TaskUpdateEvent:
			e := u.Event
			switch {
			case e.Step != nil:
				parts = append(parts, fmt.Sprintf("%s#%d", e.TurnID, e.Seq))
			case e.Gap:
				parts = append(parts, e.TurnID+"gap")
			case e.Delta != nil:
				parts = append(parts, e.TurnID+"~"+e.Delta.Text)
			}
		}
	}
	return strings.Join(parts, " ")
}

func stepsOf(events []dashi.Event) []dashi.Step {
	var out []dashi.Step
	for _, e := range events {
		out = append(out, *e.Step)
	}
	return out
}

// reloadDue lets the gap wait pass and returns the Turns due for a reload.
func reloadDue(w *watchState) []string {
	start := time.Unix(1000, 0)
	w.checkMissing(start)
	due, _ := w.checkMissing(start.Add(watchMissingWait))
	return due
}

func TestWatchStateReplaysTheRecordThenHeldLiveEvents(t *testing.T) {
	ev := testSteps("s-1", 6)
	w := newWatchState()
	// Steps 3.. arrive while the record of steps 1..4 loads.
	for _, e := range ev[2:] {
		w.push(e)
	}
	if len(w.take()) != 0 {
		t.Fatal("events yielded before the record")
	}
	w.opened([]dashi.Turn{{ID: "s-1", Steps: stepsOf(ev[:4])}}, nil)
	if got, want := trace(w.take()), "Ts-1:4 record_end s-1#5 s-1#6"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if len(w.held) != 0 {
		t.Fatalf("held=%v", w.held)
	}
}

func TestWatchStateOrdersStepsAndDropsDuplicates(t *testing.T) {
	ev := testSteps("s-1", 6)
	w := newWatchState()
	w.opened(nil, nil)
	for _, i := range []int{0, 2, 1, 1, 3, 0, 5, 4} {
		w.push(ev[i])
	}
	if got, want := trace(w.take()), "record_end s-1#1 s-1#2 s-1#3 s-1#4 s-1#5 s-1#6"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWatchStateFillsAGapFromTheRecord(t *testing.T) {
	ev := testSteps("s-1", 6)
	w := newWatchState()
	w.opened(nil, nil)
	w.push(ev[0])
	w.push(ev[1])
	w.push(ev[3]) // Step 3 is only in the record.
	start := time.Unix(1000, 0)
	if due, next := w.checkMissing(start); len(due) != 0 || next != watchMissingWait {
		t.Fatalf("a new gap must wait: due=%v next=%v", due, next)
	}
	if due, next := w.checkMissing(start.Add(50 * time.Millisecond)); len(due) != 0 || next != 50*time.Millisecond {
		t.Fatalf("due=%v next=%v", due, next)
	}
	due, _ := w.checkMissing(start.Add(watchMissingWait))
	if len(due) != 1 || due[0] != "s-1" {
		t.Fatalf("due=%v", due)
	}
	// Events keep arriving during the reload, and wait.
	w.push(ev[4])
	w.reloaded("s-1", dashi.Turn{ID: "s-1", Steps: stepsOf(ev[:3])}, nil)
	w.push(ev[5])
	if got, want := trace(w.take()), "record_end s-1#1 s-1#2 s-1#3 s-1#4 s-1#5 s-1#6"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// Each new gap waits its own watchMissingWait, even when an earlier gap
// fills while later steps stay held.
func TestWatchStateNewGapStartsItsOwnWait(t *testing.T) {
	ev := testSteps("s-1", 8)
	w := newWatchState()
	w.opened(nil, nil)
	w.push(ev[0])
	w.push(ev[1])
	w.push(ev[2])
	w.push(ev[4]) // Step 4 is late.
	w.push(ev[6]) // Step 6 is late.
	start := time.Unix(1000, 0)
	w.checkMissing(start)
	w.push(ev[3]) // Step 4 arrives; step 5 yields; the gap is now at 6.
	seen := start.Add(90 * time.Millisecond)
	if due, next := w.checkMissing(seen); len(due) != 0 || next != watchMissingWait {
		t.Fatalf("the gap at 6 must start a new wait: due=%v next=%v", due, next)
	}
	if due, _ := w.checkMissing(seen.Add(watchMissingWait - time.Millisecond)); len(due) != 0 {
		t.Fatalf("reloaded early: due=%v", due)
	}
	if due, _ := w.checkMissing(seen.Add(watchMissingWait)); len(due) != 1 || due[0] != "s-1" {
		t.Fatalf("due=%v", due)
	}
	if got, want := trace(w.take()), "record_end s-1#1 s-1#2 s-1#3 s-1#4 s-1#5"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// A step past a record that covers the gap is a new gap, not a lost step.
func TestWatchStateReloadKeepsAStepThatArrivesLate(t *testing.T) {
	ev := testSteps("s-1", 8)
	w := newWatchState()
	w.opened(nil, nil)
	w.push(ev[0])
	w.push(ev[1])
	w.push(ev[3])
	if due := reloadDue(w); len(due) != 1 {
		t.Fatalf("due=%v", due)
	}
	w.push(ev[6]) // Step 7 overtakes step 6 during the load.
	w.reloaded("s-1", dashi.Turn{ID: "s-1", Steps: stepsOf(ev[:5])}, nil)
	for _, e := range ev[5:] {
		w.push(e)
	}
	if got, want := trace(w.take()), "record_end s-1#1 s-1#2 s-1#3 s-1#4 s-1#5 s-1#6 s-1#7 s-1#8"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWatchStateReportsStepsTheRecordLacks(t *testing.T) {
	ev := testSteps("s-1", 6)
	w := newWatchState()
	w.opened(nil, nil)
	w.push(ev[0])
	w.push(ev[3])
	w.push(ev[4])
	reloadDue(w)
	w.reloaded("s-1", dashi.Turn{ID: "s-1", Steps: stepsOf(ev[:1])}, nil)
	if got, want := trace(w.take()), "record_end s-1#1 missing s-1 s-1#4 s-1#5"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if due, _ := w.checkMissing(time.Unix(2000, 0)); len(due) != 0 {
		t.Fatal("a reported gap loaded again")
	}
}

func TestWatchStateFailedReloadContinuesFromTheHeldStep(t *testing.T) {
	ev := testSteps("s-1", 6)
	w := newWatchState()
	w.opened(nil, nil)
	w.push(ev[0])
	w.push(ev[3])
	reloadDue(w)
	w.reloaded("s-1", dashi.Turn{}, errors.New("disk gone"))
	out := w.take()
	if got, want := trace(out), "record_end s-1#1 missing s-1 s-1#4"; got != want || out[2].Err.Error() != "disk gone" {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWatchStateFailedOpenGoesLive(t *testing.T) {
	ev := testSteps("s-1", 6)
	w := newWatchState()
	w.push(ev[3])
	w.opened(nil, errors.New("record locked"))
	// A Turn met after the failure starts at its first record_end step.
	w.push(dashi.Event{TurnID: "s-2", Seq: 7, Step: &dashi.Step{Seq: 7, Kind: dashi.StepMessagesAdded}})
	out := w.take()
	if got, want := trace(out), "missing  record_end s-1#4 s-2#7"; got != want || out[0].Err.Error() != "record locked" {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWatchStateLaterTurnWaitsForAnEarlierGap(t *testing.T) {
	one, two := testSteps("s-1", 6), testSteps("s-2", 4)
	w := newWatchState()
	w.opened(nil, nil)
	w.push(one[0])
	w.push(one[2]) // Step 2 of Turn 1 is late.
	w.push(two[0])
	w.push(two[1])
	if got, want := trace(w.take()), "record_end s-1#1"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// Only the earlier Turn reloads.
	if due := reloadDue(w); len(due) != 1 || due[0] != "s-1" {
		t.Fatalf("due=%v", due)
	}
	w.reloaded("s-1", dashi.Turn{ID: "s-1", Steps: stepsOf(one)}, nil)
	if got, want := trace(w.take()), "s-1#2 s-1#3 s-1#4 s-1#5 s-1#6 s-2#1 s-2#2"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWatchStatePreviewsFollowTheirStep(t *testing.T) {
	ev := testSteps("s-1", 6)
	w := newWatchState()
	w.opened(nil, nil)
	w.push(ev[0])
	w.push(delta("s-1", "The notes")) // Ahead of its started step.
	w.push(ev[1])
	w.push(dashi.Event{TurnID: "s-1", Seq: 2, ModelCallID: "c1", MessageID: "m1", Gap: true})
	w.push(delta("s-1", " say")) // After the gap: dropped.
	w.push(ev[2])
	w.push(delta("s-1", " stale")) // After its message finished.
	if got, want := trace(w.take()), "record_end s-1#1 s-1#2 s-1~The notes s-1gap s-1#3"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWatchStateCapsHeldPreviews(t *testing.T) {
	w := newWatchState()
	w.opened(nil, nil)
	for i := range watchPreviewLimit + 1 {
		e := delta("s-1", "x")
		e.MessageID = fmt.Sprintf("m%d", i%3)
		w.push(e)
	}
	if w.heldPreviews != 0 || len(w.held["s-1"]) != 3 {
		t.Fatalf("held previews=%d events=%d", w.heldPreviews, len(w.held["s-1"]))
	}
	for _, e := range testSteps("s-1", 2) {
		w.push(e)
	}
	if got, want := trace(w.take()), "record_end s-1#1 s-1#2 s-1gap s-1gap s-1gap"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWatchQueueTurnsOverflowIntoGaps(t *testing.T) {
	q := &watchQueue{wake: make(chan struct{}, 1), lost: map[string]dashi.Event{}}
	for range watchPreviewLimit + 5 {
		q.add(delta("s-1", "x"))
	}
	q.add(testSteps("s-1", 1)[0])
	events, lost, _ := q.take()
	if len(events) != watchPreviewLimit+1 || len(lost) != 1 || !lost[0].Gap {
		t.Fatalf("events=%d lost=%+v", len(events), lost)
	}
}
