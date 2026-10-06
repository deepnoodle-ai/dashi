// Command example runs a support agent with the judge_items and pick_item
// tools from package listtool. The host has two lists: 300 open support
// tickets (30 written out, 270 generated routine ones) and the customer
// contacts. The user asks for the urgent
// billing tickets and an email to the customer on the top one. The agent
// judges every ticket with judge_items, reads the top ticket, finds the
// customer's contact ID with pick_item, and sends the email with a tool
// that checks the ID against the host's own contacts.
//
// By default it needs no API key: a scripted dashitest client plays the
// agent, and a decidetest server stands in for Jev, answering with keyword
// rules. The rules are stand-ins, not model judgments. With -live it uses
// Claude (ANTHROPIC_API_KEY; ANTHROPIC_MODEL optionally overrides the
// catalog's default model) and Jev on the TypeSafe API (TYPESAFE_API_KEY).
// A live run costs a few cents.
//
//	go run ./decide/listtool/example
//	go run ./decide/listtool/example -live
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/listtool"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

const request = "Find the urgent billing tickets, and email the customer on the most urgent one " +
	"that we're on it and will reply within the hour."

const instructions = "You are a support agent for a software company. Use the tools. " +
	"Before you email a customer, read their ticket. Finish with a short summary."

type ticket struct {
	ID, Customer, Company, Subject, Body string
}

type contact struct {
	ID, Name, Company, Role, Email string
}

// tickets is the host's ticket list: the written tickets, then routine
// ones, so the list is too long for the agent to read.
var tickets = append(written, routine(270)...)

var written = []ticket{
	{"T-1001", "Marco Diaz", "Initech", "Dark mode for the dashboard?", "Is dark mode on the roadmap? No rush."},
	{"T-1002", "Lena Fischer", "Umbrella", "Invoice address is wrong", "Our March invoice shows the old office address. Please correct it for next month."},
	{"T-1003", "Tom Becker", "Hooli", "Export to CSV fails", "Exporting reports to CSV gives an empty file."},
	{"T-1004", "Ana Costa", "Stark Industries", "Production outage: API returns 500", "All API calls fail since 9:40. Our checkout is down. Please help immediately."},
	{"T-1005", "Priya Shah", "Northwind Traders", "Charged twice, need refund today", "We were charged twice for the annual plan. The second payment overdrew our card account; we need the refund today."},
	{"T-1006", "Kenji Sato", "Wayne Enterprises", "Question about seats", "How do we add three seats to our plan?"},
	{"T-1007", "Olivia Brown", "Acme", "Card declined at renewal", "Our renewal payment was declined. Can you retry the card next week?"},
	{"T-1008", "Sam Lee", "Globex", "SSO login loop", "Users are locked out after the SSO change. Urgent: nobody on the team can log in."},
	{"T-1009", "Rachel Green", "Soylent", "Feature request: webhooks", "We would like webhooks for new orders."},
	{"T-1010", "David Kim", "Vandelay", "Refund for unused seats", "We removed five seats last month. Is a partial refund possible?"},
	{"T-1011", "Fatima Noor", "Cyberdyne", "Payment failed, account suspended", "Our payment failed and the account is now suspended. Payroll runs today; please restore access ASAP."},
	{"T-1012", "Liam Walsh", "Massive Dynamic", "Typo in the docs", "The quickstart has a typo in step 3."},
	{"T-1013", "Chen Wei", "Tyrell", "Receipt for tax filing", "Can you send a receipt for last year's plan?"},
	{"T-1014", "Grace Hopper", "Monsters Inc", "Slow search", "Search takes ten seconds on large projects."},
	{"T-1015", "Noah Jensen", "Gringotts", "Wrong currency on invoice", "The invoice is in USD but our contract is in EUR. The deadline for our books is Friday."},
	{"T-1016", "Mia Rossi", "Aperture", "Mobile app crashes", "The iOS app crashes on launch since the update."},
	{"T-1017", "Omar Haddad", "Oscorp", "Upgrade to enterprise", "We want to talk about the enterprise plan."},
	{"T-1018", "Sofia Alvarez", "Pied Piper", "Duplicate charge on card", "I see two charges this month. Please look into it when you can."},
	{"T-1019", "Jack Turner", "Dunder Mifflin", "Password reset email missing", "Reset emails don't arrive."},
	{"T-1020", "Ava Martin", "Prestige", "Billing contact change", "Please send invoices to finance@prestige.example from now on."},
	{"T-1021", "Lucas Silva", "Weyland", "Data import stuck", "Our import has been running for two days."},
	{"T-1022", "Emma Clark", "Sterling Cooper", "Cancel subscription", "Please cancel our subscription at the end of the term."},
	{"T-1023", "Ivan Petrov", "Black Mesa", "API rate limits", "What are the rate limits for the reporting API?"},
	{"T-1024", "Zoe Adams", "Bluth Company", "Tax ID missing on invoices", "Our VAT number is missing from invoices."},
	{"T-1025", "Hiro Tanaka", "Nakatomi", "Two-factor codes delayed", "2FA codes arrive minutes late."},
	{"T-1026", "Nina Kowalski", "Krusty Krab", "Overcharged after downgrade", "We downgraded but were still charged the old price. Please refund the difference."},
	{"T-1027", "Ben Carter", "Virtucon", "Dashboard widgets missing", "Two widgets disappeared from the dashboard."},
	{"T-1028", "Leila Haddad", "Ollivanders", "Can't download invoice PDF", "The invoice PDF link returns 404."},
	{"T-1029", "Paul Novak", "Strickland Propane", "Slack integration", "Does the Slack integration support threads?"},
	{"T-1030", "Yuki Mori", "Spacely Sprockets", "Account locked after failed payment", "Payment bounced and the whole team is locked out right now. We have a client demo in two hours."},
}

// routine returns n generated tickets about features, none about billing
// and none urgent, numbered after the written tickets.
func routine(n int) []ticket {
	first := []string{"Alex", "Jordan", "Taylor", "Morgan", "Casey", "Riley", "Jamie", "Avery", "Quinn", "Drew"}
	last := []string{"Ng", "Kowal", "Moreau", "Okafor", "Lindqvist", "Brennan", "Ito", "Varga", "Reyes"}
	companies := []string{"Initech", "Hooli", "Globex", "Acme", "Soylent", "Vandelay", "Tyrell", "Aperture"}
	features := []string{"reports", "exports", "webhooks", "the API", "SSO", "dashboards", "search",
		"notifications", "the mobile app", "integrations", "user roles"}
	kinds := []struct{ subject, body string }{
		{"Question about %s", "How do we set up %s for a new team?"},
		{"Feedback on %s", "We like %s. Here are a few small ideas."},
		{"%s feels slow", "Loading %s takes a while on large projects."},
		{"Docs for %s", "Where can we read more about %s?"},
		{"Feature request: %s", "Could %s support custom fields?"},
	}
	out := make([]ticket, n)
	for i := range out {
		feature := features[i%len(features)]
		kind := kinds[i%len(kinds)]
		subject := fmt.Sprintf(kind.subject, feature)
		out[i] = ticket{
			ID:       fmt.Sprintf("T-%d", 1001+len(written)+i),
			Customer: first[i%len(first)] + " " + last[i%len(last)],
			Company:  companies[i%len(companies)],
			Subject:  strings.ToUpper(subject[:1]) + subject[1:],
			Body:     fmt.Sprintf(kind.body, feature),
		}
	}
	return out
}

var contacts = []contact{
	{"c_101", "Marco Diaz", "Initech", "Product manager", "marco@initech.example"},
	{"c_204", "Priya Shah", "Northwind Traders", "Finance lead", "priya.shah@northwind.example"},
	{"c_205", "Priya Sharma", "Northwind Traders", "Engineer", "priya.sharma@northwind.example"},
	{"c_311", "Priya Shah", "Globex", "Designer", "pshah@globex.example"},
	{"c_402", "Ana Costa", "Stark Industries", "CTO", "ana@stark.example"},
	{"c_517", "Fatima Noor", "Cyberdyne", "Operations", "fatima@cyberdyne.example"},
	{"c_518", "Fatima Nour", "Cyberdyne", "Payroll", "f.nour@cyberdyne.example"},
	{"c_620", "Yuki Mori", "Spacely Sprockets", "Account owner", "yuki@spacely.example"},
	{"c_733", "Sam Lee", "Globex", "IT admin", "sam.lee@globex.example"},
	{"c_734", "Sam Lee", "Hooli", "Sales", "slee@hooli.example"},
	{"c_845", "Nina Kowalski", "Krusty Krab", "Owner", "nina@krustykrab.example"},
	{"c_912", "Sofia Alvarez", "Pied Piper", "Finance", "sofia@piedpiper.example"},
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
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
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
		srv, err := fakeDecide()
		if err != nil {
			return err
		}
		defer srv.Close()
		if judge, err = srv.NewClient(); err != nil {
			return err
		}
		model, name = scriptedModel(), dashitest.Model
	}

	cfg := listtool.Config{
		Client: judge,
		Lists:  []listtool.AnyList{ticketList(), contactList()},
		Price:  jevPrice,
		OnResult: func(_ context.Context, _ dashi.CallRef, r listtool.Result) {
			fmt.Printf("   [%s: %d Decide requests, %d input tokens, %s]\n",
				r.Tool, r.Requests, r.Usage.InputTokens, r.Model)
		},
	}
	judgeItems, err := listtool.JudgeItems(cfg)
	if err != nil {
		return err
	}
	pickItem, err := listtool.PickItem(cfg)
	if err != nil {
		return err
	}
	runner := dashi.Runner{
		Client: model,
		Tools: tool.List{
			shown(judgeItems), shown(pickItem),
			shown(tool.MustFunc("get_ticket", "Read one support ticket by its ID.", getTicket, tool.ReadOnly())),
			shown(tool.MustFunc("send_email", "Email a contact by contact ID.", sendEmail)),
		},
	}
	fmt.Println("> " + request)
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{Model: name, Instructions: instructions},
		Input: []llm.Message{llm.UserMessage(llm.Text(request))},
	})
	if err != nil {
		return err
	}
	fmt.Printf("\n%s\n\nStop: %s. Decide spend: %v USD (illustrative price).\n",
		out.Reply, out.Stop.Kind, out.Spend.ToolCost["USD"])
	return nil
}

func ticketList() listtool.List[ticket] {
	return listtool.List[ticket]{
		Name: "tickets",
		Description: `Open support tickets. The query is a search: "status:open" or an empty ` +
			"query means all open tickets.",
		Items: func(context.Context, string) ([]ticket, error) { return tickets, nil },
		ID:    func(t ticket) string { return t.ID },
		Text:  func(t ticket) string { return t.Subject + "\n" + t.Body },
	}
}

func contactList() listtool.List[contact] {
	return listtool.List[contact]{
		Name:        "contacts",
		Description: "Customer contacts: name, company, and role.",
		// A real host would search by the looking_for text here.
		Items: func(context.Context, string) ([]contact, error) { return contacts, nil },
		ID:    func(c contact) string { return c.ID },
		Text:  func(c contact) string { return fmt.Sprintf("%s, %s, %s", c.Name, c.Company, c.Role) },
	}
}

type ticketArgs struct {
	ID string `json:"id" description:"The ticket ID, such as T-1001"`
}

func getTicket(_ context.Context, _ tool.Call, in ticketArgs) (tool.Outcome, error) {
	for _, t := range tickets {
		if t.ID == in.ID {
			return tool.Succeeded(fmt.Sprintf("%s from %s (%s)\nSubject: %s\n%s",
				t.ID, t.Customer, t.Company, t.Subject, t.Body)), nil
		}
	}
	return tool.Outcome{}, fmt.Errorf("no ticket %q", in.ID)
}

type emailArgs struct {
	ContactID string `json:"contact_id" description:"The contact's ID, from pick_item"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
}

// sendEmail checks the ID against the host's own contacts, as an action
// tool always should. It prints the email instead of sending it.
func sendEmail(_ context.Context, _ tool.Call, in emailArgs) (tool.Outcome, error) {
	i := slices.IndexFunc(contacts, func(c contact) bool { return c.ID == in.ContactID })
	if i < 0 {
		return tool.Outcome{}, fmt.Errorf("no contact %q", in.ContactID)
	}
	return tool.Succeeded(fmt.Sprintf("Sent to %s <%s>.", contacts[i].Name, contacts[i].Email)), nil
}

// jevPrice is an illustrative price, $0.042 per million input tokens. Use
// your contract's price.
func jevPrice(_ string, u decide.Usage) *tool.Cost {
	return &tool.Cost{Amount: money.AmountFromNanos(int64(u.InputTokens) * 42), Currency: "USD"}
}

// shown prints each call and its result.
func shown(t tool.Tool) tool.Tool {
	inner := t.Executor
	t.Executor = printing{inner}
	return t
}

type printing struct{ tool.Executor }

func (p printing) Execute(ctx context.Context, call tool.Call) (tool.Outcome, error) {
	fmt.Printf("\n-> %s %s\n", call.Name, call.Args)
	o, err := p.Executor.Execute(ctx, call)
	switch {
	case err != nil:
		fmt.Printf("   error: %v\n", err)
	case o.Output != nil:
		for _, b := range o.Output.Content {
			if b.Text != nil {
				fmt.Println("   " + strings.ReplaceAll(b.Text.Text, "\n", "\n   "))
			}
		}
	}
	return o, err
}

// scriptedModel is what a model would do with these tools.
func scriptedModel() *dashitest.Client {
	return dashitest.NewClient(
		dashitest.CallTool("call-1", "judge_items", `{"list": "tickets", "query": "status:open",
			"questions": [
				{"key": "billing", "type": "yes_no", "text": "Is this ticket about a billing or payment problem?"},
				{"key": "urgent", "type": "yes_no", "text": "Does it need a reply within hours rather than days?"}],
			"find": {"billing": "yes", "urgent": "yes"}}`),
		dashitest.CallTool("call-2", "get_ticket", `{"id": "T-1011"}`),
		dashitest.CallTool("call-3", "pick_item", `{"list": "contacts",
			"looking_for": "Fatima Noor from Cyberdyne, the customer on ticket T-1011"}`),
		dashitest.CallTool("call-4", "send_email", `{"contact_id": "c_517",
			"subject": "Your suspended account (T-1011)",
			"body": "Hi Fatima, we're on it and will reply within the hour."}`),
		dashitest.Reply("Three tickets are urgent billing problems: T-1011, T-1030, and T-1005. "+
			"T-1011 (Fatima Noor, Cyberdyne: a failed payment suspended the account on payroll day) "+
			"ranks first, and I emailed Fatima that we're on it. T-1013 and T-1020 were close calls; "+
			"neither looks urgent."),
	)
}

// Keyword weights stand in for Jev in the keyless run. P(yes) for a
// question is 1 - (1-w1)(1-w2)... over the words in the item.
var keywords = map[string]map[string]float64{
	"billing": {"refund": 0.9, "charged": 0.85, "charge": 0.8, "invoice": 0.75, "payment": 0.8,
		"card": 0.4, "receipt": 0.3, "plan": 0.1, "subscription": 0.2},
	"urgent": {"today": 0.7, "asap": 0.8, "urgent": 0.9, "immediately": 0.85, "down": 0.5,
		"locked": 0.6, "now": 0.4, "hours": 0.5, "deadline": 0.3, "friday": 0.1},
}

var word = regexp.MustCompile(`[a-z0-9]+`)

// fakeDecide answers yes_no questions with the keyword weights and a pick
// by the words looking_for shares with each option.
func fakeDecide() (*decidetest.Server, error) {
	srv, err := decidetest.Start()
	if err != nil {
		return nil, err
	}
	srv.Respond(func(req *decide.Request) (*decide.Response, error) {
		state, _ := req.State.(string)
		words := word.FindAllString(strings.ToLower(state), -1)
		resp := &decide.Response{
			Answers: map[string]decide.Answer{},
			Usage:   decide.Usage{InputTokens: len(state)/4 + 30*len(req.Questions)},
		}
		for key, q := range req.Questions {
			switch q := q.(type) {
			case *decide.NoulQuestion:
				no := 0.97
				for _, w := range words {
					no *= 1 - keywords[key][w]
				}
				resp.Answers[key] = decidetest.NoulAnswer(math.Round((1-no)*100) / 100)
			case *decide.ChoiceQuestion:
				resp.Answers[key] = pickByWords(q, state)
			}
		}
		return resp, nil
	})
	return srv, nil
}

// pickByWords weighs each option by the square of the words it shares
// with looking_for; "none of these" weighs 1.
func pickByWords(q *decide.ChoiceQuestion, state string) *decide.ChoiceAnswer {
	lookingFor, _, _ := strings.Cut(strings.TrimPrefix(state, "looking_for: "), "\n")
	wanted := word.FindAllString(strings.ToLower(lookingFor), -1)
	weights := make([]float64, len(q.Criteria))
	total := 0.0
	for i, o := range q.Criteria {
		weights[i] = 1
		if o.Key != "none" {
			shared := 0
			for _, w := range word.FindAllString(strings.ToLower(fmt.Sprint(o.Description)), -1) {
				if len(w) > 2 && slices.Contains(wanted, w) {
					shared++
				}
			}
			weights[i] = float64(shared*shared) + 0.05
		}
		total += weights[i]
	}
	for i := range weights {
		weights[i] /= total
	}
	return decidetest.ChoiceFor(q, weights...)
}
