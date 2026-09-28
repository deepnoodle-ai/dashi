package gemini

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Gemini cannot request document citations, so the setting is omitted with a
// warning, and inline bytes without a MIME type are detected before encoding.
func TestCitationsOmittedAndMIMETypeDetected(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	input := sampleInput(false)
	input.Settings.Citations = true
	input.Messages = []llm.Message{llm.UserMessage(
		llm.Document(content.Source{Data: []byte("%PDF-1.4")}, "a"),
		llm.Text("summarize"),
	)}
	prepared, err := newTestClient(t, server).Prepare(t.Context(), input)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	warnings := prepared.Warnings()
	if len(warnings) != 1 || warnings[0].Field != llm.SettingCitations {
		t.Fatalf("warnings = %+v", warnings)
	}
	if !strings.Contains(string(prepared.Body()), `"mimeType":"application/pdf"`) {
		t.Fatalf("body = %s", prepared.Body())
	}
}
