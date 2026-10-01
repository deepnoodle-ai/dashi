package dashi

import (
	"encoding/json"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// ToolStatus describes a call's recorded execution state.
type ToolStatus string

const (
	// ToolRequested means the model requested the call.
	ToolRequested ToolStatus = "requested"
	// ToolApprovalPending means a BeforeTool hook deferred the call, which
	// awaits a host answer.
	ToolApprovalPending ToolStatus = "approval_pending"
	// ToolDenied means a hook or the host refused the call.
	ToolDenied ToolStatus = "denied"
	// ToolAllowed means the call may run.
	ToolAllowed ToolStatus = "allowed"
	// ToolStarted means execution began without a settled outcome.
	ToolStarted ToolStatus = "started"
	// ToolWaiting means execution deferred completion.
	ToolWaiting ToolStatus = "waiting"
	// ToolUncertain means execution may have occurred without a known result.
	ToolUncertain ToolStatus = "uncertain"
	// ToolSettled means a final outcome was recorded.
	ToolSettled ToolStatus = "settled"
)

// ToolView projects the current call state from committed facts.
type ToolView struct {
	Call       tool.Call
	Definition tool.Definition
	Status     ToolStatus
	// Verdict is the combined BeforeTool result.
	Verdict ToolVerdict
	Outcome *tool.Outcome
	// ArgsDigest is set while Status is ToolApprovalPending. It identifies
	// Call.Args by JSON meaning; echo it in ApprovalFact.ArgsDigest to approve
	// or deny exactly the arguments shown.
	ArgsDigest string
	// Approval is the host's answer to a deferred call, recorded by Settle.
	// Verdict remains the hooks' deferral.
	Approval *ApprovalFact
	// result is what the model sees instead of the outcome; see Result.
	result    *llm.ToolResult
	shortened *Shortened
	// steered marks a call an urgent steer kept from starting.
	steered bool
}

// Shortened returns how the result limit shortened the result in the
// projected history, as recorded when the call settled, or nil when the
// result is complete. It reads the record, never the current limit.
func (v ToolView) Shortened() *Shortened {
	if v.shortened == nil {
		return nil
	}
	s := *v.shortened
	return &s
}

// Result is what the model sees for a settled call: the outcome's output,
// or what an AfterTool hook or the result limit replaced it with. It is the
// zero value before the call settles.
func (v ToolView) Result() llm.ToolResult {
	if v.result != nil {
		return cloneToolResult(*v.result)
	}
	if v.Outcome == nil || v.Status != ToolSettled {
		return llm.ToolResult{}
	}
	return modelToolResult(v.Call.ID, v.Outcome)
}

// TurnView is the current state reconstructed from acknowledged steps.
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
	// Compaction is what a compaction turn replaces, recorded at Start; nil
	// for other turns.
	Compaction *CompactionFact
	// Revision is the latest acknowledged step sequence number.
	Revision int64
	// ModelAttempts counts settled calls that were not interrupted.
	ModelAttempts int
	// Interruptions counts resumes after a context stop or worker crash,
	// except an interrupted stop before the first model attempt.
	Interruptions  int
	ProviderPauses int
	// Budget is the current cap, including any recorded raises.
	Budget *Budget
	// Raises preserves the authorized cap changes in order.
	Raises []BudgetRaise
	// Spend is rebuilt from committed model and tool facts. Its model usage
	// and cost are the sum of Attempts.
	Spend Spend
	// Attempts lists every model attempt in record order, including one
	// still pending, with what it counts toward Spend.
	Attempts []AttemptSpend
	// Stop is why the last run ended. Replay and Outcome project a missing
	// stop as Run does, and mark it Projected: uncertain for an unsettled
	// model attempt, interrupted for work in flight, waiting for pending
	// work. A hook's
	// view is taken mid-run, so its Stop is empty. Next says what to do
	// about the stop.
	Stop   Stop
	Output *StructuredOutput
	// Answer is the text of the latest assistant message. On a completed
	// turn it is the final answer, and valid tool-only work can leave it
	// empty; on a waiting turn it is text the model wrote alongside its
	// tool calls. On an ended turn (Stop.Terminal) it is fixed at the
	// terminal stop, as Project is: a late model result does not change it.
	Answer string
	// Pending lists the work awaiting a host action or settlement: model
	// attempts, then local calls in call order, then hosted approval
	// requests in the order the model made them.
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
	// usedContextIDs includes all recorded delivery IDs, including expired items.
	usedContextIDs map[string]bool
	// acted holds the hooks, by point and name, with a recorded answer; see
	// HookActed.
	acted map[hookKey]bool
}

type hookKey struct {
	point HookPoint
	name  string
}

// HookActed reports whether the named hook at point has given an answer
// this turn, other than no opinion or a failure, so a hook can act once per
// turn: a BeforeFinish hook that nudges only once asks
// HookActed(HookBeforeFinish, its name). It reads the turn record, so the
// answer holds for a turn resumed in another process. An answer is recorded
// with the next step committed after the hook ran, so a hook does not see
// its own answer until then.
func (v TurnView) HookActed(point HookPoint, name string) bool {
	return v.acted[hookKey{point, name}]
}

// Context returns the latest context item with this origin and name that the
// next model call would still see, so a BeforeModel hook can tell whether
// the item it would deliver is already current. An empty origin means
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
