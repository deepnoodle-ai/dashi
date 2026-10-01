package dashi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

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

func (r *activeDiagnosticResolver) Resolve(_ context.Context, _ string, ref content.Ref) ([]byte, error) {
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
			out, err := (dashi.Runner{Client: c, Tools: tool.List{read}, ContentResolver: resolver}).Run(t.Context(), dashi.Turn{}, dashi.Start{
				Agent: dashi.Agent{Model: dashitest.Model, Settings: llm.Settings{MaxTokens: 256}, ActiveCompaction: &dashi.ActiveCompactionPolicy{Window: 16000, MaxTokens: 256}},
				Input: []llm.Message{llm.UserMessage(llm.Text("Read the diagnostic and fix the failure."))},
			})
			if err != nil || out.Stop.Kind != dashi.StopCompleted || effects != 1 || len(out.ContextCompactions) != 1 || c.summaries != 1 || !c.summaryHadDiagnostic || out.Answer != "Fixed and verified. "+activeDiagnostic {
				t.Fatalf("stop=%+v effects=%d compactions=%d summaries=%d summary diagnostic=%v answer=%q err=%v", out.Stop, effects, len(out.ContextCompactions), c.summaries, c.summaryHadDiagnostic, out.Answer, err)
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
