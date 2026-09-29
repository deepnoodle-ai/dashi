package mention_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content/mention"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string][]byte{
		"a.go":  []byte("package a\n"),
		"a.png": append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...),
		"a.pdf": []byte("%PDF-1.4 fixture"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	return root
}

// prepareInput builds the model input a front end sends for the expansion.
func prepareInput(t *testing.T, model string) llm.Input {
	t.Helper()
	res := mention.Expand(
		"explain @a.go and a.png and @a.pdf",
		mention.Options{Root: fixtureRoot(t)},
	)
	if len(res.Blocks) != 3 || len(res.Warnings) != 0 {
		t.Fatalf("blocks = %d, warnings = %v", len(res.Blocks), res.Warnings)
	}
	parts := []llm.Content{llm.Text(res.Text)}
	for _, b := range res.Blocks {
		parts = append(parts, llm.Block(b))
	}
	input := llm.Input{
		Model:    model,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: parts}},
	}
	if err := input.Validate(); err != nil {
		t.Fatalf("validate input: %v", err)
	}
	return input
}

func TestExpandOutputPreparesForOpenAI(t *testing.T) {
	client, err := responses.New(responses.Config{
		Provider: "openai",
		APIKey:   "test",
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	req, err := client.Prepare(t.Context(), prepareInput(t, "gpt-6-luna"))

	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	assertAttached(t, req.Body())
}

func TestExpandOutputPreparesForAnthropic(t *testing.T) {
	client, err := messages.New(messages.Config{
		Provider: "anthropic",
		APIKey:   "test",
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	req, err := client.Prepare(
		t.Context(),
		prepareInput(t, "claude-sonnet-4-5"),
	)

	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	assertAttached(t, req.Body())
}

// assertAttached checks that the request body holds the text document, the
// image, and the PDF. The default capability policy fails on a block that the
// model cannot take, so a dropped block cannot pass this test.
func assertAttached(t *testing.T, body []byte) {
	t.Helper()
	text := string(body)
	// OpenAI sends the text file as base64 ("package a\n"); Anthropic sends it
	// as plain text.
	if !strings.Contains(text, "package a") && !strings.Contains(text, "cGFja2FnZSBhCg==") {
		t.Error("request body has no text document")
	}
	for _, want := range []string{"image/png", "application/pdf"} {
		if !strings.Contains(text, want) {
			t.Errorf("request body has no %q", want)
		}
	}
}
