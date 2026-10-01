package dashi

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

// nextRow is one row of the Next table. covers names the stops the row
// stands for, as found in the code by stopSites: "kind/reason", with "*"
// for a reason that is not a constant and, for a limited stop with a limit
// detail, the LimitKind in place of the reason.
type nextRow struct {
	name   string
	covers []string
	stop   *Stop
	setup  func(*replayState)
	want   Next
}

func nextRows() []nextRow {
	limited := func(kind LimitKind) *Stop {
		return &Stop{Kind: StopLimited, Reason: string(kind), Limit: &LimitDetail{Kind: kind}}
	}
	usd := func(amount money.Amount) *Money { return &Money{Amount: amount, Currency: "USD"} }
	modelCallsSpent := func(s *replayState) { s.ModelCalls = s.modelCallCap }
	costSpent := func(s *replayState) {
		s.Budget = &Budget{MaxCost: usd(100)}
		s.Spend.Cost = map[string]money.Amount{"USD": 100}
	}
	failed := func(reason string) *Stop { return &Stop{Kind: StopFailed, Reason: reason} }
	return []nextRow{
		{name: "not started", setup: func(s *replayState) { s.LastSeq = 0 }, want: NextNone},
		{name: "no stop", want: NextRecover},
		{
			name:  "no stop, model call in flight",
			setup: func(s *replayState) { s.modelCallStart = "m1" },
			want:  NextRecover,
		},
		{name: "completed", covers: []string{"completed/"}, stop: &Stop{Kind: StopCompleted}, want: NextNone},
		{name: "canceled", covers: []string{"canceled/*"}, stop: &Stop{Kind: StopCanceled}, want: NextNone},
		{
			name:   "deadline",
			covers: []string{"limited/deadline"},
			stop:   limited(LimitDeadline),
			want:   NextNone,
		},
		{
			name:   "resumes",
			covers: []string{"limited/resumes"},
			stop:   limited(LimitResumes),
			want:   NextNone,
		},
		{
			name:  "cost with unpriced spend",
			stop:  limited(LimitCost),
			setup: func(s *replayState) { costSpent(s); s.Spend.CostUnknown = 1 },
			want:  NextNone,
		},
		{
			name:   "model calls",
			covers: []string{"limited/model_calls"},
			stop:   limited(LimitModelCalls),
			setup:  modelCallsSpent,
			want:   NextRaiseModelCalls,
		},
		{
			name:   "cost",
			covers: []string{"limited/cost"},
			stop:   limited(LimitCost),
			setup:  costSpent,
			want:   NextRaiseBudget,
		},
		{
			name:  "cost after the last model call",
			stop:  limited(LimitCost),
			setup: func(s *replayState) { costSpent(s); modelCallsSpent(s) },
			want:  NextRaiseBudget,
		},
		{
			// The next call's cost could not be bounded, so the cap did
			// not stop the turn and a raise would not help.
			name: "cost risk unavailable",
			stop: &Stop{
				Kind:   StopLimited,
				Reason: "no price for model m",
				Limit:  &LimitDetail{Kind: LimitCost, UnpricedModel: "m"},
			},
			setup: func(s *replayState) { s.Budget = &Budget{MaxCost: usd(100)} },
			want:  NextFix,
		},
		{
			name:   "output tokens",
			covers: []string{"limited/output_tokens"},
			stop:   limited(LimitOutputTokens),
			setup: func(s *replayState) {
				s.Budget = &Budget{MaxOutputTokens: 10}
				s.Spend.Usage.OutputTokens = 10
			},
			want: NextRaiseBudget,
		},
		{
			name:   "total tokens",
			covers: []string{"limited/total_tokens"},
			stop:   limited(LimitTotalTokens),
			setup: func(s *replayState) {
				s.Budget = &Budget{MaxTotalTokens: 10}
				s.Spend.Usage.InputTokens = 10
			},
			want: NextRaiseBudget,
		},
		{
			name:   "provider pauses",
			covers: []string{"limited/provider_pauses"},
			stop:   limited(LimitProviderPauses),
			want:   NextContinue,
		},
		{
			// A BeforeSend hook's spending denial, with its own reason.
			name:   "host",
			covers: []string{"limited/host", "limited/*"},
			stop:   limited(LimitHost),
			want:   NextFix,
		},
		{name: "active context window", covers: []string{"limited/context_window"}, stop: limited(LimitContextWindow), want: NextFix},
		{name: "compaction failed", covers: []string{"failed/compaction_failed"}, stop: &Stop{Kind: StopFailed, Reason: StopReasonCompactionFailed}, want: NextFix},
		{
			name: "one call hit its output or context window",
			stop: &Stop{Kind: StopLimited, Reason: string(LimitOutputTokens)},
			want: NextFix,
		},
		{
			name:   "waiting, every answer recorded",
			covers: []string{"waiting/"},
			stop:   &Stop{Kind: StopWaiting, Reason: "tool result or approval pending"},
			want:   NextContinue,
		},
		{
			name:   "waiting on an approval",
			covers: []string{"waiting/tool result or approval pending"},
			stop:   &Stop{Kind: StopWaiting, Reason: "tool result or approval pending"},
			setup:  pendingCall(ToolApprovalPending),
			want:   NextResolvePending,
		},
		{
			name:  "waiting on an external result",
			stop:  &Stop{Kind: StopWaiting, Reason: "tool result or approval pending"},
			setup: pendingCall(ToolWaiting),
			want:  NextResolvePending,
		},
		{
			name:   "waiting on a provider approval",
			covers: []string{"waiting/provider approval pending"},
			stop:   &Stop{Kind: StopWaiting, Reason: "provider approval pending"},
			setup: func(s *replayState) {
				s.providerTool["h1"] = llm.ProviderToolApprovalRequest{ID: "h1"}
			},
			want: NextResolvePending,
		},
		{
			name: "interrupted",
			covers: []string{
				"interrupted/pause requested",
				"interrupted/worker stopped before result",
				"interrupted/*",
			},
			stop: &Stop{Kind: StopInterrupted, Reason: "pause requested"},
			want: NextContinue,
		},
		{
			name: "interrupted, interruptions used up",
			stop: &Stop{Kind: StopInterrupted, Reason: "context canceled"},
			setup: func(s *replayState) {
				s.modelCalls = 1
				s.Resumes = s.Agent.Limits.Resumes
			},
			want: NextFix,
		},
		{
			name:  "interrupted, provider tool approval pending",
			stop:  &Stop{Kind: StopInterrupted, Reason: "context canceled"},
			setup: func(s *replayState) { s.providerTool["h1"] = llm.ProviderToolApprovalRequest{ID: "h1"} },
			want:  NextFix,
		},
		{
			name:   "refused",
			covers: []string{"refused/*"},
			stop:   &Stop{Kind: StopRefused, Reason: "refusal"},
			want:   NextFix,
		},
		{
			name: "failed by a tool or a hook",
			covers: []string{
				"failed/" + StopReasonToolFatal,
				"failed/" + StopReasonToolOutcomeInvalid,
				"failed/" + StopReasonHookFailed,
				"failed/" + StopReasonOutputRejected,
			},
			stop: failed(StopReasonToolFatal),
			want: NextFix,
		},
		{
			name: "failed on a content reference",
			covers: []string{
				"failed/" + StopReasonContentNoLoader,
				"failed/" + StopReasonContentUnavailable,
				"failed/" + StopReasonContentTooLarge,
				"failed/" + StopReasonContentSizeMismatch,
				"failed/" + StopReasonContentHashMismatch,
			},
			stop: failed(StopReasonContentUnavailable),
			want: NextFix,
		},
		{
			name: "failed, free text",
			covers: []string{
				"failed/*",
				"failed/incomplete_model_result",
				"failed/empty_response",
				"failed/tool binding unavailable",
				"failed/tool binding changed or unavailable",
				"failed/provider tool cannot execute locally",
				"failed/prepared request changed destination",
				"failed/prepared request changed original model input",
				"failed/untrusted stop with client calls",
				"failed/untrusted stop with provider tool approval",
				"failed/native output_schema is not supported in agent turns",
				"failed/tool name is reserved for structured output",
			},
			stop: failed("provider rejected the request"),
			want: NextFix,
		},
		{
			name:   "result unknown, finished by the host",
			covers: []string{"result_unknown/*"},
			stop:   &Stop{Kind: StopResultUnknown, Reason: "tool call result unknown; the host must resolve it"},
			want:   NextContinue,
		},
		{
			name:   "result unknown tool call",
			covers: []string{"result_unknown/tool call result unknown; the host must resolve it"},
			stop:   &Stop{Kind: StopResultUnknown, Reason: "tool call result unknown; the host must resolve it"},
			setup:  pendingCall(ToolResultUnknown),
			want:   NextFix,
		},
		{
			name: "result unknown model call",
			covers: []string{
				"result_unknown/model call result unknown; the host must resolve it",
				"result_unknown/provider tool may have run",
			},
			stop:  &Stop{Kind: StopResultUnknown, Reason: "model call result unknown; the host must resolve it"},
			setup: func(s *replayState) { s.unresolvedModel = "m1" },
			want:  NextFix,
		},
		{
			name:  "output recorded, interrupted",
			stop:  &Stop{Kind: StopInterrupted, Reason: "context canceled"},
			setup: func(s *replayState) { s.Output = &StructuredOutput{} },
			want:  NextContinue,
		},
		{
			// The reserved final reply ends at the model-call limit; a
			// raise, not a bare Continue, completes it.
			name: "output recorded at the model-call limit",
			stop: limited(LimitModelCalls),
			setup: func(s *replayState) {
				modelCallsSpent(s)
				s.finalReplyOnly = true
				s.Output = &StructuredOutput{}
			},
			want: NextRaiseModelCalls,
		},
	}
}

// pendingCall adds one local call in the given state.
func pendingCall(status ToolStatus) func(*replayState) {
	return func(s *replayState) {
		s.Tools["c1"] = ToolView{Call: tool.Call{ID: "c1", Name: "write"}, Status: status}
		s.callOrder = append(s.callOrder, "c1")
	}
}

// rowView builds the view of a turn at rest at the row's stop. Readiness
// and Ended come from the same code View uses.
func rowView(row nextRow) TurnView {
	s := &replayState{
		TurnView:     TurnView{Tools: map[string]ToolView{}, LastSeq: 5},
		providerTool: map[string]llm.ProviderToolApprovalRequest{},
	}
	s.Agent.Limits.Resumes = 2
	s.modelCallCap, s.ModelCalls = 8, 1
	if row.stop != nil {
		s.Stop = *row.stop
	}
	if row.setup != nil {
		row.setup(s)
	}
	s.Stop.Ended = s.Stop.Kind != "" && s.ended()
	s.ReadyToContinue = s.canContinue()
	view := s.TurnView
	view.Stop = s.inferredStop()
	return view
}

func TestNextCoversEveryStop(t *testing.T) {
	rows := nextRows()
	covered := map[string]bool{}
	kinds := map[StopKind]bool{}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			view := rowView(row)
			if got := view.Next(); got != row.want {
				t.Fatalf("Next()=%s, want %s (stop %+v, ready %t)",
					got, row.want, view.Stop, view.ReadyToContinue)
			}
			if view.Stop.Kind == StopRefused && view.Next() == NextContinue {
				t.Fatal("a refusal asks the model again")
			}
		})
		for _, key := range row.covers {
			covered[key] = true
		}
		if row.stop != nil {
			kinds[row.stop.Kind] = true
		}
	}
	sites, consts := stopSites(t)
	for _, key := range sites {
		if !covered[key] {
			t.Errorf("stop %q in the code has no row in the Next table", key)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(consts)) {
		value := consts[name]
		switch {
		case strings.HasPrefix(name, "StopReason"):
			if !covered["failed/"+value] {
				t.Errorf("%s has no row in the Next table", name)
			}
		case strings.HasPrefix(name, "Limit"):
			if !covered["limited/"+value] {
				t.Errorf("%s has no row in the Next table", name)
			}
		default:
			if !kinds[StopKind(value)] {
				t.Errorf("%s has no row in the Next table", name)
			}
		}
	}
}

// stopSites reads the package source for every stop the code can record or
// infer, as "kind/reason" keys (see nextRow), and for the StopKind,
// LimitKind, and StopReason constants by name.
func stopSites(t *testing.T) ([]string, map[string]string) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var parsed []*ast.File
	consts := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, f)
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				typ, _ := vs.Type.(*ast.Ident)
				for i, id := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, _ := strconv.Unquote(lit.Value)
					if typ != nil && (typ.Name == "StopKind" || typ.Name == "LimitKind") ||
						strings.HasPrefix(id.Name, "StopReason") {
						consts[id.Name] = value
					}
				}
			}
		}
	}
	text := func(e ast.Expr) string {
		switch e := e.(type) {
		case nil:
			return ""
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				v, _ := strconv.Unquote(e.Value)
				return v
			}
		case *ast.Ident:
			if v, ok := consts[e.Name]; ok {
				return v
			}
		}
		return "*"
	}
	keys := map[string]bool{}
	add := func(kind, reason, limit ast.Expr) {
		// A literal whose kind is a variable, such as the one in
		// runtime.stop, is a helper that its callers are read through.
		id, ok := kind.(*ast.Ident)
		if !ok || consts[id.Name] == "" {
			return
		}
		key := consts[id.Name] + "/" + text(reason)
		if l := text(limit); limit != nil && l != "*" {
			key = consts[id.Name] + "/" + l
		}
		keys[key] = true
	}
	for _, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "stop" && len(n.Args) == 2 {
					add(n.Args[0], n.Args[1], nil)
				}
			case *ast.CompositeLit:
				if id, ok := n.Type.(*ast.Ident); !ok || id.Name != "Stop" {
					return true
				}
				var kind, reason, limit ast.Expr
				for _, elt := range n.Elts {
					kv := elt.(*ast.KeyValueExpr)
					switch kv.Key.(*ast.Ident).Name {
					case "Kind":
						kind = kv.Value
					case "Reason":
						reason = kv.Value
					case "Limit":
						limit = limitKind(kv.Value)
					}
				}
				add(kind, reason, limit)
			}
			return true
		})
	}
	if len(keys) < 20 {
		t.Fatalf("found only %d stops; the scan is broken", len(keys))
	}
	return slices.Sorted(maps.Keys(keys)), consts
}

// limitKind returns the Kind of a &LimitDetail{...} literal, or the
// expression itself when it is not one, which reads as free text.
func limitKind(e ast.Expr) ast.Expr {
	if u, ok := e.(*ast.UnaryExpr); ok {
		if lit, ok := u.X.(*ast.CompositeLit); ok {
			for _, elt := range lit.Elts {
				kv := elt.(*ast.KeyValueExpr)
				if kv.Key.(*ast.Ident).Name == "Kind" {
					return kv.Value
				}
			}
		}
	}
	return e
}

// Next reads the recorded stop, not the one View infers for a record
// that ends mid-run.
func TestNextReadsRecordedStop(t *testing.T) {
	crashed := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelCallStart("model-1"),
	)
	view, err := crashed.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Stop.Kind != StopInterrupted || view.Next() != NextRecover {
		t.Fatalf("crashed: stop=%+v next=%s", view.Stop, view.Next())
	}
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "write")}}
	out, err := start(t, client, writeTool(), NewMemoryRecorder(), deferredHooks())
	if err != nil || out.Next() != NextResolvePending {
		t.Fatalf("waiting: stop=%+v next=%s err=%v", out.Stop, out.Next(), err)
	}
	canceled, err := (Runner{Client: client}).Run(
		t.Context(), out.Turn, Cancel{CommandID: "cancel"},
	)
	if err != nil || canceled.Next() != NextNone {
		t.Fatalf("canceled: stop=%+v next=%s err=%v", canceled.Stop, canceled.Next(), err)
	}
}

// A hook's view of a live run gives NextRunning, on Start and on a Continue
// after a recorded stop.
func TestNextIsRunningInHookView(t *testing.T) {
	var seen []Next
	hooks := ModelHook{
		Name: "watch",
		Func: func(_ context.Context, plan ModelPlan) (ModelDecision, error) {
			seen = append(seen, plan.Turn.Next())
			return ModelDecision{}, nil
		},
	}.Hooks()
	paused := make(chan struct{})
	close(paused)
	client := &scriptedClient{results: []llm.Result{final("ok")}}
	runner := Runner{Client: client, Hooks: hooks}
	stopped, err := runner.Run(t.Context(), Turn{ID: "p"}, limitsStart(testAgent()), WithPause(paused))
	if err != nil || stopped.Stop.Kind != StopInterrupted {
		t.Fatalf("pause=%+v err=%v", stopped.Stop, err)
	}
	out, err := runner.Run(t.Context(), stopped.Turn, Continue{})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("continue=%+v err=%v", out.Stop, err)
	}
	if len(seen) == 0 {
		t.Fatal("hook never ran")
	}
	for i, next := range seen {
		if next != NextRunning {
			t.Fatalf("hook call %d saw %s, want running", i, next)
		}
	}
}

// Next reads only exported fields, so a view sent as JSON, for example to a
// UI, gives the same answer on the other side.
func TestNextSurvivesJSON(t *testing.T) {
	crashed := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelCallStart("model-1"),
	)
	client := &scriptedClient{results: []llm.Result{final("done")}}
	done, err := (Runner{Client: client}).Run(
		t.Context(), crashed, Continue{CommandID: "resume"},
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, turn := range map[string]Turn{"crashed": crashed, "completed": done.Turn} {
		view, err := turn.View()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		var decoded TurnView
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Next() != view.Next() {
			t.Fatalf("%s: Next %s became %s", name, view.Next(), decoded.Next())
		}
	}
}
