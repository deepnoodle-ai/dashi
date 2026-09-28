package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/internal/policyrecord"
)

// RequestSnapshot records preparation evidence without execution state. Input
// keeps the caller's original history and settings; Effective includes provider
// defaults. ContentChanges describes adaptations using original input positions.
// Unverified names features sent without known support. Warnings project the
// adjustments that replaced a requested value, for callers to surface.
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
	// value the caller asked for. NewRequest derives them from Adjustments;
	// a supplied list must match.
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
}

// UnmarshalJSON reads capability_policy, or maps the legacy
// unknown_capabilities field it replaced: "allow" is adjust and "reject" is
// strict. A record with neither field predates both and decodes as strict,
// because the legacy zero value rejected unknown support; reading it as the
// current default would change what the evidence says.
func (p *RequestSnapshot) UnmarshalJSON(data []byte) error {
	type plain RequestSnapshot
	var raw struct {
		plain
		CapabilityPolicy    *CapabilityPolicy `json:"capability_policy"`
		UnknownCapabilities *string           `json:"unknown_capabilities"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	policy, err := policyrecord.Decode(
		raw.CapabilityPolicy, raw.UnknownCapabilities,
	)
	if err != nil {
		return fmt.Errorf("llm: %w", err)
	}
	*p = RequestSnapshot(raw.plain)
	p.CapabilityPolicy = policy
	return nil
}

// Request owns immutable, destination-bound intent. Each Send is a separate
// attempt, even when reusing this request. Callers record execution separately;
// this value does not prove dispatch or receipt. Observers cannot change it.
type Request struct {
	snapshot []byte
	body     []byte
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
	effective := p.Input
	effective.Settings = p.Effective
	if err := effective.Validate(); err != nil {
		return Request{}, err
	}
	if p.Origin.Model != p.Input.Model || !json.Valid(p.Body) {
		return Request{}, errors.New("llm: preparation requires matching model and valid body")
	}
	if p.ModelInfo != nil {
		if err := p.ModelInfo.Validate(); err != nil {
			return Request{}, err
		}
		if e := p.ModelInfo.Evidence; e.Model != p.Input.Model || e.API != p.Origin.API {
			return Request{}, errors.New("llm: model evidence does not match preparation")
		}
	}
	if err := validateAdjustments(p.Input.Settings, p.Effective, p.Adjustments); err != nil {
		return Request{}, err
	}
	warnings := Warnings(p.Adjustments)
	if p.Warnings != nil && !slices.Equal(p.Warnings, warnings) {
		return Request{}, errors.New("llm: warnings disagree with adjustments")
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
		return Request{}, errors.New("llm: unverified features require the adjust capability policy")
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
	return Request{snapshot: raw, body: slices.Clone(p.Body)}, nil
}

// Snapshot returns independently owned evidence suitable for recording.
func (p Request) Snapshot() RequestSnapshot {
	var v RequestSnapshot
	_ = json.Unmarshal(p.snapshot, &v)
	// Keep the exact wire bytes, including insignificant JSON whitespace.
	v.Body = p.Body()
	return v
}

// Input returns the original caller input, including its chosen transport mode.
func (p Request) Input() Input {
	return p.Snapshot().Input
}

// Origin identifies the provider, API, model, and replay scope pinned by Prepare.
func (p Request) Origin() content.Origin {
	return p.Snapshot().Origin
}

// Effective returns the settings used to encode the wire body.
func (p Request) Effective() Settings {
	return p.Snapshot().Effective
}

// Body returns an independent copy of the exact encoded request payload.
func (p Request) Body() []byte {
	return slices.Clone(p.body)
}

// Adjustments explains each default or change made during preparation.
func (p Request) Adjustments() []Adjustment {
	return p.Snapshot().Adjustments
}

// Warnings returns what the caller asked for that was not sent as asked, so
// an application can pass it on. The same warnings appear on the Result.
func (p Request) Warnings() []Warning {
	return p.Snapshot().Warnings
}

// ContentChanges returns independent decisions made while adapting history.
func (p Request) ContentChanges() []ContentChange {
	return p.Snapshot().ContentChanges
}

// Generate prepares and sends through the same contracts as explicit users.
// Use Prepare and Send separately when intent must be recorded in between.
func Generate(ctx context.Context, client Client, input Input, sink Sink) (Result, error) {
	request, err := client.Prepare(ctx, input)
	if err != nil {
		return Result{}, err
	}
	return client.Send(ctx, request, sink)
}
