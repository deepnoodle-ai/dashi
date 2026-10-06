package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	dashisession "github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

// A recorded session folder gives one case per turn end in the size
// limits, with a window the history fills to caseFill.
func TestRecordedCases(t *testing.T) {
	ctx := context.Background()
	folder := filepath.Join(t.TempDir(), "20261006-000000-abc123")
	rec, err := filerecorder.New(filerecorder.Config{Dir: filepath.Join(folder, "turns"), DisableSync: true})
	if err != nil {
		t.Fatal(err)
	}
	id := filepath.Base(folder)
	filler := strings.Repeat("log line without anything to find\n", 3000) // about 26,000 tokens
	var history []llm.Message
	for n, text := range []string{
		"Find why the build fails.\n" + filler,
		"The build failed with code ZX-9917 in pkg/zeta/gen.go.\n" + strings.Repeat(filler, 4),
		"Now fix it.",
	} {
		runner := dashi.Runner{Client: dashitest.NewClient(dashitest.Reply("ok")), Recorder: rec}
		_, err := runner.Run(ctx, dashi.NewTurn(dashisession.TurnID(id, n+1)), dashi.Start{
			CommandID: "run", Agent: dashi.Agent{Model: dashitest.Model},
			History: history, Input: []llm.Message{llm.UserMessage(llm.Text(text))},
		})
		if err != nil {
			t.Fatal(err)
		}
		var turns []dashi.Turn
		for i := 1; i <= n+1; i++ {
			turn, err := rec.Load(ctx, dashisession.TurnID(id, i))
			if err != nil {
				t.Fatal(err)
			}
			turns = append(turns, turn)
		}
		history, _ = dashi.BuildHistory(turns)
	}

	cases, err := recordedSessions(ctx, []string{filepath.Dir(folder)})
	if err != nil {
		t.Fatal(err)
	}
	// Turn 1 is about 26,000 tokens; turns 2 and 3 are over maxCaseTokens.
	if len(cases) != 1 || cases[0].name != id+"/t1" || !strings.HasPrefix(cases[0].latest, "Find why") {
		t.Fatalf("cases = %+v, want turn 1 only", cases)
	}
	for _, c := range cases {
		if size := keptTokens(c.messages, &dashi.CompactionFact{SummaryAt: -1}); c.window != int(float64(size)/caseFill) {
			t.Errorf("%s: window %d for %d tokens", c.name, c.window, size)
		}
	}
}

// Mining keeps an answer only when it is word for word in the history
// before stock's cut and not in the tail, the user's own words, or the
// question.
func TestMineChecksAnswers(t *testing.T) {
	filler := strings.Repeat("ordinary build output line\n", 2000)
	messages := []llm.Message{
		llm.UserMessage(llm.Text("Find why the build fails. Never touch vendor/legacy.")),
		llm.AssistantMessage(llm.Text("Running the build."), llm.Content{Kind: llm.ContentToolCall,
			ToolCall: &tool.Call{ID: "c1", Name: "bash", Args: []byte(`{"command":"make"}`)}}),
		llm.UserMessage(llm.Content{Kind: llm.ContentToolResult, ToolResult: &llm.ToolResult{CallID: "c1",
			Output: tool.TextOutput("error ZX-9917 in pkg/zeta/gen.go\n" + filler)}}),
		llm.AssistantMessage(llm.Text("Found it.")),
		llm.UserMessage(llm.Text("Fix it. Keep pkg/alpha untouched.")),
		llm.AssistantMessage(llm.Text("Fixing pkg/alpha/keep.go now.")),
	}
	reply := `[
		{"kind":"error","question":"Which error code did make report?","answer":"ZX-9917"},
		{"kind":"path","question":"Which file failed?","answer":"pkg/zeta/gen.go"},
		{"kind":"path","question":"Which directory must stay untouched?","answer":"vendor/legacy"},
		{"kind":"path","question":"Which file is being fixed?","answer":"pkg/alpha/keep.go"},
		{"kind":"value","question":"What is ZX-9917?","answer":"ZX-9917 in"},
		{"kind":"name","question":"What was invented?","answer":"not-in-the-text"}
	]`
	e := &env{
		cfg:    config{mineModel: dashitest.Model},
		client: func(_, _ string) llm.Client { return dashitest.NewClient(dashitest.Reply(reply)) },
	}
	size := keptTokens(messages, &dashi.CompactionFact{SummaryAt: -1})
	got, err := e.mine(context.Background(), session{name: "s/t1", messages: messages, window: int(float64(size) / caseFill)})
	if err != nil {
		t.Fatal(err)
	}
	var answers []string
	for _, n := range got {
		answers = append(answers, n.Answer)
	}
	// ZX-9917 and the path pass. vendor/legacy is the user's own words,
	// pkg/alpha/keep.go is in the tail, "ZX-9917 in" holds an earlier
	// answer, and the last is not in the text.
	if strings.Join(answers, ",") != "ZX-9917,pkg/zeta/gen.go" {
		t.Fatalf("needles = %v", answers)
	}
}

// A live run sends each recorded case to its own provider, or stops: an
// OpenAI case without OPENAI_API_KEY and a case of an unsupported
// provider are errors, not runs on Claude.
func TestProviderError(t *testing.T) {
	for _, tt := range []struct {
		provider   string
		haveOpenAI bool
		want       string // error text; empty means no error
	}{
		{"", false, ""},
		{"anthropic", false, ""},
		{"openai", true, ""},
		{"openai", false, "set OPENAI_API_KEY"},
		{"google", true, `provider "google", which the eval does not support`},
	} {
		err := providerError(tt.provider, tt.haveOpenAI)
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("providerError(%q, %v) = %v, want %q", tt.provider, tt.haveOpenAI, err, tt.want)
		}
	}
}

// recordedWithNeedles checks every case's provider before it mines or
// runs anything. The scripted client records provider "dashitest".
func TestRecordedChecksProviders(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "20261006-000000-abc123")
	rec, err := filerecorder.New(filerecorder.Config{Dir: filepath.Join(folder, "turns"), DisableSync: true})
	if err != nil {
		t.Fatal(err)
	}
	runner := dashi.Runner{Client: dashitest.NewClient(dashitest.Reply("ok")), Recorder: rec}
	if _, err := runner.Run(context.Background(), dashi.NewTurn(dashisession.TurnID(filepath.Base(folder), 1)), dashi.Start{
		CommandID: "run", Agent: dashi.Agent{Model: dashitest.Model},
		Input: []llm.Message{llm.UserMessage(llm.Text("Find why the build fails.\n" +
			strings.Repeat("log line without anything to find\n", 3000)))},
	}); err != nil {
		t.Fatal(err)
	}
	e := &env{
		cfg:           config{live: true, recorded: []string{filepath.Dir(folder)}, needles: filepath.Join(t.TempDir(), "needles.json")},
		checkProvider: func(p string) error { return providerError(p, true) },
	}
	if _, err := e.recordedWithNeedles(context.Background()); err == nil || !strings.Contains(err.Error(), `provider "dashitest"`) {
		t.Fatalf("err = %v, want an unsupported provider error", err)
	}
}
