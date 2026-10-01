package speech

import (
	"context"

	"github.com/deepnoodle-ai/dashi/content"
)

// TranscriptionSummary is safe metadata about a prepared batch call. It
// excludes audio bytes, prompt text, credentials, and the multipart body.
type TranscriptionSummary struct {
	Origin        content.Origin `json:"origin"`
	InputMIMEType string         `json:"input_mime_type"`
	InputBytes    int            `json:"input_bytes"`
	Language      string         `json:"language,omitempty"`
	ModelInfo     ModelInfo      `json:"model_info"`
}

// SynthesisSummary is safe metadata about a prepared batch call. It
// excludes spoken text, instructions, credentials, and the request body.
type SynthesisSummary struct {
	Origin         content.Origin `json:"origin"`
	OutputMIMEType string         `json:"output_mime_type"`
	TextRunes      int            `json:"text_runes"`
	Voice          string         `json:"voice"`
	Speed          float64        `json:"speed"`
	ModelInfo      ModelInfo      `json:"model_info"`
}

// TranscriptionRequest is an opaque, provider-owned prepared batch upload.
// Summary returns detached metadata; the sensitive wire body is not exposed.
type TranscriptionRequest interface {
	Summary() TranscriptionSummary
}

// SynthesisRequest is an opaque, provider-owned prepared batch synthesis call.
type SynthesisRequest interface {
	Summary() SynthesisSummary
}

// Recognizer prepares and sends one completed-recording transcription call.
type Recognizer interface {
	PrepareTranscription(
		context.Context,
		TranscribeInput,
	) (TranscriptionRequest, error)
	SendTranscription(context.Context, TranscriptionRequest) (Transcription, error)
}

// Synthesizer prepares and sends one completed text-to-speech call.
type Synthesizer interface {
	PrepareSynthesis(context.Context, SynthesizeInput) (SynthesisRequest, error)
	SendSynthesis(context.Context, SynthesisRequest) (Synthesis, error)
}
