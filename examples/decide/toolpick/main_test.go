package main

import (
	"context"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

func offered(in llm.Input) []string {
	var names []string
	for _, def := range in.Tools {
		names = append(names, def.Name)
	}
	slices.Sort(names)
	return names
}

func TestHidesIrrelevantToolsForTheWholeTurn(t *testing.T) {
	srv, judge, err := stubJudge()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	model := scriptedModel()
	out, err := runTurn(context.Background(), model, judge, dashitest.Model)
	if err != nil {
		t.Fatal(err)
	}
	if out.Reply == "" {
		t.Fatalf("no reply; stop: %+v", out.Stop)
	}

	want := []string{"calendar_list_events", "clock_now", "contacts_search", "email_send"}
	inputs := model.Inputs()
	if len(inputs) != 4 {
		t.Fatalf("model calls = %d, want 4", len(inputs))
	}
	for i, in := range inputs {
		if got := offered(in); !slices.Equal(got, want) {
			t.Errorf("call %d offered %v, want %v", i+1, got, want)
		}
	}

	facts := modelFacts(out.Turn)
	if len(facts) != 4 {
		t.Fatalf("model_call_started steps = %d, want 4", len(facts))
	}
	for i, fact := range facts {
		if len(fact.Hidden) != len(catalog)-len(want) {
			t.Errorf("call %d hid %d tools, want %d", i+1, len(fact.Hidden), len(catalog)-len(want))
		}
		if !slices.Equal(fact.Hidden, facts[0].Hidden) {
			t.Errorf("call %d hid %v, call 1 hid %v", i+1, fact.Hidden, facts[0].Hidden)
		}
	}
	if n := len(srv.Requests()); n != 1 {
		t.Errorf("Decide requests = %d, want 1 for the turn", n)
	}
}

func TestOffersEveryToolWhenDecideFails(t *testing.T) {
	srv, judge, err := stubJudge()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.FailNext(400) // not retried
	model := scriptedModel()
	if _, err := runTurn(context.Background(), model, judge, dashitest.Model); err != nil {
		t.Fatal(err)
	}
	for i, in := range model.Inputs() {
		if len(in.Tools) != len(catalog) {
			t.Errorf("call %d offered %d tools, want all %d", i+1, len(in.Tools), len(catalog))
		}
	}
}
