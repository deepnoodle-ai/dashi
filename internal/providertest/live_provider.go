package providertest

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
)

// LiveFinalFacts makes two paid requests, blocking and streaming, and checks
// the final facts. It skips unless KERNEL_LIVE_TEST=1 and fails when key or
// model is empty.
func LiveFinalFacts(
	t *testing.T,
	key, model string,
	newClient func(key string) (llm.Client, error),
) {
	t.Helper()
	if os.Getenv("KERNEL_LIVE_TEST") != "1" {
		t.Skip("set KERNEL_LIVE_TEST=1 to make paid provider calls")
	}
	if key == "" || model == "" {
		t.Fatal("the provider needs its API key and MODEL environment variable")
	}
	client, err := newClient(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		input := llm.Input{
			Model:    model,
			Stream:   stream,
			Settings: llm.Settings{MaxTokens: 32},
			Messages: []llm.Message{
				llm.UserMessage(llm.Text("Reply with one short greeting.")),
			},
		}
		request, err := client.Prepare(ctx, input)
		if err == nil {
			var result llm.Result
			result, err = client.Send(ctx, llm.Call{Request: request})
			if err == nil &&
				(result.Stop.Kind != llm.StopFinished ||
					result.Message.Text() == "" ||
					result.Requested.Model != model) {
				t.Errorf("stream=%t incomplete final facts: %+v", stream, result)
			}
		}
		cancel()
		if err != nil {
			t.Errorf("stream=%t: %v", stream, err)
		}
	}
}

// ErrorProbe provokes classified rejections from one live provider.
type ErrorProbe struct {
	// KeyEnv names the API key variable; the probe skips when it is unset.
	KeyEnv string
	Model  string
	New    func() (llm.Client, error)
	// Window is the model's context window in tokens; zero skips the
	// context probe.
	Window int
	// Media sends a corrupt PNG and an unsupported image type.
	Media bool
}

// LiveErrorReasons provokes each classified rejection the probe enables and
// logs the provider's error body as recorded evidence. It skips unless
// KERNEL_LIVE_TEST=1.
func LiveErrorReasons(t *testing.T, p ErrorProbe) {
	t.Helper()
	if os.Getenv("KERNEL_LIVE_TEST") != "1" {
		t.Skip("set KERNEL_LIVE_TEST=1 to make provider calls")
	}
	if os.Getenv(p.KeyEnv) == "" {
		t.Skipf("%s is not set", p.KeyEnv)
	}
	client, err := p.New()
	if err != nil {
		t.Fatal(err)
	}
	send := func(t *testing.T, want llm.ErrorReason, parts ...llm.Content) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		input := llm.Input{
			Model:    p.Model,
			Settings: llm.Settings{MaxTokens: 16},
			Messages: []llm.Message{{Role: llm.RoleUser, Content: parts}},
		}
		request, err := client.Prepare(ctx, input)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		result, err := client.Send(ctx, llm.Call{Request: request})
		if err == nil {
			t.Fatalf(
				"request was accepted: served=%s usage=%+v",
				result.Served.Model,
				result.Usage,
			)
		}
		e, ok := errors.AsType[*llm.Error](err)
		if !ok {
			t.Fatalf("err = %v, want a provider rejection", err)
		}
		t.Logf(
			"status=%d category=%s code=%q reason=%q body=%s",
			e.Status,
			e.Category,
			e.Code,
			e.Reason,
			e.Body,
		)
		if e.Reason != want {
			t.Errorf("reason = %q, want %q", e.Reason, want)
		}
	}
	if p.Media {
		// A PNG signature followed by bytes that are not a PNG.
		corrupt := append(
			[]byte("\x89PNG\r\n\x1a\n"),
			[]byte("this is not an image")...,
		)
		t.Run("corrupt image", func(t *testing.T) {
			send(
				t,
				llm.ReasonMediaRejected,
				llm.Text("Describe this image."),
				llm.Block(content.ImageBlock(content.Source{
					Data:     corrupt,
					MIMEType: "image/png",
				})),
			)
		})
		t.Run("unsupported image type", func(t *testing.T) {
			send(
				t,
				llm.ReasonMediaRejected,
				llm.Text("Describe this image."),
				llm.Block(content.ImageBlock(content.Source{
					Data:     []byte("II*\x00not really a tiff"),
					MIMEType: "image/tiff",
				})),
			)
		})
	}
	if p.Window > 0 {
		t.Run("context window", func(t *testing.T) {
			// " word" is one token in the tokenizers of all probed models,
			// so this is about 1.5 windows of input.
			send(
				t,
				llm.ReasonContextWindowExceeded,
				llm.Text(strings.Repeat(" word", p.Window*3/2)),
			)
		})
	}
}
