package speech

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/speech"
)

// TestLiveBatchSpeechEndpoints is an explicitly opt-in, billable provider
// probe. Fixture tests alone do not prove remote model acceptance.
func TestLiveBatchSpeechEndpoints(t *testing.T) {
	if os.Getenv("DASHI_LIVE_SPEECH") != "1" {
		t.Skip("set DASHI_LIVE_SPEECH=1 for the live batch speech probe")
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("OPENAI_API_KEY is required for the live batch speech probe")
	}
	client, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	var generated speech.Synthesis
	t.Run("Synthesis", func(t *testing.T) {
		request, err := client.PrepareSynthesis(ctx, speech.SynthesizeInput{
			Model: synthesisModel,
			Text:  "Hello from the Dashi live speech probe.",
		})
		if err != nil {
			t.Fatal(err)
		}
		generated, err = client.SendSynthesis(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if err := generated.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	if generated.Audio.Audio == nil {
		t.Fatal("synthesis did not yield audio for transcription probe")
	}
	t.Run("Transcription", func(t *testing.T) {
		request, err := client.PrepareTranscription(ctx, speech.TranscribeInput{
			Model: transcribeModel,
			Audio: generated.Audio.Clone(),
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.SendTranscription(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if result.Text == "" {
			t.Fatal("provider returned no transcript for generated speech")
		}
	})
}
