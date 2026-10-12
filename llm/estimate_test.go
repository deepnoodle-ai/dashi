package llm

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/tool"
)

// estimateRequest prepares messages with a body that encodes them, as a
// provider adapter would, so inline media appears in the body as base64.
func estimateRequest(t *testing.T, window, override int, messages ...Message) Request {
	t.Helper()
	input := Input{Model: "fixture", Messages: messages}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var info *ModelInfo
	if window > 0 {
		info = &ModelInfo{
			Capabilities: Capabilities{ContextWindow: window},
			Evidence: ModelEvidence{
				Model:       "fixture",
				API:         "test.chat",
				Endpoint:    "https://example.test",
				Owner:       "tests",
				Source:      "fixture",
				Method:      EvidenceFixture,
				QualifiedAt: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC),
			},
		}
	}
	request, err := NewRequest(RequestSnapshot{
		Origin: content.Origin{
			Provider: "test", API: "test.chat", Model: "fixture", Scope: "fixture",
		},
		Input:                input,
		Body:                 body,
		ModelInfo:            info,
		CapabilityPolicy:     CapabilityAdjust,
		EstimatedInputTokens: override,
	})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestEstimatedInputTokensCountsTextFourBytesPerToken(t *testing.T) {
	request := estimateRequest(t, 0, 0, UserMessage(Text(strings.Repeat("a", 4000))))
	got, size := EstimatedInputTokens(request), len(request.Body())
	if got != (size+3)/4 {
		t.Fatalf("estimate=%d for %d body bytes", got, size)
	}
	if InputTokenUpperBound(request) != size+1024 {
		t.Fatalf("upper bound=%d, want body plus 1024", InputTokenUpperBound(request))
	}
}

func TestEstimatedInputTokensCountsImagesPerItem(t *testing.T) {
	// Sixteen ~60 KB screenshots, as in issue 255.
	var parts []Content
	for range 16 {
		parts = append(parts, Image(content.Source{
			Data: bytes.Repeat([]byte{0x89}, 60_000), MIMEType: "image/png",
		}))
	}
	request := estimateRequest(t, 0, 0, UserMessage(parts...))
	got := EstimatedInputTokens(request)
	media := 16 * imageTokens
	if got < media || got > media+600 {
		t.Fatalf("estimate=%d, want about %d for the images", got, media)
	}
	if bound := InputTokenUpperBound(request); bound < 16*60_000 {
		t.Fatalf("upper bound=%d no longer counts every byte", bound)
	}
}

func TestEstimatedInputTokensClampsToContextWindow(t *testing.T) {
	text := UserMessage(Text(strings.Repeat("a", 400_000)))
	if got := EstimatedInputTokens(estimateRequest(t, 50_000, 0, text)); got != 50_000 {
		t.Fatalf("estimate=%d, want the 50000 window", got)
	}
	if got := EstimatedInputTokens(estimateRequest(t, 50_000, 90_000, text)); got != 50_000 {
		t.Fatalf("override estimate=%d, want the 50000 window", got)
	}
}

func TestEstimatedInputTokensPrefersPreparerCount(t *testing.T) {
	request := estimateRequest(t, 0, 777, UserMessage(Text(strings.Repeat("a", 4000))))
	if got := EstimatedInputTokens(request); got != 777 {
		t.Fatalf("estimate=%d, want the preparer's 777", got)
	}
	snapshot := request.Snapshot()
	snapshot.EstimatedInputTokens = -1
	if _, err := NewRequest(snapshot); err == nil {
		t.Fatal("negative estimate accepted")
	}
}

func TestEstimatedMessageTokensCountsEachMediaKind(t *testing.T) {
	pdf := []byte("%PDF-1.4 /Type /Pages /Type /Page x /Type/Page>> %%EOF")
	for name, tc := range map[string]struct {
		block content.Block
		media int
	}{
		"image by url": {
			content.ImageBlock(content.Source{URL: "https://example.test/a.png"}),
			imageTokens,
		},
		"pdf pages": {
			content.DocumentBlock(content.Source{Data: pdf, MIMEType: "application/pdf"}, "a.pdf"),
			2 * documentPageTokens,
		},
		"document without pages": {
			content.DocumentBlock(content.Source{
				Data: bytes.Repeat([]byte{0}, 250_000), MIMEType: "application/octet-stream",
			}, ""),
			3 * documentPageTokens,
		},
		"remote document": {
			content.DocumentBlock(content.Source{URL: "https://example.test/a.pdf"}, ""),
			documentPageTokens,
		},
		"inline audio": {
			content.AudioBlock(content.Source{Data: make([]byte, 1_001), MIMEType: "audio/wav"}),
			3,
		},
		"remote audio": {
			content.AudioBlock(content.Source{URL: "https://example.test/a.wav"}),
			unknownMediaTokens,
		},
		"video with duration": {
			content.Block{Kind: content.KindVideo, Video: &content.Video{
				Source:   content.Source{URL: "https://example.test/a.mp4"},
				Duration: 2500 * time.Millisecond,
			}},
			3 * videoSecondTokens,
		},
	} {
		t.Run(name, func(t *testing.T) {
			tokens, _ := mediaTokens(tc.block)
			if tokens != tc.media {
				t.Fatalf("media tokens=%d, want %d", tokens, tc.media)
			}
		})
	}
}

func TestEstimatedMessageTokensFindsNestedMediaAndKeepsTextDocuments(t *testing.T) {
	image := content.ImageBlock(content.Source{
		Data: bytes.Repeat([]byte{1}, 30_000), MIMEType: "image/png",
	})
	result := ToolResult{CallID: "c1", Output: tool.Output{Content: []content.Block{image}}}
	nested := []Message{{
		Role:    RoleUser,
		Content: []Content{{Kind: ContentToolResult, ToolResult: &result}},
	}}
	if got := EstimatedMessageTokens(nested); got < imageTokens || got > imageTokens+50 {
		t.Fatalf("nested image estimate=%d", got)
	}
	notes := strings.Repeat("note ", 2_000)
	text := []Message{UserMessage(Document(content.Source{
		Data: []byte(notes), MIMEType: "text/plain",
	}, "notes.txt"))}
	// A text document is text: its base64 bytes count four to a token.
	if got := EstimatedMessageTokens(text); got < len(notes)/3 {
		t.Fatalf("text document estimate=%d", got)
	}
	if EstimatedMessageTokens(nil) != 0 {
		t.Fatal("no messages added tokens")
	}
}
