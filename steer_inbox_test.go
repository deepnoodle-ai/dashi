package dashi

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestMemoryInboxCopiesQueuedCommands(t *testing.T) {
	// firstText returns the text of the first message's first block, so
	// each case can change data nested several pointers deep.
	firstText := func(messages []llm.Message) *string {
		return &messages[0].Content[0].Block.Text.Text
	}
	cases := []struct {
		name   string
		build  func() Command
		mutate func(Command)
	}{
		{
			name: "AddMessages",
			build: func() Command {
				return AddMessages{CommandID: "c", Items: []ContextItem{
					{ID: "a", Message: llm.UserMessage(llm.Text("first"))},
				}}
			},
			mutate: func(c Command) {
				v := c.(AddMessages)
				v.Items[0].ID = "changed"
				*firstText([]llm.Message{v.Items[0].Message}) = "changed"
			},
		},
		{
			name: "Start",
			build: func() Command {
				return Start{
					CommandID:    "c",
					Agent:        Agent{Tools: []string{"search"}},
					Budget:       &Budget{MaxOutputTokens: 100},
					OutputSchema: json.RawMessage(`null`),
					History:      []llm.Message{llm.UserMessage(llm.Text("before"))},
					Input:        []llm.Message{llm.UserMessage(llm.Text("now"))},
					Items: []ContextItem{
						{ID: "a", Message: llm.UserMessage(llm.Text("item"))},
					},
				}
			},
			mutate: func(c Command) {
				v := c.(Start)
				v.Agent.Tools[0] = "changed"
				v.Budget.MaxOutputTokens = 1
				*firstText(v.History) = "changed"
				*firstText(v.Input) = "changed"
				v.Items[0].ID = "changed"
			},
		},
		{
			name: "Continue",
			build: func() Command {
				return Continue{
					CommandID: "c",
					History:   []llm.Message{llm.UserMessage(llm.Text("before"))},
					Budget:    &Budget{MaxOutputTokens: 100},
				}
			},
			mutate: func(c Command) {
				v := c.(Continue)
				*firstText(v.History) = "changed"
				v.Budget.MaxOutputTokens = 1
			},
		},
		{
			name: "Resolve",
			build: func() Command {
				return Resolve{CommandID: "c", CallID: "call", Outcome: &tool.Outcome{
					State:  tool.StateSucceeded,
					Output: &tool.Output{Content: []content.Block{content.TextBlock("result")}},
					Data:   json.RawMessage(`{"n":1}`),
				}}
			},
			mutate: func(c Command) {
				v := c.(Resolve)
				v.Outcome.Output.Content[0].Text.Text = "changed"
				v.Outcome.Data[0] = '['
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, err := json.Marshal(tc.build())
			if err != nil {
				t.Fatal(err)
			}
			in := NewMemoryInbox()
			command := tc.build()
			mustPut(t, in, "t1", command)
			tc.mutate(command)
			for _, when := range []string{"after the caller changes its command", "after a claimed copy changes"} {
				got, err := in.Waiting(t.Context(), "t1")
				if err != nil || len(got) != 1 {
					t.Fatalf("%s: got %v err=%v", when, got, err)
				}
				b, err := json.Marshal(got[0])
				if err != nil || string(b) != string(want) {
					t.Fatalf("%s:\n got %s\nwant %s (err=%v)", when, b, want, err)
				}
				tc.mutate(got[0])
			}
		})
	}
}

func TestMemoryInboxRefusesCommandThatDoesNotEncode(t *testing.T) {
	in := NewMemoryInbox()
	err := in.Put("t1", Resolve{CommandID: "c", CallID: "call", Outcome: &tool.Outcome{
		State:  tool.StateSucceeded,
		Output: &tool.Output{},
		Data:   json.RawMessage(`{not json`),
	}})
	if err == nil {
		t.Fatal("Put accepted a command that does not encode")
	}
	if got, _ := in.Waiting(t.Context(), "t1"); len(got) != 0 {
		t.Fatalf("refused command was queued: %v", got)
	}
}

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
