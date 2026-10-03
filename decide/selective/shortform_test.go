package selective

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestDefaultShortForm(t *testing.T) {
	log := bigLog(60)
	segment := []llm.Message{
		call("c1", "read_log"),
		result("c1", log),
		llm.AssistantMessage(llm.Text("here is the chart"),
			llm.Image(content.Source{URL: "https://example.com/chart.png"})),
	}
	short, ok := DefaultShortForm(segment)
	if !ok {
		t.Fatal("no short form")
	}
	if short[0].Content[0].ToolCall.ID != "c1" {
		t.Errorf("call ID lost: %+v", short[0])
	}
	r := short[1].Content[0].ToolResult
	if r == nil || r.CallID != "c1" {
		t.Fatalf("result lost its call ID: %+v", short[1])
	}
	text := r.Output.Content[0].Text.Text
	lines := strings.Split(log, "\n")
	if !strings.HasPrefix(text, lines[0]+"\n") || !strings.HasSuffix(text, lines[len(lines)-2]+"\n") {
		t.Errorf("short result does not keep the first and last lines:\n%s", text)
	}
	if !strings.Contains(text, "cut in the short form") || len(text) >= len(log) {
		t.Errorf("short result has no note or is not shorter:\n%s", text)
	}
	if got := short[2].Content[1].Block.Text; got == nil || !strings.Contains(got.Text, "image omitted") {
		t.Errorf("image not turned into a note: %+v", short[2].Content[1])
	}
	if segment[1].Content[0].ToolResult.Output.Content[0].Text.Text != log {
		t.Error("the short form changed its input")
	}
}

func TestDefaultShortFormNothingToCut(t *testing.T) {
	if _, ok := DefaultShortForm([]llm.Message{call("c1", "x"), result("c1", "small")}); ok {
		t.Fatal("a small segment has a short form")
	}
}

func TestCutTextOneLongLine(t *testing.T) {
	s := strings.Repeat("é", 2000)
	got, ok := cutText(s)
	if !ok || len(got) >= len(s) || !strings.Contains(got, "cut in the short form") {
		t.Fatalf("cut = %q, %v", got, ok)
	}
	if !strings.HasPrefix(got, "é") || !strings.HasSuffix(got, "é") {
		t.Fatalf("cut split a character: %q", got[:8])
	}
}
