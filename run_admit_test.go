package dashi

import (
	"context"
	"errors"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

type admitToolset struct{ t *testing.T }

func (s admitToolset) Tools(context.Context) ([]tool.Tool, error) {
	s.t.Fatal("admission discovered tools")
	return nil, nil
}

type admitSteering struct{ t *testing.T }

func (s admitSteering) Steers(context.Context, string) ([]Steer, error) {
	s.t.Fatal("admission fetched steering")
	return nil, nil
}

func (s admitSteering) Delivered(context.Context, string, []string) error {
	s.t.Fatal("admission acknowledged steering")
	return nil
}

type admitUntouchedClient struct{ t *testing.T }

func (c admitUntouchedClient) Provider() string {
	c.t.Fatal("retry consulted current provider")
	return ""
}

func (c admitUntouchedClient) API() string {
	c.t.Fatal("retry consulted current API")
	return ""
}

func (c admitUntouchedClient) ModelInfo(string) (llm.ModelInfo, bool) {
	c.t.Fatal("retry consulted current catalog")
	return llm.ModelInfo{}, false
}

func (c admitUntouchedClient) Prepare(context.Context, llm.Input) (llm.Request, error) {
	c.t.Fatal("retry prepared a request")
	return llm.Request{}, nil
}

func (c admitUntouchedClient) Send(context.Context, llm.Call) (llm.Result, error) {
	c.t.Fatal("retry sent a request")
	return llm.Result{}, nil
}

func TestRunnerAdmitRecordsWithoutExecutingServices(t *testing.T) {
	rec := NewMemoryRecorder()
	if err := rec.Claim(t.Context(), "queued", "owner"); err != nil {
		t.Fatal(err)
	}
	loader := &countingResolver{files: map[string][]byte{"ref": refBytes}}
	start := Say("do it")
	start.CommandID, start.Agent = "start", testAgent()
	start.Context = []ContextItem{{Message: refMessage(refFor("ref", refBytes))}}
	paused := make(chan struct{})
	close(paused)
	runner := Runner{
		Client:        noModelClient{t: t},
		Recorder:      rec,
		Tools:         admitToolset{t: t},
		ContentLoader: loader,
		Hooks: Hooks{BeforeModel: []ModelHook{{
			Name: "never",
			Func: func(context.Context, ModelPlan) (ModelDecision, error) {
				t.Fatal("admission ran a hook")
				return ModelDecision{}, nil
			},
		}}},
	}
	out, err := runner.Admit(
		t.Context(), Turn{ID: "queued"}, start,
		WithOwner("owner"), WithPause(paused), WithSteering(admitSteering{t: t}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopReady || out.Next() != NextContinue || out.Stop.Ended {
		t.Fatalf("admission is not ready: %+v", out.TurnView)
	}
	if out.ModelCalls != 0 || out.Resumes != 0 || len(out.ModelCallCosts) != 0 {
		t.Fatalf("admission executed work: %+v", out.TurnView)
	}
	if loader.count() != 0 {
		t.Fatal("admission loaded content")
	}
	saved, err := rec.Load(t.Context(), "queued")
	if err != nil || len(saved.Steps) != 2 {
		t.Fatalf("owner-fenced admission was not saved: %v", err)
	}
	if _, err := (Runner{Recorder: rec}).Run(
		t.Context(), saved, Cancel{CommandID: "cancel"}, WithOwner("wrong"),
	); !errors.Is(err, ErrStaleTurn) {
		t.Fatalf("wrong owner canceled admission: %v", err)
	}
	canceled, err := (Runner{Recorder: rec}).Run(
		t.Context(), saved, Cancel{CommandID: "cancel"}, WithOwner("owner"),
	)
	if err != nil || canceled.Stop.Kind != StopCanceled {
		t.Fatalf("credential-free cancellation failed: %v", err)
	}
}

func TestRunnerAdmitCompactionWindowWithoutClient(t *testing.T) {
	start := Say("do it")
	start.Agent = testAgent()
	start.Agent.ActiveCompaction = &ActiveCompactionPolicy{Window: 32000}
	out, err := (Runner{}).Admit(t.Context(), Turn{ID: "queued"}, start)
	if err != nil || out.ActiveCompactionWindow != 32000 {
		t.Fatalf("explicit window requires client: window=%d err=%v", out.ActiveCompactionWindow, err)
	}
	start.Agent.ActiveCompaction.Window = 0
	out, err = (Runner{}).Admit(t.Context(), Turn{ID: "unknown-window"}, start)
	if err == nil || Recorded(err) || len(out.Turn.Steps) != 0 {
		t.Fatalf("missing window was accepted: %v", err)
	}
}

func TestRunnerStartRetryUsesOriginallyRecordedDefaults(t *testing.T) {
	start := Say("accepted work")
	start.CommandID = "start"
	start.Agent.Model = "fixture"
	start.Agent.ActiveCompaction = &ActiveCompactionPolicy{}
	out, err := (Runner{Client: &activeClient{window: 128000}}).Admit(
		t.Context(), Turn{ID: "queued"}, start,
	)
	if err != nil {
		t.Fatal(err)
	}
	// These resolved defaults represent a record accepted by another
	// library version. The retry still carries zero/default fields.
	a := &out.Turn.Steps[0].Started.Agent
	a.Limits.ModelCalls = 13
	a.Limits.Resumes = 7
	a.Limits.ProviderPauses = 6
	a.Limits.ToolConcurrency = 3
	a.Limits.ToolResultBytes = 8192
	a.ActiveCompaction.Fraction = .55
	a.ActiveCompaction.MaxTokens = 512
	a.Version = agentVersion(*a)
	hashed := start
	hashed.Agent = *a
	out.Turn.Steps[0].CommandHash, err = commandHash(hashed)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewMemoryRecorder()
	for _, step := range out.Turn.Steps {
		if err := rec.Commit(t.Context(), CommitRequest{
			Step: step, ExpectedSeq: step.Seq - 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, client := range []llm.Client{nil, admitUntouchedClient{t: t}} {
		runner := Runner{Recorder: rec, Client: client}
		retried, err := runner.Admit(t.Context(), Turn{ID: "queued"}, start)
		if err != nil || retried.Agent.Limits.ModelCalls != 13 ||
			retried.ActiveCompactionWindow != 128000 {
			t.Fatalf("retry lost original defaults: %+v err=%v", retried.TurnView, err)
		}
		if _, err := runner.Run(t.Context(), Turn{ID: "queued"}, start); err != nil {
			t.Fatalf("Run did not recognize admission: %v", err)
		}
	}
	changed := start
	changed.Agent.Limits.ModelCalls = 14
	if _, err := (Runner{Recorder: rec}).Admit(
		t.Context(), Turn{ID: "queued"}, changed,
	); !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("explicit changed behavior did not conflict: %v", err)
	}
}

func TestRunnerContinueAdmittedTurnDoesNotConsumeResume(t *testing.T) {
	start := Say("do it")
	start.CommandID, start.Agent = "start", testAgent()
	start.Agent.Limits.Resumes = -1
	out, err := (Runner{}).Admit(t.Context(), Turn{ID: "queued"}, start)
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err = (Runner{Client: client}).Run(
		t.Context(), out.Turn, Continue{CommandID: "continue"},
	)
	if err != nil || out.Stop.Kind != StopCompleted || out.Resumes != 0 {
		t.Fatalf("admission consumed a resume: %+v err=%v", out.TurnView, err)
	}
}

type admitFaultRecorder struct {
	*MemoryRecorder
	seq   int64
	write bool
	fired bool
}

func (r *admitFaultRecorder) Commit(ctx context.Context, req CommitRequest) error {
	if req.Step.Seq == r.seq && !r.fired {
		r.fired = true
		if r.write {
			if err := r.MemoryRecorder.Commit(ctx, req); err != nil {
				return err
			}
		}
		return errors.New("acknowledgment lost")
	}
	return r.MemoryRecorder.Commit(ctx, req)
}

func TestRunnerAdmitUncertainWritesReloadBeforeRetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seq   int64
		write bool
	}{
		{name: "lost start write", seq: 1},
		{name: "lost start ack", seq: 1, write: true},
		{name: "lost ready write", seq: 2},
		{name: "lost ready ack", seq: 2, write: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &admitFaultRecorder{
				MemoryRecorder: NewMemoryRecorder(), seq: tc.seq, write: tc.write,
			}
			start := Say("do it")
			start.CommandID, start.Agent = "start", testAgent()
			runner := Runner{Recorder: rec}
			out, err := runner.Admit(t.Context(), Turn{ID: "queued"}, start)
			var ce *CommitError
			if !errors.As(err, &ce) || ce.NothingWritten || ce.Step.Seq != tc.seq {
				t.Fatalf("uncertain write lost evidence: %v", err)
			}
			if len(out.Turn.Steps) != int(tc.seq-1) {
				t.Fatal("unacknowledged step appeared in returned Turn")
			}
			retry, err := runner.Admit(t.Context(), Turn{ID: "queued"}, start)
			if err != nil {
				t.Fatal(err)
			}
			want := NextContinue
			if tc.seq == 1 && tc.write || tc.seq == 2 && !tc.write {
				want = NextRecover
			}
			if retry.Next() != want {
				t.Fatalf("retry next=%s want=%s", retry.Next(), want)
			}
			if retry.ModelCalls != 0 {
				t.Fatal("uncertain admission executed a model call")
			}
		})
	}
}
