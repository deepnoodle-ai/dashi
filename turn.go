// Package dashi runs identified, recordable agent turns over the independent
// llm and tool foundations. Its API is experimental.
package dashi

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Agent is reusable, serializable behavior: what to ask which model, with
// which settings and limits. Clients, credentials, tools, and storage are
// supplied separately on a Runner. The zero value works: the Runner fills
// empty fields before pinning the agent at Start, and the turn records the
// result, which TurnView.Agent shows.
type Agent struct {
	// ID names the agent for the host, for example in reports. It may be
	// empty; nothing in Dashi reads it.
	ID string `json:"id"`
	// Revision pins behavior so a continued turn cannot silently change it.
	// When empty, it is derived from the agent's behavior fields as
	// "auto:" and 16 hex digits, so it changes exactly when they do. The
	// derivation leaves out Limits.Deadline and cannot see tools, which are
	// not part of the agent; set Revision yourself to version those.
	Revision     string `json:"revision"`
	Instructions string `json:"instructions,omitempty"`
	// Provider and API default to the Runner client's.
	Provider string `json:"provider"`
	API      string `json:"api"`
	// Model defaults to the provider's catalog.Default when the client knows
	// that model; otherwise it is required.
	Model    string       `json:"model"`
	Settings llm.Settings `json:"settings"`
	// OutputSchema is the default final-result schema for turns using this
	// agent. Start.OutputSchema may override it for one turn.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Limits       Limits          `json:"limits"`
}

// Limits bounds model calls, interruptions, pauses, tool concurrency, and time
// for turns using an agent. The zero value gives the default limits, so
// set only the fields you want to change. For counts where "none" is a
// meaningful setting, a negative value means none.
//
// Dashi resolves defaults when it accepts an agent and records the
// resolved values, so a recorded turn keeps its limits even if the library
// defaults change later.
type Limits struct {
	// ModelAttempts counts settled calls; interrupted calls have their own cap.
	// Zero means 8. It must not be negative.
	ModelAttempts int `json:"model_attempts"`
	// Interruptions bounds how many times a stopped or crashed turn may be
	// resumed. Zero means 10; a negative value allows no resumes.
	Interruptions int `json:"interruptions"`
	// ProviderPauses bounds provider-requested continuations. Zero means 3;
	// a negative value allows none.
	ProviderPauses int `json:"provider_pauses"`
	// ToolConcurrency bounds how many tool calls run at once. Zero means 1.
	// It must not be negative.
	ToolConcurrency int `json:"tool_concurrency"`
	// ToolResultBytes limits the text of one tool result in the projected
	// history. Zero means 64 KiB. A negative value turns the limit off. A
	// value below 1 KiB is raised to 1 KiB, with a warning at Start. Dashi
	// keeps the full result in the record; see ToolFact.Shortened.
	ToolResultBytes int `json:"tool_result_bytes"`
	// NoReservedFinalAnswer turns off the final-answer reserve. By default,
	// the last allowed model attempt, after earlier attempts, asks for an
	// answer without advertising application tools.
	NoReservedFinalAnswer bool `json:"no_reserved_final_answer,omitempty"`
	// Deadline stops the turn at a wall-clock time. Zero means no deadline.
	Deadline time.Time `json:"deadline,omitzero"`
}

// Default turn limits, applied when a Limits field is zero.
const (
	defaultModelAttempts   = 8
	defaultInterruptions   = 10
	defaultProviderPauses  = 3
	defaultToolConcurrency = 1
)

// Validate checks limits and output settings. Every identity field may be
// empty, since the Runner fills them; a zero Agent is valid.
func (a Agent) Validate() error {
	if a.Limits.ModelAttempts < 0 || a.Limits.ToolConcurrency < 0 {
		return errors.New(
			"dashi: model attempts and tool concurrency cannot be negative",
		)
	}
	if len(a.OutputSchema) > 0 {
		if _, err := compileOutputSchema(a.OutputSchema); err != nil {
			return err
		}
		if err := validateOutputToolChoice(a.Settings); err != nil {
			return err
		}
	}
	return nil
}

// resolved returns a with every zero limit replaced by its default and every
// negative "none" count normalized to -1. Dashi records the result, so the
// same agent always compares equal to its record. Runtime checks read -1 as
// zero allowed, through max(count, 0).
func (a Agent) resolved() Agent {
	l := &a.Limits
	l.ModelAttempts = cmp.Or(l.ModelAttempts, defaultModelAttempts)
	l.ToolConcurrency = cmp.Or(l.ToolConcurrency, defaultToolConcurrency)
	l.Interruptions = resolveCount(l.Interruptions, defaultInterruptions)
	l.ProviderPauses = resolveCount(l.ProviderPauses, defaultProviderPauses)
	l.ToolResultBytes = resolveToolResultBytes(l.ToolResultBytes)
	return a
}

// ContextItem carries content whose origin and replacement behavior are
// independent of its message role. All content needed for recovery is recorded.
type ContextItem struct {
	// ID identifies one delivery of this item. A turn records an ID at most
	// once, and a repeat with the same content is skipped, so a source that
	// retries a delivery cannot deliver it twice. Name, by contrast, names a
	// slot that a later snapshot replaces. ID is optional for Deliver and
	// hook items and required for steers.
	ID string `json:"id,omitempty"`
	// Name identifies an item that a later snapshot may replace.
	Name string `json:"name,omitempty"`
	// Origin records who supplied the item; empty means ContextUser.
	Origin  ContextOrigin `json:"origin"`
	Message llm.Message   `json:"message"`
	// Snapshot replaces an earlier item with the same name.
	Snapshot bool `json:"snapshot,omitempty"`
	// NextCallOnly keeps this item until a later model call shows evidence
	// of dispatch (a provider response or rejection), then omits it. A call
	// with no such evidence, such as one recovered after a crash, is resent
	// with the item, so the model may see it twice rather than never. A
	// call an urgent steer cut short does not expire it either: the
	// replacement call sees it again. For a value that must reflect each
	// call, such as the current time, use a named Snapshot item instead.
	NextCallOnly bool `json:"next_call_only,omitempty"`
}

// Turn carries the identified sequence of acknowledged execution facts.
type Turn struct {
	ID    string `json:"id"`
	Steps []Step `json:"steps"`
}

// NewTurn returns an empty turn to start under the caller-assigned id. A
// Start on the zero Turn gets a generated ID instead (see NewID), which the
// outcome's Turn and every step event carry. Choose the ID yourself and save
// it before the call when a retry after a crash must find the same turn.
func NewTurn(id string) Turn {
	return Turn{ID: id}
}

// StepKind identifies the fact recorded by a step.
type StepKind string

const (
	// StepStarted pins the resolved agent and initial input.
	StepStarted StepKind = "turn_started"
	// StepDelivered records context supplied between runs.
	StepDelivered StepKind = "context_delivered"
	// StepContinued records a command that resumes a stopped turn.
	StepContinued StepKind = "turn_continued"
	// StepLoopContinued records progression to another model call.
	StepLoopContinued StepKind = "loop_continued"
	// StepModelIntent records a prepared model call before dispatch.
	StepModelIntent StepKind = "model_requested"
	// StepModelSettled records the result of a model call.
	StepModelSettled StepKind = "model_settled"
	// StepModelReconciled records a host-supplied result for an uncertain call.
	StepModelReconciled StepKind = "model_reconciled"
	// StepToolDecision records the BeforeTool verdict for a call.
	StepToolDecision StepKind = "tool_decision"
	// StepToolStarted records the start of tool execution.
	StepToolStarted StepKind = "tool_started"
	// StepToolWaiting records a tool outcome that awaits later settlement.
	StepToolWaiting StepKind = "tool_waiting"
	// StepToolSettled records a final tool outcome.
	StepToolSettled StepKind = "tool_settled"
	// StepApprovalSettled records a host answer to a deferred local tool
	// decision, supplied by Settle.
	StepApprovalSettled StepKind = "approval_settled"
	// StepHostedApprovalSettled records a host answer to provider-hosted
	// approval, supplied by Settle.
	StepHostedApprovalSettled StepKind = "hosted_approval_settled"
	// StepOutputRecorded pins a validated structured result.
	StepOutputRecorded StepKind = "output_recorded"
	// StepStopped records why a run ended.
	StepStopped StepKind = "turn_stopped"
)

// Step is an acknowledged fact, never a model-facing synthetic message.
// Exactly one payload is present for kinds that require one.
type Step struct {
	Version int    `json:"version"`
	TurnID  string `json:"turn_id"`
	// Seq is the one-based position used by recorder fencing.
	Seq  int64     `json:"seq"`
	At   time.Time `json:"at"`
	Kind StepKind  `json:"kind"`
	// CommandID identifies the host command that produced this step, if any.
	CommandID string `json:"command_id,omitempty"`
	// CommandDigest binds that ID to the command's content for retries.
	CommandDigest string     `json:"command_digest,omitempty"`
	Started       *StartFact `json:"started,omitempty"`
	// Budget on a continuation records the entire raised budget for audit.
	Budget *Budget `json:"budget,omitempty"`
	// ModelAttempts on a continuation records the raised model-attempt cap.
	ModelAttempts  int                         `json:"model_attempts,omitempty"`
	Context        *ContextFact                `json:"context,omitempty"`
	Model          *ModelFact                  `json:"model,omitempty"`
	Tool           *ToolFact                   `json:"tool,omitempty"`
	Approval       *ApprovalFact               `json:"approval,omitempty"`
	HostedApproval *llm.HostedApprovalResponse `json:"hosted_approval,omitempty"`
	Output         *StructuredOutput           `json:"output,omitempty"`
	Stop           *Stop                       `json:"stop,omitempty"`
	// Warnings records nonfatal conditions observed when this step was made.
	Warnings []Warning `json:"warnings,omitempty"`
	// HookResults records the hook answers given since the previous step:
	// every answer that was not "no opinion", and every failure.
	HookResults []HookResult `json:"hook_results,omitempty"`
	// ActiveHooks lists the Runner's hooks on the first step of each command.
	ActiveHooks *ActiveHooks `json:"active_hooks,omitempty"`
}

// Items returns the context items the step delivered: those of a
// Deliver or steer, a loop continuation, or a rejected submission. A durable
// host can read steer IDs from them in the transaction that saves the step.
func (s Step) Items() []ContextItem {
	if s.Context == nil {
		return nil
	}
	return s.Context.Items
}

// WarningCode identifies a nonfatal condition recorded during a turn.
type WarningCode string

const (
	// WarningHistoryMismatch means Continue received different prior history
	// from Start. The turn still runs with the history supplied to Continue,
	// unless Continue.OnHistoryMismatch rejects the command instead.
	WarningHistoryMismatch WarningCode = "history_mismatch"
	// WarningBudgetClamp records an output allowance reduced by a turn cap.
	WarningBudgetClamp WarningCode = "budget_clamp"
	// WarningHookFailed records a hook error or invalid answer that the turn
	// worked around: a skipped BeforeModel hook, or a tool call denied
	// because its BeforeTool hook failed or its rewrites did not converge.
	// The step's HookResults name the hook.
	WarningHookFailed WarningCode = "hook_failed"
	// WarningHooksChanged means a command ran with different Runner hooks
	// from the previous command. Supply the same hooks on every command.
	WarningHooksChanged WarningCode = "hooks_changed"
	// WarningDeliveryDropped means a BeforeFinish hook asked the model to
	// continue when no further model call was possible, so its items were
	// not delivered.
	WarningDeliveryDropped WarningCode = "delivery_dropped"
	// WarningSteering means a steering source failed or returned an invalid
	// steer, or waiting steers could not be delivered because no further
	// model call can happen. The steers stay with their source.
	WarningSteering WarningCode = "steering"
	// WarningToolChoiceCleared means a BeforeModel hook hid the tool a forced
	// tool choice named, or every tool while a tool call was required, so
	// the choice was cleared for that call: the hide wins.
	WarningToolChoiceCleared WarningCode = "tool_choice_cleared"
	// WarningHideIgnored means a BeforeModel hook asked to hide a name that
	// no tool of the call has, or dashi_submit_output, which cannot be
	// hidden. A name that does not match exactly hides nothing. With a
	// dynamic toolset, a tool absent from this call warns as unknown too.
	WarningHideIgnored WarningCode = "hide_ignored"
	// WarningLimitAdjusted means Start raised a limit that was too low to
	// work, so the turn runs with the raised value.
	WarningLimitAdjusted WarningCode = "limit_adjusted"
)

// Warning is a visible, recorded reason to inspect a turn's inputs. It does
// not prevent the turn from continuing.
type Warning struct {
	Code    WarningCode `json:"code"`
	Message string      `json:"message"`
}

// OutputSource identifies the model evidence for a structured result.
type OutputSource string

const (
	// OutputFromTool was submitted through the reserved output tool.
	OutputFromTool OutputSource = "tool_call"
	// OutputFromText was extracted from the final assistant text.
	OutputFromText OutputSource = "text"
)

// StructuredOutput is a schema-validated final value. ToolCallID or AttemptID
// ties it to the model evidence from which it was derived.
type StructuredOutput struct {
	Value      json.RawMessage `json:"value"`
	Source     OutputSource    `json:"source"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	AttemptID  string          `json:"attempt_id"`
}

// StartFact pins the agent, inputs, and optional cap for a turn.
type StartFact struct {
	Agent Agent `json:"agent"`
	// Budget pins the caller's cap at the start of the turn.
	Budget       *Budget         `json:"budget,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Origin       StartOrigin     `json:"origin,omitempty"`
	// HistoryFingerprint identifies the caller-owned prior conversation at
	// Start.
	HistoryFingerprint string        `json:"history_fingerprint,omitempty"`
	Input              []llm.Message `json:"input"`
	// Parent is the tool call that caused this turn, when another turn's
	// tool started it. It never changes after Start.
	Parent *CallRef `json:"parent,omitempty"`
}

// StartOrigin records who supplied the initial work. The host must authorize
// operator and scheduled starts; an absent value means a user start.
type StartOrigin string

const (
	// StartUser identifies user-supplied initial work.
	StartUser StartOrigin = "user"
	// StartScheduled identifies work started by a host schedule.
	StartScheduled StartOrigin = "scheduled"
	// StartOperator identifies work started by an authorized operator.
	StartOperator StartOrigin = "operator"
)

// ContextFact records context delivered to an existing turn.
type ContextFact struct {
	Items []ContextItem `json:"items"`
	// Vetoed marks a loop continuation that records a BeforeFinish veto of
	// the latest answer. Until a new model result arrives, that answer never
	// reaches the host through TurnView.Answer, whatever the stop.
	Vetoed bool `json:"vetoed,omitempty"`
	// Steering marks items that came from a Steering source, recorded on
	// their own step. Inbox.Leftovers uses it to tell steers from Deliver
	// and hook items.
	Steering bool `json:"steering,omitempty"`
}

// ModelFact records intent without persisting the potentially secret-bearing
// wire body. An unresolved intent must be reconciled, not reconstructed from
// this digest and dispatched again.
type ModelFact struct {
	// AttemptID is stable across the intent and its settlement.
	AttemptID string `json:"attempt_id"`
	MessageID string `json:"message_id,omitempty"`
	// RequestDigest identifies the prepared bytes without recording their body.
	RequestDigest string `json:"request_digest,omitempty"`
	// FinalAnswerOnly records that application tools were withheld on this call.
	FinalAnswerOnly bool `json:"final_answer_only,omitempty"`
	// Admission carries each BeforeSend hook's reservation terms, keyed by
	// hook name.
	Admission map[string]json.RawMessage `json:"admission,omitempty"`
	// AtRisk is the pre-send worst case; Charge is the settled accounting value.
	AtRisk *llm.Cost `json:"at_risk,omitempty"`
	// Charge is the settled amount reconstructed into turn spend.
	Charge *llm.Cost `json:"charge,omitempty"`
	// EstimatedInputTokens is the recorded fallback when input usage is absent.
	EstimatedInputTokens int `json:"estimated_input_tokens,omitempty"`
	// EstimatedUsage marks settlements with missing or partial token usage.
	EstimatedUsage   bool                 `json:"estimated_usage,omitempty"`
	Provider         string               `json:"provider,omitempty"`
	API              string               `json:"api,omitempty"`
	Model            string               `json:"model,omitempty"`
	Effective        llm.Settings         `json:"effective,omitzero"`
	Origin           content.Origin       `json:"origin,omitzero"`
	Adjustments      []llm.Adjustment     `json:"adjustments,omitempty"`
	ContentChanges   []llm.ContentChange  `json:"content_changes,omitempty"`
	ModelInfo        *llm.ModelInfo       `json:"model_info,omitempty"`
	CapabilityPolicy llm.CapabilityPolicy `json:"capability_policy"`
	Unverified       []llm.Feature        `json:"unverified,omitempty"`
	Tools            []tool.Definition    `json:"tools,omitempty"`
	// Hidden names the tools BeforeModel hooks removed from this call. A
	// call to one of them settles as not available, by this record.
	Hidden []string    `json:"hidden,omitempty"`
	Result *llm.Result `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
	// ErrorReason is a stable, optional model failure code. It is absent on
	// older records and does not replace the human-readable Error.
	ErrorReason llm.ErrorReason        `json:"error_reason,omitempty"`
	Failure     llm.FailureDisposition `json:"failure,omitempty"`
	Evidence    string                 `json:"evidence,omitempty"`
	// Uncertain means the model effect cannot yet be classified as settled or
	// safely not dispatched.
	Uncertain bool `json:"uncertain,omitempty"`

	// Interrupted, when set, names why the call was cut short. The settlement
	// preserves partial output and usage without making them actionable, and
	// a later model call may safely replace this one.
	Interrupted InterruptCause `json:"interrupted,omitempty"`
	// NoDispatch excludes an intent stopped before the provider call began.
	NoDispatch bool `json:"no_dispatch,omitempty"`
}

// InterruptCause records why a model call was cut short before it produced a
// complete result. The zero value, InterruptNone, means it was not. The cause
// is recorded rather than a flag so replay, accounting, and live consumers can
// tell a crash recovery from a stop the run chose.
type InterruptCause string

const (
	// InterruptNone means the call was not interrupted.
	InterruptNone InterruptCause = ""
	// InterruptWorker settles a call recovered after the worker stopped
	// before its result was recorded (decision 24).
	InterruptWorker InterruptCause = "worker"
	// InterruptRun settles a call the run stopped: its context ended, the
	// turn deadline passed, or a pause arrived before dispatch.
	InterruptRun InterruptCause = "run"
	// InterruptSteering settles a call an urgent steer cut short. Unlike the
	// other causes it does not stop the run: the next call sees the partial
	// output and the steer. It does not count toward ModelAttempts, but it
	// does count toward spend and every Budget cap.
	InterruptSteering InterruptCause = "steering"
)

// valid reports whether c is a cause this version records. Replay rejects
// any other value rather than guessing how to account for it.
func (c InterruptCause) valid() bool {
	switch c {
	case InterruptNone, InterruptWorker, InterruptRun, InterruptSteering:
		return true
	}
	return false
}

// ModelFailure is host-supplied evidence that a pending model attempt did not
// produce a result. The host is responsible for verifying this assertion.
type ModelFailure struct {
	Disposition llm.FailureDisposition
	Reason      string
	Evidence    string
}

// ToolFact records a call's authorization, execution, or settlement evidence.
type ToolFact struct {
	Call       tool.Call       `json:"call"`
	Definition tool.Definition `json:"definition,omitzero"`
	Verdict    ToolVerdict     `json:"verdict,omitzero"`
	// Result is what the model sees instead of Outcome's output. It is set
	// only when an AfterTool hook or Limits.ToolResultBytes changed it;
	// Outcome stays the record of what happened.
	Result *llm.ToolResult `json:"result,omitempty"`
	// Shortened is set, with Result, when the result limit shortened the
	// view. Dashi records it so a later change of the limit does not
	// change an old turn.
	Shortened *Shortened `json:"shortened,omitempty"`
	// Vetoed marks a dashi_submit_output call turned down by a BeforeFinish
	// veto. Until a new model result arrives, the text of the message that
	// made the call never reaches the host through TurnView.Answer.
	Vetoed  bool          `json:"vetoed,omitempty"`
	Outcome *tool.Outcome `json:"outcome,omitempty"`
	Reason  string        `json:"reason,omitempty"`
}

// ApprovalFact answers a deferred tool call. Settle accepts it only while
// the call awaits approval and its arguments are the ones the fact names.
// Pending.Approve and Pending.Deny build it.
type ApprovalFact struct {
	CallID  string `json:"call_id"`
	Approve bool   `json:"approve"`
	// ArgsDigest binds the answer to the call's arguments by JSON meaning,
	// not bytes. Pending.Approve copies it from ToolView.ArgsDigest; a host
	// can compute it with ArgsDigest from the arguments that were shown.
	// Bare-hex digests from before canonical hashing are still accepted,
	// but only while the arguments keep their original bytes.
	ArgsDigest string `json:"args_digest"`
	// Reason explains a denial. The next Continue gives it to the model as
	// the denied call's result.
	Reason string `json:"reason,omitempty"`
}

// StopKind classifies why a turn stopped.
type StopKind string

const (
	// StopCompleted means the turn produced an accepted final result.
	StopCompleted StopKind = "completed"
	// StopWaiting means the turn awaits an external action.
	StopWaiting StopKind = "waiting"
	// StopLimited means a turn limit or budget cap stopped execution.
	StopLimited StopKind = "limited"
	// StopRefused means a hook refused further execution, or the model
	// refused to answer.
	StopRefused StopKind = "refused"
	// StopCanceled means a caller explicitly ended the turn.
	StopCanceled StopKind = "canceled"
	// StopInterrupted leaves the turn available for Continue on any worker.
	StopInterrupted StopKind = "interrupted"
	// StopFailed means execution ended after a known failure.
	StopFailed StopKind = "failed"
	// StopUncertain means an effect requires host reconciliation.
	StopUncertain StopKind = "uncertain"
)

// Stop records a turn's terminal or resumable condition.
type Stop struct {
	Kind   StopKind `json:"kind"`
	Reason string   `json:"reason,omitempty"`
	// RefID names the content reference that caused a stop with one of the
	// StopReasonContent... codes. Otherwise it is empty.
	RefID string `json:"ref_id,omitempty"`
	// Limit enables callers to handle a limited stop without parsing Reason.
	Limit *LimitDetail `json:"limit,omitempty"`
	// Terminal means the turn has ended: no command can run it again,
	// whatever the host settles or raises. It is true for completed and
	// canceled turns; for a limited turn stopped by its deadline or its
	// interruptions limit; and for a turn limited by cost whose spend
	// includes a cost that cannot be priced. A turn stopped at its model
	// attempt limit is not terminal: Continue.ModelAttempts raises the limit.
	// The runner sets it when it records the stop.
	//
	// After a terminal stop the record accepts only late results of work
	// that had already started, and other commands give ErrTurnEnded. A
	// live reader that stops at a terminal stop can miss those results; a
	// reload shows them.
	Terminal bool `json:"terminal,omitempty"`
}

// Stop reasons for a StopFailed turn ended by a tool or a hook. The reason
// is a stable code a host can branch on; the error text stays on the record.
// The tool reasons leave the failing call settled, so once the host has
// fixed the cause, Continue resumes the turn and the model sees the failure
// as the call's result.
const (
	// StopReasonToolFatal means an executor returned an error wrapped with
	// tool.Fatal.
	StopReasonToolFatal = "tool_fatal"

	// StopReasonToolOutcomeInvalid means an executor returned an outcome
	// that fails tool.Outcome.Validate. This is a bug in the executor, not
	// an uncertain effect: the runner observed the result, and it was
	// malformed.
	StopReasonToolOutcomeInvalid = "tool_outcome_invalid"

	// StopReasonHookFailed means a BeforeSend or BeforeFinish hook returned
	// an error or an invalid answer; the step's HookResults name the hook.
	// Fix the cause, then Continue: the turn resumes where it stopped. A
	// failed stop does not count toward Limits.Interruptions.
	StopReasonHookFailed = "hook_failed"

	// StopReasonOutputRejected means a BeforeFinish hook vetoed the final
	// answer with a Reason when no further model call could happen, or
	// vetoed the reserved final answer itself. The
	// rejection is recorded and no output is accepted; Outcome.Answer is
	// empty. Continue after raising the limit that stopped it (such as the
	// Budget) lets the model see the rejection and answer again; with no
	// model attempts left, Continue ends the turn limited.
	StopReasonOutputRejected = "output_rejected"
)

// Stop reasons for a StopFailed turn that a content reference stopped. Stop.RefID
// names the reference. The stop is not terminal. The runner asks the
// ContentResolver again on Continue, so a host can fix the cause (restore the
// bytes, grant access, raise ContentLimits, or set a resolver) and then
// Continue. The stop never holds bytes or the resolver's error text.
const (
	// StopReasonContentNoResolver means the input holds a reference and the
	// Runner has no ContentResolver.
	StopReasonContentNoResolver = "content_no_resolver"

	// StopReasonContentUnavailable means the ContentResolver returned an
	// error for the reference: it is missing, or the turn may not read it.
	StopReasonContentUnavailable = "content_unavailable"

	// StopReasonContentTooLarge means the declared size of the reference, or
	// of all references in one request, is over ContentLimits.
	StopReasonContentTooLarge = "content_too_large"

	// StopReasonContentSizeMismatch means the resolved bytes have a length
	// other than the declared Size.
	StopReasonContentSizeMismatch = "content_size_mismatch"

	// StopReasonContentDigestMismatch means the SHA-256 of the resolved
	// bytes differs from the declared digest.
	StopReasonContentDigestMismatch = "content_digest_mismatch"
)

// Persistence classifies a step write the recorder did not acknowledge.
type Persistence string

const (
	// PersistenceRejected means the recorder confirmed that it did not commit.
	PersistenceRejected Persistence = "rejected"
	// PersistenceUnknown means the caller must load the turn to determine whether
	// the recorder committed the write.
	PersistenceUnknown Persistence = "unknown"
)

// Command carries a CommandID. A repeated ID with identical content
// acknowledges the first application without advancing again. An empty
// CommandID gets a generated one (see NewID), recorded on the command's
// steps. A generated ID cannot recognize a retry, since the retry cannot know
// it: to make a retry after a crash safe, choose the ID, save it before the
// call, and reuse it. Without that, a retry from an old turn snapshot is still
// refused by the recorder, so no model call or tool runs twice.
type Command interface{ commandID() string }

// Start begins a turn with caller-owned prior history and initial input.
type Start struct {
	CommandID string
	Origin    StartOrigin
	// Budget applies to this turn, independently of its agent.
	Budget *Budget
	// OutputSchema overrides the agent's default for this turn. The
	// admitted schema is pinned in the start fact for replay and recovery.
	OutputSchema json.RawMessage
	// History is caller-owned prior conversation and is not stored in the turn.
	History []llm.Message
	Input   []llm.Message
	// Parent links this turn to the tool call of another turn that started
	// it. Dashi records it and shows it in TurnView.Parent; a second Start
	// with a different Parent is a command conflict. Dashi does not check
	// that the call exists.
	Parent *CallRef `json:"Parent,omitempty"`
}

// Deliver adds context to a turn between runs. To reach a turn while it
// runs, pass a Steering source with WithSteering instead. An item with an ID
// already recorded with the same content is skipped; with different content,
// Run returns an error.
type Deliver struct {
	CommandID string
	Items     []ContextItem
}

// Continue resumes a turn using the prior conversation supplied for this
// call. Pass no History when Start had no prior history.
type Continue struct {
	CommandID string
	History   []llm.Message
	// Budget carries the complete raised cap after host authorization.
	Budget *Budget
	// ModelAttempts raises the turn's model-attempt limit to this total. Zero
	// keeps the current limit. It must exceed the limit now in force, which
	// is the agent's Limits.ModelAttempts until an earlier Continue raises
	// it. A turn that stopped at its limit continues only when this raise
	// leaves room for another call. The raise applies to this turn from then
	// on, and the recorded agent does not change.
	ModelAttempts int
	// OnHistoryMismatch controls what happens when History differs from the
	// fingerprint recorded at Start. The default warns and proceeds.
	//
	// It is left out of the command digest because it says how to handle a
	// mismatch, not what the command asks for. A rejected Continue commits
	// nothing, and a retry of an applied one returns the recorded result, so
	// changing it on a retry is not a conflicting command.
	OnHistoryMismatch HistoryMismatch `json:"-"`
}

// Settle records a host-supplied settlement: a final tool outcome, an answer
// to a tool or provider-hosted approval, or a model result or verified
// failure. Exactly one fact is set.
type Settle struct {
	CommandID      string
	CallID         string
	Outcome        *tool.Outcome
	Approval       *ApprovalFact
	HostedApproval *llm.HostedApprovalResponse
	// ModelResult settles a pending model attempt without another dispatch.
	ModelResult *llm.Result
	// ModelFailure supplies verified evidence that an attempt did not execute.
	ModelFailure *ModelFailure
}

// Cancel ends a turn with a caller-supplied reason.
type Cancel struct {
	CommandID string
	Reason    string
}

func (c Start) commandID() string {
	return c.CommandID
}

func (c Deliver) commandID() string {
	return c.CommandID
}

func (c Continue) commandID() string {
	return c.CommandID
}

func (c Settle) commandID() string {
	return c.CommandID
}

func (c Cancel) commandID() string {
	return c.CommandID
}

var (
	// ErrAgentChanged reports a command run with an agent that differs from
	// the turn's recorded one. The wrapping error names the fields.
	ErrAgentChanged = errors.New("dashi: agent changed")
	// ErrCommandConflict reports a reused command ID with different content.
	ErrCommandConflict = errors.New("dashi: command conflict")
	// ErrInvalidTurn reports a turn record that fails replay validation.
	ErrInvalidTurn = errors.New("dashi: invalid turn record")
	// ErrRevisionConflict reports a recorder sequence or owner conflict.
	ErrRevisionConflict = errors.New("dashi: revision conflict")
	// ErrStreamConsumed reports a second attempt to range one stream.
	ErrStreamConsumed = errors.New("dashi: stream already consumed")
	// ErrTurnNotFound reports that a recorder holds no steps for a turn ID.
	ErrTurnNotFound = errors.New("dashi: turn not found")
	// ErrHistoryMismatch rejects a Continue whose History differs from the
	// turn's start when Continue.OnHistoryMismatch is HistoryMismatchReject.
	// Nothing is committed; retry with the history the turn started with.
	ErrHistoryMismatch = errors.New("dashi: history differs from turn start")
	// ErrNotReady rejects a Continue on a turn that cannot run yet. The
	// wrapped message names the calls the turn waits on, or its stop. A
	// turn that can never run again gives ErrTurnEnded instead.
	ErrNotReady = errors.New("dashi: turn is not ready to continue")
	// ErrTurnEnded rejects a command on a turn whose stop is terminal (see
	// Stop.Terminal). Only a Settle reporting the result of work that had
	// already started is accepted after the turn ends.
	ErrTurnEnded = errors.New("dashi: turn has ended")
	// ErrRecordInvalid reports that steps a run recorded fail replay, a bug
	// in the runner or recorder. Unlike a rejected command, something was
	// recorded. Run wraps it together with ErrInvalidTurn.
	ErrRecordInvalid = errors.New("dashi: recorded steps fail replay")
)

func cloneStep(s Step) Step {
	b, e := json.Marshal(s)
	if e != nil {
		panic(fmt.Sprintf("dashi: invalid step: %v", e))
	}
	var out Step
	if e = json.Unmarshal(b, &out); e != nil {
		panic(fmt.Sprintf("dashi: invalid step: %v", e))
	}
	return out
}

func cloneTurn(t Turn) Turn {
	out := Turn{ID: t.ID, Steps: make([]Step, len(t.Steps))}
	for i, s := range t.Steps {
		out.Steps[i] = cloneStep(s)
	}
	return out
}

// cloneTurnSafe copies a turn that may not serialize, such as a caller's
// turn that failed replay. cloneTurn would panic on it, so such a turn is
// copied shallowly: its steps are new values that share nested data.
func cloneTurnSafe(t Turn) Turn {
	if _, err := json.Marshal(t); err != nil {
		return Turn{ID: t.ID, Steps: slices.Clone(t.Steps)}
	}
	return cloneTurn(t)
}

// resolveCount applies the default to a zero count and normalizes any
// negative count to -1, which means none.
func resolveCount(n, def int) int {
	switch {
	case n < 0:
		return -1
	case n == 0:
		return def
	}
	return n
}
