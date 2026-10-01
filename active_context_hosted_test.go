package dashi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/tool"
)

type activeHostedDiagnosticClient struct {
	llm.Client
	wantError, summaryHadEvidence bool
	ordinary, summaries           int
	sent                          []llm.Request
	first                         llm.Content
	firstResultIndex              int
	empty                         bool
	status                        string
}

func (c *activeHostedDiagnosticClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	c.sent = append(c.sent, call.Request)
	in := call.Request.Input()
	var result llm.Result
	if len(in.Tools) == 0 {
		c.summaries++
		body := string(call.Request.Body())
		c.summaryHadEvidence = activeHasDiagnostic(in.Messages) != c.empty && strings.Contains(body, "[provider tool result]") &&
			(c.empty || strings.Contains(body, "https://example.test/evidence")) &&
			strings.Contains(body, "[provider tool error]") == c.wantError
		if c.summaryHadEvidence {
			evidence := c.status
			if !c.empty {
				evidence += " " + activeDiagnostic
			}
			result = dashitest.Reply("<summary>Hosted evidence retained. " + evidence + "</summary>")
		} else {
			result = dashitest.Reply("<summary>Required hosted evidence was omitted.</summary>")
		}
	} else if c.ordinary == 0 {
		// Native decoding sees the original hosted item. The HTTP
		// transport is a local fixture and cannot make a network request.
		c.ordinary++
		var err error
		result, err = c.Client.Send(ctx, call)
		if err != nil {
			return result, err
		}
		c.first = result.Message.Content[c.firstResultIndex].Clone()
		return result, nil
	} else {
		c.ordinary++
		if c.status != "" {
			body, _ := json.Marshal(in.Messages)
			if (c.empty || activeHasDiagnostic(in.Messages)) && strings.Contains(string(body), c.status) {
				result = dashitest.Reply("Checked hosted result. " + c.status)
			} else {
				result = dashitest.Reply("Missing hosted result status.")
			}
		} else if activeHasDiagnostic(in.Messages) {
			result = dashitest.Reply("Fixed and verified. " + activeDiagnostic)
		} else {
			result = dashitest.Reply("Missing hosted diagnostic.")
		}
	}
	result.Requested = llm.ModelRef{Provider: c.Provider(), Model: in.Model}
	result.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 2}
	return result, nil
}

func TestActiveCompactionNativeSeparateHostedStatus(t *testing.T) {
	for _, tc := range []struct {
		name, status   string
		isError, empty bool
	}{
		{"succeeded", "Remote check succeeded.", false, false},
		{"failed", "Remote check failed.", true, false},
		{"empty failed", "Remote check failed.", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Output is neutral diagnostic evidence. Only the explicit native
			// is_error field tells the summary whether the remote check failed.
			output := `[]`
			if !tc.empty {
				output = fmt.Sprintf(`[{"type":"text","text":%q},{"type":"resource","uri":"https://example.test/evidence"}]`, activeDiagnostic+strings.Repeat(" diagnostic detail", 350))
			}
			transport := &wire{replies: []string{fmt.Sprintf(`{
				"id":"msg_hosted","type":"message","role":"assistant","model":"claude-sonnet-4-5",
				"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":2},"content":[
					{"type":"mcp_tool_use","id":"hosted-1","name":"check","server_name":"fixture","input":{}},
					{"type":"mcp_tool_result","tool_use_id":"hosted-1","is_error":%t,"content":%s},
					{"type":"tool_use","id":"read-1","name":"read","input":{}}
				]}`, tc.isError, output)}}
			native, err := messages.New(messages.Config{APIKey: "test", Client: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			c := &activeHostedDiagnosticClient{Client: native, wantError: tc.isError, empty: tc.empty, status: tc.status, firstResultIndex: 1}
			effects := 0
			read := tool.MustFunc("read", "Read workspace evidence", func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
				effects++
				return tool.Succeeded(strings.Repeat("workspace evidence ", 500)), nil
			})
			out, err := (dashi.Runner{Client: c, Tools: tool.List{read}}).Run(t.Context(), dashi.Turn{}, dashi.Start{
				Agent: dashi.Agent{Model: "claude-sonnet-4-5", Settings: llm.Settings{MaxTokens: 256}, ActiveCompaction: &dashi.ActiveCompactionPolicy{Window: 16000, Fraction: .5, MaxTokens: 256}},
				Input: []llm.Message{llm.UserMessage(llm.Text("Check the remote result and report its status."))},
			})
			if err != nil || out.Stop.Kind != dashi.StopCompleted || !c.summaryHadEvidence || out.Answer != "Checked hosted result. "+tc.status {
				t.Fatalf("stop=%+v summary evidence=%v answer=%q err=%v", out.Stop, c.summaryHadEvidence, out.Answer, err)
			}
			if effects != 1 || c.summaries != 1 || len(c.sent) != 3 || len(out.ContextCompactions) != 1 || out.ModelAttempts != 3 || len(transport.requests(t)) != 1 {
				t.Fatalf("effects=%d summaries=%d sends=%d applications=%d attempts=%d", effects, c.summaries, len(c.sent), len(out.ContextCompactions), out.ModelAttempts)
			}
			if out.Spend.Usage.InputTokens != 300 || out.Spend.Usage.OutputTokens != 6 {
				t.Fatalf("accounting=%+v", out.Spend)
			}
			if c.first.HostedResult == nil || c.first.HostedResult.IsError != tc.isError || c.first.Replay == nil || (len(c.first.HostedResult.Output) == 0) != tc.empty {
				t.Fatalf("native separate item=%+v", c.first)
			}
			original, _ := json.Marshal(c.first)
			raw, err := dashi.Project(out.Turn)
			if err != nil {
				t.Fatal(err)
			}
			rawJSON, _ := json.Marshal(raw)
			if !bytes.Contains(rawJSON, original) {
				t.Fatal("summary rendering changed recorded result status or native replay")
			}
			encoded, _ := json.Marshal(out.Turn)
			var loaded dashi.Turn
			if err := json.Unmarshal(encoded, &loaded); err != nil {
				t.Fatal(err)
			}
			view, err := dashi.ProjectContext(loaded)
			viewJSON, _ := json.Marshal(view)
			if err != nil || !strings.Contains(string(viewJSON), tc.status) || activeHasDiagnostic(view) == tc.empty {
				t.Fatalf("reloaded context lost hosted status or diagnostic: %v", err)
			}
		})
	}
}

func TestActiveCompactionNativeCombinedHostedDiagnostic(t *testing.T) {
	for _, isError := range []bool{false, true} {
		status := "completed"
		if isError {
			status = "failed"
		}
		t.Run(status, func(t *testing.T) {
			output := activeDiagnostic + strings.Repeat(" hosted evidence", 600)
			transport := &wire{replies: []string{fmt.Sprintf(`{
				"id":"resp_hosted", "object":"response", "model":"gpt-6-luna", "status":"completed",
				"usage":{"input_tokens":100,"output_tokens":2}, "output":[
					{"type":"mcp_call","id":"hosted-1","name":"diagnose","server_label":"fixture",
					 "arguments":"{}","status":%q,"output":%q,
					 "results":[{"url":"https://example.test/evidence","title":"Diagnostic source"}]},
					{"type":"function_call","id":"fc-1","call_id":"read-1","name":"read","arguments":"{}"}
				]}`, status, output)}}
			native, err := responses.New(responses.Config{APIKey: "test", Client: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			c := &activeHostedDiagnosticClient{Client: native, wantError: isError}
			effects := 0
			read := tool.MustFunc("read", "Read workspace evidence", func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
				effects++
				return tool.Succeeded(strings.Repeat("workspace evidence ", 220)), nil
			})
			out, err := (dashi.Runner{Client: c, Tools: tool.List{read}}).Run(t.Context(), dashi.Turn{}, dashi.Start{
				Agent: dashi.Agent{Model: "gpt-6-luna", Settings: llm.Settings{MaxTokens: 256}, ActiveCompaction: &dashi.ActiveCompactionPolicy{Window: 16000, MaxTokens: 256}},
				Input: []llm.Message{llm.UserMessage(llm.Text("Investigate and fix the failure from hosted diagnostics."))},
			})
			if err != nil || out.Stop.Kind != dashi.StopCompleted || !c.summaryHadEvidence || out.Answer != "Fixed and verified. "+activeDiagnostic {
				t.Fatalf("stop=%+v summary evidence=%v answer=%q err=%v", out.Stop, c.summaryHadEvidence, out.Answer, err)
			}
			if effects != 1 || c.summaries != 1 || len(c.sent) != 3 || len(out.ContextCompactions) != 1 || out.ModelAttempts != 3 || len(transport.requests(t)) != 1 {
				t.Fatalf("effects=%d summaries=%d sends=%d applications=%d attempts=%d", effects, c.summaries, len(c.sent), len(out.ContextCompactions), out.ModelAttempts)
			}
			if out.Spend.Usage.InputTokens != 300 || out.Spend.Usage.OutputTokens != 6 {
				t.Fatalf("accounting=%+v", out.Spend)
			}
			activeDocumentWarnings(t, out, 0)
			if c.first.HostedUse == nil || c.first.HostedUse.IsError != isError || c.first.Replay == nil {
				t.Fatalf("native combined item=%+v", c.first)
			}
			original, _ := json.Marshal(c.first)
			raw, err := dashi.Project(out.Turn)
			if err != nil {
				t.Fatal(err)
			}
			rawJSON, _ := json.Marshal(raw)
			if !bytes.Contains(rawJSON, original) {
				t.Fatal("summary rendering changed the recorded hosted item or native replay")
			}
			encoded, _ := json.Marshal(out.Turn)
			var loaded dashi.Turn
			if err := json.Unmarshal(encoded, &loaded); err != nil {
				t.Fatal(err)
			}
			view, err := dashi.ProjectContext(loaded)
			if err != nil || !activeHasDiagnostic(view) {
				t.Fatalf("reloaded context lost hosted diagnostic: %v", err)
			}
		})
	}
}
