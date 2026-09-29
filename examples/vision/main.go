// Command vision asks a model to describe an image, given by URL or as a
// local file. Set ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL
// optionally overrides the catalog's small model.
//
//	go run ./examples/vision
//	go run ./examples/vision -image ./photo.jpg -prompt "What breed is this dog?"
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
)

const defaultImage = "https://images.unsplash.com/photo-1506744038136-46273834b3fb?w=400"

func main() {
	log.SetFlags(0)
	image := flag.String("image", defaultImage, "image URL or local file path")
	prompt := flag.String("prompt", "Describe this image.", "question to ask")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *image, *prompt); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, image, prompt string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model, _ = catalog.Small("anthropic")
	}
	source, err := imageSource(image)
	if err != nil {
		return err
	}
	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
	}
	result, err := llm.Generate(
		ctx,
		client,
		llm.Input{
			Model: model,
			Messages: []llm.Message{
				llm.UserMessage(llm.Image(source), llm.Text(prompt)),
			},
		},
		nil,
	)
	if err != nil {
		return err
	}
	fmt.Println(result.Message.Text())
	return nil
}

// imageSource passes a URL through for the provider to fetch, and reads a
// local file into inline bytes. Dashi never fetches a URL itself; it detects
// the MIME type of inline bytes when preparing the request.
func imageSource(image string) (content.Source, error) {
	if strings.HasPrefix(image, "https://") ||
		strings.HasPrefix(image, "http://") {
		return content.Source{URL: image}, nil
	}
	data, err := os.ReadFile(image)
	if err != nil {
		return content.Source{}, err
	}
	return content.Source{Data: data}, nil
}
