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
			Approval:  &ApprovalDecision{CallID: "c1", Approve: false, ArgsHash: ArgsHash(call.Args), Reason: "no"},
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
