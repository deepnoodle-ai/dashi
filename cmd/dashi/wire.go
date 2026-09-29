package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/tool"
)

// wiring is what one library feature adds to the runner. A feature package
// exports plain tools and hooks and knows nothing about this program; an
// adapter in this file turns it into a wiring. Wiring a new feature is one
// added adapter.
type wiring struct {
	Tools tool.Toolset
	Hooks dashi.Hooks
	// Instructions is appended to the system prompt.
	Instructions string
	// Close releases what the feature holds. It may be nil.
	Close func() error
}

// Env is what an adapter needs to build its wiring.
type Env struct {
	// Dir is the workspace directory.
	Dir string
	// Approve asks the user about a tool call.
	Approve ApprovalFunc
	// Ask asks the user a question in the model's name.
	Ask AskFunc
}

// adapters lists the features that plug into the runner. Until the coding
// tools package merges, a stand-in supplies the workspace tools.
var adapters = []func(Env) (wiring, error){
	standInWiring,
}

// ApprovalRequest describes a call that needs approval. The fields match the
// coding tools package, so the adapter for that package converts field by
// field.
type ApprovalRequest struct {
	// Tool is the tool name.
	Tool string
	// Summary is one line, such as "edit main.go".
	Summary string
	// Preview is a unified diff for file edits, or empty.
	Preview string
	// Dir is the working directory, for shell commands.
	Dir string
	// Command is the exact command text, for shell commands.
	Command string
	// Args holds the exact arguments of the call.
	Args json.RawMessage
}

// Approval is the user's answer to an ApprovalRequest.
type Approval struct {
	Allow bool
	// Remember lets later matching calls run without a prompt, for the life
	// of the process.
	Remember bool
	// Reason tells the model why the call was declined.
	Reason string
}

// ApprovalFunc asks the user whether a call may run. It must honor ctx.
type ApprovalFunc func(ctx context.Context, r ApprovalRequest) (Approval, error)

// Question is what the model asks the user.
type Question struct {
	Prompt  string
	Choices []string
}

// AskFunc asks the user a question and returns the answer text.
type AskFunc func(ctx context.Context, q Question) (string, error)

// buildWiring runs every adapter and joins the results. Close releases every
// feature, in reverse order.
func buildWiring(env Env) (wiring, error) {
	var (
		sets    []tool.Toolset
		hooks   []dashi.Hooks
		out     wiring
		closers []func() error
	)
	for _, build := range adapters {
		w, err := build(env)
		if err != nil {
			return wiring{}, err
		}
		if w.Tools != nil {
			sets = append(sets, w.Tools)
		}
		hooks = append(hooks, w.Hooks)
		if w.Instructions != "" {
			if out.Instructions != "" {
				out.Instructions += "\n\n"
			}
			out.Instructions += w.Instructions
		}
		if w.Close != nil {
			closers = append(closers, w.Close)
		}
	}
	out.Tools = tool.Join(sets...)
	out.Hooks = dashi.JoinHooks(hooks...)
	out.Close = func() error {
		var errs []error
		for i := len(closers) - 1; i >= 0; i-- {
			errs = append(errs, closers[i]())
		}
		return errors.Join(errs...)
	}
	return out, nil
}
