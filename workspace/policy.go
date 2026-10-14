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

	// Preview is a unified diff for write_file and edit_file, against the
	// file as it is when the Policy asks. An earlier call of the same batch
	// may still change the file before this call runs. Preview is empty when
	// the tool cannot compute one, such as when the edit's text is not in the
	// file yet.
	Preview string

	// Dir is the working directory, for bash.
	Dir string

	// Command is the exact command text, for bash.
	Command string

	// Args holds the exact arguments of the call.
	Args json.RawMessage

	// Arguments is printable argument text, populated by Printable.
	// It is for display only and does not identify or authorize a call.
	Arguments string

	// RememberScope describes which later calls a remembered allow covers.
	// Its lifetime is the Policy's lifetime; it does not persist a grant.
	RememberScope string
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

// PolicyOption changes how Workspace.Policy builds a Policy.
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

// AskConcurrently lets the Policy call ask for several tool calls at once
// instead of one at a time. Use it when ask does slow work, such as a network
// check, before it shows a prompt. ask must then be safe to call from several
// goroutines, and must show one prompt at a time itself. Calls that one
// remembered answer would cover still wait for each other, so a remembered
// allow applies to the calls behind it.
func AskConcurrently() PolicyOption {
	return func(p *Policy) {
		p.concurrent = true
	}
}

// Policy decides which tool calls need approval. It plugs into Runner.Hooks.
// A Policy is safe for concurrent use. By default it asks one question at a
// time, so a prompt from one call holds up the other calls of the same
// batch; AskConcurrently changes that.
type Policy struct {
	ws         *Workspace
	ask        ApprovalFunc
	trusted    []string
	concurrent bool

	one sync.Mutex // held while asking, unless concurrent

	mu     sync.Mutex // guards rules and asking
	rules  map[string]bool
	asking map[string]chan struct{} // closed when the ask for a rule key ends
}

// Policy returns the approval Policy for this Workspace. It asks through ask.
// A nil ask makes the Policy defer every call that needs approval, for the
// durable approval flow: the turn waits, and the host answers later with the
// pending item.
//
// The Policy trusts the read-only tools that this Workspace built. It
// recognizes them by their full definition, so a tool from another package
// that has the same name, such as an MCP tool, still asks. It reads files in
// the Workspace directory to build previews.
func (w *Workspace) Policy(ask ApprovalFunc, opts ...PolicyOption) *Policy {
	p := &Policy{ws: w, ask: ask, rules: map[string]bool{}, asking: map[string]chan struct{}{}}
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
	owned, readOnly := p.ws.lookup(plan.Definition)
	if owned && readOnly {
		return dashi.Allow("read-only tool"), nil
	}
	if owned && editCannotSucceed(plan) {
		// The tool fails without a change, and its error text tells the
		// model what to fix. A prompt would ask the person to approve a
		// call that cannot do anything.
		return dashi.Allow("the call cannot change a file"), nil
	}
	if p.ask == nil {
		return dashi.NeedsApproval("The call needs approval."), nil
	}
	if !p.concurrent {
		p.one.Lock()
		defer p.one.Unlock()
	}
	key := ruleKey(owned, plan.Call.Name, plan.Call.Args)
	remembered, err := p.claim(ctx, key)
	if err != nil {
		return dashi.ToolDecision{}, err
	}
	if remembered {
		return dashi.Allow("allowed by a remembered answer"), nil
	}
	a, err := p.ask(ctx, p.request(plan, owned))
	p.mu.Lock()
	if err == nil && a.Allow && a.Remember {
		p.rules[key] = true
	}
	close(p.asking[key])
	delete(p.asking, key)
	p.mu.Unlock()
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
	return dashi.Allow("approved by the user"), nil
}

// claim waits until no other call asks under key. It reports true when a
// remembered answer covers key. Otherwise the caller now asks under key and
// must close and delete p.asking[key] when the ask ends.
func (p *Policy) claim(ctx context.Context, key string) (remembered bool, err error) {
	for {
		p.mu.Lock()
		if p.rules[key] {
			p.mu.Unlock()
			return true, nil
		}
		done, busy := p.asking[key]
		if !busy {
			p.asking[key] = make(chan struct{})
			p.mu.Unlock()
			return false, nil
		}
		p.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// ruleKey names what a remembered answer covers: the whole tool for the file
// tools, and the exact call for everything else. A rule for a command prefix
// would be unsafe, because "go test ./... && rm -rf ~" starts with "go test".
// A tool of another package never shares a rule with this package's tool of
// the same name.
func ruleKey(owned bool, name string, args json.RawMessage) string {
	if !owned {
		return "external:" + name + ":" + string(args)
	}
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

func (p *Policy) request(plan dashi.ToolPlan, owned bool) ApprovalRequest {
	r := ApprovalRequest{
		Tool:          plan.Call.Name,
		Summary:       plan.Call.Name,
		Args:          plan.Call.Args,
		RememberScope: "This tool with these exact arguments",
	}
	if !owned {
		return r
	}
	switch plan.Call.Name {
	case ToolWriteFile:
		r.RememberScope = "All write_file calls in this workspace"
		var in writeArgs
		if json.Unmarshal(plan.Call.Args, &in) == nil {
			r.Summary = "write " + in.Path
			r.Preview = p.writePreview(in)
		}
	case ToolEditFile:
		r.RememberScope = "All edit_file calls in this workspace"
		var in editArgs
		if json.Unmarshal(plan.Call.Args, &in) == nil {
			r.Summary = "edit " + in.Path
			r.Preview = p.editPreview(in)
		}
	case ToolBash:
		r.RememberScope = "This exact Bash command in this workspace"
		var in bashArgs
		if json.Unmarshal(plan.Call.Args, &in) == nil {
			r.Command, r.Dir = in.Command, p.ws.Dir()
			first, _, multi := strings.Cut(in.Command, "\n")
			if multi {
				first += " ..."
			}
			r.Summary = "run: " + first
		}
	}
	return r
}

// readOld reads the current content of path for a preview. It uses the same
// scope rules as the tools, so a path outside the workspace gives no preview
// and leaks no file content. A file that is not there reads as empty.
// ok is false when the content is unknown, and then there is no preview.
func (p *Policy) readOld(path string) (old string, ok bool) {
	resolved, err := p.ws.scope.resolve(path)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(resolved)
	switch {
	case err == nil:
		return string(data), true
	case os.IsNotExist(err):
		return "", true
	}
	return "", false
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

// editCannotSucceed reports whether plan is an edit_file call that the tool
// rejects whatever the file holds: old_string is empty or equals new_string.
// It does not read the file. An earlier call of the same batch may still be
// running or waiting for approval, and it may create the text an edit looks
// for, so a check against the file now could let a later edit run unasked.
func editCannotSucceed(plan dashi.ToolPlan) bool {
	if plan.Call.Name != ToolEditFile {
		return false
	}
	var in editArgs
	if json.Unmarshal(plan.Call.Args, &in) != nil {
		return false
	}
	return in.OldString == "" || in.OldString == in.NewString
}

func ptrTrue(p *bool) bool {
	return p != nil && *p
}
