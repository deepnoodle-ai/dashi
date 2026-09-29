package workspace

import (
	"context"
	"fmt"

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
}

// Choice is one option in a Question.
type Choice struct {
	Label       string `json:"label" description:"Short name of the option."`
	Description string `json:"description,omitempty" description:"What the option means."`
}

// Answer is the person's reply. For a question with choices, Text is the
// label of the chosen option, or the person's own words.
type Answer struct {
	Text string
}

type askArgs struct {
	Question string   `json:"question" description:"The question to ask the person."`
	Choices  []Choice `json:"choices,omitempty" description:"Options to offer. Leave empty for a free-text answer."`
}

func newAskUser(ask AskFunc) (tool.Tool, error) {
	return tool.Func(
		ToolAskUser,
		"Ask the person a question and wait for the answer. Use it when a "+
			"missing fact blocks the work and you cannot find the fact in "+
			"the workspace. Do not use it to ask permission for a tool call.",
		func(ctx context.Context, _ tool.Call, in askArgs) (tool.Outcome, error) {
			if in.Question == "" {
				return fail("question is empty. Give the question to ask.")
			}
			a, err := ask(ctx, Question{Prompt: in.Question, Choices: in.Choices})
			if err != nil {
				if ctx.Err() != nil {
					return tool.Outcome{}, ctx.Err()
				}
				return fail(fmt.Sprintf("the person did not answer: %v. Continue without the answer, or try a different approach.", err))
			}
			if a.Text == "" {
				return tool.Succeeded("The person gave an empty answer."), nil
			}
			return tool.Succeeded(a.Text), nil
		},
		tool.ReadOnly(),
	)
}
