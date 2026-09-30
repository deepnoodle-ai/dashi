package main

import (
	"strings"
	"testing"
)

func TestCopySourceBlocksAndSelection(t *testing.T) {
	source := "Prose\n```go\n日本 := \"" + strings.Repeat("long", 40) + "\"\n```\n~~~sh\necho ok\n~~~"
	blocks := []Block{{Kind: KindUser, Text: "question"}, {Kind: KindAssistant, Text: source}}
	text, notice := copyRequest(blocks, "", "1")
	if notice != "" || text != "日本 := \""+strings.Repeat("long", 40)+"\"" {
		t.Fatalf("wrapped source changed: %q %q", text, notice)
	}
	text, notice = copyRequest(blocks, "", "all")
	if notice != "" || !strings.HasSuffix(text, "\necho ok") || strings.Contains(text, "```") {
		t.Fatalf("all=%q %q", text, notice)
	}
	text, notice = copyRequest(blocks, "selected 日本", "")
	if text != "selected 日本" || notice != "" {
		t.Fatalf("selection=%q %q", text, notice)
	}
	_, notice = copyRequest(blocks, "", "")
	if !strings.Contains(notice, "1.") || !strings.Contains(notice, "2.") {
		t.Fatal(notice)
	}
	blocks = append(blocks, Block{Kind: KindUser, Text: "new request"})
	text, notice = copyRequest(blocks, "", "1")
	if text != "" || notice == "" {
		t.Fatal("copied an answer before the newest request")
	}
}

func TestCopyLiteralNestedAndUnfinishedFences(t *testing.T) {
	blocks := codeBlocks("````md\n```go\nx := 1\n```\n````\n```go\nfunc half(")
	if len(blocks) != 2 || blocks[0].text != "```go\nx := 1\n```" || blocks[1].text != "func half(" {
		t.Fatalf("blocks=%+v", blocks)
	}
	for _, source := range []string{"inline `code`", "    ```go\nnot a fence", "```bad`info\nx"} {
		if len(codeBlocks(source)) != 0 {
			t.Fatalf("invalid fence parsed: %q", source)
		}
	}
}
