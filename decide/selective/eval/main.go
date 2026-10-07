// Command eval runs the needle-in-history comparison of the selective
// compaction design (design/decide/selective-compaction.md).
//
// Each session plants exact details early: an error code, a path, a
// decision and its reason, and a constraint the user stated once. Then it
// is compacted five ways to the same size budget, and a probe turn asks
// one question per needle:
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
// set, -provider-smoke also checks one selective and stock history on OpenAI.
// Paid runs require -max-cost. All dispatches reserve their conservative
// maximum cost, with zero SDK or transport retries. -preflight prepares
// real provider payloads with scripted replies, without paid calls.
// -override-recorded-model explicitly uses -provider and -model instead
// of recorded models. Reports use per-run ordinal case IDs.
//
// Two variants change the generated sessions. -vague replaces the latest
// request, which names the subjects of the needles, with "Keep going.".
// -long-turn reads a large file after the latest request, so the last turn
// is larger than selective's tail share but fits stock's; the keyless run
// then checks that the file survives word for word.
//
// -recorded replaces the generated sessions with recorded dashi CLI
// sessions (folders such as ~/.dashi/sessions). Each turn end of 20,000 to
// 120,000 estimated tokens is one case, compacted for a window that the
// history fills to 80%. Its needles are mined once by -mine-model from the
// part every arm compacts, checked word for word in code, and saved to
// -needles before any arm runs with explicit -live -mine-needles; a later
// run reads that file. Each case is
// summarized and probed by its own recorded model: a case recorded with
// OpenAI needs OPENAI_API_KEY, and a case of another provider stops the
// run.
//
//	go run ./decide/selective/eval
//	go run ./decide/selective/eval -live -max-cost 1 -sessions 4
//	go run ./decide/selective/eval -live -max-cost 1 -vague
//	go run ./decide/selective/eval -live -max-cost 1 -recorded ~/.dashi/sessions -needles needles.json
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
	dashisession "github.com/deepnoodle-ai/dashi/session"
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
	// recorded lists dashi CLI session folders; when set, they replace
	// the generated sessions.
	recorded         []string
	needles          string // file of mined needles for recorded sessions
	mineModel        string // Claude model that mines the needles
	provider         string // explicit provider for generated or overridden recorded cases
	overrideRecorded bool
	preflight        bool
	maxCost          string
	summaryTokens    int
	probeTokens      int
	providerSmoke    bool
	mineNeedles      bool
	reasoningEffort  string
}

func main() {
	log.SetFlags(0)
	var cfg config
	flag.BoolVar(&cfg.live, "live", false, "use paid models and Jev instead of scripted stand-ins")
	flag.IntVar(&cfg.sessions, "sessions", 6, "number of sessions")
	flag.IntVar(&cfg.window, "window", 16_000, "context window in tokens that compaction sizes for")
	flag.StringVar(&cfg.model, "model", "claude-haiku-5-5", "model for summaries and probes (-live)")
	flag.StringVar(&cfg.openai, "openai-model", "gpt-6-luna", "OpenAI model that receives one selective history (-live)")
	seed := flag.Uint64("seed", 1, "session generator seed")
	flag.BoolVar(&cfg.variant.vague, "vague", false, `make the latest request "Keep going." instead of one that names the needles' subjects`)
	flag.BoolVar(&cfg.variant.longTurn, "long-turn", false, "read a large file in the last turn, over selective's tail share")
	recorded := flag.String("recorded", "", "comma-separated dashi CLI session folders, or folders of them, to use instead of generated sessions")
	flag.StringVar(&cfg.needles, "needles", "needles.json", "saved recorded needles; missing ones require -live -mine-needles")
	flag.StringVar(&cfg.mineModel, "mine-model", "claude-sonnet-5", "Claude model that mines needles from recorded sessions")
	flag.StringVar(&cfg.provider, "provider", "anthropic", "provider for -model: anthropic or openai")
	flag.BoolVar(&cfg.overrideRecorded, "override-recorded-model", false, "use -provider and -model for recorded summaries and probes")
	flag.IntVar(&cfg.summaryTokens, "summary-tokens", 0, "summary output limit; zero keeps the compactor default")
	flag.IntVar(&cfg.probeTokens, "probe-tokens", 0, "probe output limit; zero keeps the existing default")
	flag.StringVar(&cfg.maxCost, "max-cost", "", "hard USD reservation limit, required for -live and -preflight")
	flag.BoolVar(&cfg.preflight, "preflight", false, "prepare real provider requests with scripted replies and fake judgment; no paid calls")
	flag.BoolVar(&cfg.providerSmoke, "provider-smoke", false, "also send one selective history to OpenAI (-live, included in -max-cost)")
	flag.BoolVar(&cfg.mineNeedles, "mine-needles", false, "explicitly mine missing recorded needles (-live, included in -max-cost)")
	flag.StringVar(&cfg.reasoningEffort, "reasoning-effort", "", "reasoning effort for summaries and probes; empty keeps provider defaults")
	flag.Parse()
	if *recorded != "" {
		cfg.recorded = strings.Split(*recorded, ",")
	}
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
	// client returns the model client for one call to provider. Live, it
	// is the Claude client, or the OpenAI client for a recorded OpenAI
	// case; keyless, a scripted client that replies with reply.
	client func(provider, reply string) llm.Client
	// checkProvider returns an error when client has no client for
	// provider. Keyless, it is nil: the scripted client stands for any.
	checkProvider func(provider string) error
	judge         *decide.Client // selective's Decide
	recency       *decide.Client // scores by position
	broken        *decide.Client // a Decide that cannot be reached (keyless)
	limit         *spendLimit
}

// outcome is one arm's result for one session.
type outcome struct {
	completed   bool
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
	sessions   []string
	sizes      []int                // estimated tokens of each session before compaction
	results    map[string][]outcome // arm → one per session
	needles    [][]needle
	problems   []string
	provider   string
	spending   *spending
	incomplete bool
}

func run(ctx context.Context, cfg config, out io.Writer) (*report, error) {
	if cfg.provider == "" {
		cfg.provider = "anthropic"
	}
	if cfg.provider != "anthropic" && cfg.provider != "openai" {
		return nil, errors.New("-provider must be anthropic or openai")
	}
	if cfg.live && cfg.preflight {
		return nil, errors.New("choose either -live or -preflight")
	}
	if cfg.summaryTokens < 0 || cfg.probeTokens < 0 {
		return nil, errors.New("output limits cannot be negative")
	}
	e, cleanup, err := newEnv(cfg)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if e.limit != nil {
		e.limit.cancel = cancel
	}
	r := &report{results: map[string][]outcome{}}
	var sessions []session
	if len(cfg.recorded) > 0 {
		if sessions, err = e.recordedWithNeedles(ctx); err != nil {
			return nil, errors.New("recorded cases or saved needles unavailable; private details omitted")
		}
	} else {
		sessions = make([]session, cfg.sessions)
		for i := range sessions {
			sessions[i] = newSession(i, cfg.seed, cfg.variant)
		}
	}
	e.tools = evalTools(toolNames(sessions))
	for _, s := range sessions {
		r.sessions = append(r.sessions, fmt.Sprintf("case-%02d", len(r.sessions)+1))
		r.sizes = append(r.sizes, keptTokens(s.messages, &dashi.CompactionFact{SummaryAt: -1}))
		r.needles = append(r.needles, s.needles)
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
				cancel()
			}
			for a, o := range res {
				r.results[a][i] = o
			}
			for _, p := range problems {
				r.problems = append(r.problems, r.sessions[i]+": "+p)
			}
		})
	}
	wg.Wait()
	if e.limit != nil {
		v := e.limit.snapshot()
		r.spending = &v
	}
	if firstErr != nil {
		r.incomplete = true
		r.print(out, cfg)
		if len(cfg.recorded) > 0 {
			return r, errors.New("recorded evaluation incomplete; no private error details printed")
		}
		return r, firstErr
	}
	if cfg.live && cfg.providerSmoke && os.Getenv("OPENAI_API_KEY") != "" {
		r.provider = e.sendToOpenAI(ctx, sessions[0])
		v := e.limit.snapshot()
		r.spending = &v
	}
	r.print(out, cfg)
	if len(r.problems) > 0 {
		return r, fmt.Errorf("%d plumbing checks failed", len(r.problems))
	}
	return r, nil
}

func newEnv(cfg config) (*env, func(), error) {
	e := &env{cfg: cfg}
	if cfg.live || cfg.preflight {
		var err error
		if e.limit, err = newSpendLimit(cfg.maxCost); err != nil {
			return nil, nil, err
		}
	}
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
	if cfg.live || cfg.preflight {
		claude, err := messages.New(messages.Config{Client: singleDispatchHTTP()})
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		e.model = cfg.model
		var openai llm.Client
		if cfg.preflight || os.Getenv("OPENAI_API_KEY") != "" {
			if openai, err = responses.New(responses.Config{Client: singleDispatchHTTP()}); err != nil {
				cleanup()
				return nil, nil, err
			}
		}
		e.client = func(provider, reply string) llm.Client {
			var client llm.Client = claude
			if provider == "openai" {
				client = openai
			}
			return &limitedClient{Client: client, limit: e.limit, preflight: cfg.preflight, reply: reply}
		}
		e.checkProvider = func(provider string) error { return providerError(provider, openai != nil) }
		if cfg.live {
			transport, err := decide.NewHTTPTransport(decide.HTTPTransportConfig{APIKey: os.Getenv("TYPESAFE_API_KEY"), HTTPClient: singleDispatchHTTP()})
			if err != nil {
				cleanup()
				return nil, nil, err
			}
			if e.judge, err = decide.NewClient(decide.WithoutEnvironment(),
				decide.WithTransport(&limitedDecide{Transport: transport, limit: e.limit}),
				decide.WithModel(decisionModel), decide.WithMaxRetries(0)); err != nil {
				cleanup()
				return nil, nil, err
			}
			return e, cleanup, nil
		}
	}
	if !cfg.preflight {
		e.model = dashitest.Model
		e.client = func(_, reply string) llm.Client { return dashitest.NewClient(dashitest.Reply(reply)) }
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

// providerError returns why a live run cannot send a recorded case of
// provider to its own model, or nil. Empty is a generated session, which
// uses Claude.
func providerError(provider string, haveOpenAI bool) error {
	switch provider {
	case "", "anthropic":
		return nil
	case "openai":
		if !haveOpenAI {
			return errors.New("the case was recorded with OpenAI; set OPENAI_API_KEY")
		}
		return nil
	}
	return fmt.Errorf("the case was recorded with provider %q, which the eval does not support", provider)
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

// cannedNeedles returns an immutable judge for one case. Decoded segment
// text is compared with the case's actual answers, including JSON escapes.
func cannedNeedles(needles []needle) decidetest.Responder {
	needles = slices.Clone(needles)
	return func(req *decide.Request) (*decide.Response, error) {
		resp := &decide.Response{Answers: map[string]decide.Answer{}, Usage: decide.Usage{InputTokens: 100}}
		for key, q := range req.Questions {
			b, err := json.Marshal(q)
			if err != nil {
				return nil, err
			}
			var question struct {
				Instructions struct {
					Segment   string `json:"segment"`
					ShortForm string `json:"short_form"`
				} `json:"instructions"`
			}
			if err := json.Unmarshal(b, &question); err != nil {
				return nil, err
			}
			hit := false
			for _, n := range needles {
				if n.Answer == "" {
					continue
				}
				if strings.Contains(question.Instructions.Segment, n.Answer) {
					hit = hit || !strings.HasSuffix(key, ".verbatim") || !strings.Contains(question.Instructions.ShortForm, n.Answer)
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
}

// session compacts s with every arm and probes each history.
func (e *env) session(ctx context.Context, n int, s session) (map[string]outcome, []string, error) {
	stock := compaction.Compactor{Policy: e.policyFor(s), Model: e.modelFor(s), MaxTokens: e.cfg.summaryTokens}
	res := map[string]outcome{}
	var problems []string
	judgeClient := e.judge
	if !e.cfg.live {
		judge, err := decidetest.Start()
		if err != nil {
			return res, problems, err
		}
		defer judge.Close()
		judge.Respond(cannedNeedles(s.needles))
		if e.cfg.preflight {
			transport, transportErr := decide.NewHTTPTransport(decide.HTTPTransportConfig{APIKey: "test-key-00000000", BaseURL: judge.URL, HTTPClient: singleDispatchHTTP()})
			if transportErr != nil {
				return res, problems, transportErr
			}
			judgeClient, err = decide.NewClient(decide.WithoutEnvironment(), decide.WithTransport(&limitedDecide{Transport: transport, limit: e.limit, preflight: true}), decide.WithMaxRetries(0), decide.WithModel(decisionModel))
		} else {
			judgeClient, err = judge.NewClient(decide.WithMaxRetries(0), decide.WithModel(decisionModel))
		}
		if err != nil {
			return res, problems, err
		}
	}
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
			return res, problems, err
		}
		hist, spend, err := e.compact(ctx, s, id(arm, "compact"), st, s.messages)
		if err != nil {
			return res, problems, err
		}
		o := outcome{completed: true, kept: keptTokens(hist, st.Compaction)}
		o.addSpend(spend)
		if o.found, o.probeCost, err = e.probe(ctx, s, id(arm, "probe"), hist, s.needles); err != nil {
			return res, problems, err
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
			judge = judgeClient
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
			return res, problems, err
		}
		o := outcome{fallback: string(sel.Cause), wait: wait, actions: map[selective.Action]int{}}
		if arm == armSelective {
			o.decide, o.models = sel.Usage, sel.Models
		}
		for _, seg := range sel.Segments {
			o.actions[seg.Action]++
		}
		if !e.cfg.live && sel.Fallback != "" {
			check(arm, errors.New("unexpected selection fallback"))
		}
		hist, spend, err := e.compact(ctx, s, id(arm, "compact"), st, s.messages)
		if err != nil {
			return res, problems, err
		}
		o.addSpend(spend)
		o.kept = keptTokens(hist, st.Compaction)
		if o.found, o.probeCost, err = e.probe(ctx, s, id(arm, "probe"), hist, s.needles); err != nil {
			return res, problems, err
		}
		o.completed = true
		res[arm] = o
		if !e.cfg.live && arm == armSelective {
			needles := s.needles
			if s.model != "" {
				needles = presentNeedles(st.Compaction.Keep, needles)
			}
			check(arm, survives(hist, needles))
		}
		if !e.cfg.live && e.cfg.variant.longTurn {
			check(arm, keepsWhole(hist, s.lastRead))
		}
		if arm != armSelective {
			continue
		}
		first := o
		first.probeCost = 0 // fallback shares the original probe
		if sel.Fallback == "" {
			first.found, first.probeCost, err = e.probe(ctx, s, id(armSummaryFirst, "probe"),
				summaryFirst(hist, st.Compaction), s.needles)
			if err != nil {
				return res, problems, err
			}
		}
		res[armSummaryFirst] = first
	}

	if !e.cfg.live && !e.cfg.preflight {
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
func (e *env) compact(ctx context.Context, s session, id string, st dashi.Start, input []llm.Message) ([]llm.Message, dashi.Spend, error) {
	runner := dashi.Runner{
		Client:   e.client(e.providerFor(s), "<summary>Scripted notes: the work continues.</summary>"),
		Recorder: dashi.NewMemoryRecorder(),
	}
	st.CommandID = "compact:" + id
	st.Agent.Settings = e.settings(st.Agent.Settings)
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
func (e *env) probe(ctx context.Context, s session, id string, hist []llm.Message, needles []needle) (map[string]bool, float64, error) {
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
		Client:   e.client(e.providerFor(s), strings.Join(scripted, "\n")),
		Recorder: dashi.NewMemoryRecorder(),
		Tools:    e.tools,
	}
	out, err := runner.Run(ctx, dashi.NewTurn(id), dashi.Start{
		CommandID: "run:" + id,
		Agent:     dashi.Agent{Model: e.modelFor(s), Settings: e.settings(llm.Settings{MaxTokens: e.probeLimit(s)})},
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

// probeTokens is the probe's output limit. A recorded session's own model
// may reason before it answers, so it gets more room.
func probeTokens(s session) int {
	if s.model != "" {
		return 4000
	}
	return 600
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
	for i, n := range needles {
		if !strings.Contains(text, n.Answer) {
			return fmt.Errorf("needle %d did not survive", i+1)
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
					return errors.New("tool result comes before its call")
				}
				results[c.ToolResult.CallID] = true
			}
		}
	}
	for id := range calls {
		if answered[id] && !results[id] {
			return errors.New("tool call lost its result")
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

// modelFor is the model that summarizes s and answers its probe: the
// recorded session's own model, or -model.
func (e *env) modelFor(s session) string {
	if (e.cfg.live || e.cfg.preflight) && s.model != "" && !e.cfg.overrideRecorded {
		return s.model
	}
	return e.model
}

// providerFor selects the same explicit destination as modelFor.
func (e *env) providerFor(s session) string {
	if s.provider != "" && !e.cfg.overrideRecorded {
		return s.provider
	}
	return e.cfg.provider
}

func (e *env) settings(settings llm.Settings) llm.Settings {
	if e.cfg.reasoningEffort != "" {
		settings.Reasoning = &llm.ReasoningSettings{Effort: e.cfg.reasoningEffort}
	}
	return settings
}

func (e *env) probeLimit(s session) int {
	if e.cfg.probeTokens > 0 {
		return e.cfg.probeTokens
	}
	return probeTokens(s)
}

// presentNeedles identifies details chosen for verbatim or short retention.
// Recorded oracle selection can omit other needles because their segments
// exceed request or history budgets; that is recall evidence, not corruption.
func presentNeedles(hist []llm.Message, needles []needle) []needle {
	text := historyText(hist)
	return slices.DeleteFunc(slices.Clone(needles), func(n needle) bool { return !strings.Contains(text, n.Answer) })
}

// policyFor is the compaction policy of s: its own window, if it has one.
func (e *env) policyFor(s session) compaction.Policy {
	p := e.policy
	if s.window > 0 {
		p.Model.Capabilities.ContextWindow = s.window
	}
	return p
}

// evalTools are the sessions' tools, so a probe can send history with tool
// calls. They do nothing. extra names the tools recorded sessions call.
func evalTools(extra []string) tool.List {
	var list tool.List
	names := []string{"ls", "read_file", "git_log", "run_tests", "grep", "tail_log", "web_search"}
	for _, n := range extra {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	for _, name := range names {
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

// sendToOpenAI sends the first session's selective and stock histories to
// OpenAI, to check that a second provider accepts them.
func (e *env) sendToOpenAI(ctx context.Context, s session) string {
	stock := compaction.Compactor{Policy: e.policyFor(s), Model: e.modelFor(s)}
	var lines []string
	for _, arm := range []string{armSelective, armStock} {
		var c dashisession.Compactor = &stock
		if arm == armSelective {
			c = &selective.Compactor{Base: stock, Client: e.judge}
		}
		lines = append(lines, e.openAIAccepts(ctx, arm, s, c))
	}
	return strings.Join(lines, "\n")
}

// openAIAccepts compacts s with c and sends the history to OpenAI. The
// stock history is sent too, so a rejection shows whether selective
// caused it.
func (e *env) openAIAccepts(ctx context.Context, arm string, s session, c dashisession.Compactor) string {
	prefix := "OpenAI " + e.cfg.openai + ", " + arm + " history: "
	st, err := c.Start(ctx, s.messages)
	if err != nil {
		return prefix + "failed before acceptance check"
	}
	hist, _, err := e.compact(ctx, s, "openai-compact-"+strings.ReplaceAll(arm, " ", ""), st, s.messages)
	if err != nil {
		return prefix + "failed before acceptance check"
	}
	runner := dashi.Runner{Client: e.client("openai", ""), Recorder: dashi.NewMemoryRecorder(), Tools: e.tools}
	id := "openai-probe-" + strings.ReplaceAll(arm, " ", "")
	out, err := runner.Run(ctx, dashi.NewTurn(id), dashi.Start{
		CommandID: "run:" + id,
		Agent:     dashi.Agent{Model: e.cfg.openai, Settings: llm.Settings{MaxTokens: 300}},
		History:   hist,
		Input:     []llm.Message{llm.UserMessage(llm.Text("In one sentence, what is the task?"))},
	})
	switch {
	case err != nil:
		return prefix + "rejected; private error details omitted"
	case out.Stop.Kind != dashi.StopCompleted:
		return fmt.Sprintf("%sstopped %s", prefix, out.Stop.Kind)
	}
	return fmt.Sprintf("%saccepted (%d messages)", prefix, len(hist))
}

func (r *report) print(w io.Writer, cfg config) {
	mode := "keyless (canned Decide answers, scripted model: plumbing only)"
	if cfg.preflight {
		mode = "preflight (real preparation, scripted replies: no paid calls)"
	}
	if cfg.live {
		mode = "live (" + cfg.model + ", Jev)"
		if len(cfg.recorded) > 0 && !cfg.overrideRecorded {
			mode = "live (each session's own model, Jev)"
		}
	}
	if cfg.variant.vague {
		mode += ", vague latest request"
	}
	if cfg.variant.longTurn {
		mode += ", long last turn"
	}
	if len(cfg.recorded) > 0 {
		needles := 0
		for _, n := range r.needles {
			needles += len(n)
		}
		fmt.Fprintf(w, "needle in history: %s, %d recorded CLI cases, %d needles, window per case (history at %.0f%%)\n\n",
			mode, len(r.sessions), needles, 100*caseFill)
	} else {
		fmt.Fprintf(w, "needle in history: %s, %d sessions, window %d tokens\n\n", mode, len(r.sessions), cfg.window)
	}
	if cfg.overrideRecorded || cfg.preflight {
		fmt.Fprintf(w, "configured model: %s/%s; override recorded model: %v; summary limit: %d (0=default); probe limit: %d (0=default); reasoning effort: %q (empty=default)\n", cfg.provider, cfg.model, cfg.overrideRecorded, cfg.summaryTokens, cfg.probeTokens, cfg.reasoningEffort)
	}
	if b := r.spending; b != nil {
		fmt.Fprintf(w, "budget USD: assigned %s; reserved %s; fully reserved %s; observed %s; requests %d; settled %d; unknown charges %d\n", b.cap, b.reserved, b.fullReserved, b.observed, b.requests, b.settled, b.unknown)
		for _, f := range b.failures {
			fmt.Fprintf(w, "dispatch failure: model %s/%s; stage %s; category %s; HTTP %d (0=unavailable); code %q (empty=unavailable or unrecognized)\n", f.provider, f.model, f.stage, f.category, f.status, f.code)
		}
	}
	if r.incomplete {
		fmt.Fprintln(w, "INCOMPLETE: the matrix stopped before every case finished.")
	}

	fmt.Fprintf(w, "%-26s %-11s %5s %7s %8s %8s %8s %10s %9s %9s\n",
		"arm", "recall", "falls", "kept", "sum in", "sum out", "sum $", "Decide in", "Decide $", "wait max")
	recall := map[string]float64{}
	for _, a := range arms {
		found, total, falls, completed := 0, 0, 0, 0
		var kept, in, out, decideIn int
		var cost float64
		var wait time.Duration
		for i, o := range r.results[a] {
			if !o.completed {
				continue
			}
			completed++
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
		fmt.Fprintf(w, "%-26s %-11s %5d %7d %8d %8d %8.4f %10d %9.6f %9s %d/%d cases\n",
			a, fmt.Sprintf("%d/%d %.0f%%", found, total, recall[a]), falls, kept, in, out, cost,
			decideIn, float64(decideIn)*jevPricePerToken, wait.Round(time.Millisecond), completed, len(r.sessions))
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
	if !cfg.live {
		total = 0
	}
	fmt.Fprintf(w, "total spend: $%.4f (matrix summaries, probes, and Decide); decision models: %v\n",
		total, slices.Sorted(maps.Keys(models)))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%-26s", "recall by needle kind")
	kinds := []string{"error", "path", "decision", "constraint"}
	if len(cfg.recorded) > 0 {
		kinds = []string{"error", "path", "name", "value", "decision"}
	}
	for _, k := range kinds {
		fmt.Fprintf(w, " %10s", k)
	}
	fmt.Fprintln(w)
	for _, a := range arms {
		fmt.Fprintf(w, "%-26s", a)
		for _, k := range kinds {
			found, total := 0, 0
			for i, o := range r.results[a] {
				if !o.completed {
					continue
				}
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
	if !cfg.live || r.incomplete {
		fmt.Fprintln(w, "The bar applies to complete -live runs only.")
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
		where := "generated"
		if len(cfg.recorded) > 0 {
			where = "recorded"
		}
		verdict := func(ok bool) string {
			if ok {
				return "met on " + where + " sessions"
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
