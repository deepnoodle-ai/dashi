// Package catalog holds documented knowledge about models served at public
// provider endpoints: capabilities, limits, and list prices. Each entry is one
// exact model ID; nothing is matched by prefix or inherited from a sibling.
//
// The data is embedded JSON, one file per provider, in the shapes of
// llm.Capabilities and llm.Pricing. It was converted from Dive's provider
// catalogs (65c393f), Apache-2.0, and is maintained by hand. The package is
// data plus lookup: it holds no mutable state, registers nothing, and never
// estimates or aggregates cost. See design/model-catalog.md.
package catalog

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Owner identifies the catalog as the owner of the evidence it binds, so a
// recorded fact distinguishes catalog claims from application claims.
const Owner = "dashi/catalog"

//go:embed *.json
var files embed.FS

// Lifecycle records a provider's published status for a model. Retired
// models are removed from the catalog rather than marked.
type Lifecycle string

const (
	// LifecycleCurrent marks a generally available model.
	LifecycleCurrent Lifecycle = ""
	// LifecyclePreview marks a model the provider labels preview or
	// experimental; its behavior and price may change without a new ID.
	LifecyclePreview Lifecycle = "preview"
	// LifecycleDeprecated marks a model still served but scheduled for
	// retirement; Entry.Note names the replacement when one is published.
	LifecycleDeprecated Lifecycle = "deprecated"
)

// Destination is a public API and endpoint a catalog file documents.
// Endpoint may contain "{model}", which a client replaces with the escaped
// model ID, as Gemini's GenerateContent URL does.
//
// AutoCache marks a destination documented and qualified as accepting
// Anthropic's automatic cache point, where the Messages client caches by
// default. See design/prompt-cache.md.
type Destination struct {
	API       string `json:"api"`
	Endpoint  string `json:"endpoint"`
	AutoCache bool   `json:"auto_cache,omitempty"`
}

// Entry is everything the catalog knows about one exact model ID at one
// provider. Its claims are unbound: Bind scopes them to an API and endpoint
// as llm.ModelInfo evidence requires.
type Entry struct {
	// Provider is the catalog's provider name, such as "anthropic". It is
	// not the application's destination name in llm.ModelRef.
	Provider string

	// Model is the exact ID sent to the API.
	Model string

	// APIs lists the wire APIs, such as "openai.responses", that serve the
	// model. A model can be missing from one of a provider's APIs.
	APIs []string

	// Lifecycle is the provider's published status.
	Lifecycle Lifecycle

	// Snapshot is the dated model ID this alias served when the entry was
	// qualified, when the provider documents one. See Serves.
	Snapshot string

	// Note records context a reviewer needs, such as a scheduled price
	// change or a replacement model.
	Note string

	// Capabilities holds the documented limits and feature support. Features
	// the source does not state are absent, which means unknown.
	Capabilities llm.Capabilities

	// Pricing is the list-price sheet for the default service tier and
	// speed, or nil when no price is documented.
	Pricing *llm.Pricing

	// Source is the documentation URL the claims were taken from.
	Source string

	// QualifiedAt dates the claims; prices carry their own date.
	QualifiedAt time.Time
}

// Clone returns an entry that shares no slices, maps, or pricing data.
func (e Entry) Clone() Entry {
	e.APIs = slices.Clone(e.APIs)
	info := llm.ModelInfo{Capabilities: e.Capabilities, Pricing: e.Pricing}
	info = info.Clone()
	e.Capabilities, e.Pricing = info.Capabilities, info.Pricing
	return e
}

// Serves reports whether a response that names served as its model is
// covered by this entry: the same ID, or the documented snapshot of this
// alias. Callers check Result.Served this way before trusting the entry's
// price sheet for an estimate.
func (e Entry) Serves(served string) bool {
	return served == e.Model || (e.Snapshot != "" && served == e.Snapshot)
}

// Bind scopes the entry's claims to one API and endpoint and returns them as
// documentation evidence. An application can bind an entry to its own
// endpoint, such as a proxy, and supply it in a client's Config.Models; the
// catalog cannot know whether that endpoint behaves like the public one.
func (e Entry) Bind(api, endpoint string) (llm.ModelInfo, error) {
	if !slices.Contains(e.APIs, api) {
		return llm.ModelInfo{}, fmt.Errorf(
			"catalog: %s model %q is not served by API %q",
			e.Provider,
			e.Model,
			api,
		)
	}
	info := llm.ModelInfo{
		Capabilities: e.Capabilities,
		Pricing:      e.Pricing,
		Evidence: llm.ModelEvidence{
			Model:       e.Model,
			API:         api,
			Endpoint:    endpoint,
			Owner:       Owner,
			Source:      e.Source,
			Method:      llm.EvidenceDocumentation,
			QualifiedAt: e.QualifiedAt,
		},
	}.Clone()
	if err := info.Validate(); err != nil {
		return llm.ModelInfo{}, fmt.Errorf(
			"catalog: %s model %q: %w",
			e.Provider,
			e.Model,
			err,
		)
	}
	return info, nil
}

// Providers returns the catalog's provider names in sorted order.
func Providers() []string {
	c := load()
	names := make([]string, 0, len(c))
	for _, p := range c {
		names = append(names, p.name)
	}
	return names
}

// Entries returns a provider's entries in catalog order. It returns nil for
// a provider the catalog does not cover.
func Entries(provider string) []Entry {
	p, ok := find(provider)
	if !ok {
		return nil
	}
	entries := make([]Entry, len(p.entries))
	for i, e := range p.entries {
		entries[i] = e.Clone()
	}
	return entries
}

// Find returns the entry for an exact model ID at a provider.
func Find(provider, model string) (Entry, bool) {
	p, ok := find(provider)
	if !ok {
		return Entry{}, false
	}
	i, ok := p.index[model]
	if !ok {
		return Entry{}, false
	}
	return p.entries[i].Clone(), true
}

// Default returns the provider's documented general-purpose model. A
// dashi.Runner uses it when its agent names no model and the client knows
// the model; the llm layer never selects it.
func Default(provider string) (string, bool) {
	p, ok := find(provider)
	if !ok {
		return "", false
	}
	return p.defaultModel, true
}

// Small returns an inexpensive current model for the provider, suited to
// examples, tests, and simple tasks. False means the provider is unknown or
// its catalog names no small model. Nothing in Dashi selects it implicitly.
func Small(provider string) (string, bool) {
	p, ok := find(provider)
	if !ok || p.smallModel == "" {
		return "", false
	}
	return p.smallModel, true
}

// Destinations returns the public APIs and endpoints a provider's file
// documents.
func Destinations(provider string) []Destination {
	p, ok := find(provider)
	if !ok {
		return nil
	}
	return slices.Clone(p.destinations)
}

// Lookup returns bound catalog knowledge for a model at an exact API and
// endpoint. It finds nothing for an endpoint no catalog file documents, such
// as a proxy or a regional deployment, because the documentation describes
// the public API. Provider clients call it when the application supplied no
// ModelInfo for the model.
func Lookup(api, endpoint, model string) (llm.ModelInfo, bool) {
	for _, p := range load() {
		for _, d := range p.destinations {
			if d.API != api || d.resolve(model) != endpoint {
				continue
			}
			i, ok := p.index[model]
			if !ok {
				return llm.ModelInfo{}, false
			}
			info, err := p.entries[i].Bind(api, endpoint)
			return info, err == nil
		}
	}
	return llm.ModelInfo{}, false
}

// AutoCache reports whether a catalog destination with this API and
// endpoint is marked AutoCache. An endpoint the catalog does not document,
// such as a proxy, is never marked.
func AutoCache(api, endpoint string) bool {
	for _, p := range load() {
		for _, d := range p.destinations {
			if d.API == api && d.Endpoint == endpoint {
				return d.AutoCache
			}
		}
	}
	return false
}

func (d Destination) resolve(model string) string {
	return strings.ReplaceAll(d.Endpoint, "{model}", url.PathEscape(model))
}

// provider is one parsed, validated catalog file. Parsed files are never
// modified; accessors return clones.
type provider struct {
	name         string
	defaultModel string
	smallModel   string
	destinations []Destination
	entries      []Entry
	index        map[string]int
}

// load parses the embedded files once. The files are checked by tests, so a
// failure here means a broken build rather than a runtime condition.
var load = sync.OnceValue(func() []provider {
	c, err := parseAll()
	if err != nil {
		panic(err)
	}
	return c
})

func find(name string) (provider, bool) {
	c := load()
	i := slices.IndexFunc(c, func(p provider) bool {
		return p.name == name
	})
	if i < 0 {
		return provider{}, false
	}
	return c[i], true
}

func parseAll() ([]provider, error) {
	names, err := files.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var c []provider
	for _, f := range names {
		data, err := files.ReadFile(f.Name())
		if err != nil {
			return nil, err
		}
		p, err := parse(data)
		if err != nil {
			return nil, fmt.Errorf(
				"catalog: %s: %w",
				f.Name(),
				err,
			)
		}
		if want := strings.TrimSuffix(
			f.Name(),
			".json",
		); p.name != want {
			return nil, fmt.Errorf(
				"catalog: %s names provider %q",
				f.Name(),
				p.name,
			)
		}
		c = append(c, p)
	}
	return c, nil
}

// fileJSON is the on-disk shape. Capabilities and pricing use the same JSON
// as llm.ModelInfo, so a catalog entry reads like the fact it becomes.
type fileJSON struct {
	Provider     string        `json:"provider"`
	Source       string        `json:"source"`
	Default      string        `json:"default"`
	Small        string        `json:"small,omitempty"`
	Destinations []Destination `json:"destinations"`
	Models       []entryJSON   `json:"models"`
}

type entryJSON struct {
	Model        string           `json:"model"`
	APIs         []string         `json:"apis"`
	Lifecycle    Lifecycle        `json:"lifecycle,omitempty"`
	Snapshot     string           `json:"snapshot,omitempty"`
	Note         string           `json:"note,omitempty"`
	QualifiedAt  string           `json:"qualified_at"`
	Capabilities llm.Capabilities `json:"capabilities"`
	Pricing      *llm.Pricing     `json:"pricing,omitempty"`
}

func parse(data []byte) (provider, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var f fileJSON
	if err := decoder.Decode(&f); err != nil {
		return provider{}, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return provider{}, errors.New("trailing data after catalog")
	}
	if strings.TrimSpace(f.Provider) == "" || f.Source == "" {
		return provider{}, errors.New("catalog requires provider and source")
	}
	if len(f.Destinations) == 0 {
		return provider{}, errors.New("catalog requires a destination")
	}
	p := provider{
		name:         f.Provider,
		defaultModel: f.Default,
		smallModel:   f.Small,
		destinations: f.Destinations,
		index:        make(map[string]int, len(f.Models)),
	}
	for _, m := range f.Models {
		e, err := m.entry(f.Provider, f.Source)
		if err != nil {
			return provider{}, err
		}
		if _, dup := p.index[e.Model]; dup {
			return provider{}, fmt.Errorf(
				"duplicate model %q",
				e.Model,
			)
		}
		for _, api := range e.APIs {
			d := slices.IndexFunc(
				f.Destinations,
				func(d Destination) bool {
					return d.API == api
				},
			)
			if d < 0 {
				return provider{}, fmt.Errorf(
					"model %q names undocumented API %q",
					e.Model,
					api,
				)
			}
			endpoint := f.Destinations[d].resolve(e.Model)
			if _, err := e.Bind(api, endpoint); err != nil {
				return provider{}, err
			}
		}
		p.index[e.Model] = len(p.entries)
		p.entries = append(p.entries, e)
	}
	i, ok := p.index[p.defaultModel]
	if !ok || p.entries[i].Lifecycle == LifecycleDeprecated {
		return provider{}, fmt.Errorf(
			"default %q must be a current catalog model",
			p.defaultModel,
		)
	}
	if p.smallModel != "" {
		i, ok := p.index[p.smallModel]
		if !ok || p.entries[i].Lifecycle == LifecycleDeprecated {
			return provider{}, fmt.Errorf(
				"small %q must be a current catalog model",
				p.smallModel,
			)
		}
	}
	return p, nil
}

func (m entryJSON) entry(providerName, source string) (Entry, error) {
	if strings.TrimSpace(m.Model) == "" || len(m.APIs) == 0 {
		return Entry{}, errors.New("catalog entries require model and apis")
	}
	switch m.Lifecycle {
	case LifecycleCurrent, LifecyclePreview, LifecycleDeprecated:
	default:
		return Entry{}, fmt.Errorf(
			"model %q has unknown lifecycle %q",
			m.Model,
			m.Lifecycle,
		)
	}
	if m.Snapshot == m.Model {
		return Entry{}, fmt.Errorf(
			"model %q names itself as snapshot",
			m.Model,
		)
	}
	date, err := time.Parse(time.DateOnly, m.QualifiedAt)
	if err != nil {
		return Entry{}, fmt.Errorf(
			"model %q requires a qualified_at date",
			m.Model,
		)
	}
	return Entry{
		Provider:     providerName,
		Model:        m.Model,
		APIs:         m.APIs,
		Lifecycle:    m.Lifecycle,
		Snapshot:     m.Snapshot,
		Note:         m.Note,
		Capabilities: m.Capabilities,
		Pricing:      m.Pricing,
		Source:       source,
		QualifiedAt:  date,
	}, nil
}
