package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/llmtext"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

var fixtureOrigin = content.Origin{
	Provider: "test",
	API:      "test.chat",
	Model:    "fixture",
	Scope:    "fixture",
}

func toolCall(id, name, args string) llm.Content {
	call := tool.Call{ID: id, Name: name, Args: json.RawMessage(args)}
	return llm.Content{Kind: llm.ContentToolCall, ToolCall: &call}
}

func toolUse(parts ...llm.Content) llm.Result {
	return llm.Result{
		Message: llm.AssistantMessage(parts...),
		Stop:    llm.Stop{Kind: llm.StopToolUse},
	}
}

func countingTool(name string, runs *int) tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:   name,
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			*runs++
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("ran")),
			}, nil
		}),
	}
}

func externalTool(name string) tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:   name,
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindExternal,
		},
	}
}

func TestPendingBuildersAnswerMixedWork(t *testing.T) {
	request := llm.HostedApprovalRequest{
		ID:        "hosted-1",
		Name:      "remote",
		Arguments: json.RawMessage(`{"q":1}`),
		Origin:    fixtureOrigin,
	}
	client := &scriptedClient{results: []llm.Result{
		toolUse(
			toolCall("ask-1", "deploy", `{"env":"prod"}`),
			toolCall("ext-1", "mail", `{}`),
			llm.Content{
				Kind:                  llm.ContentHostedApprovalRequest,
				HostedApprovalRequest: &request,
			},
		),
		final("done"),
	}}
	runs := 0
	runner := Runner{
		Client:   client,
		Tools:    tool.List{countingTool("deploy", &runs), externalTool("mail")},
		Recorder: NewMemoryRecorder(),
		Hooks:    RequireApproval("deploy"),
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
	if err != nil || out.Stop.Kind != StopWaiting {
		t.Fatalf("start stop=%+v err=%v", out.Stop, err)
	}
	if ids := strings.Join(pendingIDs(out.Pending), ","); ids != "ask-1,ext-1,hosted-1" {
		t.Fatalf("pending order=%s", ids)
	}
	ask, ext, hosted := out.Pending[0], out.Pending[1], out.Pending[2]
	if ask.Kind != PendingApproval ||
		ask.Name != "deploy" ||
		string(ask.Args) != `{"env":"prod"}` ||
		ask.Tool == nil {
		t.Fatalf("local approval item=%+v", ask)
	}
	if ext.Kind != PendingTool || ext.Tool == nil ||
		ext.Tool.Status != ToolWaiting {
		t.Fatalf("external item=%+v", ext)
	}
	if hosted.Kind != PendingApproval ||
		hosted.Hosted == nil ||
		hosted.Name != "remote" ||
		hosted.Tool != nil {
		t.Fatalf("hosted item=%+v", hosted)
	}

	answers := []Resolve{
		ask.Approve(),
		ext.Result(tool.Outcome{
			State:  tool.StateSucceeded,
			Output: new(tool.TextOutput("sent")),
		}),
		hosted.Deny("Not today."),
	}
	for _, answer := range answers {
		if out, err = runner.Run(t.Context(), out.Turn, answer); err != nil {
			t.Fatalf("%s: %v", answer.CommandID, err)
		}
	}
	steps := len(out.Turn.Steps)
	again, err := runner.Run(t.Context(), out.Turn, ask.Approve())
	if err != nil || len(again.Turn.Steps) != steps {
		t.Fatalf("retried answer: steps=%d err=%v", len(again.Turn.Steps), err)
	}
	if _, err := runner.Run(t.Context(), out.Turn, ask.Deny("")); !errors.Is(
		err,
		ErrCommandConflict,
	) {
		t.Fatalf("second, different answer err=%v", err)
	}
	last := out.Turn.Steps[len(out.Turn.Steps)-1]
	if last.HostedApproval == nil ||
		last.HostedApproval.Approve ||
		last.HostedApproval.Reason != "Not today." {
		t.Fatalf("hosted answer=%+v", last.HostedApproval)
	}
	if len(out.Pending) != 0 || !out.ReadyToContinue {
		t.Fatalf("answered turn pending=%v ready=%t", out.Pending, out.ReadyToContinue)
	}
	done, err := runner.Run(t.Context(), out.Turn, Continue{CommandID: "continue"})
	if err != nil || done.Stop.Kind != StopCompleted || runs != 1 {
		t.Fatalf("continue stop=%+v err=%v runs=%d", done.Stop, err, runs)
	}
	if done.Reply != "done" {
		t.Fatalf("answer=%q", done.Reply)
	}
}

func TestApproveOnNonApprovalIsRejected(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("ext-1", "mail", `{}`)),
	}}
	runner := Runner{
		Client: client,
		Tools:  tool.List{externalTool("mail")},
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
	if err != nil || len(out.Pending) != 1 {
		t.Fatalf("start pending=%v err=%v", out.Pending, err)
	}
	rejected, err := runner.Run(t.Context(), out.Turn, out.Pending[0].Approve())
	if err == nil {
		t.Fatal("approval of an external call was accepted")
	}
	if len(rejected.Turn.Steps) != len(out.Turn.Steps) ||
		len(rejected.Pending) != 1 {
		t.Fatalf("rejected command lost the caller's turn: %+v", rejected)
	}
}

// A denial is recorded by Resolve alone. A crash while the next Continue
// settles the call must not lose the operator's reason.
func TestDenialReasonSurvivesCrashAndReachesModel(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("call-1", "deploy", `{}`)),
		final("understood"),
	}}
	runs := 0
	base := NewMemoryRecorder()
	runner := Runner{
		Client:   client,
		Tools:    tool.List{countingTool("deploy", &runs)},
		Recorder: base,
		Hooks:    RequireApproval("deploy"),
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
	if err != nil || len(out.Pending) != 1 {
		t.Fatalf("start pending=%v err=%v", out.Pending, err)
	}
	denied, err := runner.Run(
		t.Context(),
		out.Turn,
		out.Pending[0].Deny("Deploys are frozen."),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range denied.Turn.Steps {
		if step.Kind == StepToolFinished {
			t.Fatal("Resolve recorded more than the approval")
		}
	}
	if len(denied.Pending) != 0 || !denied.ReadyToContinue {
		t.Fatalf("denied pending=%v ready=%t", denied.Pending, denied.ReadyToContinue)
	}

	runner.Recorder = &failOnceKind{base: base, kind: StepToolFinished}
	_, err = runner.Run(t.Context(), denied.Turn, Continue{CommandID: "c1"})
	if persistence(err) != PersistenceRejected {
		t.Fatalf("crashing continue err=%v", err)
	}
	saved, err := base.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	done, err := runner.Run(t.Context(), saved, Continue{CommandID: "c2"})
	if err != nil || done.Stop.Kind != StopCompleted || runs != 0 {
		t.Fatalf("recovered stop=%+v err=%v runs=%d", done.Stop, err, runs)
	}
	sent, err := json.Marshal(client.inputs[len(client.inputs)-1].Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sent), "Deploys are frozen.") {
		t.Fatalf("model did not see the denial reason: %s", sent)
	}
}

func TestContinueOnWaitingTurnIsNotReady(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("call-1", "deploy", `{}`)),
	}}
	runs := 0
	runner := Runner{
		Client: client,
		Tools:  tool.List{countingTool("deploy", &runs)},
		Hooks:  RequireApproval("deploy"),
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	again, err := runner.Run(t.Context(), out.Turn, Continue{CommandID: "c"})
	if !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "call-1") {
		t.Fatalf("err=%v", err)
	}
	if again.Stop.Kind != StopWaiting ||
		len(again.Turn.Steps) != len(out.Turn.Steps) {
		t.Fatalf("rejected continue outcome=%+v", again)
	}
}

func TestRejectedInvalidTurnKeepsCallerTurn(t *testing.T) {
	turn := Turn{ID: "t1", Steps: []Step{{Kind: StepTurnStopped}}}
	out, err := (Runner{Client: &scriptedClient{}}).Run(
		t.Context(),
		turn,
		Continue{CommandID: "c"},
	)
	if err == nil || out.Turn.ID != "t1" || len(out.Turn.Steps) != 1 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

type conflictRecorder struct{ base *MemoryRecorder }

func (r conflictRecorder) Commit(ctx context.Context, req CommitRequest) error {
	if req.Step.Kind == StepModelCallStarted {
		return &CommitError{
			Persistence: PersistenceRejected,
			Err:         ErrRevisionConflict,
		}
	}
	return r.base.Commit(ctx, req)
}

func TestCommitErrorWrapsRecorderConflict(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("unused")}}
	out, err := start(t, client, nil, conflictRecorder{NewMemoryRecorder()}, Hooks{})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("err=%v does not match ErrRevisionConflict", err)
	}
	ce, ok := errors.AsType[*CommitError](err)
	if !ok || ce.Step == nil || ce.Step.Kind != StepModelCallStarted {
		t.Fatalf("commit error=%+v", ce)
	}
	if n := strings.Count(err.Error(), "rejected"); n != 1 {
		t.Fatalf("message repeats its persistence: %q", err.Error())
	}
	if len(out.Turn.Steps) != 1 || client.sends != 0 {
		t.Fatalf("saved steps=%d sends=%d", len(out.Turn.Steps), client.sends)
	}
}

func TestFailedModelSettlementKeepsUsageOnStep(t *testing.T) {
	result := final("answer")
	result.Usage = &llm.Usage{InputTokens: 3, OutputTokens: 5}
	client := &scriptedClient{results: []llm.Result{result}}
	rec := &failOnceKind{base: NewMemoryRecorder(), kind: StepModelCallFinished}
	out, err := start(t, client, nil, rec, Hooks{})
	step := unacknowledged(err)
	if persistence(err) != PersistenceRejected ||
		step == nil ||
		step.Model == nil ||
		step.Model.Result == nil {
		t.Fatalf("err=%v step=%+v", err, step)
	}
	if got := step.Model.Result.Usage.OutputTokens; got != 5 {
		t.Fatalf("step usage output tokens=%d", got)
	}
	if out.Spend.Usage.OutputTokens == 5 {
		t.Fatal("outcome spend counted an unsaved settlement")
	}
}

// viewRecordingHooks record the Stop of each view the hooks receive.
func viewRecordingHooks(stops *[]Stop, authorized *int) Hooks {
	return Hooks{
		BeforeModel: []ModelHook{
			{
				Name: "record",
				Func: func(_ context.Context, plan ModelPlan) (ModelDecision, error) {
					*stops = append(*stops, plan.Turn.Stop)
					return ModelDecision{}, nil
				},
			},
		},
		BeforeTool: []ToolHook{
			{
				Name: "record",
				Func: func(_ context.Context, plan ToolPlan) (ToolDecision, error) {
					*stops = append(*stops, plan.Turn.Stop)
					*authorized++
					return ToolDecision{}, nil
				},
			},
		},
		BeforeFinish: []FinishHook{
			{
				Name: "record",
				Func: func(_ context.Context, plan FinishPlan) (FinishDecision, error) {
					*stops = append(*stops, plan.Turn.Stop)
					return FinishDecision{}, nil
				},
			},
		},
	}
}

func TestHookViewStopIsEmptyMidRun(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "work"),
		final("paused"),
		final("done"),
	}}
	client.results[1].Stop = llm.Stop{Kind: llm.StopPause}
	var stops []Stop
	runs, authorized := 0, 0
	agent := testAgent()
	agent.Limits.ModelCalls = 4
	runner := Runner{
		Client: client,
		Tools:  tool.List{countingTool("work", &runs)},
		Hooks:  viewRecordingHooks(&stops, &authorized),
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     agent,
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if len(stops) == 0 || authorized != 1 || runs != 1 {
		t.Fatalf("views=%d authorized=%d runs=%d", len(stops), authorized, runs)
	}
	for _, stop := range stops {
		if stop.Kind != "" {
			t.Fatalf("hook view stop=%+v", stop)
		}
	}
	if out.Reply != "pauseddone" {
		t.Fatalf("paused answer=%q", out.Reply)
	}
}

func TestReplayProjectsStopLikeOutcome(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := start(t, client, nil, nil, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	// Cut the record after the model intent, as a crash would.
	crashed := Turn{ID: out.Turn.ID, Steps: out.Turn.Steps[:2]}
	view, err := Replay(crashed)
	if err != nil || view.Stop.Kind != StopInterrupted {
		t.Fatalf("replayed stop=%+v err=%v", view.Stop, err)
	}
	if got := restingOutcome(crashed); got.Stop != view.Stop {
		t.Fatalf("outcome stop=%+v replay stop=%+v", got.Stop, view.Stop)
	}
	view, err = Replay(out.Turn)
	if err != nil || view.Stop != out.Stop || view.Reply != out.Reply {
		t.Fatalf("replay=%+v outcome stop=%+v", view.Stop, out.Stop)
	}
	if view.Spend.Usage.InputTokens != out.Spend.Usage.InputTokens {
		t.Fatal("outcome spend differs from replay")
	}
}

func textPart(text string, citations ...content.Citation) llm.Content {
	block := content.TextBlock(text)
	block.Text.Citations = citations
	return llm.Block(block)
}

func hostedUsePart(id string) llm.Content {
	return llm.Content{
		Kind: llm.ContentHostedUse,
		HostedUse: &llm.HostedUse{
			ID:     id,
			Name:   "web_search",
			Type:   "server_tool_use",
			Origin: fixtureOrigin,
		},
	}
}

func TestJoinTextKeepsSentencesIntact(t *testing.T) {
	reasoning := llm.Content{
		Kind:      llm.ContentReasoning,
		Reasoning: &llm.Reasoning{Text: "thinking"},
	}
	tests := []struct {
		name  string
		parts []llm.Content
		want  string
	}{
		{
			name: "citation fragments",
			parts: []llm.Content{
				textPart("The capital is "),
				textPart("Dodoma", content.Citation{URL: "https://a"}),
				textPart(", with about "),
				textPart("765,000 people", content.Citation{URL: "https://b"}),
				textPart("."),
			},
			want: "The capital is Dodoma, with about 765,000 people.",
		},
		{
			name: "hosted use between passages",
			parts: []llm.Content{
				textPart("53^4 is 7890481."),
				hostedUsePart("u1"),
				textPart("**Tanzania's** capital"),
			},
			want: "53^4 is 7890481.\n\n**Tanzania's** capital",
		},
		{
			name: "reasoning between passages",
			parts: []llm.Content{
				textPart("First."),
				reasoning,
				textPart("Second."),
			},
			want: "First.\n\nSecond.",
		},
		{
			name: "newline already present",
			parts: []llm.Content{
				textPart("First.\n"),
				hostedUsePart("u1"),
				textPart("Second."),
			},
			want: "First.\nSecond.",
		},
		{
			name: "leading non-text",
			parts: []llm.Content{
				reasoning,
				textPart("Only."),
			},
			want: "Only.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := llmtext.Join(test.parts); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

func settledStep(result llm.Result) Step {
	return Step{
		Kind:  StepModelCallFinished,
		Model: &ModelFact{Result: &result},
	}
}

func TestTurnCitationsAndHostedUsesSpanCalls(t *testing.T) {
	result := llm.Content{
		Kind: llm.ContentHostedResult,
		HostedResult: &llm.HostedResult{
			CallID:  "u1",
			Type:    "web_search_tool_result",
			Origin:  fixtureOrigin,
			Output:  []content.Block{content.TextBlock("page")},
			IsError: true,
		},
	}
	interrupted := settledStep(llm.Result{
		Message: llm.AssistantMessage(hostedUsePart("lost")),
	})
	interrupted.Model.Interrupted = InterruptRun
	turn := Turn{ID: "t1", Steps: []Step{
		settledStep(llm.Result{
			Message: llm.AssistantMessage(
				hostedUsePart("u1"),
				result,
				textPart("A", content.Citation{URL: "https://a"}),
			),
			Stop: llm.Stop{Kind: llm.StopPause},
		}),
		interrupted,
		settledStep(llm.Result{
			Message: llm.AssistantMessage(
				hostedUsePart("u2"),
				textPart("B", content.Citation{URL: "https://b"}),
			),
			Stop: llm.Stop{Kind: llm.StopFinished},
		}),
	}}
	citations := turn.Citations()
	if len(citations) != 2 ||
		citations[0].URL != "https://a" ||
		citations[1].URL != "https://b" {
		t.Fatalf("citations=%+v", citations)
	}
	uses := turn.HostedUses()
	if len(uses) != 2 || uses[0].ID != "u1" || uses[1].ID != "u2" {
		t.Fatalf("hosted uses=%+v", uses)
	}
	if len(uses[0].Output) != 1 ||
		uses[0].Output[0].Text.Text != "page" ||
		!uses[0].IsError {
		t.Fatalf("hosted result was not paired: %+v", uses[0].Output)
	}
	if got := answerText(turn.Steps); got != "A\n\nB" {
		t.Fatalf("answer across a pause=%q", got)
	}
}

func TestPendingItemsListOnlyHostWork(t *testing.T) {
	s := &replayState{
		TurnView: TurnView{Tools: map[string]ToolView{}},
		hosted:   map[string]llm.HostedApprovalRequest{},
	}
	statuses := []ToolStatus{
		ToolRequested,
		ToolAllowed,
		ToolDenied,
		ToolStarted,
		ToolWaiting,
		ToolResultUnknown,
		ToolApprovalPending,
		ToolFinished,
	}
	for _, status := range statuses {
		id := string(status)
		s.Tools[id] = ToolView{Call: tool.Call{ID: id, Name: "work"}, Status: status}
		s.callOrder = append(s.callOrder, id)
	}
	// Hosted requests keep the model's order, not map order.
	for _, id := range []string{"h9", "h1", "h5"} {
		s.hosted[id] = llm.HostedApprovalRequest{ID: id, Name: "remote"}
		s.hostedOrder = append(s.hostedOrder, id)
	}
	s.modelIntent = "attempt-1"
	got := strings.Join(pendingIDs(s.pendingItems()), ",")
	want := "attempt-1,started,waiting,result_unknown,approval_pending,h9,h1,h5"
	if got != want {
		t.Fatalf("pending=%s, want %s", got, want)
	}
	kinds := map[string]PendingKind{}
	for _, p := range s.pendingItems() {
		kinds[p.ID] = p.Kind
	}
	if kinds["attempt-1"] != PendingModel ||
		kinds["result_unknown"] != PendingTool ||
		kinds["approval_pending"] != PendingApproval ||
		kinds["h1"] != PendingApproval {
		t.Fatalf("kinds=%v", kinds)
	}
}

func TestProjectedStopForRequestedCallsIsInterrupted(t *testing.T) {
	s := &replayState{
		TurnView: TurnView{Tools: map[string]ToolView{
			"a": {Call: tool.Call{ID: "a"}, Status: ToolRequested},
		}},
		callOrder: []string{"a"},
	}
	s.Pending = s.pendingItems()
	if len(s.Pending) != 0 {
		t.Fatalf("requested call listed as host work: %v", s.Pending)
	}
	if stop := s.projectedStop(); stop.Kind != StopInterrupted || !stop.Inferred {
		t.Fatalf("stop=%+v", stop)
	}
	s.Tools["a"] = ToolView{Call: tool.Call{ID: "a"}, Status: ToolFinished}
	if stop := s.projectedStop(); stop.Kind != "" || !stop.Inferred {
		t.Fatalf("settled turn without a stop step projected %+v", stop)
	}
}

func TestRequireApprovalDefersNamedTools(t *testing.T) {
	hook := RequireApproval("b", "a", "a").BeforeTool[0]
	if hook.Name != "require_approval:a,b" {
		t.Fatalf("name=%q", hook.Name)
	}
	named, err := hook.Func(t.Context(), ToolPlan{Call: tool.Call{Name: "a"}})
	if err != nil ||
		named.Kind != DecisionDefer ||
		named.Revision != "require_approval:a,b" {
		t.Fatalf("named decision=%+v err=%v", named, err)
	}
	other, err := hook.Func(t.Context(), ToolPlan{Call: tool.Call{Name: "c"}})
	if err != nil || other.Kind != "" || other.Rewrite != nil {
		t.Fatalf("other decision=%+v err=%v", other, err)
	}
	single, _ := RequireApproval("a").BeforeTool[0].Func(
		t.Context(),
		ToolPlan{Call: tool.Call{Name: "a"}},
	)
	if single.Revision == named.Revision {
		t.Fatal("different tool lists share a revision")
	}
	all := RequireApproval().BeforeTool[0]
	every, err := all.Func(t.Context(), ToolPlan{Call: tool.Call{Name: "c"}})
	if err != nil || all.Name != "require_approval:*" ||
		every.Kind != DecisionDefer {
		t.Fatalf("all tools name=%q decision=%+v err=%v", all.Name, every, err)
	}
}

func deniedDeploy(t *testing.T, reason string) (RunResult, *scriptedClient) {
	t.Helper()
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("call-1", "deploy", `{}`)),
		final("understood"),
	}}
	runs := 0
	runner := Runner{
		Client: client,
		Tools:  tool.List{countingTool("deploy", &runs)},
		Hooks:  RequireApproval("deploy"),
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out, err = runner.Run(t.Context(), out.Turn, out.Pending[0].Deny(reason)); err != nil {
		t.Fatal(err)
	}
	return out, client
}

func TestDenialKeepsHookVerdictAndRecordsAnswer(t *testing.T) {
	out, _ := deniedDeploy(t, "No.")
	call := out.Tools["call-1"]
	if call.Status != ToolDenied ||
		call.Verdict.Kind != DecisionDefer ||
		call.Verdict.Hook != "require_approval:deploy" ||
		call.Verdict.Revision != "require_approval:deploy" {
		t.Fatalf("hook verdict changed: %+v", call.Verdict)
	}
	if call.Approval == nil || call.Approval.Approve || call.Approval.Reason != "No." {
		t.Fatalf("recorded answer=%+v", call.Approval)
	}
}

func TestEmptyDenialReasonGivesDefaultText(t *testing.T) {
	out, client := deniedDeploy(t, "")
	done, err := (Runner{
		Client: client,
		Tools:  tool.List{countingTool("deploy", new(int))},
		Hooks:  RequireApproval("deploy"),
	}).Run(t.Context(), out.Turn, Continue{CommandID: "c"})
	if err != nil || done.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", done.Stop, err)
	}
	result := done.Tools["call-1"].Outcome
	if result == nil || result.Output == nil ||
		result.Output.Content[0].Text.Text != "The call was denied." {
		t.Fatalf("denied result=%+v", result)
	}
}

func TestReplayRejectsStaleApprovalStep(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		toolUse(toolCall("call-1", "deploy", `{}`)),
	}}
	out, err := (Runner{
		Client: client,
		Tools:  tool.List{countingTool("deploy", new(int))},
		Hooks:  RequireApproval("deploy"),
	}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	fact := out.Pending[0].Approve().Approval
	fact.ArgsDigest = ArgsDigest([]byte(`{"other":true}`))
	turn := cloneTurn(out.Turn)
	turn.Steps = append(turn.Steps, Step{
		Version:       1,
		TurnID:        "t1",
		Seq:           int64(len(turn.Steps) + 1),
		Kind:          StepApprovalDecided,
		CommandID:     "forged",
		CommandDigest: "forged",
		Approval:      fact,
	})
	if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("replay accepted a stale approval: %v", err)
	}
}

func TestCorruptRecordAfterCommitIsDistinct(t *testing.T) {
	rt := &runtime{turn: Turn{ID: "t1", Steps: []Step{{Kind: StepTurnStopped}}}}
	out := rt.current()
	if !errors.Is(rt.err, ErrRecordInvalid) || !errors.Is(rt.err, ErrInvalidTurn) {
		t.Fatalf("err=%v", rt.err)
	}
	if !Recorded(rt.err) {
		t.Fatal("a corrupt record after commits reads as a rejected command")
	}
	if len(out.Turn.Steps) != 1 {
		t.Fatalf("outcome dropped the recorded steps: %+v", out.Turn)
	}
	// A caller's invalid turn is a rejected command, not ErrRecordInvalid.
	_, err := (Runner{Client: &scriptedClient{}}).Run(
		t.Context(),
		rt.turn,
		Continue{CommandID: "c"},
	)
	if !errors.Is(err, ErrInvalidTurn) || errors.Is(err, ErrRecordInvalid) {
		t.Fatalf("input turn err=%v", err)
	}
}

type wrappingRecorder struct{ base *MemoryRecorder }

func (r wrappingRecorder) Commit(ctx context.Context, req CommitRequest) error {
	if req.Step.Kind == StepModelCallStarted {
		return fmt.Errorf("pg: %w", &CommitError{
			Persistence: PersistenceRejected,
			Err:         errors.New("fence"),
		})
	}
	return r.base.Commit(ctx, req)
}

func TestWrappedRecorderCommitErrorIsNotRepeated(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("unused")}}
	_, err := start(t, client, nil, wrappingRecorder{NewMemoryRecorder()}, Hooks{})
	if persistence(err) != PersistenceRejected || unacknowledged(err) == nil {
		t.Fatalf("err=%v", err)
	}
	if n := strings.Count(err.Error(), "rejected"); n != 1 {
		t.Fatalf("message repeats its persistence: %q", err.Error())
	}
}

// persistence returns the persistence of a *CommitError returned by Run,
// empty for nil, and "other" for any other error.
func persistence(err error) Persistence {
	if err == nil {
		return ""
	}
	var ce *CommitError
	if errors.As(err, &ce) {
		return ce.Persistence
	}
	return "other"
}

// unacknowledged returns the step a *CommitError names, or nil.
func unacknowledged(err error) *Step {
	var ce *CommitError
	if errors.As(err, &ce) {
		return ce.Step
	}
	return nil
}

// pendingIDs lists the IDs of pending items in order.
func pendingIDs(items []Pending) []string {
	var ids []string
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestMalformedInputTurnReturnsErrorWithoutPanic(t *testing.T) {
	turn := Turn{ID: "t1", Steps: []Step{{
		Version: 1,
		TurnID:  "t1",
		Seq:     1,
		Kind:    StepTurnStarted,
		Output:  &StructuredOutput{Value: json.RawMessage(`{bad`)},
	}}}
	out, err := (Runner{Client: &scriptedClient{}}).Run(
		t.Context(),
		turn,
		Continue{CommandID: "c"},
	)
	if !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("err=%v", err)
	}
	if out.Turn.ID != "t1" || len(out.Turn.Steps) != 1 {
		t.Fatalf("outcome lost the caller's turn: %+v", out.Turn)
	}
}

func hostedRequest(id string) llm.Result {
	request := llm.HostedApprovalRequest{
		ID:     id,
		Name:   "remote",
		Origin: fixtureOrigin,
	}
	return llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:                  llm.ContentHostedApprovalRequest,
			HostedApprovalRequest: &request,
		}),
		Stop: llm.Stop{Kind: llm.StopFinished},
	}
}

// A provider may reuse a hosted request ID after the first request was
// answered. The second request needs its own answer command.
func TestReusedHostedApprovalIDGetsItsOwnAnswer(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		hostedRequest("h1"),
		hostedRequest("h1"),
		final("done"),
	}}
	agent := testAgent()
	agent.Limits.ModelCalls = 4
	runner := Runner{Client: client}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{CommandID: "start", Agent: agent, Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
	)
	if err != nil || len(out.Pending) != 1 {
		t.Fatalf("start pending=%v err=%v", out.Pending, err)
	}
	first := out.Pending[0]
	if out, err = runner.Run(t.Context(), out.Turn, first.Approve()); err != nil {
		t.Fatal(err)
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Pending) != 1 || out.Pending[0].ID != "h1" {
		t.Fatalf("second request pending=%v stop=%+v", out.Pending, out.Stop)
	}
	second := out.Pending[0]
	if second.ModelCallID == first.ModelCallID ||
		second.Deny("no").CommandID == first.Deny("no").CommandID {
		t.Fatalf("requests share an identity: %+v %+v", first, second)
	}
	if out, err = runner.Run(t.Context(), out.Turn, second.Deny("no")); err != nil {
		t.Fatal(err)
	}
	if len(out.Pending) != 0 || !out.ReadyToContinue {
		t.Fatalf("answered request still pending: %v", out.Pending)
	}
	steps := len(out.Turn.Steps)
	again, err := runner.Run(t.Context(), out.Turn, second.Deny("no"))
	if err != nil || len(again.Turn.Steps) != steps {
		t.Fatalf("retried answer: steps=%d err=%v", len(again.Turn.Steps), err)
	}
	done, err := runner.Run(t.Context(), out.Turn, Continue{CommandID: "c2"})
	if err != nil || done.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", done.Stop, err)
	}
}

func TestIncompleteHostedApprovalIsNotActionable(t *testing.T) {
	origin := content.Origin{
		Provider: "test",
		API:      "test.chat",
		Model:    "fixture",
		Scope:    "fixture",
	}
	request := llm.HostedApprovalRequest{
		ID:     "approval-1",
		Name:   "remote",
		Origin: origin,
	}
	incomplete := llm.Result{
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
		Message: llm.AssistantMessage(
			llm.Text("partial"),
			llm.Content{
				Kind:                  llm.ContentHostedApprovalRequest,
				HostedApprovalRequest: &request,
			},
		),
		Stop: llm.Stop{Kind: llm.StopIncomplete},
	}
	client := &scriptedClient{
		results: []llm.Result{incomplete, final("recovered")},
	}
	rec := NewMemoryRecorder()
	first, err := start(t, client, nil, rec, Hooks{})
	if err != nil || first.Stop.Kind != StopFailed || first.Stop.Reason != "untrusted stop with hosted approval" || len(first.Pending) != 0 {
		t.Fatalf("first: stop=%+v pending=%v err=%v", first.Stop, first.Pending, err)
	}
	response := llm.HostedApprovalResponse{
		RequestID: request.ID,
		Approve:   true,
		Origin:    origin,
	}
	if _, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(
		t.Context(),
		first.Turn,
		Resolve{
			CommandID:      "approve",
			CallID:         request.ID,
			HostedApproval: &response,
		},
	); err == nil {
		t.Fatal("incomplete hosted approval was accepted")
	}
	second, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(t.Context(), first.Turn, Continue{CommandID: "resume"})
	if err != nil || second.Stop.Kind != StopCompleted || len(client.inputs) != 2 {
		t.Fatalf(
			"second: stop=%+v inputs=%d err=%v",
			second.Stop,
			len(client.inputs),
			err,
		)
	}
	for _, message := range client.inputs[1].Messages {
		for _, part := range message.Content {
			if part.Kind == llm.ContentHostedApprovalRequest {
				t.Fatal("incomplete hosted approval reached the next model request")
			}
		}
	}
}

func TestCanceledHostedApprovalDoesNotLeaveDanglingProjection(t *testing.T) {
	origin := content.Origin{
		Provider: "test",
		API:      "test.chat",
		Model:    "fixture",
		Scope:    "fixture",
	}
	request := llm.HostedApprovalRequest{
		ID:     "approval-1",
		Name:   "remote",
		Origin: origin,
	}
	result := llm.Result{
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
		Message: llm.AssistantMessage(
			llm.Text("before approval"),
			llm.Content{
				Kind:                  llm.ContentHostedApprovalRequest,
				HostedApprovalRequest: &request,
			},
		),
		Stop: llm.Stop{Kind: llm.StopToolUse},
	}
	client := &scriptedClient{results: []llm.Result{result}}
	rec := NewMemoryRecorder()
	first, err := start(t, client, nil, rec, Hooks{})
	if err != nil || first.Stop.Kind != StopWaiting {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	canceled, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(t.Context(), first.Turn, Cancel{CommandID: "cancel"})
	if err != nil || canceled.Stop.Kind != StopCanceled || len(canceled.Pending) != 0 {
		t.Fatalf("canceled=%+v err=%v", canceled, err)
	}
	messages, err := Project(canceled.Turn)
	if err != nil {
		t.Fatal(err)
	}
	keptText := false
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Kind == llm.ContentHostedApprovalRequest {
				t.Fatal("canceled approval reached projection")
			}
			if part.Kind == llm.ContentBlock && part.Block != nil && part.Block.Text != nil && part.Block.Text.Text == "before approval" {
				keptText = true
			}
		}
	}
	if !keptText {
		t.Fatal("non-approval content was lost")
	}
}
