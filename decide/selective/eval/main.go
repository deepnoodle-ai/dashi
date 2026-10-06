// Command eval runs the needle-in-history comparison of the selective
// compaction design (design/decide/selective-compaction.md).
//
// Each session plants exact details early: an error code, a path, a
// decision and its reason, and a constraint the user stated once. Then it
// is compacted five ways to the same size, and a probe turn asks one
// question per needle:
//
//  1. stock: compaction.Compactor, tail 0.25 of the window.
//  2. stock, request-aware: the same, with the latest request added to the
//     summary prompt. The stock summarizer never sees the latest request,
//     because it sits in the tail; Decide does, so this arm tests whether
//     that alone explains selective's lead.
//  3. recency: selective's layout, pinning, and prompt, with every older
//     segment scored by position, so the newest are kept. Its scores come
//     from an in-process fake, not from Decide.
//  4. selective: selective.Compactor with Decide.
//  5. selective, summary first: the same compaction with the summary moved
//     before the kept older segments.
//
// By default it needs no API key: a decidetest server answers with canned
// scores (a segment holding a needle is needed, any other is not), and a
// scripted model writes a fixed summary and answers each probe with the
// needles found word for word in its history. That checks the plumbing:
// needles survive, tool calls keep their results, fallbacks work, and the
// recorded compaction turn rebuilds the same history. It proves nothing
// about Decide's judgment.
//
// With -live it uses Claude (ANTHROPIC_API_KEY) for the summaries and the
// probes and Jev (TYPESAFE_API_KEY) for selective, scores replies by exact
// match on the planted strings, and reports Decide and summary spend,
// Decide wait, and fallbacks against the design's bar. With OPENAI_API_KEY
// set it also sends one selective history to OpenAI.
//
// Two variants change the generated sessions. -vague replaces the latest
// request, which names the subjects of the needles, with "Keep going.".
// -long-turn reads a large file after the latest request, so the last turn
// is larger than selective's tail share but fits stock's; the keyless run
// then checks that the file survives word for word.
//
//	go run ./decide/selective/eval
//	go run ./decide/selective/eval -live -sessions 4
//	go run ./decide/selective/eval -live -vague
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/signal"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/selective"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

// jevPricePerToken is Jev 1.13's input price, $0.042 per million tokens;
// output tokens are free (docs.typesafe.ai/models, read 2026-10-02).
const jevPricePerToken = 0.042 / 1e6

// Arm names.
const (
	armStock        = "stock"
	armStockRequest = "stock, request-aware"
	armRecency      = "recency"
	armSelective    = "selective"
	armSummaryFirst = "selective, summary first"
)

var arms = []string{armStock, armStockRequest, armRecency, armSelective, armSummaryFirst}

type config struct {
	live     bool
	sessions int
	window   int
	model    string
	openai   string
	seed     uint64
	variant  variant
}

func main() {
	log.SetFlags(0)
	var cfg config
	flag.BoolVar(&cfg.live, "live", false, "use Claude and Jev instead of scripted stand-ins")
	flag.IntVar(&cfg.sessions, "sessions", 6, "number of sessions")
	flag.IntVar(&cfg.window, "window", 16_000, "context window in tokens that compaction sizes for")
	flag.StringVar(&cfg.model, "model", "claude-haiku-4-5", "Claude model for summaries and probes (-live)")
	flag.StringVar(&cfg.openai, "openai-model", "gpt-6-luna", "OpenAI model that receives one selective history (-live)")
	seed := flag.Uint64("seed", 1, "session generator seed")
	flag.BoolVar(&cfg.variant.vague, "vague", false, `make the latest request "Keep going." instead of one that names the needles' subjects`)
	flag.BoolVar(&cfg.variant.longTurn, "long-turn", false, "read a large file in the last turn, over selective's tail share")
	flag.Parse()
	cfg.seed = *seed
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if _, err := run(ctx, cfg, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// env is what every session uses.
type env struct {
	cfg    config
	policy compaction.Policy
	model  string
	tools  tool.List
	// client returns the model client for one call. Live, it is the shared
	// Claude client; keyless, a scripted client that replies with reply.
	client  func(reply string) llm.Client
	judge   *decide.Client // selective's Decide
	recency *decide.Client // scores by position
	broken  *decide.Client // a Decide that cannot be reached (keyless)
}

// outcome is one arm's result for one session.
type outcome struct {
	found       map[string]bool // needle answer → found in the probe reply
	fallback    string
	summaryIn   int
	summaryOut  int
	summaryCost float64
	decide      decide.Usage
	wait        time.Duration
	probeCost   float64
	models      []string // resolved decision models
	kept        int      // estimated tokens of the replacement history without its summary
	actions     map[selective.Action]int
}

// report is the whole run.
type report struct {
	sessions []string
	sizes    []int                // estimated tokens of each session before compaction
	results  map[string][]outcome // arm → one per session
	needles  [][]needle
	problems []string
	provider string
}

func run(ctx context.Context, cfg config, out io.Writer) (*report, error) {
	e, cleanup, err := newEnv(cfg)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	r := &report{results: map[string][]outcome{}}
	sessions := make([]session, cfg.sessions)
	for i := range sessions {
		sessions[i] = newSession(i, cfg.seed, cfg.variant)
		r.sessions = append(r.sessions, sessions[i].name)
		r.sizes = append(r.sizes, keptTokens(sessions[i].messages, &dashi.CompactionFact{SummaryAt: -1}))
		r.needles = append(r.needles, sessions[i].needles)
	}
	for _, a := range arms {
		r.results[a] = make([]outcome, len(sessions))
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	var firstErr error
	for i, s := range sessions {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			res, problems, err := e.session(ctx, i, s)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("session %s: %w", s.name, err)
			}
			for a, o := range res {
				r.results[a][i] = o
			}
			for _, p := range problems {
				r.problems = append(r.problems, s.name+": "+p)
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if cfg.live && os.Getenv("OPENAI_API_KEY") != "" {
		r.provider = e.sendToOpenAI(ctx, sessions[0])
	}
	r.print(out, cfg)
	if len(r.problems) > 0 {
		return r, fmt.Errorf("%d plumbing checks failed", len(r.problems))
	}
	return r, nil
}

func newEnv(cfg config) (*env, func(), error) {
	e := &env{cfg: cfg, tools: evalTools()}
	e.policy.Model.Capabilities.ContextWindow = cfg.window
	recency, err := decidetest.Start()
	if err != nil {
		return nil, nil, err
	}
	recency.Respond(byPosition)
	closers := []func(){recency.Close}
	cleanup := func() {
		for _, c := range closers {
			c()
		}
	}
	if e.recency, err = recency.NewClient(decide.WithMaxRetries(0)); err != nil {
		cleanup()
		return nil, nil, err
	}
	if cfg.live {
		claude, err := messages.New(messages.Config{})
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		e.model = cfg.model
		e.client = func(string) llm.Client { return claude }
		if e.judge, err = decide.NewClient(); err != nil { // reads TYPESAFE_API_KEY
			cleanup()
			return nil, nil, err
		}
		return e, cleanup, nil
	}
	e.model = dashitest.Model
	e.client = func(reply string) llm.Client { return dashitest.NewClient(dashitest.Reply(reply)) }
	judge, err := decidetest.Start()
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	closers = append(closers, judge.Close)
	judge.Respond(cannedNeedles)
	if e.judge, err = judge.NewClient(decide.WithMaxRetries(0)); err != nil {
		cleanup()
		return nil, nil, err
	}
	broken, err := decidetest.Start()
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	brokenURL := broken.URL
	broken.Close() // nothing listens there now
	if e.broken, err = decide.NewClient(decide.WithoutEnvironment(), decide.WithAPIKey("test-key-00000000"),
		decide.WithBaseURL(brokenURL), decide.WithMaxRetries(0)); err != nil {
		cleanup()
		return nil, nil, err
	}
	return e, cleanup, nil
}

var segmentKey = regexp.MustCompile(`^s(\d+)\.(needed|verbatim)$`)

// byPosition scores every older segment by its position, newer higher and
// all above 0.5, and always prefers the whole text. Through compact.Select
// that keeps the newest segments that fit: the recency arm.
func byPosition(req *decide.Request) (*decide.Response, error) {
	resp := &decide.Response{Answers: map[string]decide.Answer{}}
	for key := range req.Questions {
		m := segmentKey.FindStringSubmatch(key)
		if m == nil {
			return nil, fmt.Errorf("unexpected question key %q", key)
		}
		n, _ := strconv.Atoi(m[1])
		p := 1.0
		if m[2] == "needed" {
			p = 1 - 0.4/float64(n+1)
		}
		resp.Answers[key] = decidetest.NoulAnswer(p)
	}
	return resp, nil
}

// cannedNeedles stands in for Jev in the keyless run: a segment that holds
// a needle's answer is needed word for word; any other is not needed.
// These are fixed rules, not model judgments.
func cannedNeedles(req *decide.Request) (*decide.Response, error) {
	resp := &decide.Response{Answers: map[string]decide.Answer{}, Usage: decide.Usage{InputTokens: 100}}
	for key, q := range req.Questions {
		b, _ := json.Marshal(q)
		hit := false
		for _, th := range themes {
			for _, n := range []needle{th.errNeedle, th.pathNeedle, th.decNeedle} {
				hit = hit || strings.Contains(string(b), n.Answer)
			}
		}
		p := 0.1
		if hit {
			p = 0.9
		}
		resp.Answers[key] = decidetest.NoulAnswer(p)
	}
	return resp, nil
}

// session compacts s with every arm and probes each history.
func (e *env) session(ctx context.Context, n int, s session) (map[string]outcome, []string, error) {
	stock := compaction.Compactor{Policy: e.policy, Model: e.model}
	res := map[string]outcome{}
	var problems []string
	check := func(arm string, err error) {
		if err != nil {
			problems = append(problems, arm+": "+err.Error())
		}
	}
	id := func(arm, what string) string {
		return fmt.Sprintf("s%d-%s-%s", n, strings.ReplaceAll(arm, " ", ""), what)
	}

	// Stock, and stock with the latest request in its summary prompt.
	aware := stock
	aware.Prompt = compaction.DefaultPrompt + requestNote(s.latest)
	for arm, c := range map[string]compaction.Compactor{armStock: stock, armStockRequest: aware} {
		st, err := c.Start(ctx, s.messages)
		if err != nil {
			return nil, nil, err
		}
		hist, spend, err := e.compact(ctx, id(arm, "compact"), st, s.messages)
		if err != nil {
			return nil, nil, err
		}
		o := outcome{kept: keptTokens(hist, st.Compaction)}
		o.addSpend(spend)
		if o.found, o.probeCost, err = e.probe(ctx, id(arm, "probe"), hist, s.needles); err != nil {
			return nil, nil, err
		}
		res[arm] = o
		if !e.cfg.live && e.cfg.variant.longTurn {
			check(arm, keepsWhole(hist, s.lastRead))
		}
	}

	// Recency and selective share everything but the Decide client.
	for _, arm := range []string{armRecency, armSelective} {
		judge := e.recency
		if arm == armSelective {
			judge = e.judge
		}
		var sel selective.Selection
		c := &selective.Compactor{
			Base:        stock,
			Client:      judge,
			OnSelection: func(s selective.Selection) { sel = s },
		}
		began := time.Now()
		st, err := c.Start(ctx, s.messages)
		wait := time.Since(began)
		if err != nil {
			return nil, nil, err
		}
		o := outcome{fallback: sel.Fallback, wait: wait, actions: map[selective.Action]int{}}
		if arm == armSelective {
			o.decide, o.models = sel.Usage, sel.Models
		}
		for _, seg := range sel.Segments {
			o.actions[seg.Action]++
		}
		if !e.cfg.live && sel.Fallback != "" {
			check(arm, fmt.Errorf("unexpected fallback: %s", sel.Fallback))
		}
		hist, spend, err := e.compact(ctx, id(arm, "compact"), st, s.messages)
		if err != nil {
			return nil, nil, err
		}
		o.addSpend(spend)
		o.kept = keptTokens(hist, st.Compaction)
		if o.found, o.probeCost, err = e.probe(ctx, id(arm, "probe"), hist, s.needles); err != nil {
			return nil, nil, err
		}
		res[arm] = o
		if !e.cfg.live && arm == armSelective {
			check(arm, survives(hist, s.needles))
		}
		if !e.cfg.live && e.cfg.variant.longTurn {
			check(arm, keepsWhole(hist, s.lastRead))
		}
		if arm != armSelective {
			continue
		}
		first := o
		if sel.Fallback == "" {
			first.found, first.probeCost, err = e.probe(ctx, id(armSummaryFirst, "probe"),
				summaryFirst(hist, st.Compaction), s.needles)
			if err != nil {
				return nil, nil, err
			}
		}
		res[armSummaryFirst] = first
	}

	if !e.cfg.live {
		check("fallback", e.checkFallback(ctx, stock, s.messages))
	}
	return res, problems, nil
}

func (o *outcome) addSpend(s dashi.Spend) {
	o.summaryIn = s.Usage.TotalInputTokens()
	o.summaryOut = s.Usage.OutputTokens
	if usd, ok := s.Cost["USD"]; ok {
		o.summaryCost = usd.Float64()
	}
}

// compact runs the compaction turn of st and returns its replacement
// history. It checks that the record rebuilds the same history after a
// JSON round trip, and that every tool call keeps its results.
func (e *env) compact(ctx context.Context, id string, st dashi.Start, input []llm.Message) ([]llm.Message, dashi.Spend, error) {
	runner := dashi.Runner{
		Client:   e.client("<summary>Scripted notes: the work continues.</summary>"),
		Recorder: dashi.NewMemoryRecorder(),
	}
	st.CommandID = "compact:" + id
	out, err := runner.Run(ctx, dashi.NewTurn(id), st)
	if err != nil {
		return nil, dashi.Spend{}, err
	}
	hist, ok := out.TurnView.ReplacementHistory()
	if !ok {
		return nil, dashi.Spend{}, fmt.Errorf("compaction turn %s ended %s without a summary", id, out.Stop.Kind)
	}
	b, err := json.Marshal(out.Turn)
	if err != nil {
		return nil, dashi.Spend{}, err
	}
	var stored dashi.Turn
	if err := json.Unmarshal(b, &stored); err != nil {
		return nil, dashi.Spend{}, err
	}
	rebuilt, warnings := dashi.BuildHistory([]dashi.Turn{stored})
	a, _ := json.Marshal(hist)
	c, _ := json.Marshal(rebuilt)
	if len(warnings) > 0 || string(a) != string(c) {
		return nil, dashi.Spend{}, fmt.Errorf("compaction turn %s does not rebuild its history", id)
	}
	if err := pairing(input, hist); err != nil {
		return nil, dashi.Spend{}, fmt.Errorf("compaction turn %s: %w", id, err)
	}
	return hist, out.TurnView.Spend, nil
}

const probeIntro = "Answer from our conversation so far, without calling tools. " +
	"Write one line per question, quoting exact strings (error codes, paths, names) " +
	"word for word. If you do not know, write \"unknown\".\n\n"

// probe asks every needle's question after hist and reports which answers
// the reply contains.
func (e *env) probe(ctx context.Context, id string, hist []llm.Message, needles []needle) (map[string]bool, float64, error) {
	var q strings.Builder
	q.WriteString(probeIntro)
	for i, n := range needles {
		fmt.Fprintf(&q, "%d. %s\n", i+1, n.Question)
	}
	// The scripted model recalls exactly what is in its context.
	text := historyText(hist)
	var scripted []string
	for i, n := range needles {
		answer := "unknown"
		if strings.Contains(text, n.Answer) {
			answer = n.Answer
		}
		scripted = append(scripted, fmt.Sprintf("%d. %s", i+1, answer))
	}
	runner := dashi.Runner{
		Client:   e.client(strings.Join(scripted, "\n")),
		Recorder: dashi.NewMemoryRecorder(),
		Tools:    e.tools,
	}
	out, err := runner.Run(ctx, dashi.NewTurn(id), dashi.Start{
		CommandID: "run:" + id,
		Agent:     dashi.Agent{Model: e.model, Settings: llm.Settings{MaxTokens: 600}},
		History:   hist,
		Input:     []llm.Message{llm.UserMessage(llm.Text(q.String()))},
	})
	if err != nil {
		return nil, 0, err
	}
	if out.Stop.Kind != dashi.StopCompleted {
		return nil, 0, fmt.Errorf("probe %s stopped %s: %s", id, out.Stop.Kind, out.Stop.Reason)
	}
	reply := strings.ToLower(out.Reply)
	found := map[string]bool{}
	for _, n := range needles {
		found[n.Answer] = strings.Contains(reply, strings.ToLower(n.Answer))
	}
	return found, out.TurnView.Spend.Cost["USD"].Float64(), nil
}

// keptTokens estimates the tokens of hist without its summary, as package
// compaction does: encoded bytes over four.
func keptTokens(hist []llm.Message, f *dashi.CompactionFact) int {
	n := 0
	for i, m := range hist {
		if i != f.SummaryAt {
			b, _ := json.Marshal(m)
			n += len(b)/4 + 1
		}
	}
	return n
}

// summaryFirst moves the summary of hist, at f.SummaryAt, to just after the
// leading system and developer messages.
func summaryFirst(hist []llm.Message, f *dashi.CompactionFact) []llm.Message {
	at := 0
	for at < len(hist) && (hist[at].Role == llm.RoleSystem || hist[at].Role == llm.RoleDeveloper) {
		at++
	}
	out := slices.Clone(hist)
	summary := out[f.SummaryAt]
	out = slices.Delete(out, f.SummaryAt, f.SummaryAt+1)
	return slices.Insert(out, at, summary)
}

// checkFallback compacts with a Decide that cannot be reached and checks
// that the stock Start comes back with the cause in Selection.Fallback.
func (e *env) checkFallback(ctx context.Context, stock compaction.Compactor, msgs []llm.Message) error {
	var sel selective.Selection
	c := &selective.Compactor{Base: stock, Client: e.broken, OnSelection: func(s selective.Selection) { sel = s }}
	got, err := c.Start(ctx, msgs)
	if err != nil {
		return err
	}
	want, err := stock.Start(ctx, msgs)
	if err != nil {
		return err
	}
	if sel.Fallback == "" || !reflect.DeepEqual(got, want) {
		return errors.New("an unreachable Decide did not fall back to the stock Start")
	}
	return nil
}

// requestNote tells the stock summarizer the latest request, which it does
// not otherwise see, because the request sits in the kept tail.
func requestNote(latest string) string {
	return "\n\nThe user's latest request, kept word for word after your summary, is:\n" +
		"<latest_request>\n" + latest + "\n</latest_request>\n" +
		"Make sure the summary keeps every exact detail that request may need."
}

// keepsWhole checks that a tool result in hist holds text whole.
func keepsWhole(hist []llm.Message, text string) error {
	for _, m := range hist {
		for _, c := range m.Content {
			if c.ToolResult == nil {
				continue
			}
			for _, b := range c.ToolResult.Output.Content {
				if b.Text != nil && b.Text.Text == text {
					return nil
				}
			}
		}
	}
	return errors.New("the file read in the last turn is not kept word for word")
}

// survives checks that every needle is word for word in hist.
func survives(hist []llm.Message, needles []needle) error {
	text := historyText(hist)
	for _, n := range needles {
		if !strings.Contains(text, n.Answer) {
			return fmt.Errorf("needle %q (%s) did not survive", n.Answer, n.Kind)
		}
	}
	return nil
}

// pairing checks that every tool result in hist follows its call, and that
// every call answered in input is answered in hist.
func pairing(input, hist []llm.Message) error {
	answered := map[string]bool{}
	for _, m := range input {
		for _, c := range m.Content {
			if c.ToolResult != nil {
				answered[c.ToolResult.CallID] = true
			}
		}
	}
	calls, results := map[string]bool{}, map[string]bool{}
	for _, m := range hist {
		for _, c := range m.Content {
			switch {
			case c.ToolCall != nil:
				calls[c.ToolCall.ID] = true
			case c.ToolResult != nil:
				if !calls[c.ToolResult.CallID] {
					return fmt.Errorf("result %s comes before its call", c.ToolResult.CallID)
				}
				results[c.ToolResult.CallID] = true
			}
		}
	}
	for id := range calls {
		if answered[id] && !results[id] {
			return fmt.Errorf("call %s lost its result", id)
		}
	}
	return nil
}

// historyText is every text the model sees in hist.
func historyText(hist []llm.Message) string {
	var b strings.Builder
	for _, m := range hist {
		for _, c := range m.Content {
			switch {
			case c.Block != nil && c.Block.Text != nil:
				b.WriteString(c.Block.Text.Text)
			case c.ToolResult != nil:
				for _, bl := range c.ToolResult.Output.Content {
					if bl.Text != nil {
						b.WriteString(bl.Text.Text)
					}
				}
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// toolArgs covers the arguments of every session tool.
type toolArgs struct {
	Path    string `json:"path,omitempty"`
	N       int    `json:"n,omitempty"`
	Pkg     string `json:"pkg,omitempty"`
	Pattern string `json:"pattern,omitempty"`
	Service string `json:"service,omitempty"`
	Q       string `json:"q,omitempty"`
}

// evalTools are the session's tools, so a probe can send history with tool
// calls. They do nothing.
func evalTools() tool.List {
	var list tool.List
	for _, name := range []string{"ls", "read_file", "git_log", "run_tests", "grep", "tail_log", "web_search"} {
		t, err := tool.Func(name, "Not available during this question.",
			func(context.Context, tool.Call, toolArgs) (tool.Outcome, error) {
				return tool.Failed("This tool is not available now. Answer from the conversation."), nil
			})
		if err != nil {
			panic(err)
		}
		list = append(list, t)
	}
	return list
}

// sendToOpenAI sends the first session's selective history to OpenAI, to
// check that a second provider accepts it.
func (e *env) sendToOpenAI(ctx context.Context, s session) string {
	stock := compaction.Compactor{Policy: e.policy, Model: e.model}
	c := &selective.Compactor{Base: stock, Client: e.judge}
	st, err := c.Start(ctx, s.messages)
	if err != nil {
		return "OpenAI: " + err.Error()
	}
	hist, _, err := e.compact(ctx, "openai-compact", st, s.messages)
	if err != nil {
		return "OpenAI: " + err.Error()
	}
	client, err := responses.New(responses.Config{})
	if err != nil {
		return "OpenAI: " + err.Error()
	}
	runner := dashi.Runner{Client: client, Recorder: dashi.NewMemoryRecorder(), Tools: e.tools}
	out, err := runner.Run(ctx, dashi.NewTurn("openai-probe"), dashi.Start{
		CommandID: "run:openai-probe",
		Agent:     dashi.Agent{Model: e.cfg.openai, Settings: llm.Settings{MaxTokens: 300}},
		History:   hist,
		Input:     []llm.Message{llm.UserMessage(llm.Text("In one sentence, what is the task?"))},
	})
	switch {
	case err != nil:
		return "OpenAI " + e.cfg.openai + ": rejected: " + err.Error()
	case out.Stop.Kind != dashi.StopCompleted:
		return fmt.Sprintf("OpenAI %s: stopped %s: %s", e.cfg.openai, out.Stop.Kind, out.Stop.Reason)
	}
	return fmt.Sprintf("OpenAI %s accepted a selective history (%d messages): %.80q", e.cfg.openai, len(hist), out.Reply)
}

func (r *report) print(w io.Writer, cfg config) {
	mode := "keyless (canned Decide answers, scripted model: plumbing only)"
	if cfg.live {
		mode = "live (" + cfg.model + ", Jev)"
	}
	if cfg.variant.vague {
		mode += ", vague latest request"
	}
	if cfg.variant.longTurn {
		mode += ", long last turn"
	}
	fmt.Fprintf(w, "needle in history: %s, %d sessions, window %d tokens\n\n", mode, len(r.sessions), cfg.window)
	fmt.Fprintf(w, "%-26s %-11s %5s %7s %8s %8s %8s %10s %9s %9s\n",
		"arm", "recall", "falls", "kept", "sum in", "sum out", "sum $", "Decide in", "Decide $", "wait max")
	recall := map[string]float64{}
	for _, a := range arms {
		found, total, falls := 0, 0, 0
		var kept, in, out, decideIn int
		var cost float64
		var wait time.Duration
		for i, o := range r.results[a] {
			for _, n := range r.needles[i] {
				total++
				if o.found[n.Answer] {
					found++
				}
			}
			if o.fallback != "" {
				falls++
			}
			kept += o.kept
			in += o.summaryIn
			out += o.summaryOut
			cost += o.summaryCost
			decideIn += o.decide.InputTokens
			wait = max(wait, o.wait)
		}
		recall[a] = 100 * float64(found) / float64(max(total, 1))
		fmt.Fprintf(w, "%-26s %-11s %5d %7d %8d %8d %8.4f %10d %9.6f %9s\n",
			a, fmt.Sprintf("%d/%d %.0f%%", found, total, recall[a]), falls, kept, in, out, cost,
			decideIn, float64(decideIn)*jevPricePerToken, wait.Round(time.Millisecond))
	}
	fmt.Fprintln(w, "kept: estimated tokens of the histories after compaction, without the summaries.")
	total := 0.0
	models := map[string]bool{}
	for _, a := range arms {
		for _, o := range r.results[a] {
			total += o.probeCost
			if a != armSummaryFirst { // it shares selective's summary and Decide calls
				total += o.summaryCost + float64(o.decide.InputTokens)*jevPricePerToken
			}
			for _, m := range o.models {
				models[m] = true
			}
		}
	}
	fmt.Fprintf(w, "total spend: $%.4f (summaries, probes, and Decide; OpenAI not included); decision models: %v\n",
		total, slices.Sorted(maps.Keys(models)))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%-26s", "recall by needle kind")
	kinds := []string{"error", "path", "decision", "constraint"}
	for _, k := range kinds {
		fmt.Fprintf(w, " %10s", k)
	}
	fmt.Fprintln(w)
	for _, a := range arms {
		fmt.Fprintf(w, "%-26s", a)
		for _, k := range kinds {
			found, total := 0, 0
			for i, o := range r.results[a] {
				for _, n := range r.needles[i] {
					if n.Kind == k {
						total++
						if o.found[n.Answer] {
							found++
						}
					}
				}
			}
			fmt.Fprintf(w, " %6d/%-3d", found, total)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
	for i, name := range r.sessions {
		sel := r.results[armSelective][i]
		fmt.Fprintf(w, "%-14s %6d tokens before; selective segments: %d kept, %d short, %d dropped\n",
			name, r.sizes[i], sel.actions[selective.ActionKeep], sel.actions[selective.ActionShort],
			sel.actions[selective.ActionDrop])
	}
	for i, name := range r.sessions {
		for _, a := range []string{armRecency, armSelective} {
			if f := r.results[a][i].fallback; f != "" {
				fmt.Fprintf(w, "fallback: %s, %s: %s\n", name, a, f)
			}
		}
	}
	if r.provider != "" {
		fmt.Fprintln(w, r.provider)
	}
	fmt.Fprintln(w)
	if !cfg.live {
		fmt.Fprintln(w, "The bar applies to -live runs only.")
	} else {
		sel := r.results[armSelective]
		falls, decideCost, summaryCost := 0, 0.0, 0.0
		for _, o := range sel {
			if o.fallback != "" {
				falls++
			}
			decideCost += float64(o.decide.InputTokens) * jevPricePerToken
			summaryCost += o.summaryCost
		}
		verdict := func(ok bool) string {
			if ok {
				return "met on generated sessions"
			}
			return "NOT met"
		}
		for _, a := range []string{armStock, armStockRequest, armRecency} {
			fmt.Fprintf(w, "bar: selective beats %s by 10+ points: %+.0f, %s\n",
				a, recall[armSelective]-recall[a], verdict(recall[armSelective]-recall[a] >= 10))
		}
		rate := 100 * float64(falls) / float64(max(len(sel), 1))
		fmt.Fprintf(w, "bar: fallbacks under 5%%: %.0f%%, %s\n", rate, verdict(rate < 5))
		fmt.Fprintf(w, "bar: Decide spend below summary spend: $%.6f vs $%.4f, %s\n",
			decideCost, summaryCost, verdict(decideCost < summaryCost))
	}
	for _, p := range r.problems {
		fmt.Fprintln(w, "PROBLEM:", p)
	}
}
