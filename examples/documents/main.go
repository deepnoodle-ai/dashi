// Command documents asks a model a question about a PDF, given by URL or as a
// local file, and prints the answer with any citations it carries. Requesting
// citations on a document is not supported yet, so a PDF answer has none.
// Set ANTHROPIC_API_KEY and ANTHROPIC_MODEL before running it.
//
//	go run ./examples/documents
//	go run ./examples/documents -pdf ./report.pdf -prompt "List the risks."
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
)

const defaultPDF = "https://assets.anthropic.com/m/1cd9d098ac3e6467/original/Claude-3-Model-Card-October-Addendum.pdf"

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	pdf := flag.String("pdf", defaultPDF, "PDF URL or local file path")
	prompt := flag.String(
		"prompt",
		"What are the key findings? Answer in three short bullets.",
		"question to ask about the document",
	)
	flag.Parse()

	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" || os.Getenv("ANTHROPIC_API_KEY") == "" {
		return errors.New("documents: set ANTHROPIC_MODEL and ANTHROPIC_API_KEY")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	document, err := pdfDocument(*pdf)
	if err != nil {
		return fmt.Errorf("documents: %w", err)
	}
	client, err := messages.New(messages.Config{})
	if err != nil {
		return fmt.Errorf("documents: create client: %w", err)
	}
	// Put the document before the question; Anthropic recommends that order
	// for long inputs.
	result, err := llm.Generate(
		ctx,
		client,
		llm.Input{
			Model: model,
			Messages: []llm.Message{
				llm.UserMessage(llm.Block(document), llm.Text(*prompt)),
			},
		},
		nil,
	)
	if err != nil {
		return fmt.Errorf("documents: generate: %w", err)
	}

	// Each text block keeps its own citations. Anthropic attaches them only
	// when a document enables citations, which content.Document cannot request
	// yet, so this loop prints citations from hosted tools such as web search
	// but none for this PDF today.
	for _, c := range result.Message.Content {
		if c.Block == nil || c.Block.Text == nil {
			continue
		}
		fmt.Print(c.Block.Text.Text)
		for _, citation := range c.Block.Text.Citations {
			fmt.Printf("\n  [cited %q: %q]", citation.Title, citation.Text)
		}
	}
	fmt.Printf(
		"\n\nTokens: %d in, %d out\n",
		result.Usage.InputTokens,
		result.Usage.OutputTokens,
	)
	return nil
}

// pdfDocument passes a URL through for the provider to fetch, and reads a
// local file into inline bytes. Anthropic accepts inline documents only as
// PDFs; plain-text documents need a text source that Dashi does not encode.
func pdfDocument(pdf string) (content.Block, error) {
	if strings.HasPrefix(pdf, "https://") || strings.HasPrefix(pdf, "http://") {
		return content.DocumentBlock(content.Source{URL: pdf}), nil
	}
	data, err := os.ReadFile(pdf)
	if err != nil {
		return content.Block{}, err
	}
	block := content.DocumentBlock(content.Source{
		Data:     data,
		MIMEType: "application/pdf",
	})
	block.Document.Title = filepath.Base(pdf)
	return block, nil
}
