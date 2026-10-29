package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

// codecCommands returns every command type with its optional fields empty
// and with them set.
func codecCommands() map[string]Command {
	temp := 0.2
	expires := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	call := tool.Call{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q":"x","n":1}`)}
	history := []llm.Message{
		llm.UserMessage(llm.Text("hi")),
		llm.AssistantMessage(llm.Content{Kind: llm.ContentToolCall, ToolCall: &call}),
		{Role: llm.RoleUser, Content: []llm.Content{{
			Kind: llm.ContentToolResult,
			ToolResult: &llm.ToolResult{
				CallID:  "c1",
				Output:  tool.TextOutput("found"),
				IsError: true,
			},
		}}},
	}
	input := []llm.Message{llm.UserMessage(
		llm.Text("look"),
		llm.Image(content.Source{Data: []byte{0x89, 'P', 'N', 'G'}, MIMEType: "image/png"}),
		llm.Document(content.Source{URL: "https://example.com/a.pdf", ExpiresAt: &expires}, "a.pdf"),
	)}
	budget := &Budget{
		MaxOutputTokens: 100,
		MaxTotalTokens:  1000,
		MaxCost:         &Money{Amount: money.MustParseAmount("1.25"), Currency: "USD"},
	}
	origin := content.Origin{Provider: "p", API: "a", Model: "m"}
	return map[string]Command{
		"start/empty": Start{},
		"start/full": Start{
			CommandID: "cmd-start",
			Agent: Agent{
				ID:           "agent",
				Version:      "r1",
				Instructions: "be brief",
				Provider:     "p",
				API:          "a",
				Model:        "m",
				Settings: llm.Settings{
					MaxTokens:   50,
					Temperature: &temp,
					PromptCache: llm.PromptCache{Key: "k"},
				},
				NoInheritedSettings: true,
				OutputSchema:        json.RawMessage(`{"type":"object"}`),
				Tools:               []string{"lookup"},
				Limits: Limits{
					ModelCalls: 3,
					Resumes:    -1,
					Deadline:   expires,
				},
				ActiveCompaction: &ActiveCompactionPolicy{Window: 2048, Fraction: 0.5},
			},
			Origin:       StartCompaction,
			Budget:       budget,
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
			History:      history,
			Input:        input,
			Parent:       &CallRef{TurnID: "t0", CallID: "c0"},
			Compaction: &CompactionFact{
				Through:   2,
				Hash:      "d",
				Keep:      history[:1],
				SummaryAt: 1,
				Frame:     "summary:",
			},
		},
		"add_messages/empty": AddMessages{},
		"add_messages/full": AddMessages{
			CommandID: "cmd-deliver",
			Items: []ContextItem{{
				ID:           "i1",
				Name:         "env",
				Origin:       ContextRuntime,
				Message:      llm.UserMessage(llm.Text("state")),
				Snapshot:     true,
				NextCallOnly: true,
			}},
		},
		"continue/empty": Continue{},
		"continue/full": Continue{
			CommandID:         "cmd-continue",
			History:           history,
			Budget:            budget,
			ModelCalls:        9,
			OnHistoryMismatch: HistoryMismatchReject,
		},
		"resolve/empty": Resolve{},
		"resolve/outcome": Resolve{
			CommandID: "cmd-finish",
			CallID:    "c1",
			Outcome: &tool.Outcome{
				State:  tool.StateSucceeded,
				Output: &tool.Output{Content: []content.Block{content.TextBlock("ok")}, Display: "ok!"},
				Cost:   &tool.Cost{Amount: money.MustParseAmount("0.01"), Currency: "USD"},
			},
		},
		"resolve/approval": Resolve{
			CommandID: "cmd-approve",
			CallID:    "c1",
			Approval: &ApprovalDecision{
				CallID: "c1", Approve: false, ArgsHash: ArgsHash(call.Args), Reason: "no",
				RulesVersion: "policy-v3", RulesFinding: "risk 91%",
			},
		},
		"resolve/provider tool": Resolve{
			CommandID:            "cmd-provider tool",
			ProviderToolApproval: &llm.ProviderToolApprovalResponse{RequestID: "r1", Approve: true, Reason: "fine", Origin: origin},
		},
		"resolve/result": Resolve{
			CommandID: "cmd-result",
			ModelResult: &llm.Result{
				ID:      "resp",
				Message: llm.AssistantMessage(llm.Text("{}")),
				Output:  json.RawMessage(`{}`),
				Stop:    llm.Stop{Kind: llm.StopFinished, Raw: "end_turn"},
				Usage:   &llm.Usage{InputTokens: 10, OutputTokens: 2},
				Cost: &llm.Cost{
					Input:    money.MustParseAmount("0.001"),
					Total:    money.MustParseAmount("0.002"),
					Currency: "USD",
					Source:   llm.CostSourceProviderReported,
				},
			},
		},
		"resolve/failure": Resolve{
			CommandID: "cmd-failure",
			ModelFailure: &ModelFailure{
				SendStatus: llm.SendStatusNotSent,
				Reason:     "dial failed",
				Evidence:   "connection refused",
			},
		},
		"cancel/empty": Cancel{},
		"cancel/full":  Cancel{CommandID: "cmd-cancel", Reason: "operator"},
		"steer/empty":  Steer{},
		"steer/full": Steer{Item: ContextItem{
			ID:      "s1",
			Origin:  ContextUser,
			Message: llm.UserMessage(llm.Text("use staging")),
		}, Urgent: true},
	}
}

func TestCommandCodecRoundTrip(t *testing.T) {
	for name, c := range codecCommands() {
		t.Run(name, func(t *testing.T) {
			data, err := MarshalCommand(c)
			if err != nil {
				t.Fatal(err)
			}
			kind := strings.Split(name, "/")[0]
			if !strings.HasPrefix(string(data), `{"kind":"`+kind+`","command":`) {
				t.Fatalf("encoding = %s", data)
			}
			got, err := UnmarshalCommand(data)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c) {
				t.Fatalf("round trip changed the command:\n got %#v\nwant %#v", got, c)
			}
			if CommandKindOf(got) != CommandKind(kind) || CommandIDOf(got) != CommandIDOf(c) {
				t.Fatalf("identity = %q %q", CommandKindOf(got), CommandIDOf(got))
			}
			want, err := commandHash(c)
			if err != nil {
				t.Fatal(err)
			}
			if d, _ := commandHash(got); d != want {
				t.Fatalf("hash changed: %s != %s", d, want)
			}
			// A second round trip is byte-stable.
			again, err := MarshalCommand(got)
			if err != nil {
				t.Fatal(err)
			}
			if string(again) != string(data) {
				t.Fatalf("re-encoding differs:\n%s\n%s", again, data)
			}
		})
	}
}

func TestCommandCodecCompactsRawJSON(t *testing.T) {
	c := Start{CommandID: "s", OutputSchema: json.RawMessage(`{ "type" : "object" }`)}
	data, err := MarshalCommand(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalCommand(data)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(got.(Start).OutputSchema); s != `{"type":"object"}` {
		t.Fatalf("schema = %s", s)
	}
	a, _ := commandHash(c)
	b, _ := commandHash(got)
	if a != b {
		t.Fatal("compacting raw JSON changed the hash")
	}
}

func TestCommandJSONUsesSnakeCase(t *testing.T) {
	for name, c := range codecCommands() {
		if !strings.HasSuffix(name, "/empty") {
			continue
		}
		data, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		// Only the always-present Agent survives an empty command.
		if s := string(data); s != "{}" && !strings.HasPrefix(s, `{"agent":{`) {
			t.Errorf("%s: empty command encodes as %s", name, s)
		}
	}
	data, _ := json.Marshal(codecCommands()["start/full"])
	for _, key := range []string{`"command_id"`, `"output_schema"`, `"parent"`, `"compaction"`, `"history"`, `"input"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("start encoding lacks %s", key)
		}
	}
}

func TestCommandHashIgnoresOnHistoryMismatch(t *testing.T) {
	a, _ := commandHash(Continue{CommandID: "c"})
	b, _ := commandHash(Continue{CommandID: "c", OnHistoryMismatch: HistoryMismatchReject})
	if a != b {
		t.Fatal("OnHistoryMismatch changed the hash")
	}
}

func TestUnmarshalCommandRejectsBadInput(t *testing.T) {
	for _, data := range []string{
		``,
		`null`,
		`{}`,
		`{"kind":"start"}`,
		`{"kind":"start","command":null}`,
		`{"kind":"pause","command":{}}`,
		`{"kind":"start","command":{"agent":7}}`,
		`{"kind":"cancel","command":[]}`,
	} {
		if c, err := UnmarshalCommand([]byte(data)); err == nil {
			t.Errorf("UnmarshalCommand(%q) = %#v, want error", data, c)
		}
	}
	if _, err := MarshalCommand(nil); err == nil {
		t.Error("MarshalCommand(nil) succeeded")
	}
	if _, err := MarshalCommand(&Start{}); err == nil {
		t.Error("MarshalCommand(*Start) succeeded")
	}
	if CommandIDOf(nil) != "" || CommandKindOf(nil) != "" {
		t.Error("nil command has an identity")
	}
}

func TestCommandIdentityOfPointers(t *testing.T) {
	for _, c := range []Command{
		(*Start)(nil), (*AddMessages)(nil), (*Continue)(nil), (*Resolve)(nil), (*Cancel)(nil),
		&Start{CommandID: "s"}, &Cancel{CommandID: "c"},
	} {
		if id, kind := CommandIDOf(c), CommandKindOf(c); id != "" || kind != "" {
			t.Errorf("%T: identity = %q %q, want none", c, id, kind)
		}
		if _, err := MarshalCommand(c); err == nil {
			t.Errorf("MarshalCommand(%T) succeeded", c)
		}
	}
}

func TestNormalizeCommandDropsNullSchemas(t *testing.T) {
	c := normalizeCommand(Start{
		CommandID:    "s",
		OutputSchema: json.RawMessage(" null "),
		Agent:        Agent{OutputSchema: json.RawMessage("null")},
	}).(Start)
	if c.OutputSchema != nil || c.Agent.OutputSchema != nil {
		t.Fatalf("schemas = %q, %q", c.OutputSchema, c.Agent.OutputSchema)
	}
	if err := (Agent{OutputSchema: json.RawMessage("null")}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// WithRules records the rules that informed an approval decision. The
// record and a reload keep them, and no model request holds them, for an
// approval or a denial; a denial's result is its reason alone.
func TestWithRulesIsRecordedAndHiddenFromModel(t *testing.T) {
	const version, finding = "decide/jev-1.13.0", "decide: severe 91% flagged (jev-1.13.0, request r_12)"
	for _, approve := range []bool{true, false} {
		client := &scriptedClient{results: []llm.Result{callResult("call-1", "write"), final("done")}}
		rec := NewMemoryRecorder()
		binding := tool.Tool{
			Definition: tool.Definition{Name: "write", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal},
			Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
				return tool.Succeeded("ok"), nil
			}),
		}
		runner := Runner{Client: client, Tools: tool.List{binding}, Recorder: rec, Hooks: deferredHooks()}
		got, err := start(t, client, tool.List{binding}, rec, deferredHooks())
		if err != nil || len(got.Pending) != 1 {
			t.Fatalf("pending=%+v err=%v", got.Pending, err)
		}
		cmd := got.Pending[0].Deny("The operator declined.")
		if approve {
			cmd = got.Pending[0].Approve()
		}
		got, err = runner.Run(t.Context(), got.Turn, cmd.WithRules(version, finding))
		if err != nil {
			t.Fatal(err)
		}
		a := got.Tools["call-1"].Approval
		if a == nil || a.Approve != approve || a.RulesVersion != version || a.RulesFinding != finding {
			t.Fatalf("approve=%t: approval=%+v", approve, a)
		}
		got, err = runner.Run(t.Context(), got.Turn, Continue{CommandID: "continue"})
		if err != nil || got.Stop.Kind != StopCompleted {
			t.Fatalf("approve=%t: stop=%+v err=%v", approve, got.Stop, err)
		}
		raw, err := json.Marshal(got.Turn)
		if err != nil {
			t.Fatal(err)
		}
		var reloaded Turn
		if err := json.Unmarshal(raw, &reloaded); err != nil {
			t.Fatal(err)
		}
		view, err := reloaded.View()
		if err != nil {
			t.Fatal(err)
		}
		if got := view.Tools["call-1"].Approval; !reflect.DeepEqual(got, a) {
			t.Fatalf("approve=%t: reloaded approval=%+v, want %+v", approve, got, a)
		}
		sent, err := json.Marshal(client.inputs)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(sent), "decide") || strings.Contains(string(sent), "91%") {
			t.Fatalf("approve=%t: a model request holds the rules: %s", approve, sent)
		}
		if !approve && !strings.Contains(string(sent), "The operator declined.") {
			t.Fatalf("denial reason did not reach the model: %s", sent)
		}
	}
}

// WithRules leaves a command without an approval decision unchanged, and
// never changes a Resolve the caller kept.
func TestWithRulesCopiesAndSkipsProviderApproval(t *testing.T) {
	provider := Resolve{ProviderToolApproval: &llm.ProviderToolApprovalResponse{RequestID: "r1", Approve: true}}
	if got := provider.WithRules("v1", "found"); !reflect.DeepEqual(got, provider) {
		t.Fatalf("provider approval changed: %+v", got)
	}
	kept := Resolve{CallID: "c1", Approval: &ApprovalDecision{CallID: "c1", Approve: true}}
	got := kept.WithRules("v1", "found")
	if kept.Approval.RulesVersion != "" || kept.Approval.RulesFinding != "" {
		t.Fatalf("kept command changed: %+v", kept.Approval)
	}
	if got.Approval.RulesVersion != "v1" || got.Approval.RulesFinding != "found" {
		t.Fatalf("approval=%+v", got.Approval)
	}
	raw, err := json.Marshal(kept.Approval)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "rules_") {
		t.Fatalf("empty rules encoded: %s", raw)
	}
}

// A retried approval whose check ran again, with other rules, is a
// duplicate: the first recorded rules stay. Other changes still conflict.
func TestRetriedApprovalWithOtherRulesIsDuplicate(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "write")}}
	rec := NewMemoryRecorder()
	binding := tool.Tool{Definition: tool.Definition{Name: "write", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}}
	runner := Runner{Client: client, Tools: tool.List{binding}, Recorder: rec, Hooks: deferredHooks()}
	got, err := start(t, client, tool.List{binding}, rec, deferredHooks())
	if err != nil || len(got.Pending) != 1 {
		t.Fatalf("pending=%+v err=%v", got.Pending, err)
	}
	p := got.Pending[0]
	first, err := runner.Run(t.Context(), got.Turn, p.Approve().WithRules("decide/jev-1", "severe 10% (request r_1)"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := runner.Run(t.Context(), first.Turn, p.Approve().WithRules("decide/jev-1", "severe 11% (request r_2)"))
	if err != nil {
		t.Fatalf("retry with other rules: %v", err)
	}
	if f := again.Tools["call-1"].Approval.RulesFinding; f != "severe 10% (request r_1)" {
		t.Fatalf("finding=%q", f)
	}
	if _, err := runner.Run(t.Context(), first.Turn, p.Deny("no")); !errors.Is(err, ErrCommandIDReused) {
		t.Fatalf("a different decision: err=%v", err)
	}
}

// commandHash leaves an approval's rules out on a copy: the hash matches
// the command without rules, and the caller's decision keeps its rules.
func TestCommandHashBlanksRulesOnCopy(t *testing.T) {
	plain := Resolve{CommandID: "r", Approval: &ApprovalDecision{CallID: "c1", Approve: true}}
	cmd := plain.WithRules("decide/jev-1", "severe 9% (request r_1)")
	want, _ := commandHash(plain)
	got, err := commandHash(cmd)
	if err != nil || got != want {
		t.Fatalf("hash = %s, %v; want %s", got, err, want)
	}
	if cmd.Approval.RulesVersion != "decide/jev-1" || cmd.Approval.RulesFinding != "severe 9% (request r_1)" {
		t.Fatalf("hashing changed the caller's decision: %+v", *cmd.Approval)
	}
}

// A BeforeTool hook's RulesFinding is recorded in the verdict. A denial
// gives the model its Reason, never the finding.
func TestToolDecisionRulesFindingIsHiddenFromModel(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "write"), final("done")}}
	binding := tool.Tool{Definition: tool.Definition{Name: "write", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}}
	hooks := ToolHook{Name: "check", Func: func(context.Context, ToolPlan) (ToolDecision, error) {
		d := Deny("The operator declined.")
		d.RulesVersion, d.RulesFinding = "decide/jev-1", "severe 91% flagged (request r_12)"
		return d, nil
	}}.Hooks()
	got, err := start(t, client, tool.List{binding}, NewMemoryRecorder(), hooks)
	if err != nil || got.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", got.Stop, err)
	}
	v := got.Tools["call-1"].Verdict
	if v.RulesVersion != "decide/jev-1" || v.RulesFinding != "severe 91% flagged (request r_12)" || v.Reason != "The operator declined." {
		t.Fatalf("verdict=%+v", v)
	}
	sent, err := json.Marshal(client.inputs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sent), "91%") || !strings.Contains(string(sent), "The operator declined.") {
		t.Fatalf("model input: %s", sent)
	}
}
