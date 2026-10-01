package main

import (
	"context"
	"github.com/deepnoodle-ai/dashi"
)

// Descriptor fixtures use one saved answer with no execution-report fields.
func finishAutomationContent(o *automationOutput, content string) error {
	o.source(func(context.Context, int) (dashi.TurnView, error) {
		return dashi.TurnView{Answer: content, Origin: dashi.StartUser}, nil
	}, nil)
	if content != "" {
		o.selectTurn(1)
	}
	return o.Finish()
}
func responseContent(result responseMessages) string {
	if len(result.Messages) == 0 {
		return ""
	}
	return result.Messages[0].Content
}
