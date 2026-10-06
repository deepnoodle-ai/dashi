// Package client supplies the Decide client a package uses when the host
// gives none.
package client

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/backend"
)

// fromEnvironment is made once per process, so every package without a
// client shares one.
var fromEnvironment = sync.OnceValues(func() (*decide.Client, error) {
	return fromLookup(os.Getenv)
})

// Default returns c, or when c is nil, a client made from the environment
// as the decide command reads it:
//
//	DECIDE_PROVIDER   "typesafe" (the default when empty) or "cloudflare"
//	typesafe          TYPESAFE_API_KEY, and the other TYPESAFE_* variables
//	                  that decide.NewClient reads, such as TYPESAFE_BASE_URL
//	cloudflare        CLOUDFLARE_AUTH_TOKEN, CLOUDFLARE_ACCOUNT_ID, and
//	                  CLOUDFLARE_BASE_URL if set
//
// Missing credentials give an error that wraps decide.ErrNoAPIKey and names
// the variables to set. Another DECIDE_PROVIDER value is an error that
// names the valid ones. The client is made once per process, and its error
// is returned on every call.
func Default(c *decide.Client) (*decide.Client, error) {
	if c != nil {
		return c, nil
	}
	return fromEnvironment()
}

// fromLookup makes a client from the variables getenv returns.
func fromLookup(getenv func(string) string) (*decide.Client, error) {
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	switch provider := get("DECIDE_PROVIDER"); provider {
	case "", string(backend.TypeSafe):
		if get("TYPESAFE_API_KEY") == "" {
			return nil, fmt.Errorf("%w; or set DECIDE_PROVIDER=cloudflare with "+
				"CLOUDFLARE_AUTH_TOKEN and CLOUDFLARE_ACCOUNT_ID", decide.ErrNoAPIKey)
		}
		// decide.NewClient reads TYPESAFE_API_KEY, TYPESAFE_BASE_URL, and
		// the other TYPESAFE_* settings itself.
		return decide.NewClient()
	case string(backend.Cloudflare):
		token, account := get("CLOUDFLARE_AUTH_TOKEN"), get("CLOUDFLARE_ACCOUNT_ID")
		if token == "" || account == "" {
			return nil, fmt.Errorf("%w: DECIDE_PROVIDER is cloudflare, so set "+
				"CLOUDFLARE_AUTH_TOKEN and CLOUDFLARE_ACCOUNT_ID", decide.ErrNoAPIKey)
		}
		return backend.NewClient(backend.Config{
			Provider:  backend.Cloudflare,
			APIKey:    token,
			AccountID: account,
			BaseURL:   get("CLOUDFLARE_BASE_URL"),
		})
	default:
		return nil, fmt.Errorf("%w: DECIDE_PROVIDER is %q; use typesafe or cloudflare",
			errInvalidProvider, provider)
	}
}

var errInvalidProvider = errors.New("decide: unknown provider")
