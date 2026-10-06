package client

import (
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/decide"
)

func TestDefault(t *testing.T) {
	given, err := decide.NewClient(decide.WithAPIKey("given"), decide.WithoutEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Default(given); got != given || err != nil {
		t.Fatalf("Default(given) = %v, %v; want given", got, err)
	}
	t.Setenv("TYPESAFE_API_KEY", "from-environment")
	first, err := Default(nil)
	if err != nil || first == nil {
		t.Fatalf("Default(nil) = %v, %v", first, err)
	}
	if again, _ := Default(nil); again != first {
		t.Fatal("Default(nil) made a second client")
	}
}

func TestFromLookupChoosesTheProvider(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(name string) string { return vars[name] }
	}
	t.Run("typesafe without a key", func(t *testing.T) {
		_, err := fromLookup(env(nil))
		if !errors.Is(err, decide.ErrNoAPIKey) || !strings.Contains(err.Error(), "TYPESAFE_API_KEY") ||
			!strings.Contains(err.Error(), "CLOUDFLARE_AUTH_TOKEN") {
			t.Fatalf("err = %v; want ErrNoAPIKey naming both providers' variables", err)
		}
	})
	t.Run("typesafe with a key", func(t *testing.T) {
		t.Setenv("TYPESAFE_API_KEY", "k")
		c, err := fromLookup(env(map[string]string{"DECIDE_PROVIDER": "typesafe", "TYPESAFE_API_KEY": "k"}))
		if err != nil || c == nil {
			t.Fatalf("fromLookup = %v, %v", c, err)
		}
	})
	t.Run("cloudflare without an account", func(t *testing.T) {
		_, err := fromLookup(env(map[string]string{"DECIDE_PROVIDER": "cloudflare", "CLOUDFLARE_AUTH_TOKEN": "t"}))
		if !errors.Is(err, decide.ErrNoAPIKey) || !strings.Contains(err.Error(), "CLOUDFLARE_ACCOUNT_ID") {
			t.Fatalf("err = %v; want ErrNoAPIKey naming CLOUDFLARE_ACCOUNT_ID", err)
		}
	})
	t.Run("cloudflare", func(t *testing.T) {
		c, err := fromLookup(env(map[string]string{
			"DECIDE_PROVIDER": "cloudflare", "CLOUDFLARE_AUTH_TOKEN": "t", "CLOUDFLARE_ACCOUNT_ID": "a",
		}))
		if err != nil || c == nil {
			t.Fatalf("fromLookup = %v, %v", c, err)
		}
		if got := c.DefaultModel(); got != "clef" {
			t.Fatalf("DefaultModel = %q; want clef", got)
		}
	})
	t.Run("unknown provider", func(t *testing.T) {
		_, err := fromLookup(env(map[string]string{"DECIDE_PROVIDER": "openai"}))
		if err == nil || errors.Is(err, decide.ErrNoAPIKey) || !strings.Contains(err.Error(), "typesafe or cloudflare") {
			t.Fatalf("err = %v; want an error naming the valid providers", err)
		}
	})
}
