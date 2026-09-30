package main

import (
	"context"
	"errors"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/dashi/workspace"
)

// wiring is what one library feature adds to the runner. A feature package
// exports plain tools and hooks and knows nothing about this program; an
// adapter in this file turns it into a wiring. Wiring a new feature is one
// added adapter.
type wiring struct {
	Tools tool.Toolset
	Hooks dashi.Hooks
	// Scopes describes registered tool sources and their effective authority.
	Scopes map[string]string
	// Instructions is appended to the system prompt.
	Instructions string
	// Close releases what the feature holds. It may be nil.
	Close func() error
}

// Env is what an adapter needs to build its wiring.
type Env struct {
	// Dir is the workspace directory.
	Dir       string
	Resources *resourceBindingManager
	// Approve asks the user about a tool call.
	Approve ApprovalFunc
	// Ask asks the user a question in the model's name.
	Ask AskFunc
}

// adapters lists the features that plug into the runner. To wire a feature,
// add one adapter here.
var adapters = []func(Env) (wiring, error){
	resourceWiring,
	workspaceWiring,
}

// The approval and question types come from the coding tools package, so a
// host has one set of types for every feature that asks the user.
type (
	// ApprovalRequest describes a call that needs approval. Its text fields
	// hold what the model controls; show only Printable().
	ApprovalRequest = workspace.ApprovalRequest
	// Approval is the user's answer to an ApprovalRequest.
	Approval = workspace.Approval
	// ApprovalFunc asks the user whether a call may run. It must honor ctx.
	ApprovalFunc = workspace.ApprovalFunc
	// Question is what the model asks the user.
	Question = workspace.Question
	// AskFunc asks the user a question in the model's name.
	AskFunc = workspace.AskFunc
)

// declinedReason is what the model reads when the user declines a call.
const declinedReason = "The user declined this call. Do not retry the same " +
	"call. Ask the user what to do, or try a different approach."

// workspaceWiring adds the coding tools and their approval policy.
func workspaceWiring(env Env) (wiring, error) {
	ws, err := workspace.New(env.Dir)
	if err != nil {
		return wiring{}, err
	}
	var opts []workspace.Option
	if env.Ask != nil {
		opts = append(opts, workspace.WithAsker(env.Ask))
	}
	tools := ws.Tools(opts...)
	entries, err := tools.Tools(context.Background())
	if err != nil {
		return wiring{}, err
	}
	scopes := make(map[string]string, len(entries))
	for _, entry := range entries {
		scope := "workspace " + ws.Dir() + "; read-only, host trusted"
		switch entry.Definition.Name {
		case workspace.ToolWriteFile, workspace.ToolEditFile:
			scope = "workspace " + ws.Dir() + "; approval required; remembered grants cover this owned tool"
		case workspace.ToolBash:
			scope = "shell in " + ws.Dir() + "; approval required; remembers exact command; shell is not filesystem-confined"
		case workspace.ToolAskUser:
			scope = "workspace question; asks the user; no file authority"
		}
		scopes[entry.Definition.Name] = scope
	}
	return wiring{Tools: tools, Hooks: ws.Policy(env.Approve, resourcePolicyOptions(env.Resources)...).Hooks(), Scopes: scopes}, nil
}

// buildWiring runs every adapter and joins the results. Close releases every
// feature, in reverse order.
func buildWiring(env Env) (wiring, error) {
	var (
		sets    []tool.Toolset
		hooks   []dashi.HookSet
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
		if out.Scopes == nil {
			out.Scopes = map[string]string{}
		}
		for name, scope := range w.Scopes {
			out.Scopes[name] = scope
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
	if _, err := out.Tools.Tools(context.Background()); err != nil {
		return wiring{}, err
	}
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

func resourceWiring(env Env) (wiring, error) {
	if env.Resources == nil {
		return wiring{}, nil
	}
	tools, err := env.Resources.tools()
	if err != nil {
		return wiring{}, err
	}
	return wiring{Tools: tools, Hooks: env.Resources.hooks(), Scopes: map[string]string{
		"load_skill":      "CLI model-eligible catalog; host-owned loader; no supporting-file grant",
		"read_skill_file": "selected canonical skill folders; approval required; remembers exact canonical arguments for this run",
	}}, nil
}
func resourcePolicyOptions(resources *resourceBindingManager) []workspace.PolicyOption {
	if resources == nil {
		return nil
	}
	return []workspace.PolicyOption{workspace.TrustTools("load_skill")}
}
