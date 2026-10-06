package defaultclient

import (
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/decide"
)

func TestFromLookupChoosesTheProvider(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(name string) string { return vars[name] }
	}
	t.Run("typesafe without a key", func(t *testing.T) {
		s, err := fromLookup(env(nil))
		if !errors.Is(err, ErrSetup) || !errors.Is(err, decide.ErrNoAPIKey) ||
			!strings.Contains(err.Error(), "TYPESAFE_API_KEY") ||
			!strings.Contains(err.Error(), "CLOUDFLARE_AUTH_TOKEN") {
			t.Fatalf("err = %v; want ErrSetup and ErrNoAPIKey naming both providers' variables", err)
		}
		if strings.Contains(err.Error(), "WithAPIKey") {
			t.Fatalf("err = %v; names a Go option a person cannot set", err)
		}
		if s.Provider != ProviderTypeSafe || s.Client != nil {
			t.Fatalf("setup = %+v", s)
		}
	})
	t.Run("typesafe with a key", func(t *testing.T) {
		t.Setenv("TYPESAFE_API_KEY", "k")
		s, err := fromLookup(env(map[string]string{"DECIDE_PROVIDER": "typesafe", "TYPESAFE_API_KEY": "k"}))
		if err != nil || s.Client == nil || s.Provider != "typesafe" {
			t.Fatalf("fromLookup = %+v, %v", s, err)
		}
	})
	t.Run("cloudflare without an account", func(t *testing.T) {
		s, err := fromLookup(env(map[string]string{"DECIDE_PROVIDER": "cloudflare", "CLOUDFLARE_AUTH_TOKEN": "t"}))
		if !errors.Is(err, ErrSetup) || !errors.Is(err, decide.ErrNoAPIKey) ||
			!strings.Contains(err.Error(), "CLOUDFLARE_ACCOUNT_ID") || s.Provider != "cloudflare" {
			t.Fatalf("fromLookup = %+v, %v; want ErrSetup naming CLOUDFLARE_ACCOUNT_ID", s, err)
		}
	})
	t.Run("cloudflare", func(t *testing.T) {
		s, err := fromLookup(env(map[string]string{
			"DECIDE_PROVIDER": "cloudflare", "CLOUDFLARE_AUTH_TOKEN": "t", "CLOUDFLARE_ACCOUNT_ID": "a",
		}))
		if err != nil || s.Client == nil || s.Provider != "cloudflare" {
			t.Fatalf("fromLookup = %+v, %v", s, err)
		}
		if got := s.Client.DefaultModel(); got != "clef" {
			t.Fatalf("DefaultModel = %q; want clef", got)
		}
	})
	t.Run("unknown provider", func(t *testing.T) {
		s, err := fromLookup(env(map[string]string{"DECIDE_PROVIDER": "cloudfare"}))
		if !errors.Is(err, ErrUnknownProvider) || !errors.Is(err, ErrSetup) || errors.Is(err, decide.ErrNoAPIKey) ||
			!strings.Contains(err.Error(), `"cloudfare"`) || !strings.Contains(err.Error(), "typesafe or cloudflare") {
			t.Fatalf("err = %v; want ErrUnknownProvider naming the value and the valid providers", err)
		}
		if s.Provider != "cloudfare" || s.Client != nil {
			t.Fatalf("setup = %+v", s)
		}
	})
}
