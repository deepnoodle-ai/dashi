// Package kernel runs identified, recordable agent turns over the independent
// llm and tool foundations. Its API is experimental.
package kernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/internal/policyrecord"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

// Definition is reusable behavior. Runtime clients, credentials, and storage
// are supplied separately to Engine.Apply.
type Definition struct {
	ID           string       `json:"id"`
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

type Limits struct {
	ModelAttempts int `json:"model_attempts"`

	// Interruptions bounds how many times a stopped or crashed turn may be
	// resumed. Zero uses the default to preserve older definitions.
	Interruptions   int `json:"interruptions,omitempty"`
	ProviderPauses  int `json:"provider_pauses"`
	ToolConcurrency int `json:"tool_concurrency"`
	// ReserveFinalAnswer uses the last allowed model attempt for an answer
	// after earlier attempts, without advertising application tools.
	ReserveFinalAnswer bool      `json:"reserve_final_answer,omitempty"`
	Deadline           time.Time `json:"deadline,omitempty"`
}

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

func (d Definition) Validate() error {
	if d.ID == "" || d.Revision == "" || d.Provider == "" || d.API == "" || d.Model == "" {
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
	Name         string      `json:"name,omitempty"`
	Origin       string      `json:"origin"`
	Message      llm.Message `json:"message"`
	Snapshot     bool        `json:"snapshot,omitempty"`
	NextCallOnly bool        `json:"next_call_only,omitempty"`
}

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
	for i := len(t.Steps) - 1; i >= 0; i-- {
		step := t.Steps[i]
		if (step.Kind != StepModelSettled &&
			step.Kind != StepModelReconciled) || step.Model == nil ||
			step.Model.Result == nil || step.Model.Error != "" ||
			step.Model.Interrupted {
			continue
		}
		var out strings.Builder
		for _, part := range step.Model.Result.Message.Content {
			if part.Kind == llm.ContentBlock && part.Block != nil && part.Block.Text != nil {
				out.WriteString(part.Block.Text.Text)
			}
		}
		return out.String()
	}
	return ""
}

type StepKind string

const (
	StepStarted         StepKind = "turn_started"
	StepDelivered       StepKind = "context_delivered"
	StepContinued       StepKind = "turn_continued"
	StepLoopContinued   StepKind = "loop_continued"
	StepModelIntent     StepKind = "model_requested"
	StepModelSettled    StepKind = "model_settled"
	StepModelReconciled StepKind = "model_reconciled"
	StepToolDecision    StepKind = "tool_decision"
	StepToolStarted     StepKind = "tool_started"
	StepToolWaiting     StepKind = "tool_waiting"
	StepToolSettled     StepKind = "tool_settled"
	StepApproval        StepKind = "approval_accepted"
	StepHostedApproval  StepKind = "hosted_approval_accepted"
	StepOutputRecorded  StepKind = "output_recorded"
	StepStopped         StepKind = "turn_stopped"
)

// Step is an acknowledged fact, never a model-facing synthetic message.
// Exactly one payload is present for kinds that require one.
type Step struct {
	Version       int        `json:"version"`
	TurnID        string     `json:"turn_id"`
	Seq           int64      `json:"seq"`
	At            time.Time  `json:"at"`
	Kind          StepKind   `json:"kind"`
	CommandID     string     `json:"command_id,omitempty"`
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

type OutputSource string

const (
	OutputFromTool OutputSource = "tool_call"
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

type StartFact struct {
	Definition Definition `json:"definition"`
	// Budget pins the caller's cap at the start of the turn.
	Budget       *Budget         `json:"budget,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Origin       StartOrigin     `json:"origin,omitempty"`
	// HistoryFingerprint identifies the caller-owned prior conversation at
	// Start. An empty value identifies an older turn without a fingerprint.
	HistoryFingerprint string        `json:"history_fingerprint,omitempty"`
	Input              []llm.Message `json:"input"`
}

// StartOrigin records who supplied the initial work. The host must authorize
// operator and scheduled starts; an absent value means a user start.
type StartOrigin string

const (
	StartUser      StartOrigin = "user"
	StartScheduled StartOrigin = "scheduled"
	StartOperator  StartOrigin = "operator"
)

type ContextFact struct {
	Items []ContextItem `json:"items"`
}

// ModelFact records intent without persisting the potentially secret-bearing
// wire body. An unresolved intent must be reconciled, not reconstructed from
// this digest and dispatched again.
type ModelFact struct {
	AttemptID       string          `json:"attempt_id"`
	MessageID       string          `json:"message_id,omitempty"`
	RequestDigest   string          `json:"request_digest,omitempty"`
	FinalAnswerOnly bool            `json:"final_answer_only,omitempty"`
	Admission       json.RawMessage `json:"admission,omitempty"`
	// AtRisk is the pre-send worst case; Charge is the settled accounting value.
	AtRisk *llm.Cost `json:"at_risk,omitempty"`
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
	Uncertain        bool                   `json:"uncertain,omitempty"`

	// Interrupted preserves partial output and usage without making them
	// actionable. A later model call may safely replace this one.
	Interrupted bool `json:"interrupted,omitempty"`
	// NoDispatch excludes an intent stopped before the provider call began.
	NoDispatch bool `json:"no_dispatch,omitempty"`
}

// UnmarshalJSON reads capability_policy, or maps the legacy
// unknown_capabilities field it replaced: "allow" is adjust and "reject" is
// strict. A fact with neither field predates both and decodes as strict,
// because the legacy zero value rejected unknown support; reading it as the
// current default would change what the recorded intent says.
func (f *ModelFact) UnmarshalJSON(data []byte) error {
	type plain ModelFact
	var raw struct {
		plain
		CapabilityPolicy    *llm.CapabilityPolicy `json:"capability_policy"`
		UnknownCapabilities *string               `json:"unknown_capabilities"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	policy, err := policyrecord.Decode(
		raw.CapabilityPolicy, raw.UnknownCapabilities,
	)
	if err != nil {
		return fmt.Errorf("kernel: model fact: %w", err)
	}
	*f = ModelFact(raw.plain)
	f.CapabilityPolicy = policy
	return nil
}

// ModelFailure is host-supplied evidence that a pending model attempt did not
// produce a result. The host is responsible for verifying this assertion.
type ModelFailure struct {
	Disposition llm.FailureDisposition
	Reason      string
	Evidence    string
}

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

type StopKind string

const (
	StopCompleted StopKind = "completed"
	StopWaiting   StopKind = "waiting"
	StopLimited   StopKind = "limited"
	StopRefused   StopKind = "refused"
	StopCanceled  StopKind = "canceled"
	// StopInterrupted leaves the turn available for Continue on any worker.
	StopInterrupted StopKind = "interrupted"
	StopFailed      StopKind = "failed"
	StopUncertain   StopKind = "uncertain"
)

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

type Persistence string

const (
	PersistenceAcknowledged Persistence = "acknowledged"
	PersistenceRejected     Persistence = "rejected"
	PersistenceUnknown      Persistence = "unknown"
)

type Outcome struct {
	Turn    Turn
	Stop    Stop
	Output  *StructuredOutput
	Pending []string
	// Warnings are recorded, nonfatal conditions visible on replay.
	Warnings []Warning

	// ReadyToContinue is true when Continue needs no budget raise to run.
	ReadyToContinue bool
	Persistence     Persistence
	Unacknowledged  *Step
	WriteError      error
}

// Commands carry caller-assigned stable IDs. A repeated ID with identical
// content acknowledges the first application without advancing again.
type Command interface{ commandID() string }
type Start struct {
	ID     string
	Origin StartOrigin
	// Budget applies to this turn, independently of its definition.
	Budget *Budget
	// OutputSchema overrides the definition's default for this turn. The
	// admitted schema is pinned in the start fact for replay and recovery.
	OutputSchema json.RawMessage
	History      []llm.Message
	Input        []llm.Message
}
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
type Accept struct {
	ID             string
	CallID         string
	Outcome        *tool.Outcome
	Approval       *ApprovalFact
	HostedApproval *llm.HostedApprovalResponse
	ModelResult    *llm.Result
	ModelFailure   *ModelFailure
}
type Cancel struct {
	ID     string
	Reason string
}

func (c Start) commandID() string    { return c.ID }
func (c Deliver) commandID() string  { return c.ID }
func (c Continue) commandID() string { return c.ID }
func (c Accept) commandID() string   { return c.ID }
func (c Cancel) commandID() string   { return c.ID }

var (
	ErrDefinitionChanged = errors.New("kernel: definition changed")
	ErrCommandConflict   = errors.New("kernel: command conflict")
	ErrInvalidTurn       = errors.New("kernel: invalid turn record")
	ErrRevisionConflict  = errors.New("kernel: revision conflict")
	ErrStreamConsumed    = errors.New("kernel: stream already consumed")
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
