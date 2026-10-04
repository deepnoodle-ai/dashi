package main

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		args []string
		want Options
	}{
		{nil, Options{}},
		{[]string{"-c"}, Options{Resume: true}},
		{[]string{"--resume"}, Options{Resume: true}},
		{[]string{"--resume", "abc"}, Options{Resume: true, ResumeID: "abc"}},
		{[]string{"--resume=abc"}, Options{Resume: true, ResumeID: "abc"}},
		{[]string{"--resume", "--model", "m"}, Options{Resume: true, Model: "m"}},
		{[]string{"-p", "hi there", "--yes"}, Options{Print: "hi there", PrintSet: true, Yes: true}},
		{[]string{"-p=hi", "--print", "-5"}, Options{Print: "-5", PrintSet: true}},
		{[]string{"-p=a=b"}, Options{Print: "a=b", PrintSet: true}},
		{[]string{"--model=gpt-6-luna"}, Options{Model: "gpt-6-luna"}},
		{[]string{"--plain", "-h"}, Options{Help: true}},
		{[]string{"--help"}, Options{Help: true}},
		{[]string{"--inline"}, Options{Inline: true}},
	}
	for _, tt := range tests {
		got, err := parseArgs(tt.args, io.Discard)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseArgs(%v)=%+v, %v; want %+v", tt.args, got, err, tt.want)
		}
	}
	for _, bad := range [][]string{{"--nope"}, {"-p"}, {"--model"}, {"--plain", "--inline"}, {"stray"}} {
		if _, err := parseArgs(bad, io.Discard); err == nil {
			t.Errorf("parseArgs(%v) succeeded", bad)
		}
	}
}

// --resume takes an optional session ID: the next argument, unless it is a
// flag. -c never takes one.
func TestParseArgsResumeOptionalID(t *testing.T) {
	tests := []struct {
		args []string
		id   string
	}{
		{[]string{"--resume", "-p", "go"}, ""},
		{[]string{"-p", "go", "--resume"}, ""},
		{[]string{"--resume=", "-p", "go"}, ""},
		{[]string{"--resume", "models"}, "models"},
		{[]string{"-c", "-p", "go"}, ""},
	}
	for _, tt := range tests {
		got, err := parseArgs(tt.args, io.Discard)
		if err != nil || !got.Resume || got.ResumeID != tt.id {
			t.Errorf("parseArgs(%v)=%+v, %v; want resume %q", tt.args, got, err, tt.id)
		}
	}
	if _, err := parseArgs([]string{"-c", "abc"}, io.Discard); err == nil {
		t.Error("-c accepted a session ID")
	}
}

// A flag value that names a command stays a value.
func TestParseArgsValueNamedLikeCommand(t *testing.T) {
	for _, prompt := range []string{"help", "models", "version"} {
		got, err := parseArgs([]string{"-p", prompt}, io.Discard)
		if err != nil || got.Print != prompt || got.Help || got.Models {
			t.Errorf("-p %s: %+v, %v", prompt, got, err)
		}
	}
}

func TestParseArgsShowThinking(t *testing.T) {
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
	got, err := parseArgs([]string{"--thinking-effort", "omit", "--temperature=omit", "--max-tokens", "default"}, io.Discard)
	want := generationPreferences{
		Effort:      preferenceField[string]{Set: true},
		Temperature: preferenceField[float64]{Set: true, Null: true},
		MaxTokens:   preferenceField[int]{Set: true},
	}
	if err != nil || got.Controls != want {
		t.Fatalf("got %+v, %v", got.Controls, err)
	}
	got, err = parseArgs([]string{"--thinking-effort=high", "--temperature", "0.5", "--max-tokens=800"}, io.Discard)
	want = generationPreferences{
		Effort:      preferenceField[string]{Set: true, Value: "high"},
		Temperature: preferenceField[float64]{Set: true, Value: 0.5},
		MaxTokens:   preferenceField[int]{Set: true, Value: 800},
	}
	if err != nil || got.Controls != want {
		t.Fatalf("got %+v, %v", got.Controls, err)
	}
	for _, bad := range [][]string{{"--temperature", "hot"}, {"--temperature=-1"}, {"--max-tokens", "many"}, {"--max-tokens=-5"}, {"--thinking-effort", " high"}} {
		if _, err := parseArgs(bad, io.Discard); err == nil {
			t.Errorf("parseArgs(%v) succeeded", bad)
		}
	}
}

func TestParseArgsRepeatableFlags(t *testing.T) {
	got, err := parseArgs([]string{
		"-p", "go",
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
	for _, bad := range [][]string{{"-p", "go", "--attach="}, {"-p", "go", "--attach", " "}, {"--env-file="}, {"--mcp-config", ""}} {
		if _, err := parseArgs(bad, io.Discard); err == nil {
			t.Errorf("parseArgs(%v) succeeded", bad)
		}
	}
}

func TestParseArgsModels(t *testing.T) {
	tests := []struct {
		args []string
		want Options
	}{
		{[]string{"models"}, Options{Models: true}},
		{[]string{"models", "--available"}, Options{Models: true, Available: true}},
		{[]string{"models", "--available", "--workspace", "w", "--env-file=e"}, Options{Models: true, Available: true, Workspace: "w", EnvFiles: []string{"e"}}},
		{[]string{"models", "-h"}, Options{Help: true}},
	}
	for _, tt := range tests {
		got, err := parseArgs(tt.args, io.Discard)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseArgs(%v)=%+v, %v; want %+v", tt.args, got, err, tt.want)
		}
	}
	for _, bad := range [][]string{{"--available"}, {"models", "--plain"}, {"models", "--output-format", "json"}, {"models", "extra"}, {"models", "--workspace="}} {
		if _, err := parseArgs(bad, io.Discard); err == nil {
			t.Errorf("parseArgs(%v) succeeded", bad)
		}
	}
}

func TestParseArgsCrossFlagErrors(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"--output-format", "json", "--plain"}, "--output-format cannot be combined with --plain, --inline, or models"},
		{[]string{"--output-format", "text", "--inline"}, "--output-format cannot be combined with --plain, --inline, or models"},
		{[]string{"--attach", "a.go"}, "--attach requires -p; use /attach or @file interactively"},
		{[]string{"--plain", "--inline"}, "--plain and --inline cannot be combined"},
		{[]string{"--model", " "}, "--model needs a nonempty exact model ID"},
		{[]string{"--provider="}, "--provider needs a nonempty value"},
		{[]string{"--system-prompt", ""}, "--system-prompt needs nonempty text"},
		{[]string{"--temperature", "hot"}, "invalid --temperature"},
		{[]string{"--max-tokens=-5"}, "invalid max_tokens"},
	}
	for _, tt := range tests {
		_, err := parseArgs(tt.args, io.Discard)
		if err == nil || !strings.HasPrefix(err.Error(), tt.want+"\n\nRun 'dashi --help'") {
			t.Errorf("parseArgs(%v) error %v; want %q", tt.args, err, tt.want)
		}
	}
	// --attach is allowed with --output-format, which also runs one request.
	if _, err := parseArgs([]string{"--output-format", "json", "--attach", "a.go"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestHelpCoversFlagsAndSettings(t *testing.T) {
	var out bytes.Buffer
	o, err := parseArgs([]string{"--help"}, &out)
	if err != nil || !o.Help {
		t.Fatalf("o=%+v err=%v", o, err)
	}
	help := out.String()
	for _, want := range []string{
		"A terminal coding agent.",
		"dashi [flags]",
		"models",
		"-p, --print", "PROMPT",
		"--output-format", "text|json|jsonl",
		"-c, --resume", "session ID",
		"--provider", "--model", "--api-endpoint", "--mcp-config", "--attach",
		"--workspace", "--env-file", "--system-prompt", "--thinking-effort",
		"--show-thinking=false", "--temperature", "--max-tokens", "--yes", "--plain", "--inline",
		"DASHI_PROVIDER, DASHI_MODEL", "~/.dashi/settings.json", "workspace .dashi/settings.json",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q:\n%s", want, help)
		}
	}
	out.Reset()
	if _, err := parseArgs([]string{"models", "--help"}, &out); err != nil || !strings.Contains(out.String(), "--available") || strings.Contains(out.String(), "--print") {
		t.Fatalf("models help: %v\n%s", err, out.String())
	}
}

func TestRunFlagErrorsExitTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"--nope"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "dashi: unknown flag: --nope") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stderr.Reset()
	code = run(t.Context(), []string{"-h"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "--print") || stderr.Len() != 0 {
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
