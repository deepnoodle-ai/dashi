package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Hook is the shape of every hook: a named function Dashi calls at one hook
// point of a turn. Use it through ModelHook, SendHook, ToolHook, ResultHook
// and FinishHook, one per point. Every hook at a point runs, one at a time and in
// order, and their answers combine by the point's rule; see Hooks. A zero
// answer means no opinion. Dashi records each hook's answer with the step it
// leads to, and replay reads that record instead of calling hooks again, so a
// hook may be called again only when a crash came before its answer was
// saved.
//
// A hook receives copies and returns a value: it cannot change the turn in
// place. It should honor ctx, which ends with the run. One Runner may run
// many turns at once and calls the same hook from each, so a hook that keeps
// state must guard it. A BeforeTool hook that blocks, such as a terminal
// prompt, holds up the batch it belongs to while earlier calls run.
type Hook[P, D any] struct {
	// Name identifies the hook in the record. It must be nonempty and unique
	// within its hook point. Reusable packages should prefix their names. A
	// failing BeforeTool hook's name is shown to the model, so keep private
	// details out of it.
	Name string
	Func func(context.Context, P) (D, error)
	// FailOpen makes an error or invalid answer a warning instead of a
	// guard, for hooks that only improve a result. It is allowed only for
	// AfterTool, where the result stands as earlier hooks left it, and
	// BeforeFinish, where the error counts as no opinion. Run rejects it at
	// the other points.
	FailOpen bool
}

// ModelHook runs before each model request is prepared. It fails open: an
// error or invalid answer skips that hook with a WarningHookFailed.
type ModelHook Hook[ModelPlan, ModelDecision]

// Hooks returns a Hooks value holding just h, for a Runner or WithHooks.
func (h ModelHook) Hooks() Hooks {
	return Hooks{BeforeModel: []ModelHook{h}}
}

// SendHook authorizes each prepared model request and its spend. It fails
// closed: an error stops the turn as StopFailed with StopReasonHookFailed.
type SendHook Hook[SendPlan, SendDecision]

// Hooks returns a Hooks value holding just h, for a Runner or WithHooks.
func (h SendHook) Hooks() Hooks {
	return Hooks{BeforeSend: []SendHook{h}}
}

// ToolHook decides whether a model-requested client tool call may run. It
// fails closed: an error denies the call and records a WarningHookFailed.
type ToolHook Hook[ToolPlan, ToolDecision]

// Hooks returns a Hooks value holding just h, for a Runner or WithHooks.
func (h ToolHook) Hooks() Hooks {
	return Hooks{BeforeTool: []ToolHook{h}}
}

// FinishHook sees each candidate final answer, final text or output
// submitted with dashi_submit_output, and may turn it down so the model
// continues. It fails closed: an error stops the turn as StopFailed with
// StopReasonHookFailed, and Continue runs the hooks again on the same
// answer, unless context was delivered since and a normal model call
// remains, in which case the model is asked again.
type FinishHook Hook[FinishPlan, FinishDecision]

// Hooks returns a Hooks value holding just h, for a Runner or WithHooks.
func (h FinishHook) Hooks() Hooks {
	return Hooks{BeforeFinish: []FinishHook{h}}
}

// ResultHook sees each final tool result from a tool or the host and may
// replace what the model sees of it; see ResultPlan. It fails closed: an
// error withholds the result from the model, with a WarningHookFailed,
// unless FailOpen is set.
type ResultHook Hook[ResultPlan, ResultDecision]

// Hooks returns a Hooks value holding just h, for a Runner or WithHooks.
func (h ResultHook) Hooks() Hooks {
	return Hooks{AfterTool: []ResultHook{h}}
}

// Hooks holds the hooks for each hook point. The zero value has none, and
// then Dashi runs every bound tool and every model call.
//
// Combining rules, applied in list order:
//
//   - BeforeModel: each hook sees the settings as changed by earlier hooks,
//     deliveries append, and the first Stop ends the chain without
//     committing any deliveries.
//   - BeforeSend: the first denial stops the turn. Each hook's Admission is
//     recorded under its name. An urgent steer that arrives before the
//     request is recorded sends the run back for the steer, so BeforeSend
//     may be asked again for the same reply.
//   - BeforeTool: the first denial ends the chain; otherwise a deferral wins
//     over an allow. A Rewrite restarts the chain on the new arguments.
//   - AfterTool: each hook sees the result as earlier hooks replaced it.
//   - BeforeFinish: deliveries append, the first Reason is kept, and
//     AcceptEmpty holds if any hook says so.
//
// When the run's context has ended, a hook's error gives the normal context
// stop, and nothing is recorded for that hook, except at AfterTool: the
// tool's result is still recorded, withheld from the model unless the
// remaining hooks fail open. A panic counts as an error.
type Hooks struct {
	BeforeModel  []ModelHook
	BeforeSend   []SendHook
	BeforeTool   []ToolHook
	AfterTool    []ResultHook
	BeforeFinish []FinishHook
}

// HookSet is anything that supplies hooks: a Hooks value, a single hook
// such as a ToolHook, or a package's bundle of hooks.
type HookSet interface {
	Hooks() Hooks
}

// Hooks returns h, so a Hooks value is a HookSet.
func (h Hooks) Hooks() Hooks {
	return h
}

// JoinHooks returns the hooks of hs in order, in new slices, so later
// changes to either side do not affect the other. Pass hooks directly, as
// in JoinHooks(onCall, guard), or whole sets. A nil set is skipped,
// including a nil pointer such as a *ModelHook.
func JoinHooks(hs ...HookSet) Hooks {
	var out Hooks
	for _, set := range hs {
		if isNilHookSet(set) {
			continue
		}
		h := set.Hooks()
		out.BeforeModel = append(out.BeforeModel, h.BeforeModel...)
		out.BeforeSend = append(out.BeforeSend, h.BeforeSend...)
		out.BeforeTool = append(out.BeforeTool, h.BeforeTool...)
		out.AfterTool = append(out.AfterTool, h.AfterTool...)
		out.BeforeFinish = append(out.BeforeFinish, h.BeforeFinish...)
	}
	return out
}

// isNilHookSet reports whether set is nil or holds a nil pointer, whose
// value-receiver Hooks method would panic.
func isNilHookSet(set HookSet) bool {
	if set == nil {
		return true
	}
	v := reflect.ValueOf(set)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// WithHooks adds hooks for one Run or Stream call, after Runner.Hooks. Use it
// for per-command extras such as a terminal prompt; hooks every command
// needs belong on the Runner, since each command runs only the hooks it is
// given.
func WithHooks(h Hooks) RunOption {
	return func(o *runOptions) {
		o.hooks = JoinHooks(o.hooks, h)
	}
}

// HookPoint names where in a turn a hook runs.
type HookPoint string

const (
	HookBeforeModel  HookPoint = "before_model"
	HookBeforeSend   HookPoint = "before_send"
	HookBeforeTool   HookPoint = "before_tool"
	HookAfterTool    HookPoint = "after_tool"
	HookBeforeFinish HookPoint = "before_finish"
)

// HookVerdict summarizes one hook's answer in the record.
type HookVerdict string

const (
	// VerdictFailed is a hook error, panic or invalid answer, at any point.
	VerdictFailed HookVerdict = "failed"
	// VerdictChange and VerdictStop are BeforeModel answers: changed
	// settings or delivered context, or a stop.
	VerdictChange HookVerdict = "change"
	VerdictStop   HookVerdict = "stop"
	// VerdictReserve and VerdictLimit are BeforeSend answers: admission terms,
	// or a denial with a limit. VerdictDeny is also a BeforeTool answer.
	VerdictReserve HookVerdict = "reserve"
	VerdictLimit   HookVerdict = "limit"
	VerdictDeny    HookVerdict = "deny"
	// VerdictAllow, VerdictDefer and VerdictRewrite are BeforeTool answers.
	VerdictAllow         HookVerdict = "allow"
	VerdictNeedsApproval HookVerdict = "needs_approval"
	VerdictRewrite       HookVerdict = "rewrite"
	// VerdictAddMessages, VerdictReject and VerdictAcceptEmpty are BeforeFinish
	// answers: delivered items, a reason alone, or accepting an empty
	// answer.
	VerdictAddMessages HookVerdict = "add_messages"
	VerdictReject      HookVerdict = "reject"
	VerdictAcceptEmpty HookVerdict = "accept_empty"
	// VerdictReplace is an AfterTool answer: the model sees a replaced
	// result.
	VerdictReplace HookVerdict = "replace"
)

// HookResult records one hook's answer: every answer that was not "no
// opinion", and every failure. It rides on the next step committed after
// the hook ran, whatever that step's kind.
type HookResult struct {
	Point   HookPoint   `json:"point"`
	Name    string      `json:"name"`
	Verdict HookVerdict `json:"verdict"`
	// Pass counts the rewrite restarts before a BeforeTool answer.
	Pass         int    `json:"pass,omitempty"`
	RulesVersion string `json:"rules_version,omitempty"`
	Reason       string `json:"reason,omitempty"`
	// Error is the hook's error text for a failed verdict. The model never
	// sees it.
	Error string `json:"error,omitempty"`
}

// ActiveHooks lists the names of the Runner's hooks per point, recorded on
// the first step of each Start and Continue and on a Resolve with a tool
// result, the commands that run hooks.
// Hooks added with WithHooks are left out, since they are per-command by
// design.
type ActiveHooks struct {
	BeforeModel  []string `json:"before_model,omitempty"`
	BeforeSend   []string `json:"before_send,omitempty"`
	BeforeTool   []string `json:"before_tool,omitempty"`
	AfterTool    []string `json:"after_tool,omitempty"`
	BeforeFinish []string `json:"before_finish,omitempty"`
}

// ModelPlan gives BeforeModel the proposed model input and current turn
// state.
type ModelPlan struct {
	Purpose ModelPurpose
	// Input is the proposed request, with settings as changed by earlier
	// hooks.
	Input llm.Input
	// Turn is the committed state projected before the next model call.
	Turn TurnView
}

// ModelDecision changes the next model input or stops before preparation.
type ModelDecision struct {
	// Settings replaces the input settings when non-nil.
	Settings *llm.Settings
	// AddMessages records additional context before preparing the model request.
	AddMessages []ContextItem
	// Hide removes the named tools from this call; every hook's list
	// applies, and names must match exactly. Names no tool of this call has,
	// and dashi_submit_output, which cannot be hidden, are ignored with one
	// WarningHideIgnored per call. A hide wins over a forced tool choice that
	// names a tool any hook hid: the choice is cleared, with
	// WarningToolChoiceCleared. When no tools are left, the tool choice and
	// ParallelTools are cleared too, since providers reject them without
	// tools. The hidden names are recorded in ModelFact.Hidden. Hide has no
	// effect on the reserved final-answer call, which offers no application
	// tools.
	//
	// Hide shapes what the model is offered; it does not forbid a call. A
	// failing BeforeModel hook is skipped, which offers the tools again, so
	// to fail closed, handle errors inside the hook and return the hide, or
	// pair it with a BeforeTool deny. A call to a hidden tool never reaches
	// BeforeTool, so the deny acts only when a failed hook offered the tool
	// again. Changing the offered tools resets the whole cached prompt,
	// not only the tools, so keep hides stable.
	Hide []string
	// Stop ends this run before a model call when non-nil. Its kind must be
	// StopRefused, StopFailed, or StopLimited with a host, cost or token
	// Limit; other stops could end the turn for good by accident.
	Stop *Stop
}

// SendPlan gives BeforeSend the immutable prepared request, the turn's spend,
// and the cost of allowing this call. A nil AtRisk is not a zero-cost call.
type SendPlan struct {
	Purpose ModelPurpose
	// Request is the immutable provider request after preparation.
	Request llm.Request
	// Turn is a snapshot of committed spend and state before admission.
	Turn TurnView
	// WorstCaseCost is nil when the call cannot be bounded by available prices.
	WorstCaseCost *llm.Cost
	// Remaining is nil when the turn has no cost cap.
	Remaining *Money
}

// SendDecision is a BeforeSend answer. The zero value allows the call.
type SendDecision struct {
	// Deny stops the turn as StopRefused with Reason.
	Deny   bool
	Reason string
	// Limit stops the turn as StopLimited instead, so a host spending denial
	// can resume after funding. It implies Deny, and its kind must be host,
	// cost, or a token cap.
	Limit *LimitDetail
	// Reservation is host-owned, non-secret JSON recorded with the model intent
	// under this hook's name, so a recorder can atomically reserve spending
	// against these exact terms. All hooks' admissions share a 16 KiB limit.
	// BeforeSend may be asked again for the same reply, so reserve in the
	// recorder with the intent, not in the hook.
	Reservation json.RawMessage
}

// ToolPlan gives BeforeTool a call whose arguments already match the tool's
// schema.
type ToolPlan struct {
	Call       tool.Call
	Definition tool.Definition
	// Turn is a mid-run view: its Stop is empty.
	Turn TurnView
}

// DecisionKind identifies a tool authorization result.
type DecisionKind string

const (
	// DecisionAllow permits the tool call.
	DecisionAllow DecisionKind = "allow"
	// DecisionDeny records a denied tool call.
	DecisionDeny DecisionKind = "deny"
	// DecisionNeedsApproval leaves authorization to a later host decision.
	DecisionNeedsApproval DecisionKind = "needs_approval"
)

// ToolDecision is a BeforeTool answer; build it with Allow, Deny, Defer or
// Rewrite. The zero value is no opinion. When every hook has no opinion, the
// call runs: binding a tool is the app's statement that the model may use it.
type ToolDecision struct {
	Kind DecisionKind
	// Reason is recorded; for a denial it is also the result the model sees.
	Reason string
	// RulesVersion is audit data naming the rules that decided, such as a policy
	// version.
	RulesVersion string
	// Rewrite replaces the call's arguments. The new arguments are checked
	// against the tool's schema, and every hook is asked again about them.
	// Kind is ignored.
	Rewrite json.RawMessage
}

// ToolVerdict is the combined BeforeTool result recorded for a call. An allow
// names the first hook that returned Allow; when none did, Hook is empty and
// Reason is "no hook opinion". A deny or defer names the hook that decided.
type ToolVerdict struct {
	Kind         DecisionKind `json:"kind"`
	Reason       string       `json:"reason,omitempty"`
	Hook         string       `json:"hook,omitempty"`
	RulesVersion string       `json:"rules_version,omitempty"`
}

// ResultPlan gives AfterTool one final tool result. Outcome is what
// happened and stays the record of execution state and cost; Result is what
// the model will see, as earlier hooks left it.
//
// A result hook changes only what the model sees: the next model call, and
// the next turn's history through Project. The record, Observer and Stream
// events, and ToolView.Outcome keep the raw output, so a result hook is not
// storage redaction; keep a secret out of the record in the executor.
type ResultPlan struct {
	Call       tool.Call
	Definition tool.Definition
	Outcome    tool.Outcome
	Result     llm.ToolResult
	// Turn is the state before this result is recorded.
	Turn TurnView
}

// ResultDecision is an AfterTool answer. The zero value keeps the result.
type ResultDecision struct {
	// Replace is what the model sees instead of Result. The runner sets its
	// CallID. A replacement with the same JSON meaning as Result is no
	// opinion, so a hook may return one on every call.
	Replace *llm.ToolResult
}

// FinishPlan gives BeforeFinish a candidate final answer: model text that
// would end the turn, or a schema-valid dashi_submit_output call.
type FinishPlan struct {
	Turn        TurnView
	ModelCallID string
	// CallID is the dashi_submit_output call for a submitted output, and
	// empty for a final text answer.
	CallID string
	// Reply is the candidate model message's text.
	Reply string
	// Output is the structured result on a turn with an output schema: the
	// submitted value, or the value parsed from the text. It is nil when
	// parsing failed, in which case OutputErr says why.
	Output    json.RawMessage
	OutputErr error
}

// FinishDecision is a BeforeFinish answer. The zero value accepts the
// candidate.
type FinishDecision struct {
	// AddMessages, when nonempty, asks the model again with these items instead
	// of accepting the candidate. When no further normal model call can
	// happen, such as on the reserved final-answer call, the items are
	// dropped with a warning and the candidate stands.
	AddMessages []ContextItem
	// Reason vetoes the candidate: it is unacceptable. The model sees "Not
	// accepted: " and the reason, on either path, and answers again, using
	// the reserved final-answer call if that is the only one left. When no
	// model call at all can happen, or the reserved answer itself is vetoed,
	// the veto still holds: the rejection is recorded and the turn stops as
	// StopFailed with StopReasonOutputRejected, instead of accepting the
	// answer. AddMessages alone is a nudge, which never spends the reserved call
	// and is dropped when no normal call remains. The first hook's
	// reason is used, bounded to 1 KiB. An empty reason on a submission
	// turned down by items gives "a check did not accept this output".
	Reason string
	// AcceptEmpty accepts a candidate with no text, which otherwise fails the
	// turn as empty_response.
	AcceptEmpty bool
}

// validate reports hooks that cannot run: an empty or repeated name within a
// point, or a nil function.
func (h Hooks) validate() error {
	return errors.Join(
		validatePoint(HookBeforeModel, h.BeforeModel),
		validatePoint(HookBeforeSend, h.BeforeSend),
		validatePoint(HookBeforeTool, h.BeforeTool),
		validatePoint(HookAfterTool, h.AfterTool),
		validatePoint(HookBeforeFinish, h.BeforeFinish),
	)
}

// hookShape matches the four hook types, which share Hook's fields, so the
// helpers below serve every hook point.
type hookShape[P, D any] interface {
	~struct {
		Name     string
		Func     func(context.Context, P) (D, error)
		FailOpen bool
	}
}

func validatePoint[H hookShape[P, D], P, D any](
	point HookPoint,
	hooks []H,
) error {
	seen := map[string]bool{}
	for i, x := range hooks {
		h := Hook[P, D](x)
		if h.Name == "" {
			return fmt.Errorf("dashi: %s hook %d has no name", point, i+1)
		}
		if seen[h.Name] {
			hint := ""
			if point == HookBeforeModel {
				hint = "; a SeedContext hook is named after its item"
			}
			return fmt.Errorf(
				"dashi: %s hook %q is listed twice%s",
				point,
				h.Name,
				hint,
			)
		}
		if h.Func == nil {
			return fmt.Errorf("dashi: %s hook %q has no function", point, h.Name)
		}
		if h.FailOpen && point != HookAfterTool && point != HookBeforeFinish {
			return fmt.Errorf(
				"dashi: %s hook %q cannot fail open; only after_tool and before_finish hooks can",
				point,
				h.Name,
			)
		}
		seen[h.Name] = true
	}
	return nil
}

// active lists the hook names per point for the record.
func (h Hooks) active() *ActiveHooks {
	a := &ActiveHooks{
		BeforeModel:  hookNames(h.BeforeModel),
		BeforeSend:   hookNames(h.BeforeSend),
		BeforeTool:   hookNames(h.BeforeTool),
		AfterTool:    hookNames(h.AfterTool),
		BeforeFinish: hookNames(h.BeforeFinish),
	}
	if a.BeforeModel == nil && a.BeforeSend == nil &&
		a.BeforeTool == nil && a.AfterTool == nil && a.BeforeFinish == nil {
		return nil
	}
	return a
}

func hookNames[H hookShape[P, D], P, D any](hooks []H) []string {
	var names []string
	for _, h := range hooks {
		names = append(names, Hook[P, D](h).Name)
	}
	return names
}

// callHook runs one hook, turning a panic into an error that keeps a bounded
// stack, so a hook bug never leaves the turn half-recorded and can still be
// debugged from the record.
func callHook[H hookShape[P, D], P, D any](
	ctx context.Context,
	x H,
	plan P,
) (decision D, err error) {
	h := Hook[P, D](x)
	defer func() {
		if v := recover(); v != nil {
			var zero D
			decision = zero
			err = fmt.Errorf("panic: %v\n%s", v, debug.Stack())
		}
	}()
	return h.Func(ctx, plan)
}

// validHookStop reports whether a hook may end the turn with stop. A hook may
// refuse, fail, or report a host, cost or token limit; the other limit kinds
// make a stop terminal, and a completed or waiting stop would misstate the
// turn.
func validHookStop(stop Stop) bool {
	if reservedStopReason(stop.Reason) {
		return false
	}
	switch stop.Kind {
	case StopRefused, StopFailed:
		return stop.Limit == nil
	case StopLimited:
		return stop.Limit != nil && validHookLimit(*stop.Limit)
	}
	return false
}

func validHookLimit(limit LimitDetail) bool {
	switch limit.Kind {
	case LimitHost, LimitCost, LimitOutputTokens, LimitTotalTokens:
		return limit.Validate() == nil
	}
	return false
}

// reservedStopReason reports the reasons Dashi gives its own failed stops, so
// a host branching on them never mistakes a hook's stop for one.
func reservedStopReason(reason string) bool {
	switch reason {
	case StopReasonHookFailed, StopReasonOutputRejected, StopReasonToolFatal,
		StopReasonToolOutcomeInvalid, StopReasonContentNoLoader,
		StopReasonContentUnavailable, StopReasonContentTooLarge,
		StopReasonContentSizeMismatch, StopReasonContentDigestMismatch:
		return true
	}
	return false
}

// maxAdmission bounds all hooks' admission JSON on one model intent.
const maxAdmission = 16 << 10
