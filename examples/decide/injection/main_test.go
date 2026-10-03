package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/decide/decidetest"
)

func TestScreenWithholdsPoisonedPage(t *testing.T) {
	srv := decidetest.NewServer(t)
	srv.Respond(standIn)
	model := script()
	out, err := research(context.Background(), model, dashitest.Model, srv.Client(t), io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	// What the model saw of each call.
	clean := out.Tools["call-1"].Result().Output.Content[0].Text.Text
	if clean != pages[cleanURL] {
		t.Errorf("clean page changed: %q", clean)
	}
	poisoned := out.Tools["call-2"].Result().Output.Content[0].Text.Text
	if strings.Contains(poisoned, "collector.example") || !strings.Contains(poisoned, "withheld") {
		t.Errorf("poisoned page reached the model: %q", poisoned)
	}
	// The record keeps the raw page.
	if raw := out.Tools["call-2"].Outcome.Output.Content[0].Text.Text; raw != pages[poisonedURL] {
		t.Errorf("record lost the raw page: %q", raw)
	}
	// And the final model request never held the attacker's text.
	inputs := model.Inputs()
	results := 0
	for _, m := range inputs[len(inputs)-1].Messages {
		for _, c := range m.Content {
			if c.ToolResult == nil {
				continue
			}
			results++
			if strings.Contains(c.ToolResult.Output.Content[0].Text.Text, "collector.example") {
				t.Errorf("model request held the poisoned page")
			}
		}
	}
	if results != 2 {
		t.Errorf("last model request held %d tool results, want 2", results)
	}
}
