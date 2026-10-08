// Command documents asks a model a question about a document and prints the
// reply with the passages it cited. The document is a short built-in text by
// default, or a PDF or text file given by URL or local path. Set
// ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL optionally overrides
// the default small model.
//
//	go run ./examples/documents
//	go run ./examples/documents -doc ./report.pdf -prompt "List the risks."
//	go run ./examples/documents -doc https://example.com/paper.pdf
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
)

const builtIn = "The grass is green. The sky is blue. " +
	"Water boils at 100 degrees Celsius at sea level."

func main() {
	log.SetFlags(0)
	doc := flag.String("doc", "", "PDF URL, or local PDF or text file")
	prompt := flag.String(
		"prompt",
		"What color are the grass and the sky?",
		"question to ask about the document",
	)
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *doc, *prompt); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, doc, prompt string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model, _ = catalog.Small("anthropic")
	}
	document, err := loadDocument(doc)
	if err != nil {
		return err
	}
	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
	}
	// Citations asks the model to point to the passages it relied on. Put
	// the document before the question; Anthropic recommends that order.
	result, err := llm.Generate(
		ctx,
		client,
		llm.Input{
			Model:    model,
			Settings: llm.Settings{Citations: true},
			Messages: []llm.Message{
				llm.UserMessage(document, llm.Text(prompt)),
			},
		},
		nil,
	)
	if err != nil {
		return err
	}
	for _, w := range result.Warnings {
		fmt.Printf("Warning (%s): %s\n", w.Code, w.Message)
	}

	// A cited reply arrives in several text blocks, each with its
	// citations. Text joins the blocks into one reply.
	fmt.Println(result.Message.Text())
	n := 0
	for _, c := range result.Message.Content {
		if c.Block == nil || c.Block.Text == nil {
			continue
		}
		for _, citation := range c.Block.Text.Citations {
			n++
			fmt.Printf("\n[%d] %q from %s", n, citation.Text, citation.Title)
			if l := citation.Location; l != nil {
				fmt.Printf(", %s %d-%d", l.Unit, l.Start, l.End)
			}
		}
	}
	fmt.Println()
	// Usage is nil when the provider did not report it.
	if u := result.Usage; u != nil {
		fmt.Printf(
			"\nTokens: %d in, %d out\n",
			u.TotalInputTokens(),
			u.OutputTokens,
		)
	}
	return nil
}

// loadDocument passes a URL through for the provider to fetch, and reads a
// local file into memory. Dashi tells a PDF from text by the file's name and
// contents.
func loadDocument(doc string) (llm.Content, error) {
	switch {
	case doc == "":
		return llm.Document(content.Source{Data: []byte(builtIn)}, "Facts"), nil
	case strings.HasPrefix(doc, "https://") || strings.HasPrefix(doc, "http://"):
		return llm.Document(content.Source{URL: doc}, filepath.Base(doc)), nil
	}
	data, err := os.ReadFile(doc)
	if err != nil {
		return llm.Content{}, err
	}
	return llm.Document(content.Source{Data: data}, filepath.Base(doc)), nil
}
