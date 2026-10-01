package dashi

import (
	"encoding/json"
	"errors"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// ArgsDigest returns the digest that binds an ApprovalDecision to a tool call's
// arguments. It depends only on the arguments' JSON meaning, so arguments
// shown before a recorder stored them and the same arguments reloaded from a
// store that reorders keys or respells numbers give the same digest. A
// pending call's ToolView.ArgsDigest holds this value already.
func ArgsDigest(args json.RawMessage) string {
	return canonjson.Digest(args)
}

// CompactionFact is recorded in the start fact of a compaction turn. With
// the turn's answer it forms the replacement history: Keep[:SummaryAt], a
// user message of Frame and the summary, then Keep[SummaryAt:]. The
// summary is the text inside the answer's last <summary> and </summary>
// tags, or the whole answer when it has none. This is the
// one place a turn records earlier messages, so a reader with no Compactor
// builds the same history.
type CompactionFact struct {
	// Through is the last turn covered.
	Through int `json:"through"`
	// Digest is the history fingerprint of the projected history replaced.
	Digest string `json:"digest"`
	// Keep holds the messages the replacement history keeps, which need not
	// be a contiguous tail.
	Keep []llm.Message `json:"keep,omitempty"`
	// SummaryAt is the index in Keep where the summary goes.
	SummaryAt int `json:"summary_at"`
	// Frame is the text that introduces the summary.
	Frame string `json:"frame"`
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
	// OutputFromTool was submitted through the reserved output tool.
	OutputFromTool OutputSource = "tool_call"
	// OutputFromText was extracted from the final assistant text.
	OutputFromText OutputSource = "text"
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
	// HistoryFingerprint identifies the caller-owned prior conversation at
	// Start.
	HistoryFingerprint string        `json:"history_fingerprint,omitempty"`
	Input              []llm.Message `json:"input"`
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
	// StartCompaction identifies a compaction turn, which
	// summarizes the projected history. Its start records a CompactionFact.
	StartCompaction StartOrigin = "compaction"
)

// ContextFact records context delivered to an existing turn.
type ContextFact struct {
	Items []ContextItem `json:"items"`
	// Vetoed marks a loop continuation that records a BeforeFinish veto of
	// the latest answer. Until a new model result arrives, that answer never
	// reaches the host through TurnView.Reply, whatever the stop.
	Vetoed bool `json:"vetoed,omitempty"`
	// Steering marks items that came from a Steering source, recorded on
	// their own step. Inbox.Leftovers uses it to tell steers from AddMessages
	// and hook items.
	Steering bool `json:"steering,omitempty"`
}

// ModelFact records intent without persisting the potentially secret-bearing
// wire body. An unresolved intent must be reconciled, not reconstructed from
// this digest and dispatched again.
type ModelFact struct {
	Purpose    ModelPurpose           `json:"purpose,omitempty"`
	Compaction *ContextCompactionPlan `json:"compaction,omitempty"`
	// ModelCallID is stable across the intent and its settlement.
	ModelCallID string `json:"model_call_id"`
	MessageID   string `json:"message_id,omitempty"`
	// RequestDigest identifies the prepared bytes without recording their body.
	RequestDigest string `json:"request_digest,omitempty"`
	// FinalReplyOnly records that application tools were withheld on this call.
	FinalReplyOnly bool `json:"final_reply_only,omitempty"`
	// Reservation carries each BeforeSend hook's reservation terms, keyed by
	// hook name.
	Reservation map[string]json.RawMessage `json:"reservation,omitempty"`
	// WorstCaseCost is the pre-send worst case; Charge is the settled accounting value.
	WorstCaseCost *llm.Cost `json:"worst_case_cost,omitempty"`
	// Cost is the settled amount reconstructed into turn spend.
	Cost *llm.Cost `json:"cost,omitempty"`
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
	ErrorReason llm.ErrorReason `json:"error_reason,omitempty"`
	SendStatus  llm.SendStatus  `json:"send_status,omitempty"`
	Evidence    string          `json:"evidence,omitempty"`
	// ResultUnknown means the model effect cannot yet be classified as settled or
	// safely not dispatched.
	ResultUnknown bool `json:"result_unknown,omitempty"`

	// Interrupted, when set, names why the call was cut short. The settlement
	// preserves partial output and usage without making them actionable, and
	// a later model call may safely replace this one.
	Interrupted InterruptCause `json:"interrupted,omitempty"`
	// NotSent excludes an intent stopped before the provider call began.
	NotSent bool `json:"not_sent,omitempty"`
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
	// output and the steer. It does not count toward ModelCalls, but it
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
	SendStatus llm.SendStatus `json:"send_status"`
	Reason     string         `json:"reason"`
	Evidence   string         `json:"evidence"`
}

// ToolFact records a call's authorization, execution, or settlement evidence.
type ToolFact struct {
	Call       tool.Call       `json:"call"`
	Definition tool.Definition `json:"definition,omitzero"`
	Verdict    ToolVerdict     `json:"verdict,omitzero"`
	// Result is what the model sees instead of RunResult's output. It is set
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

// ApprovalDecision answers a deferred tool call. Resolve accepts it only while
// the call awaits approval and its arguments are the ones the fact names.
// Pending.Approve and Pending.Deny build it.
type ApprovalDecision struct {
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
	// StopResultUnknown means an effect requires host reconciliation.
	StopResultUnknown StopKind = "result_unknown"
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
	// Ended means the turn has ended: no command can run it again,
	// whatever the host settles or raises. It is true for completed and
	// canceled turns; for a limited turn stopped by its deadline or its
	// interruptions limit; and for a turn limited by cost whose spend
	// includes a cost that cannot be priced; or a token-capped turn whose
	// usage aggregation is incomplete. A turn stopped at its model
	// attempt limit is not terminal: Continue.ModelCalls raises the limit.
	// The runner sets it when it records the stop.
	//
	// After a terminal stop the record accepts only late results of work
	// that had already started, and other commands give ErrTurnEnded. A
	// live reader that stops at a terminal stop can miss those results; a
	// reload shows them. Project ignores them: an ended turn's projection
	// is fixed at its terminal stop, and so is TurnView.Reply.
	Ended bool `json:"ended,omitempty"`
	// Inferred means the record holds no stop after the latest Start or
	// Continue: the run ended without recording one, or is still going.
	// Replay and Outcome then infer Kind from the work in flight, and Kind
	// is empty when none is. A recorded stop is never projected. A hook's
	// view of a live run has an empty Stop that is not projected.
	Inferred bool `json:"inferred,omitempty"`
}

// Stop reasons for a StopFailed turn ended by a tool or a hook. The reason
// is a stable code a host can branch on; the error text stays on the record.
// The tool reasons leave the failing call settled, so once the host has
// fixed the cause, Continue resumes the turn and the model sees the failure
// as the call's result.
const (
	// StopReasonToolFatal means an executor returned an error wrapped with
	// tool.Fatal.
	StopReasonToolFatal        = "tool_fatal"
	StopReasonCompactionFailed = "compaction_failed"

	// StopReasonToolOutcomeInvalid means an executor returned an outcome
	// that fails tool.Outcome.Validate. This is a bug in the executor, not
	// an uncertain effect: the runner observed the result, and it was
	// malformed.
	StopReasonToolOutcomeInvalid = "tool_outcome_invalid"

	// StopReasonHookFailed means a BeforeSend or BeforeFinish hook returned
	// an error or an invalid answer; the step's HookResults name the hook.
	// Fix the cause, then Continue: the turn resumes where it stopped. A
	// failed stop does not count toward Limits.Resumes.
	StopReasonHookFailed = "hook_failed"

	// StopReasonOutputRejected means a BeforeFinish hook vetoed the final
	// answer with a Reason when no further model call could happen, or
	// vetoed the reserved final answer itself. The
	// rejection is recorded and no output is accepted; RunResult.Reply is
	// empty. Continue after raising the limit that stopped it (such as the
	// Budget) lets the model see the rejection and answer again; with no
	// model attempts left, Continue ends the turn limited.
	StopReasonOutputRejected = "output_rejected"

	// StopReasonEmptyResponse means the model's final reply had no text and
	// no BeforeFinish hook accepted an empty reply.
	StopReasonEmptyResponse = "empty_response"

	// StopReasonIncompleteModelResult means the provider reported a stop
	// that Dashi does not recognize, so the result may be cut short.
	StopReasonIncompleteModelResult = "incomplete_model_result"
)

// Stop reasons for a StopFailed turn that a content reference stopped. Stop.RefID
// names the reference. The stop is not terminal. The runner asks the
// ContentLoader again on Continue, so a host can fix the cause (restore the
// bytes, grant access, raise ContentLimits, or set a resolver) and then
// Continue. The stop never holds bytes or the resolver's error text.
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
