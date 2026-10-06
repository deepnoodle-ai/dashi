// Package defaultclient makes the Decide client that every decide package
// uses when the host gives none, from environment variables.
//
// A host calls Load at startup to learn, before any check runs and without
// a request, which provider the checks will use and whether the
// environment is set up for it.
package defaultclient

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/backend"
)

// The providers DECIDE_PROVIDER can name.
const (
	ProviderTypeSafe   = string(backend.TypeSafe)
	ProviderCloudflare = string(backend.Cloudflare)
)

// Setup is the default client and the provider it uses.
type Setup struct {
	// Provider is ProviderTypeSafe or ProviderCloudflare. With
	// ErrUnknownProvider it is the DECIDE_PROVIDER value.
	Provider string
	// Client is nil when Load returns an error.
	Client *decide.Client
}

// ErrSetup marks an environment that cannot make the default client:
// missing credentials or an unknown DECIDE_PROVIDER. No request was sent,
// and none will succeed until the variables change and the process
// restarts. Missing credentials also match decide.ErrNoAPIKey.
var ErrSetup = errors.New("decide: not set up")

// ErrUnknownProvider is the ErrSetup for a DECIDE_PROVIDER value that names
// no provider.
var ErrUnknownProvider = fmt.Errorf("%w: unknown DECIDE_PROVIDER", ErrSetup)

// load is called once per process, so every package without a client
// shares one.
var load = sync.OnceValues(func() (Setup, error) {
	return fromLookup(os.Getenv)
})

// Load returns the default client, made from the environment as the
// decide command reads it:
//
//	DECIDE_PROVIDER   "typesafe" (the default when empty) or "cloudflare"
//	typesafe          TYPESAFE_API_KEY, and the other TYPESAFE_* variables
//	                  that decide.NewClient reads, such as TYPESAFE_BASE_URL
//	cloudflare        CLOUDFLARE_AUTH_TOKEN, CLOUDFLARE_ACCOUNT_ID, and
//	                  CLOUDFLARE_BASE_URL if set
//
// It sends no request. The client is made once per process, on the first
// call, so a host that loads variables from a file loads them first. An
// error wraps ErrSetup and says which variables to set.
func Load() (Setup, error) {
	return load()
}

// setupError is an ErrSetup with its own text, so a host can show it to a
// person as is.
type setupError struct {
	text  string
	kinds []error
}

func (e *setupError) Error() string   { return e.text }
func (e *setupError) Unwrap() []error { return e.kinds }

// noCredentials is the error for missing credentials.
func noCredentials(text string) error {
	return &setupError{text: "decide: no credentials: " + text, kinds: []error{ErrSetup, decide.ErrNoAPIKey}}
}

// fromLookup makes the client from the variables getenv returns.
func fromLookup(getenv func(string) string) (Setup, error) {
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	provider := get("DECIDE_PROVIDER")
	switch provider {
	case "", ProviderTypeSafe:
		s := Setup{Provider: ProviderTypeSafe}
		if get("TYPESAFE_API_KEY") == "" {
			return s, noCredentials("set TYPESAFE_API_KEY, or set DECIDE_PROVIDER=cloudflare " +
				"with CLOUDFLARE_AUTH_TOKEN and CLOUDFLARE_ACCOUNT_ID")
		}
		// decide.NewClient reads TYPESAFE_API_KEY, TYPESAFE_BASE_URL, and
		// the other TYPESAFE_* settings itself.
		c, err := decide.NewClient()
		if err != nil {
			return s, fmt.Errorf("%w: %w", ErrSetup, err)
		}
		s.Client = c
		return s, nil
	case ProviderCloudflare:
		s := Setup{Provider: ProviderCloudflare}
		token, account := get("CLOUDFLARE_AUTH_TOKEN"), get("CLOUDFLARE_ACCOUNT_ID")
		if token == "" || account == "" {
			return s, noCredentials("DECIDE_PROVIDER is cloudflare, so set " +
				"CLOUDFLARE_AUTH_TOKEN and CLOUDFLARE_ACCOUNT_ID")
		}
		c, err := backend.NewClient(backend.Config{
			Provider:  backend.Cloudflare,
			APIKey:    token,
			AccountID: account,
			BaseURL:   get("CLOUDFLARE_BASE_URL"),
		})
		if err != nil {
			return s, fmt.Errorf("%w: %w", ErrSetup, err)
		}
		s.Client = c
		return s, nil
	}
	return Setup{Provider: provider}, &setupError{
		text:  fmt.Sprintf("decide: unknown DECIDE_PROVIDER %q: use typesafe or cloudflare", provider),
		kinds: []error{ErrUnknownProvider},
	}
}
