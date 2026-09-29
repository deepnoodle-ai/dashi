//go:build live

package workspace_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/workspace"
)

// TestLiveAgentFixesFailingTest makes paid OpenAI calls. It needs
// OPENAI_API_KEY. OPENAI_MODEL selects the model, gpt-6-luna by default.
func TestLiveAgentFixesFailingTest(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("set OPENAI_API_KEY to make paid agent calls")
	}
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-6-luna"
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/calc\n\ngo 1.22\n",
		"calc.go": "package calc\n\n// Add returns the sum of a and b.\n" +
			"func Add(a, b int) int {\n\treturn a - b\n}\n",
		"calc_test.go": "package calc\n\nimport \"testing\"\n\n" +
			"func TestAdd(t *testing.T) {\n\tif got := Add(2, 3); got != 5 {\n" +
			"\t\tt.Fatalf(\"Add(2, 3) = %d, want 5\", got)\n\t}\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	tools := ws.Tools()
	var approvals atomic.Int32
	policy := ws.Policy(
		func(context.Context, workspace.ApprovalRequest) (workspace.Approval, error) {
			approvals.Add(1)
			return workspace.Approval{Allow: true}, nil
		},
	)
	client, err := responses.New(responses.Config{APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := (dashi.Runner{
		Client: client,
		Tools:  tools,
		Hooks:  policy.Hooks(),
	}).Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{
			Model: model,
			Instructions: "You are a coding agent. Use the tools to work in " +
				"the workspace. Run the tests to check your work.",
		},
		Input: []llm.Message{llm.UserMessage(llm.Text(
			"The test in this Go module fails. Fix the bug in the code, not the test."))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("stop = %s: %s", out.Stop.Kind, out.Stop.Reason)
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tests still fail: %v\n%s", err, output)
	}
	if approvals.Load() == 0 {
		t.Fatal("the agent changed code without an approval")
	}
	t.Logf("model %s: answer %q, %d approvals", model, out.Answer, approvals.Load())
}
