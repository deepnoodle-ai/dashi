// Package client supplies the Decide client a package uses when the host
// gives none.
package client

import (
	"github.com/deepnoodle-ai/dashi/decide/defaultclient"
	"github.com/deepnoodle-ai/decide"
)

// Default returns c, or when c is nil, the client defaultclient.Load makes
// from the environment. That client is made once per process, and its
// error, which wraps defaultclient.ErrSetup, is returned on every call.
func Default(c *decide.Client) (*decide.Client, error) {
	if c != nil {
		return c, nil
	}
	s, err := defaultclient.Load()
	return s.Client, err
}
