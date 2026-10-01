package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestAutomationWholePayloadOneTurn(t *testing.T) {
	stdin := "日本\tline\n\n/quit\nyes\n@missing-file\n"
	data, err := readAutomationStdin(t.Context(), strings.NewReader(stdin), nil)
	if err != nil {
		t.Fatal(err)
	}
	combined, err := combineAutomationInput(" Explain exactly \n", data, false)
	if err != nil {
		t.Fatal(err)
	}
	want := " Explain exactly \n\n\n" + stdin
	if combined != want {
		t.Fatalf("payload=%q, want=%q", combined, want)
	}
	h := newHarness(t, say("one answer"))
	if _, err := h.app.eng.Prompt(t.Context(), combined); err != nil {
		t.Fatal(err)
	}
	if h.turns() != 1 || h.client.sends != 1 {
		t.Fatalf("turns=%d sends=%d", h.turns(), h.client.sends)
	}
	in := h.client.inputAt(0)
	var userText string
	for _, m := range in.Messages {
		if m.Role == llm.RoleUser {
			for _, c := range m.Content {
				if c.Kind == llm.ContentBlock && c.Block != nil && c.Block.Text != nil {
					userText += c.Block.Text.Text
				}
			}
		}
	}
	if !strings.Contains(userText, want) {
		t.Fatalf("literal payload missing from model input: %q", userText)
	}
}

func TestAutomationInputRejectsBeforeAdmission(t *testing.T) {
	for _, data := range []string{strings.Repeat("x", automationStdinLimit+1), "nul\x00byte", "bad\xffutf8"} {
		got, err := readAutomationStdin(t.Context(), strings.NewReader(data), nil)
		if err == nil || got != "" {
			t.Fatalf("failed input returned %d bytes, err=%v", len(got), err)
		}
	}
	for _, parts := range [][2]string{{"", " \n\t"}, {"/quit", "literal"}, {"/help", "\n"}} {
		if got, err := combineAutomationInput(parts[0], parts[1], false); err == nil || got != "" {
			t.Fatalf("parts=%q got=%q err=%v", parts, got, err)
		}
	}
	if got, err := combineAutomationInput("", "", true); err != nil || got != "" {
		t.Fatalf("empty resume=%q %v", got, err)
	}
	if got, err := readAutomationStdin(t.Context(), strings.NewReader(strings.Repeat("x", automationStdinLimit)), nil); err != nil || len(got) != automationStdinLimit {
		t.Fatalf("exact limit=%d %v", len(got), err)
	}
}

func TestAutomationCanceledPipeJoinsReader(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		text, err := readAutomationStdin(ctx, r, r)
		if text != "" {
			done <- errors.New("canceled read returned data")
			return
		}
		done <- err
	}()
	if _, err := w.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pipe reader did not stop")
	}
}
