package dashi

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func textResult(text string) llm.ToolResult {
	return llm.ToolResult{CallID: "c", Output: tool.TextOutput(text)}
}

func TestShortenResultBoundaries(t *testing.T) {
	limit := 2048
	for _, n := range []int{0, 1, limit - 1, limit} {
		if _, _, ok := shortenResult(textResult(strings.Repeat("a", n)), limit); ok {
			t.Fatalf("%d bytes at limit %d was shortened", n, limit)
		}
	}
	r := textResult(strings.Repeat("a", limit+1))
	view, facts, ok := shortenResult(r, limit)
	if !ok {
		t.Fatal("limit plus one byte was not shortened")
	}
	got := modelText(view)
	if len(got) > limit {
		t.Fatalf("view has %d bytes, limit %d", len(got), limit)
	}
	if facts.OriginalBytes != limit+1 || facts.Limit != limit ||
		facts.ShownBytes >= facts.OriginalBytes {
		t.Fatalf("facts = %+v", facts)
	}
	if !strings.Contains(got, "shortened: 2049 bytes,") ||
		strings.Contains(strings.ToLower(got), "run the call again") {
		t.Fatalf("marker = %q", got)
	}
	if len(modelText(r)) != limit+1 {
		t.Fatal("shortenResult changed its input")
	}
}

func TestShortenResultKeepsHeadAndTail(t *testing.T) {
	text := "HEAD" + strings.Repeat("x", 10000) + "TAIL"
	view, _, ok := shortenResult(textResult(text), 2048)
	got := modelText(view)
	if !ok || !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") {
		t.Fatalf("view = %.60q ... %.20q", got, got[max(len(got)-20, 0):])
	}
}

func TestShortenResultUTF8Edges(t *testing.T) {
	for _, r := range []string{"é", "€", "😀"} {
		// Vary the length so cuts land on every byte of a rune.
		for pad := range 5 {
			text := strings.Repeat("a", pad) + strings.Repeat(r, 4000)
			view, facts, ok := shortenResult(textResult(text), 2048)
			got := modelText(view)
			if !ok || !utf8.ValidString(got) || len(got) > 2048 {
				t.Fatalf("%q pad %d: ok=%v valid=%v len=%d",
					r, pad, ok, utf8.ValidString(got), len(got))
			}
			if strings.ContainsRune(got, utf8.RuneError) {
				t.Fatalf("%q pad %d: cut inside a rune", r, pad)
			}
			if facts.ShownBytes > 2048 {
				t.Fatalf("facts = %+v", facts)
			}
		}
	}
}

func TestShortenResultRepairsInvalidBytesWithinLimit(t *testing.T) {
	text := strings.Repeat("\xff", 10000)
	view, _, ok := shortenResult(textResult(text), 1024)
	got := modelText(view)
	if !ok || !utf8.ValidString(got) || len(got) > 1024 {
		t.Fatalf("ok=%v valid=%v len=%d", ok, utf8.ValidString(got), len(got))
	}
	// Separated invalid bytes each become U+FFFD, three bytes for one.
	spread := strings.Repeat("a\xff", 5000)
	view, _, _ = shortenResult(textResult(spread), 1024)
	got = modelText(view)
	if !utf8.ValidString(got) || len(got) > 1024 {
		t.Fatalf("valid=%v len=%d", utf8.ValidString(got), len(got))
	}
}

func TestShortenResultSpansBlocks(t *testing.T) {
	image := content.Block{Kind: content.KindImage, Image: &content.Image{}}
	block := func(s string) content.Block { return content.TextBlock(s) }
	r := llm.ToolResult{
		CallID:  "c",
		IsError: true,
		Output: tool.Output{Content: []content.Block{
			block("one-" + strings.Repeat("a", 3000)),
			image,
			block(strings.Repeat("b", 3000) + "-two"),
			block("last"),
		}},
	}
	view, facts, ok := shortenResult(r, 2048)
	if !ok || !view.IsError || view.CallID != "c" {
		t.Fatalf("ok=%v view=%+v", ok, view)
	}
	if facts.OriginalBytes != 6012 || len(modelText(view)) > 2048 {
		t.Fatalf("facts=%+v len=%d", facts, len(modelText(view)))
	}
	images := 0
	for _, b := range view.Output.Content {
		if b.Kind == content.KindImage {
			images++
		}
	}
	got := modelText(view)
	if images != 1 || !strings.HasPrefix(got, "one-") ||
		!strings.HasSuffix(got, "-twolast") ||
		strings.Count(got, "[Tool output shortened") != 1 {
		t.Fatalf("images=%d view=%.40q", images, got)
	}
}

func TestShortenResultOffAndEmpty(t *testing.T) {
	big := textResult(strings.Repeat("a", 1<<20))
	if _, _, ok := shortenResult(big, -1); ok {
		t.Fatal("a negative limit shortened")
	}
	if _, _, ok := shortenResult(llm.ToolResult{CallID: "c"}, 1024); ok {
		t.Fatal("an empty result was shortened")
	}
}

func TestResolveToolResultBytes(t *testing.T) {
	for in, want := range map[int]int{
		0: 64 << 10, -7: -1, 100: 1024, 1024: 1024, 16 << 10: 16 << 10,
	} {
		if got := resolveToolResultBytes(in); got != want {
			t.Errorf("resolve(%d) = %d, want %d", in, got, want)
		}
	}
}

func bigTool(text string, state tool.State, runs *int) tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:   "remote",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			*runs++
			if state == tool.StateFailed {
				return tool.Outcome{
					State:  tool.StateFailed,
					Output: new(tool.TextOutput(text)),
				}, nil
			}
			return tool.Succeeded(text), nil
		}),
	}
}

func runBig(
	t *testing.T,
	agent Agent,
	text string,
	state tool.State,
	hooks Hooks,
) (*scriptedClient, Outcome) {
	t.Helper()
	runs := 0
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := (Runner{
		Agent:  agent,
		Client: client,
		Tools:  tool.List{bigTool(text, state, &runs)},
		Hooks:  hooks,
	}).Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
	})
	if err != nil || out.Stop.Kind != StopCompleted || runs != 1 {
		t.Fatalf("stop=%+v runs=%d err=%v", out.Stop, runs, err)
	}
	return client, out
}

func TestZeroAgentShortensLargeResult(t *testing.T) {
	text := "start-" + strings.Repeat("log line\n", 2<<20/9) + "-end"
	client, out := runBig(t, testAgent(), text, tool.StateSucceeded, Hooks{})
	view := out.Tools["call-1"]
	if got := modelText(modelToolResult("call-1", view.Outcome)); len(got) != len(text) {
		t.Fatalf("record holds %d of %d bytes", len(got), len(text))
	}
	if got := len(modelText(view.Result())); got > 64<<10 {
		t.Fatalf("view holds %d bytes", got)
	}
	facts := view.Shortened()
	if facts == nil || facts.OriginalBytes != len(text) ||
		facts.Limit != 64<<10 || facts.ShownBytes <= 0 {
		t.Fatalf("shortened = %+v", facts)
	}
	sent := client.inputs[1].Messages
	res := sent[len(sent)-1].Content[0].ToolResult
	if modelText(*res) != modelText(view.Result()) {
		t.Fatal("the model saw something other than the recorded view")
	}
	if got := toolResultText(t, out.Turn, "call-1"); got != modelText(view.Result()) {
		t.Fatal("Project differs from the recorded view")
	}
}

func TestSmallResultIsUnchanged(t *testing.T) {
	_, out := runBig(t, testAgent(), "small", tool.StateSucceeded, Hooks{})
	settled := settlementOf(t, out.Turn, "call-1")
	if settled.Tool.Result != nil || settled.Tool.Shortened != nil ||
		out.Tools["call-1"].Shortened() != nil {
		t.Fatalf("recorded a view for a small result: %+v", settled.Tool)
	}
}

func TestFailedResultIsShortenedAndStaysAnError(t *testing.T) {
	text := strings.Repeat("boom ", 4000)
	agent := testAgent()
	agent.Limits.ToolResultBytes = 2048
	_, out := runBig(t, agent, text, tool.StateFailed, Hooks{})
	view := out.Tools["call-1"]
	res := view.Result()
	if !res.IsError || len(modelText(res)) > 2048 ||
		view.Shortened() == nil || view.Shortened().Limit != 2048 {
		t.Fatalf("result=%+v shortened=%+v", res.IsError, view.Shortened())
	}
}

func TestNegativeLimitKeepsFullResult(t *testing.T) {
	text := strings.Repeat("a", 200<<10)
	agent := testAgent()
	agent.Limits.ToolResultBytes = -1
	_, out := runBig(t, agent, text, tool.StateSucceeded, Hooks{})
	if len(modelText(out.Tools["call-1"].Result())) != len(text) ||
		out.Tools["call-1"].Shortened() != nil {
		t.Fatal("a negative limit shortened the result")
	}
}

func TestLimitAfterHooksOrdering(t *testing.T) {
	secret := "sk-123"
	text := secret + strings.Repeat("a", 5000) + secret
	var seen int
	redact := ResultHook{Name: "redact", Func: func(
		_ context.Context,
		p ResultPlan,
	) (ResultDecision, error) {
		seen = len(modelText(p.Result))
		r := p.Result
		r.Output = r.Output.MapText(func(s string) string {
			return strings.ReplaceAll(s, secret, "[redacted]")
		})
		return ResultDecision{Replace: &r}, nil
	}}
	agent := testAgent()
	agent.Limits.ToolResultBytes = 2048
	_, out := runBig(t, agent, text, tool.StateSucceeded, redact.Hooks())
	if seen != len(text) {
		t.Fatalf("the hook saw %d of %d bytes", seen, len(text))
	}
	view := out.Tools["call-1"]
	got := modelText(view.Result())
	if strings.Contains(got, secret) || !strings.HasPrefix(got, "[redacted]") {
		t.Fatalf("view = %.30q", got)
	}
	// Sizes count the text after the hook.
	if want := len(text) - 2*(len(secret)-len("[redacted]")); view.Shortened().OriginalBytes != want {
		t.Fatalf("original = %d, want %d", view.Shortened().OriginalBytes, want)
	}
}

func TestReplayIgnoresACurrentLimitChange(t *testing.T) {
	text := strings.Repeat("a", 5000)
	agent := testAgent()
	agent.Limits.ToolResultBytes = 2048
	_, out := runBig(t, agent, text, tool.StateSucceeded, Hooks{})
	want := modelText(out.Tools["call-1"].Result())
	// Replay reads the record; no runner or limit is involved.
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if got := modelText(view.Tools["call-1"].Result()); got != want {
		t.Fatal("replay recomputed the view")
	}
	if view.Tools["call-1"].Shortened() == nil ||
		view.Tools["call-1"].Shortened().Limit != 2048 {
		t.Fatalf("shortened = %+v", view.Tools["call-1"].Shortened())
	}
	if got := toolResultText(t, out.Turn, "call-1"); got != want {
		t.Fatal("Project differs from the recorded view")
	}
}

func TestStreamCarriesRawResultAndSameView(t *testing.T) {
	text := "a" + strings.Repeat("b", 100<<10)
	runs := 0
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	runner := Runner{
		Agent:  testAgent(),
		Client: client,
		Tools:  tool.List{bigTool(text, tool.StateSucceeded, &runs)},
	}
	var streamed *ToolFact
	var final Outcome
	for event, err := range runner.Stream(t.Context(), Turn{ID: "t1"}, Start{
		Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
	}) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Step != nil && event.Step.Kind == StepToolSettled {
			streamed = event.Step.Tool
		}
		if event.Outcome != nil {
			final = *event.Outcome
		}
	}
	if streamed == nil || streamed.Outcome == nil ||
		len(streamed.Outcome.Output.Content[0].Text.Text) != len(text) {
		t.Fatalf("the stream did not carry the full result: %+v", streamed)
	}
	if streamed.Result == nil || streamed.Shortened == nil ||
		modelText(*streamed.Result) != modelText(final.Tools["call-1"].Result()) {
		t.Fatal("the streamed view differs from the recorded view")
	}
}

func TestStartWarnsWhenLimitIsRaised(t *testing.T) {
	agent := testAgent()
	agent.Limits.ToolResultBytes = 100
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := (Runner{Agent: agent, Client: client}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Turn.Steps[0].Started.Agent.Limits.ToolResultBytes != 1024 {
		t.Fatal("the recorded limit was not raised")
	}
	found := false
	for _, w := range out.Warnings {
		found = found || w.Code == WarningLimitAdjusted
	}
	if !found {
		t.Fatalf("warnings = %+v", out.Warnings)
	}
}

func TestReplayRejectsBadShortenedRecords(t *testing.T) {
	agent := testAgent()
	agent.Limits.ToolResultBytes = 2048
	_, out := runBig(t, agent, strings.Repeat("a", 5000), tool.StateSucceeded, Hooks{})
	for name, edit := range map[string]func(*ToolFact){
		"no result": func(f *ToolFact) { f.Result = nil },
		"no limit":  func(f *ToolFact) { f.Shortened.Limit = 0 },
		"not less":  func(f *ToolFact) { f.Shortened.ShownBytes = f.Shortened.OriginalBytes },
	} {
		turn := cloneTurn(out.Turn)
		for i, s := range turn.Steps {
			if s.Kind == StepToolSettled && s.Tool.Call.ID == "call-1" {
				fact := *s.Tool
				sh := *fact.Shortened
				fact.Shortened = &sh
				edit(&fact)
				turn.Steps[i].Tool = &fact
			}
		}
		if _, err := Replay(turn); err == nil {
			t.Errorf("%s: replay accepted the record", name)
		}
	}
}

func TestSettledHostResultIsShortened(t *testing.T) {
	external := tool.Tool{Definition: tool.Definition{
		Name:   "lookup",
		Schema: []byte(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	text := strings.Repeat("z", 200<<10)
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "lookup"), final("found")},
	}
	runner := Runner{Agent: testAgent(), Client: client, Tools: tool.List{external}}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
	})
	if err != nil || out.Stop.Kind != StopWaiting {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	result := tool.Outcome{
		State:  tool.StateSucceeded,
		Output: new(tool.TextOutput(text)),
	}
	out, err = runner.Run(t.Context(), out.Turn, out.Pending[0].Result(result))
	if err != nil {
		t.Fatal(err)
	}
	view := out.Tools["call-1"]
	if view.Shortened() == nil || len(modelText(view.Result())) > 64<<10 ||
		len(modelText(modelToolResult("call-1", view.Outcome))) != len(text) {
		t.Fatalf("shortened = %+v", view.Shortened())
	}
}
