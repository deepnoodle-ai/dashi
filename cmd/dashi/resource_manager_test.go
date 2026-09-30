package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func fixtureResourceSkill(t *testing.T, dir, name, body, extra string) string {
	t.Helper()
	path := filepath.Join(dir, ".dashi/skills", name, "SKILL.md")
	putResource(t, path, "---\nname: "+name+"\ndescription: Fixture skill\n"+extra+"---\n"+body)
	return path
}

func TestResourceLineApprovalsAllowFollowingCommands(t *testing.T) {
	var script []reply
	var input strings.Builder
	for range 20 {
		script = append(script, callTool("file", "read_skill_file", `{"skill":"reference","path":"guide.txt"}`), say("read complete"))
		input.WriteString("read\ny\n/resources\n")
	}
	input.WriteString("/quit\n")
	cfg := testConfig(t, script...)
	path := fixtureResourceSkill(t, cfg.HomeDir, "reference", "Use guide.txt", "")
	putResource(t, filepath.Join(filepath.Dir(path), "guide.txt"), "approved-line-body")
	var out bytes.Buffer
	ui := newLineUI(&out)
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	ui.ctrl = a.ctrl
	if _, err := a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	ui.SetLive()
	if err := ui.serveLines(strings.NewReader(input.String()), true); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "read complete") != 20 {
		t.Fatalf("line workflow dropped work: %s", out.String())
	}
}

func TestResourcePrintStartupNoticeUsesStderr(t *testing.T) {
	cfg := testConfig(t, say("only answer"))
	putResource(t, filepath.Join(cfg.Dir, "AGENTS.md"), "private-instruction-body")
	var out, errOut bytes.Buffer
	if code := runPrint(t.Context(), cfg, Options{Print: "start", PrintSet: true}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	if out.String() != "only answer\n" || !strings.Contains(errOut.String(), "AGENTS.md") || strings.Contains(errOut.String(), "private-instruction-body") {
		t.Fatalf("notice boundary stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestResourceExternalDefinitionCannotLoadPrivateBytes(t *testing.T) {
	h := newHarness(t, callTool("load", "load_skill", `{"name":"reference"}`), say("done"))
	path := filepath.Join(h.dir, ".dashi/skills/reference/SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "secret.md")
	putResource(t, external, "external-private-canary")
	if err := os.Symlink(external, path); err != nil {
		t.Skip(err)
	}
	h.input("load reference")
	if strings.Contains(inputText(h.client.inputAt(1)), "external-private-canary") {
		t.Fatal("external defining file leaked")
	}
	h.ui.mu.Lock()
	approvals := len(h.ui.approvals)
	h.ui.mu.Unlock()
	if approvals != 0 {
		t.Fatal("invalid defining file reached approval instead of discovery warning")
	}
	mustHave(t, h.ui.text(), "cannot read")
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
	if counts["workspace-instructions"] != 1 || counts["workspace-instructions-source"] != 1 || counts["skills"] != 1 {
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
func TestResourceLoadedToolResultSurvivesReopenAndNewLoadUsesCurrentFile(t *testing.T) {
	h := newHarness(t, callTool("old-load", "load_skill", `{"name":"review"}`), say("first done"), callTool("new-load", "load_skill", `{"name":"review"}`), say("second done"))
	path := fixtureResourceSkill(t, h.dir, "review", "old-private-skill-body", "")
	h.input("review first")
	if strings.Contains(inputText(h.client.inputAt(0)), "old-private-skill-body") {
		t.Fatal("skill body eagerly delivered")
	}
	if !strings.Contains(inputText(h.client.inputAt(1)), "old-private-skill-body") {
		t.Fatal("load result missing")
	}
	h.app.eng.Close()
	putResource(t, path, "---\nname: review\ndescription: Fixture skill\n---\nnew-private-skill-body")
	h.build(Options{Resume: true})
	h.input("/resources")
	if strings.Contains(h.ui.text(), "private-skill-body") {
		t.Fatal("inventory printed skill body")
	}
	h.input("review again")
	final := inputText(h.client.inputAt(3))
	if !strings.Contains(final, "old-private-skill-body") || !strings.Contains(final, "new-private-skill-body") {
		t.Fatalf("history or new load missing: %s", final)
	}
	if _, err := os.Stat(filepath.Join(h.store.Root(), h.app.eng.Session().ID, "resources")); !os.IsNotExist(err) {
		t.Fatalf("unexpected resource store: %v", err)
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
	h.input("/HELP")
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
