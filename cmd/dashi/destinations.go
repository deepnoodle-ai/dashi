package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// destinationDefinition contains routing configuration, never credential values.
// A settings file replaces a complete definition; fields cannot inherit trust.
type destinationDefinition struct {
	API           string   `json:"api"`
	Endpoint      string   `json:"endpoint"`
	Models        []string `json:"models"`
	DefaultModel  string   `json:"default_model"`
	CredentialEnv string   `json:"credential_env"`
}
type configuredDestination struct {
	ID         string
	Definition destinationDefinition
	Source     string
	Project    bool
	// BuiltinAliases apply only to a built-in binding, never a custom name.
	BuiltinAliases []string
}

func validConfigName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}
func validDestinationID(s string) bool {
	return s != "" && utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '/' || r == '\\'
	})
}
func (d configuredDestination) validate() error {
	fail := func(field string) error { return fmt.Errorf("destination %q: invalid %s", d.ID, field) }
	if !validDestinationID(d.ID) {
		return errors.New("invalid destination identity")
	}
	switch d.Definition.API {
	case "anthropic.messages", "openai.responses", "openai.completions", "google.generate_content":
	default:
		return fail("api")
	}
	if d.Definition.CredentialEnv != "" && !validConfigName(d.Definition.CredentialEnv) {
		return fail("credential_env")
	}
	if len(d.Definition.Models) == 0 {
		return fail("models: register at least one exact model ID")
	}
	seen := make(map[string]bool, len(d.Definition.Models))
	for _, id := range d.Definition.Models {
		if id == "" || !utf8.ValidString(id) || strings.TrimSpace(id) != id || strings.ContainsFunc(id, unicode.IsControl) || seen[id] {
			return fail("models")
		}
		seen[id] = true
	}
	if !seen[d.Definition.DefaultModel] {
		return fail("default_model: must be a registered model ID")
	}
	if _, err := d.endpoint(); err != nil {
		return fail("endpoint")
	}
	return nil
}

// endpoint validates transport before any credential lookup or construction.
// HTTP exemptions use literal addresses; DNS names never establish loopback.
func (d configuredDestination) endpoint() (*url.URL, error) {
	raw := d.Definition.Endpoint
	if raw == "" || strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return nil, errors.New("invalid endpoint")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, errors.New("endpoint must be an absolute HTTP URL without userinfo, query, or fragment")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("endpoint must use HTTP or HTTPS")
	}
	if u.Scheme == "http" && (d.Definition.CredentialEnv != "" || len(d.BuiltinAliases) != 0) {
		ip, err := netip.ParseAddr(u.Hostname())
		if err != nil || ip.Zone() != "" || ip.Is4In6() || !(ip.Is4() && ip.IsLoopback() || ip == netip.IPv6Loopback()) {
			return nil, errors.New("credential-bearing HTTP requires literal 127/8 or ::1")
		}
	}
	if d.Definition.API == "google.generate_content" && !strings.Contains(u.Path, "{model}:generateContent") {
		return nil, errors.New("Gemini endpoint needs its model template")
	}
	return u, nil
}

// fingerprint records the whole binding without its credential values. Model
// registration order does not change binding identity.
func (d configuredDestination) hash() (string, error) {
	if err := d.validate(); err != nil {
		return "", err
	}
	u, _ := d.endpoint()
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	models, aliases := slices.Clone(d.Definition.Models), slices.Clone(d.BuiltinAliases)
	slices.Sort(models)
	slices.Sort(aliases)
	binding := struct {
		Version    int      `json:"version"`
		ID         string   `json:"id"`
		API        string   `json:"api"`
		Endpoint   string   `json:"endpoint"`
		Models     []string `json:"models"`
		Default    string   `json:"default_model"`
		Credential string   `json:"credential_env"`
		Aliases    []string `json:"aliases,omitempty"`
	}{1, d.ID, d.Definition.API, u.String(), models, d.Definition.DefaultModel, d.Definition.CredentialEnv, aliases}
	data, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}
func (d configuredDestination) trustedBy(user map[string]configuredDestination) bool {
	if !d.Project {
		return true
	}
	other, ok := user[d.ID]
	if !ok {
		return false
	}
	want, err := other.hash()
	got, gotErr := d.hash()
	return err == nil && gotErr == nil && want == got
}

type modelSelection struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}
type selectionTier struct {
	modelSelection
	Source string
}
type resolvedSelection struct {
	Destination configuredDestination
	Model       string
	Source      string
	Overridden  []selectionTier
}

// resolveSelection never fills a missing selector from a lower tier or guesses
// a destination from a model prefix.
func resolveSelection(destinations map[string]configuredDestination, tiers []selectionTier) (resolvedSelection, error) {
	var selected *selectionTier
	var overridden []selectionTier
	for i := range tiers {
		tier := &tiers[i]
		if tier.Provider == "" && tier.Model == "" {
			continue
		}
		if selected == nil {
			selected = tier
		} else {
			overridden = append(overridden, *tier)
		}
	}
	if selected == nil {
		return resolvedSelection{}, errors.New("no destination/model selected")
	}
	provider, model := selected.Provider, selected.Model
	if provider == "" {
		var matches []string
		for id, d := range destinations {
			if slices.Contains(d.Definition.Models, model) {
				matches = append(matches, id)
			}
		}
		slices.Sort(matches)
		if len(matches) == 0 {
			return resolvedSelection{}, fmt.Errorf("model %q is not registered; select a destination and register its exact ID", model)
		}
		if len(matches) > 1 {
			return resolvedSelection{}, fmt.Errorf("model %q is ambiguous across %s; select a destination", model, strings.Join(matches, ", "))
		}
		provider = matches[0]
	}
	d, ok := destinations[provider]
	if !ok {
		return resolvedSelection{}, fmt.Errorf("destination %q is not configured", provider)
	}
	if err := d.validate(); err != nil {
		return resolvedSelection{}, err
	}
	if model == "" {
		model = d.Definition.DefaultModel
	}
	if !slices.Contains(d.Definition.Models, model) {
		return resolvedSelection{}, fmt.Errorf("model %q is not registered for destination %q", model, provider)
	}
	return resolvedSelection{d, model, selected.Source, overridden}, nil
}

// credential resolves only a validated, trusted binding. Lookup is explicit so
// custom destinations cannot silently inherit a vendor credential.
func (d configuredDestination) credential(lookup func(string) (string, bool), trusted bool) (string, error) {
	if err := d.validate(); err != nil {
		return "", err
	}
	if d.Project && !trusted {
		return "", fmt.Errorf("destination %q needs binding trust before credential lookup", d.ID)
	}
	names := []string{d.Definition.CredentialEnv}
	if len(d.BuiltinAliases) != 0 {
		names = d.BuiltinAliases
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		if value, ok := lookup(name); ok && value != "" {
			return value, nil
		}
	}
	return "", nil
}
