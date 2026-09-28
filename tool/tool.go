// Package tool describes tools and execution independently of model calls or
// agent state. Typed tool construction is adapted from Dive (65c393f),
// Apache-2.0, with explicit outcomes and construction-time schema validation.
package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/deepnoodle-ai/kernel/content"
)

// Kind identifies where a tool runs. The empty Kind is accepted and leaves the
// location unspecified.
type Kind string

// Tool kinds.
const (
	// KindLocal identifies a tool with an application-provided executor.
	KindLocal Kind = "local"
	// KindExternal identifies a tool whose result is supplied externally.
	KindExternal Kind = "external"
	// KindProviderHosted identifies a tool executed by the model service.
	KindProviderHosted Kind = "provider_hosted"
)

// Annotations are advisory. Nil distinguishes an unspecified hint from false.
type Annotations struct {
	// ReadOnly hints that execution does not modify external state; nil means
	// unknown.
	ReadOnly *bool `json:"read_only,omitempty"`

	// Destructive hints that execution can destroy or overwrite data; nil means
	// unknown.
	Destructive *bool `json:"destructive,omitempty"`

	// Idempotent hints that repeated execution has the same effect; it does not
	// deduplicate calls.
	Idempotent *bool `json:"idempotent,omitempty"`

	// OpenWorld hints that the tool interacts with an open set of external
	// entities.
	OpenWorld *bool `json:"open_world,omitempty"`
}

// Definition describes a tool to a model: its name, purpose, and input schema.
// Schema stays raw JSON so it reaches providers unchanged.
type Definition struct {
	// Name is the nonempty identifier the model uses to request this tool.
	Name string `json:"name"`

	// Description explains the operation to the model.
	Description string `json:"description,omitempty"`

	// Schema is a JSON object or boolean schema, preserved through provider
	// conversion.
	Schema json.RawMessage `json:"schema"`

	// Hosted selects a provider-owned tool configuration. Its wire options
	// are validated again by the destination adapter before dispatch.
	Hosted *HostedSpec `json:"hosted,omitempty"`

	// Kind identifies the execution location; empty leaves it unspecified.
	Kind Kind `json:"kind,omitempty"`

	// Annotations supply advisory hints rather than execution guarantees.
	Annotations Annotations `json:"annotations,omitempty"`
}

// HostedSpec binds a server-executed tool to one provider and API. Options
// contain non-secret provider fields; credentials belong on the client.
type HostedSpec struct {
	// Provider identifies the configured destination that owns this tool.
	Provider string `json:"provider"`
	// API prevents the same provider name from crossing wire formats.
	API string `json:"api"`
	// Type is the provider's wire tool version or kind.
	Type string `json:"type"`
	// Options contain non-secret provider fields and are owned after Clone.
	Options json.RawMessage `json:"options,omitempty"`
}

// Validate requires a bound wire type and an optional JSON object. Adapters
// impose the provider's narrower option vocabulary.
func (s HostedSpec) Validate() error {
	if s.Provider == "" || s.API == "" || s.Type == "" {
		return errors.New("tool: hosted tool requires provider, API, and type")
	}
	if len(s.Options) == 0 {
		return nil
	}
	var options map[string]json.RawMessage
	if err := json.Unmarshal(s.Options, &options); err != nil || options == nil {
		return errors.New("tool: hosted options require a JSON object")
	}
	return nil
}

// Clone detaches hosted wire options from the caller's buffer.
func (s HostedSpec) Clone() HostedSpec {
	s.Options = slices.Clone(s.Options)
	return s
}

// Validate reports whether d has a name, a known or empty Kind, and a schema
// that is a JSON object or boolean.
func (d Definition) Validate() error {
	if d.Name == "" {
		return errors.New("tool: name is required")
	}
	if d.Kind != "" && d.Kind != KindLocal && d.Kind != KindExternal && d.Kind != KindProviderHosted {
		return fmt.Errorf("tool: invalid kind %q", d.Kind)
	}
	if d.Kind == KindProviderHosted {
		// A missing RawMessage is written as JSON null in saved turns.
		// Treat it as absent only for hosted tools, which have no schema.
		hasSchema := len(d.Schema) != 0 &&
			!bytes.Equal(bytes.TrimSpace(d.Schema), []byte("null"))
		if hasSchema || d.Hosted == nil {
			return errors.New(
				"tool: hosted definition requires hosted options without schema",
			)
		}
		return d.Hosted.Validate()
	}
	if d.Hosted != nil {
		return errors.New("tool: local or external definition cannot carry hosted options")
	}
	var schema any
	if err := json.Unmarshal(d.Schema, &schema); err != nil {
		return fmt.Errorf("tool: %q: invalid JSON schema: %w", d.Name, err)
	}
	switch schema.(type) {
	case map[string]any, bool:
		return nil
	}
	return fmt.Errorf("tool: %q: schema must be a JSON object or boolean", d.Name)
}

// Clone returns a copy of d that shares no schema bytes or annotation pointers
// with the original.
func (d Definition) Clone() Definition {
	d.Schema = slices.Clone(d.Schema)
	if d.Hosted != nil {
		v := d.Hosted.Clone()
		d.Hosted = &v
	}
	d.Annotations = Annotations{
		ReadOnly:    cloneBool(d.Annotations.ReadOnly),
		Destructive: cloneBool(d.Annotations.Destructive),
		Idempotent:  cloneBool(d.Annotations.Idempotent),
		OpenWorld:   cloneBool(d.Annotations.OpenWorld),
	}
	return d
}

// Call is an identified request to run a named tool. Args holds the effective
// arguments; RequestedArgs optionally preserves the model's original arguments
// when they differ.
type Call struct {
	// ID correlates execution with the model request and any later result.
	// It must be valid UTF-8 so stored JSON preserves the original identity.
	ID string `json:"id"`

	// Name must match the definition selected for execution.
	Name string `json:"name"`

	// Args contains valid JSON for the effective arguments passed to the
	// executor.
	Args json.RawMessage `json:"args"`

	// RequestedArgs optionally retains original JSON arguments when policy changed
	// them.
	RequestedArgs json.RawMessage `json:"requested_args,omitempty"`
}

// Validate reports whether c has an ID, a name, and JSON arguments, and whether
// any requested arguments are also JSON.
func (c Call) Validate() error {
	if c.ID == "" || c.Name == "" || !json.Valid(c.Args) {
		return errors.New("tool: call requires ID, name, and JSON arguments")
	}
	if !utf8.ValidString(c.ID) {
		return errors.New("tool: call ID must be valid UTF-8")
	}
	if len(c.RequestedArgs) > 0 && !json.Valid(c.RequestedArgs) {
		return errors.New("tool: requested arguments must be JSON")
	}
	return nil
}

// Clone returns a copy of c that shares no argument bytes with the original.
func (c Call) Clone() Call {
	c.Args = slices.Clone(c.Args)
	c.RequestedArgs = slices.Clone(c.RequestedArgs)
	return c
}

// Output is the content a tool returns, plus optional display text.
type Output struct {
	// Content holds ordered result blocks; Clone detaches their nested data.
	Content []content.Block `json:"content"`

	// Display optionally supplies text for presentation separately from model
	// content.
	Display string `json:"display,omitempty"`
}

// TextOutput constructs model-facing text without a separate display override.
func TextOutput(text string) Output {
	return Output{Content: []content.Block{content.TextBlock(text)}}
}

// Clone returns a deep copy of o whose content blocks share no data with the
// original.
func (o Output) Clone() Output {
	o.Content = slices.Clone(o.Content)
	for i := range o.Content {
		o.Content[i] = o.Content[i].Clone()
	}
	return o
}

// State is the execution state of a tool call. Each state permits a specific
// set of Outcome payloads; see Outcome.Validate.
type State string

// Outcome states. StateFailed is a known failure the model sees, whether the
// executor returned it or the engine derived it from an executor's error.
const (
	// StateSucceeded requires output from a completed call.
	StateSucceeded State = "succeeded"
	// StateFailed requires output describing a known failure, not an uncertain
	// effect.
	StateFailed State = "failed"
	// StateNotExecuted confirms the executor did not run; explanatory output is
	// optional.
	StateNotExecuted State = "not_executed"
	// StateWaiting requires wait details and excludes output or a detached
	// handle.
	StateWaiting State = "waiting"
	// StateDetached requires a handle for work whose result will arrive later.
	StateDetached State = "detached"
)

// Wait describes why a call is waiting before it can complete.
type Wait struct {
	// Reason is required so the host can explain why execution is waiting.
	Reason string `json:"reason"`

	// Prompt optionally supplies a question or instruction to show the user.
	Prompt string `json:"prompt,omitempty"`
}

// Handle identifies detached work whose result arrives later.
type Handle struct {
	// ID correlates detached work with the result supplied later.
	ID string `json:"id"`
}

// Outcome is a call's execution state and the payload that state permits:
// output for completed calls, wait details for waiting calls, and a handle for
// detached calls. Copies share payload pointers; Output.Clone isolates returned
// content when it needs to be edited independently.
type Outcome struct {
	// State determines which payload fields are valid.
	State State `json:"state"`

	// Output is required for completed calls and optional for nonexecution.
	Output *Output `json:"output,omitempty"`

	// Wait is required only for StateWaiting.
	Wait *Wait `json:"wait,omitempty"`

	// Handle is required only for StateDetached.
	Handle *Handle `json:"handle,omitempty"`
	// Cost is the application-reported charge for completed tool work.
	Cost *Cost `json:"cost,omitempty"`
}

// Cost is an application-reported charge for one completed tool call.
type Cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// Validate reports whether o carries exactly the payloads its State permits
// and whether any output content is well formed.
func (o Outcome) Validate() error {
	if o.Cost != nil {
		if strings.TrimSpace(o.Cost.Currency) == "" ||
			strings.TrimSpace(o.Cost.Currency) != o.Cost.Currency ||
			o.Cost.Amount < 0 ||
			math.IsNaN(o.Cost.Amount) || math.IsInf(o.Cost.Amount, 0) {
			return errors.New("tool: cost requires finite nonnegative amount and currency")
		}
		if o.State != StateSucceeded && o.State != StateFailed {
			return errors.New("tool: cost requires completed execution")
		}
	}
	switch o.State {
	case StateSucceeded, StateFailed:
		if o.Output == nil || o.Wait != nil || o.Handle != nil {
			return errors.New("tool: completed outcome requires only output")
		}
	case StateNotExecuted:
		if o.Wait != nil || o.Handle != nil {
			return errors.New("tool: nonexecution cannot wait or detach")
		}
	case StateWaiting:
		if o.Wait == nil || o.Wait.Reason == "" || o.Output != nil || o.Handle != nil {
			return errors.New("tool: waiting requires only wait details")
		}
	case StateDetached:
		if o.Handle == nil || o.Handle.ID == "" || o.Output != nil || o.Wait != nil {
			return errors.New("tool: detached outcome requires only a handle")
		}
	default:
		return fmt.Errorf("tool: invalid outcome state %q", o.State)
	}
	if o.Output != nil {
		for _, b := range o.Output.Content {
			if err := b.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Executor runs one call. A returned error is a failure the model sees: the
// engine records the call as StateFailed with the error text and continues the
// turn, ignoring any Outcome returned with it. Returning an Outcome with
// StateFailed and nil error does the same with content the tool chooses. Wrap
// the error with Fatal when continuing the turn would be wrong.
//
// An executor never reports uncertainty. The engine alone decides that a
// result was not observed: after a panic, or when a recorded start has no
// recorded result after a crash.
type Executor interface {
	// Execute returns the call's outcome, or an error describing why it
	// failed.
	Execute(context.Context, Call) (Outcome, error)
}

// ErrFatal marks an executor error that ends the turn instead of returning to
// the model. Match it with errors.Is; construct it with Fatal.
var ErrFatal = errors.New("tool: fatal error")

// ErrInvalidOutcome marks an executor result that fails Outcome.Validate. It
// is a programming error, so it also matches ErrFatal.
var ErrInvalidOutcome = errors.New("tool: invalid outcome")

// Fatal marks err as fatal to the turn while keeping it as the cause, so
// errors.Is and errors.As still see err and the recorded text is unchanged.
// Use it for faults the model cannot correct and should not be told to work
// around: an infrastructure outage, lost or revoked credentials, a broken
// dependency, or any state in which continuing the turn is wrong. The engine
// records the call as failed and stops the turn; the host can fix the fault
// and continue it. Fatal(nil) returns ErrFatal, so a fatal marker is never
// lost.
func Fatal(err error) error {
	if err == nil {
		return ErrFatal
	}
	return fatalError{err: err}
}

// fatalError keeps the cause's message and chain while matching ErrFatal.
type fatalError struct {
	err error
}

func (e fatalError) Error() string {
	return e.err.Error()
}

func (e fatalError) Unwrap() error {
	return e.err
}

func (e fatalError) Is(target error) bool {
	return target == ErrFatal
}

// Tool pairs a definition with the executor that runs its calls.
type Tool struct {
	// Definition is presented to the model; copying Tool shares its nested
	// data.
	Definition Definition

	// Executor runs local calls and may be nil for external or provider-hosted
	// tools.
	Executor Executor
}

// Toolset is a source of tools resolved before each model call, so the
// available tools may change between calls.
type Toolset interface {
	// Tools resolves the definitions and executors available for this call.
	Tools(context.Context) ([]Tool, error)
}

func cloneBool(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
