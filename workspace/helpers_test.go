package workspace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/tool"
)

// newTestTools returns the tools for a fresh temporary workspace.
func newTestTools(t *testing.T, opts ...Option) (tool.List, string) {
	t.Helper()
	dir := t.TempDir()
	list, err := Tools(dir, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return list, dir
}

// call runs the named tool and returns its outcome and text.
func call(
	t *testing.T,
	list tool.List,
	name string,
	args any,
) (tool.Outcome, string) {
	t.Helper()
	return callCtx(t, context.Background(), list, name, args)
}

func callCtx(
	t *testing.T,
	ctx context.Context,
	list tool.List,
	name string,
	args any,
) (tool.Outcome, string) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range list {
		if tl.Definition.Name != name {
			continue
		}
		out, err := tl.Executor.Execute(ctx, tool.Call{
			ID:   "call-1",
			Name: name,
			Args: raw,
		})
		if err != nil {
			t.Fatal(err)
		}
		text := ""
		if out.Output != nil {
			for _, b := range out.Output.Content {
				text += b.Text.Text
			}
		}
		return out, text
	}
	t.Fatalf("tool %q not found", name)
	return tool.Outcome{}, ""
}

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func wantState(t *testing.T, out tool.Outcome, text string, state tool.State) {
	t.Helper()
	if out.State != state {
		t.Fatalf("state = %s, want %s; text: %s", out.State, state, text)
	}
}

func wantContains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("text %q does not contain %q", text, want)
	}
}
