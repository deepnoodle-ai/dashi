// Command toolpick narrows a large tool catalog for each turn. An agent
// connected to several MCP servers is offered dozens of tools; every
// definition costs input tokens on every model call, and long lists make
// tool choice worse. A BeforeModel hook (pick.go) asks a decision model,
// through Decide, whether each tool could help with the user's request,
// hides the rest, and hides the same tools on every model call of the turn
// so the cached prompt survives.
//
// By default it needs no API key: a scripted dashitest client stands in for
// the model, and a decidetest server stands in for Jev, answering with
// keyword rules. With -live it uses Claude (ANTHROPIC_API_KEY; set
// ANTHROPIC_MODEL to override the catalog's default model) and Jev
// (TYPESAFE_API_KEY).
//
//	go run ./examples/decide/toolpick
//	go run ./examples/decide/toolpick -live
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

const request = "What's on my calendar tomorrow, and email Sam that I'll be late to the 10am."

// catalog is the agent's tools, as if from several MCP servers. Each has
// string parameters and a canned result; most are never called.
var catalog = []struct{ name, description, params, result string }{
	{"calendar_list_events", "List the events on the user's calendar for one date.", "date",
		"Tomorrow, Oct 3: 10:00 Design review with Sam Lee; 14:00 1:1 with Priya."},
	{"calendar_create_event", "Create a calendar event and invite attendees.", "title,start,end,attendees", ""},
	{"calendar_delete_event", "Delete a calendar event by ID.", "event_id", ""},
	{"calendar_find_free_time", "Find open time slots shared by several people.", "attendees,duration_minutes", ""},
	{"email_send", "Send an email from the user's account.", "to,subject,body", "Sent."},
	{"email_search", "Search the user's mailbox.", "query", ""},
	{"email_archive", "Archive an email thread.", "thread_id", ""},
	{"contacts_search", "Find a person's email address and phone number by name.", "name",
		"Sam Lee <sam.lee@example.com>, +1 555 0199"},
	{"crm_find_account", "Look up a customer account in the CRM.", "company", ""},
	{"crm_update_deal", "Change a deal's stage or amount in the CRM.", "deal_id,stage,amount", ""},
	{"crm_log_call", "Log a sales call against a CRM contact.", "contact_id,notes", ""},
	{"github_search_issues", "Search GitHub issues and pull requests.", "repo,query", ""},
	{"github_create_issue", "Open a GitHub issue.", "repo,title,body", ""},
	{"github_merge_pr", "Merge a GitHub pull request.", "repo,number", ""},
	{"weather_forecast", "Get the weather forecast for a city.", "city,days", ""},
	{"billing_list_invoices", "List a customer's invoices.", "customer_id", ""},
	{"billing_refund", "Refund a payment, in full or in part.", "payment_id,amount", ""},
	{"files_search", "Search the user's cloud drive for documents.", "query", ""},
	{"files_share", "Share a document with someone.", "file_id,email,role", ""},
	{"slack_post_message", "Post a message to a Slack channel.", "channel,text", ""},
	{"slack_search", "Search Slack messages.", "query", ""},
	{"clock_now", "Get the current date, time, and time zone.", "", "2026-10-02 16:40 America/New_York"},
}

// stubRules stand in for Jev in the keyless run. They are keyword rules,
// not model judgments: the first rule whose trigger is in the request and
// whose prefix starts the tool name gives the tool's P(yes); any other
// tool gets 0.04.
var stubRules = []struct {
	trigger, prefix string
	p               float64
}{
	{"calendar", "calendar_list", 0.96},
	{"calendar", "calendar_", 0.31},
	{"email", "email_send", 0.93},
	{"email", "contacts_search", 0.74},
	{"email", "email_", 0.22},
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
		model, judge, name = scriptedModel(), client, dashitest.Model
	}
	fmt.Println("> " + request)
	out, err := runTurn(ctx, model, judge, name)
	if err != nil {
		return err
	}
	fmt.Printf("\n%s\n\n", out.Reply)
	for i, fact := range modelFacts(out.Turn) {
		fmt.Printf("model call %d: offered %d tools, hid %d\n",
			i+1, len(fact.Tools), len(fact.Hidden))
	}
	return nil
}

// runTurn runs one turn over the whole catalog with the picker hook.
func runTurn(ctx context.Context, model llm.Client, judge *decide.Client,
	name string) (dashi.RunResult, error) {
	picker := &Picker{
		Client:    judge,
		Threshold: 0.5, // a placeholder; tune it against real requests
		Keep:      3,
		Always:    []string{"clock_now"},
		Report:    printPick,
	}
	runner := dashi.Runner{Client: model, Tools: catalogTools(), Hooks: picker.Hook().Hooks()}
	return runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{
			Model:        name,
			Instructions: "You are a personal assistant. Use the tools, then answer in two sentences.",
		},
		Input: []llm.Message{llm.UserMessage(llm.Text(request))},
	})
}

func printPick(p Pick) {
	if p.Err != nil {
		return // the hook already warned
	}
	fmt.Printf("[toolpick] %s kept %d tools, hid %d (~%d bytes of definitions per model call)\n",
		p.Model, len(p.Kept), len(p.Hidden), p.HiddenBytes)
	for _, k := range p.Kept {
		if k.Always {
			fmt.Printf("   always  %s\n", k.Name)
		} else {
			fmt.Printf("   %.2f    %s\n", k.P, k.Name)
		}
	}
}

// catalogTools builds the catalog. Each tool prints its call and returns
// its canned result.
func catalogTools() tool.List {
	var list tool.List
	for _, c := range catalog {
		props := map[string]any{}
		required := []string{} // null is not a valid "required"
		for p := range strings.SplitSeq(c.params, ",") {
			if p != "" {
				props[p] = map[string]string{"type": "string"}
				required = append(required, p)
			}
		}
		schema, _ := json.Marshal(map[string]any{
			"type": "object", "properties": props, "required": required})
		list = append(list, tool.Tool{
			Definition: tool.Definition{
				Name: c.name, Description: c.description, Schema: schema, Kind: tool.KindLocal},
			Executor: canned(cmp.Or(c.result, "Done.")),
		})
	}
	return list
}

type canned string

func (c canned) Execute(_ context.Context, call tool.Call) (tool.Outcome, error) {
	fmt.Printf("   %s %s\n", call.Name, call.Args)
	return tool.Succeeded(string(c)), nil
}

// scriptedModel is what a model would do with the narrowed tools.
func scriptedModel() *dashitest.Client {
	return dashitest.NewClient(
		dashitest.CallTool("call-1", "calendar_list_events", `{"date":"2026-10-03"}`),
		dashitest.CallTool("call-2", "contacts_search", `{"name":"Sam"}`),
		dashitest.CallTool("call-3", "email_send", `{"to":"sam.lee@example.com",`+
			`"subject":"Running late","body":"Hi Sam, I'll be a few minutes late to the 10am design review."}`),
		dashitest.Reply("Tomorrow you have a 10:00 design review with Sam Lee and a 14:00 1:1 "+
			"with Priya. I emailed Sam that you'll be late to the 10am."),
	)
}

// stubJudge starts a fake Decide server that answers with stubRules.
func stubJudge() (*decidetest.Server, *decide.Client, error) {
	srv, err := decidetest.Start()
	if err != nil {
		return nil, nil, err
	}
	srv.Respond(func(req *decide.Request) (*decide.Response, error) {
		state, _ := req.State.(string)
		resp := &decide.Response{Answers: map[string]decide.Answer{}}
		for key := range req.Questions {
			p := 0.04
			for _, r := range stubRules {
				if strings.Contains(state, r.trigger) && strings.HasPrefix(key, r.prefix) {
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

// modelFacts returns the turn's model calls as recorded when each started.
func modelFacts(turn dashi.Turn) []*dashi.ModelFact {
	var facts []*dashi.ModelFact
	for _, s := range turn.Steps {
		if s.Kind == dashi.StepModelCallStarted {
			facts = append(facts, s.Model)
		}
	}
	return facts
}
