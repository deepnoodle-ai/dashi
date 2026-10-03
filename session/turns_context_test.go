package session_test

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/skill"
)

// jsonContextRecorder makes every record cross a JSON storage boundary.
type jsonContextRecorder struct{ *dashi.MemoryRecorder }

func (r jsonContextRecorder) Commit(ctx context.Context, req dashi.CommitRequest) error {
	data, err := json.Marshal(req.Step)
	if err != nil {
		return err
	}
	var step dashi.Step
	if err := json.Unmarshal(data, &step); err != nil {
		return err
	}
	req.Step = step
	return r.MemoryRecorder.Commit(ctx, req)
}

func (r jsonContextRecorder) Load(ctx context.Context, id string) (dashi.Turn, error) {
	turn, err := r.MemoryRecorder.Load(ctx, id)
	if err != nil {
		return turn, err
	}
	data, err := json.Marshal(turn)
	if err != nil {
		return turn, err
	}
	var restored dashi.Turn
	err = json.Unmarshal(data, &restored)
	return restored, err
}

func namedContext(messages []llm.Message, origin, name string) []llm.Message {
	var result []llm.Message
	for _, m := range messages {
		if m.Context != nil && m.Context.Origin == origin && m.Context.Name == name {
			result = append(result, m)
		}
	}
	return result
}

func TestSessionSeedAndCatalogStayVisibleAcrossFreshHandles(t *testing.T) {
	for _, message := range []llm.Message{
		llm.UserMessage(llm.Text("persistent")),
		llm.ReminderMessage(llm.Reminder{Name: "seed", Tier: llm.ReminderContextual, Content: "persistent"}),
		llm.UserMessage(llm.Text("first block"), llm.Text("second block")),
	} {
		t.Run(string(message.Content[0].Kind)+message.Text(), func(t *testing.T) {
			rec := jsonContextRecorder{dashi.NewMemoryRecorder()}
			client := dashitest.NewClient(dashitest.Reply("one"), dashitest.Reply("two"), dashitest.Reply("three"))
			bundle, err := skill.New([]skill.Definition{{Name: "review", Description: "Review code", Instructions: "Body"}}, skill.Options{})
			if err != nil {
				t.Fatal(err)
			}
			seed := dashi.SeedContext(dashi.ContextItem{Name: "seed", Message: message})
			for i := 0; i < 3; i++ {
				s := handle(rec, client)
				s.New = i == 0
				s.Runner.Hooks = dashi.JoinHooks(seed, bundle)
				if _, err := s.Send(t.Context(), "go"); err != nil {
					t.Fatal(err)
				}
			}
			for i, input := range client.Inputs() {
				for _, slot := range [][2]string{{"user", "seed"}, {"runtime", "skills"}} {
					if got := len(namedContext(input.Messages, slot[0], slot[1])); got != 1 {
						t.Fatalf("request %d slot %v has %d copies", i, slot, got)
					}
				}
			}
		})
	}
}

func TestSeedContextAppendsLatestValueAcrossTurns(t *testing.T) {
	rec := jsonContextRecorder{dashi.NewMemoryRecorder()}
	client := dashitest.NewClient(dashitest.Reply("one"), dashitest.Reply("two"), dashitest.Reply("three"), dashitest.Reply("four"))
	for i, value := range []string{"A", "B", "A", "A"} {
		s := handle(rec, client)
		s.New = i == 0
		// The changed ID alone on the fourth turn must not add the seed again.
		s.Runner.Hooks = dashi.JoinHooks(dashi.SeedContext(dashi.ContextItem{ID: value + string(rune('0'+i)), Name: "seed", Snapshot: true, Message: llm.UserMessage(llm.Text(value))}))
		if _, err := s.Send(t.Context(), "go"); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, m := range namedContext(client.Inputs()[3].Messages, "user", "seed") {
		got = append(got, m.Text())
	}
	if !reflect.DeepEqual(got, []string{"A", "B", "A"}) {
		t.Fatalf("values = %q", got)
	}
}

func TestSeedContextRestoresValueWithFreshIDAfterContinue(t *testing.T) {
	rec := jsonContextRecorder{dashi.NewMemoryRecorder()}
	client := dashitest.NewClient(dashitest.Reply("done"))
	var turn dashi.Turn
	for i, value := range []string{"A", "B", "A"} {
		hooks := dashi.JoinHooks(dashi.SeedContext(dashi.ContextItem{ID: "seed", Name: "seed", Snapshot: true, Message: llm.UserMessage(llm.Text(value))}))
		if i < 2 {
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
		if i < 2 && out.Stop.Kind != dashi.StopLimited {
			t.Fatalf("stop = %+v", out.Stop)
		}
		turn, err = rec.Load(t.Context(), out.Turn.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	var ids, values []string
	for _, step := range turn.Steps {
		for _, item := range step.Items() {
			if item.Name == "seed" {
				ids = append(ids, item.ID)
				values = append(values, item.Message.Text())
			}
		}
	}
	if !reflect.DeepEqual(values, []string{"A", "B", "A"}) || len(ids) != 3 || ids[0] != "seed" || ids[1] == ids[0] || ids[2] == ids[1] || ids[2] == ids[0] {
		t.Fatalf("values=%q ids=%q", values, ids)
	}
	if got := namedContext(client.Inputs()[0].Messages, "user", "seed"); len(got) != 3 {
		t.Fatalf("visible = %+v", got)
	}
}

// contextKeep retains all seed messages, just the first, or none. Legacy mode
// represents CompactionFact.Keep written before message context metadata.
type contextKeep struct {
	mode   string
	legacy bool
}

func (c contextKeep) Needed(_ []llm.Message, usage llm.Usage) bool { return usage.InputTokens >= 100 }
func (c contextKeep) Start(_ context.Context, messages []llm.Message) (dashi.Start, error) {
	var keep []llm.Message
	for _, m := range namedContext(messages, "user", "seed") {
		if c.mode == "none" || c.mode == "first" && len(keep) > 0 {
			continue
		}
		m = m.Clone()
		keep = append(keep, m)
	}
	fact := &dashi.CompactionFact{Keep: keep}
	if c.legacy {
		data, err := os.ReadFile("testdata/session-context-legacy-keep.json")
		if err != nil {
			return dashi.Start{}, err
		}
		fact = new(dashi.CompactionFact)
		if err := json.Unmarshal(data, fact); err != nil {
			return dashi.Start{}, err
		}
	}
	return dashi.Start{Input: []llm.Message{llm.UserMessage(llm.Text("Summarize"))}, Compaction: fact}, nil
}

func TestSessionContextUsesRetainedCompactionMessages(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		legacy     bool
		before     []string
		want       []string
	}{
		{"retain current", "all", false, []string{"A"}, []string{"A"}},
		{"drop current", "none", false, []string{"A"}, []string{"A"}},
		{"retain older", "first", false, []string{"A", "B"}, []string{"A", "B"}},
		{"legacy keep", "all", true, []string{"A"}, []string{"A"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := jsonContextRecorder{dashi.NewMemoryRecorder()}
			var replies []llm.Result
			for i := range tc.before {
				tokens := 0
				if i == len(tc.before)-1 {
					tokens = 100
				}
				replies = append(replies, used("ok", tokens))
			}
			// The summary mentions the value, but must never establish presence.
			replies = append(replies, dashitest.Reply("A and B are context"), dashitest.Reply("after"), dashitest.Reply("reopened"))
			client := dashitest.NewClient(replies...)
			for i, value := range tc.before {
				s := handle(rec, client)
				s.New = i == 0
				s.Runner.Hooks = dashi.JoinHooks(dashi.SeedContext(dashi.ContextItem{Name: "seed", Message: llm.UserMessage(llm.Text(value))}))
				if _, err := s.Send(t.Context(), "go"); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				s := handle(rec, client)
				if i == 0 {
					s.Compactor = contextKeep{mode: tc.mode, legacy: tc.legacy}
				}
				s.Runner.Hooks = dashi.JoinHooks(dashi.SeedContext(dashi.ContextItem{Name: "seed", Message: llm.UserMessage(llm.Text(tc.before[len(tc.before)-1]))}))
				if _, err := s.Send(t.Context(), "next"); err != nil {
					t.Fatal(err)
				}
			}
			for _, input := range client.Inputs()[len(tc.before)+1:] {
				var values []string
				for _, m := range namedContext(input.Messages, "user", "seed") {
					values = append(values, m.Text())
				}
				if !reflect.DeepEqual(values, tc.want) {
					t.Fatalf("values=%q want=%q", values, tc.want)
				}
				if tc.legacy {
					unidentified := 0
					for _, m := range input.Messages {
						if m.Context == nil && m.Text() == "A" {
							unidentified++
						}
					}
					if unidentified != 1 {
						t.Fatalf("old copies=%d", unidentified)
					}
				}
			}
		})
	}
}

func TestSeedContextDoesNotInferIdentityOrConsultTurnView(t *testing.T) {
	seed := dashi.SeedContext(dashi.ContextItem{Name: "seed", Message: llm.UserMessage(llm.Text("A"))})
	for _, messages := range [][]llm.Message{
		{llm.UserMessage(llm.Text("A"))},
		{{Role: llm.RoleUser, Content: []llm.Content{llm.Text("A")}, Context: &llm.MessageContext{Origin: "runtime", Name: "seed"}}},
		{{Role: llm.RoleUser, Content: []llm.Content{llm.Text("A")}, Context: &llm.MessageContext{Origin: "user", Name: "other"}}},
	} {
		d, err := seed.Func(t.Context(), dashi.ModelPlan{Input: llm.Input{Messages: messages}})
		if err != nil || len(d.AddMessages) != 1 {
			t.Fatalf("decision=%+v err=%v", d, err)
		}
	}
	// TurnView.Context sees the seed, but a plan whose input lacks it must
	// add it.
	runner := dashi.Runner{Client: dashitest.NewClient(dashitest.Reply("ok")), Hooks: dashi.JoinHooks(seed)}
	out, err := runner.Run(t.Context(), dashi.Turn{}, dashi.Start{Agent: dashi.Agent{Model: dashitest.Model}, Input: []llm.Message{llm.UserMessage(llm.Text("go"))}})
	if err != nil {
		t.Fatal(err)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	d, err := seed.Func(t.Context(), dashi.ModelPlan{Turn: view})
	if err != nil || len(d.AddMessages) != 1 {
		t.Fatalf("decision=%+v err=%v", d, err)
	}
}
