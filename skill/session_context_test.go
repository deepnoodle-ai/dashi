package skill_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/skill"
)

func contextBundle(t *testing.T, description string) *skill.Bundle {
	t.Helper()
	bundle, err := skill.New([]skill.Definition{{Name: "review", Description: description, Instructions: "Body"}}, skill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestCatalogRestoresValueAcrossFreshRunnerContinues(t *testing.T) {
	a, b := contextBundle(t, "A"), contextBundle(t, "B")
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(dashitest.Reply("done"))
	var turn dashi.Turn
	for i, bundle := range []*skill.Bundle{a, b, a, a} {
		hooks := bundle.Hooks()
		if i < 3 {
			hooks = dashi.JoinHooks(hooks, dashi.SendHook{Name: "pause", Func: func(context.Context, dashi.SendPlan) (dashi.SendDecision, error) {
				return dashi.SendDecision{Limit: &dashi.LimitDetail{Kind: dashi.LimitHost, Code: "pause"}}, nil
			}})
		}
		runner := dashi.Runner{Client: client, Recorder: rec, Hooks: hooks}
		var cmd dashi.Command = dashi.Continue{}
		if i == 0 {
			cmd = dashi.Start{Agent: dashi.Agent{Model: dashitest.Model}, Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}
		}
		out, err := runner.Run(t.Context(), turn, cmd)
		if err != nil {
			t.Fatal(err)
		}
		turn, err = rec.Load(t.Context(), out.Turn.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	var values, ids []string
	for _, step := range turn.Steps {
		for _, item := range step.Items() {
			if item.Name == "skills" {
				values = append(values, item.Message.Content[0].Reminder.Content)
				ids = append(ids, item.ID)
			}
		}
	}
	if !reflect.DeepEqual(values, []string{a.Catalog(), b.Catalog(), a.Catalog()}) {
		t.Fatalf("catalog values=%q", values)
	}
	if ids[0] != "skills-"+a.Hash() || ids[1] != "skills-"+b.Hash() || ids[2] == ids[0] {
		t.Fatalf("IDs=%q", ids)
	}
	input := client.Inputs()[0]
	if countCatalog(input, a.Catalog()) != 2 || countCatalog(input, b.Catalog()) != 1 {
		t.Fatalf("restored catalog lost: %+v", input)
	}
}

type catalogKeep struct{ keep bool }

func (c catalogKeep) Needed(_ []llm.Message, u llm.Usage) bool { return u.InputTokens >= 100 }
func (c catalogKeep) Start(_ context.Context, messages []llm.Message, _ llm.Usage) (dashi.Start, error) {
	var keep []llm.Message
	if c.keep {
		for _, m := range messages {
			if m.Context != nil && m.Context.Origin == "runtime" && m.Context.Name == "skills" {
				keep = append(keep, m.Clone())
			}
		}
	}
	return dashi.Start{Input: []llm.Message{llm.UserMessage(llm.Text("summarize"))}, Compaction: &dashi.CompactionFact{Keep: keep}}, nil
}

func TestCatalogVisibilityAfterCompactionAndReopen(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(map[bool]string{false: "drop", true: "retain"}[keep], func(t *testing.T) {
			bundle := contextBundle(t, "Review code")
			rec := dashi.NewMemoryRecorder()
			first := dashitest.Reply("one")
			first.Usage = &llm.Usage{InputTokens: 100}
			client := dashitest.NewClient(first, dashitest.Reply("summary"), dashitest.Reply("two"), dashitest.Reply("three"))
			for i := 0; i < 3; i++ {
				s := &session.Session{ID: "catalog", New: i == 0, Runner: dashi.Runner{Client: client, Recorder: rec, Hooks: bundle.Hooks()}, Agent: dashi.Agent{Model: dashitest.Model}}
				if i == 1 {
					s.Compactor = catalogKeep{keep: keep}
				}
				if _, err := s.Send(t.Context(), "go"); err != nil {
					t.Fatal(err)
				}
			}
			for _, input := range client.Inputs()[2:] {
				if n := countCatalog(input, bundle.Catalog()); n != 1 {
					t.Fatalf("copies=%d", n)
				}
			}
		})
	}
}
