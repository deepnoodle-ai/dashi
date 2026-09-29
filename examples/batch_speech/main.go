// Command batch_speech synthesizes a short spoken clip, then transcribes it
// back to text. It makes two paid OpenAI requests; set OPENAI_API_KEY before
// running it.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	openaispeech "github.com/deepnoodle-ai/dashi/providers/openai/speech"
	"github.com/deepnoodle-ai/dashi/speech"
)

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	client, err := openaispeech.New(openaispeech.Config{})
	if err != nil {
		return fmt.Errorf("batch_speech: create client: %w", err)
	}
	// Each call is prepared, then sent. Summary describes a prepared request
	// without its text or audio, so a host can record what it is about to
	// pay for before sending it.
	synthesis, err := client.PrepareSynthesis(ctx, speech.SynthesizeInput{
		Model: "gpt-4o-mini-tts",
		Text:  "Hello from Dashi.",
	})
	if err != nil {
		return fmt.Errorf("batch_speech: prepare synthesis: %w", err)
	}
	s := synthesis.Summary()
	fmt.Printf(
		"Synthesizing %d characters as %s, voice %q\n",
		s.TextRunes,
		s.OutputMIMEType,
		s.Voice,
	)
	generated, err := client.SendSynthesis(ctx, synthesis)
	if err != nil {
		return fmt.Errorf("batch_speech: synthesize: %w", err)
	}

	transcription, err := client.PrepareTranscription(ctx, speech.TranscribeInput{
		Model: "gpt-4o-mini-transcribe",
		Audio: generated.Audio,
	})
	if err != nil {
		return fmt.Errorf("batch_speech: prepare transcription: %w", err)
	}
	t := transcription.Summary()
	fmt.Printf("Transcribing %d bytes of %s\n", t.InputBytes, t.InputMIMEType)
	result, err := client.SendTranscription(ctx, transcription)
	if err != nil {
		return fmt.Errorf("batch_speech: transcribe: %w", err)
	}
	fmt.Printf("Transcript: %s\n", result.Text)
	return nil
}
