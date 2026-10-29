package dashi

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// RunResult is what Run returns: the turn's record and what it means now.
//
// A RunResult is always the latest known state, and Run's error says whether
// the command applied. After a rejected command it holds the caller's turn,
// unchanged. After a step write that was not acknowledged (a *CommitError),
// it holds the steps this run knows were saved.
//
// The embedded TurnView gives the stop, reply text, structured output,
// spend, pending work, and per-call state without calling Turn.View.
type RunResult struct {
	// Turn is the record to pass to the next Run.
	Turn Turn `json:"turn"`
	TurnView
	// SessionWarnings holds warnings that no turn records, which a host
	// built on Runner adds: package session reports its own there, and
	// those of BuildHistory. Runner.Run leaves it empty.
	SessionWarnings []Warning `json:"session_warnings,omitempty"`
}

// PendingKind classifies what a stopped turn waits on.
type PendingKind string

const (
	// PendingApproval is a local call a BeforeTool hook said needs
	// approval, or a provider tool approval request. Resolve it with
	// Approve or Deny.
	PendingApproval PendingKind = "approval"
	// PendingTool is a local call that started, waits for an outside
	// result, or may have run with an unknown result; Tool.Status says
	// which. Report its result with Result. Continue also recovers a
	// started call: it reruns a call declared read-only or idempotent and
	// marks any other result_unknown.
	PendingTool PendingKind = "tool"
	// PendingModel is a model call without a recorded result. Continue
	// recovers it, or Resolve supplies its result with a ModelResult or
	// ModelFailure.
	PendingModel PendingKind = "model"
)

// Pending is one piece of work a stopped turn waits on. Its methods build
// the Resolve command for it, so the caller never copies argument hashes or
// IDs by hand. Each builder derives the command ID from the item: a retried
// command is recognized as a duplicate, and a different second command
// fails with ErrCommandIDReused.
//
// Calls that are only requested, allowed, or denied need nothing but
// Continue and are not listed.
type Pending struct {
	Kind PendingKind `json:"kind"`
	// ID is the tool call, provider tool request, or model call ID.
	ID string `json:"id"`
	// ModelCallID is the model call that produced the item.
	ModelCallID string `json:"model_call_id,omitempty"`
	// Name and Args describe what a person approves: the tool name and
	// arguments of a local call or a provider tool request.
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
	// Tool is the call's state, set for local calls.
	Tool *ToolView `json:"tool,omitempty"`
	// ProviderTool is the provider's request, set for provider tool
	// approvals.
	ProviderTool *llm.ProviderToolApprovalRequest `json:"provider_tool,omitempty"`
}

// Approve builds the Resolve command that approves this call with exactly
// the arguments shown. Resolve only records the decision; Continue runs the
// call. To record the check that informed the decision, chain
// Resolve.WithRules.
func (p Pending) Approve() Resolve {
	return p.decide(true, "")
}

// Deny builds the Resolve command that denies this call. The next Continue
// gives the reason to the model as the call's result, so the model knows why
// the call did not run; an empty reason gives "The call was denied."
func (p Pending) Deny(reason string) Resolve {
	return p.decide(false, reason)
}

// Result builds the Resolve command that reports a final outcome for a
// local call that is waiting, started, or has an unknown result.
func (p Pending) Result(outcome tool.Outcome) Resolve {
	// Call IDs are unique within a turn; see answer.
	return Resolve{
		CommandID: "result:" + p.ID,
		CallID:    p.ID,
		Outcome:   &outcome,
	}
}

// HandleID returns the ID a detached call returned with tool.Detached, such
// as a job or message ID from an outside service, or "" when the call has no
// handle.
func (p Pending) HandleID() string {
	if p.Tool == nil || p.Tool.Outcome == nil || p.Tool.Outcome.Handle == nil {
		return ""
	}
	return p.Tool.Outcome.Handle.ID
}

func (p Pending) decide(approve bool, reason string) Resolve {
	if p.ProviderTool != nil {
		// A provider may reuse a request ID after the earlier request was
		// resolved, so the model call ID scopes the command ID to this
		// request.
		return Resolve{
			CommandID: "provider-tool-approval:" + p.ModelCallID + ":" + p.ID,
			CallID:    p.ID,
			ProviderToolApproval: &llm.ProviderToolApprovalResponse{
				RequestID: p.ID,
				Approve:   approve,
				Reason:    reason,
				Origin:    p.ProviderTool.Origin,
			},
		}
	}
	fact := &ApprovalDecision{
		CallID:  p.ID,
		Approve: approve,
		Reason:  reason,
	}
	// View rejects a repeated tool call ID anywhere in a turn, so the
	// call ID alone identifies this decision. An item that is not an
	// approval gets an empty hash, which Run rejects as not pending.
	if p.Tool != nil {
		fact.ArgsHash = p.Tool.ArgsHash
	}
	return Resolve{
		CommandID: "approval:" + p.ID,
		CallID:    p.ID,
		Approval:  fact,
	}
}

// restingOutcome is the RunResult for a command that did not apply: the
// caller's turn and its view. A turn whose view cannot be built has no view.
func unchangedResult(turn Turn) RunResult {
	s, err := replay(turn)
	if err != nil {
		return RunResult{Turn: turn.Clone()}
	}
	return outcome(turn, s)
}

func outcome(t Turn, s *replayState) RunResult {
	view := snapshotView(s.TurnView)
	view.Stop = s.inferredStop()
	return RunResult{Turn: cloneTurn(t), TurnView: view}
}

// notReady explains why Continue cannot run the turn.
func notReady(s *replayState) error {
	var ids []string
	for _, p := range s.Pending {
		ids = append(ids, p.ID)
	}
	if len(ids) > 0 {
		return fmt.Errorf("%w: waiting on %s", ErrNotReady, strings.Join(ids, ", "))
	}
	stop := s.inferredStop()
	if stop.Reason != "" {
		return fmt.Errorf("%w: turn %s: %s", ErrNotReady, stop.Kind, stop.Reason)
	}
	return fmt.Errorf("%w: turn %s", ErrNotReady, cmp.Or(string(stop.Kind), "not started"))
}

// commitError wraps a recorder failure for the step it rejected. A
// recorder's own CommitError is copied rather than mutated, and its message
// is not repeated.
func commitError(step Step, err error) *CommitError {
	out := &CommitError{Step: &step, Err: err}
	var ce *CommitError
	if errors.As(err, &ce) {
		out.NothingWritten = ce.NothingWritten
		if e, ok := err.(*CommitError); ok {
			out.Err = e.Err
		}
	}
	return out
}

// clone returns a deep copy of p.
func (p Pending) clone() Pending {
	p.Args = slices.Clone(p.Args)
	if p.Tool != nil {
		p.Tool = new(p.Tool.clone())
	}
	if p.ProviderTool != nil {
		p.ProviderTool = new(p.ProviderTool.Clone())
	}
	return p
}
