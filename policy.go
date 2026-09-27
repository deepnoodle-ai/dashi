package kernel

import (
	"context"
	"encoding/json"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

type ModelPlan struct {
	Input llm.Input
	Turn  TurnView
}
type ModelDecision struct {
	Settings *llm.Settings
	Deliver  []ContextItem
	// ModelOnlyReminders apply to this model call only. BeforeModel runs again
	// before the next call; Bindings.ModelOnlyReminders spans the invocation.
	ModelOnlyReminders []llm.Reminder
	Stop               *Stop
}
type ModelAuthorization struct {
	Allow  bool
	Reason string
	// Admission is host-owned, non-secret JSON copied into the model intent.
	// The recorder can atomically reserve spending against these exact terms.
	Admission json.RawMessage
}
type DecisionKind string

const (
	DecisionAllow DecisionKind = "allow"
	DecisionDeny  DecisionKind = "deny"
	DecisionDefer DecisionKind = "defer"
)

type ToolDecision struct {
	Kind     DecisionKind    `json:"kind"`
	Revision string          `json:"revision,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Rewrite  json.RawMessage `json:"rewrite,omitempty"`
}
type StopDecision struct {
	Continue    bool
	Deliver     []ContextItem
	AcceptEmpty bool
}

// Policy has no mutable hook bag. A changed tool argument must be authorized
// again. BeforeSend sees the final prepared request but cannot mutate it.
type Policy interface {
	BeforeModel(context.Context, ModelPlan) (ModelDecision, error)
	BeforeSend(context.Context, llm.Request, TurnView) (ModelAuthorization, error)
	AuthorizeTool(context.Context, tool.Call, TurnView) (ToolDecision, error)
	OnStop(context.Context, TurnView) (StopDecision, error)
}

// AllowLocalPolicy explicitly permits bound local and external tools. Hosts
// with approval or spending rules should supply their own policy.
type AllowLocalPolicy struct{}

func (AllowLocalPolicy) BeforeModel(context.Context, ModelPlan) (ModelDecision, error) {
	return ModelDecision{}, nil
}
func (AllowLocalPolicy) BeforeSend(context.Context, llm.Request, TurnView) (ModelAuthorization, error) {
	return ModelAuthorization{Allow: true}, nil
}
func (AllowLocalPolicy) AuthorizeTool(context.Context, tool.Call, TurnView) (ToolDecision, error) {
	return ToolDecision{Kind: DecisionAllow, Revision: "allow-local-v1"}, nil
}
func (AllowLocalPolicy) OnStop(context.Context, TurnView) (StopDecision, error) {
	return StopDecision{}, nil
}

type Event struct {
	TurnID    string
	AttemptID string
	MessageID string
	Step      *Step
	Delta     *llm.Delta
	// Outcome is set only on the final Engine.Stream event. Observer receives
	// committed steps and provisional deltas, not this terminal event.
	Outcome *Outcome
}
type Observer func(Event)
