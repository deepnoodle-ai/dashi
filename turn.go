// Package kernel runs identified, recordable agent turns over the independent
// llm and tool foundations. Its API is experimental.
package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
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
	ModelAttempts   int `json:"model_attempts"`
	ProviderPauses  int `json:"provider_pauses"`
	ToolConcurrency int `json:"tool_concurrency"`
	// ReserveFinalAnswer uses the last allowed model attempt for an answer
	// after earlier attempts, without advertising application tools.
	ReserveFinalAnswer bool      `json:"reserve_final_answer,omitempty"`
	Deadline           time.Time `json:"deadline,omitempty"`
}

func DefaultLimits() Limits {
	return Limits{ModelAttempts: 8, ProviderPauses: 3, ToolConcurrency: 1, ReserveFinalAnswer: true}
}

func (d Definition) Validate() error {
	if d.ID == "" || d.Revision == "" || d.Provider == "" || d.API == "" || d.Model == "" {
		return errors.New("kernel: definition requires ID, revision, provider, API, and model")
	}
	if d.Limits.ModelAttempts < 1 || d.Limits.ToolConcurrency < 1 || d.Limits.ProviderPauses < 0 {
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
		if (step.Kind != StepModelSettled && step.Kind != StepModelReconciled) || step.Model == nil || step.Model.Result == nil || step.Model.Error != "" {
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
	Version        int                         `json:"version"`
	TurnID         string                      `json:"turn_id"`
	Seq            int64                       `json:"seq"`
	At             time.Time                   `json:"at"`
	Kind           StepKind                    `json:"kind"`
	CommandID      string                      `json:"command_id,omitempty"`
	CommandDigest  string                      `json:"command_digest,omitempty"`
	Started        *StartFact                  `json:"started,omitempty"`
	Context        *ContextFact                `json:"context,omitempty"`
	Model          *ModelFact                  `json:"model,omitempty"`
	Tool           *ToolFact                   `json:"tool,omitempty"`
	Approval       *ApprovalFact               `json:"approval,omitempty"`
	HostedApproval *llm.HostedApprovalResponse `json:"hosted_approval,omitempty"`
	Output         *StructuredOutput           `json:"output,omitempty"`
	Stop           *Stop                       `json:"stop,omitempty"`
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
	Definition   Definition      `json:"definition"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Origin       StartOrigin     `json:"origin,omitempty"`
	History      []llm.Message   `json:"history,omitempty"`
	Input        []llm.Message   `json:"input"`
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
	AttemptID        string                 `json:"attempt_id"`
	MessageID        string                 `json:"message_id,omitempty"`
	RequestDigest    string                 `json:"request_digest,omitempty"`
	FinalAnswerOnly  bool                   `json:"final_answer_only,omitempty"`
	Admission        json.RawMessage        `json:"admission,omitempty"`
	Provider         string                 `json:"provider,omitempty"`
	API              string                 `json:"api,omitempty"`
	Model            string                 `json:"model,omitempty"`
	Effective        llm.Settings           `json:"effective,omitzero"`
	Origin           content.Origin         `json:"origin,omitzero"`
	Adjustments      []llm.Adjustment       `json:"adjustments,omitempty"`
	ContentChanges   []llm.ContentChange    `json:"content_changes,omitempty"`
	ModelInfo        *llm.ModelInfo         `json:"model_info,omitempty"`
	CapabilityPolicy llm.CapabilityPolicy   `json:"capability_policy,omitempty"`
	Unverified       []llm.Feature          `json:"unverified,omitempty"`
	Tools            []tool.Definition      `json:"tools,omitempty"`
	Result           *llm.Result            `json:"result,omitempty"`
	Error            string                 `json:"error,omitempty"`
	Failure          llm.FailureDisposition `json:"failure,omitempty"`
	Evidence         string                 `json:"evidence,omitempty"`
	Uncertain        bool                   `json:"uncertain,omitempty"`
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

type ApprovalFact struct {
	CallID         string `json:"call_id"`
	Approve        bool   `json:"approve"`
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
	StopFailed    StopKind = "failed"
	StopUncertain StopKind = "uncertain"
)

type Stop struct {
	Kind   StopKind `json:"kind"`
	Reason string   `json:"reason,omitempty"`
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
	Turn           Turn
	Stop           Stop
	Output         *StructuredOutput
	Pending        []string
	Persistence    Persistence
	Unacknowledged *Step
	WriteError     error
}

// Commands carry caller-assigned stable IDs. A repeated ID with identical
// content acknowledges the first application without advancing again.
type Command interface{ commandID() string }
type Start struct {
	ID     string
	Origin StartOrigin
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
type Continue struct{ ID string }
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

func commandDigest(c Command) (string, error) {
	b, e := json.Marshal(c)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

var (
	ErrDefinitionChanged = errors.New("kernel: definition changed")
	ErrCommandConflict   = errors.New("kernel: command conflict")
	ErrInvalidTurn       = errors.New("kernel: invalid turn record")
	ErrRevisionConflict  = errors.New("kernel: revision conflict")
	ErrStreamConsumed    = errors.New("kernel: stream already consumed")
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
