package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// homeSkillLayout writes a home dir with the common problem shapes: two
// broken symlinks, a long description reached through two search dirs, and
// a container folder without SKILL.md.
func homeSkillLayout(t *testing.T, home string) {
	t.Helper()
	claude := filepath.Join(home, ".claude/skills")
	agents := filepath.Join(home, ".agents/skills")
	long := "description: >\n  " + strings.Repeat("Plan and test schema changes. ", 35) + "\n"
	putResource(t, filepath.Join(agents, "migration/SKILL.md"), "---\nname: migration\n"+long+"---\nMigration body.")
	putResource(t, filepath.Join(claude, "synced/7c1e0a2b/SKILL.md"), "Nested body.")
	for name, target := range map[string]string{
		"migration": filepath.Join(agents, "migration"),
		"cli":       filepath.Join(home, "missing/cli"),
		"work":      filepath.Join(home, "missing/work"),
	} {
		if err := os.Symlink(target, filepath.Join(claude, name)); err != nil {
			t.Skip(err)
		}
	}
}

func TestResourceWarningsCollapseOncePerProblem(t *testing.T) {
	cfg := testConfig(t, say("Hello."), say("Again."), say("Third."))
	homeSkillLayout(t, cfg.HomeDir)
	var out bytes.Buffer
	ui := newLineUI(&out)
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	ui.ctrl = a.ctrl
	notice, err := a.openSession(cfg.Store, cfg.Dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ui.SetLive()
	if err := ui.serveLines(strings.NewReader("hi\nagain\n/resources\n/quit\n"), true); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	t.Logf("startup notice: %s\ntranscript:\n%s", notice, got)
	if n := strings.Count(got, "2 skills were not loaded. /resources shows why."); n != 1 {
		t.Fatalf("summary shown %d times:\n%s", n, got)
	}
	for _, quiet := range []string{"Resource ", "description", "SKILL.md;", "text-only", "Unverified support"} {
		if strings.Contains(before(got, "Available instructions"), quiet) {
			t.Fatalf("transcript shows %q:\n%s", quiet, got)
		}
	}
	// /resources lists each problem in full, once, and the adjustment apart.
	details := got[strings.Index(got, "Available instructions"):]
	mustHave(t, details, "Not loaded:")
	mustHave(t, details, filepath.Join(cfg.HomeDir, ".claude/skills/cli")+": broken symlink")
	mustHave(t, details, filepath.Join(cfg.HomeDir, ".claude/skills/work")+": broken symlink")
	mustHave(t, details, "Loaded with adjustments:")
	if n := strings.Count(details, "description shortened"); n != 1 {
		t.Fatalf("shortened description listed %d times:\n%s", n, details)
	}
	mustHave(t, details, filepath.Join(cfg.HomeDir, ".agents/skills/migration/SKILL.md")+": description shortened")
	if strings.Contains(details, "synced") {
		t.Fatalf("container folder reported:\n%s", details)
	}
	found, err := a.eng.cfg.Resources.discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Skills) != 1 || found.Skills[0].Info.Name != "migration" || !found.Skills[0].Info.ModelInvocable {
		t.Fatalf("long-description skill did not load: %+v", found.Skills)
	}
}

func TestResourceWarningsShowNewAndReturningProblems(t *testing.T) {
	home := t.TempDir()
	var shown []string
	m := newResourceManager(t.TempDir(), home)
	m.warning = func(format string, args ...any) { shown = append(shown, format) }
	discover := func() {
		t.Helper()
		if _, err := m.discover(); err != nil {
			t.Fatal(err)
		}
	}
	bad := filepath.Join(home, ".dashi/skills/review/SKILL.md")
	putResource(t, bad, "Review body.")
	discover()
	discover()
	if len(shown) != 0 {
		t.Fatalf("valid skill warned: %v", shown)
	}
	// Edited mid-session to become invalid: shown once, not on each refresh.
	putResource(t, bad, "---\nname: has space\n---\nBody")
	discover()
	discover()
	if len(shown) != 1 {
		t.Fatalf("warnings = %v", shown)
	}
	// Fixed, then broken again: the returning problem shows again.
	putResource(t, bad, "Review body.")
	discover()
	putResource(t, bad, "---\nname: has space\n---\nBody")
	discover()
	if len(shown) != 2 {
		t.Fatalf("warnings = %v", shown)
	}
}

func TestResourceWarningsDedupeSymlinkedRoots(t *testing.T) {
	home := t.TempDir()
	agents := filepath.Join(home, ".agents/skills/broken")
	putResource(t, filepath.Join(agents, "SKILL.md"), "---\nname: broken\n---\n")
	if err := os.MkdirAll(filepath.Join(home, ".claude/skills"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(agents, filepath.Join(home, ".claude/skills/broken")); err != nil {
		t.Skip(err)
	}
	found, err := discoverResources(t.TempDir(), home)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Warnings) != 1 || found.Warnings[0].Path != filepath.Join(agents, "SKILL.md") {
		t.Fatalf("warnings = %+v", found.Warnings)
	}
	if got := notLoadedSummary(found.Warnings); got != "1 skill was not loaded. /resources shows why." {
		t.Fatalf("summary = %q", got)
	}
}

func TestNotLoadedSummaryWording(t *testing.T) {
	skill := resourceWarning{}
	instructions := resourceWarning{Instructions: true}
	for _, tc := range []struct {
		in   []resourceWarning
		want string
	}{
		{nil, ""},
		{[]resourceWarning{skill}, "1 skill was not loaded. /resources shows why."},
		{[]resourceWarning{skill, skill}, "2 skills were not loaded. /resources shows why."},
		{[]resourceWarning{instructions}, "Workspace instructions were not loaded. /resources shows why."},
		{[]resourceWarning{instructions, skill}, "Workspace instructions and 1 skill were not loaded. /resources shows why."},
	} {
		if got := notLoadedSummary(tc.in); got != tc.want {
			t.Errorf("notLoadedSummary(%d) = %q, want %q", len(tc.in), got, tc.want)
		}
	}
}

func TestSessionNoticeShowsOncePerSession(t *testing.T) {
	e := NewEngine(EngineConfig{})
	media := func(turn string) Block {
		return Block{ID: "active-protection:" + turn, Kind: KindCompaction, State: "unavailable", Text: "Active-Turn protection unavailable for media.", Done: true}
	}
	if e.repeatsSessionNotice(media("t1")) || e.repeatsSessionNotice(media("t1")) {
		t.Fatal("first notice or its update suppressed")
	}
	if !e.repeatsSessionNotice(media("t2")) {
		t.Fatal("second Turn repeated the notice")
	}
	if e.repeatsSessionNotice(Block{Kind: KindNotice, Text: "other"}) {
		t.Fatal("ordinary notice suppressed")
	}
	e.sessionNotices = nil // what opening another session does
	if e.repeatsSessionNotice(media("t3")) {
		t.Fatal("new session suppressed the notice")
	}
}

func TestSecondaryTextIsReadableGray(t *testing.T) {
	text, ok := nativeBlockText(Block{Kind: KindFooter, Text: "[turn 1 | fixture]", Done: true}, 80)
	if !ok || !strings.Contains(text, "38;5;245") {
		t.Fatalf("footer = %q", text)
	}
	if strings.Contains(text, "[2;") || strings.Contains(text, ";2;") {
		t.Fatalf("footer uses dim: %q", text)
	}
}

func before(s, marker string) string {
	if i := strings.Index(s, marker); i >= 0 {
		return s[:i]
	}
	return s
}
