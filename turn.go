// Package kernel runs identified, recordable agent turns over the independent
// llm and tool foundations. Its API is experimental.
package kernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

// Definition is reusable behavior. Runtime clients, credentials, and storage
// are supplied separately to Engine.Apply.
type Definition struct {
	ID string `json:"id"`
	// Revision pins behavior so a continued turn cannot silently change it.
	Revision     string       `json:"revision"`
	Instructions string       `json:"instructions,omitempty"`
	Provider     string       `json:"provider"`
	API          string       `json:"api"`
	Model        string       `json:"model"`
	Settings     llm.Settings `json:"settings"`
	// OutputSchema is the default final-result schema for turns using this
	// definition. Start.OutputSchema may override it for one turn.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Limits       Limits          `json:"limits"`
}

// Limits bounds model calls, interruptions, pauses, tool concurrency, and time
// for turns using a definition.
type Limits struct {
	// ModelAttempts counts settled calls; interrupted calls have their own cap.
	ModelAttempts int `json:"model_attempts"`

	// Interruptions bounds how many times a stopped or crashed turn may be
	// resumed. Zero uses the default to preserve older definitions.
	Interruptions int `json:"interruptions,omitempty"`
	// ProviderPauses bounds provider-requested continuations.
	ProviderPauses  int `json:"provider_pauses"`
	ToolConcurrency int `json:"tool_concurrency"`
	// ReserveFinalAnswer uses the last allowed model attempt for an answer
	// after earlier attempts, without advertising application tools.
	ReserveFinalAnswer bool      `json:"reserve_final_answer,omitempty"`
	Deadline           time.Time `json:"deadline,omitzero"`
}

// DefaultLimits returns the library's turn limits.
func DefaultLimits() Limits {
	return Limits{
		ModelAttempts:      8,
		Interruptions:      10,
		ProviderPauses:     3,
		ToolConcurrency:    1,
		ReserveFinalAnswer: true,
	}
}

func (l Limits) interruptionLimit() int {
	if l.Interruptions == 0 {
		return DefaultLimits().Interruptions
	}
	return l.Interruptions
}

// Validate checks required identity fields, limits, and output settings.
func (d Definition) Validate() error {
	if d.ID == "" ||
		d.Revision == "" ||
		d.Provider == "" ||
		d.API == "" ||
		d.Model == "" {
		return errors.New("kernel: definition requires ID, revision, provider, API, and model")
	}
	if d.Limits.ModelAttempts < 1 || d.Limits.ToolConcurrency < 1 ||
		d.Limits.ProviderPauses < 0 || d.Limits.Interruptions < 0 {
		return errors.New("kernel: invalid limits")
	}
	if len(d.OutputSchema) > 0 {
		if _, err := compileOutputSchema(d.OutputSchema); err != nil {
			return err
		}
		if err := validateOutputToolChoice(d.Settings); err != nil {
			return err
		}
	}
	return nil
}

// ContextItem carries content whose origin and replacement behavior are
// independent of its message role. All content needed for recovery is recorded.
type ContextItem struct {
	// Name identifies an item that a later snapshot may replace.
	Name string `json:"name,omitempty"`
	// Origin records who supplied the item, independently of its message role.
	Origin  string      `json:"origin"`
	Message llm.Message `json:"message"`
	// Snapshot replaces an earlier item with the same name.
	Snapshot bool `json:"snapshot,omitempty"`
	// NextCallOnly omits this item from later model calls.
	NextCallOnly bool `json:"next_call_only,omitempty"`
}

// Turn carries the identified sequence of acknowledged execution facts.
type Turn struct {
	ID    string `json:"id"`
	Steps []Step `json:"steps"`
}

// StructuredOutput returns the validated terminal value and its provenance.
// The boolean is false when this turn has no recorded structured output.
func (t Turn) StructuredOutput() (StructuredOutput, bool, error) {
	view, err := Replay(t)
	if err != nil {
		return StructuredOutput{}, false, err
	}
	if view.Output == nil {
		return StructuredOutput{}, false, nil
	}
	value := *view.Output
	value.Value = append(json.RawMessage(nil), value.Value...)
	return value, true, nil
}

// AnswerText returns text from the latest settled assistant message. A turn
// can complete valid tool-only work and have no answer text.
func (t Turn) AnswerText() string {
	for _, step := range slices.Backward(t.Steps) {
		if (step.Kind != StepModelSettled &&
			step.Kind != StepModelReconciled) || step.Model == nil ||
			step.Model.Result == nil || step.Model.Error != "" ||
			step.Model.Interrupted {
			continue
		}
		var out strings.Builder
		for _, part := range step.Model.Result.Message.Content {
			if part.Kind == llm.ContentBlock &&
				part.Block != nil &&
				part.Block.Text != nil {
				out.WriteString(part.Block.Text.Text)
			}
		}
		return out.String()
	}
	return ""
}

// StepKind identifies the fact recorded by a step.
type StepKind string

const (
	// StepStarted pins the accepted definition and initial input.
	StepStarted StepKind = "turn_started"
	// StepDelivered records context supplied between engine calls.
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
	// StepToolDecision records policy authorization for a call.
	StepToolDecision StepKind = "tool_decision"
	// StepToolStarted records the start of tool execution.
	StepToolStarted StepKind = "tool_started"
	// StepToolWaiting records a tool outcome that awaits later settlement.
	StepToolWaiting StepKind = "tool_waiting"
	// StepToolSettled records a final tool outcome.
	StepToolSettled StepKind = "tool_settled"
	// StepApproval records a host answer to a deferred local tool decision.
	StepApproval StepKind = "approval_accepted"
	// StepHostedApproval records a host answer to provider-hosted approval.
	StepHostedApproval StepKind = "hosted_approval_accepted"
	// StepOutputRecorded pins a validated structured result.
	StepOutputRecorded StepKind = "output_recorded"
	// StepStopped records why engine execution ended.
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
	Budget         *Budget                     `json:"budget,omitempty"`
	Context        *ContextFact                `json:"context,omitempty"`
	Model          *ModelFact                  `json:"model,omitempty"`
	Tool           *ToolFact                   `json:"tool,omitempty"`
	Approval       *ApprovalFact               `json:"approval,omitempty"`
	HostedApproval *llm.HostedApprovalResponse `json:"hosted_approval,omitempty"`
	Output         *StructuredOutput           `json:"output,omitempty"`
	Stop           *Stop                       `json:"stop,omitempty"`
	// Warnings records nonfatal conditions observed when this step was made.
	Warnings []Warning `json:"warnings,omitempty"`
}

// WarningCode identifies a nonfatal condition recorded during a turn.
type WarningCode string

const (
	// WarningHistoryMismatch means Continue received different prior history
	// from Start. The turn still runs with the history supplied to Continue,
	// unless Bindings.OnHistoryMismatch rejects the command instead.
	WarningHistoryMismatch WarningCode = "history_mismatch"
	// WarningBudgetClamp records an output allowance reduced by a turn cap.
	WarningBudgetClamp WarningCode = "budget_clamp"
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

// StartFact pins the definition, inputs, and optional cap for a turn.
type StartFact struct {
	Definition Definition `json:"definition"`
	// Budget pins the caller's cap at the start of the turn.
	Budget       *Budget         `json:"budget,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Origin       StartOrigin     `json:"origin,omitempty"`
	// HistoryFingerprint identifies the caller-owned prior conversation at
	// Start.
	HistoryFingerprint string        `json:"history_fingerprint,omitempty"`
	Input              []llm.Message `json:"input"`
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
	// Admission carries the host's reservation terms from BeforeSend.
	Admission json.RawMessage `json:"admission,omitempty"`
	// AtRisk is the pre-send worst case; Charge is the settled accounting value.
	AtRisk *llm.Cost `json:"at_risk,omitempty"`
	// Charge is the settled amount reconstructed into turn spend.
	Charge *llm.Cost `json:"charge,omitempty"`
	// EstimatedInputTokens is the recorded fallback when input usage is absent.
	EstimatedInputTokens int `json:"estimated_input_tokens,omitempty"`
	// EstimatedUsage marks settlements with missing or partial token usage.
	EstimatedUsage   bool                   `json:"estimated_usage,omitempty"`
	Provider         string                 `json:"provider,omitempty"`
	API              string                 `json:"api,omitempty"`
	Model            string                 `json:"model,omitempty"`
	Effective        llm.Settings           `json:"effective,omitzero"`
	Origin           content.Origin         `json:"origin,omitzero"`
	Adjustments      []llm.Adjustment       `json:"adjustments,omitempty"`
	ContentChanges   []llm.ContentChange    `json:"content_changes,omitempty"`
	ModelInfo        *llm.ModelInfo         `json:"model_info,omitempty"`
	CapabilityPolicy llm.CapabilityPolicy   `json:"capability_policy"`
	Unverified       []llm.Feature          `json:"unverified,omitempty"`
	Tools            []tool.Definition      `json:"tools,omitempty"`
	Result           *llm.Result            `json:"result,omitempty"`
	Error            string                 `json:"error,omitempty"`
	Failure          llm.FailureDisposition `json:"failure,omitempty"`
	Evidence         string                 `json:"evidence,omitempty"`
	// Uncertain means the model effect cannot yet be classified as settled or
	// safely not dispatched.
	Uncertain bool `json:"uncertain,omitempty"`

	// Interrupted preserves partial output and usage without making them
	// actionable. A later model call may safely replace this one.
	Interrupted bool `json:"interrupted,omitempty"`
	// NoDispatch excludes an intent stopped before the provider call began.
	NoDispatch bool `json:"no_dispatch,omitempty"`
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
	Decision   ToolDecision    `json:"decision,omitzero"`
	Outcome    *tool.Outcome   `json:"outcome,omitempty"`
	Reason     string          `json:"reason,omitempty"`
}

// ApprovalFact answers a deferred tool decision. It is accepted only while
// the call awaits approval and its arguments and policy revision are the ones
// the fact names.
type ApprovalFact struct {
	CallID  string `json:"call_id"`
	Approve bool   `json:"approve"`
	// ArgsDigest binds the answer to the call's arguments by JSON meaning,
	// not bytes. Copy it from the pending call's ToolView.ArgsDigest (from
	// Replay), or compute it with ArgsDigest from the arguments that were
	// shown. Bare-hex digests from before canonical hashing are still
	// accepted, but only while the arguments keep their original bytes.
	ArgsDigest     string `json:"args_digest"`
	PolicyRevision string `json:"policy_revision"`
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
	// StopRefused means policy denied further execution.
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
	// Limit enables callers to handle a limited stop without parsing Reason.
	Limit *LimitDetail `json:"limit,omitempty"`
}

// Stop reasons for a StopFailed turn ended by a tool. The reason is a stable
// code a host can branch on; the error text stays on the call's settled step
// in ToolFact.Reason. Both leave the failing call settled, so once the host
// has fixed the cause, Continue resumes the turn and the model sees the
// failure as the call's result.
const (
	// StopReasonToolFatal means an executor returned an error wrapped with
	// tool.Fatal.
	StopReasonToolFatal = "tool_fatal"

	// StopReasonToolOutcomeInvalid means an executor returned an outcome
	// that fails tool.Outcome.Validate. This is a bug in the executor, not
	// an uncertain effect: the engine observed the result, and it was
	// malformed.
	StopReasonToolOutcomeInvalid = "tool_outcome_invalid"
)

// Persistence classifies whether a step write was acknowledged, rejected, or
// left uncertain by the recorder.
type Persistence string

const (
	// PersistenceAcknowledged means the recorder confirmed the write.
	PersistenceAcknowledged Persistence = "acknowledged"
	// PersistenceRejected means the recorder confirmed that it did not commit.
	PersistenceRejected Persistence = "rejected"
	// PersistenceUnknown means the caller must load the turn to determine whether
	// the recorder committed the write.
	PersistenceUnknown Persistence = "unknown"
)

// Outcome combines the recorded turn with its projected stop and pending work.
type Outcome struct {
	Turn   Turn
	Stop   Stop
	Output *StructuredOutput
	// Pending names calls that still need a host action or settlement.
	Pending []string
	// Warnings are recorded, nonfatal conditions visible on replay.
	Warnings []Warning

	// ReadyToContinue is true when Continue needs no budget raise to run.
	ReadyToContinue bool
	// Persistence classifies the last attempted recorder write.
	Persistence Persistence
	// Unacknowledged is the step whose write outcome is unknown or rejected.
	Unacknowledged *Step
	// WriteError is the recorder error associated with Unacknowledged.
	WriteError error
}

// Command carries a caller-assigned stable ID. A repeated ID with identical
// content acknowledges the first application without advancing again.
type Command interface{ commandID() string }

// Start begins a turn with caller-owned prior history and initial input.
type Start struct {
	ID     string
	Origin StartOrigin
	// Budget applies to this turn, independently of its definition.
	Budget *Budget
	// OutputSchema overrides the definition's default for this turn. The
	// admitted schema is pinned in the start fact for replay and recovery.
	OutputSchema json.RawMessage
	// History is caller-owned prior conversation and is not stored in the turn.
	History []llm.Message
	Input   []llm.Message
}

// Deliver adds context to a turn between engine calls.
type Deliver struct {
	ID    string
	Items []ContextItem
}

// Continue resumes a turn using the prior conversation supplied for this
// call. Pass no History when Start had no prior history.
type Continue struct {
	ID      string
	History []llm.Message
	// Budget carries the complete raised cap after host authorization.
	Budget *Budget
}

// Accept records a host-supplied tool, approval, or model settlement.
type Accept struct {
	ID             string
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
	ID     string
	Reason string
}

func (c Start) commandID() string {
	return c.ID
}

func (c Deliver) commandID() string {
	return c.ID
}

func (c Continue) commandID() string {
	return c.ID
}

func (c Accept) commandID() string {
	return c.ID
}

func (c Cancel) commandID() string {
	return c.ID
}

var (
	// ErrDefinitionChanged reports a turn continued with a different definition.
	ErrDefinitionChanged = errors.New("kernel: definition changed")
	// ErrCommandConflict reports a reused command ID with different content.
	ErrCommandConflict = errors.New("kernel: command conflict")
	// ErrInvalidTurn reports a turn record that fails replay validation.
	ErrInvalidTurn = errors.New("kernel: invalid turn record")
	// ErrRevisionConflict reports a recorder sequence or owner conflict.
	ErrRevisionConflict = errors.New("kernel: revision conflict")
	// ErrStreamConsumed reports a second attempt to range one stream.
	ErrStreamConsumed = errors.New("kernel: stream already consumed")
	// ErrTurnNotFound reports that a recorder holds no steps for a turn ID.
	ErrTurnNotFound = errors.New("kernel: turn not found")
	// ErrHistoryMismatch rejects a Continue whose History differs from the
	// turn's start when Bindings.OnHistoryMismatch is HistoryMismatchReject.
	// Nothing is committed; retry with the history the turn started with.
	ErrHistoryMismatch = errors.New("kernel: history differs from turn start")
)

func cloneStep(s Step) Step {
	b, e := json.Marshal(s)
	if e != nil {
		panic(fmt.Sprintf("kernel: invalid step: %v", e))
	}
	var out Step
	if e = json.Unmarshal(b, &out); e != nil {
		panic(fmt.Sprintf("kernel: invalid step: %v", e))
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
