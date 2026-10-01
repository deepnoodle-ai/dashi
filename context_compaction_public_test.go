package dashi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/content/mention"
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
			if err != nil || out.Stop.Kind != dashi.StopCompleted || !c.summaryHadEvidence || out.Reply != "Checked hosted result. "+tc.status {
				t.Fatalf("stop=%+v summary evidence=%v answer=%q err=%v", out.Stop, c.summaryHadEvidence, out.Reply, err)
			}
			if effects != 1 || c.summaries != 1 || len(c.sent) != 3 || len(out.ContextCompactions) != 1 || out.ModelCalls != 3 || len(transport.requests(t)) != 1 {
				t.Fatalf("effects=%d summaries=%d sends=%d applications=%d attempts=%d", effects, c.summaries, len(c.sent), len(out.ContextCompactions), out.ModelCalls)
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
			if err != nil || out.Stop.Kind != dashi.StopCompleted || !c.summaryHadEvidence || out.Reply != "Fixed and verified. "+activeDiagnostic {
				t.Fatalf("stop=%+v summary evidence=%v answer=%q err=%v", out.Stop, c.summaryHadEvidence, out.Reply, err)
			}
			if effects != 1 || c.summaries != 1 || len(c.sent) != 3 || len(out.ContextCompactions) != 1 || out.ModelCalls != 3 || len(transport.requests(t)) != 1 {
				t.Fatalf("effects=%d summaries=%d sends=%d applications=%d attempts=%d", effects, c.summaries, len(c.sent), len(out.ContextCompactions), out.ModelCalls)
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

type stopAfterActiveApplication struct {
	dashi.Recorder
	cancel  context.CancelFunc
	loseAck bool
	fired   bool
}

func (r *stopAfterActiveApplication) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if err := r.Recorder.Commit(ctx, req); err != nil {
		return err
	}
	if !r.fired && req.Step.Kind == dashi.StepContextCompacted {
		r.fired = true
		if r.loseAck {
			return errors.New("lost application acknowledgement")
		}
		r.cancel()
	}
	return nil
}

func TestActiveCompactionContinueAfterApplication(t *testing.T) {
	for _, loseAck := range []bool{false, true} {
		for _, attempts := range []int{3, 5} {
			name := "interrupt"
			if loseAck {
				name = "lost acknowledgement"
			}
			if attempts == 3 {
				name += "/one attempt left"
			} else {
				name += "/spare attempts"
			}
			t.Run(name, func(t *testing.T) {
				effects := 0
				read := tool.MustFunc("read", "Read evidence", func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
					effects++
					return tool.Succeeded(strings.Repeat("evidence ", 1000)), nil
				})
				replies := []llm.Result{
					dashitest.CallTool("read-1", "read", "{}"),
					dashitest.Reply("<summary>Original objective retained. Evidence read finished. " + strings.Repeat("settled evidence ", 300) + "</summary>"),
					dashitest.Reply("Fixed and verified."),
				}
				for i := range replies {
					replies[i].Usage = &llm.Usage{InputTokens: 100, OutputTokens: 2}
				}
				client := activeExampleClient{dashitest.NewClient(replies...)}
				recorder := dashi.NewMemoryRecorder()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				fault := &stopAfterActiveApplication{Recorder: recorder, cancel: cancel, loseAck: loseAck}
				first, err := (dashi.Runner{Client: client, Tools: tool.List{read}, Recorder: fault}).Run(ctx, dashi.Turn{ID: "application-recovery"}, dashi.Start{
					Agent: dashi.Agent{
						Model: dashitest.Model, Settings: llm.Settings{MaxTokens: 256},
						Limits: dashi.Limits{ModelCalls: attempts, NoReservedFinalReply: true},
						// The applied replacement remains above this soft threshold.
						ActiveCompaction: &dashi.ActiveCompactionPolicy{Window: 16000, Fraction: .3, MaxTokens: 256},
					},
					Input: []llm.Message{llm.UserMessage(llm.Text("Investigate, fix, and verify the original objective."))},
				})
				if loseAck {
					if _, ok := errors.AsType[*dashi.CommitError](err); !ok {
						t.Fatalf("expected unknown application commit, got %v", err)
					}
				} else if err != nil || first.Stop.Kind != dashi.StopInterrupted {
					t.Fatalf("interrupt: stop=%+v err=%v", first.Stop, err)
				}
				if !fault.fired || len(client.Inputs()) != 2 || effects != 1 {
					t.Fatalf("application boundary: fired=%v sends=%d effects=%d", fault.fired, len(client.Inputs()), effects)
				}
				loaded, err := recorder.Load(t.Context(), "application-recovery")
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(loaded)
				if err != nil {
					t.Fatal(err)
				}
				var restored dashi.Turn
				if err := json.Unmarshal(data, &restored); err != nil {
					t.Fatal(err)
				}
				view, err := dashi.Replay(restored)
				if err != nil || view.ModelCalls != 2 || len(view.ContextCompactions) != 1 {
					t.Fatalf("reload: attempts=%d summaries=%d err=%v", view.ModelCalls, len(view.ContextCompactions), err)
				}
				input := client.Inputs()[0]
				input.Messages, err = dashi.ProjectContext(restored)
				if err != nil {
					t.Fatal(err)
				}
				req, err := client.Prepare(t.Context(), input)
				if err != nil {
					t.Fatal(err)
				}
				estimate := llm.EstimatedInputTokens(req)
				allowance := 16000 - req.Snapshot().Effective.MaxTokens
				if float64(estimate) < .3*float64(allowance) || estimate > allowance {
					t.Fatalf("replacement must exceed soft threshold and fit hard limit: estimate=%d allowance=%d", estimate, allowance)
				}
				out, err := (dashi.Runner{Client: client, Tools: tool.List{read}, Recorder: recorder}).Run(t.Context(), restored, dashi.Continue{CommandID: "resume"})
				if err != nil || out.Stop.Kind != dashi.StopCompleted || out.Reply != "Fixed and verified." {
					t.Fatalf("resume: stop=%+v answer=%q err=%v", out.Stop, out.Reply, err)
				}
				if effects != 1 || len(client.Inputs()) != 3 || len(out.ContextCompactions) != 1 || out.ModelCalls != 3 {
					t.Fatalf("resume repeated work: effects=%d sends=%d summaries=%d attempts=%d", effects, len(client.Inputs()), len(out.ContextCompactions), out.ModelCalls)
				}
				if out.Spend.Usage.InputTokens != 300 || out.Spend.Usage.OutputTokens != 6 {
					t.Fatalf("resume accounting: %+v", out.Spend)
				}
				summaryIntents := 0
				for _, step := range out.Turn.Steps {
					if step.Kind == dashi.StepModelCallStarted && step.Model.Purpose == dashi.ModelPurposeCompaction {
						summaryIntents++
					}
				}
				if summaryIntents != 1 {
					t.Fatalf("summary dispatches=%d", summaryIntents)
				}
			})
		}
	}
}

const activeDiagnostic = "ROOT_CAUSE: upper bound must be exclusive."

func activeHasDiagnostic(messages []llm.Message) bool {
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Block != nil && part.Block.Text != nil && strings.Contains(part.Block.Text.Text, activeDiagnostic) {
				return true
			}
		}
	}
	return false
}

type activeDiagnosticClient struct {
	activeExampleClient
	document             content.Block
	direct               bool
	ordinary, summaries  int
	summaryHadDiagnostic bool
}

func (c *activeDiagnosticClient) Send(_ context.Context, call llm.Call) (llm.Result, error) {
	in := call.Request.Input()
	var result llm.Result
	if len(in.Tools) == 0 {
		c.summaries++
		c.summaryHadDiagnostic = activeHasDiagnostic(in.Messages)
		body := string(call.Request.Body())
		if c.summaryHadDiagnostic && strings.Contains(body, "[text document: diagnostic.txt]") && !strings.Contains(body, "document omitted") {
			result = dashitest.Reply("<summary>Completed the read. " + activeDiagnostic + "</summary>")
		} else {
			result = dashitest.Reply("<summary>No diagnostic was provided.</summary>")
		}
	} else if c.ordinary == 0 && c.direct {
		result = llm.Result{Message: llm.AssistantMessage(llm.Block(c.document)), Stop: llm.Stop{Kind: llm.StopPause}}
		c.ordinary++
	} else if c.ordinary == 0 || c.direct && c.ordinary == 1 {
		result = dashitest.CallTool("read-1", "read", "{}")
		c.ordinary++
	} else {
		if activeHasDiagnostic(in.Messages) {
			result = dashitest.Reply("Fixed and verified. " + activeDiagnostic)
		} else {
			result = dashitest.Reply("Missing diagnostic.")
		}
		c.ordinary++
	}
	result.Requested = llm.ModelRef{Provider: dashitest.Provider, Model: in.Model}
	return result, nil
}

type activeDiagnosticResolver struct {
	ref   content.Ref
	data  []byte
	calls int
}

func (r *activeDiagnosticResolver) Load(_ context.Context, _ string, ref content.Ref) ([]byte, error) {
	r.calls++
	if ref != r.ref {
		return nil, fmt.Errorf("unexpected reference: %+v", ref)
	}
	return bytes.Clone(r.data), nil
}

func TestActiveCompactionTextDocumentDiagnostic(t *testing.T) {
	for _, mode := range []string{"direct", "tool", "tool reference"} {
		t.Run(mode, func(t *testing.T) {
			data := []byte(activeDiagnostic + strings.Repeat("x", 9000))
			sum := sha256.Sum256(data)
			ref := content.Ref{ID: "diagnostic", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
			source := content.Source{Data: data, MIMEType: "text/plain"}
			resolver := &activeDiagnosticResolver{ref: ref, data: data}
			if mode == "tool reference" {
				source.Data, source.Ref = nil, &ref
			}
			document := content.DocumentBlock(source, "diagnostic.txt")
			original, _ := json.Marshal(document)
			c := &activeDiagnosticClient{activeExampleClient: activeExampleClient{dashitest.NewClient()}, document: document, direct: mode == "direct"}
			effects := 0
			read := tool.MustFunc("read", "Read diagnostic evidence", func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
				effects++
				if c.direct {
					return tool.Succeeded(strings.Repeat("more evidence ", 300)), nil
				}
				blocks := []content.Block{document}
				if mode == "tool reference" {
					// The raw exchange must exceed recent retention too. Its native
					// document bytes enter the candidate only after reference hydration.
					blocks = append(blocks, content.TextBlock(strings.Repeat("other evidence ", 350)))
				}
				return tool.Outcome{State: tool.StateSucceeded, Output: &tool.Output{Content: blocks}}, nil
			})
			out, err := (dashi.Runner{Client: c, Tools: tool.List{read}, ContentLoader: resolver}).Run(t.Context(), dashi.Turn{}, dashi.Start{
				Agent: dashi.Agent{Model: dashitest.Model, Settings: llm.Settings{MaxTokens: 256}, ActiveCompaction: &dashi.ActiveCompactionPolicy{Window: 16000, MaxTokens: 256}},
				Input: []llm.Message{llm.UserMessage(llm.Text("Read the diagnostic and fix the failure."))},
			})
			if err != nil || out.Stop.Kind != dashi.StopCompleted || effects != 1 || len(out.ContextCompactions) != 1 || c.summaries != 1 || !c.summaryHadDiagnostic || out.Reply != "Fixed and verified. "+activeDiagnostic {
				t.Fatalf("stop=%+v effects=%d compactions=%d summaries=%d summary diagnostic=%v answer=%q err=%v", out.Stop, effects, len(out.ContextCompactions), c.summaries, c.summaryHadDiagnostic, out.Reply, err)
			}
			activeDocumentWarnings(t, out, 0)
			wantReads := 0
			if mode == "tool reference" {
				wantReads = 1
			}
			if resolver.calls != wantReads {
				t.Fatalf("resolver reads=%d, want %d", resolver.calls, wantReads)
			}
			after, _ := json.Marshal(document)
			if !bytes.Equal(original, after) {
				t.Fatal("summary rendering changed the original document")
			}
			raw, err := dashi.Project(out.Turn)
			if err != nil {
				t.Fatal(err)
			}
			rawJSON, _ := json.Marshal(raw)
			if !bytes.Contains(rawJSON, original) {
				t.Fatal("summary rendering changed the recorded document or reference")
			}
			encoded, _ := json.Marshal(out.Turn)
			var loaded dashi.Turn
			if err := json.Unmarshal(encoded, &loaded); err != nil {
				t.Fatal(err)
			}
			view, err := dashi.ProjectContext(loaded)
			if err != nil || !activeHasDiagnostic(view) {
				t.Fatalf("reloaded context lost diagnostic: %v", err)
			}
			t.Logf("%s: summary receives diagnostic=true; final context retains diagnostic=true; compactions=1; effects=1; resolver reads=%d", mode, resolver.calls)
		})
	}
}

// Use the native provider's offline preparation and replace only dispatch.
// The request retains its actual text-document encoding and destination.
type activeDocumentClient struct {
	llm.Client
	replies *dashitest.Client
	sent    []llm.Request
}

func (c *activeDocumentClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	c.sent = append(c.sent, call.Request)
	result, err := c.replies.Send(ctx, call)
	result.Requested = llm.ModelRef{Provider: c.Provider(), Model: call.Request.Origin().Model}
	return result, err
}

func activeDocumentProviders(t *testing.T) []struct {
	model  string
	client llm.Client
} {
	t.Helper()
	a, err := messages.New(messages.Config{APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	o, err := responses.New(responses.Config{APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return []struct {
		model  string
		client llm.Client
	}{{"claude-sonnet-4-5", a}, {"gpt-6-luna", o}}
}

func activeDocumentWarnings(t *testing.T, out dashi.RunResult, want int) {
	t.Helper()
	n := 0
	for _, warning := range out.Warnings {
		if warning.Code == dashi.WarningActiveCompactionMediaEstimateUnavailable {
			n++
		}
	}
	if n != want {
		t.Fatalf("media warnings=%d, want %d: %+v", n, want, out.Warnings)
	}
}

func TestActiveCompactionNativeTextDocumentLongRequest(t *testing.T) {
	const document = "Investigate the failing test file"
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	expanded := mention.Expand("Fix and verify the failure described in @notes.txt", mention.Options{Root: root})
	if len(expanded.Blocks) != 1 || len(expanded.Warnings) != 0 || expanded.Blocks[0].Kind != content.KindDocument || expanded.Blocks[0].Document.Source.MIMEType != "text/plain" {
		t.Fatalf("text attachment=%+v", expanded)
	}
	for _, provider := range activeDocumentProviders(t) {
		t.Run(provider.client.API(), func(t *testing.T) {
			c := &activeDocumentClient{Client: provider.client, replies: dashitest.NewClient(
				dashitest.CallTool("read-1", "read", "{}"), dashitest.CallTool("read-2", "read", "{}"),
				dashitest.Reply("<summary>Objective: fix the failure. Reads one and two found the diagnostic.</summary>"),
				dashitest.CallTool("read-3", "read", "{}"), dashitest.CallTool("read-4", "read", "{}"),
				dashitest.Reply("<summary>Objective: fix the failure. Reads one through four finished. Checks passed.</summary>"),
				dashitest.Reply("Fixed and verified."),
			)}
			effects := 0
			read := tool.MustFunc("read", "Read evidence", func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
				effects++
				return tool.Succeeded(strings.Repeat("x", 9000)), nil
			})
			input := []llm.Message{llm.UserMessage(llm.Text(expanded.Text), llm.Block(expanded.Blocks[0]))}
			original, _ := json.Marshal(input)
			out, err := (dashi.Runner{Client: c, Tools: tool.List{read}}).Run(t.Context(), dashi.Turn{}, dashi.Start{
				Agent: dashi.Agent{Model: provider.model, Limits: dashi.Limits{ModelCalls: 12}, Settings: llm.Settings{MaxTokens: 256}, ActiveCompaction: &dashi.ActiveCompactionPolicy{Window: 16000, MaxTokens: 256}},
				Input: input,
			})
			if err != nil || out.Stop.Kind != dashi.StopCompleted || effects != 4 || len(out.ContextCompactions) != 2 || len(c.sent) != 7 || out.Reply != "Fixed and verified." {
				t.Fatalf("stop=%+v effects=%d compactions=%d sends=%d answer=%q err=%v", out.Stop, effects, len(out.ContextCompactions), len(c.sent), out.Reply, err)
			}
			activeDocumentWarnings(t, out, 0)
			for i, req := range c.sent {
				if llm.EstimatedInputTokens(req)+req.Effective().MaxTokens > 16000 {
					t.Fatalf("dispatch %d exceeds text context allowance", i)
				}
			}
			body := string(c.sent[0].Body())
			if !strings.Contains(body, `"type":"document"`) && !strings.Contains(body, `"type":"input_file"`) {
				t.Fatalf("native document missing from prepared body: %s", body)
			}
			after, _ := json.Marshal(input)
			recorded, _ := json.Marshal(out.Turn.Steps[0].Started.Input)
			if !bytes.Equal(original, after) || !bytes.Equal(original, recorded) {
				t.Fatal("text attachment changed caller input or recorded source facts")
			}
			encoded, _ := json.Marshal(out.Turn)
			var loaded dashi.Turn
			if err := json.Unmarshal(encoded, &loaded); err != nil {
				t.Fatal(err)
			}
			view, err := dashi.Replay(loaded)
			if err != nil || len(view.ContextCompactions) != 2 {
				t.Fatalf("reload compactions=%d err=%v", len(view.ContextCompactions), err)
			}
			t.Logf("native %s: text file=%d bytes, effects=4, compactions=2, sends=7, media warnings=0", provider.client.API(), len(document))
		})
	}
}

func TestActiveCompactionNativeDocumentHardLimit(t *testing.T) {
	for _, provider := range activeDocumentProviders(t) {
		for _, mimeType := range []string{"text/plain", "application/pdf"} {
			t.Run(provider.client.API()+"/"+mimeType, func(t *testing.T) {
				c := &activeDocumentClient{Client: provider.client, replies: dashitest.NewClient(dashitest.Reply("Inspected."))}
				out, err := (dashi.Runner{Client: c}).Run(t.Context(), dashi.Turn{}, dashi.Start{
					Agent: dashi.Agent{Model: provider.model, Settings: llm.Settings{MaxTokens: 256}, ActiveCompaction: &dashi.ActiveCompactionPolicy{Window: 4096}},
					Input: []llm.Message{llm.UserMessage(llm.Document(content.Source{Data: []byte(strings.Repeat("x", 9000)), MIMEType: mimeType}, ""))},
				})
				if err != nil {
					t.Fatal(err)
				}
				if mimeType == "text/plain" {
					if out.Stop.Limit == nil || out.Stop.Limit.Kind != dashi.LimitContextWindow || len(c.sent) != 0 {
						t.Fatalf("text guard: stop=%+v sends=%d", out.Stop, len(c.sent))
					}
					activeDocumentWarnings(t, out, 0)
				} else {
					if out.Stop.Kind != dashi.StopCompleted || len(c.sent) != 1 || len(out.ContextCompactions) != 0 {
						t.Fatalf("PDF exception: stop=%+v sends=%d", out.Stop, len(c.sent))
					}
					activeDocumentWarnings(t, out, 1)
				}
			})
		}
	}
}
