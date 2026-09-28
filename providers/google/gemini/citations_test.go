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

// Media reference images may omit their MIME type; it is detected on a copy.
func TestMediaInlineImageDetectsMIMEType(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	part, err := (&MediaClient{}).inlineImage(content.Source{Data: png})
	if err != nil {
		t.Fatal(err)
	}
	inline := part.(map[string]any)["inlineData"].(map[string]any)
	if inline["mimeType"] != "image/png" {
		t.Fatalf("mimeType = %v", inline["mimeType"])
	}
}
