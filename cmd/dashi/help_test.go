package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/tui"
)

func helpTitles(h helpDoc) []string {
	var titles []string
	for _, s := range h.Sections {
		titles = append(titles, s.Title)
	}
	return titles
}

func TestHelpGroupsCommandsAndListsSkillsSeparately(t *testing.T) {
	cfg := testConfig(t)
	fixtureResourceSkill(t, cfg.HomeDir, "deploy", "Deploy it", "argument-hint: <env>\n")
	fixtureResourceSkill(t, cfg.HomeDir, "hidden", "Model only", "user-invocable: false\n")
	a, err := buildApp(t.Context(), cfg, newLineUI(&bytes.Buffer{}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	h := a.ctrl.helpDoc()
	want := []string{"Session", "Turn", "Model", "Inspect", "Tasks", "Attachments", "Skills", "Keys (fullscreen and inline modes)"}
	if got := helpTitles(h); !slices.Equal(got, want) {
		t.Fatalf("sections = %q", got)
	}
	for _, s := range h.Sections {
		for _, r := range s.Rows {
			isSkill := r.Name == "/deploy"
			if isSkill != (s.Title == skillGroup) || r.Name == "/hidden" {
				t.Fatalf("%s listed in %s", r.Name, s.Title)
			}
		}
	}
	skills := h.Sections[6]
	if len(skills.Rows) != 1 || skills.Rows[0] != (helpRow{Name: "/deploy", Usage: "<env>", Text: "Fixture skill"}) {
		t.Fatalf("skills = %+v", skills.Rows)
	}
	text := h.String()
	for _, line := range []string{
		"  • /status [ID]                  inspect recorded status and own-session spend",
		"    /deploy <env>                 Fixture skill",
		"mention tokens\n\n  • also works while a turn runs",
		"    Ctrl+C        interrupt; press twice to exit when idle",
	} {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("missing %q in:\n%s", line, text)
		}
	}
	// A command too long for the column puts its description below.
	if !strings.Contains(text, "/task ID [continue|done CALL_ID|discard]\n                                  inspect") {
		t.Fatalf("long command:\n%s", text)
	}
	if strings.Contains(text, "idle or during") {
		t.Fatal("old availability suffix")
	}
}

func TestHelpOmitsSkillsSectionWithoutSkills(t *testing.T) {
	h := newHarness(t)
	if titles := helpTitles(h.app.ctrl.helpDoc()); slices.Contains(titles, skillGroup) {
		t.Fatalf("sections = %q", titles)
	}
}

func TestHelpBlockRendersStyledWithPlainSource(t *testing.T) {
	h := helpDoc{Sections: []helpSection{{Title: "Session", Rows: []helpRow{{Name: "/status", Usage: "[ID]", Text: "inspect", During: true}}, Note: "  • legend"}}}
	b := Block{Kind: KindHelp, Text: h.String(), Help: &h, Done: true}
	if FormatBlock(b) != "Session\n  • /status [ID]  inspect\n\n  • legend" {
		t.Fatalf("plain = %q", FormatBlock(b))
	}
	styled, ok := nativeBlockText(b, 80)
	if !ok {
		t.Fatal("no native text")
	}
	for _, want := range []string{"\x1b[0;1;36mSession", "\x1b[0;1m/status", "\x1b[0;38;5;248m [ID]", "\n \n\x1b[0m\x1b[0;3;38;5;248m  • legend"} {
		if !strings.Contains(styled, want) {
			t.Fatalf("missing %q in %q", want, styled)
		}
	}
	if got := tui.SprintScreen(blockView(b), tui.WithWidth(40)).Text(); !strings.Contains(got, "  • /status [ID]  inspect\n\n  • legend") {
		t.Fatalf("screen = %q", got)
	}
}
