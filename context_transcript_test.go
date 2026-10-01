package dashi

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestActiveTranscriptDocumentsAndNestedOutputs(t *testing.T) {
	const fact = "diagnostic only in text document"
	document := content.DocumentBlock(content.Source{Data: []byte(fact), MIMEType: "text/plain"}, "diagnostic.txt")
	for _, part := range []llm.Content{
		llm.Block(document),
		{Kind: llm.ContentToolResult, ToolResult: &llm.ToolResult{Output: tool.Output{Content: []content.Block{document}}}},
		{Kind: llm.ContentProviderToolUse, ProviderToolUse: &llm.ProviderToolUse{Output: []content.Block{document}}},
		{Kind: llm.ContentProviderToolResult, ProviderToolResult: &llm.ProviderToolResult{Output: []content.Block{document}}},
	} {
		t.Run(string(part.Kind), func(t *testing.T) {
			messages := []llm.Message{llm.UserMessage(part, llm.Document(content.Source{Data: []byte("%PDF-1.4"), MIMEType: "application/pdf"}, "binary.pdf"))}
			before, _ := json.Marshal(messages)
			got, err := (&runtime{}).activeTranscript(messages)
			text := strings.Join(got, "\n")
			if err != nil || !strings.Contains(text, "[text document: diagnostic.txt]\n"+fact) || !strings.Contains(text, "[document omitted: binary.pdf]") {
				t.Fatalf("transcript=%s err=%v", text, err)
			}
			after, _ := json.Marshal(messages)
			if !bytes.Equal(before, after) {
				t.Fatal("transcript rendering changed source content")
			}
			if strings.Contains(strings.Join(compactiontext.Transcript(messages), "\n"), fact) {
				t.Fatal("active-only rendering changed between-Turn transcript behavior")
			}
		})
	}
}

func TestActiveTranscriptReferencesRequireVerifiedBytes(t *testing.T) {
	data := []byte("verified diagnostic")
	ref := refFor("diagnostic", data)
	for _, mode := range []string{"verified", "missing", "wrong hash", "wrong size"} {
		t.Run(mode, func(t *testing.T) {
			declared := ref
			candidate := llm.Input{Messages: []llm.Message{llm.UserMessage(llm.Document(content.Source{Data: data, MIMEType: "text/plain"}, "diagnostic.txt"))}}
			if mode == "wrong hash" {
				candidate.Messages[0].Content[0].Block.Document.Source.Data = []byte("different diagnostic")
			}
			if mode == "wrong size" {
				declared.Size++
			}
			resolver := &countingResolver{files: map[string][]byte{ref.ID: data}}
			r := runtime{runner: Runner{ContentLoader: resolver}, compactionCandidate: &candidate}
			if mode == "missing" {
				r.compactionCandidate = nil
			}
			messages := []llm.Message{llm.UserMessage(llm.Document(content.Source{Ref: &declared, MIMEType: "text/plain"}, "diagnostic.txt"))}
			before, _ := json.Marshal(messages)
			got, err := r.activeTranscript(messages)
			if mode == "verified" {
				if err != nil || !strings.Contains(strings.Join(got, "\n"), string(data)) {
					t.Fatalf("verified evidence=%v err=%v", got, err)
				}
			} else if err == nil {
				t.Fatal("transcript accepted unverified reference bytes")
			}
			after, _ := json.Marshal(messages)
			if resolver.count() != 0 || !bytes.Equal(before, after) {
				t.Fatal("rendering performed source I/O or changed reference identity")
			}
		})
	}
}

func TestActiveTranscriptExplicitResultStatus(t *testing.T) {
	for _, shape := range []string{"local", "combined provider tool", "separate provider tool"} {
		for _, isError := range []bool{false, true} {
			for _, empty := range []bool{false, true} {
				blocks := []content.Block{content.TextBlock("diagnostic mentions failure; the recorded flag governs status")}
				if empty {
					blocks = nil
				}
				var part llm.Content
				label := "[provider tool error]"
				switch shape {
				case "local":
					part = llm.Content{Kind: llm.ContentToolResult, ToolResult: &llm.ToolResult{CallID: "check", IsError: isError, Output: tool.Output{Content: blocks}}}
					label = "[tool error id=check]"
				case "combined provider tool":
					part = llm.Content{Kind: llm.ContentProviderToolUse, ProviderToolUse: &llm.ProviderToolUse{ID: "check", Name: "check", IsError: isError, Output: blocks}}
				case "separate provider tool":
					part = llm.Content{Kind: llm.ContentProviderToolResult, ProviderToolResult: &llm.ProviderToolResult{CallID: "check", IsError: isError, Output: blocks}}
				}
				messages := []llm.Message{llm.AssistantMessage(part)}
				before, _ := json.Marshal(messages)
				got, err := (&runtime{}).activeTranscript(messages)
				if err != nil || strings.Contains(strings.Join(got, "\n"), label) != isError {
					t.Fatalf("shape=%s error=%v empty=%v transcript=%v err=%v", shape, isError, empty, got, err)
				}
				after, _ := json.Marshal(messages)
				if !bytes.Equal(before, after) {
					t.Fatal("active status rendering changed the recorded result")
				}
				if shape != "local" && strings.Contains(strings.Join(compactiontext.Transcript(messages), "\n"), label) {
					t.Fatal("active status rendering changed the between-Turn renderer")
				}
			}
		}
	}
}
