package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/workspace"
)

func TestMultipleQuestionLineAnswers(t *testing.T) {
	q := Question{Multiple: true, Choices: []workspace.Choice{{Label: "a,b"}, {Label: "c"}}}
	for _, tc := range []struct {
		input string
		want  questionAnswer
	}{
		{"1,2", questionAnswer{choices: []int{0, 1}}},
		{"2, 1", questionAnswer{choices: []int{1, 0}}},
		{"other: my answer", questionAnswer{text: "my answer"}},
	} {
		got, err := parseQuestionAnswer(q, tc.input)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q: %+v %v", tc.input, got, err)
		}
	}
	for _, invalid := range []string{"", " ", "1,1", "3", "1,", "other:", "own words"} {
		if _, err := parseQuestionAnswer(q, invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

type questionFixtureUI struct {
	*recorderUI
	ask   func(context.Context, Question) (questionAnswer, error)
	shown taskQuestion
}

func (u *questionFixtureUI) Ask(ctx context.Context, q taskQuestion) (questionAnswer, error) {
	u.shown = q
	return u.ask(ctx, q.Question)
}

func TestTaskQuestionNamesTaskWithoutInternalIDs(t *testing.T) {
	ui := &questionFixtureUI{recorderUI: newRecorderUI(), ask: func(context.Context, Question) (questionAnswer, error) {
		return questionAnswer{text: "main"}, nil
	}}
	c := NewController(t.Context(), nil, ui, nil, "")
	owner := brokerOwner{TaskID: "child-session", Call: dashi.CallRef{TurnID: "turn-123", CallID: "call_abc"}}
	ctx := context.WithValue(t.Context(), brokerOwnerKey{}, owner)
	if _, err := c.Ask(ctx, Question{Prompt: "Which branch?"}); err != nil {
		t.Fatal(err)
	}
	if ui.shown.Prompt != "Which branch?" || ui.shown.Task != delegate.TaskID("child-session") {
		t.Fatalf("shown=%+v", ui.shown)
	}
	var out strings.Builder
	line := &lineUI{out: &out, ctrl: c}
	go func() { c.answers <- "main" }()
	if _, err := line.Ask(t.Context(), ui.shown); err != nil {
		t.Fatal(err)
	}
	if want := "Which branch?\n  From task " + ui.shown.Task + " · /task " + ui.shown.Task + " shows it\n"; !strings.HasPrefix(out.String(), want) {
		t.Fatalf("line mode:\n%s", out.String())
	}
}

func TestQuestionAnswersKeepExactLabelsAfterSafeDisplay(t *testing.T) {
	labels := []string{"a\x1b", "a␛"}
	ui := &questionFixtureUI{recorderUI: newRecorderUI(), ask: func(_ context.Context, q Question) (questionAnswer, error) {
		if strings.Contains(q.Prompt, "\x1b") || strings.Contains(q.Choices[0].Label, "\x1b") {
			t.Fatal("raw terminal control reached UI")
		}
		if q.Choices[0].Label != q.Choices[1].Label {
			t.Fatal("fixture needs a display collision")
		}
		return questionAnswer{choices: []int{0, 1}}, nil
	}}
	c := NewController(t.Context(), nil, ui, nil, "")
	got, err := c.Ask(t.Context(), Question{Prompt: "Which?\x1b", Multiple: true, Choices: []workspace.Choice{{Label: labels[0]}, {Label: labels[1]}}})
	if err != nil || !reflect.DeepEqual(got.Selections, labels) {
		t.Fatalf("exact labels lost: %+v %v", got, err)
	}
}
