package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/tool"
)

// This file is a stand-in for the coding tools package. It gives the loop two
// read-only tools with the same names and a small approval hook, so the CLI
// runs before that package merges. Delete this file when the real adapter
// lands.

type readFileArgs struct {
	Path string `json:"path" description:"File to read, relative to the workspace."`
}

type listDirArgs struct {
	Path string `json:"path,omitempty" description:"Directory to list. Defaults to the workspace root."`
}

func standInWiring(env Env) (wiring, error) {
	read, err := tool.Func(
		"read_file",
		"Read a text file from the workspace.",
		func(_ context.Context, _ tool.Call, in readFileArgs) (tool.Outcome, error) {
			p, err := standInPath(env.Dir, in.Path)
			if err != nil {
				return tool.Failed(err.Error()), nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return tool.Failed(err.Error()), nil
			}
			return tool.Succeeded(string(data)), nil
		},
	)
	if err != nil {
		return wiring{}, err
	}
	list, err := tool.Func(
		"list_directory",
		"List the entries of a workspace directory.",
		func(_ context.Context, _ tool.Call, in listDirArgs) (tool.Outcome, error) {
			p, err := standInPath(env.Dir, in.Path)
			if err != nil {
				return tool.Failed(err.Error()), nil
			}
			entries, err := os.ReadDir(p)
			if err != nil {
				return tool.Failed(err.Error()), nil
			}
			var names []string
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() {
					name += "/"
				}
				names = append(names, name)
			}
			return tool.Succeeded(strings.Join(names, "\n")), nil
		},
	)
	if err != nil {
		return wiring{}, err
	}
	yes := true
	for _, t := range []*tool.Tool{&read, &list} {
		t.Definition.Annotations.ReadOnly = &yes
	}
	return wiring{
		Tools: tool.List{read, list},
		Hooks: dashi.ToolHook{
			Name: "standin-approval",
			Func: standInPolicy(env),
		}.Hooks(),
	}, nil
}

// standInPolicy allows read-only tools and asks about the rest.
func standInPolicy(env Env) func(
	context.Context,
	dashi.ToolPlan,
) (dashi.ToolDecision, error) {
	return func(
		ctx context.Context,
		plan dashi.ToolPlan,
	) (dashi.ToolDecision, error) {
		if ro := plan.Definition.Annotations.ReadOnly; ro != nil && *ro {
			return dashi.Allow("read-only tool"), nil
		}
		if env.Approve == nil {
			return dashi.Deny("No one can approve this call."), nil
		}
		a, err := env.Approve(ctx, ApprovalRequest{
			Tool:    plan.Call.Name,
			Summary: plan.Call.Name + " " + mainArg(plan.Call.Name, plan.Call.Args),
			Dir:     env.Dir,
			Args:    plan.Call.Args,
		})
		if err != nil {
			return dashi.ToolDecision{}, err
		}
		if !a.Allow {
			return dashi.Deny(orDefault(a.Reason, declinedReason)), nil
		}
		return dashi.Allow("approved by the user"), nil
	}
}

// declinedReason is what the model reads when the user declines a call.
const declinedReason = "The user declined this call. Do not retry the same " +
	"call. Ask the user what to do, or try a different approach."

func standInPath(dir, p string) (string, error) {
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p = filepath.Clean(p)
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace", p)
	}
	return p, nil
}
