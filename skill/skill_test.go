package skill_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/skill"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestBundleCatalogAndLoader(t *testing.T) {
	definitions := []skill.Definition{
		{
			Name:         "Writer",
			Description:  "Draft concise\n responses.",
			Instructions: "SECRET_WRITER_BODY\nallowed-tools: bash\n!{echo nope}",
		},
		{
			Name:         "Incident Review",
			Instructions: "Read the incident timeline first.",
		},
	}
	bundle, err := skill.New(
		definitions,
		skill.Options{ToolName: "invoke_skill"},
	)
	if err != nil {
		t.Fatal(err)
	}
	definitions[0].Instructions = "changed after construction"
	catalog := bundle.Catalog()
	if !strings.Contains(catalog, "invoke_skill") ||
		!strings.Contains(catalog, "\"Incident Review\"") ||
		!strings.Contains(catalog, "\"Writer\": \"Draft concise responses.\"") ||
		strings.Contains(catalog, "SECRET_WRITER_BODY") ||
		strings.Contains(catalog, "Read the incident timeline first.") {
		t.Fatalf("catalog = %q", catalog)
	}
	tools, err := bundle.Tools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Definition.Name != "invoke_skill" ||
		tools[0].Definition.Kind != tool.KindLocal ||
		tools[0].Definition.Annotations.ReadOnly == nil ||
		!*tools[0].Definition.Annotations.ReadOnly {
		t.Fatalf("loader = %+v", tools)
	}
	tools[0].Definition.Schema[0] = '['
	fresh, err := bundle.Tools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh[0].Definition.Validate(); err != nil {
		t.Fatalf("caller mutated bundle tool: %v", err)
	}

	got := load(t, bundle, "incident review")
	if got.State != tool.StateSucceeded ||
		outputText(got) != "# Incident Review\n\nRead the incident timeline first." {
		t.Fatalf("loaded = %+v", got)
	}
	got = load(t, bundle, "writer")
	if got.State != tool.StateSucceeded ||
		!strings.Contains(outputText(got), "SECRET_WRITER_BODY") ||
		!strings.Contains(outputText(got), "!{echo nope}") ||
		strings.Contains(outputText(got), "changed after construction") {
		t.Fatalf("loaded = %+v", got)
	}
	got = load(t, bundle, "missing")
	if got.State != tool.StateFailed ||
		!strings.Contains(outputText(got), "Available skills:") ||
		strings.Contains(outputText(got), "SECRET_WRITER_BODY") {
		t.Fatalf("unknown = %+v", got)
	}
	got = load(t, bundle, strings.Repeat("x", 10000))
	if got.State != tool.StateFailed ||
		len(outputText(got)) > 100 {
		t.Fatalf("unbounded invalid-name response = %+v", got)
	}
}

func TestBundleDeterministicHashAndEmptySet(t *testing.T) {
	a := skill.Definition{Name: "Alpha", Description: "First", Instructions: "A"}
	b := skill.Definition{Name: "Beta", Instructions: "B"}
	first, err := skill.New([]skill.Definition{b, a}, skill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := skill.New([]skill.Definition{a, b}, skill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Catalog() != second.Catalog() ||
		first.Hash() != second.Hash() ||
		!strings.HasPrefix(first.Hash(), "sha256:") {
		t.Fatalf("bundle depends on input order: %q vs %q", first.Hash(), second.Hash())
	}
	changed, err := skill.New(
		[]skill.Definition{a, {Name: "Beta", Instructions: "changed"}},
		skill.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	renamedTool, err := skill.New(
		[]skill.Definition{a, b},
		skill.Options{ToolName: "invoke_skill"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash() == changed.Hash() ||
		first.Hash() == renamedTool.Hash() {
		t.Fatal("changed body or loader name did not change digest")
	}
	empty, err := skill.New(nil, skill.Options{})
	emptyTools, toolErr := empty.Tools(t.Context())
	if err != nil || toolErr != nil ||
		empty.Catalog() != "" || len(emptyTools) != 0 {
		t.Fatalf("empty = %+v, err %v", empty, err)
	}
}

func TestBundleJoinReportsHostToolNameCollision(t *testing.T) {
	bundle, err := skill.New(
		[]skill.Definition{{Name: "Review", Instructions: "Read first."}},
		skill.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	loader, err := bundle.Tools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.Join(tool.List{loader[0]}, bundle).Tools(t.Context())
	if err == nil || !strings.Contains(err.Error(), "bound twice") {
		t.Fatalf("duplicate loader name error = %v", err)
	}
}

func TestBundleValidationAndSizeLimits(t *testing.T) {
	tests := []struct {
		name string
		defs []skill.Definition
		opts skill.Options
		want string
	}{
		{
			name: "blank name",
			defs: []skill.Definition{{Instructions: "text"}},
			want: "empty name",
		},
		{
			name: "blank body",
			defs: []skill.Definition{{Name: "one", Instructions: "  "}},
			want: "empty instructions",
		},
		{
			name: "duplicate",
			defs: []skill.Definition{
				{Name: "Incident Review", Instructions: "a"},
				{Name: "incident review", Instructions: "b"},
			},
			want: "duplicate names",
		},
		{
			name: "unicode fold",
			defs: []skill.Definition{
				{Name: "Kelvin", Instructions: "a"},
				{Name: "Kelvin", Instructions: "b"},
			},
			want: "duplicate names",
		},
		{
			name: "newline name",
			defs: []skill.Definition{{Name: "one\ntwo", Instructions: "a"}},
			want: "invalid name",
		},
		{
			name: "control description",
			defs: []skill.Definition{{
				Name: "one", Description: "first\x00second", Instructions: "a",
			}},
			want: "invalid text",
		},
		{
			name: "long name",
			defs: []skill.Definition{{
				Name: strings.Repeat("a", 81), Instructions: "a",
			}},
			want: "80 bytes",
		},
		{
			name: "long description",
			defs: []skill.Definition{{
				Name: "one", Description: strings.Repeat("a", 1025),
				Instructions: "a",
			}},
			want: "1024 bytes",
		},
		{
			name: "large multi-token description",
			defs: []skill.Definition{{
				Name: "one", Description: strings.Repeat("word ", 100_000),
				Instructions: "a",
			}},
			want: "1024 bytes",
		},
		{
			name: "long body",
			defs: []skill.Definition{{
				Name: "one", Instructions: strings.Repeat("a", 65537),
			}},
			want: "65536 bytes",
		},
		{
			name: "bad tool name",
			defs: []skill.Definition{{Name: "one", Instructions: "a"}},
			opts: skill.Options{ToolName: "bad name"},
			want: "invalid tool name",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := skill.New(test.defs, test.opts)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New error = %v; want %q", err, test.want)
			}
		})
	}

	// Each description fits alone, but the rendered catalog must also fit.
	many := make([]skill.Definition, 80)
	for i := range many {
		many[i] = skill.Definition{
			Name:         fmt.Sprintf("skill-%03d", i),
			Description:  strings.Repeat("d", 1024),
			Instructions: "body",
		}
	}
	if _, err := skill.New(many, skill.Options{}); err == nil ||
		!strings.Contains(err.Error(), "catalog") {
		t.Fatalf("large catalog error = %v", err)
	}
}

func TestOversizedDescriptionHasBoundedConstructionAllocation(t *testing.T) {
	// A prior implementation split the entire input into words before
	// checking the limit. This input is much larger than the limit and has
	// many tokens, so that path allocated proportionally to its size.
	definitions := []skill.Definition{{
		Name:         "one",
		Description:  strings.Repeat("word ", 100_000),
		Instructions: "body",
	}}
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			_, err := skill.New(definitions, skill.Options{})
			if err == nil {
				b.Fatal("oversized description was accepted")
			}
		}
	})
	if got := result.AllocedBytesPerOp(); got > 32<<10 {
		t.Fatalf("constructor allocated %d bytes per rejection", got)
	}
}

func load(t *testing.T, bundle *skill.Bundle, name string) tool.Outcome {
	t.Helper()
	args, err := json.Marshal(struct {
		Name string `json:"name"`
	}{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := bundle.Tools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	loader := tools[0]
	outcome, err := loader.Executor.Execute(
		t.Context(),
		tool.Call{ID: "load-1", Name: loader.Definition.Name, Args: args},
	)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func outputText(outcome tool.Outcome) string {
	if outcome.Output == nil || len(outcome.Output.Content) == 0 ||
		outcome.Output.Content[0].Text == nil {
		return ""
	}
	return outcome.Output.Content[0].Text.Text
}
