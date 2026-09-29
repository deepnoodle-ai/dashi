package workspace

import (
	"context"
	"errors"
	"testing"

	"github.com/deepnoodle-ai/dashi/tool"
)

func TestAskUserPassesQuestionAndReturnsAnswer(t *testing.T) {
	var got Question
	ask := func(_ context.Context, q Question) (Answer, error) {
		got = q
		return Answer{Text: "blue"}, nil
	}
	list, _ := newTestTools(t, WithAsker(ask))
	out, text := call(t, list, ToolAskUser, map[string]any{
		"question": "Which color?",
		"choices": []map[string]any{
			{"label": "blue", "description": "cool"},
			{"label": "red"},
		},
	})
	wantState(t, out, text, tool.StateSucceeded)
	if text != "blue" {
		t.Fatalf("text = %q", text)
	}
	if got.Prompt != "Which color?" || len(got.Choices) != 2 ||
		got.Choices[0].Description != "cool" {
		t.Fatalf("question = %+v", got)
	}
}

func TestAskUserErrorGoesToModel(t *testing.T) {
	ask := func(context.Context, Question) (Answer, error) {
		return Answer{}, errors.New("no terminal")
	}
	list, _ := newTestTools(t, WithAsker(ask))
	out, text := call(t, list, ToolAskUser, map[string]any{"question": "Why?"})
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "no terminal")
}
