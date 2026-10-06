// Command riskgate screens each tool call with a decision model before it
// runs. A BeforeTool hook (gate.go) sends the user's request, the tool, and
// the exact arguments to Decide, asks two yes-or-no questions, and turns the
// probabilities into an outcome with Decide's gate pattern: ordinary calls
// run without interruption, a destructive call waits for an operator, and a
// call beyond the request is refused. The advice only tightens; it never
// approves a call by itself. Each check prints one line.
//
// By default it needs no API keys: a scripted dashitest client plays the
// agent, and a decidetest server answers with canned probabilities from
// simple rules. Both are stand-ins, so the output is always the same.
//
//	go run ./examples/decide/riskgate
//
// With -live it uses Claude (ANTHROPIC_API_KEY; ANTHROPIC_MODEL optionally
// overrides the catalog's default model) and Jev on the TypeSafe API
// (TYPESAFE_API_KEY):
//
//	go run ./examples/decide/riskgate -live
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	"github.com/deepnoodle-ai/decide/patterns/gate"
)

const request = "build-01's disk is almost full. Look in /var/cache/build " +
	"and delete the 2025 cache directory."

type pathArgs struct {
	Path string `json:"path" description:"Absolute path"`
}

type shellArgs struct {
	Command string `json:"command" description:"Shell command to run"`
}

func main() {
	live := flag.Bool("live", false, "use Claude and Jev instead of stand-ins")
	flag.Parse()
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	out, err := run(ctx, *live, os.Stdout)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nStop: %s\n%s\n", out.Stop.Kind, out.Reply)
}

func run(ctx context.Context, live bool, w io.Writer) (dashi.RunResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	var client llm.Client
	var judge *decide.Client
	var err error
	model := os.Getenv("ANTHROPIC_MODEL")
	if live {
		if client, err = messages.New(messages.Config{}); err != nil {
			return dashi.RunResult{}, err
		}
		if judge, err = decide.NewClient(); err != nil {
			return dashi.RunResult{}, err
		}
	} else {
		srv, err := decidetest.Start()
		if err != nil {
			return dashi.RunResult{}, err
		}
		defer srv.Close()
		srv.Respond(standInJudge)
		if judge, err = srv.NewClient(); err != nil {
			return dashi.RunResult{}, err
		}
		client, model = scriptedAgent(), dashitest.Model
	}

	effect := map[gate.Outcome]string{
		gate.Allow: "no opinion", gate.Review: "held", gate.Escalate: "denied"}
	report := func(call tool.Call, d gate.Decision, err error) {
		var b strings.Builder
		for _, r := range d.Readings {
			if r.Problem != "" {
				fmt.Fprintf(&b, " %s=%s", r.Input, r.Problem)
			} else {
				fmt.Fprintf(&b, " %s=%.2f", r.Input, r.Value)
			}
		}
		if err != nil {
			fmt.Fprintf(&b, " (decide: %v)", err)
		}
		fmt.Fprintf(w, "[risk] %s%s -> %s (%s)\n",
			call.Name, b.String(), d.Outcome, effect[d.Outcome])
	}
	runner := dashi.Runner{
		Client: client,
		Tools:  opsTools(w),
		Hooks:  riskGate(judge, request, report).Hooks(),
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{
			Model: model,
			Instructions: "You are an ops assistant for the build hosts. " +
				"Use the tools, then answer in two sentences.",
		},
		Input: []llm.Message{llm.UserMessage(llm.Text(request))},
	})
	// A held call stops the run waiting. A stand-in operator reads each one
	// and approves it; Approve records the decision and Continue runs it.
	for err == nil && out.Stop.Kind == dashi.StopWaiting {
		for _, p := range out.Pending {
			if p.Kind != dashi.PendingApproval {
				continue // Continue reports anything left unanswered.
			}
			fmt.Fprintf(w, "[operator] approved %s %s\n", p.Name, p.Args)
			if out, err = runner.Run(ctx, out.Turn, p.Approve()); err != nil {
				return out, err
			}
		}
		out, err = runner.Run(ctx, out.Turn, dashi.Continue{})
	}
	return out, err
}

// opsTools are fakes: they print what they would do and return canned text.
func opsTools(w io.Writer) tool.List {
	list := tool.MustFunc("list_files", "Lists a directory with sizes.",
		func(_ context.Context, _ tool.Call, in pathArgs) (tool.Outcome, error) {
			fmt.Fprintf(w, "   list_files %s\n", in.Path)
			return tool.Succeeded("2025/  41G\n2026/  3G\ncurrent -> 2026"), nil
		}, tool.ReadOnly())
	del := tool.MustFunc("delete_path", "Deletes a file or directory tree.",
		func(_ context.Context, _ tool.Call, in pathArgs) (tool.Outcome, error) {
			fmt.Fprintf(w, "   delete_path %s\n", in.Path)
			return tool.Succeeded("Deleted " + in.Path), nil
		})
	shell := tool.MustFunc("run_shell", "Runs a shell command on the host.",
		func(_ context.Context, _ tool.Call, in shellArgs) (tool.Outcome, error) {
			fmt.Fprintf(w, "   run_shell %s\n", in.Command)
			return tool.Succeeded("ok"), nil
		})
	return tool.List{list, del, shell}
}

// scriptedAgent plays an agent that lists, deletes what was asked, and
// then overreaches with a shell command the user never asked for.
func scriptedAgent() *dashitest.Client {
	return dashitest.NewClient(
		dashitest.CallTool("call-1", "list_files", `{"path":"/var/cache/build"}`),
		dashitest.CallTool("call-2", "delete_path", `{"path":"/var/cache/build/2025"}`),
		dashitest.CallTool("call-3", "run_shell", `{"command":"rm -rf /var/log/*"}`),
		dashitest.Reply("I deleted /var/cache/build/2025, freeing 41G. "+
			"Clearing /var/log was refused as outside your request."),
	)
}

// standInJudge answers for the fake Decide server. Its probabilities come
// from fixed rules, not a model's judgment: a delete or an rm is
// destructive, and arguments outside /var/cache/build are out of scope.
func standInJudge(req *decide.Request) (*decide.Response, error) {
	raw, _ := req.State.(json.RawMessage)
	var s callState
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	args := string(s.Arguments)
	risky, beyond := 0.04, 0.08
	if s.Tool == "delete_path" || strings.Contains(args, "rm ") {
		risky = 0.91
	}
	if !strings.Contains(args, "/var/cache/build") {
		beyond = 0.87
	}
	return &decide.Response{Answers: map[string]decide.Answer{
		"destructive":  decidetest.NoulAnswer(risky),
		"out_of_scope": decidetest.NoulAnswer(beyond),
	}}, nil
}
