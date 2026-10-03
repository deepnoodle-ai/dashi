// Package client supplies the Decide client a package uses when the host
// gives none.
package client

import (
	"sync"

	"github.com/deepnoodle-ai/decide"
)

// fromEnvironment is made once per process, so every package without a
// client shares one.
var fromEnvironment = sync.OnceValues(func() (*decide.Client, error) {
	return decide.NewClient()
})

// Default returns c, or when c is nil, a client that decide.NewClient makes
// from the environment, such as TYPESAFE_API_KEY. That client is made once
// per process, and its error, such as decide.ErrNoAPIKey, is returned on
// every call.
func Default(c *decide.Client) (*decide.Client, error) {
	if c != nil {
		return c, nil
	}
	return fromEnvironment()
}
