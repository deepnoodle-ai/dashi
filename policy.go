package dashi

import (
	"context"
	"encoding/json"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// ModelPlan gives policy the proposed model input and current turn state.
type ModelPlan struct {
	// Input is the proposed request before policy changes.
	Input llm.Input
	// Turn is the committed state projected before the next model call.
	Turn TurnView
}

// ModelDecision changes the next model input or stops before preparation.
type ModelDecision struct {
	// Settings replaces the input settings when non-nil.
	Settings *llm.Settings
	// Deliver records additional context before preparing the model request.
	Deliver []ContextItem
	// Stop ends this invocation before a model call when non-nil.
	Stop *Stop
}

// ModelAuthorization carries the host's last spending decision for a prepared
// call. Admission is persisted with the intent for atomic reservation.
type ModelAuthorization struct {
	// Allow permits dispatch of the prepared request.
	Allow bool
	// Reason is recorded when the host denies dispatch.
	Reason string
	// Limit describes a host spending denial that can resume after funding.
	Limit *LimitDetail
	// Admission is host-owned, non-secret JSON copied into the model intent.
	// The recorder can atomically reserve spending against these exact terms.
	Admission json.RawMessage
}

// SendPlan gives BeforeSend the immutable prepared request, the turn's spend,
// and the cost of allowing this call. A nil AtRisk is not a zero-cost call.
type SendPlan struct {
	// Request is the immutable provider request after preparation.
	Request llm.Request
	// Turn is a snapshot of committed spend and state before admission.
	Turn TurnView
	// AtRisk is nil when the call cannot be bounded by available prices.
	AtRisk *llm.Cost
	// Remaining is nil when the turn has no cost cap.
	Remaining *Money
}

// DecisionKind identifies a tool authorization result.
type DecisionKind string

const (
	// DecisionAllow permits the tool call.
	DecisionAllow DecisionKind = "allow"
	// DecisionDeny records a denied tool call.
	DecisionDeny DecisionKind = "deny"
	// DecisionDefer leaves authorization to a later host decision.
	DecisionDefer DecisionKind = "defer"
)

// ToolDecision records the policy revision and any approved argument rewrite.
type ToolDecision struct {
	Kind DecisionKind `json:"kind"`
	// Revision binds a later approval to this policy decision.
	Revision string `json:"revision,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Rewrite replaces the requested arguments before the call is authorized.
	Rewrite json.RawMessage `json:"rewrite,omitempty"`
}

// StopDecision can continue a stopped turn with additional context.
type StopDecision struct {
	Continue bool
	Deliver  []ContextItem
	// AcceptEmpty allows a continuation with no additional context.
	AcceptEmpty bool
}

// Policy has no mutable hook bag. A changed tool argument must be authorized
// again. BeforeSend sees the final prepared request but cannot mutate it.
type Policy interface {
	// BeforeModel may change the next input or stop before request preparation.
	BeforeModel(context.Context, ModelPlan) (ModelDecision, error)
	// BeforeSend authorizes the immutable prepared request and its spend risk.
	BeforeSend(context.Context, SendPlan) (ModelAuthorization, error)
	// AuthorizeTool decides whether a model-requested call may execute.
	AuthorizeTool(context.Context, tool.Call, TurnView) (ToolDecision, error)
	// OnStop may continue after a model stop with additional context.
	OnStop(context.Context, TurnView) (StopDecision, error)
}

// AllowLocalPolicy explicitly permits bound local and external tools. Hosts
// with approval or spending rules should supply their own policy.
type AllowLocalPolicy struct{}

func (AllowLocalPolicy) BeforeModel(
	context.Context,
	ModelPlan,
) (ModelDecision, error) {
	return ModelDecision{}, nil
}

func (AllowLocalPolicy) BeforeSend(
	context.Context,
	SendPlan,
) (ModelAuthorization, error) {
	return ModelAuthorization{Allow: true}, nil
}

func (AllowLocalPolicy) AuthorizeTool(
	context.Context,
	tool.Call,
	TurnView,
) (ToolDecision, error) {
	return ToolDecision{Kind: DecisionAllow, Revision: "allow-local-v1"}, nil
}

func (AllowLocalPolicy) OnStop(
	context.Context,
	TurnView,
) (StopDecision, error) {
	return StopDecision{}, nil
}

// Event carries a committed step or provisional model delta from Runner.Stream.
type Event struct {
	TurnID    string
	AttemptID string
	MessageID string
	Step      *Step
	Delta     *llm.Delta
	// Outcome is set only on the final Runner.Stream event. Observer receives
	// committed steps and provisional deltas, not this terminal event.
	Outcome *Outcome
}

// Observer receives progress while a run is in progress.
type Observer func(Event)
