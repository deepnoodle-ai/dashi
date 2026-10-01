package sessionstore

import (
	"math"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

type recordedText struct {
	Role    llm.Role
	Text    string
	initial bool
}

func ordinaryText(turn dashi.Turn) []recordedText {
	var out []recordedText
	if len(turn.Steps) == 0 || turn.Steps[0].Started == nil || turn.Steps[0].Started.Origin == dashi.StartCompaction {
		return out
	}
	purposes := map[string]dashi.ModelPurpose{}
	delivered := map[string]bool{}
	for _, step := range turn.Steps {
		if step.Model != nil && step.Kind == dashi.StepModelCallStarted {
			purposes[step.Model.AttemptID] = step.Model.Purpose
		}
		if step.Started != nil {
			for _, message := range step.Started.Input {
				if message.Context != nil && message.Context.Origin != "" && message.Context.Origin != string(dashi.ContextUser) {
					continue
				}
				if message.Role == llm.RoleUser {
					out = appendText(out, message, true)
				}
			}
		}
		if step.Context != nil {
			for _, item := range step.Context.Items {
				if item.Origin != "" && item.Origin != dashi.ContextUser || item.Message.Role != llm.RoleUser {
					continue
				}
				if item.ID != "" {
					if delivered[item.ID] {
						continue
					}
					delivered[item.ID] = true
				}
				out = appendText(out, item.Message, false)
			}
		}
		if step.Model != nil && (step.Kind == dashi.StepModelCallFinished || step.Kind == dashi.StepModelCallFinishedByHost) && step.Model.Result != nil {
			if purposes[step.Model.AttemptID] == dashi.ModelPurposeCompaction || step.Model.Purpose == dashi.ModelPurposeCompaction {
				continue
			}
			if step.Model.Result.Message.Role == llm.RoleAssistant {
				out = appendText(out, step.Model.Result.Message, false)
			}
		}
	}
	return out
}

func appendText(out []recordedText, message llm.Message, initial bool) []recordedText {
	var text strings.Builder
	for _, part := range message.Content {
		if part.Kind == llm.ContentBlock && part.Block != nil && part.Block.Text != nil {
			text.WriteString(part.Block.Text.Text)
		}
	}
	if text.Len() > 0 {
		out = append(out, recordedText{message.Role, text.String(), initial})
	}
	return out
}

// Accounting preserves Spend facts and separately describes omitted records.
// Incomplete never fabricates unknown-charge or estimated-usage counters.
type Accounting struct {
	Spend                              dashi.Spend
	ModelCalls, UnresolvedModelIntents int
	CountsIncomplete, Incomplete       bool
	OmittedTurns                       int
}

func (a Accounting) add(b Accounting) Accounting {
	a.Spend = a.Spend.Add(b.Spend)
	a.Incomplete = a.Incomplete || b.Incomplete
	a.CountsIncomplete = a.CountsIncomplete || b.CountsIncomplete
	if b.ModelCalls > math.MaxInt-a.ModelCalls {
		a.ModelCalls = math.MaxInt
		a.CountsIncomplete = true
	} else {
		a.ModelCalls += b.ModelCalls
	}
	if b.UnresolvedModelIntents > math.MaxInt-a.UnresolvedModelIntents {
		a.UnresolvedModelIntents = math.MaxInt
		a.CountsIncomplete = true
	} else {
		a.UnresolvedModelIntents += b.UnresolvedModelIntents
	}
	return a
}

func turnAccounting(turn dashi.Turn, view dashi.TurnView) Accounting {
	a := Accounting{Spend: view.Spend.Clone()}
	intents := map[string]bool{}
	settled := map[string]bool{}
	dispatched := map[string]bool{}
	for _, step := range turn.Steps {
		f := step.Model
		if f == nil {
			continue
		}
		if step.Kind == dashi.StepModelCallStarted {
			intents[f.AttemptID] = true
			continue
		}
		if step.Kind != dashi.StepModelCallFinished && step.Kind != dashi.StepModelCallFinishedByHost {
			continue
		}
		settled[f.AttemptID] = !f.Uncertain && f.Failure != llm.FailureUnknown
		if f.NoDispatch || f.Failure == llm.FailureNotDispatched {
			continue
		}
		r := f.Result
		if f.Failure == llm.FailureRejected || r != nil && (r.ID != "" || len(r.Message.Content) > 0 || r.Usage != nil || r.Stop.Kind != "") {
			dispatched[f.AttemptID] = true
		}
	}
	a.ModelCalls = len(dispatched)
	for id := range intents {
		if !settled[id] {
			a.UnresolvedModelIntents++
		}
	}
	return a
}

func executionSnapshot(turn dashi.Turn, view dashi.TurnView, accounting Accounting) Execution {
	e := Execution{TurnID: turn.ID, Revision: view.Revision, Readable: true, Summary: view.Origin == dashi.StartCompaction, Stop: view.Stop, Accounting: accounting, Next: view.Next(), ReadyToContinue: view.ReadyToContinue}
	e.Destination = Destination{view.Agent.Provider, view.Agent.API, view.Agent.Model, view.Agent.Revision}
	for _, item := range view.Pending {
		p := PendingObservation{Kind: item.Kind, ID: item.ID}
		if item.Tool != nil {
			p.ToolStatus = item.Tool.Status
			a := item.Tool.Definition.Annotations
			safe := a.ReadOnly != nil && *a.ReadOnly || a.Idempotent != nil && *a.Idempotent
			p.UnsafeEffect = item.Tool.Status == dashi.ToolResultUnknown || item.Tool.Status == dashi.ToolStarted && !safe
		}
		e.Pending = append(e.Pending, p)
	}
	e.Context = contextOccupancy(turn, view)
	return e
}

// ContextOccupancy is one recorded model request, not summed session usage.
// Available applies to measured complete usage. Estimated data has no percentage.
type ContextOccupancy struct {
	TurnID, AttemptID    string
	Purpose              dashi.ModelPurpose
	Tokens, Window       int
	WindowSource         string
	Available, Estimated bool
	UnavailableReason    string
}

func contextOccupancy(turn dashi.Turn, view dashi.TurnView) ContextOccupancy {
	out := ContextOccupancy{TurnID: turn.ID, UnavailableReason: "no model request recorded"}
	var intent, settled *dashi.ModelFact
	for _, step := range turn.Steps {
		if step.Model == nil {
			continue
		}
		if step.Kind == dashi.StepModelCallStarted {
			intent = step.Model
			settled = nil
		} else if intent != nil && step.Model.AttemptID == intent.AttemptID && (step.Kind == dashi.StepModelCallFinished || step.Kind == dashi.StepModelCallFinishedByHost) {
			settled = step.Model
		}
	}
	if intent == nil {
		return out
	}
	out.AttemptID = intent.AttemptID
	out.Purpose = intent.Purpose
	if view.Origin == dashi.StartCompaction {
		out.Purpose = dashi.ModelPurposeCompaction
	}
	if intent.ModelInfo != nil && intent.ModelInfo.Capabilities.ContextWindow > 0 {
		out.Window = intent.ModelInfo.Capabilities.ContextWindow
		out.WindowSource = "recorded model window"
	}
	if view.ActiveCompactionWindow > 0 && (out.Window == 0 || view.ActiveCompactionWindow < out.Window) {
		out.Window = view.ActiveCompactionWindow
		out.WindowSource = "recorded active-protection window"
	}
	if settled == nil {
		out.UnavailableReason = "model intent unresolved"
		return out
	}
	out.Estimated = settled.EstimatedUsage
	if settled.NoDispatch || settled.Failure == llm.FailureNotDispatched {
		out.UnavailableReason = "model call not dispatched"
		return out
	}
	if settled.Result == nil || settled.Result.Usage == nil {
		out.UnavailableReason = "usage unavailable"
		return out
	}
	usage := settled.Result.Usage
	if usage.Validate() != nil || usage.TotalTokenCountMismatch {
		out.UnavailableReason = "usage invalid or mismatched"
		return out
	}
	out.Tokens = usage.TotalInputTokens() + usage.OutputTokens // Validate checks this disjoint sum.
	if out.Estimated {
		out.UnavailableReason = "usage estimated or partial"
		return out
	}
	if out.Window == 0 {
		out.UnavailableReason = "recorded window unavailable"
		return out
	}
	out.Available = true
	out.UnavailableReason = ""
	return out
}
