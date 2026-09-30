package skill

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	skillFile     = "SKILL.md"
	maxFileBytes  = maxBodyBytes + 16<<10
	maxLoaderName = 64
)

var loaderName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Warning reports one file or folder that the loader skipped or changed. The
// load continues after a warning.
type Warning struct {
	// Path is the file or folder that caused the warning.
	Path    string
	Message string
}

// Info describes one loaded skill for a host that lists skills or explains
// why a name did not run.
type Info struct {
	// Path is the defining SKILL.md or command file.
	Path         string
	Name         string
	Description  string
	ArgumentHint string
	// Dir is the skill folder. It is empty for a command file.
	Dir string
	// ModelInvocable is true when the skill is in the bundle catalog.
	ModelInvocable bool
	// UserInvocable is true when Set.Expand returns the skill.
	UserInvocable bool
}

// Set is the immutable result of a load. It holds every skill that loaded,
// including skills that only a user can invoke. Use Bundle for the model
// and Expand for the user.
type Set struct {
	entries    []entry
	candidates []Info
}

type entry struct {
	info Info
	body string
}

// frontmatter holds the fields the loader reads. Zero values mean absent.
type frontmatter struct {
	Name                   string
	Description            string
	ArgumentHint           string
	DisableModelInvocation bool
	UserInvocable          *bool
}

// LoadDirs reads every <dir>/<name>/SKILL.md and every <dir>/<name>.md. Earlier
// dirs win over later dirs, so put the project dirs first. A missing dir is
// not an error. The loader never fails: a bad file becomes a Warning and the
// other skills still load. It reads only SKILL.md and command files, never
// the supporting files of a skill.
func LoadDirs(dirs ...string) (*Set, []Warning) {
	var (
		set      = &Set{}
		warnings []Warning
		seen     = map[string]bool{}
	)
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		// An absolute path keeps the base-directory line valid for a file
		// tool that runs from another working directory.
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		found, more := loadDir(dir)
		warnings = append(warnings, more...)
		for _, e := range found {
			set.candidates = append(set.candidates, e.info)
			key := strings.ToLower(e.info.Name)
			if seen[key] {
				continue
			}
			seen[key] = true
			set.entries = append(set.entries, e)
		}
	}
	slices.SortFunc(set.entries, func(a, b entry) int {
		return strings.Compare(
			strings.ToLower(a.info.Name),
			strings.ToLower(b.info.Name),
		)
	})
	warnings = append(warnings, set.fitCatalog()...)
	// Catalog fitting can change the selected skill's model access. Preserve
	// original shadowed metadata; update only the winning discovery occurrence.
	updated := map[string]bool{}
	selected := map[string]Info{}
	for _, e := range set.entries {
		selected[strings.ToLower(e.info.Name)] = e.info
	}
	for i, candidate := range set.candidates {
		key := strings.ToLower(candidate.Name)
		if !updated[key] {
			set.candidates[i] = selected[key]
			updated[key] = true
		}
	}
	return set, warnings
}

// DefaultDirs lists the standard search dirs, highest precedence first. For
// the project and then the home dir, it lists .dashi, .agents, and .claude
// skill dirs, then .dashi and .claude command dirs. An empty argument skips
// that level. Pass the repository root as projectDir, not the working
// directory, so a run from a subfolder finds the same skills.
func DefaultDirs(projectDir, homeDir string) []string {
	var dirs []string
	for _, base := range []string{projectDir, homeDir} {
		if base == "" {
			continue
		}
		for _, rel := range []string{
			".dashi/skills",
			".agents/skills",
			".claude/skills",
			".dashi/commands",
			".claude/commands",
		} {
			dirs = append(dirs, filepath.Join(base, filepath.FromSlash(rel)))
		}
	}
	return dirs
}

// Skills returns every loaded skill, sorted by name. The Set is not changed
// by the caller's use of the slice.
func (s *Set) Skills() []Info {
	if s == nil {
		return nil
	}
	infos := make([]Info, len(s.entries))
	for i, e := range s.entries {
		infos[i] = e.info
	}
	return infos
}

// Candidates returns valid selected and shadowed definitions in discovery order.
// The first candidate with each case-insensitive name is selected. The caller
// owns the returned slice. Instruction bodies are not included.
func (s *Set) Candidates() []Info {
	if s == nil {
		return nil
	}
	return slices.Clone(s.candidates)
}

// Bundle builds the model-facing catalog and load tool from the skills that
// the model may invoke. It fails only when Options.ToolName is not valid: the
// loader has already dropped any skill that does not fit the catalog limits.
func (s *Set) Bundle(options Options) (*Bundle, error) {
	var definitions []Definition
	if s != nil {
		for _, e := range s.entries {
			if !e.info.ModelInvocable {
				continue
			}
			definitions = append(definitions, Definition{
				Name:         e.info.Name,
				Description:  e.info.Description,
				Instructions: e.body,
				Dir:          e.info.Dir,
			})
		}
	}
	return New(definitions, options)
}

// fitCatalog fills the catalog in name order and drops each model-visible
// skill that does not fit the count or size limit. It measures with the
// longest tool name, so any Options.ToolName fits later.
func (s *Set) fitCatalog() []Warning {
	var (
		warnings []Warning
		kept     []Definition
	)
	longest := strings.Repeat("x", maxToolNameBytes)
	for i := range s.entries {
		e := &s.entries[i]
		if !e.info.ModelInvocable {
			continue
		}
		next := append(slices.Clone(kept), Definition{
			Name:        e.info.Name,
			Description: e.info.Description,
		})
		switch {
		case len(next) > maxDefinitions:
			warnings = append(warnings, dropped(e, "catalog holds 256 skills"))
		case len(formatCatalog(longest, next)) > maxCatalogBytes:
			warnings = append(warnings, dropped(e, "catalog is full"))
		default:
			kept = next
			continue
		}
		e.info.ModelInvocable = false
	}
	return warnings
}

func dropped(e *entry, why string) Warning {
	path := e.info.Dir
	if path == "" {
		path = e.info.Name
	}
	return Warning{
		Path: path,
		Message: fmt.Sprintf(
			"skill %q left out of the model catalog: %s; "+
				"the user can still invoke it",
			e.info.Name,
			why,
		),
	}
}

// loadDir reads one search dir. A dir that does not exist gives nothing.
func loadDir(dir string) ([]entry, []Warning) {
	items, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []Warning{{Path: dir, Message: "cannot read: " + err.Error()}}
	}
	var (
		found    []entry
		warnings []Warning
		names    = map[string]string{}
	)
	for _, item := range items {
		fileName := item.Name()
		if strings.HasPrefix(fileName, ".") {
			continue
		}
		path := filepath.Join(dir, fileName)
		// Stat, not the dir entry, so a symlink to a folder counts as a folder.
		info, err := os.Stat(path)
		if err != nil {
			warnings = append(warnings, Warning{
				Path:    path,
				Message: "cannot read: " + err.Error(),
			})
			continue
		}
		var (
			e   entry
			ok  bool
			bad []Warning
		)
		switch {
		case info.IsDir():
			e, ok, bad = loadFolder(path, fileName)
		case strings.EqualFold(filepath.Ext(fileName), ".md") &&
			!strings.EqualFold(fileName, "README.md"):
			base := strings.TrimSuffix(fileName, filepath.Ext(fileName))
			e, ok, bad = loadFile(path, base, "")
		}
		warnings = append(warnings, bad...)
		if !ok {
			continue
		}
		key := strings.ToLower(e.info.Name)
		if first, dup := names[key]; dup {
			warnings = append(warnings, Warning{
				Path: path,
				Message: fmt.Sprintf(
					"skill name %q is already defined by %s; skipped",
					e.info.Name,
					first,
				),
			})

		} else {
			names[key] = path
		}
		found = append(found, e)
	}
	return found, warnings
}

func loadFolder(dir, base string) (entry, bool, []Warning) {
	path := filepath.Join(dir, skillFile)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return entry{}, false, []Warning{{
			Path: dir,
			Message: "folder has no " + skillFile +
				"; nested directories are not loaded",
		}}
	}
	return loadFile(path, base, dir)
}

// loadFile parses one skill file. dir is empty for a command file, which only
// a user can invoke. A field of the wrong type is ignored with a warning that
// names the field; the skill still loads.
func loadFile(path, base, dir string) (entry, bool, []Warning) {
	fail := func(format string, args ...any) (entry, bool, []Warning) {
		return entry{}, false, []Warning{{
			Path:    path,
			Message: fmt.Sprintf(format, args...),
		}}
	}
	raw, err := readLimited(path)
	if err != nil {
		return fail("cannot read: %v", err)
	}
	if !utf8.Valid(raw) {
		return fail("file is not valid UTF-8")
	}
	var (
		fm       frontmatter
		warnings []Warning
	)
	body := raw
	if front, rest, found, err := splitFrontmatter(raw); err != nil {
		return fail("%v", err)
	} else if found {
		body = rest
		var problems []string
		fm, problems = parseFields(front)
		for _, problem := range problems {
			warnings = append(warnings, Warning{Path: path, Message: problem})
		}
	}
	name := strings.TrimSpace(fm.Name)
	if name == "" {
		name = base
	}
	description := strings.Join(strings.Fields(fm.Description), " ")
	text := strings.TrimSpace(string(body))
	switch {
	case len(name) > maxLoaderName || !loaderName.MatchString(name):
		return fail(
			"invalid name %q: use letters, digits, - and _, "+
				"start with a letter or digit, at most %d bytes",
			name,
			maxLoaderName,
		)
	case len(description) > maxDescriptionBytes:
		return fail("description exceeds %d bytes", maxDescriptionBytes)
	case !validMetadata(description):
		return fail("description has control characters")
	case text == "":
		return fail("skill body is empty")
	case len(text) > maxBodyBytes:
		return fail("skill body exceeds %d bytes", maxBodyBytes)
	}
	userOK := fm.UserInvocable == nil || *fm.UserInvocable
	return entry{
		info: Info{
			Path:           path,
			Name:           name,
			Description:    description,
			ArgumentHint:   strings.Join(strings.Fields(fm.ArgumentHint), " "),
			Dir:            dir,
			ModelInvocable: dir != "" && !fm.DisableModelInvocation,
			UserInvocable:  userOK,
		},
		body: text,
	}, true, warnings
}

// parseFields reads the known fields from a frontmatter block. Real command
// files are often not strict YAML (an unquoted colon in a description, or
// "argument-hint: [a] [b]"), so when YAML fails the parser falls back to
// reading "key: value" lines. A known field with a value it cannot use is
// ignored and reported by name.
func parseFields(front []byte) (frontmatter, []string) {
	values := map[string]any{}
	if err := yaml.Unmarshal(front, &values); err != nil {
		values = lineFields(string(front))
	} else {
		keepHashText(values, lineFields(string(front)))
	}
	var (
		fm       frontmatter
		problems []string
	)
	text := func(key string, allowList bool) string {
		v, ok := values[key]
		if !ok {
			return ""
		}
		var s string
		if _, isList := v.([]any); isList && !allowList {
			ok = false
		} else {
			s, ok = scalarText(v)
		}
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"frontmatter field %q ignored: expected text", key,
			))
		}
		return s
	}
	flag := func(key string) *bool {
		v, ok := values[key]
		if !ok {
			return nil
		}
		b, ok := boolValue(v)
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"frontmatter field %q ignored: expected true or false", key,
			))
			return nil
		}
		return &b
	}
	fm.Name = text("name", false)
	fm.Description = text("description", false)
	fm.ArgumentHint = text("argument-hint", true)
	if b := flag("disable-model-invocation"); b != nil {
		fm.DisableModelInvocation = *b
	}
	fm.UserInvocable = flag("user-invocable")
	return fm, problems
}

// keepHashText restores text after " #". YAML reads that as a comment, but a
// description such as "Fix issue #$ARGUMENTS" is prose in command files. It
// applies only to unquoted single-line text.
func keepHashText(values, lines map[string]any) {
	for key, v := range values {
		parsed, ok := v.(string)
		raw, _ := lines[key].(string)
		if !ok || raw == parsed || !strings.HasPrefix(raw, parsed) {
			continue
		}
		if strings.ContainsAny(raw[:1], "\"'[{&*!|>%@`") ||
			!strings.HasPrefix(strings.TrimSpace(raw[len(parsed):]), "#") {
			continue
		}
		values[key] = raw
	}
}

// scalarText turns a YAML value into text. A list, such as the YAML reading
// of "argument-hint: [issue-number]", is written back in brackets.
func scalarText(v any) (string, bool) {
	switch v := v.(type) {
	case nil:
		return "", true
	case string:
		return v, true
	case bool, int, int64, uint64, float64:
		return fmt.Sprint(v), true
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			part, ok := scalarText(item)
			if !ok {
				return "", false
			}
			parts = append(parts, part)
		}
		return "[" + strings.Join(parts, ", ") + "]", true
	}
	return "", false
}

func boolValue(v any) (bool, bool) {
	switch v := v.(type) {
	case bool:
		return v, true
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		return b, err == nil
	}
	return false, false
}

var keyLine = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*):[ \t]*(.*)$`)

// lineFields reads top-level "key: value" lines. Indented lines continue the
// value above. A lone ">" or "|" marks a block value and is dropped.
func lineFields(front string) map[string]any {
	values := map[string]any{}
	var key string
	for _, line := range strings.Split(front, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := keyLine.FindStringSubmatch(line); m != nil {
			key = m[1]
			value := strings.TrimSpace(m[2])
			switch value {
			case ">", "|", ">-", "|-":
				value = ""
			}
			values[key] = unquote(value)
			continue
		}
		if key != "" && strings.TrimSpace(line) != "" &&
			(line[0] == ' ' || line[0] == '\t') {
			prev, _ := values[key].(string)
			values[key] = strings.TrimSpace(prev + " " + strings.TrimSpace(line))
		}
	}
	return values
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxFileBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", maxFileBytes)
	}
	return raw, nil
}

// splitFrontmatter separates the YAML block between "---" lines. A file that
// does not start with "---" has no frontmatter and is valid.
func splitFrontmatter(raw []byte) (front, body []byte, found bool, err error) {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	text := string(bytes.TrimLeft(raw, " \t\r\n"))
	lines := strings.SplitAfter(text, "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return nil, raw, false, nil
	}
	size := len(lines[0])
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			rest := text[size+len(line):]
			return []byte(text[len(lines[0]):size]), []byte(rest), true, nil
		}
		size += len(line)
	}
	return nil, nil, false, errors.New("frontmatter has no closing ---")
}

func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}
