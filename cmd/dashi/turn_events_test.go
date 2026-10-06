package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// withTurnEvents rebuilds h with a --turn-events file and returns its path.
func withTurnEvents(t *testing.T, h *harness, o Options) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "turn-events.jsonl")
	log, err := openTurnEvents(path, h.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	h.turnEvents = log
	h.build(o)
	return path
}

// readTurnEvents returns each line of the file, checking that each is one
// JSON object.
func readTurnEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

// names returns the hook_event_name of each record.
func names(records []map[string]any) string {
	var s []string
	for _, r := range records {
		name, _ := r["hook_event_name"].(string)
		s = append(s, name)
	}
	return strings.Join(s, " ")
}

func TestTurnEventsCompletedTurn(t *testing.T) {
	h := newHarness(t, say("The fix is done."), say("Second."))
	path := withTurnEvents(t, h, Options{})
	h.input("fix the test")
	h.input("again")
	got := readTurnEvents(t, path)
	if names(got) != "UserPromptSubmit Stop UserPromptSubmit Stop" {
		t.Fatalf("records: %v", got)
	}
	id := h.app.eng.Session().ID
	want := []map[string]any{
		{"session_id": id, "hook_event_name": "UserPromptSubmit", "cwd": h.dir, "prompt_id": id + "-1", "prompt": "fix the test"},
		{"session_id": id, "hook_event_name": "Stop", "cwd": h.dir, "prompt_id": id + "-1", "last_assistant_message": "The fix is done."},
	}
	for i, w := range want {
		if len(got[i]) != len(w) {
			t.Errorf("record %d = %v, want %v", i, got[i], w)
		}
		for k, v := range w {
			if got[i][k] != v {
				t.Errorf("record %d %s = %v, want %v", i, k, got[i][k], v)
			}
		}
	}
	if got[2]["prompt_id"] != id+"-2" || got[3]["prompt_id"] != id+"-2" {
		t.Fatalf("second turn: %v", got[2:])
	}
}

func TestTurnEventsFailedTurn(t *testing.T) {
	h := newHarness(t,
		rejectWith(429, llm.ErrorRateLimited),
		rejectWith(529, llm.ErrorOverloaded),
	)
	path := withTurnEvents(t, h, Options{})
	h.input("hello")
	got := readTurnEvents(t, path)
	if names(got) != "UserPromptSubmit StopFailure" {
		t.Fatalf("records: %v", got)
	}
	end := got[1]
	if end["prompt_id"] != got[0]["prompt_id"] || end["error_type"] != string(dashi.StopFailed) || end["error_message"] == "" {
		t.Fatalf("failure record: %v", end)
	}
}

func TestTurnEventsApprovalWait(t *testing.T) {
	var runs atomicCounter
	withNoteTool(t, &runs)
	h := newHarness(t, callTool("c1", "write_note", `{"text":"one"}`), say("Noted."))
	path := withTurnEvents(t, h, Options{})
	h.input("write a note")
	got := readTurnEvents(t, path)
	if names(got) != "UserPromptSubmit Notification Stop" {
		t.Fatalf("records: %v", got)
	}
	n := got[1]
	if n["notification_type"] != "permission_prompt" || n["message"] != "Dashi needs your permission to use write_note" ||
		n["session_id"] != h.app.eng.Session().ID {
		t.Fatalf("notification: %v", n)
	}
	if got[2]["last_assistant_message"] != "Noted." {
		t.Fatalf("stop: %v", got[2])
	}
}

// --dangerously-skip-permissions asks nothing, so it writes no Notification.
func TestTurnEventsApproveAllWritesNoNotification(t *testing.T) {
	var runs atomicCounter
	withNoteTool(t, &runs)
	h := newHarness(t, callTool("c1", "write_note", `{"text":"one"}`), say("Noted."))
	path := withTurnEvents(t, h, Options{Yes: true})
	h.input("write a note")
	if got := names(readTurnEvents(t, path)); got != "UserPromptSubmit Stop" {
		t.Fatalf("records: %s", got)
	}
}

// An interrupted turn ends with StopFailure, so a reader does not wait for
// a Stop that never comes.
func TestTurnEventsInterruptedTurn(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, blockUntilDone(started))
	path := withTurnEvents(t, h, Options{})
	h.app.ctrl.Input("wait forever")
	<-started
	h.app.ctrl.CancelWork()
	h.app.ctrl.Wait()
	got := readTurnEvents(t, path)
	if names(got) != "UserPromptSubmit StopFailure" {
		t.Fatalf("records: %v", got)
	}
	if got[1]["error_type"] != string(dashi.StopInterrupted) || got[1]["prompt_id"] != got[0]["prompt_id"] {
		t.Fatalf("end: %v", got[1])
	}
}

// The end of a turn waits until the run is over and dashi takes input.
func TestTurnEventsHoldEndUntilIdle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := openTurnEvents(path, "/w")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	log.session = func() string { return "s" }
	if err := log.started("s-1", &dashi.StartFact{Input: []llm.Message{llm.UserMessage(llm.Text("hi"))}}); err != nil {
		t.Fatal(err)
	}
	log.ended(dashi.RunResult{Turn: dashi.Turn{ID: "s-1"}, TurnView: dashi.TurnView{Stop: dashi.Stop{Kind: dashi.StopCompleted}, Reply: "yo"}})
	if got := names(readTurnEvents(t, path)); got != "UserPromptSubmit" {
		t.Fatalf("before idle: %s", got)
	}
	if err := log.idle(nil); err != nil {
		t.Fatal(err)
	}
	if got := names(readTurnEvents(t, path)); got != "UserPromptSubmit Stop" {
		t.Fatalf("after idle: %s", got)
	}
	// A run refused before any turn starts still tells the reader.
	if err := log.idle(errors.New("MCP startup has not completed")); err != nil {
		t.Fatal(err)
	}
	got := readTurnEvents(t, path)
	if names(got) != "UserPromptSubmit Stop StopFailure" || got[2]["prompt_id"] != nil {
		t.Fatalf("refused run: %v", got)
	}
}

func TestCapTextKeepsWholeCharacters(t *testing.T) {
	s := strings.Repeat("a", turnEventTextLimit-1) + "é" + "tail"
	got := capText(s)
	if len(got) != turnEventTextLimit-1 || !strings.HasPrefix(s, got) {
		t.Fatalf("len=%d", len(got))
	}
}

func TestSessionIDNamesNewSession(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	h := newHarness(t, say("Hi."))
	path := withTurnEvents(t, h, Options{SessionID: id})
	if got := h.app.eng.Session().ID; got != id {
		t.Fatalf("session ID = %s", got)
	}
	h.input("hello")
	if got := readTurnEvents(t, path); got[0]["session_id"] != id || got[0]["prompt_id"] != id+"-1" {
		t.Fatalf("records: %v", got)
	}
	h.app.eng.Close()

	// The ID cannot start a second session, and --resume reopens it.
	a, err := buildApp(t.Context(), appConfig{Dir: h.dir, HomeDir: h.home, Model: "fixture", Client: h.client, Store: h.store}, newRecorderUI())
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.openSession(h.store, h.dir, Options{SessionID: id})
	if err == nil || !strings.Contains(err.Error(), "already exists; reopen it with --resume "+id) {
		t.Fatalf("reused ID: %v", err)
	}
	h.build(Options{Resume: true, ResumeID: id})
	if h.app.eng.Session().ID != id || h.turns() != 1 {
		t.Fatalf("resumed %s with %d turns", h.app.eng.Session().ID, h.turns())
	}
}
