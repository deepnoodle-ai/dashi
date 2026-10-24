package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
)

// preferenceField retains presence independently of its value. Null is an
// explicit omission only for temperature; it must not revive lower tiers.
type preferenceField[T any] struct {
	Set, Null bool
	Value     T
}

func (f *preferenceField[T]) UnmarshalJSON(data []byte) error {
	f.Set = true
	f.Null = bytes.Equal(bytes.TrimSpace(data), []byte("null"))
	if f.Null {
		var zero T
		f.Value = zero
		return nil
	}
	return json.Unmarshal(data, &f.Value)
}

type generationPreferences struct {
	Effort       preferenceField[string]  `json:"thinking_effort"`
	ShowThinking preferenceField[bool]    `json:"show_thinking"`
	Temperature  preferenceField[float64] `json:"temperature"`
	MaxTokens    preferenceField[int]     `json:"max_tokens"`
}
type preferenceFile struct {
	modelSelection
	generationPreferences
	Providers map[string]destinationDefinition `json:"providers"`
	// Decide is the raw "decide" list; resolveDecide reads it from the
	// user file only.
	Decide json.RawMessage `json:"decide"`
	// FeaturedModels is the raw "featured_models" object, which replaces
	// the built-in /model picker lists. Dashi reads it from the user file
	// only, with parseFeaturedModels, and never fails startup over it.
	FeaturedModels json.RawMessage `json:"featured_models"`
	Source         string          `json:"-"`
	Project        bool            `json:"-"`
	LoadError      string          `json:"-"`
}

// featuredList is one destination's entry in "featured_models".
type featuredList struct {
	destination string
	models      []string
}

// parseFeaturedModels reads "featured_models", keeping the destinations in
// the order written. A value of the wrong shape is ignored with a problem,
// so the picker falls back to the built-in lists.
func parseFeaturedModels(raw json.RawMessage) ([]featuredList, string) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, ""
	}
	const problem = "featured_models must map destinations to lists of model IDs; it is ignored"
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, problem
	}
	var lists []featuredList
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, problem
		}
		var models []string
		if err := decoder.Decode(&models); err != nil {
			return nil, problem
		}
		lists = append(lists, featuredList{destination: token.(string), models: models})
	}
	return lists, ""
}

// preferenceSourceError identifies a source whose bytes cannot be read or
// decoded. Strict destination/control validation errors remain fatal.
type preferenceSourceError struct{ message string }

func (e *preferenceSourceError) Error() string { return e.message }

func readPreferenceFile(path string, project bool) (preferenceFile, error) {
	p := preferenceFile{Source: path, Project: project}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, &preferenceSourceError{message: fmt.Sprintf("%s: cannot read settings", path)}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return p, &preferenceSourceError{message: fmt.Sprintf("%s: settings must contain a JSON object", path)}
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, &preferenceSourceError{message: fmt.Sprintf("%s: invalid settings JSON", path)}
	}
	// Validate registered definitions strictly so inline credentials and command
	// sources are errors, rather than ignored fields that appear to work.
	var raw struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return p, &preferenceSourceError{message: fmt.Sprintf("%s: invalid settings JSON", path)}
	}
	for id, body := range raw.Providers {
		var definition destinationDefinition
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&definition); err != nil {
			return p, fmt.Errorf("%s: invalid destination definition", path)
		}
		d := configuredDestination{ID: id, Definition: definition, Source: path, Project: project}
		if err := d.validate(); err != nil {
			return p, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := p.generationPreferences.validate(); err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}
func (p generationPreferences) validate() error {
	if p.Effort.Set && (p.Effort.Null || strings.TrimSpace(p.Effort.Value) != p.Effort.Value) {
		return errors.New("invalid thinking_effort")
	}
	if p.ShowThinking.Set && p.ShowThinking.Null {
		return errors.New("invalid show_thinking")
	}
	if p.MaxTokens.Set && (p.MaxTokens.Null || p.MaxTokens.Value < 0) {
		return errors.New("invalid max_tokens")
	}
	if p.Temperature.Set && !p.Temperature.Null && (math.IsNaN(p.Temperature.Value) || math.IsInf(p.Temperature.Value, 0) || p.Temperature.Value < 0) {
		return errors.New("invalid temperature")
	}
	return nil
}

type generationTier struct {
	generationPreferences
	Source string
}
type preferenceProvenance struct {
	Winning    string   `json:"winning"`
	Overridden []string `json:"overridden,omitempty"`
}
type resolvedControls struct {
	Settings     llm.Settings
	ShowThinking bool
	Sources      map[string]preferenceProvenance
}

func resolveGenerationField[T any](tiers []generationTier, get func(generationPreferences) preferenceField[T]) (preferenceField[T], preferenceProvenance) {
	var value preferenceField[T]
	var source preferenceProvenance
	for _, tier := range tiers {
		field := get(tier.generationPreferences)
		if !field.Set {
			continue
		}
		if !value.Set {
			value = field
			source.Winning = tier.Source
		} else {
			source.Overridden = append(source.Overridden, tier.Source)
		}
	}
	return value, source
}
func resolveControls(tiers []generationTier) (resolvedControls, error) {
	result := resolvedControls{Sources: make(map[string]preferenceProvenance)}
	for _, tier := range tiers {
		if err := tier.generationPreferences.validate(); err != nil {
			return result, fmt.Errorf("%s: %w", tier.Source, err)
		}
	}
	effort, source := resolveGenerationField(tiers, func(p generationPreferences) preferenceField[string] { return p.Effort })
	result.Sources["thinking_effort"] = source
	if effort.Set && effort.Value != "" {
		result.Settings.Reasoning = &llm.ReasoningSettings{Effort: effort.Value}
	}
	thinking, source := resolveGenerationField(tiers, func(p generationPreferences) preferenceField[bool] { return p.ShowThinking })
	result.Sources["show_thinking"] = source
	result.ShowThinking = thinking.Value
	if thinking.Set {
		result.Settings.ReasoningSummary = new(thinking.Value)
	}
	temperature, source := resolveGenerationField(tiers, func(p generationPreferences) preferenceField[float64] { return p.Temperature })
	result.Sources["temperature"] = source
	if temperature.Set && !temperature.Null {
		value := temperature.Value
		result.Settings.Temperature = &value
	}
	tokens, source := resolveGenerationField(tiers, func(p generationPreferences) preferenceField[int] { return p.MaxTokens })
	result.Sources["max_tokens"] = source
	result.Settings.MaxTokens = tokens.Value
	return result, nil
}
