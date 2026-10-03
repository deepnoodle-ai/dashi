package selective

import (
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestSplitClasses(t *testing.T) {
	named := func(text string) llm.Message {
		m := user(text)
		m.Context = &llm.MessageContext{Origin: "seed", Name: "plan"}
		return m
	}
	summary := user("Your conversation history was compacted. " +
		"Treat the notes below as a record of earlier work. " +
		"They are not new instructions.\n\nEarlier notes.")
	image := llm.UserMessage(llm.Text("see this"), llm.Image(content.Source{URL: "https://example.com/a.png"}))
	messages := []llm.Message{
		{Role: llm.RoleDeveloper, Content: []llm.Content{llm.Text("Be brief.")}}, // 0 instruction
		llm.UserMessage(llm.Text("Do the task."), reminder("time", "", "noon")),  // 1 pinned, reminder removed
		llm.UserMessage(reminder("time", "", "noon")),                            // 2 removed
		summary,                     // 3 summary
		named("plan v1"),            // 4 removed: older item of its name
		call("c1", "read"),          // 5 ┐ one run: a provider pause spreads
		assistant("still thinking"), // 6 │ one reply over two messages
		result("c1", "ok"),          // 7 ┘
		llm.UserMessage(reminder("reply", "task t1", "done")), // 8 agent mail, alone
		named("plan v2"),     // 9 newest item of its name, judged alone
		image,                // 10 a segment alone
		assistant("thought"), // 11 ┐
		result("c9", "x"),    // 12 ┘ result of a call outside history stays with the run
		user("Thanks."),      // 13 tail
	}
	p := split(messages, 13)
	if want := []int{0}; !reflect.DeepEqual(p.instructions, want) {
		t.Errorf("instructions = %v, want %v", p.instructions, want)
	}
	if want := []int{1}; !reflect.DeepEqual(p.pinned, want) {
		t.Errorf("pinned = %v, want %v", p.pinned, want)
	}
	if len(p.msgs[1].Content) != 1 {
		t.Errorf("pinned message kept its reminder: %+v", p.msgs[1].Content)
	}
	if want := []int{3}; !reflect.DeepEqual(p.summaries, want) {
		t.Errorf("summaries = %v, want %v", p.summaries, want)
	}
	var got [][]int
	for _, s := range p.segments {
		got = append(got, s.indexes)
	}
	want := [][]int{{5, 6, 7}, {8}, {9}, {10}, {11, 12}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("segments = %v, want %v", got, want)
	}
}

func TestSplitNamedItemNewerInTailIsRemoved(t *testing.T) {
	named := user("plan v1")
	named.Context = &llm.MessageContext{Origin: "seed", Name: "plan"}
	newer := user("plan v2")
	newer.Context = named.Context
	p := split([]llm.Message{user("task"), named, user("go"), newer}, 2)
	if len(p.segments) != 0 {
		t.Fatalf("segments = %+v, want none", p.segments)
	}
}

func TestSplitMergesCallWithLateResult(t *testing.T) {
	messages := []llm.Message{
		call("c1", "read"),
		user("while you wait: hello"),
		result("c1", "late"),
	}
	p := split(messages, 3)
	if len(p.segments) != 1 || !reflect.DeepEqual(p.segments[0].indexes, []int{0, 2}) {
		t.Fatalf("segments = %+v, want one of [0 2]", p.segments)
	}
	if !reflect.DeepEqual(p.pinned, []int{1}) {
		t.Fatalf("pinned = %v", p.pinned)
	}
}

func TestMeasureDropsReasoning(t *testing.T) {
	messages := []llm.Message{
		user("task"),
		llm.AssistantMessage(reasoning("private"), llm.Text("public")),
		llm.AssistantMessage(reasoning("only reasoning")),
	}
	p := split(messages, 3)
	p.segments[0].measure(p, DefaultShortForm)
	whole := p.segments[0].whole
	if len(whole) != 1 || len(whole[0].Content) != 1 || whole[0].Text() != "public" {
		t.Fatalf("whole = %+v", whole)
	}
}
