package deepinfra

import (
	"context"
	"errors"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Cancellation and invalid input are reported before the document check, as
// every Prepare reports them.
func TestPrepareChecksContextAndInputFirst(t *testing.T) {
	client, err := New(Config{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	document := llm.UserMessage(llm.Content{
		Kind: llm.ContentBlock,
		Block: &content.Block{
			Kind: content.KindDocument,
			Document: &content.Document{Source: content.Source{
				Data:     []byte("%PDF"),
				MIMEType: "application/pdf",
			}},
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	input := llm.Input{Model: "m", Messages: []llm.Message{document}}
	if _, err := client.Prepare(ctx, input); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Prepare error = %v", err)
	}
	input.Model = ""
	_, err = client.Prepare(t.Context(), input)
	if e, ok := errors.AsType[*llm.Error](err); !ok ||
		e.Category != llm.ErrorInvalidRequest {
		t.Fatalf("invalid input error = %v", err)
	}
}
