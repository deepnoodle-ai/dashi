//go:build darwin || linux

package sessionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/dashi/workspace"
)

type inspectionClient struct {
	results []llm.Result
	sent    int
	window  int
}

func (*inspectionClient) Provider() string { return "test" }
func (*inspectionClient) API() string      { return "test.chat" }
func (c *inspectionClient) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{Capabilities: llm.Capabilities{ContextWindow: c.window}}, c.window > 0
}
func (c *inspectionClient) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return llm.Request{}, err
	}
	info, _ := c.ModelInfo(in.Model)
	info.Evidence = llm.ModelEvidence{Model: in.Model, API: "test.chat", Endpoint: "https://test.example", Owner: "test", Source: "fixture", Method: llm.EvidenceFixture, QualifiedAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)}
	return llm.NewRequest(llm.RequestSnapshot{Input: in, Effective: in.Settings, Body: body, Origin: content.Origin{Provider: "test", API: "test.chat", Model: in.Model, Scope: "fixture"}, ModelInfo: &info})
}
func (c *inspectionClient) Send(context.Context, llm.Call) (llm.Result, error) {
	if c.sent >= len(c.results) {
		return llm.Result{}, errors.New("unexpected Send")
	}
	result := c.results[c.sent]
	c.sent++
	return result, nil
}
func reply(text, currency string) llm.Result {
	return llm.Result{Requested: llm.ModelRef{Provider: "test", Model: "recorded-model"}, Message: llm.AssistantMessage(llm.Text(text)), Stop: llm.Stop{Kind: llm.StopFinished}, Usage: &llm.Usage{InputTokens: 100, CacheReadInputTokens: 20, CacheCreationInputTokens: 5, OutputTokens: 10}, Cost: &llm.Cost{Currency: currency, Total: money.MustParseAmount("0.25"), Source: llm.CostSourceProviderReported}}
}
func inspectionStore(t *testing.T) (*Store, string) {
	t.Helper()
	w, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := NewStore(filepath.Join(t.TempDir(), "sessions"))
	st.DisableSync()
	return st, w.Dir()
}
func savedSession(t *testing.T, st *Store, dir, title string) (*Session, *filerecorder.Recorder) {
	t.Helper()
	s, err := st.Create(dir, "saved-model")
	if err != nil {
		t.Fatal(err)
	}
	s.Title = title
	s.NextTurn = json.RawMessage(`{"provider":"future","model":"saved-model"}`)
	rec, err := st.Recorder(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	return s, rec
}
func runInspectionTurn(t *testing.T, rec dashi.Recorder, id string, client *inspectionClient, start dashi.Start, tools tool.Toolset) dashi.Outcome {
	t.Helper()
	if start.Agent.Model == "" {
		start.Agent = dashi.Agent{ID: "fixture", Revision: "recorded-route", Provider: "test", API: "test.chat", Model: "recorded-model"}
	}
	out, err := (&dashi.Runner{Client: client, Recorder: rec, Tools: tools}).Run(t.Context(), dashi.NewTurn(id), start)
	if err != nil || out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("run stop=%+v err=%v", out.Stop, err)
	}
	return out
}
func storeImage(t *testing.T, path string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(path, func(name string, e os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(path, name)
		value := fmt.Sprint(info.Mode(), " ", info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			value += fmt.Sprintf(" %x", sha256.Sum256(data))
		}
		out[rel] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInspectionSearchCardsAndReadOnlyOwnedElsewhere(t *testing.T) {
	st, dir := inspectionStore(t)
	s, rec := savedSession(t, st, dir, "title-only investigation")
	runInspectionTurn(t, rec, session.TurnID(s.ID, 1), &inspectionClient{results: []llm.Result{reply("Retry handler answer", "USD")}, window: 1000}, dashi.Start{Input: dashi.Say("Review the handler").Input}, nil)
	unlock, err := st.Lock(s)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	before := storeImage(t, st.root)
	for _, query := range []string{"title-only", "RETRY HANDLER", s.ID} {
		result, err := st.Search(t.Context(), dir, query)
		if err != nil || len(result.Matches) != 1 {
			t.Fatalf("search %q: %+v %v", query, result, err)
		}
		card := result.Matches[0].Card
		if card.Metadata.Title != s.Title || !card.Metadata.Updated.Equal(s.Updated) || card.Request == nil || card.Request.Text != "Review the handler" || card.Latest == nil || card.Latest.Stop.Kind != dashi.StopCompleted || card.Latest.Revision == 0 {
			t.Fatalf("incomplete card: %+v", card)
		}
		if query == "RETRY HANDLER" && (result.Matches[0].Match == nil || result.Matches[0].Match.TurnID != session.TurnID(s.ID, 1)) {
			t.Fatal("matched text has no snapshot identity")
		}
		if query == "title-only" && result.Matches[0].Match != nil {
			t.Fatal("title-only match invented text evidence")
		}
		if card.Latest.Destination.Provider != "test" || card.NextDestination.Provider != "future" {
			t.Fatal("recorded and next destination conflated")
		}
		if card.Accounting.ModelCalls != 1 || card.Latest.Context.Tokens != 135 || card.Latest.Context.Window != 1000 {
			t.Fatalf("accounting/context: %+v %+v", card.Accounting, card.Latest.Context)
		}
	}
	report, err := st.Inspect(t.Context(), dir, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = FormatInspection(report)
	if _, err := st.Lock(s); !errors.Is(err, ErrBusy) {
		t.Fatalf("inspection displaced lock: %v", err)
	}
	if after := storeImage(t, st.root); !reflect.DeepEqual(before, after) {
		t.Fatal("inspection changed storage")
	}
	s.Title = "changed after snapshot"
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	if text := FormatInspection(report); !strings.Contains(text, "title-only investigation") || strings.Contains(text, s.Title) {
		t.Fatal("card reread mutable metadata")
	}
}

func TestInspectionWorkspaceSecurityAndMetadataBounds(t *testing.T) {
	st, dir := inspectionStore(t)
	other, _ := workspace.New(t.TempDir())
	foreign, _ := savedSession(t, st, other.Dir(), "secret foreign title")
	local, rec := savedSession(t, st, dir, "local")
	runInspectionTurn(t, rec, session.TurnID(local.ID, 1), &inspectionClient{results: []llm.Result{reply("local answer", "USD")}}, dashi.Start{Input: dashi.Say("local request").Input}, nil)
	if _, err := st.Inspect(t.Context(), dir, foreign.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign=%v", err)
	}
	if result, err := st.Search(t.Context(), dir, foreign.ID); err != nil || len(result.Matches) != 0 {
		t.Fatalf("foreign search %+v %v", result, err)
	}
	for _, id := range []string{"../" + local.ID, "/tmp/x", local.ID + "/turns", "20269999-256199-aaaaaa", string([]byte{0xff})} {
		if _, err := st.Inspect(t.Context(), dir, id); err == nil {
			t.Fatalf("accepted ID %q", id)
		}
	}
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Inspect(t.Context(), alias, local.ID); err != nil {
		t.Fatalf("canonical alias: %v", err)
	}
	for _, kind := range []string{"metadata link", "oversize", "ID mismatch", "session link", "turns link"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := savedSession(t, st, dir, "hidden "+kind)
			path := filepath.Join(st.root, s.ID)
			switch kind {
			case "metadata link":
				if err := os.Remove(filepath.Join(path, "session.json")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(st.root, foreign.ID, "session.json"), filepath.Join(path, "session.json")); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(filepath.Join(path, "session.json"), []byte(strings.Repeat(" ", inspectionMetadataLimit+1)), 0o600); err != nil {
					t.Fatal(err)
				}
			case "ID mismatch":
				s.ID = foreign.ID
				data, _ := json.Marshal(s)
				if err := os.WriteFile(filepath.Join(path, "session.json"), data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "session link":
				if err := os.Rename(path, path+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-old", path); err != nil {
					t.Fatal(err)
				}
			case "turns link":
				if err := os.RemoveAll(filepath.Join(path, "turns")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(st.root, foreign.ID, "turns"), filepath.Join(path, "turns")); err != nil {
					t.Fatal(err)
				}
			}
			list, err := st.Search(t.Context(), dir, "hidden "+kind)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "turns link" {
				if len(list.Matches) != 1 || !list.Matches[0].Card.RecordUnavailable {
					t.Fatal("title match lost uncertainty")
				}
			} else {
				if len(list.Matches) != 0 {
					t.Fatal("invalid metadata exposed title")
				}
				for _, problem := range list.Problems {
					if strings.Contains(problem.Message, "secret") || strings.Contains(problem.Message, "hidden") {
						t.Fatal("unscoped warning leaked content")
					}
				}
			}
		})
	}
}

func TestInspectionLatestUnreadableClaimOnlyAndMissingStorage(t *testing.T) {
	st, dir := inspectionStore(t)
	s, rec := savedSession(t, st, dir, "investigation")
	runInspectionTurn(t, rec, session.TurnID(s.ID, 1), &inspectionClient{results: []llm.Result{reply("good answer", "USD")}}, dashi.Start{Input: dashi.Say("good request").Input}, nil)
	runInspectionTurn(t, rec, session.TurnID(s.ID, 2), &inspectionClient{results: []llm.Result{reply("later answer", "EUR")}}, dashi.Start{Input: dashi.Say("later request").Input}, nil)
	if err := os.WriteFile(rec.Path(session.TurnID(s.ID, 2)), []byte("damaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rec.Claim(t.Context(), session.TurnID(s.ID, 3), "external-worker"); err != nil {
		t.Fatal(err)
	}
	before := storeImage(t, st.root)
	report, err := st.Inspect(t.Context(), dir, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.IndexedTurns != 3 || report.ReadableTurns != 1 || report.OmittedTurns != 2 || report.Latest == nil || report.Latest.Readable || report.Latest.TurnID != session.TurnID(s.ID, 3) || !report.RequestIncomplete {
		t.Fatalf("lost latest uncertainty: %+v", report)
	}
	if !report.Accounting.Incomplete || report.Accounting.Spend.Cost["USD"] != money.MustParseAmount("0.25") || report.Accounting.Spend.CostUnknown != 0 {
		t.Fatal("omissions changed recorded accounting")
	}
	if text := FormatInspection(report); !strings.Contains(text, "at least 0.25 USD") || !strings.Contains(text, "Latest execution unreadable") || strings.Contains(text, "Recorded stop: completed") {
		t.Fatalf("misleading report: %s", text)
	}
	if matches, err := st.Search(t.Context(), dir, "investigation"); err != nil || len(matches.Matches) != 1 || !matches.TextMatchesMayBeMissing {
		t.Fatalf("damaged match: %+v %v", matches, err)
	}
	if after := storeImage(t, st.root); !reflect.DeepEqual(before, after) {
		t.Fatal("unreadable inspection changed storage")
	}
	empty, _ := savedSession(t, st, dir, "missing storage")
	if err := os.RemoveAll(filepath.Join(st.root, empty.ID, "turns")); err != nil {
		t.Fatal(err)
	}
	if r, err := st.Inspect(t.Context(), dir, empty.ID); err != nil || !r.RecordUnavailable || !r.Accounting.Incomplete || strings.Contains(FormatInspection(r), "0 indexed") || !strings.Contains(FormatInspection(r), "omitted Turn count unknown") {
		t.Fatalf("missing storage %+v %v", r, err)
	}
	if _, err := os.Stat(filepath.Join(st.root, empty.ID, "turns")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing storage was created")
	}
}

func TestInspectionLiteralSearchSteeringAndSummaryExclusion(t *testing.T) {
	st, dir := inspectionStore(t)
	s, rec := savedSession(t, st, dir, "plain title")
	out := runInspectionTurn(t, rec, session.TurnID(s.ID, 1), &inspectionClient{results: []llm.Result{reply("plain answer", "USD")}}, dashi.Start{Origin: dashi.StartOperator, Input: []llm.Message{llm.Message{Role: llm.RoleSystem, Content: []llm.Content{llm.Text("system secret")}}, llm.UserMessage(llm.Text("regex [a.*] two  spaces"))}}, nil)
	prefix := dashi.NewTurn(session.TurnID(s.ID, 2))
	start := out.Turn.Steps[0]
	start.TurnID = prefix.ID
	start.Started.Input = dashi.Say("next ordinary request").Input
	prefix.Steps = []dashi.Step{start, {Version: 1, TurnID: prefix.ID, Seq: 2, Kind: dashi.StepMessagesAdded, Context: &dashi.ContextFact{Items: []dashi.ContextItem{{ID: "steer-1", Message: llm.UserMessage(llm.Text("steering needle"))}, {ID: "runtime-1", Origin: dashi.ContextRuntime, Message: llm.UserMessage(llm.Text("runtime secret"))}}}}}
	if _, err := dashi.Replay(prefix); err != nil {
		t.Fatal(err)
	}
	for _, step := range prefix.Steps {
		if err := rec.Commit(t.Context(), dashi.CommitRequest{ExpectedSeq: step.Seq - 1, Step: step}); err != nil {
			t.Fatal(err)
		}
	}
	runInspectionTurn(t, rec, session.TurnID(s.ID, 3), &inspectionClient{results: []llm.Result{reply("internal summary secret", "EUR")}}, dashi.Start{Origin: dashi.StartCompaction, Compaction: &dashi.CompactionFact{Through: 2, Digest: "fixture"}, Input: dashi.Say("summary prompt secret").Input}, nil)
	for _, query := range []string{"[a.*]", "two  spaces", "steering needle"} {
		if r, err := st.Search(t.Context(), dir, query); err != nil || len(r.Matches) != 1 || r.Matches[0].Match == nil {
			t.Fatalf("literal %q: %+v %v", query, r, err)
		}
	}
	for _, query := range []string{"aZZZZ", "two spaces", "system secret", "runtime secret", "internal summary secret", "summary prompt secret"} {
		if r, err := st.Search(t.Context(), dir, query); err != nil || len(r.Matches) != 0 {
			t.Fatalf("excluded %q: %+v %v", query, r, err)
		}
	}
	report, err := st.Inspect(t.Context(), dir, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Latest.Summary || report.Request.TurnID != session.TurnID(s.ID, 2) || report.Accounting.ModelCalls != 2 || report.Accounting.Spend.Cost["EUR"] != money.MustParseAmount("0.25") {
		t.Fatalf("summary/request/accounting: %+v", report)
	}
}

func TestInspectionBoundCancellationAndNoCreate(t *testing.T) {
	st, dir := inspectionStore(t)
	if r, err := st.Search(t.Context(), dir, ""); err != nil || r.Sessions != 0 {
		t.Fatalf("empty %+v %v", r, err)
	}
	if _, err := os.Stat(st.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty search created storage")
	}
	st.now = func() time.Time { return time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC) }
	for i := 0; i < 52; i++ {
		savedSession(t, st, dir, fmt.Sprintf("title %d", i))
	}
	r, err := st.Search(t.Context(), dir, "title")
	if err != nil || len(r.Matches) != 50 || !r.Truncated || r.Sessions != 52 {
		t.Fatalf("bound %+v %v", r, err)
	}
	for i := 1; i < len(r.Matches); i++ {
		if r.Matches[i-1].Card.Metadata.ID > r.Matches[i].Card.Metadata.ID {
			t.Fatal("tie ordering unstable")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := st.Search(ctx, dir, "title"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v", err)
	}
	if r, err := st.Search(t.Context(), dir, "absent"); err != nil || len(r.Matches) != 0 || r.Sessions != 52 || r.Truncated {
		t.Fatalf("no matches %+v %v", r, err)
	}
}

func TestRenameRollbackRestartAndValidation(t *testing.T) {
	st, dir := inspectionStore(t)
	current, rec := savedSession(t, st, dir, "old title")
	runInspectionTurn(t, rec, session.TurnID(current.ID, 1), &inspectionClient{results: []llm.Result{reply("answer", "USD")}}, dashi.Start{Input: dashi.Say("request").Input}, nil)
	unlock, err := st.Lock(current)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	before := *current
	before.NextTurn = bytes.Clone(current.NextTurn)
	diskBefore := storeImage(t, st.root)
	rejected := errors.New("injected replacement failure")
	if next, err := st.rename(current, "new title", func() error { return rejected }); next != nil || !errors.Is(err, rejected) {
		t.Fatalf("failure %+v %v", next, err)
	}
	if !reflect.DeepEqual(*current, before) {
		t.Fatal("failed rename mutated current")
	}
	oldData, _ := os.ReadFile(filepath.Join(st.root, current.ID, "session.json"))
	var old Session
	if err := json.Unmarshal(oldData, &old); err != nil || old.Title != before.Title || !old.Updated.Equal(before.Updated) {
		t.Fatal("failed rename replaced metadata")
	}
	for path := range storeImage(t, st.root) {
		if strings.HasSuffix(path, ".tmp") {
			t.Fatal("temporary metadata leaked")
		}
	}
	next, err := st.Rename(current, "  調査 rate-limit  ")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*current, before) || next.Title != "調査 rate-limit" {
		t.Fatal("candidate alias or title")
	}
	next.NextTurn[0] = 'x'
	if current.NextTurn[0] == 'x' {
		t.Fatal("candidate aliases caller preferences")
	}
	reopened, err := NewStore(st.root).Inspect(t.Context(), dir, current.ID)
	if err != nil || reopened.Metadata.Title != "調査 rate-limit" {
		t.Fatalf("relaunch %+v %v", reopened, err)
	}
	if reopened.Metadata.ID != before.ID || !reopened.Metadata.Created.Equal(before.Created) || reopened.Metadata.Dir != before.Dir || reopened.Metadata.Model != before.Model || !jsonSame(reopened.Metadata.NextTurn, before.NextTurn) {
		t.Fatal("unrelated metadata changed")
	}
	for path, value := range storeImage(t, st.root) {
		if strings.Contains(path, ".jsonl") && diskBefore[path] != value {
			t.Fatal("Turn facts changed")
		}
	}
	if r, err := st.Search(t.Context(), dir, "調査"); err != nil || len(r.Matches) != 1 {
		t.Fatalf("renamed search %+v %v", r, err)
	}
	for _, title := range []string{"", "   ", "two\nlines", "tab\tend", "\ttrimmed", "escape\x1b", "bidi\u202e", strings.Repeat("a", 161), string([]byte{0xff})} {
		if _, err := st.Rename(current, title); err == nil {
			t.Fatalf("accepted title %q", title)
		}
	}
	unsaved, _ := st.Create(dir, "m")
	if _, err := st.Rename(unsaved, "not saved"); err == nil {
		t.Fatal("rename created empty saved session")
	}
}

func TestAccountingCompletenessCurrencyAndEndpoint(t *testing.T) {
	a := Accounting{Spend: dashi.Spend{Usage: llm.Usage{InputTokens: math.MaxInt - 1}, Cost: map[string]money.Amount{"USD": money.MustParseAmount("0.5")}, ToolCost: map[string]money.Amount{"EUR": money.MustParseAmount("2")}}}
	b := Accounting{Spend: dashi.Spend{Usage: llm.Usage{OutputTokens: 2}, Cost: map[string]money.Amount{"USD": money.MustParseAmount("0.25"), "EUR": money.MustParseAmount("0.1")}}}
	sum := a.add(b)
	if !sum.Spend.UsageIncomplete || sum.Spend.CostUnknown != 0 {
		t.Fatal("usage failure corrupted monetary meaning")
	}
	if text := FormatAccounting(sum); !strings.Contains(text, "0.1 EUR, 0.75 USD") || strings.Contains(text, "at least") || !strings.Contains(text, "Usage incomplete") {
		t.Fatalf("independent money: %s", text)
	}
	sum.Incomplete = true
	sum.OmittedTurns = 1
	if text := FormatAccounting(sum); !strings.Contains(text, "at least 0.1 EUR, 0.75 USD") || !strings.Contains(text, "omitted Turns: 1") {
		t.Fatalf("omitted money: %s", text)
	}
	if got := PrintableEndpoint("https://name:secret@host.test/v1?key=secret#private"); got != "https://host.test/v1" {
		t.Fatalf("endpoint=%q", got)
	}
	if got := PrintableEndpoint("secret broken endpoint"); got != "unavailable" {
		t.Fatalf("malformed endpoint=%q", got)
	}
}

func jsonSame(a, b []byte) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflect.DeepEqual(left, right)
}

func TestInspectionActiveSummariesToolOutputsAndPinnedContext(t *testing.T) {
	st, dir := inspectionStore(t)
	s, rec := savedSession(t, st, dir, "long investigation")
	var results []llm.Result
	for i := 1; i <= 4; i++ {
		r := reply("", "USD")
		r.Message = llm.AssistantMessage(llm.Content{Kind: llm.ContentToolCall, ToolCall: &tool.Call{ID: fmt.Sprintf("read-%d", i), Name: "read", Args: json.RawMessage(`{}`)}})
		r.Stop.Kind = llm.StopToolUse
		results = append(results, r)
		if i%2 == 0 {
			results = append(results, reply("<summary>INTERNAL_SUMMARY_NEEDLE original objective and evidence retained.</summary>", "USD"))
		}
	}
	results = append(results, reply("Ordinary final answer searchable", "USD"))
	calls := 0
	tools := tool.List{tool.MustFunc("read", "Read fixture evidence", func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
		calls++
		outcome := tool.Succeeded("TOOL_OUTPUT_NEEDLE " + strings.Repeat("x", 9000))
		outcome.Cost = &tool.Cost{Currency: "EUR", Amount: money.MustParseAmount("0.5")}
		return outcome, nil
	})}
	client := &inspectionClient{results: results, window: 16000}
	start := dashi.Start{Agent: dashi.Agent{ID: "fixture", Revision: "recorded-route", Provider: "test", API: "test.chat", Model: "recorded-model", Settings: llm.Settings{MaxTokens: 256}, Tools: []string{"read"}, Limits: dashi.Limits{ModelAttempts: 16, ToolResultBytes: 12000}, ActiveCompaction: &dashi.ActiveCompactionPolicy{Window: 16000, MaxTokens: 256}}, Input: dashi.Say("Original objective: investigate evidence").Input}
	out := runInspectionTurn(t, rec, session.TurnID(s.ID, 1), client, start, tools)
	applied := 0
	for _, step := range out.Turn.Steps {
		if step.Compacted != nil {
			applied++
		}
	}
	if applied != 2 || client.sent != 7 || calls != 4 {
		t.Fatalf("fixture compactions=%d Sends=%d effects=%d", applied, client.sent, calls)
	}
	report, err := st.Inspect(t.Context(), dir, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Accounting.ModelCalls != 7 || report.Accounting.Spend.Cost["USD"] != money.MustParseAmount("1.75") || report.Accounting.Spend.ToolCost["EUR"] != money.MustParseAmount("2") {
		t.Fatalf("summary accounting=%+v", report.Accounting)
	}
	if !report.Latest.Context.Available || report.Latest.Context.Tokens != 135 || report.Latest.Context.Window != 16000 {
		t.Fatalf("per-call context=%+v", report.Latest.Context)
	}
	for _, query := range []string{"INTERNAL_SUMMARY_NEEDLE", "TOOL_OUTPUT_NEEDLE", "read-1"} {
		if r, err := st.Search(t.Context(), dir, query); err != nil || len(r.Matches) != 0 {
			t.Fatalf("internal search %q %+v %v", query, r, err)
		}
	}
	if r, err := st.Search(t.Context(), dir, "Ordinary final answer"); err != nil || len(r.Matches) != 1 {
		t.Fatalf("ordinary answer=%+v %v", r, err)
	}
	// A current catalog change cannot alter the recorded observation.
	client.window = 32000
	again, err := st.Inspect(t.Context(), dir, s.ID)
	if err != nil || again.Latest.Context.Window != 16000 {
		t.Fatal("catalog replaced recorded window")
	}
}

func TestInspectionReferencesRemainUnresolved(t *testing.T) {
	st, dir := inspectionStore(t)
	s, rec := savedSession(t, st, dir, "referenced evidence")
	sum := sha256.Sum256([]byte("ATTACHED_BYTES_NEEDLE"))
	ref := content.Ref{ID: "ATTACHMENT_REF_NEEDLE", SHA256: fmt.Sprintf("%x", sum), Size: 21}
	client := &inspectionClient{results: []llm.Result{reply("unexpected", "USD")}}
	start := dashi.Say("Public attachment request")
	start.Agent = dashi.Agent{ID: "fixture", Provider: "test", API: "test.chat", Model: "recorded-model"}
	start.Input[0].Content = append(start.Input[0].Content, llm.Document(content.Source{Ref: &ref, MIMEType: "application/pdf"}, "ATTACHMENT_TITLE_NEEDLE.pdf"))
	out, err := (&dashi.Runner{Client: client, Recorder: rec}).Run(t.Context(), dashi.NewTurn(session.TurnID(s.ID, 1)), start)
	if err != nil || out.Stop.Reason != dashi.StopReasonContentNoLoader || client.sent != 0 {
		t.Fatalf("unresolved fixture stop=%+v err=%v", out.Stop, err)
	}
	before := storeImage(t, st.root)
	for _, query := range []string{"ATTACHMENT_REF_NEEDLE", "ATTACHMENT_TITLE_NEEDLE", "ATTACHED_BYTES_NEEDLE"} {
		if r, err := st.Search(t.Context(), dir, query); err != nil || len(r.Matches) != 0 {
			t.Fatalf("resolved reference %q %+v %v", query, r, err)
		}
	}
	if r, err := st.Search(t.Context(), dir, "Public attachment request"); err != nil || len(r.Matches) != 1 {
		t.Fatalf("public text %+v %v", r, err)
	}
	if after := storeImage(t, st.root); !reflect.DeepEqual(before, after) {
		t.Fatal("reference inspection changed files")
	}
	// A newer attachment-only request must not inherit older request prose.
	start.Input[0].Content = []llm.Content{llm.Document(content.Source{Ref: &ref, MIMEType: "application/pdf"}, "evidence.pdf")}
	second, err := (&dashi.Runner{Client: client, Recorder: rec}).Run(t.Context(), dashi.NewTurn(session.TurnID(s.ID, 2)), start)
	if err != nil || second.Stop.Reason != dashi.StopReasonContentNoLoader {
		t.Fatalf("attachment-only fixture: %v", err)
	}
	report, err := st.Inspect(t.Context(), dir, s.ID)
	if err != nil || report.Request == nil || report.Request.TurnID != session.TurnID(s.ID, 2) || report.Request.Text != "[request has no public text]" || report.RequestIncomplete {
		t.Fatalf("attachment-only preview %+v %v", report.Request, err)
	}

}

func TestInspectionIncompleteAndUnresolvedContextNeverUsesOlderCall(t *testing.T) {
	st, dir := inspectionStore(t)
	s, rec := savedSession(t, st, dir, "partial")
	client := &inspectionClient{results: []llm.Result{reply("answer", "USD")}, window: 2000}
	out := runInspectionTurn(t, rec, session.TurnID(s.ID, 1), client, dashi.Start{Input: dashi.Say("request").Input}, nil)
	// Crash snapshot: Started, first intent, settled response, continuation,
	// second intent. No fake model settlement or run stop is introduced.
	for _, step := range out.Turn.Steps {
		if step.Kind == dashi.StepModelCallFinished {
			step.Model.EstimatedUsage = true
			c := contextOccupancy(dashi.Turn{ID: out.Turn.ID, Steps: []dashi.Step{out.Turn.Steps[0], out.Turn.Steps[1], step}}, out.TurnView)
			if c.Available || !c.Estimated {
				t.Fatal("partial usage presented as exact context")
			}
			step.Model.EstimatedUsage = false
		}
	}
	turn := dashi.NewTurn("fixture")
	var intent dashi.ModelFact
	for _, step := range out.Turn.Steps {
		if step.Kind == dashi.StepModelCallStarted {
			intent = *step.Model
			break
		}
	}
	intent.AttemptID = "new-unresolved"
	turn.Steps = []dashi.Step{{Kind: dashi.StepModelCallStarted, Model: &intent}}
	c := contextOccupancy(turn, out.TurnView)
	if c.Available || c.AttemptID != "new-unresolved" || c.UnavailableReason != "model intent unresolved" {
		t.Fatalf("unresolved context=%+v", c)
	}
	unresolved := turnAccounting(turn, dashi.TurnView{})
	if unresolved.ModelCalls != 0 || unresolved.UnresolvedModelIntents != 1 {
		t.Fatalf("unresolved calls=%+v", unresolved)
	}
}
