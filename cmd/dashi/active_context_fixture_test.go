package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/workspace"
)

const activeObjective = "Investigate the off-by-one bug, fix it, and run checks."
const activeDiagnostic = "ROOT_CAUSE: exclusive upper bound was treated as inclusive."
const activePrivateSummary = "PRIVATE_SUMMARY_PAYLOAD"

// This client sizes its actual prepared JSON body, including the full CLI tool
// inventory. It runs real workspace reads, one write, and one local check.
// Its summary answer copies a diagnostic only when it exists in the request.
type activeCLIClient struct {
	fakeClient
	ordinary, summaries int
	toolActions         int
	started             chan struct{}
	holdFirst           bool
}

func (c *activeCLIClient) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return llm.Request{}, err
	}
	c.mu.Lock()
	c.inputs = append(c.inputs, in)
	c.mu.Unlock()
	effective := in.Settings
	return llm.NewRequest(llm.RequestSnapshot{Origin: content.Origin{Provider: "test", API: "test.chat", Model: in.Model, Scope: "fixture"}, Input: in, Effective: effective, Body: body})
}
func (c *activeCLIClient) Send(ctx context.Context, call llm.Call) (res llm.Result, err error) {
	in := call.Request.Snapshot().Input
	defer func() { res.Requested = llm.ModelRef{Provider: "test", Model: in.Model} }()
	c.mu.Lock()
	c.sends++
	summary := len(in.Tools) == 0
	var n int
	if summary {
		c.summaries++
		n = c.summaries
	} else {
		n = c.ordinary
		c.ordinary++
	}
	c.mu.Unlock()
	if summary {
		if c.holdFirst && n == 1 {
			close(c.started)
			<-ctx.Done()
			return llm.Result{}, ctx.Err()
		}
		if !strings.Contains(inputText(in), activeDiagnostic) {
			return llm.Result{}, fmt.Errorf("summary lost early diagnostic")
		}
		return say("<summary>"+activePrivateSummary+". Objective: "+activeObjective+" Finding: "+activeDiagnostic+" Fix written; continue remaining checks.</summary>")(ctx, call)
	}
	var name string
	var args any
	switch {
	case n == 0:
		name = workspace.ToolReadFile
		args = map[string]any{"path": "diagnostic.txt"}
	case n == 1:
		name = workspace.ToolWriteFile
		args = map[string]any{"path": "fix.txt", "content": "fixed"}
	case n == 2:
		name = workspace.ToolBash
		args = map[string]any{"command": "test \"$(cat fix.txt)\" = fixed && printf 'CHECK_OK\\n' >> checks.log && cat test-output.txt"}
	case n < c.toolActions:
		name = workspace.ToolReadFile
		args = map[string]any{"path": "test-output.txt"}
	default:
		text := inputText(in)
		if !strings.Contains(text, activeObjective) || !strings.Contains(text, activeDiagnostic) {
			return llm.Result{}, fmt.Errorf("final request lost objective or diagnostic")
		}
		return say("Fixed the off-by-one bug and ran checks. "+activeDiagnostic)(ctx, call)
	}
	encoded, _ := json.Marshal(args)
	return callTool(fmt.Sprintf("coding-%d", n), name, string(encoded))(ctx, call)
}
func activeFixtureConfig(t *testing.T, interrupt bool) (appConfig, *activeCLIClient) {
	t.Helper()
	cfg := testConfig(t)
	c := &activeCLIClient{fakeClient: fakeClient{window: 65536}, started: make(chan struct{}), holdFirst: interrupt, toolActions: 7}
	cfg.Client = c
	for name, text := range map[string]string{
		"diagnostic.txt":  activeDiagnostic + "\n" + strings.Repeat("diagnostic detail retained in source record\n", 280),
		"test-output.txt": strings.Repeat("check passed; extra output retained in source record\n", 240),
	} {
		if err := os.WriteFile(filepath.Join(cfg.Dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, c
}

func TestActiveContextCLIFiveCompactionsRetainEffects(t *testing.T) {
	cfg, c := activeFixtureConfig(t, false)
	c.toolActions = 12
	a, err := buildApp(t.Context(), cfg, newRecorderUI())
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.eng.Prompt(t.Context(), activeObjective); err != nil {
		t.Fatal(err)
	}
	turn := activeAssertCompleted(t, cfg, a, c)
	view, err := turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if len(view.ContextCompactions) < 5 {
		t.Fatalf("longer scaling fixture applied %d summaries", len(view.ContextCompactions))
	}
	if view.ModelCalls != 18 || view.Spend.Usage.InputTokens != 1200 || view.Spend.Usage.OutputTokens != 240 {
		t.Fatalf("summary accounting lost: calls=%d spend=%+v", view.ModelCalls, view.Spend)
	}
	proof := fmt.Sprintf("12 finished tool actions; write=1; bash check=1; ordinary calls=%d; summary calls=%d; applications=%d; recorded model calls=%d; finished usage=%d input + %d output; no declared fixture pricing. Full source record reloaded; final objective/diagnostic retained.\n", c.ordinary, c.summaries, len(view.ContextCompactions), view.ModelCalls, view.Spend.Usage.InputTokens, view.Spend.Usage.OutputTokens)
	t.Log(proof)
	if dir := os.Getenv("DASHI_ACTIVE_CONTEXT_EVIDENCE"); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "five-compaction-proof.txt"), []byte(proof), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
func activeAssertCompleted(t *testing.T, cfg appConfig, a *app, c *activeCLIClient) dashi.Turn {
	t.Helper()
	rec, err := cfg.Store.Recorder(a.eng.Session())
	if err != nil {
		t.Fatal(err)
	}
	turn, err := rec.Load(t.Context(), session.TurnID(a.eng.Session().ID, 1))
	if err != nil {
		t.Fatal(err)
	}
	view, err := turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Stop.Kind != dashi.StopCompleted || len(view.ContextCompactions) < 2 {
		t.Fatalf("stop=%+v compactions=%d summaries=%d", view.Stop, len(view.ContextCompactions), c.summaries)
	}
	if got, err := os.ReadFile(filepath.Join(cfg.Dir, "fix.txt")); err != nil || string(got) != "fixed" {
		t.Fatalf("fix=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(cfg.Dir, "checks.log")); err != nil || string(got) != "CHECK_OK\n" {
		t.Fatalf("check effect repeated/lost: %q %v", got, err)
	}
	writes := 0
	for _, s := range turn.Steps {
		if s.Kind == dashi.StepToolStarted && s.Tool.Call.Name == workspace.ToolWriteFile {
			writes++
		}
	}
	if writes != 1 {
		t.Fatalf("write dispatched %d times", writes)
	}
	return turn
}
func TestActiveContextCLIPrintAndLineCompleteLongLoop(t *testing.T) {
	for _, mode := range []string{"print", "line"} {
		t.Run(mode, func(t *testing.T) {
			cfg, c := activeFixtureConfig(t, false)
			var out synchronizedBuffer
			var diagnostics bytes.Buffer
			var code int
			if mode == "print" {
				code = runPrint(t.Context(), cfg, Options{Prompt: activeObjective, Run: true, Yes: true}, &out, &diagnostics)
			} else {
				reader, writer := io.Pipe()
				go func() {
					defer writer.Close()
					fmt.Fprintln(writer, activeObjective)
					answered := 0
					deadline := time.Now().Add(40 * time.Second)
					for time.Now().Before(deadline) {
						text := out.text()
						pending := strings.Count(text, "[y/a/n or n REASON]")
						if pending > answered {
							fmt.Fprintln(writer, "y")
							answered++
						}
						if strings.Contains(text, "Fixed the off-by-one bug") {
							return
						}
						time.Sleep(10 * time.Millisecond)
					}
				}()
				code = runLines(t.Context(), cfg, Options{Plain: true}, reader, &out, &diagnostics)
			}
			if code != 0 {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, out.text(), diagnostics.String())
			}
			if strings.Contains(out.text()+diagnostics.String(), activePrivateSummary) {
				t.Fatal("summary escaped into output")
			}
			if mode == "print" && out.text() != "Fixed the off-by-one bug and ran checks. "+activeDiagnostic+"\n" {
				t.Fatalf("dirty print stdout: %q", out.text())
			}
			if c.summaries < 2 {
				t.Fatalf("only %d summaries", c.summaries)
			}
			status := out.text() + diagnostics.String()
			compactionMustHave(t, status, "Compacting this Turn", "estimated input", "Full record retained")
			// Reopen the persisted completed Turn with no further model work.
			ui := newRecorderUI()
			a, err := buildApp(t.Context(), cfg, ui)
			if err != nil {
				t.Fatal(err)
			}
			defer a.eng.Close()
			if _, err = a.openSession(cfg.Store, cfg.Dir, Options{Resume: true}); err != nil {
				t.Fatal(err)
			}
			a.ctrl.Open()
			activeAssertCompleted(t, cfg, a, c)
			compactionMustHave(t, ui.text(), "estimated input", "Full record retained")
			if strings.Contains(ui.text(), activePrivateSummary) {
				t.Fatal("record replay leaked summary")
			}
			if dir := os.Getenv("DASHI_ACTIVE_CONTEXT_EVIDENCE"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				text := fmt.Sprintf("mode=%s window=65536 summaries=%d ordinary=%d\nstdout:\n%s\nstderr:\n%s\nreopened:\n%s", mode, c.summaries, c.ordinary, out.text(), diagnostics.String(), ui.text())
				if err := os.WriteFile(filepath.Join(dir, mode+"-long-loop.txt"), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
