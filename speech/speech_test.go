package speech

import (
	"math"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/model"
)

func TestTranscribeInputCloneCopiesAudioBytes(t *testing.T) {
	input := TranscribeInput{
		Model: "recognizer",
		Audio: content.AudioBlock(content.Source{
			Data:     []byte{1},
			MIMEType: "audio/wav",
		}),
	}
	if err := input.Validate(); err != nil {
		t.Fatalf("valid recognition input rejected: %v", err)
	}
	clone := input.Clone()
	clone.Audio.Audio.Source.Data[0] = 9
	if input.Audio.Audio.Source.Data[0] != 1 {
		t.Fatal("recognition audio bytes were shared")
	}
}

func TestSynthesizeInputCloneCopiesOptionalControls(t *testing.T) {
	speed := 1.5
	input := SynthesizeInput{
		Speed: &speed,
		Format: &content.AudioFormat{
			Encoding:     "pcm_s16le",
			SampleRateHz: 16000,
			Channels:     1,
		},
	}
	clone := input.Clone()
	*clone.Speed = 2
	clone.Format.SampleRateHz = 24000
	if *input.Speed != 1.5 {
		t.Fatal("synthesis speed was shared")
	}
	if input.Format.SampleRateHz != 16000 {
		t.Fatal("synthesis frame format was shared")
	}
}

func TestSynthesizeInputValidateRejectsNonfiniteSpeed(t *testing.T) {
	for _, speed := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		input := SynthesizeInput{
			Model: "synthesizer",
			Text:  "hello",
			Speed: &speed,
		}
		if err := input.Validate(); err == nil {
			t.Fatalf("accepted invalid speed %v", speed)
		}
	}
}

func TestTranscriptionValidateRequiresOrderedSegments(t *testing.T) {
	result := Transcription{
		Requested: model.ModelRef{Provider: "test", Model: "recognizer"},
		API:       "transcriptions",
		Segments: []Segment{
			{Start: time.Second, End: 2 * time.Second, Text: "first"},
			{Start: 0, End: time.Second, Text: "second"},
		},
	}
	if err := result.Validate(); err == nil {
		t.Fatal("accepted transcript segments out of order")
	}
	result.Segments[1].Start = 2 * time.Second
	result.Segments[1].End = 3 * time.Second
	if err := result.Validate(); err != nil {
		t.Fatalf("ordered transcript rejected: %v", err)
	}
}

func TestModelInfoCloneCopiesCapabilityLists(t *testing.T) {
	info := ModelInfo{
		Recognition: &RecognitionCapabilities{
			Granularities: []TimestampGranularity{TimestampWord},
		},
		Synthesis: &SynthesisCapabilities{
			Voices: []string{"voice-a"},
		},
	}
	clone := info.Clone()
	clone.Recognition.Granularities[0] = TimestampSegment
	clone.Synthesis.Voices[0] = "voice-b"
	if info.Recognition.Granularities[0] != TimestampWord {
		t.Fatal("recognition timestamp constraints were shared")
	}
	if info.Synthesis.Voices[0] != "voice-a" {
		t.Fatal("synthesis voices were shared")
	}
}
