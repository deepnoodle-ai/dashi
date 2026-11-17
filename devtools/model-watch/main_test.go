package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/cli"
)

// fakeSpecs returns grok and meta specs against canned servers. Grok lists
// one new model; meta's key is left unset so it is skipped.
func fakeSpecs(t *testing.T) []providerSpec {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"grok-4.7"},{"id":"grok-4.6"},{"id":"grok-4.5"},{"id":"grok-4.3"},
			{"id":"grok-4.20-0309-reasoning"},{"id":"grok-4.20-0309-non-reasoning"},
			{"id":"grok-4.20-multi-agent-0309"},{"id":"grok-build-0.1"},
			{"id":"grok-9"},{"id":"grok-imagine-video"}]}`)
	}))
	t.Cleanup(srv.Close)
	return []providerSpec{
		{Name: "grok", KeyEnv: []string{"TEST_GROK_KEY"}, BaseURL: srv.URL, List: listOpenAIStyle},
		{Name: "meta", KeyEnv: []string{"TEST_META_KEY"}, BaseURL: srv.URL, List: listOpenAIStyle},
	}
}

// runCLI runs model-watch with an empty .env file, so a real one nearby is
// never loaded.
func runCLI(t *testing.T, args ...string) *cli.TestResult {
	t.Helper()
	empty := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_GROK_KEY", "k")
	args = append([]string{"--env-file", empty}, args...)
	return newApp(fakeSpecs(t)).Test(t, cli.TestArgs(args...))
}

func TestTextReport(t *testing.T) {
	r := runCLI(t)
	if r.ExitCode != 0 {
		t.Fatalf("exit %d: %v %s", r.ExitCode, r.Err, r.Stderr)
	}
	for _, want := range []string{
		"grok: 10 listed, 8 in catalog\n",
		"  new: listed, not in catalog (1):\n    grok-9\n",
		"  media and other (1): grok-imagine-video\n",
		"meta: skipped, TEST_META_KEY not set\n",
	} {
		if !r.Contains(want) {
			t.Errorf("output lacks %q:\n%s", want, r.Stdout)
		}
	}
}

func TestJSONReport(t *testing.T) {
	r := runCLI(t, "--json", "--provider", "grok")
	if r.ExitCode != 0 {
		t.Fatalf("exit %d: %v", r.ExitCode, r.Err)
	}
	var reports []report
	if err := json.Unmarshal([]byte(r.Stdout), &reports); err != nil {
		t.Fatalf("%v:\n%s", err, r.Stdout)
	}
	if len(reports) != 1 || reports[0].Provider != "grok" || len(reports[0].New) != 1 || len(reports[0].Missing) != 0 {
		t.Errorf("reports = %+v", reports)
	}
}

func TestFailOnNew(t *testing.T) {
	r := runCLI(t, "--fail-on-new")
	if r.ExitCode != 1 || r.Err == nil || !strings.Contains(r.Err.Error(), "grok (grok-9)") {
		t.Errorf("exit %d, err %v", r.ExitCode, r.Err)
	}
}

func TestListingErrorExitsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TEST_GROK_KEY", "k")
	empty := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	specs := []providerSpec{{Name: "grok", KeyEnv: []string{"TEST_GROK_KEY"}, BaseURL: srv.URL, List: listOpenAIStyle}}
	r := newApp(specs).Test(t, cli.TestArgs("--env-file", empty, "--fail-on-new"))
	if r.ExitCode != 1 || r.Err == nil || !strings.Contains(r.Err.Error(), "listing failed: grok") {
		t.Errorf("exit %d, err %v", r.ExitCode, r.Err)
	}
}

func TestUnknownProvider(t *testing.T) {
	if r := runCLI(t, "--provider", "nope"); r.ExitCode == 0 {
		t.Error("an unknown provider should fail")
	}
}
