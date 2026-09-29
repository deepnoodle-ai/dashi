// Package skill provides a small, optional in-memory catalog and load tool.
// The host selects definitions, places the catalog at an appropriate context
// origin, and owns every policy or action constraint associated with them.
package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/tool"
)

const (
	defaultToolName     = "load_skill"
	maxDefinitions      = 256
	maxNameBytes        = 80
	maxDescriptionBytes = 1024
	maxBodyBytes        = 64 << 10
	maxCatalogBytes     = 64 << 10
	maxToolNameBytes    = 64
)

// Definition is a host-selected skill. Instructions are returned only when
// the model calls the loader. This type does not carry tool permissions.
type Definition struct {
	Name         string
	Description  string
	Instructions string
	// Dir is the folder of a skill that came from disk. When it is set,
	// loaded text starts with a "Base directory" line, so the model can
	// find supporting files. The bundle never reads that folder.
	Dir string
}

// Options controls the model-facing name of the load tool. The zero value uses
// load_skill. The host must avoid collisions with its other tool names.
type Options struct {
	ToolName string
}

// Bundle is a frozen catalog and its matching load tool. Build a new bundle
// when definitions change, and rebind the original bundle on turn continuation.
type Bundle struct {
	definitions []Definition
	catalog     string
	digest      string
	loader      tool.Tool
}

var _ tool.Toolset = (*Bundle)(nil)

// New validates and freezes host-selected definitions. Names may contain
// spaces; name matching and duplicate detection both use strings.EqualFold.
// It rejects more than 256 definitions, names above 80 bytes, descriptions
// above 1024 bytes, bodies above 64 KiB, and a rendered catalog above 64 KiB.
func New(definitions []Definition, options Options) (*Bundle, error) {
	if len(definitions) > maxDefinitions {
		return nil, fmt.Errorf(
			"skill: %d definitions exceed limit %d",
			len(definitions),
			maxDefinitions,
		)
	}
	name := options.ToolName
	if name == "" {
		name = defaultToolName
	}
	if !validToolName(name) {
		return nil, fmt.Errorf(
			"skill: invalid tool name %q: "+
				"use 1-64 ASCII letters, digits, or underscores; "+
				"start with a letter or underscore",
			name,
		)
	}

	entries := make([]Definition, len(definitions))
	for i, definition := range definitions {
		if len(definition.Description) > maxDescriptionBytes {
			return nil, fmt.Errorf(
				"skill: definition %d description exceeds %d bytes",
				i,
				maxDescriptionBytes,
			)
		}
		if !utf8.ValidString(definition.Description) {
			return nil, fmt.Errorf(
				"skill: definition %d has invalid UTF-8 description",
				i,
			)
		}
		entry := Definition{
			Name:         strings.TrimSpace(definition.Name),
			Description:  strings.Join(strings.Fields(definition.Description), " "),
			Instructions: definition.Instructions,
			Dir:          definition.Dir,
		}
		switch {
		case entry.Name == "":
			return nil, fmt.Errorf("skill: definition %d has an empty name", i)
		case !validMetadata(entry.Name):
			return nil, fmt.Errorf("skill: definition %d has invalid name text", i)
		case len(entry.Name) > maxNameBytes:
			return nil, fmt.Errorf(
				"skill: name %q exceeds %d bytes",
				entry.Name,
				maxNameBytes,
			)
		case !validMetadata(entry.Description):
			return nil, fmt.Errorf(
				"skill: description for %q has invalid text",
				entry.Name,
			)
		case strings.TrimSpace(entry.Instructions) == "":
			return nil, fmt.Errorf(
				"skill: %q has empty instructions",
				entry.Name,
			)
		case !utf8.ValidString(entry.Instructions):
			return nil, fmt.Errorf(
				"skill: %q has invalid UTF-8 instructions",
				entry.Name,
			)
		case len(entry.Instructions) > maxBodyBytes:
			return nil, fmt.Errorf(
				"skill: instructions for %q exceed %d bytes",
				entry.Name,
				maxBodyBytes,
			)
		}
		for j := range entries[:i] {
			if strings.EqualFold(entries[j].Name, entry.Name) {
				return nil, fmt.Errorf(
					"skill: duplicate names %q and %q",
					entries[j].Name,
					entry.Name,
				)
			}
		}
		entries[i] = entry
	}
	slices.SortFunc(entries, func(a, b Definition) int {
		first := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		if first != 0 {
			return first
		}
		return strings.Compare(a.Name, b.Name)
	})
	catalog := formatCatalog(name, entries)
	if len(catalog) > maxCatalogBytes {
		return nil, fmt.Errorf(
			"skill: catalog is %d bytes; limit is %d bytes",
			len(catalog),
			maxCatalogBytes,
		)
	}
	raw, err := json.Marshal(struct {
		ToolName    string
		Definitions []Definition
	}{ToolName: name, Definitions: entries})
	if err != nil {
		return nil, fmt.Errorf("skill: digest input: %w", err)
	}
	sum := sha256.Sum256(raw)
	bundle := &Bundle{
		definitions: entries,
		catalog:     catalog,
		digest:      "sha256:" + hex.EncodeToString(sum[:]),
	}
	if len(entries) == 0 {
		return bundle, nil
	}
	loader, err := tool.Func(
		name,
		"Load the full instructions for an available skill by name.",
		func(
			_ context.Context,
			_ tool.Call,
			input loadInput,
		) (tool.Outcome, error) {
			return bundle.load(input.Name), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("skill: load tool: %w", err)
	}
	loader.Definition.Annotations = tool.Annotations{
		ReadOnly:    new(true),
		Destructive: new(false),
		Idempotent:  new(true),
		OpenWorld:   new(false),
	}
	bundle.loader = loader
	return bundle, nil
}

type loadInput struct {
	Name string `json:"name" description:"Name of the available skill to load"`
}

// Catalog returns the short model-facing list. It contains no instruction
// bodies. The host chooses its context origin and where to place it.
func (b *Bundle) Catalog() string {
	if b == nil {
		return ""
	}
	return b.catalog
}

// Digest identifies the names, descriptions, bodies, and loader name in this
// bundle. It does not include host policy or action selectors.
func (b *Bundle) Digest() string {
	if b == nil {
		return ""
	}
	return b.digest
}

// Tools implements tool.Toolset. It returns one independently cloned loader,
// or no tools for an empty bundle. Use tool.Join to compose it with host tools.
func (b *Bundle) Tools(context.Context) ([]tool.Tool, error) {
	if b == nil || len(b.definitions) == 0 {
		return nil, nil
	}
	loader := b.loader
	loader.Definition = loader.Definition.Clone()
	return []tool.Tool{loader}, nil
}

func (b *Bundle) load(name string) tool.Outcome {
	name = strings.TrimSpace(name)
	if name == "" {
		return tool.Failed("skill name is required")
	}
	if len(name) > maxNameBytes {
		return tool.Failed(
			fmt.Sprintf("skill name exceeds %d bytes", maxNameBytes),
		)
	}
	for _, entry := range b.definitions {
		if strings.EqualFold(entry.Name, name) {
			return tool.Succeeded(withBaseDir(
				entry.Dir,
				"# "+entry.Name+"\n\n"+entry.Instructions,
			))
		}
	}
	return tool.Failed(
		fmt.Sprintf(
			"skill %q is not available. Available skills: %s",
			name,
			availableNames(b.definitions),
		),
	)
}

// withBaseDir puts the base-directory line first for a skill that has a
// folder. Loaded text and slash text share this rule.
func withBaseDir(dir, text string) string {
	if dir == "" {
		return text
	}
	return "Base directory: " + dir + "\n\n" + text
}

func formatCatalog(toolName string, definitions []Definition) string {
	if len(definitions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Skills\n")
	b.WriteString("These skills are available. Use ")
	b.WriteString(toolName)
	b.WriteString(" with a listed skill name to load its instructions when relevant.\n")
	for _, definition := range definitions {
		b.WriteString("- ")
		b.WriteString(strconv.Quote(definition.Name))
		if definition.Description != "" {
			b.WriteString(": ")
			b.WriteString(strconv.Quote(definition.Description))
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func availableNames(definitions []Definition) string {
	const maxShown = 8
	shown := min(len(definitions), maxShown)
	names := make([]string, 0, shown)
	for _, entry := range definitions[:shown] {
		names = append(names, strconv.Quote(entry.Name))
	}
	result := strings.Join(names, ", ")
	if len(definitions) > shown {
		result += fmt.Sprintf(" (and %d more)", len(definitions)-shown)
	}
	return result
}

func validMetadata(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}

func validToolName(name string) bool {
	if len(name) == 0 || len(name) > maxToolNameBytes {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if c == '_' || c >= 'a' && c <= 'z' ||
			c >= 'A' && c <= 'Z' {
			continue
		}
		if i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}
