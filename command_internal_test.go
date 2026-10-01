package dashi

import (
	"encoding/json"
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
				Revision:     "r1",
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
					ModelAttempts: 3,
					Interruptions: -1,
					Deadline:      expires,
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
				Digest:    "d",
				Keep:      history[:1],
				SummaryAt: 1,
				Frame:     "summary:",
			},
		},
		"deliver/empty": Deliver{},
		"deliver/full": Deliver{
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
			ModelAttempts:     9,
			OnHistoryMismatch: HistoryMismatchReject,
		},
		"settle/empty": Settle{},
		"settle/outcome": Settle{
			CommandID: "cmd-settle",
			CallID:    "c1",
			Outcome: &tool.Outcome{
				State:  tool.StateSucceeded,
				Output: &tool.Output{Content: []content.Block{content.TextBlock("ok")}, Display: "ok!"},
				Cost:   &tool.Cost{Amount: money.MustParseAmount("0.01"), Currency: "USD"},
			},
		},
		"settle/approval": Settle{
			CommandID: "cmd-approve",
			CallID:    "c1",
			Approval:  &ApprovalFact{CallID: "c1", Approve: false, ArgsDigest: ArgsDigest(call.Args), Reason: "no"},
		},
		"settle/hosted": Settle{
			CommandID:      "cmd-hosted",
			HostedApproval: &llm.HostedApprovalResponse{RequestID: "r1", Approve: true, Reason: "fine", Origin: origin},
		},
		"settle/result": Settle{
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
		"settle/failure": Settle{
			CommandID: "cmd-failure",
			ModelFailure: &ModelFailure{
				Disposition: llm.FailureNotDispatched,
				Reason:      "dial failed",
				Evidence:    "connection refused",
			},
		},
		"cancel/empty": Cancel{},
		"cancel/full":  Cancel{CommandID: "cmd-cancel", Reason: "operator"},
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
			want, err := commandDigest(c)
			if err != nil {
				t.Fatal(err)
			}
			if d, _ := commandDigest(got); d != want {
				t.Fatalf("digest changed: %s != %s", d, want)
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
	a, _ := commandDigest(c)
	b, _ := commandDigest(got)
	if a != b {
		t.Fatal("compacting raw JSON changed the digest")
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

func TestCommandDigestIgnoresOnHistoryMismatch(t *testing.T) {
	a, _ := commandDigest(Continue{CommandID: "c"})
	b, _ := commandDigest(Continue{CommandID: "c", OnHistoryMismatch: HistoryMismatchReject})
	if a != b {
		t.Fatal("OnHistoryMismatch changed the digest")
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
		(*Start)(nil), (*Deliver)(nil), (*Continue)(nil), (*Settle)(nil), (*Cancel)(nil),
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
