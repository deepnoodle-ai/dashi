// Command model-watch compares each provider's live model list with Dashi's
// catalog, so new and vanished models show up in one command. It is a
// report, not a gate: it exits 1 only when a provider fails to list, or when
// --fail-on-new is set and a new text model is listed.
package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/wonton/cli"
	"github.com/deepnoodle-ai/wonton/env"
)

func main() {
	app := newApp(providers())
	if err := app.Execute(); err != nil {
		app.PrintError(err)
		os.Exit(cli.GetExitCode(err))
	}
}

// defaultEnvFiles are tried, when present, if --env-file is not given: the
// current directory, then the repository root as seen from this folder.
var defaultEnvFiles = []string{".env", "../../.env"}

func newApp(specs []providerSpec) *cli.App {
	app := cli.New("model-watch").
		Description("Compare provider model lists with Dashi's catalog")
	app.Main().
		Flags(
			cli.String("provider", "p").
				Help("comma-separated providers to check (default all: "+specNames(specs)+")"),
			cli.String("env-file", "").
				Help("load API keys from this .env file (default: ./.env and ../../.env, when present; the first to set a variable wins)"),
			cli.Bool("json").Help("print the reports as JSON"),
			cli.Bool("no-verify").Help("skip checking unlisted Anthropic catalog IDs one by one"),
			cli.Bool("fail-on-new").Help("exit 1 when any provider lists a new text model"),
		).
		Run(func(ctx *cli.Context) error {
			if err := loadEnv(ctx.String("env-file")); err != nil {
				return err
			}
			selected, err := selectProviders(specs, ctx.String("provider"))
			if err != nil {
				return err
			}
			ignore, err := parseIgnore(ignoreJSON)
			if err != nil {
				return err
			}
			reports := run(selected, ignore, !ctx.Bool("no-verify"))
			if ctx.Bool("json") {
				if err := printJSON(ctx.Stdout(), reports); err != nil {
					return err
				}
			} else if err := printText(ctx.Stdout(), reports); err != nil {
				return err
			}
			return exitStatus(reports, ctx.Bool("fail-on-new"))
		})
	return app
}

// exitStatus returns an exit-1 error naming the cause when a provider failed
// to list, or when failOnNew is set and a provider lists a new model.
func exitStatus(reports []report, failOnNew bool) error {
	var failed, withNew []string
	for _, r := range reports {
		if r.Error != "" {
			failed = append(failed, r.Provider)
		}
		if len(r.New) > 0 {
			withNew = append(withNew, fmt.Sprintf("%s (%s)", r.Provider, strings.Join(r.New, ", ")))
		}
	}
	switch {
	case len(failed) > 0:
		return &cli.ExitError{Code: 1, Message: "listing failed: " + strings.Join(failed, ", ")}
	case failOnNew && len(withNew) > 0:
		return &cli.ExitError{Code: 1, Message: "new models: " + strings.Join(withNew, "; ")}
	}
	return nil
}

func specNames(specs []providerSpec) string {
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
	}
	return strings.Join(names, ",")
}

// loadEnv loads keys from .env files without overriding variables already
// set. An explicit file must exist. Every default file present is loaded, so
// the first one that sets a variable wins.
func loadEnv(path string) error {
	if path != "" {
		return env.LoadEnvFile(path)
	}
	for _, p := range defaultEnvFiles {
		if _, err := os.Stat(p); err == nil {
			if err := env.LoadEnvFile(p); err != nil {
				return err
			}
		}
	}
	return nil
}

func selectProviders(specs []providerSpec, list string) ([]providerSpec, error) {
	if strings.TrimSpace(list) == "" {
		return specs, nil
	}
	var out []providerSpec
	for name := range strings.SplitSeq(list, ",") {
		name = strings.TrimSpace(name)
		i := slices.IndexFunc(specs, func(s providerSpec) bool { return s.Name == name })
		if i < 0 {
			return nil, fmt.Errorf("unknown provider %q (known: %s)", name, specNames(specs))
		}
		out = append(out, specs[i])
	}
	return out, nil
}

// run checks the providers in parallel and returns their reports in order.
func run(specs []providerSpec, ignore ignoreSet, verify bool) []report {
	reports := make([]report, len(specs))
	var wg sync.WaitGroup
	for i, p := range specs {
		wg.Go(func() { reports[i] = check(p, ignore[p.Name], verify) })
	}
	wg.Wait()
	return reports
}

func check(p providerSpec, ignore map[string]bool, verify bool) report {
	key := p.key()
	if key == "" {
		return report{Provider: p.Name, Skipped: strings.Join(p.KeyEnv, " or ") + " not set"}
	}
	c := newClient(p, key)
	listed, err := p.List(c)
	if err != nil {
		return report{Provider: p.Name, Error: err.Error()}
	}
	var cat []catalogModel
	for _, e := range catalog.Entries(p.Name) {
		cat = append(cat, catalogModel{ID: e.Model, Snapshot: e.Snapshot})
	}
	var exists func(string) (bool, error)
	if verify && p.Exists != nil {
		exists = func(id string) (bool, error) { return p.Exists(c, id) }
	}
	return compare(p.Name, listed, cat, ignore, exists)
}

// errWriter remembers the first write error so printing code stays short.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err == nil {
		_, e.err = fmt.Fprintf(e.w, format, args...)
	}
}
