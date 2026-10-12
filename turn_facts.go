package dashi

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// ArgsHash returns the hash that ties an ApprovalDecision to a tool call's
// arguments. It depends only on the arguments' JSON meaning, so arguments
// shown before a recorder stored them and the same arguments reloaded from a
// store that reorders keys or respells numbers give the same hash. A
// pending call's ToolView.ArgsHash holds this value already.
func ArgsHash(args json.RawMessage) string {
	return canonjson.Hash(args)
}

// CompactionFact is recorded in the start fact of a compaction turn. Once
// the turn has ended, it forms the replacement history: Keep[:SummaryAt], one
// user message, then Keep[SummaryAt:]. When the turn completed with a
// summary, the message is Frame and the summary: the text inside the reply's
// last <summary> and </summary> tags, or the whole reply when it has none.
// When it ended any other way, the message is SummaryFallback, with no
// Frame. This is the one place a turn records earlier messages, so a reader
// with no Compactor builds the same history.
type CompactionFact struct {
	// Through is the last turn covered.
	Through int `json:"through"`
	// Hash is the history hash of the messages this compaction replaces.
	Hash string `json:"hash"`
	// Keep holds the messages the replacement history keeps, which need not
	// be a contiguous tail.
	Keep []llm.Message `json:"keep,omitempty"`
	// SummaryAt is the index in Keep where the summary goes.
	SummaryAt int `json:"summary_at"`
	// Frame is the text that introduces the summary.
	Frame string `json:"frame"`
	// SummaryFallback stands in for the summary when the turn ends without
	// one. It is built with no model call, so the replacement history never
	// depends on the summary call succeeding. Empty means a fixed note.
	SummaryFallback string `json:"summary_fallback,omitempty"`
}

func (f *CompactionFact) clone() *CompactionFact {
	if f == nil {
		return nil
	}
	c := *f
	c.Keep = llmclone.Messages(f.Keep)
	return &c
}

// validateCompaction checks that a compaction fact comes with, and only
// with, origin StartCompaction, and that it is well formed.
func validateCompaction(origin StartOrigin, f *CompactionFact) error {
	switch {
	case f == nil && origin == StartCompaction:
		return errors.New("compaction start requires a compaction fact")
	case f == nil:
		return nil
	case origin != StartCompaction:
		return errors.New("compaction fact requires origin compaction")
	case f.Through < 0 || f.SummaryAt < 0 || f.SummaryAt > len(f.Keep):
		return errors.New("compaction fact is out of range")
	}
	return validateHistory(f.Keep)
}

// OutputSource identifies the model evidence for a structured result.
type OutputSource string

const (
	// OutputSourceToolCall was submitted through the reserved output tool.
	OutputSourceToolCall OutputSource = "tool_call"
	// OutputSourceText was extracted from the final assistant text.
	OutputSourceText OutputSource = "text"
)

// StructuredOutput is a schema-validated final value. ToolCallID or ModelCallID
// ties it to the model evidence from which it was derived.
type StructuredOutput struct {
	Value       json.RawMessage `json:"value"`
	Source      OutputSource    `json:"source"`
	ToolCallID  string          `json:"tool_call_id,omitempty"`
	ModelCallID string          `json:"model_call_id"`
}

// StartFact pins the agent, inputs, and optional cap for a turn.
type StartFact struct {
	Agent                  Agent `json:"agent"`
	ActiveCompactionWindow int   `json:"active_compaction_window,omitempty"`
	// Budget pins the caller's cap at the start of the turn.
	Budget       *Budget         `json:"budget,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Origin       StartOrigin     `json:"origin,omitempty"`
	// HistoryHash identifies the caller-owned prior conversation at
	// Start.
	HistoryHash string        `json:"history_hash,omitempty"`
	Input       []llm.Message `json:"input"`
	// Items are the Start's context items, which follow Input.
	Items []ContextItem `json:"items,omitempty"`
	// Parent is the tool call that caused this turn, when another turn's
	// tool started it. It never changes after Start.
	Parent *CallRef `json:"parent,omitempty"`
	// Compaction is set on a compaction turn, and only there.
	Compaction *CompactionFact `json:"compaction,omitempty"`
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
	// StartCompaction identifies a compaction turn, which summarizes the
	// conversation history the model sees. Its start records a
	// CompactionFact.
	StartCompaction StartOrigin = "compaction"
)

// ContextFact records context items added to an existing turn.
type ContextFact struct {
	Items []ContextItem `json:"items"`
	// Vetoed marks a loop continuation that records a BeforeFinish veto of
	// the latest reply. Until a new model result arrives, that reply never
	// reaches the host through TurnView.Reply, whatever the stop.
	Vetoed bool `json:"vetoed,omitempty"`
	// Steering marks items that came from steers in an Inbox, recorded on
	// their own step. MemoryInbox.Leftovers uses it to tell steers from AddMessages
	// and hook items.
	Steering bool `json:"steering,omitempty"`
}

// ModelFact records a model call: what was about to be sent when the call
// started, and later how it finished. It never stores the request body,
// which may hold secrets. A started call with no recorded finish must be
// resolved by the host, not rebuilt from RequestHash and sent again.
type ModelFact struct {
	Purpose    ModelPurpose           `json:"purpose,omitempty"`
	Compaction *ContextCompactionPlan `json:"compaction,omitempty"`
	// ModelCallID is the same on the call's start step and its finish step.
	ModelCallID string `json:"model_call_id"`
	MessageID   string `json:"message_id,omitempty"`
	// RequestHash identifies the prepared bytes without recording their body.
	RequestHash string `json:"request_hash,omitempty"`
	// FinalReplyOnly records that application tools were withheld on this call.
	FinalReplyOnly bool `json:"final_reply_only,omitempty"`
	// Reservation carries each BeforeSend hook's reservation terms, keyed by
	// hook name.
	Reservation map[string]json.RawMessage `json:"reservation,omitempty"`
	// WorstCaseCost is the most the call could cost for its estimated input,
	// computed before it is sent. What the call counts toward Spend is
	// derived from its result (ModelCallCostOf), never stored.
	WorstCaseCost *llm.Cost `json:"worst_case_cost,omitempty"`
	// EstimatedInputTokens is llm.EstimatedInputTokens of the prepared
	// request: what WorstCaseCost priced and the base of the next call's
	// estimate. It is never charged.
	EstimatedInputTokens int                  `json:"estimated_input_tokens,omitempty"`
	Provider             string               `json:"provider,omitempty"`
	API                  string               `json:"api,omitempty"`
	Model                string               `json:"model,omitempty"`
	Effective            llm.Settings         `json:"effective,omitzero"`
	Endpoint             content.Origin       `json:"endpoint,omitzero"`
	Adjustments          []llm.Adjustment     `json:"adjustments,omitempty"`
	ContentChanges       []llm.ContentChange  `json:"content_changes,omitempty"`
	ModelInfo            *llm.ModelInfo       `json:"model_info,omitempty"`
	CapabilityPolicy     llm.CapabilityPolicy `json:"capability_policy"`
	Unverified           []llm.Feature        `json:"unverified,omitempty"`
	Tools                []tool.Definition    `json:"tools,omitempty"`
	// Hidden names the tools BeforeModel hooks removed from this call. A
	// call to one of them finishes as not available, by this record.
	Hidden []string    `json:"hidden,omitempty"`
	Result *llm.Result `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
	// ErrorReason is a stable, optional model failure code. It is absent on
	// older records and does not replace the human-readable Error.
	ErrorReason llm.ErrorReason `json:"error_reason,omitempty"`
	// ErrorCategory, HTTPStatus, Retryable, and RetryAfter copy the
	// classified llm.Error a failed call returned (Category, Status,
	// Retryable, RetryAfter), so a host can schedule a retry without parsing
	// Error. They are empty when the call did not fail with an llm.Error.
	// Retryable is the provider's hint about this error only, not whether
	// the turn can continue, and never permission to resend a call whose
	// result is unknown. A failure the host supplies with Resolve has none
	// of these fields.
	ErrorCategory llm.ErrorCategory `json:"error_category,omitempty"`
	// HTTPStatus is the provider's HTTP status, or zero with no response.
	HTTPStatus int  `json:"http_status,omitempty"`
	Retryable  bool `json:"retryable,omitempty"`
	// RetryAfter is the delay the provider asked for, counted from the
	// step's At. JSON encodes it in nanoseconds.
	RetryAfter time.Duration  `json:"retry_after,omitempty"`
	SendStatus llm.SendStatus `json:"send_status,omitempty"`
	Evidence   string         `json:"evidence,omitempty"`
	// ResultUnknown means it is not yet known whether the provider ran the
	// call: it may have run, or it may safely never have been sent.
	ResultUnknown bool `json:"result_unknown,omitempty"`

	// Interrupted, when set, names why the call was cut short. The finish
	// step keeps the partial output and usage without acting on them, and
	// a later model call may safely replace this one.
	Interrupted InterruptCause `json:"interrupted,omitempty"`
	// NotSent marks a started call that stopped before the request went to
	// the provider.
	NotSent bool `json:"not_sent,omitempty"`
}

// InterruptCause records why a model call was cut short before it produced a
// complete result. The zero value, InterruptNone, means it was not. The cause
// is recorded rather than a flag so View, cost accounting, and live readers
// can tell a crash recovery from a stop the run chose.
type InterruptCause string

const (
	// InterruptNone means the call was not interrupted.
	InterruptNone InterruptCause = ""
	// InterruptWorker finishes a call recovered after the worker stopped
	// before its result was recorded. With no reported usage it counts
	// zero (decision 47).
	InterruptWorker InterruptCause = "worker"
	// InterruptRun finishes a call the run stopped: its context ended, the
	// turn deadline passed, or a pause arrived before the call was sent.
	InterruptRun InterruptCause = "run"
	// InterruptSteering finishes a call an urgent steer cut short. Unlike the
	// other causes it does not stop the run: the next call sees the partial
	// output and the steer. It does not count toward ModelCalls, but it
	// does count toward spend and every Budget cap.
	InterruptSteering InterruptCause = "steering"
)

// valid reports whether c is a cause this version records. View rejects
// any other value rather than guessing how to account for it.
func (c InterruptCause) valid() bool {
	switch c {
	case InterruptNone, InterruptWorker, InterruptRun, InterruptSteering:
		return true
	}
	return false
}

// ModelFailure is host-supplied evidence that a pending model call did not
// produce a result. The host is responsible for checking that this is true.
type ModelFailure struct {
	SendStatus llm.SendStatus `json:"send_status"`
	Reason     string         `json:"reason"`
	Evidence   string         `json:"evidence"`
}

// ToolFact records a tool call's approval, its start, or its result.
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
	// made the call never reaches the host through TurnView.Reply.
	Vetoed  bool          `json:"vetoed,omitempty"`
	Outcome *tool.Outcome `json:"outcome,omitempty"`
	Reason  string        `json:"reason,omitempty"`
}

// ApprovalDecision approves or denies a tool call that needs approval.
// Resolve accepts it only while the call awaits approval and its arguments
// are the ones ArgsHash names.
// Pending.Approve and Pending.Deny build it.
type ApprovalDecision struct {
	CallID  string `json:"call_id"`
	Approve bool   `json:"approve"`
	// ArgsHash ties the decision to the call's arguments by JSON meaning,
	// not bytes. Pending.Approve copies it from ToolView.ArgsHash; a host
	// can compute it with ArgsHash from the arguments that were shown.
	// Bare-hex hashes from before canonical hashing are still accepted,
	// but only while the arguments keep their original bytes.
	ArgsHash string `json:"args_hash"`
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
	// refused to reply.
	StopRefused StopKind = "refused"
	// StopCanceled means a caller explicitly ended the turn.
	StopCanceled StopKind = "canceled"
	// StopInterrupted leaves the turn available for Continue on any worker.
	StopInterrupted StopKind = "interrupted"
	// StopFailed means execution ended after a known failure.
	StopFailed StopKind = "failed"
	// StopResultUnknown means a model or tool call may have run, but its
	// result is unknown; the host must resolve it.
	StopResultUnknown StopKind = "result_unknown"
)

// Stop records why a turn stopped, and whether it has ended or can go on.
type Stop struct {
	Kind   StopKind `json:"kind"`
	Reason string   `json:"reason,omitempty"`
	// RefID names the content reference that caused a stop with one of the
	// StopReasonContent... codes. Otherwise it is empty.
	RefID string `json:"ref_id,omitempty"`
	// Limit enables callers to handle a limited stop without parsing Reason.
	Limit *LimitDetail `json:"limit,omitempty"`
	// Ended means the turn has ended: no command can run it again,
	// whatever the host resolves or raises. It is true for completed and
	// canceled turns; for a limited turn stopped by its deadline, its
	// resumes limit, or its context window; for a host limit with
	// LimitDetail.Ends; for a model-call, cost, or token limit when the
	// agent sets Limits.NoRaise; and for a turn limited by cost whose spend
	// includes a cost that cannot be priced, or a token-capped turn whose
	// usage totals are incomplete. Otherwise a turn stopped at its model
	// call limit has not ended: Continue.ModelCalls raises the limit.
	// The runner sets it when it records the stop.
	//
	// After the stop that ended the turn, the conversation is closed: the
	// record accepts only late results of model calls that had already
	// started, which correct Spend, and other commands, tool results
	// included, give ErrTurnEnded. A live reader that stops at the ending stop can miss
	// those results; a reload shows them. TurnMessagesFull ignores them: an
	// ended turn's messages are fixed at its ending stop, and so is
	// TurnView.Reply.
	Ended bool `json:"ended,omitempty"`
	// Inferred means the record holds no stop after the latest Start or
	// Continue: the run ended without recording one, or is still going.
	// View and RunResult then infer Kind from the work in flight, and Kind
	// is empty when none is. A recorded stop is never inferred. A hook's
	// view of a live run has an empty Stop that is not inferred.
	Inferred bool `json:"inferred,omitempty"`
}

// Stop reasons for a StopFailed turn ended by a tool or a hook. The reason
// is a stable code a host can branch on; the error text stays on the record.
// The tool reasons leave the failing call finished, so once the host has
// fixed the cause, Continue resumes the turn and the model sees the failure
// as the call's result.
const (
	// StopReasonToolFatal means an executor returned an error wrapped with
	// tool.Fatal.
	StopReasonToolFatal        = "tool_fatal"
	StopReasonCompactionFailed = "compaction_failed"

	// StopReasonToolOutcomeInvalid means an executor returned an outcome
	// that fails tool.Outcome.Validate. This is a bug in the executor, not
	// an unknown result: the runner saw the result, and it was malformed.
	StopReasonToolOutcomeInvalid = "tool_outcome_invalid"

	// StopReasonHookFailed means a BeforeSend or BeforeFinish hook returned
	// an error or an invalid decision; the step's HookResults name the hook.
	// Fix the cause, then Continue: the turn resumes where it stopped. A
	// failed stop does not count toward Limits.Resumes.
	StopReasonHookFailed = "hook_failed"

	// StopReasonOutputRejected means a BeforeFinish hook vetoed the final
	// reply with a Reason when no further model call could happen, or
	// vetoed the reserved final reply itself. The
	// rejection is recorded and no output is accepted; RunResult.Reply is
	// empty. Continue after raising the limit that stopped it (such as the
	// Budget) lets the model see the rejection and reply again; with no
	// model calls left, Continue ends the turn limited.
	StopReasonOutputRejected = "output_rejected"

	// StopReasonEmptyResponse means the model's final reply had no text and
	// no BeforeFinish hook accepted an empty reply.
	StopReasonEmptyResponse = "empty_response"

	// StopReasonIncompleteModelResult means the provider reported a stop
	// that Dashi does not recognize, so the result may be cut short.
	StopReasonIncompleteModelResult = "incomplete_model_result"
)

// Stop reasons for a StopFailed turn that a content reference stopped. Stop.RefID
// names the reference. The stop does not end the turn. The runner asks the
// ContentLoader again on Continue, so a host can fix the cause (restore the
// bytes, grant access, raise ContentLimits, or set a loader) and then
// Continue. The stop never holds bytes or the loader's error text.
const (
	// StopReasonContentNoLoader means the input holds a reference and the
	// Runner has no ContentLoader.
	StopReasonContentNoLoader = "content_no_loader"

	// StopReasonContentUnavailable means the ContentLoader returned an
	// error for the reference: it is missing, or the turn may not read it.
	StopReasonContentUnavailable = "content_unavailable"

	// StopReasonContentTooLarge means the declared size of the reference, or
	// of all references in one request, is over ContentLimits.
	StopReasonContentTooLarge = "content_too_large"

	// StopReasonContentSizeMismatch means the loaded bytes have a length
	// other than the declared Size.
	StopReasonContentSizeMismatch = "content_size_mismatch"

	// StopReasonContentHashMismatch means the SHA-256 of the loaded
	// bytes differs from the declared hash.
	StopReasonContentHashMismatch = "content_hash_mismatch"
)
