package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi/tool"
)

// AskFunc shows a question to the person and returns the answer. The host
// supplies it. It must honor ctx.
type AskFunc func(ctx context.Context, q Question) (Answer, error)

// Question is what the model asks the person.
type Question struct {
	Prompt string

	// Choices lists the options. An empty list means a free-text answer.
	Choices []Choice

	// Multiple permits several offered choices or the person's own words.
	// The host returns Selections or Text, never both.
	Multiple bool
}

// Choice is one option in a Question.
type Choice struct {
	Label       string `json:"label" description:"Short name of the option."`
	Description string `json:"description,omitempty" description:"What the option means."`
}

// Answer is the person's reply. For a question with choices, Text is the
// label of the chosen option, or the person's own words.
type Answer struct {
	Text string `json:"text,omitempty"`

	// Selections holds exact choice labels for a multiple-choice question.
	Selections []string `json:"selections,omitempty"`
}

type askArgs struct {
	Question string   `json:"question" description:"The question to ask the person."`
	Choices  []Choice `json:"choices,omitempty" description:"Options to offer. Leave empty for a free-text answer."`
	Multiple bool     `json:"multiple,omitempty" description:"Permit several offered choices. The answer is JSON with selections or the person's own text."`
}

func newAskUser(ask AskFunc) (tool.Tool, error) {
	return closedWorld(tool.Func(
		ToolAskUser,
		"Ask the person a question and wait for the answer. Use it when a "+
			"missing fact blocks the work and you cannot find the fact in "+
			"the workspace. Do not use it to ask permission for a tool call.",
		func(ctx context.Context, _ tool.Call, in askArgs) (tool.Outcome, error) {
			if in.Question == "" {
				return fail("question is empty. Give the question to ask.")
			}
			if in.Multiple {
				seen := map[string]bool{}
				for _, choice := range in.Choices {
					if strings.TrimSpace(choice.Label) == "" || seen[choice.Label] {
						return fail("multiple-choice labels must be nonempty and distinct.")
					}
					seen[choice.Label] = true
				}
			}
			a, err := ask(ctx, Question{Prompt: in.Question, Choices: in.Choices, Multiple: in.Multiple})
			if err != nil {
				if ctx.Err() != nil {
					return tool.Outcome{}, ctx.Err()
				}
				return fail(fmt.Sprintf("the person did not answer: %v. Continue without the answer, or try a different approach.", err))
			}
			if in.Multiple {
				if err := validateSelections(in.Choices, a); err != nil {
					return fail(fmt.Sprintf("the person did not give a valid answer: %v", err))
				}
				encoded, err := json.Marshal(a)
				if err != nil {
					return tool.Outcome{}, err
				}
				return tool.Succeeded(string(encoded)), nil
			}
			if len(a.Selections) != 0 {
				return fail("this question accepts one text answer, not several selections.")
			}
			if a.Text == "" {
				return tool.Succeeded("The person gave an empty answer."), nil
			}
			return tool.Succeeded(a.Text), nil
		},
		tool.ReadOnly(),
	))
}

func validateSelections(choices []Choice, a Answer) error {
	if (strings.TrimSpace(a.Text) != "") == (len(a.Selections) != 0) {
		return fmt.Errorf("give a nonempty selection or your own words, never both")
	}
	if len(a.Selections) == 0 {
		return nil
	}
	if a.Text != "" {
		return fmt.Errorf("selected choices cannot include a text answer")
	}
	offered := make(map[string]bool, len(choices))
	for _, choice := range choices {
		offered[choice.Label] = true
	}
	seen := make(map[string]bool, len(a.Selections))
	for _, label := range a.Selections {
		if !offered[label] || seen[label] {
			return fmt.Errorf("selected labels must be offered choices and cannot repeat")
		}
		seen[label] = true
	}
	return nil
}
