package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
)

func runAutomationFixture(t *testing.T, cfg appConfig, o Options, input string) (int, string, string) {
	t.Helper()
	var out, err bytes.Buffer
	code := runAutomation(t.Context(), o, strings.NewReader(input), &out, &err, cfg.Dir, cfg.HomeDir, cfg.Store, func() (appConfig, error) { return cfg, nil })
	return code, out.String(), err.String()
}
func decodeResponses(t *testing.T, format, data string) []responseMessage {
	t.Helper()
	var got []responseMessage
	if format == "json" {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &fields); err != nil {
			t.Fatal(err, data)
		}
		if len(fields) != 1 || fields["messages"] == nil {
			t.Fatalf("extra fields: %s", data)
		}
		if err := json.Unmarshal(fields["messages"], &got); err != nil {
			t.Fatal(err)
		}
	} else {
		for _, line := range strings.Split(strings.TrimSuffix(data, "\n"), "\n") {
			if line == "" {
				continue
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(line), &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 1 || fields["content"] == nil {
				t.Fatalf("extra fields: %s", line)
			}
			var message responseMessage
			if err := json.Unmarshal([]byte(line), &message); err != nil {
				t.Fatal(err)
			}
			got = append(got, message)
		}
	}
	return got
}
func TestAutomationWholeInputOneLiteralRequest(t *testing.T) {
	literal := "日本\tline\n\n/quit\nyes\n@missing-file.txt\n"
	for _, format := range []string{"text", "json", "jsonl"} {
		cfg := testConfig(t, say("first\nlast\t日本\u2028\u2029\x1b[31m"))
		code, out, diag := runAutomationFixture(t, cfg, Options{Print: "Explain exactly ", PrintSet: true, OutputFormat: format, OutputFormatSet: true}, literal)
		if code != 0 {
			t.Fatalf("%s code=%d %s", format, code, diag)
		}
		c := cfg.Client.(*fakeClient)
		if c.sends != 1 || joinMessages(c.inputAt(0).Messages) != "Explain exactly \n\n"+literal {
			t.Fatalf("input=%+v", c.inputAt(0))
		}

		if format != "text" {
			got := decodeResponses(t, format, out)
			if len(got) != 1 || got[0].Content != "first\nlast\t日本\u2028\u2029\x1b[31m" {
				t.Fatal(out)
			}
		}
	}
	cfg := testConfig(t, say("literal"))
	code, _, diag := runAutomationFixture(t, cfg, Options{OutputFormat: "json"}, literal)
	if code != 0 || joinMessages(cfg.Client.(*fakeClient).inputAt(0).Messages) != literal {
		t.Fatalf("code=%d %s", code, diag)
	}
}
func TestAutomationPreflightDoesNotBuildLiveApp(t *testing.T) {
	for _, input := range []string{"", " \t\n", "bad\x00", string([]byte{0xff}), strings.Repeat("x", automationStdinLimit+1)} {
		cfg := testConfig(t)
		var out, diag bytes.Buffer
		called := false
		code := runAutomation(t.Context(), Options{OutputFormat: "json"}, strings.NewReader(input), &out, &diag, cfg.Dir, cfg.HomeDir, cfg.Store, func() (appConfig, error) { called = true; return cfg, nil })
		if code != 2 || called || out.String() != "{\"messages\":[]}\n" {
			t.Fatalf("code=%d called=%v out=%q diag=%s", code, called, out.String(), diag.String())
		}
	}
	cfg := testConfig(t)
	for _, prompt := range []string{"/help", "/quit", "/model other"} {
		code, out, _ := runAutomationFixture(t, cfg, Options{Print: prompt, PrintSet: true, OutputFormat: "json"}, "")
		want := 0
		if prompt == "/model other" {
			want = 2
		}
		if code != want || out != "{\"messages\":[]}\n" {
			t.Fatalf("%s %d %s", prompt, code, out)
		}
	}
	code, _, _ := runAutomationFixture(t, cfg, Options{Print: "/help", PrintSet: true, OutputFormat: "json"}, "data")
	if code != 2 || cfg.Client.(*fakeClient).sends != 0 {
		t.Fatal("mixed command dispatched")
	}
}
func TestAutomationParentNoticeOrderAndKeylessSavedRead(t *testing.T) {
	for _, format := range []string{"json", "jsonl"} {
		cfg := testConfig(t)
		client := &taskFixtureClient{}
		cfg.Client = client
		code, out, diag := runAutomationFixture(t, cfg, Options{Print: "delegate", PrintSet: true, Yes: true, OutputFormat: format}, "")
		if code != 0 {
			t.Fatalf("code=%d %s", code, diag)
		}
		got := decodeResponses(t, format, out)
		want := []responseMessage{{"Parent remains available."}, {"Task result received."}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got=%+v out=%s", got, out)
		}
		saved, err := cfg.Store.Latest(cfg.Dir)
		if err != nil {
			t.Fatal(err)
		}
		beforeParent, beforeChild := client.counts()
		for i := 0; i < 3; i++ {
			var out, diag bytes.Buffer
			code := runAutomation(t.Context(), Options{Resume: true, ResumeID: saved.ID, OutputFormat: format}, nil, &out, &diag, cfg.Dir, cfg.HomeDir, cfg.Store, func() (appConfig, error) {
				t.Fatal("saved response entered live setup")
				return appConfig{}, errors.New("invalid credentials, tool binding, attachment storage, and MCP")
			})
			if code != 0 || !reflect.DeepEqual(decodeResponses(t, format, out.String()), want[1:]) {
				t.Fatalf("saved code=%d %s %s", code, out.String(), diag.String())
			}
		}
		p, c := client.counts()
		if p != beforeParent || c != beforeChild {
			t.Fatal("saved read dispatched")
		}
	}
}
func TestAutomationEmptySavedAnswerNeverBorrowsHistory(t *testing.T) {
	old := adapters
	t.Cleanup(func() { adapters = old })
	adapters = append(slicesClone(old), func(Env) (wiring, error) {
		return wiring{Hooks: dashi.Hooks{BeforeFinish: []dashi.FinishHook{{Name: "fixture empty", Func: func(context.Context, dashi.FinishPlan) (dashi.FinishDecision, error) {
			return dashi.FinishDecision{AcceptEmpty: true}, nil
		}}}}}, nil
	})
	cfg := testConfig(t, say("old"), say(""))
	code, _, diag := runAutomationFixture(t, cfg, Options{Print: "first", PrintSet: true, OutputFormat: "json"}, "")
	if code != 0 {
		t.Fatal(diag)
	}
	saved, _ := cfg.Store.Latest(cfg.Dir)
	code, out, diag := runAutomationFixture(t, cfg, Options{Resume: true, ResumeID: saved.ID, Print: "new", PrintSet: true, OutputFormat: "json"}, "")
	if code != 0 || out != "{\"messages\":[]}\n" {
		t.Fatalf("new %d %s %s", code, out, diag)
	}
	for _, format := range []string{"json", "jsonl"} {
		code, out, diag := runAutomationFixture(t, cfg, Options{Resume: true, ResumeID: saved.ID, OutputFormat: format}, "")
		if code != 0 || len(decodeResponses(t, format, out)) != 0 {
			t.Fatalf("saved %d %s %s", code, out, diag)
		}
	}
}
func TestAutomationBlockedSavedContentAndEveryTarget(t *testing.T) {
	var runs atomic.Int32
	installTaskEffect(t, &runs, true)
	cfg := testConfig(t)
	cfg.Client = &taskFixtureClient{effect: true}
	code, out, diag := runAutomationFixture(t, cfg, Options{Print: "delegate", PrintSet: true, Yes: true, OutputFormat: "json"}, "")
	if code != 3 || len(decodeResponses(t, "json", out)) != 1 || !strings.Contains(diag, "task ") || !strings.Contains(diag, "call effect-call") {
		t.Fatalf("%d %s %s", code, out, diag)
	}
	saved, _ := cfg.Store.Latest(cfg.Dir)
	var result, stderr bytes.Buffer
	code = runAutomation(t.Context(), Options{Resume: true, ResumeID: saved.ID, OutputFormat: "json"}, nil, &result, &stderr, cfg.Dir, cfg.HomeDir, cfg.Store, func() (appConfig, error) { t.Fatal("blocked saved read entered live setup"); return cfg, nil })
	if code != 3 || responseContentFromJSON(t, result.String()) != "Parent remains available." {
		t.Fatalf("%d %s %s", code, result.String(), stderr.String())
	}
	if runs.Load() != 1 {
		t.Fatalf("effects=%d", runs.Load())
	}
}
func responseContentFromJSON(t *testing.T, data string) string {
	got := decodeResponses(t, "json", data)
	if len(got) == 0 {
		return ""
	}
	return got[0].Content
}
func TestAutomationCorruptLatestAndAnotherOwnerFailWithoutFallback(t *testing.T) {
	cfg := testConfig(t, say("older answer"), say("latest answer"))
	runAutomationFixture(t, cfg, Options{Print: "one", PrintSet: true}, "")
	saved, _ := cfg.Store.Latest(cfg.Dir)
	runAutomationFixture(t, cfg, Options{Print: "two", PrintSet: true, Resume: true, ResumeID: saved.ID}, "")
	unlock, err := cfg.Store.Lock(saved)
	if err != nil {
		t.Fatal(err)
	}
	code, out, _ := runAutomationFixture(t, cfg, Options{Resume: true, ResumeID: saved.ID, OutputFormat: "json"}, "")
	unlock()
	if code != 1 || out != "{\"messages\":[]}\n" {
		t.Fatalf("busy %d %s", code, out)
	}
	sum := sha256.Sum256([]byte(session.TurnID(saved.ID, 2)))
	hash := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(cfg.Store.Root(), saved.ID, "turns", hash[:2], hash+".jsonl"), []byte("not-json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, diag := runAutomationFixture(t, cfg, Options{Resume: true, ResumeID: saved.ID, OutputFormat: "json"}, "")
	if code != 1 || out != "{\"messages\":[]}\n" || !strings.Contains(diag, "unreadable") {
		t.Fatalf("corrupt %d %s %s", code, out, diag)
	}
}
func TestAutomationExitPriorityAndAllAttentionGuidance(t *testing.T) {
	affected := []automationAttention{{TurnID: "z", Stop: dashi.Stop{Kind: dashi.StopCanceled}}, {TurnID: "y", Stop: dashi.Stop{Kind: dashi.StopRefused}}, {TurnID: "x", Stop: dashi.Stop{Kind: dashi.StopLimited}}, {TurnID: "w", TaskID: "task", CallID: "call", Stop: dashi.Stop{Kind: dashi.StopWaiting, Reason: "Authorization: TOKEN\nhttps://user:pass@example.test/path?secret=x"}}, {TurnID: "v", Stop: dashi.Stop{Kind: dashi.StopFailed}}}
	for _, tt := range []struct {
		signal            int
		failed, preflight bool
		want              int
	}{{0, false, false, 1}, {0, true, true, 1}, {0, false, true, 2}, {130, true, true, 130}, {143, true, true, 143}} {
		if got := automationCode(tt.signal, tt.failed, tt.preflight, affected); got != tt.want {
			t.Fatalf("%+v got=%d", tt, got)
		}
	}
	if automationCode(0, false, false, affected[:4]) != 3 || automationCode(0, false, false, affected[:3]) != 4 {
		t.Fatal("rank order changed")
	}
	var diag bytes.Buffer
	printAutomationAttention(&diag, affected, "w")
	for _, id := range []string{"Turn v", "Turn w", "Turn x", "Turn y", "Turn z", "task task", "call call"} {
		if !strings.Contains(diag.String(), id) {
			t.Fatal(diag.String())
		}
	}
	for _, secret := range []string{"TOKEN", "user:pass", "secret=x"} {
		if strings.Contains(diag.String(), secret) {
			t.Fatal(diag.String())
		}
	}
}
func TestAutomationOutputCoalescesWithoutDroppingOrBlocking(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	o, err := newAutomationOutput(t.Context(), "jsonl", server)
	if err != nil {
		t.Fatal(err)
	}
	o.source(func(_ context.Context, n int) (dashi.TurnView, error) {
		return dashi.TurnView{Origin: dashi.StartUser, Reply: fmt.Sprint(n)}, nil
	}, nil)
	for i := 1; i <= 1000; i++ {
		o.selectTurn(i)
	}
	read := make(chan []byte, 1)
	go func() { data, _ := io.ReadAll(client); read <- data }()
	if err := o.Finish(); err != nil {
		t.Fatal(err)
	}
	server.Close()
	got := decodeResponses(t, "jsonl", string(<-read))
	if len(got) != 1000 {
		t.Fatalf("messages=%d", len(got))
	}
	for i, m := range got {
		if m.Content != fmt.Sprint(i+1) {
			t.Fatalf("%d %s", i, m.Content)
		}
	}
}
func TestAutomationOutputFailureCancelsOwnedExecution(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	w, err := qualifyAutomationWriter(server)
	if err != nil {
		t.Fatal(err)
	}
	o := startAutomationOutput(t.Context(), "jsonl", w, 30*time.Millisecond)
	var canceled atomic.Bool
	o.source(func(context.Context, int) (dashi.TurnView, error) {
		return dashi.TurnView{Reply: "response", Origin: dashi.StartUser}, nil
	}, func() { canceled.Store(true) })
	o.selectTurn(1)
	if err := o.Finish(); err == nil || !canceled.Load() {
		t.Fatal("stalled reader certified success")
	}
}
func TestAutomationFormatsAndRoutes(t *testing.T) {
	for _, args := range [][]string{{"--output-format", "JSON"}, {"--output-format", "json", "--plain"}, {"--output-format", "text", "--inline"}, {"models", "--output-format", "json"}} {
		if _, err := parseArgs(args, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if !automationOneShot(Options{}, strings.NewReader("data")) || automationOneShot(Options{Plain: true}, strings.NewReader("data")) || !automationOneShot(Options{OutputFormatSet: true}, nil) {
		t.Fatal("wrong route")
	}
}
func TestAutomationRunProcessFixture(t *testing.T) {
	mode := os.Getenv("DASHI_AUTOMATION_RUN_FIXTURE")
	if mode == "" {
		return
	}
	cfg := testConfig(t, say("The test failed: expected 3, got 4.\n日本\u2028"))
	args := Options{Print: "Explain the failure", PrintSet: true, OutputFormat: mode}
	if mode == "signal-input" {
		args.OutputFormat = "json"
		fmt.Fprintln(os.Stderr, "ready")
	}
	code := runAutomation(context.Background(), args, os.Stdin, os.Stdout, os.Stderr, cfg.Dir, cfg.HomeDir, cfg.Store, func() (appConfig, error) { return cfg, nil })
	os.Exit(code)
}
func TestAutomationCIKeepsBuildStatus(t *testing.T) {
	dir := t.TempDir()
	script := `build_status=0
"$1" -test.run=^TestAutomationFailedBuildFixture$ >build.log 2>&1 || build_status=$?
if [ "$build_status" -ne 0 ]; then
 "$1" -test.run=^TestAutomationRunProcessFixture$ <build.log >response.json || true
fi
cat response.json
exit "$build_status"
`
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script, "fixture", os.Args[0])
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DASHI_AUTOMATION_RUN_FIXTURE=json", "DASHI_AUTOMATION_BUILD_FAIL=1")
	var out, diag bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diag
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || responseContentFromJSON(t, out.String()) != "The test failed: expected 3, got 4.\n日本\u2028" {
		t.Fatalf("err=%v out=%s diag=%s", err, out.String(), diag.String())
	}
}

func TestAutomationPartialParentAnswerAndSafeBlockedResume(t *testing.T) {
	var effects atomic.Int32
	installTaskEffect(t, &effects, true)
	response := func(ctx context.Context, call llm.Call) (llm.Result, error) {
		result, err := callTool("effect-call", "external_effect", `{"target":"once"}`)(ctx, call)
		result.Message.Content = append([]llm.Content{llm.Text("I started the check.\nVerify its external result.")}, result.Message.Content...)
		return result, err
	}
	cfg := testConfig(t, response)
	code, out, diag := runAutomationFixture(t, cfg, Options{Print: "start", PrintSet: true, Yes: true, OutputFormat: "jsonl"}, "")
	if code != 3 || len(decodeResponses(t, "jsonl", out)) != 1 || !strings.Contains(diag, "waiting") {
		t.Fatalf("%d %s %s", code, out, diag)
	}
	saved, _ := cfg.Store.Latest(cfg.Dir)
	for i := 0; i < 2; i++ {
		code, content, diag := runAutomationFixture(t, cfg, Options{Resume: true, ResumeID: saved.ID, OutputFormat: "json"}, "")
		if code != 3 || responseContentFromJSON(t, content) != "I started the check.\nVerify its external result." {
			t.Fatalf("%d %s %s", code, content, diag)
		}
	}
	if effects.Load() != 1 || cfg.Client.(*fakeClient).sends != 1 {
		t.Fatal("blocked read retried effect")
	}
	code, out, _ = runAutomationFixture(t, cfg, Options{Resume: true, ResumeID: saved.ID, Print: "new request", PrintSet: true, OutputFormat: "json"}, "")
	if code != 3 || len(decodeResponses(t, "json", out)) != 0 || effects.Load() != 1 {
		t.Fatal("new payload closed pending work")
	}
}
func TestAutomationInterruptedParentContinuesWithoutRepeatingEffect(t *testing.T) {
	var effects atomic.Int32
	installTaskEffect(t, &effects, false)
	started := make(chan struct{})
	cfg := testConfig(t, callTool("effect-call", "external_effect", `{"target":"once"}`), blockUntilDone(started), say("Finished once."))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan int, 1)
	var out, diag bytes.Buffer
	go func() {
		done <- runAutomation(ctx, Options{Print: "run", PrintSet: true, Yes: true, OutputFormat: "json"}, nil, &out, &diag, cfg.Dir, cfg.HomeDir, cfg.Store, func() (appConfig, error) { return cfg, nil })
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not start")
	}
	if code := <-done; code != 3 {
		t.Fatalf("interrupted %d %s %s", code, out.String(), diag.String())
	}
	saved, _ := cfg.Store.Latest(cfg.Dir)
	code, content, stderr := runAutomationFixture(t, cfg, Options{Resume: true, ResumeID: saved.ID, OutputFormat: "json"}, "")
	if code != 0 || responseContentFromJSON(t, content) != "Finished once." || effects.Load() != 1 {
		t.Fatalf("resume %d effects=%d %s %s", code, effects.Load(), content, stderr)
	}
}
func TestAutomationMachineActiveCompactionExcludesPrivateSummary(t *testing.T) {
	cfg, client := activeFixtureConfig(t, false)
	code, out, diag := runAutomationFixture(t, cfg, Options{Print: activeObjective, PrintSet: true, Yes: true, OutputFormat: "json"}, "")
	if code != 0 || client.summaries < 2 {
		t.Fatalf("%d %s", code, diag)
	}
	got := decodeResponses(t, "json", out)
	if len(got) != 1 || strings.Contains(out, activePrivateSummary) {
		t.Fatal(out)
	}
}
func TestAutomationFailedBuildFixture(t *testing.T) {
	if os.Getenv("DASHI_AUTOMATION_BUILD_FAIL") == "1" {
		t.Fatal("expected 3, got 4")
	}
}

func TestAutomationWorkspacePreflightStillHasEmptyMachineOutput(t *testing.T) {
	var out, diag bytes.Buffer
	code := run(t.Context(), []string{"--workspace", filepath.Join(t.TempDir(), "missing"), "--output-format", "json"}, strings.NewReader("data"), &out, &diag)
	if code != 1 || out.String() != "{\"messages\":[]}\n" {
		t.Fatalf("%d %q %s", code, out.String(), diag.String())
	}
}

func TestAutomationAttachmentOnlyNewAndResumedRequests(t *testing.T) {
	cfg := testConfig(t, say("first file"), say("second file"))
	path := filepath.Join(cfg.Dir, "evidence.txt")
	if err := os.WriteFile(path, []byte("selected bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	o := Options{PrintSet: true, AttachFiles: []string{path}, OutputFormat: "json"}
	for _, want := range []string{"first file", "second file"} {
		code, out, diag := runAutomationFixture(t, cfg, o, "")
		if code != 0 || !reflect.DeepEqual(decodeResponses(t, "json", out), []responseMessage{{want}}) {
			t.Fatalf("attachment-only code=%d out=%s diag=%s", code, out, diag)
		}
		client := cfg.Client.(*fakeClient)
		request := client.inputAt(client.sends - 1)
		found := false
		for _, message := range request.Messages {
			for _, c := range message.Content {
				if c.Block != nil && c.Block.Document != nil && string(c.Block.Document.Source.Data) == "selected bytes" {
					found = true
				}
			}
		}
		if !found {
			t.Fatal("selected file did not reach the model")
		}
		saved, err := cfg.Store.Latest(cfg.Dir)
		if err != nil {
			t.Fatal(err)
		}
		o.Resume, o.ResumeID = true, saved.ID
	}
	if cfg.Client.(*fakeClient).sends != 2 {
		t.Fatal("attachment-only resume was treated as a saved-response read")
	}
}
