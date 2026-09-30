package dashi_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestSessionManualCompactionResults(t *testing.T) {
	for _, answer := range []string{"<summary>kept fact</summary>", "<summary> </summary>"} {
		t.Run(answer, func(t *testing.T) {
			rec := dashi.NewMemoryRecorder()
			client := dashitest.NewClient(dashitest.Reply("first answer"), dashitest.Reply(answer), dashitest.Reply("next answer"))
			s := handle(rec, client)
			s.New = true
			s.Compactor = keepLast{limit: 100000}
			out, err := s.Compact(t.Context(), nil)
			if !errors.Is(err, dashi.ErrNothingToCompact) || !errors.Is(err, compaction.ErrNothingToCompact) || !reflect.DeepEqual(out, dashi.Outcome{}) {
				t.Fatalf("no work=%+v, %v", out, err)
			}
			sendAll(t, s, "first")
			if _, err := s.Enqueue(t.Context(), "queued"); err != nil {
				t.Fatal(err)
			}
			out, err = s.Compact(t.Context(), nil)
			if err != nil || out.Origin != dashi.StartCompaction || out.Turn.ID != "s-2" {
				t.Fatalf("compact=%+v err=%v", out, err)
			}
			st, err := s.Open(t.Context())
			if err != nil || st.Queued != 1 || len(client.Inputs()) != 2 {
				t.Fatalf("manual dispatched queue: %+v %v calls=%d", st, err, len(client.Inputs()))
			}
			want := []string{"Summary:\n\nkept fact"}
			if answer == "<summary> </summary>" {
				want = []string{"first", "first answer"}
				if !slices.Contains(codes(out.SessionWarnings), dashi.WarningCompactionFailed) {
					t.Fatal("empty summary lacks failure warning")
				}
			} else if len(out.SessionWarnings) != 0 {
				t.Fatal(out.SessionWarnings)
			}
			reopened := handle(rec, client)
			projected, err := reopened.ProjectedHistory(t.Context())
			if err != nil || !slices.Equal(textsOf(projected), want) {
				t.Fatalf("reopened=%q err=%v", textsOf(projected), err)
			}
			sendAll(t, s, "next")
			if got := textsOf(client.Inputs()[2].Messages); !slices.Equal(got, append(want, "queued", "next")) {
				t.Fatalf("queue order=%q", got)
			}
		})
	}
}

type cappedCompactor struct{ keepLast }

func (c cappedCompactor) Start(h []llm.Message) (dashi.Start, error) {
	start, err := c.keepLast.Start(h)
	start.Budget = &dashi.Budget{MaxOutputTokens: 100}
	start.Agent.Model = "summary-model"
	return start, err
}

func TestSessionManualCompactionBudget(t *testing.T) {
	for _, cap := range []*dashi.Budget{nil, {MaxOutputTokens: 50}} {
		rec := dashi.NewMemoryRecorder()
		client := dashitest.NewClient(dashitest.Reply("answer"), dashitest.Reply("summary"))
		s := handle(rec, client)
		s.New = true
		sendAll(t, s, "first")
		s.Compactor = cappedCompactor{keepLast{limit: 100000}}
		out, err := s.Compact(t.Context(), cap)
		if err != nil {
			t.Fatal(err)
		}
		want := 100
		if cap != nil {
			want = cap.MaxOutputTokens
		}
		fact := out.Turn.Steps[0].Started
		if fact.Budget == nil || fact.Budget.MaxOutputTokens != want || fact.Agent.Model != "summary-model" || !fact.Agent.NoTools {
			t.Fatalf("fact=%+v", fact)
		}
		if client.Inputs()[1].Settings.MaxTokens != int(want) {
			t.Fatalf("max=%d", client.Inputs()[1].Settings.MaxTokens)
		}
	}
}

func TestSessionManualCompactionPreparationAndBusy(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(dashitest.Reply("answer"))
	s := handle(rec, client)
	s.New = true
	sendAll(t, s, "first")
	sentinel := errors.New("cannot prepare summary")
	s.Compactor = keepLast{err: sentinel}
	if _, err := s.Compact(t.Context(), nil); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if len(client.Inputs()) != 1 {
		t.Fatal("preparation called model")
	}
	s.Compactor = keepLast{limit: 10000}
	client.Add(dashitest.Reply("not reached"))
	// An interrupted summary is open and manual compaction cannot close it.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seq := s.StreamCompact(ctx, nil)
	for ev, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Step != nil && ev.Step.Kind == dashi.StepStarted {
			cancel()
			break
		}
	}
	var busy *dashi.TurnBusyError
	if _, err := s.Compact(t.Context(), nil); !errors.As(err, &busy) || busy.Origin != dashi.StartCompaction {
		t.Fatalf("busy=%+v err=%v", busy, err)
	}
	for _, err := range seq {
		if !errors.Is(err, dashi.ErrStreamConsumed) {
			t.Fatal(err)
		}
	}
}

func TestSessionManualCompactionSlotRace(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(dashitest.Reply("answer"), dashitest.Reply("summary"))
	b := handle(rec, client)
	b.New = true
	b.Compactor = keepLast{limit: 10000}
	sendAll(t, b, "first")
	var winner dashi.Outcome
	var winnerErr error
	a := handle(&raceRecorder{MemoryRecorder: rec, before: func() { winner, winnerErr = b.Compact(t.Context(), nil) }}, client)
	a.Compactor = keepLast{limit: 10000}
	loser, err := a.Compact(t.Context(), nil)
	if err != nil || winnerErr != nil || loser.Turn.ID != winner.Turn.ID || loser.Answer != "summary" || len(client.Inputs()) != 2 {
		t.Fatalf("winner=%s/%v loser=%s/%v calls=%d", winner.Turn.ID, winnerErr, loser.Turn.ID, err, len(client.Inputs()))
	}
}

func ExampleSession_Compact() {
	client := dashitest.NewClient(dashitest.Reply("The port is 8443."), dashitest.Reply("<summary>Port 8443.</summary>"), dashitest.Reply("<summary>Port 8443.</summary>"))
	s := &dashi.Session{ID: "example", New: true, Runner: dashi.Runner{Client: client, Recorder: dashi.NewMemoryRecorder()}, Agent: dashi.Agent{Model: "fixture"}, Compactor: &compaction.Compactor{TailFraction: -1}}
	ctx := context.Background()
	if _, err := s.Send(ctx, "Remember the port."); err != nil {
		panic(err)
	}
	out, err := s.Compact(ctx, nil)
	if errors.Is(err, dashi.ErrNothingToCompact) {
		fmt.Println("No history needs a summary.")
		return
	}
	if err != nil {
		panic(err)
	}
	fmt.Println(out.Origin, out.Stop.Kind)
	// The cap belongs to the recorded summary, not later Run commands.
	out, err = s.Compact(ctx, &dashi.Budget{MaxOutputTokens: 100})
	if err != nil {
		panic(err)
	}
	fmt.Println(out.Turn.Steps[0].Started.Budget.MaxOutputTokens)
	// Output:
	// compaction completed
	// 100
}
