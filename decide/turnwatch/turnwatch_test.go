package turnwatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/turnwatch"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

const request = "Fix the failing parser test."

type testArgs struct {
	Package string `json:"package"`
}

type fileArgs struct {
	Path string `json:"path"`
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// tools returns run_tests, which always fails the same way, and
// read_file, which always succeeds.
func tools() tool.List {
	return tool.List{
		tool.MustFunc("run_tests", "Runs the tests of a package.",
			func(context.Context, tool.Call, testArgs) (tool.Outcome, error) {
				return tool.Failed("FAIL TestParse: got 4 tokens, want 3"), nil
			}),
		tool.MustFunc("read_file", "Reads a file.",
			func(_ context.Context, _ tool.Call, in fileArgs) (tool.Outcome, error) {
				return tool.Succeeded("contents of " + in.Path), nil
			}),
	}
}

func runTests(id string) llm.Result {
	return dashitest.CallTool(id, "run_tests", `{"package":"./parser"}`)
}

func start(agent dashi.Agent) dashi.Start {
	agent.Model = dashitest.Model
	return dashi.Start{
		Agent: agent,
		Input: []llm.Message{llm.UserMessage(llm.Text(request))},
	}
}

// checks collects every Check the watcher reports.
type checks struct {
	mu   sync.Mutex
	list []turnwatch.Check
}

func (c *checks) add(check turnwatch.Check) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = append(c.list, check)
}

func (c *checks) all() []turnwatch.Check {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.list)
}

// judge starts a fake Decide server that answers with these probabilities.
func judge(t *testing.T, repeating, offRequest float64) (*decidetest.Server, *decide.Client) {
	t.Helper()
	srv := decidetest.NewServer(t)
	srv.Answer("repeating", decidetest.NoulAnswer(repeating))
	srv.Answer("off_request", decidetest.NoulAnswer(offRequest))
	return srv, srv.Client(t, decide.WithMaxRetries(0))
}

func run(
	t *testing.T,
	w *turnwatch.Watcher,
	model *dashitest.Client,
	turn dashi.Turn,
	cmd dashi.Command,
	opts ...dashi.RunOption,
) dashi.RunResult {
	t.Helper()
	runner := dashi.Runner{Client: model, Tools: tools(), Hooks: w.Hook().Hooks()}
	out, err := runner.Run(t.Context(), turn, cmd, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// watcherResults returns the turn's recorded results for the watcher.
func watcherResults(turn dashi.Turn) []dashi.HookResult {
	var out []dashi.HookResult
	for _, step := range turn.Steps {
		for _, r := range step.HookResults {
			if r.Name == turnwatch.Name {
				out = append(out, r)
			}
		}
	}
	return out
}

// reminders returns the watcher reminders in a model input.
func reminders(in llm.Input) []llm.Reminder {
	var out []llm.Reminder
	for _, m := range in.Messages {
		for _, part := range m.Content {
			if part.Kind == llm.ContentReminder && part.Reminder.Name == turnwatch.Name {
				out = append(out, *part.Reminder)
			}
		}
	}
	return out
}

func TestRepeatedFailureReminds(t *testing.T) {
	srv, client := judge(t, 0.95, 0.08)
	var seen checks
	w := &turnwatch.Watcher{Client: client, OnCheck: seen.add}
	model := dashitest.NewClient(
		runTests("c1"),
		runTests("c2"),
		dashitest.CallTool("c3", "read_file", `{"path":"parser/lex.go"}`),
		dashitest.Reply("The lexer splits '->' in two; fixed."),
	)
	out := run(t, w, model, dashi.Turn{ID: "t1"}, start(dashi.Agent{}))
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("stop = %+v", out.Stop)
	}

	// One Decide request, after the second identical failure.
	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d Decide requests, want 1", len(reqs))
	}
	var state struct {
		UserRequest string `json:"user_request"`
		Recent      []struct {
			Tool   string `json:"tool"`
			Result string `json:"result"`
			Failed bool   `json:"failed"`
		} `json:"recent_activity"`
	}
	raw, _ := reqs[0].Request.State.(json.RawMessage)
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("state %s: %v", raw, err)
	}
	if state.UserRequest != request || len(state.Recent) != 2 ||
		!state.Recent[1].Failed || !strings.Contains(state.Recent[1].Result, "FAIL") {
		t.Fatalf("state = %s", raw)
	}
	if _, ok := reqs[0].Request.Questions["off_request"]; !ok {
		t.Fatal("off_request was not asked")
	}

	// The next model call sees the fixed reminder, from the watcher.
	inputs := model.Inputs()
	if n := len(reminders(inputs[1])); n != 0 {
		t.Fatalf("second call saw %d reminders", n)
	}
	got := reminders(inputs[2])
	if len(got) != 1 || !strings.HasPrefix(got[0].Content, "A call you made keeps failing") ||
		got[0].Name != turnwatch.Name || got[0].Tier != llm.ReminderContextual ||
		got[0].From != "" {
		t.Fatalf("third call reminders = %+v", got)
	}
	item, ok := out.Context(dashi.ContextRuntime, turnwatch.Name)
	if !ok || item.ID != "turnwatch-1" {
		t.Fatalf("context item = %+v, %v", item, ok)
	}

	// The record says why, and which decision model decided.
	results := watcherResults(out.Turn)
	if len(results) != 1 {
		t.Fatalf("hook results = %+v", results)
	}
	r := results[0]
	want := "repeating: failed_again: run_tests, 2 times; repeating 0.95, off_request 0.08 (request req_1)"
	if r.Verdict != dashi.VerdictChange || r.Reason != want || r.RulesVersion != "jev-1.13.0" {
		t.Fatalf("hook result = %+v", r)
	}

	all := seen.all()
	if len(all) != 1 {
		t.Fatalf("checks = %+v", all)
	}
	c := all[0]
	if c.Trigger != turnwatch.TriggerFailedAgain || c.Action != turnwatch.ActionRemind ||
		c.TurnID != "t1" || c.ModelCalls != 2 || c.RequestID != "req_1" ||
		c.Model != "jev-1.13.0" || c.Usage.OutputTokens == 0 || len(c.Answers) != 2 {
		t.Fatalf("check = %+v", c)
	}
}

func TestNoAnswerAddsNothing(t *testing.T) {
	srv, client := judge(t, 0.2, 0.1)
	var seen checks
	w := &turnwatch.Watcher{Client: client, OnCheck: seen.add}
	model := dashitest.NewClient(runTests("c1"), runTests("c2"), dashitest.Reply("Stuck."))
	out := run(t, w, model, dashi.Turn{ID: "t1"}, start(dashi.Agent{}))
	if len(srv.Requests()) != 1 {
		t.Fatalf("%d Decide requests, want 1", len(srv.Requests()))
	}
	if r := watcherResults(out.Turn); len(r) != 0 {
		t.Fatalf("a check that finds nothing left %+v", r)
	}
	if all := seen.all(); len(all) != 1 || all[0].Action != turnwatch.ActionNone {
		t.Fatalf("checks = %+v", all)
	}
	if n := len(reminders(model.Inputs()[2])); n != 0 {
		t.Fatalf("%d reminders", n)
	}
}

func TestNoTriggerNoRequest(t *testing.T) {
	srv, client := judge(t, 0.95, 0.95)
	w := &turnwatch.Watcher{Client: client}
	model := dashitest.NewClient(
		dashitest.CallTool("c1", "run_tests", `{"package":"./parser"}`),
		dashitest.CallTool("c2", "run_tests", `{"package":"./lexer"}`),
		dashitest.CallTool("c3", "read_file", `{"path":"a.go"}`),
		dashitest.CallTool("c4", "read_file", `{"path":"a.go"}`),
		dashitest.Reply("Done."),
	)
	out := run(t, w, model, dashi.Turn{ID: "t1"}, start(dashi.Agent{}))
	if out.Stop.Kind != dashi.StopCompleted || len(srv.Requests()) != 0 {
		t.Fatalf("stop %s, %d Decide requests; want none", out.Stop.Kind, len(srv.Requests()))
	}
}

// TestDisplayIgnored fails the same call twice with the same content but a
// new Display each time. The model never sees Display, so the second
// failure still triggers a check.
func TestDisplayIgnored(t *testing.T) {
	srv, client := judge(t, 0.95, 0.05)
	var runs atomic.Int32
	flaky := tool.MustFunc("run_tests", "Runs the tests of a package.",
		func(context.Context, tool.Call, testArgs) (tool.Outcome, error) {
			o := tool.Failed("FAIL TestParse: got 4 tokens, want 3")
			o.Output.Display = fmt.Sprintf("run %d failed", runs.Add(1))
			return o, nil
		})
	model := dashitest.NewClient(runTests("c1"), runTests("c2"), dashitest.Reply("Stuck."))
	runner := dashi.Runner{Client: model, Tools: tool.List{flaky}, Hooks: (&turnwatch.Watcher{Client: client}).Hook().Hooks()}
	out, err := runner.Run(t.Context(), dashi.Turn{ID: "t1"}, start(dashi.Agent{}))
	if err != nil {
		t.Fatal(err)
	}
	if runs.Load() != 2 || len(srv.Requests()) != 1 {
		t.Fatalf("%d runs, %d Decide requests; want 2, 1", runs.Load(), len(srv.Requests()))
	}
	if r := watcherResults(out.Turn); len(r) != 1 ||
		!strings.HasPrefix(r[0].Reason, "repeating: failed_again: run_tests, 2 times;") {
		t.Fatalf("hook results = %+v", r)
	}
}

func TestSameCallAndCheckEvery(t *testing.T) {
	srv, client := judge(t, 0.1, 0.9)
	w := &turnwatch.Watcher{Client: client, SameCalls: 2, CheckEvery: 3}
	read := func(id string) llm.Result {
		return dashitest.CallTool(id, "read_file", `{"path":"a.go"}`)
	}
	model := dashitest.NewClient(read("c1"), read("c2"), read("c3"), dashitest.Reply("Done."))
	out := run(t, w, model, dashi.Turn{ID: "t1"}, start(dashi.Agent{}))
	results := watcherResults(out.Turn)
	// After the third read, both same_call (two earlier reads) and
	// check_every (3 model calls) hold; same_call is named, and off_request
	// is the confirmed key.
	if len(srv.Requests()) != 1 || len(results) != 1 ||
		!strings.HasPrefix(results[0].Reason, "off_request: same_call: read_file, 3 times;") {
		t.Fatalf("%d requests, results %+v", len(srv.Requests()), results)
	}
	got := reminders(model.Inputs()[3])
	if len(got) != 1 || !strings.HasPrefix(got[0].Content, "Your recent calls may have moved") {
		t.Fatalf("reminders = %+v", got)
	}
}

// TestCapAcrossResume stops a turn after two reminders and resumes it from
// its stored record with a new Watcher: the cap still holds, and the
// watcher asks Decide no more.
func TestCapAcrossResume(t *testing.T) {
	srv, client := judge(t, 0.95, 0.05)
	agent := dashi.Agent{Limits: dashi.Limits{ModelCalls: 4, NoReservedFinalReply: true}}
	model := dashitest.NewClient(runTests("c1"), runTests("c2"), runTests("c3"), runTests("c4"))
	out := run(t, &turnwatch.Watcher{Client: client}, model, dashi.Turn{ID: "t1"}, start(agent))
	if out.Stop.Kind != dashi.StopLimited || len(srv.Requests()) != 2 {
		t.Fatalf("stop %s, %d Decide requests; want limited, 2", out.Stop.Kind, len(srv.Requests()))
	}

	// Another process loads the stored record.
	b, err := json.Marshal(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	var stored dashi.Turn
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatal(err)
	}
	model.Add(runTests("c5"), dashitest.Reply("Giving up."))
	out = run(t, &turnwatch.Watcher{Client: client}, model, stored, dashi.Continue{ModelCalls: 6})
	if out.Stop.Kind != dashi.StopCompleted || len(srv.Requests()) != 2 {
		t.Fatalf("stop %s, %d Decide requests; want completed, still 2",
			out.Stop.Kind, len(srv.Requests()))
	}
	if r := watcherResults(out.Turn); len(r) != 2 {
		t.Fatalf("hook results = %+v", r)
	}
	last := model.Inputs()[5]
	if n := len(reminders(last)); n != 2 {
		t.Fatalf("last call saw %d reminders, want 2", n)
	}
}

// steerInbox is an Inbox that reports when the run fetched its steers, so
// a hook can send an urgent steer and know the run saw it.
type steerInbox struct {
	*dashi.Inbox
	fetched chan struct{}
}

func (s *steerInbox) Steers(ctx context.Context, turnID string) ([]dashi.Steer, error) {
	steers, err := s.Inbox.Steers(ctx, turnID)
	select {
	case s.fetched <- struct{}{}:
	default:
	}
	return steers, err
}

// TestUrgentSteerRetryNotCheckedTwice sends an urgent steer after the
// watcher reminded, so Dashi asks the hooks again for the same model call.
// The second pass sees its reminder and does not ask Decide again.
func TestUrgentSteerRetryNotCheckedTwice(t *testing.T) {
	srv, client := judge(t, 0.95, 0.05)
	var seen checks
	w := &turnwatch.Watcher{Client: client, OnCheck: seen.add}
	inbox := &steerInbox{Inbox: dashi.NewInbox(), fetched: make(chan struct{}, 1)}
	steered, passes := false, 0
	steer := dashi.ModelHook{Name: "steer", Func: func(
		context.Context, dashi.ModelPlan) (dashi.ModelDecision, error) {
		passes++
		if steered || len(seen.all()) == 0 {
			return dashi.ModelDecision{}, nil
		}
		steered = true
		for len(inbox.fetched) > 0 {
			<-inbox.fetched
		}
		if _, err := inbox.SendUrgent("Run only the parser tests."); err != nil {
			return dashi.ModelDecision{}, err
		}
		select {
		case <-inbox.fetched:
		case <-time.After(5 * time.Second):
			t.Error("the run never fetched the urgent steer")
		}
		return dashi.ModelDecision{}, nil
	}}
	model := dashitest.NewClient(runTests("c1"), runTests("c2"), dashitest.Reply("Done."))
	runner := dashi.Runner{
		Client: model,
		Tools:  tools(),
		Hooks:  dashi.JoinHooks(w.Hook(), steer),
	}
	out, err := runner.Run(t.Context(), dashi.Turn{ID: "t1"}, start(dashi.Agent{}),
		dashi.WithSteering(inbox))
	if err != nil {
		t.Fatal(err)
	}
	// Three model calls, and one pass the steer sent back.
	if !steered || passes != 4 || out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("steered %v, %d hook passes, stop %+v", steered, passes, out.Stop)
	}
	if len(srv.Requests()) != 1 || len(seen.all()) != 1 {
		t.Fatalf("%d Decide requests, %d checks; want 1 each",
			len(srv.Requests()), len(seen.all()))
	}
	if r := watcherResults(out.Turn); len(r) != 1 {
		t.Fatalf("hook results = %+v", r)
	}
	last := model.Inputs()[2]
	if n := len(reminders(last)); n != 1 {
		t.Fatalf("the call saw %d reminders, want 1", n)
	}
	if !strings.Contains(messagesText(last), "Run only the parser tests.") {
		t.Fatal("the steer did not reach the call")
	}
}

func messagesText(in llm.Input) string {
	var b strings.Builder
	for _, m := range in.Messages {
		b.WriteString(m.Text())
	}
	return b.String()
}

func TestDecideFailureWarns(t *testing.T) {
	srv, client := judge(t, 0.95, 0.95)
	srv.FailNext(500)
	var seen checks
	w := &turnwatch.Watcher{Client: client, OnCheck: seen.add}
	model := dashitest.NewClient(runTests("c1"), runTests("c2"), dashitest.Reply("Stuck."))
	out := run(t, w, model, dashi.Turn{ID: "t1"}, start(dashi.Agent{}))
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("stop = %+v", out.Stop)
	}
	results := watcherResults(out.Turn)
	if len(results) != 1 || results[0].Verdict != dashi.VerdictFailed || results[0].Error == "" {
		t.Fatalf("hook results = %+v", results)
	}
	warned := slices.ContainsFunc(out.Warnings, func(w dashi.Warning) bool {
		return w.Code == dashi.WarningHookFailed
	})
	if !warned {
		t.Fatalf("warnings = %+v", out.Warnings)
	}
	if n := len(reminders(model.Inputs()[2])); n != 0 {
		t.Fatalf("%d reminders after a failed check", n)
	}
	if all := seen.all(); len(all) != 1 || all[0].Err == nil || all[0].Action != turnwatch.ActionNone {
		t.Fatalf("checks = %+v", all)
	}
}

func TestNoClientWarns(t *testing.T) {
	// A nil Client reads the environment, once per process; no other test
	// here leaves Client nil and asks Decide.
	t.Setenv("TYPESAFE_API_KEY", "")
	model := dashitest.NewClient(runTests("c1"), runTests("c2"), dashitest.Reply("Stuck."))
	out := run(t, &turnwatch.Watcher{}, model, dashi.Turn{ID: "t1"}, start(dashi.Agent{}))
	results := watcherResults(out.Turn)
	if out.Stop.Kind != dashi.StopCompleted || len(results) != 1 ||
		results[0].Verdict != dashi.VerdictFailed {
		t.Fatalf("stop %s, hook results %+v", out.Stop.Kind, results)
	}
}

func TestHeuristicOnly(t *testing.T) {
	var seen checks
	w := &turnwatch.Watcher{MinProbability: -1, CheckEvery: 1, OnCheck: seen.add}
	model := dashitest.NewClient(
		dashitest.CallTool("c0", "read_file", `{"path":"a.go"}`),
		runTests("c1"), runTests("c2"), dashitest.Reply("Stuck."))
	out := run(t, w, model, dashi.Turn{ID: "t1"}, start(dashi.Agent{}))
	results := watcherResults(out.Turn)
	if len(results) != 1 || results[0].Reason != "repeating: failed_again: run_tests, 2 times" ||
		results[0].RulesVersion != "" {
		t.Fatalf("hook results = %+v", results)
	}
	got := reminders(model.Inputs()[3])
	if len(got) != 1 || !strings.HasPrefix(got[0].Content, "A call you made keeps failing") {
		t.Fatalf("reminders = %+v", got)
	}
	// CheckEvery is ignored: the only check is the failed_again trigger.
	all := seen.all()
	if len(all) != 1 || all[0].Trigger != turnwatch.TriggerFailedAgain ||
		all[0].Action != turnwatch.ActionRemind || all[0].Model != "" {
		t.Fatalf("checks = %+v", all)
	}
}

func TestNegativeMaxRemindersOnlyReports(t *testing.T) {
	_, client := judge(t, 0.95, 0.05)
	var seen checks
	w := &turnwatch.Watcher{Client: client, MaxReminders: -1, OnCheck: seen.add}
	model := dashitest.NewClient(runTests("c1"), runTests("c2"), runTests("c3"), dashitest.Reply("Stuck."))
	out := run(t, w, model, dashi.Turn{ID: "t1"}, start(dashi.Agent{}))
	if r := watcherResults(out.Turn); len(r) != 0 {
		t.Fatalf("hook results = %+v", r)
	}
	all := seen.all()
	if len(all) != 2 || all[0].Action != turnwatch.ActionNone || all[1].Action != turnwatch.ActionNone {
		t.Fatalf("checks = %+v", all)
	}
}

func TestChildTurnSkipped(t *testing.T) {
	srv, client := judge(t, 0.95, 0.95)
	w := &turnwatch.Watcher{Client: client, MinProbability: 0}
	model := dashitest.NewClient(runTests("c1"), runTests("c2"), dashitest.Reply("Stuck."))
	cmd := start(dashi.Agent{})
	cmd.Parent = &dashi.CallRef{TurnID: "parent", CallID: "task-1"}
	out := run(t, w, model, dashi.Turn{ID: "child"}, cmd)
	if len(srv.Requests()) != 0 || len(watcherResults(out.Turn)) != 0 {
		t.Fatalf("%d Decide requests, results %+v; want none",
			len(srv.Requests()), watcherResults(out.Turn))
	}
}

// TestSessionCompactionSkipped runs a session compaction turn, which
// summarizes earlier turns, with CheckEvery on and two identical failures.
// The watcher asks nothing and reminds nothing in that turn.
func TestSessionCompactionSkipped(t *testing.T) {
	srv, client := judge(t, 0.95, 0.95)
	var seen checks
	w := &turnwatch.Watcher{Client: client, CheckEvery: 1, OnCheck: seen.add}
	model := dashitest.NewClient(runTests("c1"), runTests("c2"),
		dashitest.Reply("<summary>The parser tests fail.</summary>"))
	out := run(t, w, model, dashi.Turn{ID: "t1"}, dashi.Start{
		Origin: dashi.StartCompaction,
		Agent:  dashi.Agent{Model: dashitest.Model},
		Input:  []llm.Message{llm.UserMessage(llm.Text("Summarize."))},
		Compaction: &dashi.CompactionFact{
			Frame: "Summary:",
		},
	})
	if out.Stop.Kind != dashi.StopCompleted || out.ModelCalls != 3 {
		t.Fatalf("stop %s after %d model calls", out.Stop.Kind, out.ModelCalls)
	}
	if len(srv.Requests()) != 0 || len(seen.all()) != 0 {
		t.Fatalf("%d Decide requests, checks %+v; want none", len(srv.Requests()), seen.all())
	}
	if r := watcherResults(out.Turn); len(r) != 0 {
		t.Fatalf("hook results = %+v", r)
	}
	for i, in := range model.Inputs() {
		if n := len(reminders(in)); n != 0 {
			t.Fatalf("call %d saw %d reminders", i+1, n)
		}
	}
}

// healthyTools returns write_file, which always succeeds, and run_tests,
// which fails with a new error on each of its first fails runs and then
// passes: the edit-and-test loop of an agent that is making progress.
func healthyTools(fails int) tool.List {
	var runs atomic.Int32
	return tool.List{
		tool.MustFunc("write_file", "Writes a file.",
			func(_ context.Context, _ tool.Call, in writeArgs) (tool.Outcome, error) {
				return tool.Succeeded("wrote " + in.Path), nil
			}),
		tool.MustFunc("run_tests", "Runs the tests of a package.",
			func(context.Context, tool.Call, testArgs) (tool.Outcome, error) {
				n := int(runs.Add(1))
				if n <= fails {
					return tool.Failed(fmt.Sprintf("FAIL TestParse: case %d of %d", n, fails+1)), nil
				}
				return tool.Succeeded("ok ./parser"), nil
			}),
	}
}

// TestHealthyLoopAsksNothing runs an edit-and-test loop whose test runs
// fail in a new way each time and then pass. The same call with a new
// result is progress, so no check runs, in either mode.
func TestHealthyLoopAsksNothing(t *testing.T) {
	const fails = 5
	for _, mode := range []struct {
		name string
		min  float64
	}{{"decide", 0}, {"heuristic", -1}} {
		t.Run(mode.name, func(t *testing.T) {
			srv, client := judge(t, 0.95, 0.95)
			var seen checks
			w := &turnwatch.Watcher{Client: client, MinProbability: mode.min, OnCheck: seen.add}
			var script []llm.Result
			for i := range fails + 1 {
				args := fmt.Sprintf(`{"path":"parser/lex.go","content":"fix %d"}`, i)
				script = append(script,
					dashitest.CallTool(fmt.Sprintf("w%d", i), "write_file", args),
					runTests(fmt.Sprintf("t%d", i)))
			}
			model := dashitest.NewClient(append(script, dashitest.Reply("Fixed."))...)
			runner := dashi.Runner{Client: model, Tools: healthyTools(fails), Hooks: w.Hook().Hooks()}
			agent := dashi.Agent{Limits: dashi.Limits{ModelCalls: 20}}
			out, err := runner.Run(t.Context(), dashi.Turn{ID: "t1"}, start(agent))
			if err != nil {
				t.Fatal(err)
			}
			if out.Stop.Kind != dashi.StopCompleted || out.ModelCalls != 2*(fails+1)+1 {
				t.Fatalf("stop %s after %d model calls", out.Stop.Kind, out.ModelCalls)
			}
			if n := len(srv.Requests()); n != 0 {
				t.Fatalf("%d Decide requests, want 0", n)
			}
			if all := seen.all(); len(all) != 0 {
				t.Fatalf("checks = %+v", all)
			}
			if r := watcherResults(out.Turn); len(r) != 0 {
				t.Fatalf("hook results = %+v", r)
			}
		})
	}
}

// TestTimeoutWarns gives Decide more time than Timeout allows: the check
// fails open quickly with a hook_failed warning and no reminder.
func TestTimeoutWarns(t *testing.T) {
	srv, client := judge(t, 0.95, 0.95)
	srv.SetLatency(2 * time.Second)
	var seen checks
	w := &turnwatch.Watcher{Client: client, Timeout: 100 * time.Millisecond, OnCheck: seen.add}
	model := dashitest.NewClient(runTests("c1"), runTests("c2"), dashitest.Reply("Stuck."))
	began := time.Now()
	out := run(t, w, model, dashi.Turn{ID: "t1"}, start(dashi.Agent{}))
	if took := time.Since(began); took > time.Second {
		t.Fatalf("the turn took %s; the timeout did not hold", took)
	}
	results := watcherResults(out.Turn)
	if out.Stop.Kind != dashi.StopCompleted || len(results) != 1 ||
		results[0].Verdict != dashi.VerdictFailed {
		t.Fatalf("stop %s, hook results %+v", out.Stop.Kind, results)
	}
	warned := slices.ContainsFunc(out.Warnings, func(w dashi.Warning) bool {
		return w.Code == dashi.WarningHookFailed
	})
	if !warned {
		t.Fatalf("warnings = %+v", out.Warnings)
	}
	if n := len(reminders(model.Inputs()[2])); n != 0 {
		t.Fatalf("%d reminders after a timed-out check", n)
	}
	all := seen.all()
	if len(all) != 1 || !errors.Is(all[0].Err, context.DeadlineExceeded) {
		t.Fatalf("checks = %+v", all)
	}
}

// TestContinueAfterSavedReminder stops the turn after the reminder is
// saved but before the model call is sent: a BeforeSend hook denies that
// call once. Continue asks the hooks again for the same model call; the
// watcher sees its reminder and does not ask Decide again.
func TestContinueAfterSavedReminder(t *testing.T) {
	srv, client := judge(t, 0.95, 0.05)
	var seen checks
	w := &turnwatch.Watcher{Client: client, OnCheck: seen.add}
	denied := false
	deny := dashi.SendHook{Name: "deny-once", Func: func(
		_ context.Context, p dashi.SendPlan) (dashi.SendDecision, error) {
		// The third model call is the one the reminder is for.
		if p.Turn.ModelCalls == 2 && !denied {
			denied = true
			return dashi.SendDecision{Deny: true, Reason: "paused for review"}, nil
		}
		return dashi.SendDecision{}, nil
	}}
	model := dashitest.NewClient(runTests("c1"), runTests("c2"), dashitest.Reply("Done."))
	runner := dashi.Runner{
		Client: model,
		Tools:  tools(),
		Hooks:  dashi.JoinHooks(w.Hook(), deny),
	}
	out, err := runner.Run(t.Context(), dashi.Turn{ID: "t1"}, start(dashi.Agent{}))
	if err != nil {
		t.Fatal(err)
	}
	if !denied || out.Stop.Kind != dashi.StopRefused || len(srv.Requests()) != 1 {
		t.Fatalf("denied %v, stop %+v, %d Decide requests", denied, out.Stop, len(srv.Requests()))
	}

	out, err = runner.Run(t.Context(), out.Turn, dashi.Continue{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("stop = %+v", out.Stop)
	}
	if len(srv.Requests()) != 1 || len(seen.all()) != 1 {
		t.Fatalf("%d Decide requests, %d checks; want 1 each",
			len(srv.Requests()), len(seen.all()))
	}
	if r := watcherResults(out.Turn); len(r) != 1 {
		t.Fatalf("hook results = %+v", r)
	}
	inputs := model.Inputs()
	if n := len(reminders(inputs[len(inputs)-1])); n != 1 {
		t.Fatalf("the call after Continue saw %d reminders, want 1", n)
	}
}

// sizedClient reports the request body's real size, so active compaction
// can size it.
type sizedClient struct{ *dashitest.Client }

func (c sizedClient) Prepare(ctx context.Context, in llm.Input) (llm.Request, error) {
	req, err := c.Client.Prepare(ctx, in)
	if err != nil {
		return llm.Request{}, err
	}
	snapshot := req.Snapshot()
	if snapshot.Body, err = json.Marshal(in); err != nil {
		return llm.Request{}, err
	}
	return llm.NewRequest(snapshot)
}

// TestActiveCompaction runs a turn whose large, identical failures fill
// the context window. The watcher reminds before the third model call,
// then a summary replaces every assistant message, and the hooks run again
// for that call with the summary call counted, so CheckEvery triggers. The
// watcher still finds its reminder and does not check that call twice. A
// later identical failure, after the summary, is counted from the record
// and triggers the second check.
func TestActiveCompaction(t *testing.T) {
	srv, client := judge(t, 0.95, 0.05)
	var seen checks
	w := &turnwatch.Watcher{Client: client, CheckEvery: 3, OnCheck: seen.add}
	huge := strings.Repeat("FAIL TestParse: got 4 tokens, want 3\n", 250)
	big := tool.MustFunc("run_tests", "Runs the tests of a package.",
		func(context.Context, tool.Call, testArgs) (tool.Outcome, error) {
			return tool.Failed(huge), nil
		})
	model := dashitest.NewClient(
		runTests("c1"),
		runTests("c2"),
		dashitest.Reply("<summary>The parser tests failed twice the same way.</summary>"),
		runTests("c3"),
		dashitest.Reply("Stuck."),
	)
	runner := dashi.Runner{Client: sizedClient{model}, Tools: tool.List{big}, Hooks: w.Hook().Hooks()}
	agent := dashi.Agent{
		Model:            dashitest.Model,
		Settings:         llm.Settings{MaxTokens: 256},
		ActiveCompaction: &dashi.ActiveCompactionPolicy{Window: 16000, MaxTokens: 256},
	}
	out, err := runner.Run(t.Context(), dashi.Turn{ID: "t1"}, dashi.Start{
		Agent: agent,
		Input: []llm.Message{llm.UserMessage(llm.Text(request))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted || len(out.ContextCompactions) != 1 {
		t.Fatalf("stop %s, %d compactions", out.Stop.Kind, len(out.ContextCompactions))
	}
	all := seen.all()
	if len(srv.Requests()) != 2 || len(all) != 2 ||
		all[0].Trigger != turnwatch.TriggerFailedAgain || all[0].Action != turnwatch.ActionRemind ||
		all[1].Trigger != turnwatch.TriggerFailedAgain || all[1].Action != turnwatch.ActionRemind {
		t.Fatalf("%d Decide requests, checks %+v; want 2 reminders",
			len(srv.Requests()), all)
	}
	// Inputs: c1, c2, the summary, the third call, the last call.
	inputs := model.Inputs()
	if len(inputs) != 5 {
		t.Fatalf("%d model calls, want 5", len(inputs))
	}
	if n := len(reminders(inputs[3])); n != 1 {
		t.Fatalf("the call after the summary saw %d reminders, want 1", n)
	}
	if n := len(reminders(inputs[4])); n != 2 {
		t.Fatalf("the last call saw %d reminders, want 2", n)
	}
	if r := watcherResults(out.Turn); len(r) != 2 {
		t.Fatalf("hook results = %+v", r)
	}
}
