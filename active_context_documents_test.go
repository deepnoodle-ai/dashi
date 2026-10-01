package dashi_test

import (
	"bytes"
	"context"
	"encoding/json"
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

func activeDocumentWarnings(t *testing.T, out dashi.Outcome, want int) {
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
				Agent: dashi.Agent{Model: provider.model, Limits: dashi.Limits{ModelAttempts: 12}, Settings: llm.Settings{MaxTokens: 256}, ActiveCompaction: &dashi.ActiveCompactionPolicy{Window: 16000, MaxTokens: 256}},
				Input: input,
			})
			if err != nil || out.Stop.Kind != dashi.StopCompleted || effects != 4 || len(out.ContextCompactions) != 2 || len(c.sent) != 7 || out.Answer != "Fixed and verified." {
				t.Fatalf("stop=%+v effects=%d compactions=%d sends=%d answer=%q err=%v", out.Stop, effects, len(out.ContextCompactions), len(c.sent), out.Answer, err)
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
