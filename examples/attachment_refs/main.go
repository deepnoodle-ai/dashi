// Command attachment_refs asks the model about a file kept in the program's
// own storage. The message names the file by ID and hash instead of holding
// its bytes, so the saved turn stays small. A content loader, which the
// runner calls before each model request, fetches the bytes. Set
// ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL optionally overrides
// the default model.
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

// Load implements dashi.ContentLoader. It checks that the file belongs to
// the tenant (customer) named in the Run call's context, so one customer
// cannot load another's file.
func (s store) Load(
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
	// The reference names the file. The runner checks the loaded bytes
	// against its hash and size.
	ref := content.Ref{
		ID:     "upl_1",
		SHA256: hex.EncodeToString(sum[:]),
		Size:   int64(len(text)),
	}
	runner := dashi.Runner{
		Client:        client,
		ContentLoader: uploads,
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
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Reply)

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
