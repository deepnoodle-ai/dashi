// Command example runs an agent whose run_tests tool keeps failing the same
// way, with a turnwatch.Watcher on its turn. After the second identical
// failure the watcher asks Decide whether the agent is repeating a failed
// approach; on a yes, the agent gets a short reminder from the watcher and
// changes approach: it reads the code, fixes it, and the tests pass. Each
// check prints one line.
//
// By default it needs no API keys: a scripted dashitest client plays the
// agent, and a decidetest server answers from a fixed rule. Both are
// stand-ins, so the output is always the same.
//
//	go run ./decide/turnwatch/example
//
// With -live it uses Claude (ANTHROPIC_API_KEY; ANTHROPIC_MODEL optionally
// overrides the catalog's default model) and Jev on the TypeSafe API
// (TYPESAFE_API_KEY). The agent is told the tests are flaky, so it is
// likely to rerun them before it looks at the code:
//
//	go run ./decide/turnwatch/example -live
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
	"sync/atomic"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/turnwatch"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

const request = "The parser tests fail. Make them pass."

// instructions invite the loop the watcher is for: told the tests are
// flaky, an agent tends to rerun them before reading the code.
const instructions = "You maintain a small Go parser in parser/. Its tests " +
	"are flaky and usually pass on a rerun, so rerun failing tests a few " +
	"times before you investigate. Answer in two sentences when you are done."

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

	watcher := &turnwatch.Watcher{Client: judge, OnCheck: func(c turnwatch.Check) {
		fmt.Fprintf(w, "[turnwatch] %s after %d model calls:", c.Trigger, c.ModelCalls)
		for _, key := range []string{"repeating", "off_request"} {
			if a, ok := c.Answers[key].(*decide.NoulAnswer); ok {
				fmt.Fprintf(w, " %s=%.2f", key, a.Noul)
			}
		}
		if c.Err != nil {
			fmt.Fprintf(w, " (decide: %v)", c.Err)
		}
		fmt.Fprintf(w, " -> %s\n", c.Action)
	}}
	runner := dashi.Runner{
		Client: client,
		Tools:  repoTools(w),
		Hooks:  watcher.Hook().Hooks(),
	}
	return runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{
			Model:        model,
			Instructions: instructions,
			Limits:       dashi.Limits{ModelCalls: 12},
		},
		Input: []llm.Message{llm.UserMessage(llm.Text(request))},
	})
}

type testArgs struct{}

type readArgs struct {
	Path string `json:"path" description:"File path"`
}

type writeArgs struct {
	Path    string `json:"path" description:"File path"`
	Content string `json:"content" description:"The whole new file content"`
}

const lexSource = `package parser

// next returns the next token. Operators are one byte long.
func (l *lexer) next() token {
	c := l.src[l.pos]
	l.pos++
	return token{kind: opKind(c), text: string(c)}
}
`

// files is the fake repository's parser package.
var files = map[string]string{
	"lex.go": lexSource,
	"parse.go": "package parser\n\n// Parse lexes src and builds the tree " +
		"from its tokens.\nfunc Parse(src string) (*Node, error) { ... }\n",
	"parser_test.go": "package parser\n\nfunc TestParseArrow(t *testing.T) " +
		"{\n\tcheckTokens(t, \"a->b\", \"a\", \"->\", \"b\")\n}\n",
}

// repoTools are fakes for a repository: run_tests fails the same way, word
// for word, until parser/lex.go has been written, and then passes.
func repoTools(w io.Writer) tool.List {
	var fixed atomic.Bool
	tests := tool.MustFunc("run_tests", "Runs the parser package's Go tests.",
		func(context.Context, tool.Call, testArgs) (tool.Outcome, error) {
			if fixed.Load() {
				fmt.Fprintln(w, "   run_tests -> ok")
				return tool.Succeeded("ok  \texample.com/parser\t0.004s"), nil
			}
			fmt.Fprintln(w, "   run_tests -> FAIL")
			return tool.Failed("--- FAIL: TestParseArrow (0.00s)\n" +
				"    parser_test.go:41: tokens of \"a->b\": got [a - > b], want [a -> b]\n" +
				"FAIL\texample.com/parser\t0.003s"), nil
		})
	read := tool.MustFunc("read_file", "Reads a file in the repository.",
		func(_ context.Context, _ tool.Call, in readArgs) (tool.Outcome, error) {
			fmt.Fprintf(w, "   read_file %s\n", in.Path)
			if text, ok := files[strings.TrimPrefix(in.Path, "parser/")]; ok {
				return tool.Succeeded(text), nil
			}
			return tool.Failed("no such file: " + in.Path +
				"; parser/ holds lex.go, parse.go, and parser_test.go"), nil
		}, tool.ReadOnly())
	write := tool.MustFunc("write_file", "Writes a file in the repository.",
		func(_ context.Context, _ tool.Call, in writeArgs) (tool.Outcome, error) {
			fmt.Fprintf(w, "   write_file %s\n", in.Path)
			if strings.HasSuffix(in.Path, "lex.go") {
				fixed.Store(true)
			}
			return tool.Succeeded("wrote " + in.Path), nil
		})
	return tool.List{tests, read, write}
}

// scriptedAgent plays an agent that reruns the failing tests, then, after
// the watcher's reminder, reads the lexer, fixes it, and reruns them.
func scriptedAgent() *dashitest.Client {
	fix := strings.Replace(lexSource, "Operators are one byte long.",
		"\"->\" is one operator.", 1)
	args, _ := json.Marshal(writeArgs{Path: "parser/lex.go", Content: fix})
	return dashitest.NewClient(
		dashitest.CallTool("call-1", "run_tests", `{}`),
		dashitest.CallTool("call-2", "run_tests", `{}`),
		dashitest.CallTool("call-3", "read_file", `{"path":"parser/lex.go"}`),
		dashitest.CallTool("call-4", "write_file", string(args)),
		dashitest.CallTool("call-5", "run_tests", `{}`),
		dashitest.Reply("The lexer split \"->\" into two tokens; it now reads "+
			"it as one operator. The parser tests pass."),
	)
}

// standInJudge answers for the fake Decide server. Its probabilities come
// from a fixed rule, not a model's judgment: two failed results in a row
// from one tool read as repeating.
func standInJudge(req *decide.Request) (*decide.Response, error) {
	raw, _ := req.State.(json.RawMessage)
	var s struct {
		Recent []struct {
			Tool   string `json:"tool"`
			Failed bool   `json:"failed"`
		} `json:"recent_activity"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	repeating := 0.12
	if n := len(s.Recent); n >= 2 && s.Recent[n-1].Failed && s.Recent[n-2].Failed &&
		s.Recent[n-1].Tool == s.Recent[n-2].Tool {
		repeating = 0.93
	}
	return &decide.Response{Answers: map[string]decide.Answer{
		"repeating":   decidetest.NoulAnswer(repeating),
		"off_request": decidetest.NoulAnswer(0.06),
	}}, nil
}
