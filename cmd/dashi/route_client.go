package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

const cliRouteRevision = "dashi-cli:route-v1:"

// routeClients owns immutable destination definitions for this invocation.
// The grant map contains whole-binding fingerprints, never credential values.
type routeClients struct {
	grantMu        sync.RWMutex
	registry, user map[string]configuredDestination
	grants         map[string]bool
	lookup         func(string) (string, bool)
	factory        func(configuredDestination, func(string) (string, bool), bool) (llm.Client, error)
}

func newRouteClients(registry, user map[string]configuredDestination, lookup func(string) (string, bool)) *routeClients {
	clone := func(m map[string]configuredDestination) map[string]configuredDestination {
		out := make(map[string]configuredDestination, len(m))
		for id, d := range m {
			d.Definition.Models = slices.Clone(d.Definition.Models)
			d.BuiltinAliases = slices.Clone(d.BuiltinAliases)
			out[id] = d
		}
		return out
	}
	return &routeClients{registry: clone(registry), user: clone(user), grants: map[string]bool{}, lookup: lookup, factory: newDestinationClient}
}
func (r *routeClients) clientFor(agent dashi.Agent) (llm.Client, error) {
	d, ok := r.registry[agent.Provider]
	if !ok {
		return nil, fmt.Errorf("recorded destination %q is no longer configured", agent.Provider)
	}
	fingerprint, err := d.fingerprint()
	if err != nil {
		return nil, err
	}
	if agent.API != d.Definition.API || !slices.Contains(d.Definition.Models, agent.Model) {
		return nil, errors.New("recorded destination API or model no longer matches its configuration")
	}
	if strings.HasPrefix(agent.Revision, cliRouteRevision) {
		if agent.Revision != cliRouteRevision+fingerprint {
			return nil, errors.New("recorded destination binding changed; restore its configuration before continuing")
		}
	} else {
		// The earlier CLI used only the public OpenAI Responses route. Its
		// automatic behavior revision cannot establish any custom binding.
		builtin := builtinDestinations()["openai"]
		if agent.ID != "dashi-cli" || !strings.HasPrefix(agent.Revision, "auto:") || agent.Provider != "openai" || d.Definition.API != builtin.Definition.API || d.Definition.Endpoint != builtin.Definition.Endpoint || len(d.BuiltinAliases) == 0 {
			return nil, errors.New("recorded turn has unknown destination provenance; its route cannot be reconstructed")
		}
	}
	return r.factory(d, r.lookup, d.trustedBy(r.user) || r.granted(fingerprint))
}
func selectionAgent(selection resolvedSelection, controls llm.Settings) (dashi.Agent, error) {
	fingerprint, err := selection.Destination.fingerprint()
	if err != nil {
		return dashi.Agent{}, err
	}
	return dashi.Agent{ID: "dashi-cli", Revision: cliRouteRevision + fingerprint, Provider: selection.Destination.ID, API: selection.Destination.Definition.API, Model: selection.Model, Settings: controls, NoInheritedSettings: true}, nil
}

// turnClient forwards preparation to the bound turn. Each frozen request keeps
// its original client even when the next turn selects another destination.
type turnClient struct {
	mu       sync.RWMutex
	client   llm.Client
	blocked  error
	prepared map[[sha256.Size]byte]llm.Client
}

func newTurnClient(client llm.Client) *turnClient {
	return &turnClient{client: client, prepared: make(map[[sha256.Size]byte]llm.Client)}
}
func (c *turnClient) bind(client llm.Client, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client, c.blocked = client, err
}
func (c *turnClient) current() (llm.Client, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.blocked != nil {
		return nil, c.blocked
	}
	if c.client == nil {
		return nil, errors.New("turn destination is not bound")
	}
	return c.client, nil
}
func (c *turnClient) Provider() string {
	client, err := c.current()
	if err != nil {
		return ""
	}
	return client.Provider()
}
func (c *turnClient) API() string {
	client, err := c.current()
	if err != nil {
		return ""
	}
	return client.API()
}
func (c *turnClient) ModelInfo(model string) (llm.ModelInfo, bool) {
	client, err := c.current()
	if err != nil {
		return llm.ModelInfo{}, false
	}
	return client.ModelInfo(model)
}
func (c *turnClient) Prepare(ctx context.Context, input llm.Input) (llm.Request, error) {
	client, err := c.current()
	if err != nil {
		return llm.Request{}, err
	}
	request, err := client.Prepare(ctx, input)
	if err != nil {
		return request, err
	}
	key, err := requestKey(request)
	if err != nil {
		return llm.Request{}, err
	}
	c.mu.Lock()
	c.prepared[key] = client
	c.mu.Unlock()
	return request, nil
}
func (c *turnClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	key, err := requestKey(call.Request)
	if err != nil {
		return llm.Result{}, err
	}
	c.mu.RLock()
	client := c.prepared[key]
	c.mu.RUnlock()
	if client == nil {
		return llm.Result{}, errors.New("request has no bound preparation client")
	}
	return client.Send(ctx, call)
}
func requestKey(request llm.Request) ([sha256.Size]byte, error) {
	raw, err := json.Marshal(request.Snapshot())
	return sha256.Sum256(raw), err
}

func (r *routeClients) granted(key string) bool {
	r.grantMu.RLock()
	defer r.grantMu.RUnlock()
	return r.grants[key]
}
func (r *routeClients) grant(key string) {
	r.grantMu.Lock()
	defer r.grantMu.Unlock()
	r.grants[key] = true
}
