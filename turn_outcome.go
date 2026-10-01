package dashi

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Outcome is what Run returns: the turn's record and what it means now.
//
// An Outcome is always the latest known state, and Run's error says whether
// the command applied. After a rejected command it holds the caller's turn,
// unchanged. After a step write that was not acknowledged (a *CommitError),
// it holds the steps this run knows were saved.
//
// The embedded TurnView gives the stop, answer text, structured output,
// spend, pending work, and per-call state without a replay.
type Outcome struct {
	// Turn is the record to pass to the next Run.
	Turn Turn
	TurnView
	// SessionWarnings holds warnings that no turn records, which a host
	// built on Runner adds: package session reports its own there, and
	// those of ProjectHistory. Runner.Run leaves it empty.
	SessionWarnings []Warning
}

// PendingKind classifies what a stopped turn waits on.
type PendingKind string

const (
	// PendingApproval is a local call a BeforeTool hook deferred, or a
	// provider-hosted approval request. Answer it with Approve or Deny.
	PendingApproval PendingKind = "approval"
	// PendingTool is a local call that started, waits for an external
	// result, or has an uncertain effect; Tool.Status says which. Report
	// its result with Result. Continue also recovers a started call: it
	// reruns a call declared read-only or idempotent and marks any other
	// uncertain.
	PendingTool PendingKind = "tool"
	// PendingModel is a model attempt without a recorded result. Continue
	// recovers it, or Resolve reconciles it with a ModelResult or
	// ModelFailure.
	PendingModel PendingKind = "model"
)

// Pending is one piece of work a stopped turn waits on. Its methods build
// the Resolve command that answers it, so the caller never copies argument
// digests or IDs by hand. Each builder derives the command
// ID from the item: a retried answer is recognized as a duplicate, and a
// different second answer fails with ErrCommandConflict.
//
// Calls that are only requested, allowed, or denied need nothing but
// Continue and are not listed.
type Pending struct {
	Kind PendingKind `json:"kind"`
	// ID is the tool call, hosted request, or model attempt ID.
	ID string `json:"id"`
	// ModelCallID is the model attempt that produced the item.
	ModelCallID string `json:"model_call_id,omitempty"`
	// Name and Args describe what a person approves: the tool name and
	// arguments of a local call or a hosted request.
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
	// Tool is the call's state, set for local calls.
	Tool *ToolView `json:"tool,omitempty"`
	// Hosted is the provider's request, set for hosted approvals.
	Hosted *llm.HostedApprovalRequest `json:"hosted,omitempty"`
}

// Approve builds the Resolve command that approves this call with exactly
// the arguments shown. Resolve
// only records the answer; Continue runs the call.
func (p Pending) Approve() Resolve {
	return p.answer(true, "")
}

// Deny builds the Resolve command that denies this call. The next Continue
// gives the reason to the model as the call's result, so the model knows why
// the call did not run; an empty reason gives "The call was denied."
func (p Pending) Deny(reason string) Resolve {
	return p.answer(false, reason)
}

// Result builds the Resolve command that reports a final outcome for a
// waiting, started, or uncertain local call.
func (p Pending) Result(outcome tool.Outcome) Resolve {
	// Call IDs are unique within a turn; see answer.
	return Resolve{
		CommandID: "result:" + p.ID,
		CallID:    p.ID,
		Outcome:   &outcome,
	}
}

func (p Pending) answer(approve bool, reason string) Resolve {
	if p.Hosted != nil {
		// A provider may reuse a request ID after the earlier request was
		// answered, so the attempt scopes the command ID to this request.
		return Resolve{
			CommandID: "hosted-approval:" + p.ModelCallID + ":" + p.ID,
			CallID:    p.ID,
			HostedApproval: &llm.HostedApprovalResponse{
				RequestID: p.ID,
				Approve:   approve,
				Reason:    reason,
				Origin:    p.Hosted.Origin,
			},
		}
	}
	fact := &ApprovalDecision{
		CallID:  p.ID,
		Approve: approve,
		Reason:  reason,
	}
	// Replay rejects a repeated tool call ID anywhere in a turn, so the
	// call ID alone identifies this answer. An item that is not an
	// approval gets an empty digest, which Run rejects as not pending.
	if p.Tool != nil {
		fact.ArgsDigest = p.Tool.ArgsDigest
	}
	return Resolve{
		CommandID: "approval:" + p.ID,
		CallID:    p.ID,
		Approval:  fact,
	}
}

// restingOutcome is the Outcome for a command that did not apply: the
// caller's turn and its view. A turn that fails replay has no view.
func restingOutcome(turn Turn) Outcome {
	s, err := replay(turn)
	if err != nil {
		return Outcome{Turn: turn.Clone()}
	}
	return outcome(turn, s)
}

func outcome(t Turn, s *replayState) Outcome {
	view := snapshotView(s.TurnView)
	view.Stop = s.projectedStop()
	return Outcome{Turn: cloneTurn(t), TurnView: view}
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
	stop := s.projectedStop()
	if stop.Reason != "" {
		return fmt.Errorf("%w: turn %s: %s", ErrNotReady, stop.Kind, stop.Reason)
	}
	return fmt.Errorf("%w: turn %s", ErrNotReady, cmp.Or(string(stop.Kind), "not started"))
}

// commitError wraps a recorder failure for the step it rejected. A
// recorder's own CommitError is copied rather than mutated, and its message
// is not repeated.
func commitError(step Step, err error) *CommitError {
	out := &CommitError{Persistence: PersistenceUnknown, Step: &step, Err: err}
	var ce *CommitError
	if errors.As(err, &ce) {
		out.Persistence = cmp.Or(ce.Persistence, PersistenceUnknown)
		if e, ok := err.(*CommitError); ok {
			out.Err = e.Err
		}
	}
	return out
}
