// Package dashi runs identified, recordable agent turns over the independent
// llm and tool foundations. Its API is experimental.
package dashi

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Turn is the saved record of one turn: its ID and the steps recorded so
// far, in order.
type Turn struct {
	ID    string `json:"id"`
	Steps []Step `json:"steps"`
}

// NewTurn returns an empty turn to start under the caller-assigned id. A
// Start on the zero Turn gets a generated ID instead (see NewID), which the
// RunResult's Turn and every step event carry. Choose the ID yourself and save
// it before the call when a retry after a crash must find the same turn.
func NewTurn(id string) Turn {
	return Turn{ID: id}
}

// StepKind says what a step records.
type StepKind string

const (
	// StepTurnStarted records the turn's start: the agent and the initial
	// input.
	StepTurnStarted StepKind = "turn_started"
	// StepMessagesAdded records messages the host added between runs.
	StepMessagesAdded StepKind = "messages_added"
	// StepTurnContinued records a Continue command that resumes a stopped
	// turn.
	StepTurnContinued StepKind = "turn_continued"
	// StepLoopContinued records that the turn goes on to another model call.
	StepLoopContinued StepKind = "loop_continued"
	// StepModelCallStarted records a model call just before it is sent.
	StepModelCallStarted StepKind = "model_call_started"
	// StepContextCompacted records that part of the turn's own messages
	// were replaced by a summary mid-turn.
	StepContextCompacted StepKind = "context_compacted"
	// StepModelCallFinished records the result of a model call.
	StepModelCallFinished StepKind = "model_call_finished"
	// StepModelCallFinishedByHost records a result the host supplied, with
	// Resolve, for a model call whose result was unknown.
	StepModelCallFinishedByHost StepKind = "model_call_finished_by_host"
	// StepToolDecision records the BeforeTool hooks' verdict for a call.
	StepToolDecision StepKind = "tool_decision"
	// StepToolStarted records that a tool call started running.
	StepToolStarted StepKind = "tool_started"
	// StepToolWaiting records that a tool call's result will come later.
	StepToolWaiting StepKind = "tool_waiting"
	// StepToolFinished records a tool call's final outcome.
	StepToolFinished StepKind = "tool_finished"
	// StepApprovalDecided records the host's approve or deny decision, given
	// with Resolve, for a local tool call that needed approval.
	StepApprovalDecided StepKind = "approval_decided"
	// StepProviderToolApprovalDecided records the host's approve or deny
	// decision, given with Resolve, for a provider tool approval request.
	StepProviderToolApprovalDecided StepKind = "provider_tool_approval_decided"
	// StepOutputRecorded records an accepted, schema-validated structured
	// output.
	StepOutputRecorded StepKind = "output_recorded"
	// StepTurnStopped records why a run ended.
	StepTurnStopped StepKind = "turn_stopped"
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
	// CommandHash binds that ID to the command's content for retries.
	CommandHash string     `json:"command_hash,omitempty"`
	Started     *StartFact `json:"started,omitempty"`
	// Budget on a continuation records the entire raised budget for audit.
	Budget *Budget `json:"budget,omitempty"`
	// ModelCalls on a continuation records the raised model call limit.
	ModelCalls           int                               `json:"model_calls,omitempty"`
	Compacted            *ContextCompactionFact            `json:"compacted,omitempty"`
	Context              *ContextFact                      `json:"context,omitempty"`
	Model                *ModelFact                        `json:"model,omitempty"`
	Tool                 *ToolFact                         `json:"tool,omitempty"`
	Approval             *ApprovalDecision                 `json:"approval,omitempty"`
	ProviderToolApproval *llm.ProviderToolApprovalResponse `json:"provider_tool_approval,omitempty"`
	Output               *StructuredOutput                 `json:"output,omitempty"`
	Stop                 *Stop                             `json:"stop,omitempty"`
	// Warnings records nonfatal conditions observed when this step was made.
	Warnings []Warning `json:"warnings,omitempty"`
	// HookResults records the hook decisions made since the previous step:
	// every decision that was not "no opinion", and every failure.
	HookResults []HookResult `json:"hook_results,omitempty"`
	// ActiveHooks lists the Runner's hooks on the first step of each command.
	ActiveHooks *ActiveHooks `json:"active_hooks,omitempty"`
}

// Items returns the context items the step added: those of a Start, an
// AddMessages or steer, a loop continuation, or a rejected submission. A durable
// host can read steer IDs from them in the transaction that saves the step.
func (s Step) Items() []ContextItem {
	if s.Started != nil {
		return s.Started.Items
	}
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
	// WarningHookFailed records a hook error or invalid decision that the turn
	// worked around: a skipped BeforeModel hook, or a tool call denied
	// because its BeforeTool hook failed or its rewrites did not converge.
	// The step's HookResults name the hook.
	WarningHookFailed WarningCode = "hook_failed"
	// WarningHooksChanged means a command ran with different Runner hooks
	// from the previous command. Supply the same hooks on every command.
	WarningHooksChanged WarningCode = "hooks_changed"
	// WarningMessagesDropped means a BeforeFinish hook asked the model to
	// continue when no further model call was possible, so its items were
	// not added.
	WarningMessagesDropped WarningCode = "messages_dropped"
	// WarningSteering means waiting steers could not be added, or an
	// urgent steer could not cut a call short, because no further model
	// call can happen. The steers stay in the inbox.
	WarningSteering WarningCode = "steering"
	// WarningInbox means the inbox failed, returned an invalid steer, or
	// held a command the turn could not take, such as a result for an
	// unknown call or a raise on a turn not stopped at a limit. The
	// command was acknowledged without being applied; a steer stays in the
	// inbox.
	WarningInbox WarningCode = "inbox"
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
	// WarningToolNotOffered means Agent.Tools names a tool that the Runner's
	// Tools did not offer for a model call. The call runs without it. Each
	// name warns once per turn.
	WarningToolNotOffered WarningCode = "tool_not_offered"
	// WarningTaskBudgetAdjusted means the task budget sent differs from the
	// one the Turn would otherwise send: a BeforeModel hook lowered Total
	// below the Remaining it was given, so Remaining was lowered to Total,
	// or Remaining could not be derived after a compaction, so the agent's
	// budget was sent unchanged.
	WarningTaskBudgetAdjusted WarningCode = "task_budget_adjusted"
	// WarningToolDataDropped means the call's outcome was recorded without
	// its Data: the Data was not valid JSON or exceeded 64 KiB, or the model
	// saw a changed result (see tool.Outcome.Data).
	WarningToolDataDropped             WarningCode = "tool_data_dropped"
	WarningActiveCompactionUnavailable WarningCode = "active_compaction_unavailable"
	WarningActiveCompactionBlocked     WarningCode = "active_compaction_blocked"
	// WarningActiveCompactionMediaEstimateUnavailable means this request has
	// prepared media, whose context size cannot be estimated from wire bytes.
	WarningActiveCompactionMediaEstimateUnavailable WarningCode = "active_compaction_media_estimate_unavailable"
	// WarningSummaryFallback means a compaction had no usable summary, so
	// a summary fallback stands in for the history it replaces: shorter,
	// but with less of the earlier work. For a compaction turn, the stop
	// step that ended the turn records it, once. For active compaction
	// within a turn, the context_compacted step that applied the fallback
	// records it. Either way it is in that turn's RunResult and
	// TurnView.Warnings.
	WarningSummaryFallback WarningCode = "summary_fallback"

	// BuildHistory reports the warnings below, which no turn records.
	// Package session passes them on in RunResult.SessionWarnings.

	// WarningTurnUnreadable means a turn failed to load, or its messages
	// could not be built, and it was left out of the history sent to the
	// model.
	WarningTurnUnreadable WarningCode = "turn_unreadable"
	// WarningCompactionMismatch means the turns a compaction turn covers no
	// longer build the history whose hash it recorded. The compaction
	// turn is still honored, so every reader sends the same history.
	WarningCompactionMismatch WarningCode = "compaction_mismatch"
)

// Warning is a visible, recorded reason to inspect a turn's inputs. It does
// not prevent the turn from continuing.
type Warning struct {
	Code    WarningCode `json:"code"`
	Message string      `json:"message"`
	// Hook names the hook of a hook_failed warning, so a host that shows a
	// hook's failures its own way can find them. It is empty when no one
	// hook failed.
	Hook string `json:"hook,omitempty"`
	// TurnID names the turn a BuildHistory warning is about: the turn left
	// out, or the compaction turn that does not match or used its summary
	// fallback. A warning a turn
	// records about itself leaves it empty.
	TurnID string `json:"turn_id,omitempty"`
}

var (
	// ErrAgentChanged reports a retried Start whose agent differs from the
	// turn's recorded one, or a turn snapshot whose recorded agent differs
	// from the recorder's. The wrapping error names the fields or the fix.
	ErrAgentChanged = errors.New("dashi: agent changed")
	// ErrCommandIDReused reports a reused command ID with different content.
	ErrCommandIDReused = errors.New("dashi: command ID reused with different content")
	// ErrInvalidTurn reports a turn record that fails the checks run when
	// its view is built.
	ErrInvalidTurn = errors.New("dashi: invalid turn record")
	// ErrStaleTurn reports a commit made from an out-of-date copy of the
	// turn: another worker added a step first, or holds the turn's lease.
	// Reload the turn.
	ErrStaleTurn = errors.New("dashi: stale turn")
	// ErrStreamConsumed reports a second try to range over one stream.
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
	// ErrTurnEnded rejects a command on a turn that has ended (see
	// Stop.Ended). Only a Resolve reporting the result of a model call that
	// had already started is accepted after the turn ends; it corrects the
	// turn's spend. Tool results are refused.
	ErrTurnEnded = errors.New("dashi: turn has ended")
	// ErrRecordInvalid reports that steps a run recorded fail the checks
	// run when the turn's view is built ("replay"), a bug in the runner or
	// recorder. Unlike a rejected command, something was
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

// Clone returns a deep copy of t, which shares nothing with it. A turn that
// does not encode as JSON, which no recorded turn is, is copied step by
// step instead, and its steps share nested data with t.
func (t Turn) Clone() Turn {
	if _, err := json.Marshal(t); err != nil {
		return Turn{ID: t.ID, Steps: slices.Clone(t.Steps)}
	}
	return cloneTurn(t)
}

// NewID returns a new random UUIDv7 string, such as
// "0199a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b". The first 48 bits are the Unix time
// in milliseconds, so IDs sort by creation time to the millisecond (IDs from
// the same millisecond are in random order), and the string fits a
// Postgres uuid column. The runner uses it for an empty turn ID or CommandID.
// To make retries safe across restarts, call NewID yourself, save the ID
// before the call, and reuse it on retry.
func NewID() string {
	var b [16]byte
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(time.Now().UnixMilli()))
	copy(b[:6], ms[2:])
	_, _ = rand.Read(b[6:])
	b[6] = b[6]&0x0f | 0x70 // version 7
	b[8] = b[8]&0x3f | 0x80 // RFC 9562 variant
	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:], b[10:])
	return string(s[:])
}

// Equal reports whether s and other have the same JSON meaning, the
// comparison Dashi uses for every recorded value (decision 27). Key order,
// whitespace, string escaping, and the spelling of numerically equal numbers
// do not matter, so a step reloaded from a store such as Postgres jsonb
// equals the step that was committed. A step that cannot be encoded equals
// nothing.
//
// A recorder uses Equal to decide whether a commit at an occupied sequence
// is a retry of the recorded step, which succeeds, or a different step from
// a stale worker, which it rejects.
func (s Step) Equal(other Step) bool {
	a, err := canonjson.Marshal(s)
	if err != nil {
		return false
	}
	b, err := canonjson.Marshal(other)
	if err != nil {
		return false
	}
	return bytes.Equal(a, b)
}
