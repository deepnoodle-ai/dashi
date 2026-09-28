package wake

import "testing"

func TestSetForgetsUnwatchedTurns(t *testing.T) {
	var s Set
	a, doneA := s.Watch("t1")
	b, doneB := s.Watch("t1")
	if a != b {
		t.Fatal("two watchers of one turn got different channels")
	}
	doneA()
	doneA() // idempotent
	if s.Len() != 1 {
		t.Fatalf("len=%d with one watcher left", s.Len())
	}
	doneB()
	if s.Len() != 0 {
		t.Fatalf("len=%d after every watcher left", s.Len())
	}
	c, doneC := s.Watch("t1")
	s.Notify("t1")
	select {
	case <-c:
	default:
		t.Fatal("notify did not close the channel")
	}
	doneC() // after Notify removed the entry
	if s.Len() != 0 {
		t.Fatalf("len=%d", s.Len())
	}
	// A watcher of an old entry must not remove a newer one.
	_, doneD := s.Watch("t1")
	s.Notify("t1")
	_, doneE := s.Watch("t1")
	doneD()
	if s.Len() != 1 {
		t.Fatalf("an old watcher removed a newer entry: len=%d", s.Len())
	}
	doneE()
}
