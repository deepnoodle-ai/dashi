package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
)

func fixtureResourceSkill(t *testing.T, dir, name, body, extra string) string {
	t.Helper()
	path := filepath.Join(dir, ".dashi/skills", name, "SKILL.md")
	putResource(t, path, "---\nname: "+name+"\ndescription: Fixture skill\n"+extra+"---\n"+body)
	return path
}

func TestResourceApprovalLifetimeAndChangedTarget(t *testing.T) {
	request := callTool("file", "read_skill_file", `{"skill":"reference","path":"guide.txt"}`)
	h := newHarness(t, request, say("first"), request, say("second"), request, say("third"), request, say("fourth"))
	skillPath := fixtureResourceSkill(t, h.home, "reference", "Read guide.txt", "")
	root := filepath.Dir(skillPath)
	putResource(t, filepath.Join(root, "one.txt"), "first-approved-body")
	putResource(t, filepath.Join(root, "two.txt"), "second-approved-body")
	link := filepath.Join(root, "guide.txt")
	if err := os.Symlink("one.txt", link); err != nil {
		t.Skip(err)
	}
	h.ui.answer = func(context.Context, ApprovalRequest) (Approval, error) {
		return Approval{Allow: true, Remember: true}, nil
	}
	approvalCount := func() int { h.ui.mu.Lock(); defer h.ui.mu.Unlock(); return len(h.ui.approvals) }
	h.input("read first")
	h.input("/new")
	h.input("read again")
	if approvalCount() != 1 {
		t.Fatal("remembered exact call lost across /new")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("two.txt", link); err != nil {
		t.Fatal(err)
	}
	h.input("read new target")
	if approvalCount() != 2 || !strings.Contains(inputText(h.client.inputAt(5)), "second-approved-body") {
		t.Fatal("changed canonical target reused approval")
	}
	h.app.eng.Close()
	h.build(Options{Resume: true})
	h.input("read after restart")
	if approvalCount() != 3 {
		t.Fatal("process restart restored a remembered grant")
	}
}

type resourceFaultRecorder struct {
	*filerecorder.Recorder
	onStart func(dashi.Step) error
}

func (r *resourceFaultRecorder) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if err := r.Recorder.Commit(ctx, req); err != nil {
		return err
	}
	if req.Step.Kind == dashi.StepStarted {
		return r.onStart(req.Step)
	}
	return nil
}
func TestResourcePublicationFailureStopsBeforePreparation(t *testing.T) {
	h := newHarness(t, say("must not send"))
	h.app.eng.Close()
	h.recorder = func(rec *filerecorder.Recorder) dashi.Recorder {
		return &resourceFaultRecorder{Recorder: rec, onStart: func(step dashi.Step) error {
			id := step.TurnID[:strings.LastIndex(step.TurnID, "-")]
			return os.WriteFile(filepath.Join(h.store.Root(), id, "resources"), []byte("not a directory"), 0600)
		}}
	}
	h.build(Options{})
	h.input("start")
	h.client.mu.Lock()
	prepares, sends := len(h.client.inputs), h.client.sends
	h.client.mu.Unlock()
	if prepares != 0 || sends != 0 {
		t.Fatalf("publication failure dispatched: %d/%d", prepares, sends)
	}
	mustHave(t, h.ui.text(), "Resources unavailable")
}

func TestResourceReopenBeforeFirstIntentMayRediscover(t *testing.T) {
	h := newHarness(t, say("recovered"))
	putResource(t, filepath.Join(h.dir, "AGENTS.md"), "old-before-start")
	h.app.eng.Close()
	ctx, cancel := context.WithCancel(t.Context())
	h.recorder = func(rec *filerecorder.Recorder) dashi.Recorder {
		return &resourceFaultRecorder{Recorder: rec, onStart: func(dashi.Step) error { cancel(); return nil }}
	}
	h.build(Options{})
	_, _ = h.app.eng.Prompt(ctx, "recover first admission")
	turn := h.turn(1)
	if _, found, err := turnResourceReference(turn); err != nil || found {
		t.Fatalf("unexpected pin before cancellation: %v/%v", found, err)
	}
	h.app.eng.Close()
	putResource(t, filepath.Join(h.dir, "AGENTS.md"), "new-before-first-intent")
	h.recorder = nil
	h.build(Options{Resume: true})
	h.input("")
	if latestWorkspaceInstructions(h.client.inputAt(0)) != "new-before-first-intent" {
		t.Fatal("unstarted turn failed to rediscover")
	}
}

type resourceTurnLoader struct{ turn dashi.Turn }

func (l resourceTurnLoader) Load(context.Context, string) (dashi.Turn, error) { return l.turn, nil }
func TestResourceMissingPinAfterIntentCannotRediscover(t *testing.T) {
	h := newHarnessLimit(t, 1, callTool("load", "load_skill", `{"name":"review"}`))
	fixtureResourceSkill(t, h.dir, "review", "pinned-body", "")
	h.input("start")
	turn := h.turn(1)
	for i := range turn.Steps {
		if fact := turn.Steps[i].Context; fact != nil {
			var kept []dashi.ContextItem
			for _, item := range fact.Items {
				if item.Name != resourcePinName {
					kept = append(kept, item)
				}
			}
			fact.Items = kept
		}
	}
	manager := h.app.eng.cfg.Resources
	loader := resourceTurnLoader{turn}
	if _, err := manager.preflight(t.Context(), h.app.eng.Session(), loader, turn.ID); err == nil {
		t.Fatal("missing pin preflight succeeded")
	}
	manager.open(h.app.eng.Session(), loader)
	if _, _, err := manager.bind(t.Context(), turn.ID, true); err == nil {
		t.Fatal("started turn rediscovered resources")
	}
}

func TestResourceUnchangedContextIsDeliveredOnce(t *testing.T) {
	h := newHarness(t, say("first"), say("second"), say("third"))
	fixtureResourceSkill(t, h.dir, "review", "private-body", "")
	putResource(t, filepath.Join(h.dir, "AGENTS.md"), "unchanged-instructions")
	h.input("first")
	h.input("second")
	h.app.eng.Close()
	h.build(Options{Resume: true})
	h.input("third")
	input := h.client.inputAt(2)
	counts := map[string]int{}
	for _, message := range input.Messages {
		if message.Context != nil {
			counts[message.Context.Name]++
		}
	}
	if counts["workspace-instructions"] != 1 || counts["workspace-instructions-source"] != 1 || counts["skills"] != 1 || counts[resourcePinName] != 3 {
		t.Fatalf("context repetitions=%v", counts)
	}
	first := h.client.inputAt(0).Messages
	for i, message := range first {
		a, _ := json.Marshal(message)
		b, _ := json.Marshal(input.Messages[i])
		if string(a) != string(b) {
			t.Fatal("provider-facing history prefix changed")
		}
	}
}

func TestResourceSupportingFileCancellationReadsNoBody(t *testing.T) {
	h := newHarness(t, callTool("file", "read_skill_file", `{"skill":"reference","path":"guide.txt"}`), say("continued"))
	path := fixtureResourceSkill(t, h.home, "reference", "Use guide.txt", "")
	putResource(t, filepath.Join(filepath.Dir(path), "guide.txt"), "never-returned-body")
	h.ui.answer = func(context.Context, ApprovalRequest) (Approval, error) { return Approval{}, context.Canceled }
	h.input("read")
	for _, step := range h.turn(1).Steps {
		if step.Tool != nil && strings.Contains(outputText(step.Tool.Outcome), "never-returned-body") {
			t.Fatal("canceled approval exposed contents")
		}
	}
}
func latestWorkspaceInstructions(in llm.Input) string {
	latest := ""
	for _, message := range in.Messages {
		for _, content := range message.Content {
			if content.Reminder != nil && content.Reminder.Name == "workspace-instructions" {
				latest = content.Reminder.Content
			}
		}
	}
	return latest
}
func TestResourceTurnPinAndProgressiveLoadSurvivePhysicalReopen(t *testing.T) {
	h := newHarnessLimit(t, 1, callTool("load", "load_skill", `{"name":"review"}`), say("finished"))
	path := fixtureResourceSkill(t, h.dir, "review", "old-private-skill-body", "")
	putResource(t, filepath.Join(h.dir, "AGENTS.md"), "old-workspace-instructions")
	h.input("review these changes")
	if strings.Contains(inputText(h.client.inputAt(0)), "old-private-skill-body") {
		t.Fatal("skill body eagerly delivered")
	}
	turn := h.turn(1)
	ref, found, err := turnResourceReference(turn)
	if err != nil || !found {
		t.Fatalf("pin=%+v %v %v", ref, found, err)
	}
	if len(turn.Steps) < 2 || turn.Steps[1].Kind != dashi.StepDelivered {
		t.Fatalf("pin not before model: %+v", turn.Steps)
	}
	raw, err := h.store.ReadResources(h.app.eng.Session(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "old-private-skill-body") {
		t.Fatal("selected definition was not cached")
	}
	putResource(t, path, "---\nname: review\ndescription: Fixture skill\n---\nnew-private-skill-body")
	putResource(t, filepath.Join(h.dir, "AGENTS.md"), "new-workspace-instructions")
	h.ui.Reset()
	h.input("/resources")
	if strings.Contains(h.ui.text(), "old-private-skill-body") || strings.Contains(h.ui.text(), "new-private-skill-body") {
		t.Fatal("inventory printed instruction body")
	}
	h.app.eng.Close()
	h.build(Options{Resume: true})
	h.input("")
	input := h.client.inputAt(1)
	if latestWorkspaceInstructions(input) != "old-workspace-instructions" || !strings.Contains(inputText(input), "old-private-skill-body") || strings.Contains(inputText(input), "new-private-skill-body") {
		t.Fatalf("recovery changed resources: %s", inputText(input))
	}
}
func TestResourceCorruptionStopsBeforePreparation(t *testing.T) {
	h := newHarnessLimit(t, 1, callTool("load", "load_skill", `{"name":"review"}`), say("must not send"))
	fixtureResourceSkill(t, h.dir, "review", "original-body", "")
	h.input("review")
	ref, found, err := turnResourceReference(h.turn(1))
	if err != nil || !found {
		t.Fatal(err)
	}
	// Same-size corruption retains valid JSON; content verification must catch it.
	raw, err := h.store.ReadResources(h.app.eng.Session(), ref)
	if err != nil {
		t.Fatal(err)
	}
	raw[strings.Index(string(raw), "original-body")] = 'X'
	directory := filepath.Join(h.store.Root(), h.app.eng.Session().ID)
	path := filepath.Join(directory, "resources", ref.SHA256+".json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = h.app.eng.Resume(context.Background())
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("corruption=%v", err)
	}
	h.client.mu.Lock()
	preparations, sends := len(h.client.inputs), h.client.sends
	h.client.mu.Unlock()
	if preparations != 1 || sends != 1 {
		t.Fatalf("corrupt resources dispatched: prepare=%d send=%d", preparations, sends)
	}
}
func TestResourceRefreshAtEveryPromptAndQueuedSuccessor(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	first := func(ctx context.Context, call llm.Call) (llm.Result, error) {
		close(entered)
		<-release
		return say("first done")(ctx, call)
	}
	h := newHarness(t, first, say("queued done"), say("next done"))
	putResource(t, filepath.Join(h.dir, "AGENTS.md"), "first-instructions")
	h.app.ctrl.Input("first")
	<-entered
	h.app.ctrl.Input("/queue second")
	putResource(t, filepath.Join(h.dir, "AGENTS.md"), "queued-instructions")
	close(release)
	h.app.ctrl.Wait()
	if latestWorkspaceInstructions(h.client.inputAt(1)) != "queued-instructions" {
		t.Fatal("queued successor reused earlier selection")
	}
	firstRef, _, err := turnResourceReference(h.turn(1))
	if err != nil {
		t.Fatal(err)
	}
	secondRef, _, err := turnResourceReference(h.turn(2))
	if err != nil {
		t.Fatal(err)
	}
	if firstRef == secondRef {
		t.Fatal("queued successor did not pin its own resources")
	}
	if err := os.Remove(filepath.Join(h.dir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	h.input("next")
	if latestWorkspaceInstructions(h.client.inputAt(2)) != "No workspace instructions selected." {
		t.Fatal("deleted instruction remained current")
	}
}
func TestResourceSlashRestrictionsBuiltinsAndLiteralArguments(t *testing.T) {
	h := newHarness(t, say("ran expanded skill"))
	fixtureResourceSkill(t, h.dir, "review", "Review $ARGUMENTS and $1", "")
	fixtureResourceSkill(t, h.dir, "hidden", "never-user-body", "user-invocable: false\n")
	fixtureResourceSkill(t, h.dir, "help", "never-shadow-help", "")
	h.input("/hidden")
	h.input("/missing")
	h.input("/help")
	if h.turns() != 0 {
		t.Fatal("invalid/builtin slash commands started a turn")
	}
	mustHave(t, h.ui.text(), "cannot be invoked as a user command")
	mustHave(t, h.ui.text(), "Unknown command /missing")
	h.input("/review $(literal-command) $HOME")
	input := inputText(h.client.inputAt(0))
	if !strings.Contains(input, "Review $(literal-command) $HOME and $(literal-command)") {
		t.Fatalf("literal argument expansion=%s", input)
	}
}
func TestResourceSupportingFileApprovalAndReplacement(t *testing.T) {
	for _, scenario := range []string{"allow", "deny", "replace", "escape"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, callTool("file", "read_skill_file", `{"skill":"reference","path":"guide.txt"}`), say("done"))
			home := h.app.eng.cfg.Resources.home
			skillPath := fixtureResourceSkill(t, home, "reference", "Use guide.txt", "")
			file := filepath.Join(filepath.Dir(skillPath), "guide.txt")
			putResource(t, file, "approved-support-body")
			canonicalFile, err := filepath.EvalSymlinks(file)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "escape" {
				outside := filepath.Join(t.TempDir(), "outside.txt")
				putResource(t, outside, "outside-secret")
				os.Remove(file)
				if err := os.Symlink(outside, file); err != nil {
					t.Skip(err)
				}
			}
			h.ui.answer = func(_ context.Context, request ApprovalRequest) (Approval, error) {
				if request.Tool != "read_skill_file" || !strings.Contains(request.Summary, skillPath) {
					t.Errorf("approval source=%+v", request)
				}
				var args skillFileArgs
				if err := json.Unmarshal([]byte(request.Arguments), &args); err != nil || args.Path != canonicalFile {
					t.Errorf("canonical args=%s %v", request.Arguments, err)
				}
				if scenario == "replace" {
					if err := os.Rename(file, file+".old"); err != nil {
						t.Fatal(err)
					}
					putResource(t, file, "replacement-secret")
				}
				return Approval{Allow: scenario != "deny"}, nil
			}
			h.input("read the supporting file")
			final := inputText(h.client.inputAt(1))
			if scenario == "allow" && !strings.Contains(final, "approved-support-body") {
				t.Fatalf("approved body missing: %s; UI: %s", final, h.ui.text())
			}
			if scenario != "allow" && (strings.Contains(final, "approved-support-body") || strings.Contains(final, "replacement-secret") || strings.Contains(final, "outside-secret")) {
				t.Fatalf("unauthorized body returned: %s", final)
			}
			h.ui.mu.Lock()
			count := len(h.ui.approvals)
			h.ui.mu.Unlock()
			if scenario == "escape" && count != 0 {
				t.Fatal("invalid link reached approval")
			}
		})
	}
}
