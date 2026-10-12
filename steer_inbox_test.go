package dashi

import (
	"testing"
)

func TestMemoryInboxPutAddressesOneTurn(t *testing.T) {
	in := NewMemoryInbox()
	ready, unwatch := in.Ready("t1")
	defer unwatch()
	other, unwatchOther := in.Ready("t2")
	defer unwatchOther()
	mustPut(t, in, "t1", Cancel{CommandID: "cancel"})
	select {
	case <-ready:
	default:
		t.Fatal("Put did not signal its turn")
	}
	select {
	case <-other:
		t.Fatal("Put signaled another turn")
	default:
	}
	if got, _ := in.Waiting(t.Context(), "t2"); len(got) != 0 {
		t.Fatalf("t2 sees %v", got)
	}
	got, err := in.Waiting(t.Context(), "t1")
	if err != nil || len(got) != 1 || CommandIDOf(got[0]) != "cancel" {
		t.Fatalf("t1 sees %v err=%v", got, err)
	}
	if err := in.Delivered(t.Context(), "t1", []string{"cancel"}); err != nil || in.Has("cancel") {
		t.Fatal("Delivered did not remove the command")
	}
}

func TestMemoryInboxPutChecksItsCommand(t *testing.T) {
	in := NewMemoryInbox()
	if err := in.Put("t1", Cancel{}); err == nil {
		t.Fatal("a command without an ID was queued")
	}
	if err := in.Put("", Cancel{CommandID: "c"}); err == nil {
		t.Fatal("a command without a turn was queued")
	}
	if err := in.Put("t1", Steer{}); err == nil {
		t.Fatal("an invalid steer was queued")
	}
	mustPut(t, in, "t1", Cancel{CommandID: "c"})
	if err := in.Put("t1", Cancel{CommandID: "c"}); err == nil {
		t.Fatal("a repeated ID was queued")
	}
}

func TestMemoryInboxLeftoversKeepOtherCommands(t *testing.T) {
	in := NewMemoryInbox()
	mustPut(t, in, "t1", textSteer("s1", "for t1"))
	mustPut(t, in, "t1", Cancel{CommandID: "c"})
	mustPut(t, in, "t2", textSteer("s2", "for t2"))
	left, err := in.Leftovers(Turn{ID: "t1"})
	if err != nil || len(left) != 1 || left[0].Item.ID != "s1" {
		t.Fatalf("left=%+v err=%v", left, err)
	}
	if !in.Has("c") || !in.Has("s2") || in.Has("s1") {
		t.Fatal("Leftovers removed the wrong entries")
	}
}
