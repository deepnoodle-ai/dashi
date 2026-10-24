package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/runewidth"
)

// summaryWidth is the most columns a tool line's result line takes in
// plain output.
const summaryWidth = 96

// toolSummary is the plain-output result line of a finished tool call
// (see formatTool): what happened, in
// a few words, such as the user's answer to a question or "214 lines". A
// failed call shows its last error line, and a denied call the reason. A
// tool without its own summary shows its first output line. The result is
// one printable line of at most summaryWidth columns, or empty when there
// is nothing to say.
func toolSummary(name string, args json.RawMessage, state, output, denied string) string {
	var s string
	switch state {
	case StateFailed:
		s = failedSummary(name, output)
	case StateDenied:
		s = denied
	case StateOK:
		s = okSummary(name, args, output)
	}
	s = strings.TrimSpace(workspace.PrintableText(s))
	if first, _, multi := strings.Cut(s, "\n"); multi {
		s = strings.TrimSpace(first) + " …"
	}
	return runewidth.Truncate(s, summaryWidth, "…")
}

func okSummary(name string, args json.RawMessage, output string) string {
	switch name {
	case workspace.ToolAskUser:
		return askSummary(output)
	case workspace.ToolReadFile:
		return readSummary(output)
	case workspace.ToolListDirectory:
		if output == "The directory is empty." {
			return "empty directory"
		}
		return countText(len(outputLines(output)), "entry", "entries")
	case workspace.ToolGlob:
		return globSummary(output)
	case workspace.ToolGrep:
		return grepSummary(args, output)
	case workspace.ToolWriteFile:
		return writeSummary(args, output)
	case workspace.ToolEditFile:
		return editSummary(args, output)
	case workspace.ToolBash:
		return lastOutputLine(output, "no output")
	case toolLoadSkill:
		return skillSummary(args, output)
	case toolReadSkillFile:
		// The output is "Skill name — path", an empty line, then the file.
		parts := strings.SplitN(output, "\n", 3)
		if len(parts) < 3 {
			return "empty file"
		}
		return countText(len(strings.Split(strings.TrimRight(parts[2], "\n"), "\n")), "line", "lines")
	case delegate.ToolName:
		// The task status line under the tool line says more.
		return ""
	}
	return firstOutputLine(output)
}

// askSummary is the person's answer. A multiple-choice answer is JSON
// that holds the selected labels or the person's own words.
func askSummary(output string) string {
	if output == "The person gave an empty answer." {
		return "empty answer"
	}
	var a workspace.Answer
	if strings.HasPrefix(output, "{") && json.Unmarshal([]byte(output), &a) == nil {
		if len(a.Selections) > 0 {
			return strings.Join(a.Selections, ", ")
		}
		return a.Text
	}
	return output
}

var (
	readLinePattern = regexp.MustCompile(`^ *\d+\t`)
	readNotePattern = regexp.MustCompile(`^\[Showed lines (\d+)-(\d+) of (\d+)\.`)
)

// readSummary counts the lines read_file returned, such as "214 lines" or
// "lines 1-2000 of 5120" when it showed part of the file.
func readSummary(output string) string {
	if output == "The file is empty." {
		return "empty file"
	}
	n := 0
	for _, line := range strings.Split(output, "\n") {
		if m := readNotePattern.FindStringSubmatch(line); m != nil {
			return fmt.Sprintf("lines %s-%s of %s", m[1], m[2], m[3])
		}
		if readLinePattern.MatchString(line) {
			n++
		}
	}
	return countText(n, "line", "lines")
}

var globNotePattern = regexp.MustCompile(`^\[Showed \d+ of (\d+) files\.`)

func globSummary(output string) string {
	if output == "No files match." {
		return "no files"
	}
	lines := outputLines(output)
	if n := len(lines); n > 0 {
		if m := globNotePattern.FindStringSubmatch(lines[n-1]); m != nil {
			total, _ := strconv.Atoi(m[1])
			return countText(total, "file", "files")
		}
	}
	return countText(len(lines), "file", "files")
}

var (
	grepMatchPattern = regexp.MustCompile(`^.*?:\d+:`)
	grepCountPattern = regexp.MustCompile(`:(\d+)$`)
)

// grepSummary counts what grep found in its output mode: matching lines,
// files, or matches per file. A trailing "+" marks a count that a limit cut
// short, such as "250+ matches".
func grepSummary(args json.RawMessage, output string) string {
	if output == "No matches." {
		return "no matches"
	}
	var in struct {
		Mode string `json:"output_mode"`
	}
	_ = json.Unmarshal(args, &in)
	more := ""
	lines := outputLines(output)
	if n := len(lines); n > 0 && strings.HasPrefix(lines[n-1], "[Showed the first ") {
		lines, more = lines[:n-1], "+"
	}
	switch in.Mode {
	case "content":
		n := 0
		for _, line := range lines {
			if grepMatchPattern.MatchString(line) {
				n++
			}
		}
		return limitedCount(n, more, "match", "matches")
	case "count":
		total := 0
		for _, line := range lines {
			if m := grepCountPattern.FindStringSubmatch(line); m != nil {
				c, _ := strconv.Atoi(m[1])
				total += c
			}
		}
		return limitedCount(total, more, "match", "matches") + " in " + countText(len(lines), "file", "files")
	}
	return limitedCount(len(lines), more, "file", "files")
}

func writeSummary(args json.RawMessage, output string) string {
	var in struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(args, &in)
	lines := countText(textLines(in.Content), "line", "lines")
	if strings.HasPrefix(output, "Created ") {
		return "created · " + lines
	}
	return "wrote " + lines
}

var editCountPattern = regexp.MustCompile(`replaced (\d+) occurrence`)

// editSummary counts the lines an edit added and removed, such as "+3 -1".
// Lines that the old and new text share at their start and end do not
// count. A replace_all edit counts every occurrence.
func editSummary(args json.RawMessage, output string) string {
	var in struct {
		Old string `json:"old_string"`
		New string `json:"new_string"`
	}
	if json.Unmarshal(args, &in) != nil {
		return firstOutputLine(output)
	}
	times := 1
	if m := editCountPattern.FindStringSubmatch(output); m != nil {
		times, _ = strconv.Atoi(m[1])
	}
	old, new := splitTextLines(in.Old), splitTextLines(in.New)
	for len(old) > 0 && len(new) > 0 && old[0] == new[0] {
		old, new = old[1:], new[1:]
	}
	for len(old) > 0 && len(new) > 0 && old[len(old)-1] == new[len(new)-1] {
		old, new = old[:len(old)-1], new[:len(new)-1]
	}
	s := fmt.Sprintf("+%d -%d", len(new)*times, len(old)*times)
	if times > 1 {
		s += fmt.Sprintf(" · %d replacements", times)
	}
	return s
}

// skillSummary names the loaded skill. Its instructions start with a
// "# name" heading.
func skillSummary(args json.RawMessage, output string) string {
	for _, line := range outputLines(output) {
		if name, ok := strings.CutPrefix(line, "# "); ok {
			return "loaded " + name
		}
	}
	var in struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(args, &in) == nil && in.Name != "" {
		return "loaded " + in.Name
	}
	return "loaded"
}

var exitCodePattern = regexp.MustCompile(`^\[Exit code (\d+)\]`)

// failedSummary is the last error line of a failed call. A shell command
// names how it ended, such as "exit 2 · undefined: foo", since its last
// line is a note in brackets.
func failedSummary(name, output string) string {
	if name != workspace.ToolBash {
		lines := outputLines(output)
		if len(lines) == 0 {
			return ""
		}
		return lines[len(lines)-1]
	}
	end := ""
	for _, line := range outputLines(output) {
		switch {
		case exitCodePattern.MatchString(line):
			end = "exit " + exitCodePattern.FindStringSubmatch(line)[1]
		case strings.HasPrefix(line, "[The command ran past the timeout"):
			end = "timed out"
		case strings.HasPrefix(line, "[The command was canceled"):
			end = "canceled"
		}
	}
	last := lastOutputLine(output, "")
	switch {
	case end == "" && last == "":
		return failedSummary("", output)
	case end == "":
		return last
	case last == "":
		return end
	}
	return end + " · " + last
}

// lastOutputLine is the last line of shell output that is not empty and
// not one of the bash tool's notes. A command's own bracketed line, such as
// "[done]", counts as output.
func lastOutputLine(output, empty string) string {
	lines := outputLines(output)
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if bashNote(line) || line == "(no output)" {
			continue
		}
		return line
	}
	return empty
}

// bashNote reports whether line is a note the bash tool adds to a command's
// output, such as "[Exit code 2]" or "[The command was canceled.]".
func bashNote(line string) bool {
	if !strings.HasSuffix(line, "]") {
		return false
	}
	for _, prefix := range []string{"[The ", "[Exit code ", "[... "} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func firstOutputLine(output string) string {
	if lines := outputLines(output); len(lines) > 0 {
		return lines[0]
	}
	return ""
}

// outputLines returns the lines of output that are not blank, trimmed.
func outputLines(output string) []string {
	var out []string
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// textLines counts the lines of text. A final newline does not start
// another line.
func textLines(s string) int {
	return len(splitTextLines(s))
}

func splitTextLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// limitedCount is countText with a mark after the number, such as "250+
// matches".
func limitedCount(n int, mark, one, many string) string {
	number, noun, _ := strings.Cut(countText(n, one, many), " ")
	return number + mark + " " + noun
}

// countText is a count and its noun, such as "1 line" or "3 entries".
func countText(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
