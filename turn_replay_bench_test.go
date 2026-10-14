package dashi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// benchToolScript scripts calls model calls, each but the last asking for
// two tool calls.
func benchToolScript(calls int) ([]llm.Result, Agent, tool.Toolset) {
	results := make([]llm.Result, 0, calls)
	for i := range calls - 1 {
		parts := []llm.Content{llm.Text("reading")}
		for j := range 2 {
			call := tool.Call{
				ID:   fmt.Sprintf("c%d_%d", i, j),
				Name: "read",
				Args: []byte(`{"value":1}`),
			}
			parts = append(parts, llm.Content{Kind: llm.ContentToolCall, ToolCall: &call})
		}
		results = append(results, llm.Result{
			Message:   llm.AssistantMessage(parts...),
			Stop:      llm.Stop{Kind: llm.StopToolUse},
			Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
			Usage:     &llm.Usage{InputTokens: 1000, OutputTokens: 20},
		})
	}
	last := final("done")
	last.Usage = &llm.Usage{InputTokens: 1000, OutputTokens: 20}
	results = append(results, last)
	a := testAgent()
	a.Limits.ModelCalls = calls + 1
	tools := tool.List{tool.MustFunc("read", "Read test evidence", func(_ context.Context, _ tool.Call, _ struct {
		Value int `json:"value"`
	}) (tool.Outcome, error) {
		return tool.Succeeded(strings.Repeat("evidence ", 200)), nil
	})}
	return results, a, tools
}

func runBenchToolTurn(tb testing.TB, calls int) Turn {
	tb.Helper()
	results, a, tools := benchToolScript(calls)
	out, err := (Runner{Client: &scriptedClient{results: results}, Tools: tools}).Run(
		context.Background(),
		Turn{ID: "bench"},
		Start{CommandID: "start", Agent: a, Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		tb.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	return out.Turn
}

// BenchmarkRunToolTurn measures a whole run, in which the runner rebuilds
// the view many times.
func BenchmarkRunToolTurn(b *testing.B) {
	for _, calls := range []int{10, 25, 50} {
		steps := len(runBenchToolTurn(b, calls).Steps)
		b.Run(fmt.Sprintf("steps=%d", steps), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				runBenchToolTurn(b, calls)
			}
		})
	}
}

// BenchmarkTurnView measures one View of a finished turn.
func BenchmarkTurnView(b *testing.B) {
	for _, calls := range []int{25, 50} {
		turn := runBenchToolTurn(b, calls)
		b.Run(fmt.Sprintf("steps=%d", len(turn.Steps)), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := turn.View(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSnapshotView measures copying a finished turn's view.
func BenchmarkSnapshotView(b *testing.B) {
	s, err := replay(runBenchToolTurn(b, 50))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = snapshotView(s.TurnView)
	}
}
