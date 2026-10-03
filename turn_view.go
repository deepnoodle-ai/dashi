package dashi

import (
	"encoding/json"

	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// ToolStatus says how far a tool call has got, by the record.
type ToolStatus string

const (
	// ToolRequested means the model requested the call.
	ToolRequested ToolStatus = "requested"
	// ToolApprovalPending means a BeforeTool hook said the call needs
	// approval, and it waits for the host to approve or deny it.
	ToolApprovalPending ToolStatus = "approval_pending"
	// ToolDenied means a hook or the host refused the call.
	ToolDenied ToolStatus = "denied"
	// ToolAllowed means the call may run.
	ToolAllowed ToolStatus = "allowed"
	// ToolStarted means the call started and no result is recorded yet.
	ToolStarted ToolStatus = "started"
	// ToolWaiting means the executor said the result will come later, from
	// outside the run.
	ToolWaiting ToolStatus = "waiting"
	// ToolResultUnknown means the call may have run, but its result is
	// unknown.
	ToolResultUnknown ToolStatus = "result_unknown"
	// ToolFinished means a final outcome was recorded.
	ToolFinished ToolStatus = "finished"
)

// ToolView is a tool call's current state, built from the committed steps.
type ToolView struct {
	Call       tool.Call
	Definition tool.Definition
	Status     ToolStatus
	// Verdict is the combined BeforeTool result.
	Verdict ToolVerdict
	Outcome *tool.Outcome
	// ArgsHash is set while Status is ToolApprovalPending. It identifies
	// Call.Args by JSON meaning; echo it in ApprovalDecision.ArgsHash to approve
	// or deny exactly the arguments shown.
	ArgsHash string
	// Approval is the host's decision on a call that needed approval,
	// recorded by Resolve. Verdict still holds the hooks' NeedsApproval.
	Approval *ApprovalDecision
	// result is what the model sees instead of the outcome; see Result.
	result    *llm.ToolResult
	shortened *Shortened
	// steered marks a call an urgent steer kept from starting.
	steered bool
}

// Shortened returns how the result limit shortened the result the model
// sees, as recorded when the call finished, or nil when the result is
// complete. It reads the record, never the current limit.
func (v ToolView) Shortened() *Shortened {
	if v.shortened == nil {
		return nil
	}
	s := *v.shortened
	return &s
}

// Result is what the model sees for a finished call: the outcome's output,
// or what an AfterTool hook or the result limit replaced it with. It is the
// zero value before the call finishes.
func (v ToolView) Result() llm.ToolResult {
	if v.result != nil {
		return llmclone.ToolResult(*v.result)
	}
	if v.Outcome == nil || v.Status != ToolFinished {
		return llm.ToolResult{}
	}
	return modelToolResult(v.Call.ID, v.Outcome)
}

// TurnView is a turn's current state, rebuilt from its saved steps. Get one
// from Turn.View or from a RunResult.
type TurnView struct {
	ID                     string
	Agent                  Agent
	ActiveCompactionWindow int
	ContextCompactions     []ContextCompactionFact
	// OutputSchema is the schema pinned at Start for this turn.
	OutputSchema json.RawMessage
	// Parent is the tool call that caused this turn, or nil for a root
	// turn. It is fixed at Start.
	Parent *CallRef
	// Origin is who supplied the turn's initial work, recorded at Start: an
	// unset Start.Origin reads as StartUser. It is empty before Start.
	Origin StartOrigin
	// Input is the turn's input, in record order: Start.Input, then the
	// message of each later item from the user (origin ContextUser) that an
	// AddMessages command or a steer added. Items that hooks add are not
	// input. A named item's message carries its slot in Context, as in the
	// turn's messages. A hook or tool reads the user's request here.
	Input []llm.Message
	// Compaction is what a compaction turn replaces, recorded at Start; nil
	// for other turns.
	Compaction *CompactionFact
	// LastSeq is the latest acknowledged step sequence number.
	LastSeq int64
	// ModelCalls counts finished model calls that were not interrupted.
	ModelCalls int
	// Resumes counts resumes after a context stop or worker crash,
	// except an interrupted stop before the first model call.
	Resumes        int
	ProviderPauses int
	// Budget is the current cap, including any recorded raises.
	Budget *Budget
	// Raises preserves the authorized cap changes in order.
	Raises []BudgetRaise
	// Spend is rebuilt from the committed model and tool steps. Its model
	// usage and cost are the sum of ModelCallCosts.
	Spend Spend
	// ModelCallCosts lists every model call in record order, including one
	// still pending, with what it counts toward Spend.
	ModelCallCosts []ModelCallCost
	// Stop is why the last run ended. When the record holds no stop, View
	// and RunResult infer one the way Run would, and set Stop.Inferred:
	// result_unknown for a model call with no recorded result, interrupted
	// for work in flight, waiting for pending work. A hook's view is taken
	// mid-run, so its Stop is empty. Next says what to do about the stop.
	Stop   Stop
	Output *StructuredOutput
	// Reply is the text of the latest assistant message. On a completed
	// turn it is the final reply, and valid tool-only work can leave it
	// empty; on a waiting turn it is text the model wrote alongside its
	// tool calls. On an ended turn (Stop.Ended) it is fixed at the stop
	// that ended the turn, as TurnMessagesFull is: a late model result does
	// not change it.
	Reply string
	// Pending lists the work waiting for the host to act or resolve it:
	// model calls, then local tool calls in call order, then provider tool
	// approval requests in the order the model made them.
	Pending []Pending
	// Warnings includes nonfatal conditions from acknowledged steps.
	Warnings []Warning
	// ReadyToContinue means Continue can run without first raising a budget.
	// A limited turn with a spent cap can still accept a Continue that raises it.
	ReadyToContinue bool
	// Tools is keyed by tool call ID and rebuilt from committed steps.
	Tools map[string]ToolView

	// live holds the latest named context item per origin and name that the
	// next model call would still see; see Context.
	live map[string]ContextItem
	// usedContextIDs holds the IDs of every recorded context item,
	// including expired items.
	usedContextIDs map[string]bool
	// acted holds the hooks, by point and name, with a recorded decision;
	// see HookActed.
	acted map[hookKey]bool
}

type hookKey struct {
	point HookPoint
	name  string
}

// HookActed reports whether the named hook at point has made a decision
// this turn, other than no opinion or a failure, so a hook can act once per
// turn: a BeforeFinish hook that nudges only once asks
// HookActed(HookBeforeFinish, its name). It reads the turn record, so the
// result holds for a turn resumed in another process. A decision is
// recorded with the next step committed after the hook ran, so a hook does
// not see its own decision until then.
func (v TurnView) HookActed(point HookPoint, name string) bool {
	return v.acted[hookKey{point, name}]
}

// Context returns the latest context item with this origin and name that the
// next model call would still see, so a BeforeModel hook can tell whether
// the item it would add is already current. An empty origin means
// ContextUser.
func (v TurnView) Context(
	origin ContextOrigin,
	name string,
) (ContextItem, bool) {
	item, ok := v.live[contextKey(ContextItem{Origin: origin, Name: name})]
	if !ok {
		return ContextItem{}, false
	}
	return cloneContextItem(item), true
}
