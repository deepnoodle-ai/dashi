package speech

import (
	"errors"
	"slices"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/model"
)

// RecognitionCapabilities states known language, format, timing, and stream
// support. Empty lists mean unknown, not unrestricted.
type RecognitionCapabilities struct {
	Batch          model.Support         `json:"batch,omitempty"`
	Streaming      model.Support         `json:"streaming,omitempty"`
	Diarization    model.Support         `json:"diarization,omitempty"`
	Languages      []string              `json:"languages,omitempty"`
	InputMIMETypes []string              `json:"input_mime_types,omitempty"`
	FrameFormats   []content.AudioFormat `json:"frame_formats,omitempty"`
	// Granularities lists supported timestamp resolutions when known.
	Granularities []TimestampGranularity `json:"granularities,omitempty"`
}

// Clone detaches constraint lists.
func (c RecognitionCapabilities) Clone() RecognitionCapabilities {
	c.Languages = slices.Clone(c.Languages)
	c.InputMIMETypes = slices.Clone(c.InputMIMETypes)
	c.FrameFormats = slices.Clone(c.FrameFormats)
	c.Granularities = slices.Clone(c.Granularities)
	return c
}

// SynthesisCapabilities states known voice, language, format, and stream
// support. IncrementalText is separate from streaming output audio.
type SynthesisCapabilities struct {
	Batch           model.Support         `json:"batch,omitempty"`
	Streaming       model.Support         `json:"streaming,omitempty"`
	IncrementalText model.Support         `json:"incremental_text,omitempty"`
	Voices          []string              `json:"voices,omitempty"`
	Languages       []string              `json:"languages,omitempty"`
	OutputMIMETypes []string              `json:"output_mime_types,omitempty"`
	FrameFormats    []content.AudioFormat `json:"frame_formats,omitempty"`
}

// Clone detaches constraint lists.
func (c SynthesisCapabilities) Clone() SynthesisCapabilities {
	c.Voices = slices.Clone(c.Voices)
	c.Languages = slices.Clone(c.Languages)
	c.OutputMIMETypes = slices.Clone(c.OutputMIMETypes)
	c.FrameFormats = slices.Clone(c.FrameFormats)
	return c
}

// ModelInfo scopes speech capability claims to one model and endpoint.
type ModelInfo struct {
	Recognition *RecognitionCapabilities `json:"recognition,omitempty"`
	Synthesis   *SynthesisCapabilities   `json:"synthesis,omitempty"`
	Evidence    model.ModelEvidence      `json:"evidence"`
}

// Validate checks the evidence and internal shape, not the truth of claims.
func (m ModelInfo) Validate() error {
	if err := m.Evidence.Validate(); err != nil {
		return err
	}
	if m.Recognition == nil && m.Synthesis == nil {
		return errors.New("speech: model info requires recognition or synthesis")
	}
	if c := m.Recognition; c != nil {
		if !c.Batch.Valid() || !c.Streaming.Valid() ||
			!c.Diarization.Valid() {
			return errors.New("speech: invalid recognition support")
		}
		for _, granularity := range c.Granularities {
			if granularity != TimestampSegment && granularity != TimestampWord {
				return errors.New("speech: invalid timestamp granularity")
			}
		}
		for _, format := range c.FrameFormats {
			if err := format.Validate(); err != nil {
				return err
			}
		}
	}
	if c := m.Synthesis; c != nil {
		if !c.Batch.Valid() || !c.Streaming.Valid() ||
			!c.IncrementalText.Valid() {
			return errors.New("speech: invalid synthesis support")
		}
		for _, format := range c.FrameFormats {
			if err := format.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Clone detaches optional capability records and their constraint lists.
func (m ModelInfo) Clone() ModelInfo {
	if m.Recognition != nil {
		recognition := m.Recognition.Clone()
		m.Recognition = &recognition
	}
	if m.Synthesis != nil {
		synthesis := m.Synthesis.Clone()
		m.Synthesis = &synthesis
	}
	return m
}
