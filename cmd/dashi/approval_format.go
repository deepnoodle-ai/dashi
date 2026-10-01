package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/runewidth"
	"github.com/deepnoodle-ai/wonton/tui"
)

// toolApproval is an approval request as a UI shows it.
type toolApproval struct {
	// ApprovalRequest is the printable form of the request.
	ApprovalRequest
	// Task is the short ID of the delegated task whose call asks. It is
	// empty for a call of the foreground conversation.
	Task string
}

// clippedValueRows is how many rows one argument shows before the rest
// collapses into a "+N lines" row.
const clippedValueRows = 6

type rowKind int

const (
	rowText rowKind = iota
	rowMuted
	rowCommand
	rowDiff
)

// approvalRow is one terminal row of approval details. Label is the muted
// "key: " part of an argument row. Rows are wrapped before display, so each
// one fits the width it was built for.
type approvalRow struct {
	indent int
	label  string
	text   string
	kind   rowKind
}

func (r approvalRow) String() string {
	return strings.Repeat(" ", r.indent) + r.label + r.text
}

// approvalLayout is what the prompt shows: a one-line summary for the title,
// and the detail rows.
type approvalLayout struct {
	summary string
	rows    []approvalRow
	// clipped reports that at least one argument hides rows.
	clipped bool
}

// layoutApproval lays out a for width columns of detail text. A width of 0
// neither wraps nor clips, for line mode. When expanded is false, long
// arguments show their first rows only.
func layoutApproval(a toolApproval, width int, expanded bool) approvalLayout {
	fields, ok := decodeArgs(a.Arguments)
	summary, skip := approvalSummary(a.ApprovalRequest, fields)
	b := rowBuilder{width: width, clip: width > 0 && !expanded}
	if width > 0 && runewidth.StringWidth(approvalTitle(a.Tool)+"  "+summary) > width {
		// A summary too long for the title shows in full as the first rows.
		b.text(0, "", summary, rowText)
		summary = ""
	}
	if a.Task != "" {
		b.text(0, "", fmt.Sprintf("From task %s · /task %s shows it", a.Task, a.Task), rowMuted)
	}
	if a.Command != "" {
		skip = append(skip, "command")
		for i, line := range strings.Split(a.Command, "\n") {
			prefix := "$ "
			if i > 0 {
				prefix = "  "
			}
			b.text(0, "", prefix+line, rowCommand)
		}
		if a.Dir != "" {
			b.text(0, "", "in "+a.Dir, rowMuted)
		}
	}
	if a.Preview != "" {
		skip = append(skip, "path", "content", "old_string", "new_string")
		for _, line := range strings.Split(strings.TrimRight(a.Preview, "\n"), "\n") {
			b.text(0, "", line, rowDiff)
		}
	}
	switch {
	case ok:
		for _, f := range fields {
			if !slices.Contains(skip, f.key) {
				b.field(f)
			}
		}
	case a.Arguments != "":
		b.text(0, "", a.Arguments, rowText)
	}
	if a.RememberScope != "" {
		b.text(0, "", "Allow for this Dashi run covers "+lowerFirst(a.RememberScope), rowMuted)
	}
	return approvalLayout{summary: summary, rows: b.rows, clipped: b.clipped}
}

func approvalTitle(tool string) string {
	return "Allow " + tool + "?"
}

// approvalSummary says in one line what the call does, and names the
// arguments that the summary already shows. A shell command has no summary,
// because the details show the command itself.
func approvalSummary(r ApprovalRequest, fields []argField) (string, []string) {
	if r.Command != "" {
		return "", nil
	}
	if r.Summary != "" && r.Summary != r.Tool {
		return firstLine(r.Summary), nil
	}
	text := func(key string) string {
		for _, f := range fields {
			if s, ok := f.value.(string); ok && f.key == key && !strings.Contains(s, "\n") {
				return s
			}
		}
		return ""
	}
	if r.Tool == delegate.ToolName {
		subagent, description := text("subagent"), text("description")
		switch {
		case subagent != "" && description != "":
			return subagent + " subagent: " + description, []string{"subagent", "description"}
		case subagent != "":
			return subagent + " subagent", []string{"subagent"}
		}
	}
	for _, key := range []string{"description", "path", "file_path", "url", "query", "name"} {
		if s := text(key); s != "" {
			return s, []string{key}
		}
	}
	return "", nil
}

// argField is one argument of a call. A value is a string, json.Number,
// bool, nil, []argField for an object, or []any for an array.
type argField struct {
	key   string
	value any
}

// decodeArgs decodes a JSON object and keeps the order of its keys, which is
// the order the model wrote them in.
func decodeArgs(text string) ([]argField, bool) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil || dec.More() {
		return nil, false
	}
	fields, ok := v.([]argField)
	return fields, ok
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		fields := []argField{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			fields = append(fields, argField{key: workspace.PrintableText(key.(string)), value: v})
		}
		_, err = dec.Token()
		return fields, err
	case json.Delim('['):
		items := []any{}
		for dec.More() {
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			items = append(items, v)
		}
		_, err = dec.Token()
		return items, err
	case nil:
		return nil, nil
	}
	if s, ok := tok.(string); ok {
		// A JSON escape can carry a control character.
		return workspace.PrintableText(s), nil
	}
	return tok, nil
}

type rowBuilder struct {
	width   int
	clip    bool
	clipped bool
	rows    []approvalRow
}

// text adds s as rows at indent, wrapped to the width that is left.
func (b *rowBuilder) text(indent int, label, s string, kind rowKind) {
	s = strings.ReplaceAll(s, "\t", "    ")
	if b.width > 0 {
		s = tui.WrapText(s, max(10, b.width-indent-runewidth.StringWidth(label)))
	}
	for i, line := range strings.Split(s, "\n") {
		if i == 0 {
			b.rows = append(b.rows, approvalRow{indent: indent, label: label, text: line, kind: kind})
		} else {
			b.rows = append(b.rows, approvalRow{indent: indent + len(label), text: line, kind: kind})
		}
	}
}

// field adds one top-level argument and clips it to clippedValueRows.
func (b *rowBuilder) field(f argField) {
	start := len(b.rows)
	b.value(0, f.key+": ", f.value)
	if hidden := len(b.rows) - start - clippedValueRows; b.clip && hidden > 0 {
		b.rows = b.rows[:start+clippedValueRows-1]
		b.rows = append(b.rows, approvalRow{indent: 2, text: fmt.Sprintf("… +%d more lines", hidden+1), kind: rowMuted})
		b.clipped = true
	}
}

// value adds v under label. A short scalar stays on the label's row. A
// multi-line or long string, an object, and an array start below it.
func (b *rowBuilder) value(indent int, label string, v any) {
	switch v := v.(type) {
	case []argField:
		if len(v) == 0 {
			b.text(indent, label, "{}", rowText)
			return
		}
		b.text(indent, strings.TrimRight(label, " "), "", rowText)
		for _, f := range v {
			b.value(indent+2, f.key+": ", f.value)
		}
	case []any:
		if len(v) == 0 {
			b.text(indent, label, "[]", rowText)
			return
		}
		b.text(indent, strings.TrimRight(label, " "), "", rowText)
		for _, item := range v {
			b.value(indent+2, "- ", item)
		}
	case string:
		long := b.width > 0 && indent+runewidth.StringWidth(label+v) > b.width
		if v == "" || !strings.Contains(v, "\n") && !long {
			if v == "" {
				v = `""`
			}
			b.text(indent, label, v, rowText)
			return
		}
		b.text(indent, strings.TrimRight(label, " "), "", rowText)
		b.text(indent+2, "", v, rowText)
	case nil:
		b.text(indent, label, "null", rowText)
	default:
		b.text(indent, label, fmt.Sprint(v), rowText)
	}
}

// approvalText is the plain-text form of a, for line mode.
func approvalText(a toolApproval) string {
	l := layoutApproval(a, 0, true)
	var out strings.Builder
	out.WriteString(approvalTitle(a.Tool))
	if l.summary != "" {
		out.WriteString("  " + l.summary)
	}
	out.WriteString("\n")
	for _, r := range l.rows {
		out.WriteString(strings.TrimRight("  "+r.String(), " ") + "\n")
	}
	return out.String()
}

// lowerFirst lowercases the first letter of a sentence, so it can continue
// another one. A word in capitals, such as "MCP", stays as it is.
func lowerFirst(s string) string {
	r := []rune(s)
	if len(r) > 1 && unicode.IsUpper(r[0]) && unicode.IsLower(r[1]) {
		r[0] = unicode.ToLower(r[0])
	}
	return string(r)
}
