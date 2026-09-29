package workspace

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/deepnoodle-ai/dashi"
)

// ApprovalFunc asks the person whether a call may run. The host supplies it.
// It must honor ctx.
type ApprovalFunc func(ctx context.Context, r ApprovalRequest) (Approval, error)

// ApprovalRequest describes a call that needs approval.
type ApprovalRequest struct {
	// Tool is the tool name.
	Tool string

	// Summary is one line, such as "edit main.go" or "run: go test ./...".
	Summary string

	// Preview is a unified diff for write_file and edit_file. It is empty
	// when the tool cannot compute one.
	Preview string

	// Dir is the working directory, for bash.
	Dir string

	// Command is the exact command text, for bash.
	Command string

	// Args holds the exact arguments of the call.
	Args json.RawMessage
}

// Approval is the person's answer.
type Approval struct {
	// Allow lets the call run.
	Allow bool

	// Remember records an allow so that later matching calls run without a
	// prompt while the Policy exists. A refusal is never remembered.
	Remember bool

	// Reason tells the model why the call was refused. An empty reason
	// selects a default.
	Reason string
}

// PolicyOption changes how NewPolicy builds a Policy.
type PolicyOption func(*Policy)

// TrustTools names tools that run without a prompt, in addition to the
// read-only tools of this package. Name a tool only when you trust its
// implementation. The Policy does not trust a ReadOnly annotation, because
// the tool's author sets it.
func TrustTools(names ...string) PolicyOption {
	return func(p *Policy) {
		p.trusted = append(p.trusted, names...)
	}
}

// InDir sets the workspace directory. The Policy reads files there to build
// previews, and reports it as the working directory of shell commands. The
// default is the current directory. Pass the same directory as Tools.
func InDir(dir string) PolicyOption {
	return func(p *Policy) {
		p.dir = dir
	}
}

// Policy decides which tool calls need approval. It plugs into Runner.Hooks.
// A Policy is safe for concurrent use. It asks one question at a time, so a
// prompt from one call holds up the other calls of the same batch.
type Policy struct {
	ask     ApprovalFunc
	trusted []string
	dir     string

	mu    sync.Mutex // held while asking, so prompts do not overlap
	rules map[string]bool
}

// NewPolicy returns a Policy that asks through ask. A nil ask makes the
// Policy defer every call that needs approval, for the durable approval flow:
// the turn waits, and the host answers later with the pending item.
func NewPolicy(ask ApprovalFunc, opts ...PolicyOption) *Policy {
	p := &Policy{ask: ask, dir: ".", rules: map[string]bool{}}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Hooks returns the hook to set as Runner.Hooks, or to join with other hooks
// through dashi.JoinHooks.
func (p *Policy) Hooks() dashi.Hooks {
	return dashi.ToolHook{
		Name: "workspace-approval",
		Func: p.decide,
	}.Hooks()
}

func (p *Policy) decide(
	ctx context.Context,
	plan dashi.ToolPlan,
) (dashi.ToolDecision, error) {
	name := plan.Call.Name
	if slices.Contains(p.trusted, name) {
		return dashi.Allow("trusted tool"), nil
	}
	if slices.Contains(readOnlyTools, name) &&
		ptrTrue(plan.Definition.Annotations.ReadOnly) {
		return dashi.Allow("read-only tool"), nil
	}
	if p.ask == nil {
		return dashi.Defer("The call needs approval."), nil
	}
	key := ruleKey(plan.Call.Name, plan.Call.Args)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rules[key] {
		return dashi.Allow("allowed by a remembered answer"), nil
	}
	a, err := p.ask(ctx, p.request(plan))
	if err != nil {
		return dashi.ToolDecision{}, err
	}
	if !a.Allow {
		reason := a.Reason
		if reason == "" {
			reason = "The user declined this call. Do not retry the same " +
				"call. Ask the user what to do, or try a different approach."
		}
		return dashi.Deny(reason), nil
	}
	if a.Remember {
		p.rules[key] = true
	}
	return dashi.Allow("approved by the user"), nil
}

// ruleKey names what a remembered answer covers: the whole tool for the file
// tools, and the exact call for everything else. A rule for a command prefix
// would be unsafe, because "go test ./... && rm -rf ~" starts with "go test".
func ruleKey(name string, args json.RawMessage) string {
	switch name {
	case ToolWriteFile, ToolEditFile:
		return name
	case ToolBash:
		var in bashArgs
		if json.Unmarshal(args, &in) == nil {
			return "bash:" + in.Command
		}
	}
	return name + ":" + string(args)
}

func (p *Policy) request(plan dashi.ToolPlan) ApprovalRequest {
	r := ApprovalRequest{
		Tool:    plan.Call.Name,
		Summary: plan.Call.Name,
		Args:    plan.Call.Args,
	}
	switch plan.Call.Name {
	case ToolWriteFile:
		var in writeArgs
		if json.Unmarshal(plan.Call.Args, &in) == nil {
			r.Summary = "write " + in.Path
			r.Preview = p.writePreview(in)
		}
	case ToolEditFile:
		var in editArgs
		if json.Unmarshal(plan.Call.Args, &in) == nil {
			r.Summary = "edit " + in.Path
			r.Preview = p.editPreview(in)
		}
	case ToolBash:
		var in bashArgs
		if json.Unmarshal(plan.Call.Args, &in) == nil {
			r.Command, r.Dir = in.Command, p.dir
			first, _, multi := strings.Cut(in.Command, "\n")
			if multi {
				first += " ..."
			}
			r.Summary = "run: " + first
		}
	}
	return r
}

// preview reads use the same scope rules as the tools, so a path outside the
// workspace gives no preview and leaks no file content.
func (p *Policy) readOld(path string) (string, bool) {
	s, err := newScope(p.dir)
	if err != nil {
		return "", false
	}
	resolved, err := s.resolve(path)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", os.IsNotExist(err)
	}
	return string(data), true
}

func (p *Policy) writePreview(in writeArgs) string {
	old, ok := p.readOld(in.Path)
	if !ok {
		return ""
	}
	return unifiedDiff(in.Path, old, in.Content)
}

func (p *Policy) editPreview(in editArgs) string {
	old, ok := p.readOld(in.Path)
	if !ok {
		return ""
	}
	updated, _, err := applyEdit(in.Path, old, in)
	if err != nil {
		return ""
	}
	return unifiedDiff(in.Path, old, updated)
}

func ptrTrue(p *bool) bool {
	return p != nil && *p
}
