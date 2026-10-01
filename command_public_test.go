package dashi_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

// A decoded command runs exactly like the original, however a host saved
// it: through MarshalCommand or plain encoding/json.
func TestDecodedStartRunsLikeOriginal(t *testing.T) {
	start := dashi.Start{
		CommandID: "start-1",
		Agent:     dashi.Agent{Model: "m"},
		Input:     []llm.Message{llm.UserMessage(llm.Text("hi"))},
	}
	codec := func(t *testing.T, c dashi.Start) dashi.Command {
		data, err := dashi.MarshalCommand(c)
		if err != nil {
			t.Fatal(err)
		}
		got, err := dashi.UnmarshalCommand(data)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	plain := func(t *testing.T, c dashi.Start) dashi.Command {
		data, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var got dashi.Start
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	nullSchemas := func(t *testing.T, c dashi.Start) dashi.Command {
		c.OutputSchema = json.RawMessage("null")
		c.Agent.OutputSchema = json.RawMessage("null")
		return c
	}
	run := func(t *testing.T, turn dashi.Turn, c dashi.Command) dashi.RunResult {
		t.Helper()
		r := dashi.Runner{Client: dashitest.NewClient(dashitest.Reply("done"))}
		out, err := r.Run(context.Background(), turn, c)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	want := run(t, dashi.Turn{ID: "t"}, start)
	for name, decode := range map[string]func(*testing.T, dashi.Start) dashi.Command{
		"codec":         codec,
		"encoding/json": plain,
		"null schemas":  nullSchemas,
	} {
		t.Run(name, func(t *testing.T) {
			c := decode(t, start)
			got := run(t, dashi.Turn{ID: "t"}, c)
			if got.Stop.Kind != want.Stop.Kind || len(got.Turn.Steps) != len(want.Turn.Steps) {
				t.Fatalf("stop %q with %d steps, want %q with %d",
					got.Stop.Kind, len(got.Turn.Steps), want.Stop.Kind, len(want.Turn.Steps))
			}
			g, w := got.Turn.Steps[0], want.Turn.Steps[0]
			if g.CommandDigest != w.CommandDigest || !reflect.DeepEqual(g.Started, w.Started) {
				t.Fatalf("start step differs:\n got %+v\nwant %+v", g, w)
			}
			// Replayed on the turn the original started, it is a retry.
			again := run(t, want.Turn, c)
			if again.Revision != want.Revision {
				t.Fatalf("retry advanced the turn to %d from %d", again.Revision, want.Revision)
			}
		})
	}
}

func TestDecodedCommandConflictsWhenChanged(t *testing.T) {
	r := dashi.Runner{Client: dashitest.NewClient(dashitest.Reply("done"))}
	start := dashi.Start{
		CommandID: "s",
		Agent:     dashi.Agent{Model: "m"},
		Input:     []llm.Message{llm.UserMessage(llm.Text("hi"))},
	}
	out, err := r.Run(context.Background(), dashi.Turn{ID: "t"}, start)
	if err != nil {
		t.Fatal(err)
	}
	start.Input = []llm.Message{llm.UserMessage(llm.Text("other"))}
	data, err := dashi.MarshalCommand(start)
	if err != nil {
		t.Fatal(err)
	}
	c, err := dashi.UnmarshalCommand(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), out.Turn, c); !errors.Is(err, dashi.ErrCommandConflict) {
		t.Fatalf("err = %v, want ErrCommandConflict", err)
	}
}
