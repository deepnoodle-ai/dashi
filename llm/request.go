package llm

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"

	"github.com/deepnoodle-ai/dashi/content"
)

// RequestSnapshot records preparation evidence without execution state. Input
// keeps the caller's original history and settings; Effective includes provider
// defaults. ContentChanges describes adaptations using original input positions.
// Unverified names features sent without known support. Warnings show the
// adjustments that replaced a requested value, and content changes the caller
// should fix, for callers to surface.
// Body is the single encoded payload Send will send, including stream mode.
// JSON encodes Body as base64 so recorded bytes survive without normalization.
type RequestSnapshot struct {
	// Origin pins the provider, API, model, and private replay scope.
	Origin content.Origin `json:"origin"`

	// Input preserves the original history, settings, and selected stream mode.
	Input Input `json:"input"`

	// Effective contains the settings used to encode Body, including applied
	// defaults.
	Effective Settings `json:"effective"`

	// Body holds the exact wire bytes; JSON uses base64 to avoid normalization.
	Body []byte `json:"body"`

	// Adjustments explains each default or change made during preparation.
	Adjustments []Adjustment `json:"adjustments,omitempty"`

	// Warnings lists, in order, the adjustments that replaced or dropped a
	// value the caller asked for, then content changes the caller should fix.
	// NewRequest derives them from Adjustments and ContentChanges; a supplied
	// list must match.
	Warnings []Warning `json:"warnings,omitempty"`

	// ContentChanges explains each history transformation using original input
	// positions. These decisions are separate from generation settings.
	ContentChanges []ContentChange `json:"content_changes,omitempty"`

	// ModelInfo snapshots the knowledge used, or is nil when the model is
	// unknown.
	ModelInfo *ModelInfo `json:"model_info,omitempty"`

	// CapabilityPolicy records the resolved policy used for this preparation.
	// It is always written, so a record never depends on a later default.
	CapabilityPolicy CapabilityPolicy `json:"capability_policy"`

	// Unverified names features sent under the adjust policy although their
	// support is unknown. They are recorded, not warned about: until a model
	// is described, every call would otherwise warn.
	Unverified []Feature `json:"unverified,omitempty"`

	// EstimatedInputTokens is an optional input token estimate from the
	// preparer, such as a provider's token count. Zero means
	// EstimatedInputTokens computes one; a negative value is invalid.
	EstimatedInputTokens int `json:"estimated_input_tokens,omitempty"`
}

// Request holds what the caller asked to send, fixed and bound to one
// destination. Each Send is a separate model call, even when reusing this
// request. Callers record execution separately; this value does not prove
// dispatch or receipt. Observers cannot change it.
type Request struct {
	snapshot []byte
	body     []byte
	// decoded holds the snapshot decoded from its JSON once, without Body.
	// Accessors return clones of it, so callers never share its memory.
	decoded func() RequestSnapshot
}

// NewRequest snapshots provider-produced evidence. Providers must ensure
// their encoded body agrees with the input, effective settings, and origin.
// Restoring a snapshot neither requalifies evidence nor authorizes dispatch.
func NewRequest(p RequestSnapshot) (Request, error) {
	if err := p.Origin.Validate(); err != nil {
		return Request{}, err
	}
	if err := p.Input.Validate(); err != nil {
		return Request{}, err
	}
	if p.Input.HasRef() {
		return Request{}, errors.New(
			"llm: preparation requires resolved content references",
		)
	}
	effective := p.Input
	effective.Settings = p.Effective
	if err := effective.Validate(); err != nil {
		return Request{}, err
	}
	if p.EstimatedInputTokens < 0 {
		return Request{}, errors.New(
			"llm: estimated input tokens must not be negative",
		)
	}
	if p.Origin.Model != p.Input.Model || !json.Valid(p.Body) {
		return Request{}, errors.New(
			"llm: preparation requires matching model and valid body",
		)
	}
	if p.ModelInfo != nil {
		if err := p.ModelInfo.Validate(); err != nil {
			return Request{}, err
		}
		if e := p.ModelInfo.Evidence; e.Model != p.Input.Model ||
			e.API != p.Origin.API {
			return Request{}, errors.New(
				"llm: model evidence does not match preparation",
			)
		}
	}
	if err := validateAdjustments(
		p.Input.Settings,
		p.Effective,
		p.Adjustments,
	); err != nil {
		return Request{}, err
	}
	warnings := collectWarnings(p.Adjustments, p.ContentChanges)
	if p.Warnings != nil && !slices.Equal(p.Warnings, warnings) {
		return Request{}, errors.New(
			"llm: warnings disagree with adjustments and content changes",
		)
	}
	p.Warnings = warnings
	if err := validateContentChanges(p); err != nil {
		return Request{}, err
	}
	if !p.CapabilityPolicy.Valid() {
		return Request{}, errors.New("llm: invalid capability policy")
	}
	p.CapabilityPolicy = p.CapabilityPolicy.Resolved()
	if len(p.Unverified) > 0 && p.CapabilityPolicy != CapabilityAdjust {
		return Request{}, errors.New(
			"llm: unverified features require the adjust capability policy",
		)
	}
	for _, feature := range p.Unverified {
		if !knownFeature(feature) {
			return Request{}, errors.New("llm: invalid unverified feature")
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return Request{}, err
	}
	return Request{
		snapshot: raw,
		body:     slices.Clone(p.Body),
		decoded:  sync.OnceValue(func() RequestSnapshot { return decodeSnapshot(raw) }),
	}, nil
}

// decodeSnapshot decodes a snapshot NewRequest encoded. Decoding, rather than
// keeping the caller's value, gives every reader what a recorded snapshot
// restores to. Body is left out: Request keeps the exact wire bytes.
func decodeSnapshot(raw []byte) RequestSnapshot {
	var v RequestSnapshot
	_ = json.Unmarshal(raw, &v)
	v.Body = nil
	return v
}

// view returns the decoded snapshot shared by all accessors. Callers must
// clone what they return.
func (p Request) view() RequestSnapshot {
	if p.decoded == nil {
		return RequestSnapshot{}
	}
	return p.decoded()
}

// Snapshot returns independently owned evidence suitable for recording.
func (p Request) Snapshot() RequestSnapshot {
	v := p.view().clone()
	// Keep the exact wire bytes, including insignificant JSON whitespace.
	v.Body = p.Body()
	return v
}

// Input returns the original caller input, including its chosen transport mode.
func (p Request) Input() Input {
	return p.view().Input.clone()
}

// Origin identifies the provider, API, model, and replay scope pinned by Prepare.
func (p Request) Origin() content.Origin {
	return p.view().Origin
}

// Effective returns the settings used to encode the wire body.
func (p Request) Effective() Settings {
	return p.view().Effective.clone()
}

// Body returns an independent copy of the exact encoded request payload.
func (p Request) Body() []byte {
	return slices.Clone(p.body)
}

// Adjustments explains each default or change made during preparation.
func (p Request) Adjustments() []Adjustment {
	return cloneAdjustments(p.view().Adjustments)
}

// Warnings returns what the caller asked for that was not sent as asked, so
// an application can pass it on. The same warnings appear on the Result.
func (p Request) Warnings() []Warning {
	return slices.Clone(p.view().Warnings)
}

// ContentChanges returns independent decisions made while adapting history.
func (p Request) ContentChanges() []ContentChange {
	return slices.Clone(p.view().ContentChanges)
}

// Generate prepares and sends through the same contracts as explicit users.
// Use Prepare and Send separately when the call's start must be recorded in
// between.
func Generate(
	ctx context.Context,
	client Client,
	input Input,
	sink Sink,
) (Result, error) {
	request, err := client.Prepare(ctx, input)
	if err != nil {
		return Result{}, err
	}
	return client.Send(ctx, Call{Request: request, Sink: sink})
}

// HasRef reports whether any message holds an unresolved content reference. A
// prepared request must carry bytes, because a reference names bytes that only
// the application can supply.
func (i Input) HasRef() bool {
	for _, m := range i.Messages {
		for _, c := range m.Content {
			for _, s := range c.sources() {
				if s.Ref != nil {
					return true
				}
			}
		}
	}
	return false
}
