// Package speech defines values for spoken-language recognition and synthesis.
// It covers conversation and produced speech alike. Provider clients, batch
// preparation, and stream transports will be qualified separately; this
// package does not capture audio devices or own a conversation runtime.
package speech

import (
	"errors"
	"math"
	"slices"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/model"
)

// TimestampGranularity names a requested or reported transcript time scale.
// The empty value requests no particular scale.
type TimestampGranularity string

const (
	// TimestampSegment requests one interval per recognized utterance.
	TimestampSegment TimestampGranularity = "segment"
	// TimestampWord requests timing for individual recognized words.
	TimestampWord TimestampGranularity = "word"
)

// TranscribeInput keeps speech audio separate from linguistic hints. A client
// must reject hints the selected model or API cannot encode during Prepare.
type TranscribeInput struct {
	Model       string               `json:"model"`
	Audio       content.Block        `json:"audio"`
	Language    string               `json:"language,omitempty"`
	Prompt      string               `json:"prompt,omitempty"`
	Granularity TimestampGranularity `json:"granularity,omitempty"`
	Diarization bool                 `json:"diarization,omitempty"`
}

// Validate checks provider-independent input shape, not model support.
func (in TranscribeInput) Validate() error {
	if in.Model == "" || in.Audio.Kind != content.KindAudio {
		return errors.New("speech: transcription requires model and audio block")
	}
	if err := in.Audio.Validate(); err != nil {
		return err
	}
	if in.Granularity != "" && in.Granularity != TimestampSegment &&
		in.Granularity != TimestampWord {
		return errors.New("speech: unsupported timestamp granularity")
	}
	return nil
}

// Clone detaches audio bytes and source expiry.
func (in TranscribeInput) Clone() TranscribeInput {
	in.Audio = in.Audio.Clone()
	return in
}

// SynthesizeInput describes spoken output without implying a playback device.
// OutputMIMEType names the media type; Format describes sample layout when
// requested.
type SynthesizeInput struct {
	Model          string               `json:"model"`
	Text           string               `json:"text"`
	Voice          string               `json:"voice,omitempty"`
	Language       string               `json:"language,omitempty"`
	Instructions   string               `json:"instructions,omitempty"`
	Speed          *float64             `json:"speed,omitempty"`
	OutputMIMEType string               `json:"output_mime_type,omitempty"`
	Format         *content.AudioFormat `json:"format,omitempty"`
}

// Validate checks provider-independent input shape, not voice or format
// support. Speed is optional so explicit 1.0 remains distinguishable.
func (in SynthesizeInput) Validate() error {
	if in.Model == "" || in.Text == "" {
		return errors.New("speech: synthesis requires model and text")
	}
	if in.Speed != nil && (*in.Speed <= 0 ||
		math.IsNaN(*in.Speed) || math.IsInf(*in.Speed, 0)) {
		return errors.New("speech: synthesis speed must be positive")
	}
	if in.Format != nil {
		return in.Format.Validate()
	}
	return nil
}

// Clone detaches optional controls before a caller changes a copy.
func (in SynthesizeInput) Clone() SynthesizeInput {
	if in.Speed != nil {
		speed := *in.Speed
		in.Speed = &speed
	}
	if in.Format != nil {
		format := *in.Format
		in.Format = &format
	}
	return in
}

// Segment is a timed transcript span. Times are nanoseconds in JSON. Speaker
// labels are local to one result and do not identify people across results.
type Segment struct {
	Start   time.Duration `json:"start"`
	End     time.Duration `json:"end"`
	Text    string        `json:"text"`
	Speaker string        `json:"speaker,omitempty"`
}

// Validate requires a nonnegative, ordered interval. Zero length can
// represent a provider marker with no measured duration.
func (s Segment) Validate() error {
	if s.Start < 0 || s.End < s.Start {
		return errors.New("speech: segment requires ordered nonnegative times")
	}
	return nil
}

// Transcription is a completed recognition result. Empty text may mean no
// speech was recognized; partial stream previews are separate.
type Transcription struct {
	Text        string               `json:"text"`
	Segments    []Segment            `json:"segments,omitempty"`
	Language    string               `json:"language,omitempty"`
	Granularity TimestampGranularity `json:"granularity,omitempty"`
	Requested   model.ModelRef       `json:"requested"`
	Served      model.ModelRef       `json:"served"`
	API         string               `json:"api"`
	ResponseID  string               `json:"response_id,omitempty"`
}

// Validate checks record shape and segment ordering without claiming the
// provider's timestamps or speaker attribution are accurate.
func (r Transcription) Validate() error {
	if r.Requested.Provider == "" || r.Requested.Model == "" || r.API == "" {
		return errors.New("speech: transcription requires request origin")
	}
	if r.Granularity != "" && r.Granularity != TimestampSegment &&
		r.Granularity != TimestampWord {
		return errors.New("speech: invalid transcript granularity")
	}
	if err := validateServed(r.Requested, r.Served); err != nil {
		return err
	}
	for i, segment := range r.Segments {
		if err := segment.Validate(); err != nil {
			return err
		}
		if i > 0 && segment.Start < r.Segments[i-1].Start {
			return errors.New("speech: segments must be ordered by start time")
		}
	}
	return nil
}

// Clone detaches segment storage.
func (r Transcription) Clone() Transcription {
	r.Segments = slices.Clone(r.Segments)
	return r
}

// Synthesis is collected spoken output. Audio is a content block, not a stored
// artifact or proof that any listener heard it.
type Synthesis struct {
	Audio      content.Block  `json:"audio"`
	Requested  model.ModelRef `json:"requested"`
	Served     model.ModelRef `json:"served"`
	API        string         `json:"api"`
	ResponseID string         `json:"response_id,omitempty"`
}

// Validate requires an audio block and request origin.
func (r Synthesis) Validate() error {
	if r.Audio.Kind != content.KindAudio || r.Requested.Provider == "" ||
		r.Requested.Model == "" || r.API == "" {
		return errors.New("speech: synthesis requires audio and request origin")
	}
	if err := validateServed(r.Requested, r.Served); err != nil {
		return err
	}
	return r.Audio.Validate()
}

// Clone detaches audio bytes, format, and expiry.
func (r Synthesis) Clone() Synthesis {
	r.Audio = r.Audio.Clone()
	return r
}

func validateServed(requested, served model.ModelRef) error {
	if served == (model.ModelRef{}) {
		return nil
	}
	if served.Provider != requested.Provider || served.Model == "" {
		return errors.New("speech: served model must identify the configured provider")
	}
	return nil
}
