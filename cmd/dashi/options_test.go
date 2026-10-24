package main

import (
	"bytes"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

// parseArgs runs the command line without running a command. It returns
// zero Options when the command line wrote help or the version instead.
func parseArgs(args []string, out io.Writer) (Options, error) {
	var o Options
	app := newApp(out, io.Discard, func(got Options) error {
		o = got
		return nil
	})
	return o, app.ExecuteArgs(args)
}

// unsetSettingsVariables removes the DASHI_* variables bound to flags.
func unsetSettingsVariables(t *testing.T) {
	t.Helper()
	for _, name := range settingsVariables {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

func TestParseArgs(t *testing.T) {
	unsetSettingsVariables(t)
	tests := []struct {
		args []string
		want Options
	}{
		{nil, Options{}},
		{[]string{"-c"}, Options{Resume: true}},
		{[]string{"--continue"}, Options{Resume: true}},
		{[]string{"-r", "abc"}, Options{Resume: true, ResumeID: "abc"}},
		{[]string{"--resume=abc", "--model", "m"}, Options{Resume: true, ResumeID: "abc", Model: "m"}},
		{[]string{"--model=gpt-6-luna"}, Options{Model: "gpt-6-luna"}},
		{[]string{"--inline"}, Options{Inline: true}},
		{[]string{"--plain", "--workspace", "w"}, Options{Plain: true, Workspace: "w"}},
		{[]string{"run", "hi there", "--yes"}, Options{Run: true, Prompt: "hi there", OutputFormat: "text", Yes: true}},
		{[]string{"run"}, Options{Run: true, OutputFormat: "text"}},
		{[]string{"run", "-c", "-o", "json"}, Options{Run: true, Resume: true, OutputFormat: "json"}},
		{[]string{"run", "--", "-5"}, Options{Run: true, Prompt: "-5", OutputFormat: "text"}},
		{[]string{"-c", "run", "go"}, Options{Run: true, Prompt: "go", Resume: true, OutputFormat: "text"}},
		{[]string{"--dangerously-skip-permissions"}, Options{Yes: true}},
		{[]string{"run", "go", "--dangerously-skip-permissions"}, Options{Run: true, Prompt: "go", OutputFormat: "text", Yes: true}},
		{[]string{"--compaction-threshold", "50000"}, Options{CompactionThreshold: 50000}},
		{[]string{"--session-id", "0F8FAD5B-D9CB-469F-A165-70867728950E", "--turn-events", "t.jsonl"},
			Options{SessionID: "0f8fad5b-d9cb-469f-a165-70867728950e", TurnEvents: "t.jsonl"}},
		{[]string{"models"}, Options{Models: true}},
		{[]string{"models", "--available", "--workspace", "w", "--env-file=e"}, Options{Models: true, Available: true, Workspace: "w", EnvFiles: []string{"e"}}},
	}
	for _, tt := range tests {
		got, err := parseArgs(tt.args, io.Discard)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseArgs(%v)=%+v, %v; want %+v", tt.args, got, err, tt.want)
		}
	}
	for _, bad := range [][]string{
		{"--nope"}, {"stray"}, {"-r"}, {"--model"}, {"--plain", "--inline"},
		{"-c", "-r", "abc"}, {"--resume="}, {"run", "a", "b"}, {"run", "--plain"},
		{"run", "-o", "xml"}, {"--yes"}, {"--attach", "a.go"}, {"--output-format", "json"},
		{"--compaction-threshold", "0"}, {"--compaction-threshold", "lots"},
		{"--available"}, {"models", "extra"}, {"models", "--model", "m"}, {"models", "-c"},
		{"--session-id", "abc"}, {"--session-id", "0f8fad5b-d9cb-469f-a165-70867728950"},
		{"--session-id", "0f8fad5b-d9cb-469f-a165-70867728950e", "-c"},
		{"--session-id", "0f8fad5b-d9cb-469f-a165-70867728950e", "-r", "abc"},
		{"--turn-events="}, {"run", "--session-id", "0f8fad5b-d9cb-469f-a165-70867728950e"},
		{"run", "--turn-events", "t.jsonl"},
	} {
		if _, err := parseArgs(bad, io.Discard); err == nil {
			t.Errorf("parseArgs(%v) succeeded", bad)
		}
	}
}

// A run prompt that names a command stays the prompt.
func TestParseArgsPromptNamedLikeCommand(t *testing.T) {
	for _, prompt := range []string{"help", "models", "version", "run"} {
		got, err := parseArgs([]string{"run", prompt}, io.Discard)
		if err != nil || got.Prompt != prompt || !got.Run {
			t.Errorf("run %s: %+v, %v", prompt, got, err)
		}
	}
}

func TestParseArgsShowThinking(t *testing.T) {
	unsetSettingsVariables(t)
	tests := []struct {
		args []string
		want preferenceField[bool]
	}{
		{nil, preferenceField[bool]{}},
		{[]string{"--show-thinking"}, preferenceField[bool]{Set: true, Value: true}},
		{[]string{"--show-thinking=true"}, preferenceField[bool]{Set: true, Value: true}},
		{[]string{"--show-thinking=false"}, preferenceField[bool]{Set: true}},
	}
	for _, tt := range tests {
		got, err := parseArgs(tt.args, io.Discard)
		if err != nil || got.Controls.ShowThinking != tt.want {
			t.Errorf("parseArgs(%v)=%+v, %v", tt.args, got.Controls.ShowThinking, err)
		}
	}
	if _, err := parseArgs([]string{"--show-thinking=maybe"}, io.Discard); err == nil {
		t.Error("--show-thinking=maybe accepted")
	}
}

func TestParseArgsControls(t *testing.T) {
	unsetSettingsVariables(t)
	got, err := parseArgs([]string{"--thinking-effort", "omit", "--temperature=omit", "--max-tokens", "default"}, io.Discard)
	want := generationPreferences{
		Effort:      preferenceField[string]{Set: true},
		Temperature: preferenceField[float64]{Set: true, Null: true},
		MaxTokens:   preferenceField[int]{Set: true},
	}
	if err != nil || got.Controls != want {
		t.Fatalf("got %+v, %v", got.Controls, err)
	}
	got, err = parseArgs([]string{"run", "--thinking-effort=high", "--temperature", "0.5", "--max-tokens=800"}, io.Discard)
	want = generationPreferences{
		Effort:      preferenceField[string]{Set: true, Value: "high"},
		Temperature: preferenceField[float64]{Set: true, Value: 0.5},
		MaxTokens:   preferenceField[int]{Set: true, Value: 800},
	}
	if err != nil || got.Controls != want {
		t.Fatalf("got %+v, %v", got.Controls, err)
	}
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"--temperature", "hot"}, "invalid value for --temperature: use a number of 0 or more, or omit"},
		{[]string{"--temperature=-1"}, "invalid value for --temperature: use a number of 0 or more, or omit"},
		{[]string{"--max-tokens", "many"}, "invalid value for --max-tokens: use a whole number of 0 or more, or default"},
		{[]string{"--max-tokens=-5"}, "invalid value for --max-tokens: use a whole number of 0 or more, or default"},
		{[]string{"--thinking-effort", " high"}, "invalid value for --thinking-effort: use a level, such as high, or omit"},
	} {
		if _, err := parseArgs(tt.args, io.Discard); err == nil || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("parseArgs(%v) error %v; want %q", tt.args, err, tt.want)
		}
	}
}

// The DASHI_* variables fill the flags they are bound to, and a flag wins
// over its variable.
func TestParseArgsSettingsVariables(t *testing.T) {
	unsetSettingsVariables(t)
	t.Setenv("DASHI_PROVIDER", "local")
	t.Setenv("DASHI_MODEL", "from-variable")
	t.Setenv("DASHI_TEMPERATURE", "0.3")
	t.Setenv("DASHI_SHOW_THINKING", "false")
	got, err := parseArgs([]string{"run", "--model", "from-flag"}, io.Discard)
	want := generationPreferences{
		ShowThinking: preferenceField[bool]{Set: true},
		Temperature:  preferenceField[float64]{Set: true, Value: 0.3},
	}
	if err != nil || got.Provider != "local" || got.Model != "from-flag" || got.Controls != want {
		t.Fatalf("got %+v, %v", got, err)
	}
	// An empty variable sets nothing, so a saved choice still applies.
	t.Setenv("DASHI_TEMPERATURE", "")
	t.Setenv("DASHI_MODEL", "")
	t.Setenv("DASHI_SHOW_THINKING", "")
	if got, err = parseArgs(nil, io.Discard); err != nil || got.Controls.Temperature.Set || got.Controls.ShowThinking.Set || got.Model != "" {
		t.Fatalf("got %+v, %v", got, err)
	}
	// A flag still turns thinking summaries off.
	t.Setenv("DASHI_SHOW_THINKING", "")
	if got, err = parseArgs([]string{"--show-thinking=false"}, io.Discard); err != nil || got.Controls.ShowThinking != (preferenceField[bool]{Set: true}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	// A variable goes through its flag's validator, and the error names both.
	t.Setenv("DASHI_TEMPERATURE", "hot")
	if _, err = parseArgs(nil, io.Discard); err == nil || !strings.Contains(err.Error(), "--temperature from environment variable DASHI_TEMPERATURE: use a number") {
		t.Fatalf("invalid variable: %v", err)
	}
	t.Setenv("DASHI_TEMPERATURE", "")
	t.Setenv("DASHI_COMPACTION_THRESHOLD", "-1")
	if _, err = parseArgs(nil, io.Discard); err == nil || !strings.Contains(err.Error(), "environment variable DASHI_COMPACTION_THRESHOLD: use a whole number") {
		t.Fatalf("invalid variable: %v", err)
	}
	t.Setenv("DASHI_COMPACTION_THRESHOLD", "")
	if _, err = parseArgs([]string{"models"}, io.Discard); err != nil {
		t.Fatalf("models read an agent variable: %v", err)
	}
}

func TestParseArgsRepeatableFlags(t *testing.T) {
	unsetSettingsVariables(t)
	got, err := parseArgs([]string{
		"run", "go",
		"--attach", "a.go", "--attach=b.go",
		"--env-file", "one.env", "--env-file", "two.env",
		"--mcp-config", "one.json", "--mcp-config=two.json",
	}, io.Discard)
	if err != nil ||
		!reflect.DeepEqual(got.AttachFiles, []string{"a.go", "b.go"}) ||
		!reflect.DeepEqual(got.EnvFiles, []string{"one.env", "two.env"}) ||
		!reflect.DeepEqual(got.MCPFiles, []string{"one.json", "two.json"}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, bad := range [][]string{{"run", "go", "--attach="}, {"run", "go", "--attach", " "}, {"--env-file="}, {"models", "--env-file", ""}, {"--mcp-config", ""}} {
		if _, err := parseArgs(bad, io.Discard); err == nil {
			t.Errorf("parseArgs(%v) succeeded", bad)
		}
	}
}

func TestHelp(t *testing.T) {
	var out bytes.Buffer
	o, err := parseArgs([]string{"--help"}, &out)
	if err == nil || !reflect.DeepEqual(o, Options{}) {
		t.Fatalf("o=%+v err=%v", o, err)
	}
	help := out.String()
	for _, want := range []string{
		"A terminal coding agent",
		"dashi [flags]", "run", "models",
		"-c, --continue", "-r, --resume",
		"--provider", "--model", "--api-endpoint", "--mcp-config",
		"--workspace", "--env-file", "--system-prompt", "--thinking-effort",
		"--show-thinking=false", "--temperature", "--max-tokens", "--plain", "--inline",
		"DASHI_PROVIDER, DASHI_MODEL", "~/.dashi/settings.json", "workspace .dashi/settings.json",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q:\n%s", want, help)
		}
	}
	out.Reset()
	if _, err := parseArgs([]string{"run", "--help"}, &out); err == nil || !strings.Contains(out.String(), "text|json|jsonl") || !strings.Contains(out.String(), "--attach") || strings.Contains(out.String(), "--plain") {
		t.Fatalf("run help: %v\n%s", err, out.String())
	}
	out.Reset()
	if _, err := parseArgs([]string{"models", "-h"}, &out); err == nil || !strings.Contains(out.String(), "--available") || strings.Contains(out.String(), "--model") {
		t.Fatalf("models help: %v\n%s", err, out.String())
	}
}

func TestRunCommandLineErrorsExitTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"--nope"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "dashi: unknown flag: --nope") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stderr.Reset()
	code = run(t.Context(), []string{"-h"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "--continue") || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

// DASHI_* controls and slash commands share the flag syntax.
func TestSetControl(t *testing.T) {
	var p generationPreferences
	for _, c := range [][2]string{{"--thinking-effort", "omit"}, {"--show-thinking", "false"}, {"--temperature", "0.2"}, {"--max-tokens", "default"}} {
		if err := p.setControl(c[0], c[1]); err != nil {
			t.Fatalf("%s=%s: %v", c[0], c[1], err)
		}
	}
	want := generationPreferences{
		Effort:       preferenceField[string]{Set: true},
		ShowThinking: preferenceField[bool]{Set: true},
		Temperature:  preferenceField[float64]{Set: true, Value: 0.2},
		MaxTokens:    preferenceField[int]{Set: true},
	}
	if p != want {
		t.Fatalf("got %+v", p)
	}
	for _, c := range [][2]string{{"--show-thinking", "yes"}, {"--temperature", "x"}, {"--max-tokens", "-1"}, {"--yes", "true"}} {
		var q generationPreferences
		if err := q.setControl(c[0], c[1]); err == nil {
			t.Errorf("%s=%s accepted", c[0], c[1])
		}
	}
}

// A release build sets version at link time, and --version prints it
// without the tag's "v".
func TestBuildVersionFromLinkTime(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })
	version = "v1.2.3"
	if got := buildVersion(); got != "1.2.3" {
		t.Fatalf("buildVersion() = %q, want 1.2.3", got)
	}
}
