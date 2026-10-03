// Command example gives an assistant 20 saved notes about a user and their
// projects, and lets a decision model choose which ones each turn needs.
// For each message it prints the notes a keyword search ranks highest
// beside the notes Decide chose, so the two can be compared.
//
// By default it needs no API key: a scripted dashitest client stands in for
// the agent's model, and a decidetest server stands in for Jev, answering
// with keyword rules. With -live it uses Claude (ANTHROPIC_API_KEY; set
// ANTHROPIC_MODEL to override the catalog's default model) and Jev
// (TYPESAFE_API_KEY).
//
//	go run ./contextrank/example
//	go run ./contextrank/example -live
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/contextrank"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

// messagesToSend are the user's two messages. The first needs three notes:
// who the manager is, how the user writes updates, and the new launch
// date. The second is a follow-up that needs nothing new.
var messagesToSend = []string{
	"Draft this week's status update on Heron for my manager.",
	"Make it shorter.",
}

// notes are the user's saved notes, by ID.
var notes = []contextrank.Candidate{
	{ID: "heron-status-page", Content: "Heron's public status page is status.heron.dev; update it during incidents."},
	{ID: "kestrel-status", Content: "Kestrel's weekly status update goes out on Fridays; Priya writes it."},
	{ID: "laptop-update", Content: "Maya's laptop needs an OS update this week."},
	{ID: "heron-repo", Content: "Heron's code lives in github.com/acme/heron; main is protected."},
	{ID: "heron-port", Content: "Heron's staging server runs on port 8443."},
	{ID: "heron-mascot", Content: "The Heron team's mascot is a grey heron named Gus."},
	{ID: "heron-launch", Content: "The Heron launch moved from Oct 7 to Oct 14 because the payments review slipped."},
	{ID: "manager", Content: "Maya reports to Dana Ruiz, VP of Engineering, who reads email, not Slack."},
	{ID: "report-style", Content: "Maya writes reports to leadership as three bullets (done, next, risks), under 120 words."},
	{ID: "one-on-one", Content: "Maya meets Sam, her direct report, on Thursdays at 2pm."},
	{ID: "coffee", Content: "Maya takes her coffee black."},
	{ID: "dog", Content: "Maya's dog is a corgi named Biscuit."},
	{ID: "vacation", Content: "Maya is on vacation Oct 20 to 24."},
	{ID: "kestrel-budget", Content: "Kestrel's Q4 budget is $40,000, approved by finance."},
	{ID: "seat", Content: "Maya prefers an aisle seat on flights."},
	{ID: "timezone", Content: "Maya works from Lisbon, in Western European Time."},
	{ID: "heron-oncall", Content: "Heron's on-call rotation changes every Monday at 9am."},
	{ID: "hiring", Content: "Maya is hiring a senior backend engineer for Heron."},
	{ID: "allergy", Content: "Maya is allergic to peanuts."},
	{ID: "gym", Content: "Maya goes to the gym on Tuesday and Friday mornings."},
}

// stubRules stand in for Jev in the keyless run. They are keyword rules on
// a note's text, not model judgments: the first rule whose phrase the note
// holds gives its P(yes); any other note gets 0.04.
var stubRules = []struct {
	phrase string
	p      float64
}{
	{"Dana Ruiz", 0.93},
	{"three bullets", 0.88},
	{"launch moved", 0.81},
	{"Heron", 0.31},
	{"status", 0.22},
}

// replies are what a model would write in the keyless run.
var replies = []string{
	"Hi Dana, here is this week's Heron update.\n" +
		"- Done: payments integration is code-complete.\n" +
		"- Next: launch on Oct 14, after the payments review.\n" +
		"- Risks: the review slipped once already.",
	"Hi Dana: Heron payments are done; launch is Oct 14; the risk is the payments review.",
}

func main() {
	log.SetFlags(0)
	live := flag.Bool("live", false, "use Claude and Jev instead of scripted stand-ins")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *live); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, live bool) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var model llm.Client
	var judge *decide.Client
	name := os.Getenv("ANTHROPIC_MODEL")
	if live {
		var err error
		if model, err = messages.New(messages.Config{}); err != nil {
			return err
		}
		if judge, err = decide.NewClient(); err != nil { // reads TYPESAFE_API_KEY
			return err
		}
	} else {
		srv, client, err := stubJudge()
		if err != nil {
			return err
		}
		defer srv.Close()
		scripted := dashitest.NewClient()
		for _, r := range replies {
			scripted.Add(dashitest.Reply(r))
		}
		model, judge, name = scripted, client, dashitest.Model
	}

	var last contextrank.Selection
	var exchange string
	ranker := &contextrank.Ranker{
		Client: judge,
		Source: func(ctx context.Context, x string) ([]contextrank.Candidate, error) {
			exchange = x
			return keywordSearch(ctx, x)
		},
		MaxBytes:    640, // about three notes, to make the choice visible
		OnSelection: func(s contextrank.Selection) { last = s },
	}
	s := &session.Session{
		Runner: dashi.Runner{Client: model, Hooks: ranker.Hook().Hooks()},
		Agent: dashi.Agent{
			Model: name,
			Instructions: "You are Maya's assistant. Saved notes about Maya may " +
				"appear as context items; use the ones that help. Be brief.",
		},
	}
	for _, text := range messagesToSend {
		last = contextrank.Selection{}
		fmt.Println("> " + text)
		out, err := s.Send(ctx, text)
		if err != nil {
			return err
		}
		printSelection(exchange, last)
		for _, w := range out.Warnings {
			fmt.Printf("warning: %s: %s\n", w.Code, w.Message)
		}
		fmt.Printf("\n%s\n\n", out.Reply)
	}
	return nil
}

// printSelection prints the keyword search's top notes beside Decide's
// choice. The search gets as many notes as Decide chose, so the two use
// about the same budget.
func printSelection(exchange string, sel contextrank.Selection) {
	if sel.Err != nil {
		fmt.Println("ranking failed:", sel.Err)
		return
	}
	var chosen []contextrank.Judged
	present, below := 0, 0
	for _, j := range sel.Judged {
		switch j.Cause {
		case contextrank.CauseChosen:
			chosen = append(chosen, j)
		case contextrank.CausePresent:
			present++
		case contextrank.CauseBelowMinimum:
			below++
		}
	}
	if len(chosen) == 0 {
		fmt.Printf("Decide: nothing new (%d already present, %d below the floor)\n",
			present, below)
		return
	}
	slices.SortStableFunc(chosen, byProbability)
	top, _ := keywordSearch(context.Background(), exchange)
	fmt.Printf("\n  %-34s %s\n", "keyword search, top "+fmt.Sprint(len(chosen)),
		"Decide ("+sel.Model+"), chosen")
	for i, j := range chosen {
		fmt.Printf("  %-34s %.2f %s\n", top[i].ID, j.Probability, j.ID)
	}
	// The best notes left out show how close the floor was.
	var rest []contextrank.Judged
	for _, j := range sel.Judged {
		if j.Cause == contextrank.CauseBelowMinimum || j.Cause == contextrank.CauseOverBudget {
			rest = append(rest, j)
		}
	}
	slices.SortStableFunc(rest, byProbability)
	var next []string
	for _, j := range rest[:min(3, len(rest))] {
		next = append(next, fmt.Sprintf("%s %.2f (%s)", j.ID, j.Probability, j.Cause))
	}
	fmt.Printf("  next best: %s\n", strings.Join(next, ", "))
	fmt.Printf("  (%d notes judged in %d request(s), %d input tokens)\n",
		len(sel.Judged)-present, len(sel.RequestIDs), sel.Usage.InputTokens)
}

// byProbability orders judged notes by P(yes), highest first.
func byProbability(a, b contextrank.Judged) int {
	return cmp.Compare(b.Probability, a.Probability)
}

// keywordSearch is the host's cheap measure: notes ordered by how many
// words they share with the exchange, then by their saved order.
func keywordSearch(_ context.Context, exchange string) ([]contextrank.Candidate, error) {
	query := words(exchange)
	score := func(c contextrank.Candidate) int {
		n := 0
		for w := range words(c.Content) {
			if query[w] {
				n++
			}
		}
		return n
	}
	out := slices.Clone(notes)
	slices.SortStableFunc(out, func(a, b contextrank.Candidate) int {
		return score(b) - score(a)
	})
	return out, nil
}

var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "this": true, "that": true,
	"her": true, "who": true, "not": true, "are": true, "with": true,
}

// words returns the lowercase words of three or more letters in text,
// without a trailing "'s".
func words(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && r != '\''
	}) {
		w = strings.TrimSuffix(strings.Trim(w, "'"), "'s")
		if len(w) >= 3 && !stopWords[w] {
			out[w] = true
		}
	}
	return out
}

// stubJudge starts a fake Decide server that answers with stubRules.
func stubJudge() (*decidetest.Server, *decide.Client, error) {
	srv, err := decidetest.Start()
	if err != nil {
		return nil, nil, err
	}
	srv.Respond(func(req *decide.Request) (*decide.Response, error) {
		resp := &decide.Response{
			Answers: map[string]decide.Answer{},
			Usage:   decide.Usage{InputTokens: 2400, OutputTokens: 2 * len(req.Questions)}, // a stand-in
		}
		for key, q := range req.Questions {
			text, _ := q.(*decide.NoulQuestion).Instructions.(string)
			_, note, _ := strings.Cut(text, "The item:\n")
			p := 0.04
			for _, r := range stubRules {
				if strings.Contains(note, r.phrase) {
					p = r.p
					break
				}
			}
			resp.Answers[key] = decidetest.NoulAnswer(p)
		}
		return resp, nil
	})
	client, err := srv.NewClient()
	if err != nil {
		srv.Close()
		return nil, nil, err
	}
	return srv, client, nil
}
