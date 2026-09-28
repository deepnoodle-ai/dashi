// Batch speech synthesizes a short MP3 clip, then transcribes it. Running the
// example makes two billable OpenAI requests; set OPENAI_API_KEY first.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	openaispeech "github.com/deepnoodle-ai/dashi/providers/openai/speech"
	"github.com/deepnoodle-ai/dashi/speech"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client, err := openaispeech.New(openaispeech.Config{})
	if err != nil {
		log.Fatal(err)
	}
	synthesis, err := client.PrepareSynthesis(ctx, speech.SynthesizeInput{
		Model: "gpt-4o-mini-tts",
		Text:  "Hello from Dashi.",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Prepared synthesis: %+v\n", synthesis.Summary())
	generated, err := client.SendSynthesis(ctx, synthesis)
	if err != nil {
		log.Fatal(err)
	}
	transcription, err := client.PrepareTranscription(ctx,
		speech.TranscribeInput{
			Model: "gpt-4o-mini-transcribe",
			Audio: generated.Audio.Clone(),
		})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Prepared transcription: %+v\n", transcription.Summary())
	result, err := client.SendTranscription(ctx, transcription)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Transcript: %s\n", result.Text)
}
