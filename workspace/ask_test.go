package workspace

import (
	"context"
	"errors"
	"reflect"
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

func TestAskUserMultipleAnswers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer Answer
		want   string
	}{
		{"choices", Answer{Selections: []string{"a,b", "line\nbreak"}}, `{"selections":["a,b","line\nbreak"]}`},
		{"own words", Answer{Text: "my answer"}, `{"text":"my answer"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got Question
			list, _ := newTestTools(t, WithAsker(func(_ context.Context, q Question) (Answer, error) {
				got = q
				return tc.answer, nil
			}))
			out, text := call(t, list, ToolAskUser, map[string]any{
				"question": "Which?", "multiple": true,
				"choices": []Choice{{Label: "a,b"}, {Label: "line\nbreak"}},
			})
			wantState(t, out, text, tool.StateSucceeded)
			if text != tc.want || !got.Multiple || !reflect.DeepEqual(got.Choices, []Choice{{Label: "a,b"}, {Label: "line\nbreak"}}) {
				t.Fatalf("text=%q question=%+v", text, got)
			}
		})
	}
}

func TestAskUserRejectsInvalidMultipleAnswers(t *testing.T) {
	for _, answer := range []Answer{
		{}, {Text: " "}, {Text: "own", Selections: []string{"a"}},
		{Text: " ", Selections: []string{"a"}},
		{Selections: []string{"a", "a"}}, {Selections: []string{"not offered"}},
	} {
		list, _ := newTestTools(t, WithAsker(func(context.Context, Question) (Answer, error) { return answer, nil }))
		out, text := call(t, list, ToolAskUser, map[string]any{
			"question": "Which?", "multiple": true, "choices": []Choice{{Label: "a"}},
		})
		wantState(t, out, text, tool.StateFailed)
	}
}

func TestAskUserRejectsInvalidMultipleChoicesBeforeAsking(t *testing.T) {
	for _, choices := range [][]Choice{{{Label: "same"}, {Label: "same"}}, {{Label: " "}}} {
		list, _ := newTestTools(t, WithAsker(func(context.Context, Question) (Answer, error) {
			t.Fatal("invalid choices reached the person")
			return Answer{}, nil
		}))
		out, text := call(t, list, ToolAskUser, map[string]any{"question": "Which?", "multiple": true, "choices": choices})
		wantState(t, out, text, tool.StateFailed)
	}
}

func TestAskUserCancellationRemainsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	list, _ := newTestTools(t, WithAsker(func(context.Context, Question) (Answer, error) {
		cancel()
		return Answer{}, context.Canceled
	}))
	for _, tl := range list {
		if tl.Definition.Name == ToolAskUser {
			_, err := tl.Executor.Execute(ctx, tool.Call{ID: "call", Name: ToolAskUser, Args: []byte(`{"question":"Which?","multiple":true}`)})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation=%v", err)
			}
			return
		}
	}
	t.Fatal("ask_user missing")
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
