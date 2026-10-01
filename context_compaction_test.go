package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

type activeClient struct {
	scriptedClient
	window       int
	sent         []llm.Input
	prepareExtra int
}

func (c *activeClient) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{Capabilities: llm.Capabilities{ContextWindow: c.window}}, c.window > 0
}
func (c *activeClient) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return llm.Request{}, err
	}
	body = append(body, []byte(strings.Repeat(" ", c.prepareExtra))...)
	info, _ := c.ModelInfo(in.Model)
	info.Evidence = llm.ModelEvidence{Model: in.Model, API: "test.chat", Endpoint: "https://example.test/model", Owner: "test", Source: "fixture", Method: llm.EvidenceFixture, QualifiedAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)}
	return llm.NewRequest(llm.RequestSnapshot{Input: in, Effective: in.Settings, Origin: content.Origin{Provider: "test", API: "test.chat", Model: in.Model, Scope: "fixture"}, Body: body, ModelInfo: &info})
}
func (c *activeClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	c.sent = append(c.sent, call.Request.Snapshot().Input)
	return c.scriptedClient.Send(ctx, call)
}
func activeFixture() (*activeClient, Agent) {
	results := []llm.Result{callResult("c1", "read"), callResult("c2", "read"), final("<summary>Objective: fix the failure. Read evidence 1 and 2; no further writes needed.</summary>"), callResult("c3", "read"), callResult("c4", "read"), final("<summary>Objective: fix the failure. Read evidence 1 through 4. Checks passed.</summary>"), final("Fixed and verified.")}
	for i := range results {
		results[i].Usage = &llm.Usage{InputTokens: 100, OutputTokens: 2}
	}
	a := testAgent()
	a.Limits.ModelCalls = 12
	a.Settings.MaxTokens = 256
	a.ActiveCompaction = &ActiveCompactionPolicy{Window: 16000, MaxTokens: 256}
	return &activeClient{scriptedClient: scriptedClient{results: results}, window: 16000}, a
}
func activeTools(calls *int) tool.Toolset {
	return tool.List{tool.MustFunc("read", "Read test evidence", func(_ context.Context, _ tool.Call, _ struct {
		Value int `json:"value"`
	}) (tool.Outcome, error) {
		*calls++
		return tool.Succeeded(fmt.Sprintf("Evidence %d: %s", *calls, strings.Repeat("x", 9000))), nil
	})}
}
func activeStart(a Agent) Start {
	return Start{CommandID: "start", Agent: a, Input: []llm.Message{llm.UserMessage(llm.Text("Investigate the failure, fix it, and verify the original objective."))}}
}

func TestActiveCompactionLongRequestAndViews(t *testing.T) {
	c, a := activeFixture()
	calls := 0
	rec := NewMemoryRecorder()
	var purposes []ModelPurpose
	finish := 0
	r := Runner{Client: c, Tools: activeTools(&calls), Recorder: rec, Hooks: Hooks{BeforeModel: []ModelHook{{Name: "observe", Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
		purposes = append(purposes, p.Purpose)
		if p.Purpose == ModelPurposeCompaction && (len(p.Input.Tools) != 0 || p.Input.OutputSchema != nil) {
			t.Error("summary advertises work")
		}
		return ModelDecision{}, nil
	}}}, BeforeFinish: []FinishHook{{Name: "finish", Func: func(context.Context, FinishPlan) (FinishDecision, error) { finish++; return FinishDecision{}, nil }}}}}
	out, err := r.Run(t.Context(), Turn{ID: "active"}, activeStart(a))
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted || calls != 4 || len(out.ContextCompactions) != 2 || out.ModelCalls != 7 || finish != 1 {
		t.Fatalf("stop=%+v calls=%d applications=%d calls=%d finish=%d", out.Stop, calls, len(out.ContextCompactions), out.ModelCalls, finish)
	}
	if out.Reply != "Fixed and verified." || out.Spend.Usage.InputTokens != 700 || out.Spend.Usage.OutputTokens != 14 {
		t.Fatalf("answer/accounting=%+v", out.TurnView)
	}
	summaries := 0
	for _, p := range purposes {
		if p == ModelPurposeCompaction {
			summaries++
		}
	}
	if summaries != 2 {
		t.Fatalf("summary hooks=%d", summaries)
	}
	raw, err := TurnMessagesFull(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	view, err := TurnMessages(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	rawJSON, _ := json.Marshal(raw)
	viewJSON, _ := json.Marshal(view)
	if len(rawJSON) < 36000 || len(viewJSON) > 2000 || !strings.Contains(string(viewJSON), "original objective") || strings.Contains(string(rawJSON), activeSummaryFrame) {
		t.Fatalf("raw/context sizes=%d/%d", len(rawJSON), len(viewJSON))
	}
	data, _ := json.Marshal(out.Turn)
	var restored Turn
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	got, err := TurnMessages(restored)
	if err != nil || !canonjson.SameValue(got, view) {
		t.Fatalf("reload=%v", err)
	}
	for _, input := range c.sent {
		if input.System == activeSummaryPrompt {
			continue
		}
		b, _ := json.Marshal(input)
		if len(b)+1024+input.Settings.MaxTokens > 16000 {
			t.Errorf("oversized ordinary dispatch %d", len(b))
		}
	}
}

// A turn's own messages (TurnMessagesFull, which a session's History uses)
// keep the raw output an active compaction replaced, while the history of
// the next turn (BuildHistory) holds only the summary, before and after a
// reload.
func TestActiveCompactionHistorySeparation(t *testing.T) {
	c, a := activeFixture()
	calls := 0
	rec := NewMemoryRecorder()
	r := Runner{Client: c, Tools: activeTools(&calls), Recorder: rec}
	out, err := r.Run(t.Context(), NewTurn("session-1"), activeStart(a))
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("%+v %v", out.Stop, err)
	}
	loaded, err := rec.Load(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range []Turn{out.Turn, loaded} {
		raw, err := TurnMessagesFull(turn)
		if err != nil {
			t.Fatal(err)
		}
		view, warnings := BuildHistory([]Turn{turn})
		if len(warnings) > 0 {
			t.Fatal(warnings)
		}
		rb, _ := json.Marshal(raw)
		vb, _ := json.Marshal(view)
		if len(rb) < 36000 || len(vb) > 2000 {
			t.Fatalf("history=%d context=%d", len(rb), len(vb))
		}
	}
}
func TestActiveWindowAndSoftThreshold(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		catalog, explicit, want int
	}{{"explicit unknown", 0, 16000, 16000}, {"catalog cap", 32000, 16000, 16000}, {"known smaller", 8000, 16000, 8000}, {"unknown", 0, 0, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			c := &activeClient{scriptedClient: scriptedClient{results: []llm.Result{final("done")}}, window: tc.catalog}
			a := testAgent()
			a.Settings.MaxTokens = 256
			a.ActiveCompaction = &ActiveCompactionPolicy{Window: tc.explicit, Fraction: .01}
			out, err := (Runner{Client: c}).Run(t.Context(), Turn{}, activeStart(a))
			if err != nil || out.Stop.Kind != StopCompleted || out.ActiveCompactionWindow != tc.want || c.sends != 1 {
				t.Fatalf("stop=%+v window=%d sends=%d err=%v", out.Stop, out.ActiveCompactionWindow, c.sends, err)
			}
			warning := false
			for _, w := range out.Warnings {
				if w.Code == WarningActiveCompactionUnavailable {
					warning = true
				}
			}
			if warning != (tc.want == 0) {
				t.Fatalf("warning=%v", warning)
			}
		})
	}
}
func TestActiveCompactionFixedInputHardLimit(t *testing.T) {
	c := &activeClient{window: 4096}
	a := testAgent()
	a.Settings.MaxTokens = 256
	a.ActiveCompaction = &ActiveCompactionPolicy{}
	start := activeStart(a)
	start.Input = []llm.Message{llm.UserMessage(llm.Text(strings.Repeat("x", 8000)))}
	out, err := (Runner{Client: c}).Run(t.Context(), Turn{}, start)
	if err != nil || out.Stop.Limit == nil || out.Stop.Limit.Kind != LimitContextWindow || c.sends != 0 {
		t.Fatalf("stop=%+v sends=%d err=%v", out.Stop, c.sends, err)
	}
}

// Unknown acknowledgements stop the worker. Explicit reload+Continue is the
// only path to later dispatch, including an already completed summary.
type activeFault struct {
	Recorder
	kind  StepKind
	fired bool
}

func (f *activeFault) Load(ctx context.Context, id string) (Turn, error) {
	return f.Recorder.(Loader).Load(ctx, id)
}
func (f *activeFault) Commit(ctx context.Context, req CommitRequest) error {
	if !f.fired && req.Step.Kind == f.kind && (req.Step.Compacted != nil || req.Step.Model != nil && req.Step.Model.Purpose == ModelPurposeCompaction) {
		f.fired = true
		if err := f.Recorder.Commit(ctx, req); err != nil {
			return err
		}
		return errors.New("lost acknowledgement")
	}
	return f.Recorder.Commit(ctx, req)
}
func TestActiveCompactionAcknowledgedCrashRecovery(t *testing.T) {
	for _, kind := range []StepKind{StepModelCallStarted, StepModelCallFinished, StepContextCompacted} {
		t.Run(string(kind), func(t *testing.T) {
			c, a := activeFixture()
			calls := 0
			rec := NewMemoryRecorder()
			fault := &activeFault{Recorder: rec, kind: kind}
			r := Runner{Client: c, Tools: activeTools(&calls), Recorder: fault}
			first, err := r.Run(t.Context(), Turn{ID: "recover"}, activeStart(a))
			if _, ok := errors.AsType[*CommitError](err); !ok {
				t.Fatalf("err=%v stop=%+v", err, first.Stop)
			}
			dispatchedBefore := c.sends
			loaded, err := rec.Load(t.Context(), "recover")
			if err != nil {
				t.Fatal(err)
			}
			out, err := r.Run(t.Context(), loaded, Continue{CommandID: "continue"})
			if err != nil || out.Stop.Kind != StopCompleted || calls != 4 || len(out.ContextCompactions) != 2 || c.sends != 7 {
				t.Fatalf("stop=%+v calls=%d summaries=%d sends=%d prior=%d err=%v", out.Stop, calls, len(out.ContextCompactions), c.sends, dispatchedBefore, err)
			}
			if kind == StepModelCallStarted {
				found := false
				for _, step := range out.Turn.Steps {
					if step.Model != nil && step.Model.Purpose == ModelPurposeCompaction && step.Model.Interrupted == InterruptWorker {
						found = true
					}
				}
				if !found {
					t.Error("missing recovered attempt")
				}
			}
		})
	}
}

func TestActiveCompactionRejectsSummaryWorkAndNoSavings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result llm.Result
	}{{"empty", final("<summary></summary>")}, {"tool", callResult("evil", "read")}, {"incomplete", func() llm.Result { v := final("partial"); v.Stop.Kind = llm.StopOutputLimit; return v }()}, {"no savings", final(strings.Repeat("expanded", 5000))}} {
		t.Run(tc.name, func(t *testing.T) {
			c, a := activeFixture()
			tc.result.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 2}
			c.results[2] = tc.result
			calls := 0
			r := Runner{Client: c, Tools: activeTools(&calls)}
			out, err := r.Run(t.Context(), Turn{ID: "reject"}, activeStart(a))
			if err != nil || out.Stop.Kind != StopFailed || out.Stop.Reason != StopReasonCompactionFailed || len(out.ContextCompactions) != 0 || calls != 2 || out.ModelCalls != 3 || out.Spend.Usage.InputTokens != 300 {
				t.Fatalf("stop=%+v calls=%d calls=%d applied=%d spend=%+v err=%v", out.Stop, calls, out.ModelCalls, len(out.ContextCompactions), out.Spend, err)
			}
			if out.Reply != "" {
				t.Fatalf("summary became answer: %q", out.Reply)
			}
			// A new accepted summary on explicit Continue replaces the
			// rejected one.
			retry := final("<summary>Original objective. Evidence 1 and 2 checked.</summary>")
			retry.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 2}
			c.results = append(c.results[:3], append([]llm.Result{retry}, c.results[3:]...)...)
			resumed, err := r.Run(t.Context(), out.Turn, Continue{CommandID: "retry"})
			if err != nil || resumed.Stop.Kind != StopCompleted || calls != 4 {
				t.Fatalf("resume=%+v calls=%d err=%v", resumed.Stop, calls, err)
			}
		})
	}
}
func TestActiveCompactionModelCallAndOutputBudget(t *testing.T) {
	t.Run("reserve response", func(t *testing.T) {
		c, a := activeFixture()
		a.Limits.ModelCalls = 3
		calls := 0
		out, err := (Runner{Client: c, Tools: activeTools(&calls)}).Run(t.Context(), Turn{}, activeStart(a))
		if err != nil || out.Stop.Kind != StopLimited || out.Stop.Reason != "model_calls" || c.sends != 2 || calls != 2 {
			t.Fatalf("stop=%+v sends=%d calls=%d err=%v", out.Stop, c.sends, calls, err)
		}
	})
	t.Run("summary shares output budget", func(t *testing.T) {
		c, a := activeFixture()
		calls := 0
		start := activeStart(a)
		start.Budget = &Budget{MaxOutputTokens: 6}
		out, err := (Runner{Client: c, Tools: activeTools(&calls)}).Run(t.Context(), Turn{}, start)
		if err != nil || out.Stop.Kind != StopLimited || out.Spend.Usage.OutputTokens != 6 || c.sends != 3 || calls != 2 || len(out.ContextCompactions) != 1 {
			t.Fatalf("stop=%+v spend=%+v sends=%d effects=%d err=%v", out.Stop, out.Spend, c.sends, calls, err)
		}
		if c.sent[2].Settings.MaxTokens != 2 {
			t.Fatalf("summary output allowance=%d", c.sent[2].Settings.MaxTokens)
		}
	})
}
func TestActiveCompactionHookMessagesReachOrdinaryRequest(t *testing.T) {
	c, a := activeFixture()
	calls := 0
	var sends []ModelPurpose
	note := "Keep this exact verification note for ordinary work."
	hooks := Hooks{BeforeModel: []ModelHook{{Name: "summary-note", Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
		if p.Purpose != ModelPurposeCompaction {
			return ModelDecision{}, nil
		}
		return ModelDecision{Settings: &llm.Settings{MaxTokens: 5000, ToolChoice: &llm.ToolChoice{Mode: llm.ToolChoiceAny}}, AddMessages: []ContextItem{{ID: "summary-note", Name: "verification", Origin: ContextRuntime, NextCallOnly: true, Message: llm.UserMessage(llm.Text(note))}}}, nil
	}}}, BeforeSend: []SendHook{{Name: "purpose", Func: func(_ context.Context, p SendPlan) (SendDecision, error) {
		sends = append(sends, p.Purpose)
		if p.Purpose == ModelPurposeCompaction {
			in := p.Request.Snapshot().Input
			if len(in.Tools) > 0 || in.Settings.ToolChoice != nil || in.Settings.MaxTokens > 256 {
				t.Error("summary hook enabled work or expanded allowance")
			}
		}
		return SendDecision{}, nil
	}}}}
	out, err := (Runner{Client: c, Tools: activeTools(&calls), Hooks: hooks}).Run(t.Context(), Turn{}, activeStart(a))
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if len(sends) != 7 || sends[2] != ModelPurposeCompaction {
		t.Fatalf("send purposes=%v", sends)
	}
	b, _ := json.Marshal(c.sent[3].Messages)
	if !strings.Contains(string(b), note) {
		t.Fatal("summary delivery expired before ordinary work")
	}
}
func TestActiveCompactionPreparedBodyHardLimit(t *testing.T) {
	c := &activeClient{window: 8000, prepareExtra: 9000}
	a := testAgent()
	a.ActiveCompaction = &ActiveCompactionPolicy{}
	a.Settings.MaxTokens = 256
	out, err := (Runner{Client: c}).Run(t.Context(), Turn{}, activeStart(a))
	if err != nil || out.Stop.Limit == nil || out.Stop.Limit.Kind != LimitContextWindow || c.sends != 0 {
		t.Fatalf("stop=%+v sends=%d err=%v", out.Stop, c.sends, err)
	}
}
func TestActiveCompactionRejectsChangedCoverage(t *testing.T) {
	c, a := activeFixture()
	calls := 0
	out, err := (Runner{Client: c, Tools: activeTools(&calls)}).Run(t.Context(), Turn{ID: "proof"}, activeStart(a))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Turn){
		func(t *Turn) {
			for i := range t.Steps {
				f := t.Steps[i].Model
				if f != nil && f.Compaction != nil {
					f.Compaction.Covered = f.Compaction.Covered[:1]
					return
				}
			}
		},
		func(t *Turn) {
			for i := range t.Steps {
				f := t.Steps[i].Model
				if f != nil && f.Compaction != nil {
					f.Compaction.Covered[0] = TurnMessageRef{Seq: 1}
					return
				}
			}
		},
		func(t *Turn) {
			for i := range t.Steps {
				f := t.Steps[i].Model
				if f != nil && f.Compaction != nil {
					f.Compaction.Hash = "changed"
					return
				}
			}
		},
		func(t *Turn) {
			for i := range t.Steps {
				f := t.Steps[i].Compacted
				if f != nil {
					f.Summary = "changed"
					return
				}
			}
		},
		func(t *Turn) {
			for i := range t.Steps {
				f := t.Steps[i].Model
				if f != nil && f.Purpose == ModelPurposeCompaction && t.Steps[i].Kind == StepModelCallFinished {
					f.Purpose = "other"
					return
				}
			}
		},
	} {
		changed := cloneTurn(out.Turn)
		mutate(&changed)
		if _, err := changed.View(); !errors.Is(err, ErrInvalidTurn) {
			t.Fatalf("changed record accepted: %v", err)
		}
	}
}

type cancelActiveClient struct {
	*activeClient
	cancel context.CancelFunc
	once   bool
}

func (c *cancelActiveClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	if !c.once && call.Request.Snapshot().Input.System == activeSummaryPrompt {
		c.once = true
		c.cancel()
		return llm.Result{}, ctx.Err()
	}
	return c.activeClient.Send(ctx, call)
}
func TestActiveCompactionCancellationDoesNotRepeatWrites(t *testing.T) {
	for _, behavior := range []ContextDone{ContextInterrupt, ContextCancel} {
		t.Run(string(behavior), func(t *testing.T) {
			base, a := activeFixture()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := &cancelActiveClient{activeClient: base, cancel: cancel}
			logPath := t.TempDir() + "/effects"
			calls := 0
			write := tool.MustFunc("read", "Record an effect, then return evidence", func(_ context.Context, call tool.Call, _ struct {
				Value int `json:"value"`
			}) (tool.Outcome, error) {
				f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
				if err != nil {
					return tool.Outcome{}, err
				}
				_, err = f.WriteString(call.ID + "\n")
				closeErr := f.Close()
				if err != nil {
					return tool.Outcome{}, err
				}
				if closeErr != nil {
					return tool.Outcome{}, closeErr
				}
				calls++
				return tool.Succeeded(strings.Repeat("x", 9000)), nil
			})
			rec := NewMemoryRecorder()
			r := Runner{Client: client, Tools: tool.List{write}, Recorder: rec, OnContextDone: behavior}
			first, err := r.Run(ctx, Turn{ID: "cancel"}, activeStart(a))
			want := StopInterrupted
			if behavior == ContextCancel {
				want = StopCanceled
			}
			if err != nil || first.Stop.Kind != want || calls != 2 || len(first.ContextCompactions) != 0 {
				t.Fatalf("stop=%+v calls=%d err=%v", first.Stop, calls, err)
			}
			loaded, err := rec.Load(t.Context(), "cancel")
			if err != nil {
				t.Fatal(err)
			}
			out, err := r.Run(t.Context(), loaded, Continue{CommandID: "continue"})
			if behavior == ContextCancel {
				if !errors.Is(err, ErrTurnEnded) {
					t.Fatalf("canceled turn resumed %v", err)
				}
				return
			}
			if err != nil || out.Stop.Kind != StopCompleted || calls != 4 {
				t.Fatalf("resumed=%+v calls=%d err=%v", out.Stop, calls, err)
			}
			bytes, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(bytes) != "c1\nc2\nc3\nc4\n" {
				t.Fatalf("repeated writes: %q", bytes)
			}
		})
	}
}
func TestActiveCompactionProtectedHooksCannotBuyRoom(t *testing.T) {
	c, a := activeFixture()
	calls := 0
	hook := ModelHook{Name: "large-runtime", Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
		if p.Turn.ModelCalls < 2 {
			return ModelDecision{}, nil
		}
		return ModelDecision{AddMessages: []ContextItem{{ID: "large", Origin: ContextRuntime, Name: "fixed", Message: llm.UserMessage(llm.Text(strings.Repeat("protected", 4000)))}}}, nil
	}}
	out, err := (Runner{Client: c, Tools: activeTools(&calls), Hooks: Hooks{BeforeModel: []ModelHook{hook}}}).Run(t.Context(), Turn{}, activeStart(a))
	if err != nil || out.Stop.Limit == nil || out.Stop.Limit.Kind != LimitContextWindow || c.sends != 2 || len(out.ContextCompactions) != 0 {
		t.Fatalf("stop=%+v sends=%d summaries=%d err=%v", out.Stop, c.sends, len(out.ContextCompactions), err)
	}
}

func activeManyCompactionsFixture(tb testing.TB) (*activeClient, Agent, tool.Toolset, *int) {
	tb.Helper()
	c, a := activeFixture()
	c.results = nil
	a.Limits.ModelCalls = 32
	calls := new(int)
	for i := 1; i <= 12; i++ {
		reply := callResult(fmt.Sprintf("c%d", i), "read")
		reply.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 2}
		c.results = append(c.results, reply)
		if i%2 == 0 {
			summary := final(fmt.Sprintf("<summary>Original objective. Evidence 1 through %d checked. Remaining work recorded.</summary>", i))
			summary.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 2}
			c.results = append(c.results, summary)
		}
	}
	c.results = append(c.results, final("All twelve effects verified."))
	return c, a, activeTools(calls), calls
}
func TestActiveCompactionManyReplacementsReload(t *testing.T) {
	c, a, tools, calls := activeManyCompactionsFixture(t)
	out, err := (Runner{Client: c, Tools: tools}).Run(t.Context(), Turn{ID: "many"}, activeStart(a))
	if err != nil || out.Stop.Kind != StopCompleted || *calls != 12 || len(out.ContextCompactions) != 6 {
		t.Fatalf("stop=%+v effects=%d replacements=%d err=%v", out.Stop, *calls, len(out.ContextCompactions), err)
	}
	data, _ := json.Marshal(out.Turn)
	var restored Turn
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	cache := &contextValidationCache{prefixes: map[int64][]contextMessage{}}
	view, err := replayCached(restored, cache)
	if err != nil || len(view.ContextCompactions) != 6 || len(cache.prefixes) != 6 || len(cache.states) != 6 {
		t.Fatalf("prefixes=%d states=%d replay=%v", len(cache.prefixes), len(cache.states), err)
	}
	raw, err := TurnMessagesFull(restored)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := TurnMessages(restored)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := json.Marshal(raw)
	pb, _ := json.Marshal(projected)
	if len(rb) < 108000 || len(pb) > 2000 {
		t.Fatalf("raw/context=%d/%d", len(rb), len(pb))
	}
	// A corrupt earlier plan still fails the full long-history reload.
	for i := range restored.Steps {
		if f := restored.Steps[i].Model; f != nil && f.Compaction != nil {
			f.Compaction.Hash = "corrupt"
			break
		}
	}
	if _, err = restored.View(); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("malformed long record=%v", err)
	}
}
func BenchmarkActiveCompactionReload(b *testing.B) {
	c, a, tools, _ := activeManyCompactionsFixture(b)
	out, err := (Runner{Client: c, Tools: tools}).Run(context.Background(), Turn{ID: "benchmark"}, activeStart(a))
	if err != nil || out.Stop.Kind != StopCompleted {
		b.Fatalf("%+v %v", out.Stop, err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := TurnMessages(out.Turn); err != nil {
			b.Fatal(err)
		}
	}
}

func TestActiveCompactionHydratedReferenceSizing(t *testing.T) {
	bytes := []byte(strings.Repeat("x", 10000))
	ref := refFor("evidence", bytes)
	resolver := &countingResolver{files: map[string][]byte{"evidence": bytes}}
	c := &activeClient{window: 8000, scriptedClient: scriptedClient{results: []llm.Result{final("inspected")}}}
	a := testAgent()
	a.Settings.MaxTokens = 256
	a.ActiveCompaction = &ActiveCompactionPolicy{}
	start := activeStart(a)
	start.Input[0].Content = append(start.Input[0].Content, llm.Document(content.Source{Ref: &ref, MIMEType: "application/pdf"}, "evidence.pdf"))
	out, err := (Runner{Client: c, ContentLoader: resolver}).Run(t.Context(), Turn{}, start)
	if err != nil || out.Stop.Kind != StopCompleted || resolver.count() != 1 || c.sends != 1 || activeMediaWarnings(out.Warnings) != 1 {
		t.Fatalf("stop=%+v resolutions=%d sends=%d err=%v", out.Stop, resolver.count(), c.sends, err)
	}
	if out.Turn.Steps[0].Started.Input[0].Content[1].Block.Document.Source.Ref == nil {
		t.Fatal("hydration rewrote recorded evidence")
	}
}
func TestActiveCompactionCompleteReplayGroup(t *testing.T) {
	c, a := activeFixture()
	calls := 0
	paused := final(strings.Repeat("public finding ", 600))
	paused.Stop.Kind = llm.StopPause
	paused.Message.Content = append(paused.Message.Content, llm.Content{Kind: llm.ContentReasoning, Reasoning: &llm.Reasoning{Text: "private scratch"}}, llm.Content{Kind: llm.ContentOpaque, Opaque: &llm.Opaque{Version: 1, Origin: content.Origin{Provider: "test", API: "test.chat", Model: "fixture", Scope: "fixture"}, Unit: llm.OpaqueUnitItem, Type: "provider-continuation", Raw: json.RawMessage(`{"private":"continuation"}`)}})
	c.results = []llm.Result{paused, callResult("call", "read"), final("<summary>Objective retained. Public findings and completed read recorded.</summary>"), final("verified")}
	out, err := (Runner{Client: c, Tools: activeTools(&calls)}).Run(t.Context(), Turn{}, activeStart(a))
	if err != nil || out.Stop.Kind != StopCompleted || len(out.ContextCompactions) != 1 || calls != 1 {
		t.Fatalf("stop=%+v applications=%d effects=%d err=%v", out.Stop, len(out.ContextCompactions), calls, err)
	}
	for _, in := range c.sent {
		if in.System == activeSummaryPrompt {
			b, _ := json.Marshal(in.Messages)
			if strings.Contains(string(b), "private scratch") || strings.Contains(string(b), "provider-continuation") {
				t.Fatalf("private replay entered summary: %s", b)
			}
		}
	}
	raw, err := TurnMessagesFull(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	view, err := TurnMessages(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := json.Marshal(raw)
	vb, _ := json.Marshal(view)
	if !strings.Contains(string(rb), "provider-continuation") || strings.Contains(string(vb), "provider-continuation") {
		t.Error("complete replay group was split or lost from recorded history")
	}
}

func TestActiveCompactionSummaryUsesLiveTurnMessages(t *testing.T) {
	c, a := activeFixture()
	c.results = append(c.results[:3], final("done"))
	temporary := strings.Repeat("temporary note ", 560)
	runtimeNote := "Persistent runtime verification instruction."
	operatorNote := "Operator instruction: retain the original objective."
	runtimeMessage := llm.UserMessage(llm.Text(runtimeNote))
	operatorMessage := llm.UserMessage(llm.Text(operatorNote))
	operatorMessage.Role = llm.RoleDeveloper
	hooks := Hooks{BeforeModel: []ModelHook{{Name: "context", Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
		if p.Turn.ModelCalls != 0 {
			return ModelDecision{}, nil
		}
		return ModelDecision{AddMessages: []ContextItem{
			{ID: "temporary", Origin: ContextRuntime, NextCallOnly: true, Message: llm.UserMessage(llm.Text(temporary))},
			{ID: "runtime", Origin: ContextRuntime, Name: "persistent", Message: runtimeMessage},
			{ID: "operator", Origin: ContextOperator, Name: "objective", Message: operatorMessage},
		}}, nil
	}}}}
	calls := 0
	out, err := (Runner{Client: c, Tools: activeTools(&calls), Hooks: hooks}).Run(t.Context(), Turn{ID: "inferred-context"}, activeStart(a))
	if err != nil || out.Stop.Kind != StopCompleted || calls != 2 || c.sends != 4 || len(out.ContextCompactions) != 1 {
		t.Fatalf("stop=%+v calls=%d sends=%d summaries=%d err=%v", out.Stop, calls, c.sends, len(out.ContextCompactions), err)
	}
	for i, input := range c.sent {
		hasTemporary := false
		foundRuntime, foundOperator := -1, -1
		for index, message := range input.Messages {
			if strings.Contains(message.Text(), "temporary note") {
				hasTemporary = true
			}
			if message.Text() == runtimeNote {
				foundRuntime = index
				if !canonjson.SameValue(message, runtimeMessageWithContext(runtimeMessage, string(ContextRuntime), "persistent")) {
					t.Fatalf("send %d runtime role/metadata changed: %+v", i, message)
				}
			}
			if message.Text() == operatorNote {
				foundOperator = index
				if message.Role != llm.RoleDeveloper || message.Context == nil || message.Context.Origin != string(ContextOperator) || message.Context.Name != "objective" {
					t.Fatalf("send %d operator role/metadata changed: %+v", i, message)
				}
			}
		}
		if hasTemporary != (i == 0) {
			t.Fatalf("send %d expired one-call context=%v", i, hasTemporary)
		}
		if foundRuntime < 0 || foundOperator < foundRuntime {
			t.Fatalf("send %d persistent deliveries lost/reordered: runtime=%d operator=%d", i, foundRuntime, foundOperator)
		}
	}
	rawTemporary := false
	for _, step := range out.Turn.Steps {
		if step.Context != nil {
			for _, item := range step.Context.Items {
				if item.ID == "temporary" && item.Message.Text() == temporary {
					rawTemporary = true
				}
			}
		}
	}
	if !rawTemporary {
		t.Fatal("expired delivery erased from raw facts")
	}
	if c.sent[2].System != activeSummaryPrompt {
		t.Fatal("fixture did not dispatch a summary")
	}
}

func runtimeMessageWithContext(message llm.Message, origin, name string) llm.Message {
	message = message.Clone()
	message.Context = &llm.MessageContext{Origin: origin, Name: name}
	return message
}
