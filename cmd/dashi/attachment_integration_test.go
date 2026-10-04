package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
	"github.com/deepnoodle-ai/dashi/content/mention"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/session"
)

func fixtureSelectedText(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
func hydratedDocuments(in llm.Input) []string {
	var out []string
	for _, m := range in.Messages {
		for _, part := range m.Content {
			if part.Block != nil && part.Block.Document != nil {
				out = append(out, string(part.Block.Document.Source.Data))
			}
		}
	}
	return out
}
func TestAttachmentPrintFlagsMentionsAndRecordedRefs(t *testing.T) {
	cfg := testConfig(t, say("Evidence received."))
	fixtureSelectedText(t, cfg.Dir, "first.txt", "flag first")
	fixtureSelectedText(t, cfg.Dir, "second.txt", "mention second")
	var stdout, stderr bytes.Buffer
	code := runPrint(t.Context(), cfg, Options{Prompt: "explain @second.txt @first.txt", Run: true, AttachFiles: []string{"first.txt"}}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("%d %s", code, stderr.String())
	}
	client := cfg.Client.(*fakeClient)
	got := hydratedDocuments(client.inputAt(len(client.inputs) - 1))
	if len(got) != 2 || got[0] != "flag first" || got[1] != "mention second" {
		t.Fatalf("hydrated=%v", got)
	}
	s, err := cfg.Store.Latest(cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := cfg.Store.Recorder(s)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := rec.Load(t.Context(), session.TurnID(s.ID, 1))
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range turn.Steps[0].Started.Input[0].Content {
		if part.Block != nil && part.Block.Document != nil {
			src := part.Block.Document.Source
			if src.Ref == nil || len(src.Data) > 0 || src.URL != "" {
				t.Fatalf("recorded source=%+v", src)
			}
		}
	}
	if strings.Contains(joinMessages(turn.Steps[0].Started.Input), cfg.Dir) {
		t.Fatal("source path persisted in label")
	}
}
func TestAttachmentPrintFailureStartsNoWork(t *testing.T) {
	cfg := testConfig(t, say("must not run"))
	fixtureSelectedText(t, cfg.Dir, "valid.txt", "evidence")
	var stdout, stderr bytes.Buffer
	if code := runPrint(t.Context(), cfg, Options{Prompt: "go @missing.txt", Run: true, AttachFiles: []string{"valid.txt"}}, &stdout, &stderr); code != 1 {
		t.Fatal(code)
	}
	if cfg.Client.(*fakeClient).sends != 0 || stdout.Len() != 0 {
		t.Fatal("failed selection dispatched")
	}
	if _, err := cfg.Store.Latest(cfg.Dir); !errors.Is(err, sessionstore.ErrNotFound) {
		t.Fatalf("failed selection recorded session: %v", err)
	}
}
func TestAttachmentLineTwoStepDetachAndPlainNextPrompt(t *testing.T) {
	oldTerminal := isTerminal
	isTerminal = func(*os.File) bool { return false }
	t.Cleanup(func() { isTerminal = oldTerminal })
	cfg := testConfig(t, say("selected"), say("plain"))
	fixtureSelectedText(t, cfg.Dir, "notes.txt", "frozen note")
	var stdout, stderr bytes.Buffer
	input := "inspect @missing.txt @notes.txt\n/detach 1\n\nplain prompt\n"
	if code := runLines(t.Context(), cfg, Options{Plain: true}, strings.NewReader(input), &stdout, &stderr); code != 0 {
		t.Fatalf("%d %s", code, stderr.String())
	}
	client := cfg.Client.(*fakeClient)
	if client.sends != 2 {
		t.Fatalf("sends=%d\n%s", client.sends, stdout.String())
	}
	if docs := hydratedDocuments(client.inputAt(0)); len(docs) != 1 || docs[0] != "frozen note" {
		t.Fatalf("%v", docs)
	}
	// History retains one prior document; the new ordinary request adds none.
	last := client.inputAt(len(client.inputs) - 1)
	tail := last.Messages[len(last.Messages)-1]
	if len(tail.Content) != 1 || tail.Content[0].Block.Text.Text != "plain prompt" {
		t.Fatalf("tail=%+v", tail)
	}
	mustHave(t, stdout.String(), "2. ready")
	mustHave(t, stdout.String(), "Review the selections")
}
func TestAttachmentScreenPreviewConsumptionAndRunningRefusal(t *testing.T) {
	started := make(chan struct{})
	h := newScreenHarness(t, 0, blockUntilDone(started), say("continued"))
	fixtureSelectedText(t, h.dir, "notes.txt", "snapshot")
	raw := "explain @notes.txt"
	h.submit(raw)
	h.app.ctrl.Wait()
	h.waitFor(func() bool {
		return len(h.scr.attachments.entries) == 1 && h.scr.attachments.entries[0].state == attachmentReady
	})
	if h.client.sends != 0 || !h.check(func() bool { return h.scr.input == raw }) {
		t.Fatal("first Enter submitted or discarded draft")
	}
	h.submit(raw)
	<-started
	h.waitFor(func() bool { return len(h.scr.attachments.entries) == 0 && h.scr.input == "" })
	h.submit("new @notes.txt")
	if h.client.sends != 1 {
		t.Fatal("running attachment sent")
	}
	mustHave(t, h.render(), "fresh idle submission")
	h.app.ctrl.CancelWork()
	h.app.ctrl.Wait()
	// Continue uses the recorded reference and preserves independent input.
	h.mu.Lock()
	h.scr.input = "independent draft"
	h.mu.Unlock()
	h.app.ctrl.resume() // Continue from recorded input; no new submission
	h.app.ctrl.Wait()
	if !h.check(func() bool { return h.scr.input == "independent draft" }) {
		t.Fatal("Continue consumed independent draft")
	}
}

func TestAttachmentReopenAfterSourceDeletion(t *testing.T) {
	started := make(chan struct{})
	cfg := testConfig(t, blockUntilDone(started), say("recovered"))
	path := fixtureSelectedText(t, cfg.Dir, "notes.txt", "original frozen content")
	u := newLineUI(&bytes.Buffer{})
	a, err := buildApp(t.Context(), cfg, u)
	if err != nil {
		t.Fatal(err)
	}
	u.ctrl = a.ctrl
	if _, err := a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	in, err := printAttachmentInput(t.Context(), cfg.Dir, "inspect", []string{path})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := a.eng.PromptInput(ctx, in); done <- err }()
	<-started
	cancel()
	<-done
	s := a.eng.Session()
	a.eng.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !cfg.Store.RetainsAttachments(s) {
		t.Fatal("snapshot not retained")
	}
	a2, err := buildApp(t.Context(), cfg, newLineUI(&bytes.Buffer{}))
	if err != nil {
		t.Fatal(err)
	}
	defer a2.eng.Close()
	if _, err := a2.openSession(cfg.Store, cfg.Dir, Options{Resume: true, ResumeID: s.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := a2.eng.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	client := cfg.Client.(*fakeClient)
	docs := hydratedDocuments(client.inputAt(len(client.inputs) - 1))
	if len(docs) != 1 || docs[0] != "original frozen content" {
		t.Fatalf("%v", docs)
	}
}

func TestAttachmentUnknownStartRetainsAndResolvesFullInput(t *testing.T) {
	cfg := testConfig(t, say("delivered once"))
	fixtureSelectedText(t, cfg.Dir, "notes.txt", "frozen")
	var lost *lostStartRecord
	cfg.recorder = func(r *filerecorder.Recorder) dashi.Recorder {
		lost = &lostStartRecord{Recorder: r, unreadable: true}
		return lost
	}
	u := newLineUI(&bytes.Buffer{})
	a, err := buildApp(t.Context(), cfg, u)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	u.ctrl = a.ctrl
	if _, err := a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	u.AttachmentCommand("/attach", "notes.txt")
	u.AttachmentInput("explain")
	a.ctrl.Wait()
	u.attachmentMu.Lock()
	u.drainAttachmentReceiptsLocked()
	in := u.attachmentInput
	if u.attachments.frozenBy == "" || !in.unknown {
		t.Fatal("unknown Start lost draft")
	}
	snapshot := in.snapshots[0].id
	u.attachmentMu.Unlock()
	u.AttachmentInput("")
	a.ctrl.Wait()
	if cfg.Client.(*fakeClient).sends != 0 {
		t.Fatal("unreadable unknown result dispatched")
	}
	lost.mu.Lock()
	lost.unreadable = false
	lost.mu.Unlock()
	u.AttachmentInput("")
	a.ctrl.Wait()
	u.attachmentMu.Lock()
	u.drainAttachmentReceiptsLocked()
	if u.attachments.unsaved() {
		t.Fatal("checked Start did not consume draft")
	}
	u.attachmentMu.Unlock()
	if cfg.Client.(*fakeClient).sends != 1 || a.eng.Turns(t.Context()) != 1 {
		t.Fatal("duplicate Start/model call")
	}
	rec, _ := cfg.Store.Recorder(a.eng.Session())
	turn, err := rec.Load(t.Context(), session.TurnID(a.eng.Session().ID, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := turn.Steps[0].Started.Input[0].Content[1].Block.Document.Source.Ref.ID; got != snapshot {
		t.Fatalf("%s != %s", got, snapshot)
	}
}

// A valid single-page PDF with an uncompressed ~1 MiB comment stream.
func realisticAttachmentPDF() []byte {
	stream := "BT /F1 12 Tf 50 750 Td (Attachment evidence) Tj ET\n" + strings.Repeat("% "+strings.Repeat("x", 75)+"\n", 13500)
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream)}
	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n")
	offsets := []int{0}
	for i, obj := range objects {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return out.Bytes()
}

func realisticAttachmentPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 640, 600))
	rand.New(rand.NewSource(7)).Read(img.Pix)
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if buf.Len() < 1<<20 {
		t.Fatalf("PNG fixture unexpectedly small: %d", buf.Len())
	}
	return buf.Bytes()
}

// Full prepared bodies exercise the accepted parent media-acceptance policy.
func TestAttachmentRealisticMediaKnownWindowCheck(t *testing.T) {
	for _, kind := range []string{"png", "pdf"} {
		for _, window := range []int{128000, 200000} {
			t.Run(fmt.Sprintf("%s/%d", kind, window), func(t *testing.T) {
				cfg := testConfig(t)
				c := &mediaCLIClient{activeCLIClient: &activeCLIClient{fakeClient: fakeClient{window: window, script: []reply{say("Media received")}}}}
				cfg.Client = c
				data := realisticAttachmentPDF()
				if kind == "png" {
					data = realisticAttachmentPNG(t)
				}
				path := filepath.Join(cfg.Dir, "evidence."+kind)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				var stdout, stderr bytes.Buffer
				if code := runPrint(t.Context(), cfg, Options{Prompt: "read evidence", Run: true, AttachFiles: []string{path}}, &stdout, &stderr); code != 0 {
					t.Fatalf("%d: %s", code, stderr.String())
				}
				if c.sends != 1 {
					t.Fatal("realistic media did not reach Send")
				}
				req, err := c.Prepare(t.Context(), c.inputAt(0))
				if err != nil {
					t.Fatal(err)
				}
				if estimate := llm.EstimatedInputTokens(req); estimate <= window {
					t.Fatalf("full media body did not exercise window: bytes=%d estimate=%d window=%d", len(data), estimate, window)
				}
				if strings.Count(stderr.String(), "Active-Turn protection unavailable for media") != 1 {
					t.Fatal("missing media acceptance warning", stderr.String())
				}
				t.Logf("bytes=%d prepared estimate=%d window=%d Send=1", len(data), llm.EstimatedInputTokens(req), window)
				if dir := os.Getenv("DASHI_ATTACHMENT_TRANSCRIPTS"); dir != "" {
					proof := fmt.Sprintf("kind=%s bytes=%d prepared estimate=%d window=%d Send=1\nstdout:\n%s\nstderr:\n%s", kind, len(data), llm.EstimatedInputTokens(req), window, stdout.String(), stderr.String())
					if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("media-%s-%d-admission.txt", kind, window)), []byte(proof), 0600); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

type attachmentAcceptFault struct {
	*filerecorder.Recorder
	mode   string
	failed bool
	target string
}

func (r *attachmentAcceptFault) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if !r.failed && req.Step.Kind == dashi.StepTurnStarted && req.Step.Started.Origin != dashi.StartCompaction {
		r.failed = true
		r.target = req.Step.TurnID
		if r.mode == "conflict" {
			if err := r.Recorder.Commit(ctx, req); err != nil {
				return err
			}
		}
		return &dashi.CommitError{NothingWritten: r.mode == "rejected", Err: errors.New("fixture recording fault")}
	}
	return r.Recorder.Commit(ctx, req)
}
func (r *attachmentAcceptFault) Load(ctx context.Context, id string) (dashi.Turn, error) {
	turn, err := r.Recorder.Load(ctx, id)
	if err == nil && r.mode == "conflict" && id == r.target && len(turn.Steps) > 0 {
		turn.Steps[0].Started.Input = []llm.Message{llm.UserMessage(llm.Text("conflicting recorded input"))}
	}
	return turn, err
}
func TestAttachmentRejectedAbsentAndConflictingAcceptance(t *testing.T) {
	for _, mode := range []string{"rejected", "absent", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfig(t, say("delivered once"))
			fixtureSelectedText(t, cfg.Dir, "notes.txt", "frozen")
			cfg.recorder = func(r *filerecorder.Recorder) dashi.Recorder {
				return &attachmentAcceptFault{Recorder: r, mode: mode}
			}
			u := newLineUI(&bytes.Buffer{})
			a, err := buildApp(t.Context(), cfg, u)
			if err != nil {
				t.Fatal(err)
			}
			defer a.eng.Close()
			u.ctrl = a.ctrl
			if _, err := a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
				t.Fatal(err)
			}
			u.AttachmentCommand("/attach", "notes.txt")
			u.AttachmentInput("explain")
			a.ctrl.Wait()
			u.attachmentMu.Lock()
			u.drainAttachmentReceiptsLocked()
			in := u.attachmentInput
			snapshot := in.snapshots[0].id
			if mode == "rejected" && u.attachments.frozenBy != "" {
				t.Fatal("known rejection did not release draft")
			}
			if mode != "rejected" && !in.unknown {
				t.Fatal("unknown did not block acceptance")
			}
			u.attachmentMu.Unlock()
			u.AttachmentInput("")
			a.ctrl.Wait()
			u.attachmentMu.Lock()
			u.drainAttachmentReceiptsLocked()
			remaining := len(u.attachments.entries)
			u.attachmentMu.Unlock()
			if mode == "conflict" {
				if cfg.Client.(*fakeClient).sends != 0 || remaining != 1 || !a.eng.HasPending() {
					t.Fatal("conflict accepted or dropped request")
				}
				return
			}
			if cfg.Client.(*fakeClient).sends != 1 || remaining != 0 || a.eng.Turns(t.Context()) != 1 {
				t.Fatal("retry duplicated or lost request")
			}
			rec, _ := cfg.Store.Recorder(a.eng.Session())
			turn, err := rec.Load(t.Context(), session.TurnID(a.eng.Session().ID, 1))
			if err != nil {
				t.Fatal(err)
			}
			if turn.Steps[0].Started.Input[0].Content[1].Block.Document.Source.Ref.ID != snapshot {
				t.Fatal("publication identity changed on retry")
			}
		})
	}
}

type attachmentCrashRecorder struct {
	*filerecorder.Recorder
	state string
}

func (r *attachmentCrashRecorder) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if err := r.Recorder.Commit(ctx, req); err != nil {
		return err
	}
	if req.Step.Kind == dashi.StepTurnStarted && req.Step.Started.Origin != dashi.StartCompaction {
		id := req.Step.TurnID[:strings.LastIndexByte(req.Step.TurnID, '-')]
		if err := os.WriteFile(r.state, []byte(id), 0o600); err != nil {
			return err
		}
		os.Exit(17) // Real process loss after durable Start, before acknowledgement/model dispatch.
	}
	return nil
}
func TestAttachmentCrashChild(t *testing.T) {
	root := os.Getenv("DASHI_ATTACHMENT_CRASH_ROOT")
	if root == "" {
		t.Skip("subprocess crash fixture")
	}
	cfg := testConfig(t)
	cfg.Dir = os.Getenv("DASHI_ATTACHMENT_CRASH_WORKSPACE")
	cfg.Store = sessionstore.NewStore(root)
	cfg.recorder = func(r *filerecorder.Recorder) dashi.Recorder {
		return &attachmentCrashRecorder{r, os.Getenv("DASHI_ATTACHMENT_CRASH_STATE")}
	}
	a, err := buildApp(t.Context(), cfg, newLineUI(&bytes.Buffer{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	in, err := printAttachmentInput(t.Context(), cfg.Dir, "inspect", []string{"notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	a.eng.PromptInput(t.Context(), in)
	t.Fatal("child did not crash")
}
func TestAttachmentRealProcessCrashSourceDeletionAndReopen(t *testing.T) {
	if attachmentPlatformUnsupported() {
		t.Skip("secure snapshot platform")
	}
	workspace := t.TempDir()
	path := fixtureSelectedText(t, workspace, "notes.txt", "crash-stable evidence")
	root := filepath.Join(t.TempDir(), "store")
	state := filepath.Join(t.TempDir(), "session-id")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAttachmentCrashChild$", "-test.v")
	child.Env = append(os.Environ(), "DASHI_ATTACHMENT_CRASH_ROOT="+root, "DASHI_ATTACHMENT_CRASH_WORKSPACE="+workspace, "DASHI_ATTACHMENT_CRASH_STATE="+state)
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 17 {
		t.Fatalf("crash child: %v\n%s", err, output)
	}
	id, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, say("continued after crash"))
	cfg.Dir = workspace
	cfg.Store = sessionstore.NewStore(root)
	a, err := buildApp(t.Context(), cfg, newLineUI(&bytes.Buffer{}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	if _, err := a.openSession(cfg.Store, workspace, Options{Resume: true, ResumeID: string(id)}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.eng.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	client := cfg.Client.(*fakeClient)
	if client.sends != 1 || a.eng.Turns(t.Context()) != 1 {
		t.Fatal("crash recovery made a new Start")
	}
	got := hydratedDocuments(client.inputAt(len(client.inputs) - 1))
	if len(got) != 1 || got[0] != "crash-stable evidence" {
		t.Fatalf("%v", got)
	}
}

func TestAttachmentLinePrintEvidenceChild(t *testing.T) {
	dir := os.Getenv("DASHI_ATTACHMENT_TRANSCRIPTS")
	if dir == "" {
		t.Skip("actual line/print transcript capture helper")
	}
	for _, fixture := range []struct {
		name string
		data []byte
	}{{"screen.png", realisticAttachmentPNG(t)}, {"evidence.pdf", realisticAttachmentPDF()}} {
		path := filepath.Join(dir, fixture.name)
		if err := os.WriteFile(path, fixture.data, 0600); err != nil {
			t.Fatal(err)
		}
		file, err := mention.ReadFileContext(t.Context(), path, mention.Options{})
		if err != nil || file.Attachment.Size != int64(len(fixture.data)) {
			t.Fatal("realistic selection failed", fixture.name, err)
		}
	}
	oldTerminal := isTerminal
	isTerminal = func(*os.File) bool { return false }
	t.Cleanup(func() { isTerminal = oldTerminal })
	cfg := testConfig(t)
	cfg.Client = &mediaCLIClient{activeCLIClient: &activeCLIClient{fakeClient: fakeClient{window: 128000, script: []reply{say("Selected note received."), say("Ordinary next prompt received.")}}}}
	fixtureSelectedText(t, cfg.Dir, "notes.txt", "original line note")
	input := "inspect @missing.txt @notes.txt\n/detach 1\n\nordinary next prompt\n"
	var stdout, stderr bytes.Buffer
	code := runLines(t.Context(), cfg, Options{Plain: true}, strings.NewReader(input), &stdout, &stderr)
	if code != 0 {
		t.Fatal(code, stderr.String())
	}
	if strings.Contains(stderr.String(), "unavailable for media") {
		t.Fatal("plain text emitted media warning", stderr.String())
	}
	text := fmt.Sprintf("full prepared JSON; pinned window=128000; Send=2; text media warnings=0\nstdin:\n%s\nstdout:\n%s\nstderr:\n%s\nexit: %d\n", input, stdout.String(), stderr.String(), code)
	if err := os.WriteFile(filepath.Join(dir, "line-transcript.txt"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg = testConfig(t)
	cfg.Client = &mediaCLIClient{activeCLIClient: &activeCLIClient{fakeClient: fakeClient{window: 128000, script: []reply{say("Both original print notes received.")}}}}
	fixtureSelectedText(t, cfg.Dir, "flag.txt", "flag first")
	fixtureSelectedText(t, cfg.Dir, "mention.txt", "mention second")
	stdout.Reset()
	stderr.Reset()
	code = runPrint(t.Context(), cfg, Options{Prompt: "compare @mention.txt", Run: true, AttachFiles: []string{"flag.txt"}}, &stdout, &stderr)
	if code != 0 {
		t.Fatal(code, stderr.String())
	}
	if strings.Contains(stderr.String(), "unavailable for media") {
		t.Fatal("plain text emitted media warning", stderr.String())
	}
	text = fmt.Sprintf("full prepared JSON; pinned window=128000; Send=1; text media warnings=0\narguments: run 'compare @mention.txt' --attach flag.txt\nstdout:\n%s\nstderr:\n%s\nexit: %d\n", stdout.String(), stderr.String(), code)
	if err := os.WriteFile(filepath.Join(dir, "print-transcript.txt"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = runPrint(t.Context(), cfg, Options{Prompt: "inspect @missing.txt", Run: true}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 {
		t.Fatal("failed print selection output", code, stdout.String())
	}
	text = fmt.Sprintf("arguments: run 'inspect @missing.txt'\nstdout:\n%s\nstderr:\n%s\nexit: %d\n", stdout.String(), stderr.String(), code)
	if err := os.WriteFile(filepath.Join(dir, "print-failure-transcript.txt"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAttachmentSelectionDoesNotGrantToolAccess(t *testing.T) {
	outside := fixtureSelectedText(t, t.TempDir(), "outside.txt", "selected outside snapshot")
	args, _ := json.Marshal(map[string]string{"path": outside})
	cfg := testConfig(t, callTool("outside", "read_file", string(args)), callTool("write", "write_file", `{"path":"protected.txt","content":"changed"}`), say("Finished with denied tool access."))
	path := fixtureSelectedText(t, cfg.Dir, "protected.txt", "original")
	var stdout, stderr bytes.Buffer
	if code := runPrint(t.Context(), cfg, Options{Prompt: "inspect selected note", Run: true, AttachFiles: []string{outside}}, &stdout, &stderr); code != 0 {
		t.Fatal(code, stderr.String())
	}
	client := cfg.Client.(*fakeClient)
	if got := hydratedDocuments(client.inputAt(0)); len(got) != 1 || got[0] != "selected outside snapshot" {
		t.Fatal("explicit selection not provided", got)
	}
	failed := map[string]bool{}
	for _, m := range client.inputAt(len(client.inputs) - 1).Messages {
		for _, c := range m.Content {
			if c.ToolResult != nil {
				failed[c.ToolResult.CallID] = c.ToolResult.IsError
			}
		}
	}
	if !failed["outside"] || !failed["write"] {
		t.Fatal("selection widened tool authority", failed)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "original" {
		t.Fatal("write ran without normal approval", err, string(data))
	}
}

func TestAttachmentQueueRefusesMentionsWithoutReading(t *testing.T) {
	started := make(chan struct{})
	h := newScreenHarness(t, 0, blockUntilDone(started))
	h.app.ctrl.Input("/queue inspect @missing.txt")
	h.app.ctrl.Wait()
	if h.client.sends != 0 || h.app.eng.Queued() != 0 {
		t.Fatal("idle /queue accepted attachment-bearing text")
	}
	h.submit("ordinary running request")
	<-started
	h.app.ctrl.Input("/queue inspect @missing.txt")
	h.app.ctrl.Input("!inspect @missing.txt")
	if h.client.sends != 1 || h.app.eng.Queued() != 0 {
		t.Fatal("running /queue or steer accepted attachment-bearing text")
	}
	h.app.ctrl.CancelWork()
	h.app.ctrl.Wait()
}

func TestAttachmentCompactionInterruptPreservesOwnedInput(t *testing.T) {
	started := make(chan struct{})
	h := newScreenHarness(t, 0, fullWindow, blockUntilDone(started), say("<summary>Retained project facts.</summary>"), say("Original attachment answered."))
	compactWindow(h.client)
	h.submit(compactionSeed())
	h.app.ctrl.Wait()
	fixtureSelectedText(t, h.dir, "notes.txt", "snapshot waiting behind summary")
	raw := "inspect @notes.txt"
	h.submit(raw)
	h.app.ctrl.Wait()
	h.waitFor(func() bool {
		return len(h.scr.attachments.entries) == 1 && h.scr.attachments.entries[0].state == attachmentReady
	})
	h.submit(raw)
	<-started
	h.waitFor(func() bool { return h.scr.attachments.frozenBy != "" && h.scr.input == raw })
	h.app.ctrl.CancelWork()
	h.app.ctrl.Wait()
	if !h.app.eng.HasPending() || !h.check(func() bool {
		return h.scr.input == raw && len(h.scr.attachments.entries) == 1 && h.scr.attachments.frozenBy != ""
	}) {
		t.Fatal("summary interruption consumed attachment draft")
	}
	h.submit("")
	h.app.ctrl.Wait()
	h.waitFor(func() bool { return len(h.scr.attachments.entries) == 0 && h.scr.input == "" })
	if h.turns() != 3 || h.client.sends != 4 {
		t.Fatalf("turns=%d sends=%d", h.turns(), h.client.sends)
	}
	got := hydratedDocuments(h.client.inputAt(len(h.client.inputs) - 1))
	if len(got) != 1 || got[0] != "snapshot waiting behind summary" {
		t.Fatal("resumed prompt lost full attachment input", got)
	}
}

type attachmentPricedClient struct{ *mediaCLIClient }

func (c *attachmentPricedClient) Prepare(ctx context.Context, in llm.Input) (llm.Request, error) {
	req, err := c.mediaCLIClient.Prepare(ctx, in)
	if err != nil {
		return llm.Request{}, err
	}
	snap := req.Snapshot()
	if snap.ModelInfo == nil {
		info, _ := c.ModelInfo(in.Model)
		info.Evidence = llm.ModelEvidence{Model: in.Model, API: "test.chat", Endpoint: "https://example.test/model", Owner: "test", Source: "attachment fixture", Method: llm.EvidenceFixture, QualifiedAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)}
		snap.ModelInfo = &info
	}
	snap.ModelInfo.Pricing = &llm.Pricing{Currency: "USD", UpdatedAt: "2026-09-30", Base: llm.Rates{Input: money.MustParseAmount("1"), Output: money.MustParseAmount("1")}}
	return llm.NewRequest(snap)
}
func TestAttachmentRealisticMediaCostCheckUnchanged(t *testing.T) {
	for _, kind := range []string{"png", "pdf"} {
		for _, affordable := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/affordable=%v", kind, affordable), func(t *testing.T) {
				cfg := testConfig(t)
				c := &attachmentPricedClient{&mediaCLIClient{activeCLIClient: &activeCLIClient{fakeClient: fakeClient{window: 128000, script: []reply{say("Media admitted under cost cap.")}}}}}
				cfg.Client = c
				a, err := buildApp(t.Context(), cfg, newLineUI(&bytes.Buffer{}))
				if err != nil {
					t.Fatal(err)
				}
				defer a.eng.Close()
				if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
					t.Fatal(err)
				}
				data := realisticAttachmentPDF()
				if kind == "png" {
					data = realisticAttachmentPNG(t)
				}
				path := filepath.Join(cfg.Dir, "evidence."+kind)
				if err = os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				input, err := printAttachmentInput(t.Context(), cfg.Dir, "inspect evidence", []string{path})
				if err != nil {
					t.Fatal(err)
				}
				store, err := a.eng.attachmentResolver.bind()
				if err != nil {
					t.Fatal(err)
				}
				if err = input.publish(t.Context(), store); err != nil {
					t.Fatal(err)
				}
				message, err := input.messageForStart()
				if err != nil {
					t.Fatal(err)
				}
				agent := a.eng.agent()
				agent.Settings.MaxTokens = 256
				cap := "0.01"
				if affordable {
					cap = "2"
				}
				start := dashi.Start{CommandID: dashi.NewID(), Agent: agent, Input: []llm.Message{message}, Budget: &dashi.Budget{MaxCost: &dashi.Money{Amount: money.MustParseAmount(cap), Currency: "USD"}}}
				runner := a.eng.handle().Runner
				observed := false
				runner.Hooks.BeforeSend = append(runner.Hooks.BeforeSend, dashi.SendHook{Name: "verify unchanged risk", Func: func(_ context.Context, p dashi.SendPlan) (dashi.SendDecision, error) {
					observed = true
					expected, ok := llm.WorstCaseCost(p.Request, *p.Request.Snapshot().ModelInfo.Pricing)
					if !ok || p.WorstCaseCost == nil || p.WorstCaseCost.Total != expected.Total {
						t.Fatalf("risk changed: plan=%+v expected=%+v", p.WorstCaseCost, expected)
					}
					return dashi.SendDecision{}, nil
				}})
				out, err := runner.Run(t.Context(), dashi.Turn{ID: session.TurnID(a.eng.Session().ID, 1)}, start)
				if err != nil {
					t.Fatal(err)
				}
				if affordable {
					if c.sends != 1 || !observed || out.Stop.Kind != dashi.StopCompleted {
						t.Fatal("affordable snapshot refused", c.sends, out.Stop)
					}
				} else {
					if c.sends != 0 || observed || out.Stop.Limit == nil || out.Stop.Limit.Kind != dashi.LimitCost {
						t.Fatal("cost check bypassed", c.sends, out.Stop)
					}
				}
				t.Logf("kind=%s bytes=%d affordable=%v Send=%d stop=%s", kind, len(data), affordable, c.sends, out.Stop.Kind)
				if dir := os.Getenv("DASHI_ATTACHMENT_TRANSCRIPTS"); dir != "" {
					proof := fmt.Sprintf("kind=%s bytes=%d cap=%sUSD affordable=%v Send=%d stop=%s BeforeSend=%v\n", kind, len(data), cap, affordable, c.sends, out.Stop.Kind, observed)
					if out.Stop.Limit != nil {
						proof += fmt.Sprintf("limit=%s worst_case_cost=%sUSD\n", out.Stop.Limit.Kind, out.Stop.Limit.WorstCaseCost)
					}
					if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("cost-%s-affordable-%v.txt", kind, affordable)), []byte(proof), 0600); err != nil {
						t.Fatal(err)
					}
				}

			})
		}
	}
}
