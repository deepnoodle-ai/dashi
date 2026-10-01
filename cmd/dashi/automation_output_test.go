package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestAutomationMachineFinalFramingAndSecretExclusion(t *testing.T) {
	result := automationResult{Mode: "report", Answer: "first\n日本\u2028\x1b[31m", Quiet: true, RequestTurnID: "original", AnswerTurnID: "notice"}
	var finals []string
	for _, format := range []string{"json", "jsonl"} {
		var out bytes.Buffer
		writer, err := newAutomationOutput(t.Context(), format, &out)
		if err != nil {
			t.Fatal(err)
		}
		writer.Observe(dashi.Event{TurnID: "child", Seq: 8, Delta: &llm.Delta{Kind: llm.DeltaReasoning, Text: "secret-reasoning"}}, "task", "")
		writer.Observe(dashi.Event{TurnID: "child", Seq: 8, Delta: &llm.Delta{Kind: llm.DeltaToolArguments, Text: "secret-arguments"}}, "task", "")
		writer.Observe(dashi.Event{TurnID: "child", Seq: 8, Step: &dashi.Step{Kind: dashi.StepModelSettled, Model: &dashi.ModelFact{Error: "secret-error"}}}, "task", "")
		writer.Observe(dashi.Event{TurnID: "child", Seq: 8, Delta: &llm.Delta{Kind: llm.DeltaText, Text: "child preview"}}, "task", "")
		if err := writer.Finish(result); err != nil {
			t.Fatal(err)
		}
		if err := writer.Finish(result); err == nil {
			t.Fatal("accepted a second result")
		}
		if strings.Contains(out.String(), "secret-") || bytes.Contains(out.Bytes(), []byte{0x1b}) {
			t.Fatalf("unsafe output=%q", out.String())
		}
		lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
		for _, line := range lines {
			if !json.Valid([]byte(line)) {
				t.Fatalf("invalid frame %q", line)
			}
		}
		if format == "json" && len(lines) != 1 {
			t.Fatal("JSON emitted progress")
		}
		finals = append(finals, lines[len(lines)-1])
		var got automationResult
		if err := json.Unmarshal([]byte(finals[len(finals)-1]), &got); err != nil {
			t.Fatal(err)
		}
		if got.Answer != result.Answer || got.RequestTurnID != "original" || got.AnswerTurnID != "notice" {
			t.Fatalf("final=%+v", got)
		}
	}
	if finals[0] != finals[1] {
		t.Fatal("JSON and JSONL final schemas differ")
	}
}

func TestAutomationBoundedProgressGapAndReservedFinal(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	o, err := newAutomationOutput(t.Context(), "jsonl", server)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		o.Observe(dashi.Event{TurnID: "child", Seq: 9, Delta: &llm.Delta{Kind: llm.DeltaText, Text: strings.Repeat("日本", 700)}}, "task", "")
	}
	o.mu.Lock()
	if len(o.queue) > automationProgressEntries || o.queuedBytes > automationProgressBytes || o.gap.Dropped == 0 {
		t.Fatalf("queue=%d bytes=%d dropped=%d", len(o.queue), o.queuedBytes, o.gap.Dropped)
	}
	o.mu.Unlock()
	read := make(chan []byte, 1)
	go func() { data, _ := io.ReadAll(client); read <- data }()
	// Final exceeds the complete progress budget and must still arrive once.
	if err := o.Finish(automationResult{Mode: "execution", Answer: strings.Repeat("final", 60000)}); err != nil {
		t.Fatal(err)
	}
	server.Close()
	data := <-read
	var order uint64
	results, gaps := 0, 0
	for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
		var frame struct {
			Type    string
			Order   uint64
			Text    string
			Dropped uint64
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Type == "result" {
			results++
			continue
		}
		if frame.Order <= order {
			t.Fatal("non-monotonic progress order")
		}
		order = frame.Order
		if !utf8.ValidString(frame.Text) || len(frame.Text) > automationTextChunk {
			t.Fatal("invalid text chunk")
		}
		if frame.Type == "gap" {
			gaps++
			if frame.Dropped == 0 {
				t.Fatal("empty gap")
			}
		}
	}
	if results != 1 || gaps == 0 {
		t.Fatalf("results=%d gaps=%d", results, gaps)
	}
}

func TestAutomationPipeQualificationCancellationAndBrokenReader(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	qualified, err := qualifyAutomationWriter(w)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	o := startAutomationOutput(ctx, "json", qualified, time.Second)
	done := make(chan error, 1)
	go func() { done <- o.Finish(automationResult{Mode: "execution", Answer: strings.Repeat("x", 1<<20)}) }()
	// Prove writing started before cancellation interrupts the full pipe.
	if _, err := r.Read(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled pipe succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("writer leaked after cancellation")
	}
	brokenR, brokenW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer brokenW.Close()
	brokenR.Close()
	broken, err := newAutomationOutput(t.Context(), "json", brokenW)
	if err != nil {
		t.Fatal(err)
	}
	if err := broken.Finish(automationResult{Mode: "report"}); err == nil {
		t.Fatal("broken stdout succeeded")
	}
	if _, err := qualifyAutomationWriter(io.Discard); err == nil {
		t.Fatal("unqualified writer accepted")
	}
}

func TestAutomationInactivityAndHealthySlowReader(t *testing.T) {
	for _, consume := range []bool{false, true} {
		server, client := net.Pipe()
		qualified, err := qualifyAutomationWriter(server)
		if err != nil {
			t.Fatal(err)
		}
		o := startAutomationOutput(t.Context(), "json", qualified, 200*time.Millisecond)
		readDone := make(chan struct{})
		if consume {
			go func() {
				defer close(readDone)
				buf := make([]byte, automationTextChunk)
				for {
					_, err := client.Read(buf)
					if err != nil {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
		}
		err = o.Finish(automationResult{Mode: "report", Answer: strings.Repeat("x", automationTextChunk*20)})
		server.Close()
		client.Close()
		if consume {
			<-readDone
			if err != nil {
				t.Fatalf("healthy slow reader: %v", err)
			}
		} else if err == nil {
			t.Fatal("inactive reader succeeded")
		}
	}
}
