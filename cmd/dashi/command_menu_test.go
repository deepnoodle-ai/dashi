package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/tui"
)

func menuNames(items []commandMenuItem) []string {
	if items == nil {
		return nil
	}
	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.name
	}
	return names
}

func TestMatchCommandMenuOrderAndAliases(t *testing.T) {
	items := []commandMenuItem{
		{name: "/model"}, {name: "/mcp"}, {name: "/compact"}, {name: "/rename"},
		{name: "/help", aliases: []string{"/?"}},
		{name: "/quit", aliases: []string{"/q", "/exit"}},
	}
	for _, tc := range []struct {
		typed string
		want  []string
	}{
		// Prefix matches come before subsequence matches, each by name.
		{"/m", []string{"/mcp", "/model", "/compact", "/rename"}},
		{"/MC", []string{"/mcp", "/compact"}},
		// An alias match shows the command once, under its own name.
		{"/q", []string{"/quit"}},
		{"/ex", []string{"/quit"}},
		{"/?", []string{"/help"}},
		{"/zz", nil},
	} {
		if got := menuNames(matchCommandMenu(tc.typed, items)); !slices.Equal(got, tc.want) && (len(got) > 0 || len(tc.want) > 0) {
			t.Errorf("%s: got %v, want %v", tc.typed, got, tc.want)
		}
	}
}

func TestCommandMenuOffsetKeepsSelectionInView(t *testing.T) {
	for _, tc := range []struct{ selected, offset, n, want int }{
		{0, 0, 3, 0},
		{4, 0, 10, 0},
		{5, 0, 10, 1},
		{9, 0, 10, 5},
		{2, 5, 10, 2},
		{0, 5, 10, 0},
		// A shorter list pulls the window back.
		{6, 6, 8, 3},
	} {
		if got := commandMenuOffset(tc.selected, tc.offset, tc.n); got != tc.want {
			t.Errorf("offset(%d, %d, %d) = %d, want %d", tc.selected, tc.offset, tc.n, got, tc.want)
		}
	}
}

func TestCapitalize(t *testing.T) {
	for in, want := range map[string]string{"": "", "list work": "List work", "Already": "Already", "élan": "Élan"} {
		if got := capitalize(in); got != want {
			t.Errorf("capitalize(%q) = %q", in, got)
		}
	}
}

// typeDraft sets the draft as if the user had typed it.
func typeDraft(h *screenHarness, draft string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scr.input = draft
	h.scr.commandMenu.typing = true
}

func (h *screenHarness) menuKey(e tui.KeyEvent) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.scr.inputKey(e)
}

func (h *screenHarness) menu() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return menuNames(h.scr.commandMenuMatches())
}

func TestCommandMenuOpensOnlyWhileTypingACommandName(t *testing.T) {
	h := newScreenHarness(t, 0)
	typeDraft(h, "/m")
	got := h.menu()
	if len(got) < 2 || got[0] != "/max-tokens" || !slices.Contains(got, "/model") || !slices.Contains(got, "/mcp") {
		t.Fatalf("menu for /m: %v", got)
	}
	for _, draft := range []string{"/model ", "model", "", " /m"} {
		typeDraft(h, draft)
		if got := h.menu(); got != nil {
			t.Errorf("menu open for %q: %v", draft, got)
		}
	}
	// History recall and other drafts set without typing do not open it.
	h.mu.Lock()
	h.scr.input, h.scr.commandMenu.typing = "/help", false
	h.mu.Unlock()
	if got := h.menu(); got != nil {
		t.Fatalf("menu open after recall: %v", got)
	}
	// A question owns the composer.
	typeDraft(h, "/m")
	h.mu.Lock()
	h.scr.promptActive, h.scr.asking = true, &askPrompt{}
	h.mu.Unlock()
	if got := h.menu(); got != nil {
		t.Fatalf("menu open while asking: %v", got)
	}
}

func TestCommandMenuListsUserSkills(t *testing.T) {
	h := newScreenHarness(t, 0)
	fixtureResourceSkill(t, h.dir, "grill-me", "Ask questions.", "")
	fixtureResourceSkill(t, h.dir, "hidden-skill", "Model only.", "user-invocable: false\n")
	typeDraft(h, "/")
	got := h.menu()
	if !slices.Contains(got, "/grill-me") || slices.Contains(got, "/hidden-skill") {
		t.Fatalf("menu=%v", got)
	}
}

func TestCommandMenuKeys(t *testing.T) {
	h := newScreenHarness(t, 0)
	menu := func() *commandMenu { return &h.scr.commandMenu }
	locked := func(fn func()) {
		h.mu.Lock()
		defer h.mu.Unlock()
		fn()
	}
	typeDraft(h, "/m")
	all := h.menu()
	if !h.menuKey(tui.KeyEvent{Key: tui.KeyArrowDown}) || !h.check(func() bool { return menu().selected == 1 }) {
		t.Fatal("Down did not select the second command")
	}
	h.menuKey(tui.KeyEvent{Key: tui.KeyArrowUp})
	h.menuKey(tui.KeyEvent{Key: tui.KeyArrowUp})
	if !h.check(func() bool { return menu().selected == len(all)-1 && menu().offset == len(all)-commandMenuRows }) {
		t.Fatal("Up did not wrap to the last command and scroll to it")
	}

	// Esc closes the menu and keeps the draft; the next edit opens it.
	if !h.menuKey(tui.KeyEvent{Key: tui.KeyEscape}) || h.menu() != nil || !h.check(func() bool { return h.scr.input == "/m" }) {
		t.Fatal("Esc did not close the menu and keep the draft")
	}
	h.menuKey(tui.KeyEvent{Rune: 'o'})
	locked(func() { h.scr.input = "/mo" })
	if got := h.menu(); len(got) == 0 || got[0] != "/model" {
		t.Fatalf("menu after edit: %v", got)
	}

	// Tab leaves the change to the field's completion.
	h.menuKey(tui.KeyEvent{Key: tui.KeyArrowDown})
	want := h.menu()[1]
	if h.menuKey(tui.KeyEvent{Key: tui.KeyTab}) {
		t.Fatal("Tab was consumed")
	}
	var completion []string
	locked(func() { completion = h.scr.completeDraft("/mo") })
	if !slices.Equal(completion, []string{want + " "}) {
		t.Fatalf("completion=%v, want %s", completion, want)
	}
	if h.menu() != nil {
		t.Fatal("menu open after Tab")
	}

	// Enter on a command that needs an argument fills it in.
	typeDraft(h, "/ren")
	if !h.menuKey(tui.KeyEvent{Key: tui.KeyEnter}) || !h.check(func() bool { return h.scr.input == "/rename " }) || h.menu() != nil {
		t.Fatal("Enter on /rename did not fill in the command")
	}
	// Enter on a command that runs alone submits it in place of the draft.
	typeDraft(h, "/hel")
	if h.menuKey(tui.KeyEvent{Key: tui.KeyEnter}) || !h.check(func() bool { return menu().submit == "/help" }) {
		t.Fatal("Enter on /help did not choose it for submission")
	}
	locked(func() { h.scr.inputSubmit(h.scr.promptID)("/hel") })
	h.waitFor(func() bool {
		h.scr.drain()
		for _, b := range h.scr.tr.Blocks {
			if b.Kind == KindHelp {
				return h.scr.input == ""
			}
		}
		return false
	})
}

// descriptionColumn is where a menu row's description starts.
func descriptionColumn(row string) int {
	name := strings.Index(row, "/")
	end := name + strings.IndexByte(row[name:], ' ')
	return end + len(row[end:]) - len(strings.TrimLeft(row[end:], " "))
}

func TestCommandMenuView(t *testing.T) {
	h := newScreenHarness(t, 0)
	typeDraft(h, "/m")
	got := h.render()
	lines := strings.Split(got, "\n")
	var rows []string
	for _, line := range lines {
		if strings.HasPrefix(line, " > /") || strings.HasPrefix(line, "   /") {
			rows = append(rows, line)
		}
	}
	if len(rows) != commandMenuRows || !strings.HasPrefix(rows[0], " > /max-tokens") {
		t.Fatalf("rows=%q\n%s", rows, got)
	}
	// Names share a column, then the capitalized description.
	if !strings.Contains(rows[0], "  Set the next-turn output limit") {
		t.Fatalf("description:\n%s", rows[0])
	}
	for _, row := range rows[1:] {
		if descriptionColumn(row) != descriptionColumn(rows[0]) {
			t.Fatalf("columns:\n%s", strings.Join(rows, "\n"))
		}
	}
	for _, want := range []string{"↓ ", " more", "↑/↓ Navigate · enter Select · tab Complete", "esc to cancel"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "fixture ·") {
		t.Fatalf("status still shows the model:\n%s", got)
	}
}

func TestCommandMenuHeightIsStableWhileScrolling(t *testing.T) {
	h := newScreenHarness(t, 0)
	typeDraft(h, "/m")
	n := len(h.scr.commandMenuMatches())
	if n <= commandMenuRows {
		t.Fatalf("need more than %d matches, got %d", commandMenuRows, n)
	}
	// The composer row must not move as the selection scrolls.
	composerRow := func() int {
		for i, line := range strings.Split(h.render(), "\n") {
			if strings.HasSuffix(strings.TrimRight(line, " "), "/m") {
				return i
			}
		}
		t.Fatalf("composer not found:\n%s", h.render())
		return -1
	}
	want := composerRow()
	for i := 0; i < n; i++ {
		h.scr.commandMenuKey(tui.KeyEvent{Key: tui.KeyArrowDown})
		if got := composerRow(); got != want {
			t.Fatalf("after %d Down keys the composer is on row %d, want %d:\n%s", i+1, got, want, h.render())
		}
	}
}

func TestCommandMenuMore(t *testing.T) {
	for _, tc := range []struct {
		above, below int
		want         string
	}{
		{0, 3, "↓ 3 more"},
		{2, 1, "↑ 2 more · ↓ 1 more"},
		{3, 0, "↑ 3 more"},
	} {
		if got := commandMenuMore(tc.above, tc.below); got != tc.want {
			t.Errorf("commandMenuMore(%d, %d) = %q, want %q", tc.above, tc.below, got, tc.want)
		}
	}
}

func TestCommandMenuRowTruncates(t *testing.T) {
	row := commandMenuRow(commandMenuItem{name: "/model", description: "choose the next-turn destination/model"}, 6, true)
	got := strings.TrimRight(tui.SprintScreen(row, tui.WithWidth(24), tui.WithHeight(1)).Text(), "\n")
	if got != " > /model  Choose the n…" {
		t.Fatalf("row=%q", got)
	}
}

// TestCommandMenuRuntime drives the menu through Wonton's input field.
func TestCommandMenuRuntime(t *testing.T) {
	h := newTranscriptRuntime(t)
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "Ask Dashi to do anything") })
	h.typeText("/m")
	first := h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "> /max-tokens") })
	for _, want := range []string{"/mcp", "/model", "more", "tab Complete", "esc to cancel"} {
		if !strings.Contains(first.text, want) {
			t.Fatalf("missing %q:\n%s", want, first.text)
		}
	}
	h.key(tui.KeyEvent{Key: tui.KeyArrowDown})
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "> /mcp") })
	h.key(tui.KeyEvent{Key: tui.KeyTab})
	// The probe can answer before the frame is drawn, so wait for both.
	h.wait(func(s transcriptState) bool {
		return s.input == "/mcp " && strings.Contains(s.text, "❯ /mcp") && !strings.Contains(s.text, "tab Complete")
	})
	// Typing goes on after the completed name.
	h.typeText("x")
	h.wait(func(s transcriptState) bool { return s.input == "/mcp x" })
}
