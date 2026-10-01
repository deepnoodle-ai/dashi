package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
)

// Keep the realistic full prepared-body encoding while answering one native
// image request. The fixture runs the existing pending Start engine path;
// attachment selection and its future input API belong to their own track.
type mediaCLIClient struct{ *activeCLIClient }

func (c *mediaCLIClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	return c.activeCLIClient.fakeClient.Send(ctx, call)
}
func cliMediaPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	seed := uint32(1)
	for i := range img.Pix {
		seed = 1664525*seed + 1013904223
		img.Pix[i] = byte(seed >> 24)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(encoded.Bytes())); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}
func mediaMarkers(blocks []Block) []Block {
	var result []Block
	for _, b := range blocks {
		if b.Kind == KindCompaction && b.State == "unavailable" && strings.Contains(b.Text, "unavailable for media") {
			result = append(result, b)
		}
	}
	return result
}

func installNativeMediaPrompt(e *Engine, data []byte) {
	start := dashi.Say("Inspect this image.")
	start.CommandID = dashi.NewID()
	start.Agent = e.agent()
	start.Input[0].Content = append(start.Input[0].Content, llm.Image(content.Source{Data: data, MIMEType: "image/png"}))
	e.inputMu.Lock()
	e.pending = &pendingPrompt{start: start}
	e.inputMu.Unlock()
}
func TestCLIActiveMediaPrintAndRecordedReopen(t *testing.T) {
	data := cliMediaPNG(t)
	for _, window := range []int{128000, 200000} {
		t.Run(fmt.Sprint(window), func(t *testing.T) {
			cfg := testConfig(t)
			c := &mediaCLIClient{activeCLIClient: &activeCLIClient{fakeClient: fakeClient{window: window, script: []reply{say("Image inspected.")}}}}
			cfg.Client = c
			var stdout, stderr bytes.Buffer
			a, err := buildApp(t.Context(), cfg, &printUI{err: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			defer a.eng.Close()
			if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
				t.Fatal(err)
			}
			installNativeMediaPrompt(a.eng, data)
			res, err := a.eng.runPending(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			writer, err := newAutomationOutput(t.Context(), "text", &stdout)
			if err != nil {
				t.Fatal(err)
			}
			if err := finishAutomationContent(writer, res.Outcome.Answer); err != nil {
				t.Fatal(err)
			}
			if stdout.String() != "Image inspected.\n" || c.sends != 1 || res.Outcome.Stop.Kind != dashi.StopCompleted || len(res.Outcome.ContextCompactions) != 0 {
				t.Fatalf("stdout=%q sends=%d outcome=%+v", stdout.String(), c.sends, res.Outcome.Stop)
			}
			req, err := c.Prepare(t.Context(), c.inputAt(0))
			if err != nil {
				t.Fatal(err)
			}
			estimate := llm.EstimatedInputTokens(req)
			if len(data) < 1<<20 || estimate <= window {
				t.Fatalf("image too small: PNG=%d estimate=%d", len(data), estimate)
			}
			if strings.Count(stderr.String(), "Active-Turn protection unavailable for media") != 1 {
				t.Fatalf("live warning missing/repeated: %s", stderr.String())
			}
			if strings.Contains(stderr.String(), "text-only") {
				t.Fatalf("per-Turn protection notice shown: %s", stderr.String())
			}
			compactionMustHave(t, stderr.String(), "cannot estimate", "provider decides whether it fits", "cost limits still apply")
			rec, err := cfg.Store.Recorder(a.eng.Session())
			if err != nil {
				t.Fatal(err)
			}
			turn, err := rec.Load(t.Context(), session.TurnID(a.eng.Session().ID, 1))
			if err != nil {
				t.Fatal(err)
			}
			view, err := dashi.Replay(turn)
			if err != nil {
				t.Fatal(err)
			}
			warnings := 0
			for _, w := range view.Warnings {
				if w.Code == dashi.WarningActiveCompactionMediaEstimateUnavailable {
					warnings++
				}
			}
			if warnings != 1 || view.ActiveCompactionWindow != window {
				t.Fatalf("warning=%d window=%d", warnings, view.ActiveCompactionWindow)
			}
			blocks, err := RenderTurn(1, "fixture", cfg.Dir, turn, nil)
			if err != nil {
				t.Fatal(err)
			}
			var replay Transcript
			for _, b := range blocks {
				replay.Apply(b)
			}
			markers := mediaMarkers(replay.Blocks)
			if len(markers) != 1 || markers[0].ID != "active-protection:"+turn.ID || !markers[0].Done {
				t.Fatalf("replay marker lost identity: %+v", markers)
			}
			// Rebuild the app and reopen the file-recorded result without live model work.
			a.eng.Close()
			ui := newRecorderUI()
			reopened, err := buildApp(t.Context(), cfg, ui)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.eng.Close()
			if _, err = reopened.openSession(cfg.Store, cfg.Dir, Options{Resume: true}); err != nil {
				t.Fatal(err)
			}
			reopened.ctrl.Open()
			if c.sends != 1 || len(mediaMarkers(ui.blocks())) != 1 {
				t.Fatalf("reopen repeated work/marker: sends=%d text=%s", c.sends, ui.text())
			}
			proof := fmt.Sprintf("valid PNG=%d bytes; full prepared estimate=%d; pinned window=%d; Send=1; stop=completed; applied summaries=0; recorded/replayed media warning=1; reopened sends=1.\nstdout:\n%s\nstderr:\n%s\nreopened:\n%s", len(data), estimate, window, stdout.String(), stderr.String(), ui.text())
			t.Logf("PNG=%d bytes estimate=%d window=%d; Send=1, one unavailable marker live/replay", len(data), estimate, window)
			if dir := os.Getenv("DASHI_ACTIVE_CONTEXT_EVIDENCE"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("media-%d-print-reopen.txt", window)), []byte(proof), 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRuntimeActiveMediaUnavailableAndReopen(t *testing.T) {
	cfg := testConfig(t)
	c := &mediaCLIClient{activeCLIClient: &activeCLIClient{fakeClient: fakeClient{window: 128000, script: []reply{say("Image inspected.")}}}}
	cfg.Client = c
	h := newTranscriptRuntimeConfig(t, cfg)
	installNativeMediaPrompt(h.app.eng, cliMediaPNG(t))
	h.app.ctrl.startRun(h.app.eng.runPending)
	h.app.ctrl.Wait()
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeIdle && strings.Contains(s.text, "unavailable for media") && strings.Contains(s.text, "Image inspected.")
	})
	id := h.app.eng.Session().ID
	h.app.ctrl.openSession(id)
	h.app.ctrl.Wait()
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeIdle && strings.Contains(s.text, "unavailable for media") && strings.Contains(s.text, "Image inspected.")
	})
	c.mu.Lock()
	sends := c.sends
	c.mu.Unlock()
	if sends != 1 {
		t.Fatalf("reopened media request made %d sends", sends)
	}
	exportActiveRuntime(t, h, "media-unavailable-reopen")
}
func TestActiveMediaRendererMarksUnavailableMedia(t *testing.T) {
	r := NewRenderer(1, "fixture", "")
	started := dashi.Step{Kind: dashi.StepStarted, TurnID: "media", Started: &dashi.StartFact{Agent: dashi.Agent{ActiveCompaction: &dashi.ActiveCompactionPolicy{}}, ActiveCompactionWindow: 128000}}
	var live Transcript
	for _, b := range r.Event(stepEvent(started)) {
		live.Apply(b)
	}
	// The recorded window is inspection detail, not a per-Turn notice.
	if len(live.Blocks) != 1 || live.Blocks[0].Kind != KindUser {
		t.Fatalf("start showed more than the prompt: %+v", live.Blocks)
	}
	event := dashi.Step{Kind: dashi.StepModelIntent, TurnID: "media", Warnings: []dashi.Warning{{Code: dashi.WarningActiveCompactionMediaEstimateUnavailable, Message: "Media context size cannot be estimated; provider content preparation and actual window acceptance govern it."}}}
	b := onlyBlock(t, r.Event(stepEvent(event)))
	live.Apply(b)
	if b.Kind != KindCompaction || b.State != "unavailable" || !b.Done || b.ID != "active-protection:media" {
		t.Fatalf("marker lost state/identity: %+v", b)
	}
	compactionMustHave(t, b.Text, "unavailable for media", "cannot estimate", "provider decides whether it fits")
	// Re-delivering a display event replaces the same marker rather than adding one.
	live.Apply(b)
	if len(mediaMarkers(live.Blocks)) != 1 {
		t.Fatal("per-Turn display marker duplicated")
	}
	if got := r.Event(dashi.Event{AttemptID: "answer", Delta: &llm.Delta{Kind: llm.DeltaText, Text: "Visible answer."}}); len(got) != 1 || got[0].Kind != KindAssistant {
		t.Fatal("unavailable estimate suppressed ordinary answer")
	}
}
