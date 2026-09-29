package contentsrc

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
)

func TestBlockAliasesThePayload(t *testing.T) {
	block := content.DocumentBlock(content.Source{
		MIMEType: "text/plain",
		Ref: &content.Ref{
			ID:     "upl_1",
			SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
			Size:   1,
		},
	}, "a.txt")

	sources := Block(block)
	sources[0].Ref = nil

	if block.Document.Source.Ref != nil {
		t.Fatal("Block did not alias the payload")
	}
	if got := Block(content.TextBlock("x")); len(got) != 0 {
		t.Fatalf("text block sources = %d", len(got))
	}
}
