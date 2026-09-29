// Command attachment_refs runs an agent turn that refers to a stored upload by
// reference. The turn record holds the upload's ID, digest, and size, never
// its bytes; a resolver supplies the bytes before each model request, after it
// checks the tenant. Set ANTHROPIC_API_KEY before running it;
// ANTHROPIC_MODEL optionally overrides the catalog's default model.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
)

type tenantKey struct{}

// upload is a stored file and the tenant that owns it.
type upload struct {
	tenant string
	data   []byte
}

// store stands in for the host's upload storage.
type store map[string]upload

// Resolve implements dashi.ContentResolver. It reads the tenant from the
// context of the Run call, so a shared Runner cannot serve one tenant's file
// to another.
func (s store) Resolve(
	ctx context.Context,
	_ string,
	ref content.Ref,
) ([]byte, error) {
	tenant, _ := ctx.Value(tenantKey{}).(string)
	u, ok := s[ref.ID]
	if !ok || u.tenant != tenant {
		return nil, errors.New("upload not found")
	}
	return u.data, nil
}

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
	}

	text := []byte("Quarterly note: revenue grew 12%. Churn fell to 2%.")
	sum := sha256.Sum256(text)
	uploads := store{"upl_1": {tenant: "acme", data: text}}
	// The runner checks the resolved bytes against SHA256 and Size. A failed
	// resolve stops the turn as failed; it is not a Run error.
	ref := content.Ref{
		ID:     "upl_1",
		SHA256: hex.EncodeToString(sum[:]),
		Size:   int64(len(text)),
	}
	runner := dashi.Runner{
		Client:          client,
		ContentResolver: uploads,
	}
	tenantCtx := context.WithValue(ctx, tenantKey{}, "acme")
	out, err := runner.Run(tenantCtx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{Model: os.Getenv("ANTHROPIC_MODEL")},
		Input: []llm.Message{llm.UserMessage(
			llm.Document(
				content.Source{Ref: &ref, MIMEType: "text/plain"},
				"note.txt",
			),
			llm.Text("What happened to churn?"),
		)},
	})
	if err != nil {
		return err
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Answer)

	// The record holds the reference and none of the bytes.
	record, err := json.Marshal(out.Turn)
	if err != nil {
		return err
	}
	fmt.Printf(
		"\nTurn record: %d bytes; upload: %d bytes\n",
		len(record),
		len(text),
	)
	return nil
}
