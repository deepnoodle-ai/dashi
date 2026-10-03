package selective

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/decide/patterns/compact"
)

// start runs Start and returns it with the one Selection it reported.
func start(t *testing.T, c *Compactor, messages []llm.Message) (dashi.Start, Selection) {
	t.Helper()
	var got []Selection
	c.OnSelection = func(s Selection) { got = append(got, s) }
	st, err := c.Start(context.Background(), messages)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("OnSelection called %d times, want 1", len(got))
	}
	return st, got[0]
}

func actions(sel Selection) []Action {
	var out []Action
	for _, s := range sel.Segments {
		out = append(out, s.Action)
	}
	return out
}

func TestStartKeepsNeededSegments(t *testing.T) {
	srv, client := judge(t, map[string]float64{
		"s0.needed": 0.1, "s0.verbatim": 0.2,
		"s1.needed": 0.9,
		"s2.needed": 0.3,
	})
	messages := history()
	c := &Compactor{Base: smallWindow(), Client: client}
	st, sel := start(t, c, messages)
	if sel.Fallback != "" {
		t.Fatalf("fell back: %s", sel.Fallback)
	}
	if want := []Action{ActionDrop, ActionKeep, ActionDrop}; !reflect.DeepEqual(actions(sel), want) {
		t.Fatalf("actions = %v, want %v", actions(sel), want)
	}

	f := st.Compaction
	want := []llm.Message{messages[0], messages[1], messages[4], messages[5], messages[7], messages[8]}
	if !reflect.DeepEqual(f.Keep, want) {
		t.Fatalf("keep = %s", texts(f.Keep))
	}
	if f.SummaryAt != 4 {
		t.Fatalf("summary at %d, want 4 (before the tail)", f.SummaryAt)
	}
	hash, _ := dashi.HistoryHash(messages)
	if f.Hash != hash || sel.Hash != hash {
		t.Fatalf("hash = %q, selection %q, want %q", f.Hash, sel.Hash, hash)
	}
	base := smallWindow()
	stock, _ := base.Start(context.Background(), messages)
	if f.Frame != stock.Compaction.Frame {
		t.Fatalf("frame %q is not the stock frame", f.Frame)
	}
	summary := llm.UserMessage(llm.Text(f.Frame + "\n\nnotes"))
	if !compaction.IsSummary(summary) {
		t.Fatal("IsSummary does not recognize the summary")
	}
	if st.Origin != dashi.StartCompaction || !st.Agent.NoTools {
		t.Fatalf("start = %+v", st)
	}

	request := requestText(t, st.Input)
	for _, s := range []string{keptMark, "Fix the parser tests", "heartbeat", idea, "leaves your context"} {
		if !strings.Contains(request, s) {
			t.Errorf("summary request lacks %q", s)
		}
	}
	if strings.Contains(request, needle) || strings.Contains(request, "Rerunning") {
		t.Errorf("summary request covers a kept message:\n%s", request)
	}

	if len(sel.Requests) == 0 || sel.Usage.InputTokens == 0 ||
		!reflect.DeepEqual(sel.Models, []string{"jev-1.13.0"}) {
		t.Fatalf("selection spend = %+v %+v %v", sel.Usage, sel.Requests, sel.Models)
	}
	if len(sel.Requests) != len(srv.Requests()) {
		t.Fatalf("%d request IDs for %d requests", len(sel.Requests), len(srv.Requests()))
	}
	if s := sel.Segments[1]; !reflect.DeepEqual(s.Messages, []int{4, 5}) ||
		s.Scores.Needed != 0.9 || s.Size == 0 {
		t.Fatalf("segment 1 = %+v", s)
	}
	keys := questionKeys(srv)
	if _, ok := keys["s1.verbatim"]; ok {
		t.Error("asked about the exact text of a segment with no short form")
	}
	if _, ok := keys["s0.verbatim"]; !ok {
		t.Error("did not ask about the exact text of a segment with a short form")
	}
}

func texts(msgs []llm.Message) string {
	var out []string
	for _, m := range msgs {
		out = append(out, string(m.Role)+":"+render([]llm.Message{m}))
	}
	return strings.Join(out, " | ")
}

func TestStartKeepsReplayDataButNotReasoning(t *testing.T) {
	messages := history()
	c4 := messages[4].Content[0]
	c4.Replay = &llm.Opaque{Version: 1, Type: "signature", Unit: llm.OpaqueUnitContentBlock, Raw: []byte(`{"sig":"x"}`)}
	messages[4] = llm.AssistantMessage(reasoning("secret"), c4)
	_, client := judge(t, map[string]float64{"s1.needed": 0.9})
	st, sel := start(t, &Compactor{Base: smallWindow(), Client: client}, messages)
	if sel.Fallback != "" {
		t.Fatalf("fell back: %s", sel.Fallback)
	}
	kept := st.Compaction.Keep[2]
	if len(kept.Content) != 1 || kept.Content[0].ToolCall == nil || kept.Content[0].Replay == nil {
		t.Fatalf("kept call = %+v", kept.Content)
	}
}

func TestThresholds(t *testing.T) {
	tests := []struct {
		name    string
		answers map[string]float64
		c       Compactor
		want    []Action
	}{
		{
			name:    "zero floor needs above 0.5",
			answers: map[string]float64{"s0.needed": 0.5, "s0.verbatim": 0.9, "s1.needed": 0.51, "s2.needed": 0.5},
			want:    []Action{ActionDrop, ActionKeep, ActionDrop},
		},
		{
			name:    "negative floor keeps by rank",
			answers: map[string]float64{"s0.needed": 0.01, "s0.verbatim": 0.01, "s1.needed": 0.02, "s2.needed": 0.03},
			c:       Compactor{MinNeeded: -1},
			want:    []Action{ActionShort, ActionKeep, ActionKeep},
		},
		{
			name:    "positive floor",
			answers: map[string]float64{"s0.needed": 0.69, "s1.needed": 0.7, "s2.needed": 0.95},
			c:       Compactor{MinNeeded: 0.7, MinVerbatim: -1},
			want:    []Action{ActionDrop, ActionKeep, ActionKeep},
		},
		{
			name:    "zero verbatim floor keeps whole only above 0.5",
			answers: map[string]float64{"s0.needed": 0.9, "s0.verbatim": 0.5, "s1.needed": 0.9},
			want:    []Action{ActionShort, ActionKeep, ActionDrop},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, client := judge(t, tt.answers)
			c := tt.c
			c.Base, c.Client = compaction.Compactor{}, client // a 128,000-token window
			_, sel := start(t, &c, history())
			if sel.Fallback != "" {
				t.Fatalf("fell back: %s", sel.Fallback)
			}
			if !reflect.DeepEqual(actions(sel), tt.want) {
				t.Fatalf("actions = %v, want %v", actions(sel), tt.want)
			}
		})
	}
}

func TestNegativeMinVerbatimAsksOnlyNeeded(t *testing.T) {
	srv, client := judge(t, map[string]float64{"s0.needed": 0.9})
	c := &Compactor{Client: client, MinVerbatim: -1}
	_, sel := start(t, c, history())
	if want := []Action{ActionKeep, ActionDrop, ActionDrop}; !reflect.DeepEqual(actions(sel), want) {
		t.Fatalf("actions = %v, want %v", actions(sel), want)
	}
	for key := range questionKeys(srv) {
		if strings.HasSuffix(key, ".verbatim") {
			t.Fatalf("asked %s", key)
		}
	}
}

func TestTooLargeSegmentJudgedByShortForm(t *testing.T) {
	messages := history()
	huge := bigLog(1_000) // about 50 KB, over Decide's request limit
	messages[3] = result("c1", huge)
	srv, client := judge(t, map[string]float64{"s0.needed": 0.9, "s0.verbatim": 0.9, "s1.needed": 0.9})
	st, sel := start(t, &Compactor{Client: client}, messages)
	if sel.Fallback != "" {
		t.Fatalf("fell back: %s", sel.Fallback)
	}
	s0 := sel.Segments[0]
	if !s0.ShortJudged || s0.Action != ActionShort {
		t.Fatalf("segment 0 = %+v", s0)
	}
	q := questionKeys(srv)
	if _, ok := q["s0.verbatim"]; ok {
		t.Fatal("asked about the exact text of a segment judged by its short form")
	}
	b, _ := q["s0.needed"].MarshalJSON()
	if len(b) > 5_000 || !strings.Contains(string(b), "cut in the short form") {
		t.Fatalf("segment 0 was not asked about by its short form (%d bytes)", len(b))
	}
	kept := st.Compaction.Keep[2:4]
	r := kept[1].Content[0].ToolResult
	if kept[0].Content[0].ToolCall.ID != "c1" || r == nil || r.CallID != "c1" ||
		len(r.Output.Content[0].Text.Text) >= len(huge) {
		t.Fatalf("segment 0 not kept short: %.300s", texts(kept))
	}
	if !strings.Contains(requestText(t, st.Input), "heartbeat") {
		t.Fatal("the summary does not cover the full text of a shortened segment")
	}
}

func TestTooLargeEvenShortIsDropped(t *testing.T) {
	messages := history()
	messages[3] = result("c1", bigLog(1_000))
	// A short form that is smaller, but still too large to ask about.
	shortForm := func(seg []llm.Message) ([]llm.Message, bool) {
		if len(seg) != 2 || seg[1].Content[0].ToolResult == nil {
			return nil, false
		}
		return []llm.Message{seg[0], result("c1", bigLog(900))}, true
	}
	srv, client := judge(t, map[string]float64{"s0.needed": 0.9, "s1.needed": 0.9})
	_, sel := start(t, &Compactor{Client: client, ShortForm: shortForm}, messages)
	s0 := sel.Segments[0]
	if s0.Action != ActionDrop || !errors.Is(s0.Scores.Err, compact.ErrTooLarge) {
		t.Fatalf("segment 0 = %+v", s0)
	}
	if _, ok := questionKeys(srv)["s0.needed"]; ok {
		t.Fatal("asked about a segment too large to ask about")
	}
}

func TestInvalidAnswerDropsTheSegment(t *testing.T) {
	_, client := judge(t, map[string]float64{"s0.needed": 0.9, "s0.verbatim": 0.9, "s1.needed": 1.7})
	_, sel := start(t, &Compactor{Client: client}, history())
	if sel.Fallback != "" {
		t.Fatalf("fell back: %s", sel.Fallback)
	}
	if s1 := sel.Segments[1]; s1.Action != ActionDrop || s1.Scores.Err == nil {
		t.Fatalf("segment 1 = %+v, want dropped unscored", s1)
	}
}

func TestFallbacks(t *testing.T) {
	stockOf := func(base compaction.Compactor, messages []llm.Message) dashi.Start {
		st, err := base.Start(context.Background(), messages)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	tests := []struct {
		name     string
		setup    func(t *testing.T) *Compactor
		messages func() []llm.Message
		want     string
	}{
		{
			name: "Decide fails",
			setup: func(t *testing.T) *Compactor {
				srv, client := judge(t, nil)
				for range 5 {
					srv.FailNext(500)
				}
				return &Compactor{Base: smallWindow(), Client: client}
			},
			want: "Decide",
		},
		{
			name: "Decide passes the timeout",
			setup: func(t *testing.T) *Compactor {
				srv, client := judge(t, nil)
				srv.SetLatency(time.Second)
				return &Compactor{Base: smallWindow(), Client: client, Timeout: 20 * time.Millisecond}
			},
			want: "deadline exceeded",
		},
		{
			name: "pinned words exceed the budget",
			setup: func(t *testing.T) *Compactor {
				_, client := judge(t, nil)
				return &Compactor{Base: smallWindow(), Client: client}
			},
			messages: func() []llm.Message {
				m := history()
				m[1] = user(strings.Repeat("A long pasted spec line.\n", 300))
				return m
			},
			want: "own words exceed the budget",
		},
		{
			name: "nothing dropped",
			setup: func(t *testing.T) *Compactor {
				_, client := judge(t, map[string]float64{
					"s0.needed": 0.9, "s0.verbatim": 0.9, "s1.needed": 0.9, "s2.needed": 0.9,
				})
				return &Compactor{Client: client}
			},
			want: "nothing was dropped",
		},
		{
			name: "pairing broken by a short form",
			setup: func(t *testing.T) *Compactor {
				_, client := judge(t, map[string]float64{"s0.needed": 0.9, "s0.verbatim": 0.1})
				drop := func(seg []llm.Message) ([]llm.Message, bool) { return seg[:1], true }
				return &Compactor{Client: client, ShortForm: drop}
			},
			want: "lose its result",
		},
		{
			name: "no older segments",
			setup: func(t *testing.T) *Compactor {
				_, client := judge(t, nil)
				return &Compactor{Base: smallWindow(), Client: client}
			},
			messages: func() []llm.Message {
				return []llm.Message{user("task"), user("more"), user("go"), assistant("ok")}
			},
			want: "nothing was dropped",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.setup(t)
			messages := history()
			if tt.messages != nil {
				messages = tt.messages()
			}
			st, sel := start(t, c, messages)
			if !strings.Contains(sel.Fallback, tt.want) {
				t.Fatalf("fallback = %q, want %q", sel.Fallback, tt.want)
			}
			if want := stockOf(c.Base, messages); !reflect.DeepEqual(st, want) {
				t.Fatalf("start is not the stock start")
			}
			if sel.Hash == "" {
				t.Fatal("selection has no hash")
			}
		})
	}
}

func TestStartEndsWithContext(t *testing.T) {
	_, client := judge(t, nil)
	var got []Selection
	c := &Compactor{Base: smallWindow(), Client: client, OnSelection: func(s Selection) { got = append(got, s) }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Start(ctx, history()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(got) != 1 || got[0].Fallback == "" {
		t.Fatalf("selections = %+v", got)
	}
}

func TestStartPassesBaseErrors(t *testing.T) {
	_, client := judge(t, nil)
	var calls int
	c := &Compactor{Client: client, OnSelection: func(Selection) { calls++ }}
	_, err := c.Start(context.Background(), []llm.Message{
		{Role: llm.RoleSystem, Content: []llm.Content{llm.Text("only instructions")}},
	})
	if !errors.Is(err, compaction.ErrNothingToCompact) || calls != 1 {
		t.Fatalf("err = %v, calls = %d", err, calls)
	}
}

func TestZeroCompactorWorks(t *testing.T) {
	var c Compactor
	if c.Needed(history(), llm.Usage{InputTokens: 10}) {
		t.Fatal("needed with a small context")
	}
	if !c.Needed(history(), llm.Usage{InputTokens: 120_000}) {
		t.Fatal("not needed with a full context")
	}
}
