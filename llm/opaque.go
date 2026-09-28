package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/deepnoodle-ai/kernel/content"
)

// OpaqueUnit identifies the structural location of provider data. Each API
// supports a subset; recognizing a unit alone does not authorize replay.
type OpaqueUnit string

const (
	// OpaqueUnitItem holds a top-level provider item.
	OpaqueUnitItem OpaqueUnit = "item"
	// OpaqueUnitContentBlock holds a provider content block within a message.
	OpaqueUnitContentBlock OpaqueUnit = "content_block"
	// OpaqueUnitMessagePart holds a nested message part and the parent metadata
	// required to reconstruct it.
	OpaqueUnitMessagePart OpaqueUnit = "message_part"
)

// Opaque retains provider-private data for recording and compatible replay.
// Unit describes structure, Type preserves provider meaning, and Partial marks
// incompleteness. Version selects the wrapper contract. Copies share Raw bytes;
// Clone detaches them.
type Opaque struct {
	// Version identifies Kernel's wrapper contract, not the provider payload.
	// Version 1 is replayable; other versions remain inspectable as records.
	Version uint16 `json:"version"`

	// Origin restricts replay to a compatible destination.
	Origin content.Origin `json:"origin"`

	// Type retains the provider discriminator, including unfamiliar values.
	Type string `json:"type"`

	// Unit locates Raw within the API shape; providers accept only units they
	// can encode.
	Unit OpaqueUnit `json:"unit"`

	// Partial marks an incomplete payload. Such data can be recorded but must
	// not be replayed, even when its outer JSON object is valid.
	Partial bool `json:"partial,omitempty"`

	// Raw holds valid provider JSON, including wrappers around partial data.
	Raw json.RawMessage `json:"raw"`
}

// Validate checks recordable data, including partial payloads and unfamiliar
// provider types and wrapper versions. Input.Validate separately prohibits
// partial payloads and unsupported wrapper versions from entering requests.
func (o Opaque) Validate() error {
	if o.Origin.Validate() != nil ||
		o.Origin.Model == "" ||
		o.Type == "" ||
		!json.Valid(o.Raw) {
		return errors.New("llm: opaque data requires model origin, type, and valid JSON")
	}
	switch o.Unit {
	case OpaqueUnitItem, OpaqueUnitContentBlock, OpaqueUnitMessagePart:
		return nil
	}
	return fmt.Errorf("llm: unrecognized opaque unit %q", o.Unit)
}

// Clone returns a record without shared provider JSON bytes.
func (o Opaque) Clone() Opaque {
	o.Raw = slices.Clone(o.Raw)
	return o
}
