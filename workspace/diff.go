package workspace

import (
	"fmt"
	"strings"
)

const (
	diffContext  = 3
	maxDiffLines = 400
	maxDiffCells = 4_000_000
)

type diffOp struct {
	kind byte // ' ', '-' or '+'
	text string
}

// unifiedDiff returns a unified diff of old and new for a person to read. It
// returns "" when the texts are equal. It is not a patch format: it has no
// file header and it caps its length.
func unifiedDiff(name, old, new string) string {
	if old == new {
		return ""
	}
	ops := diffLines(splitLines(old), splitLines(new))
	var out []string
	out = append(out, "--- "+name, "+++ "+name)
	for _, h := range hunks(ops) {
		out = append(out, h...)
	}
	if len(out) > maxDiffLines {
		more := len(out) - maxDiffLines
		out = append(out[:maxDiffLines], fmt.Sprintf("... (%d more lines)", more))
	}
	return strings.Join(out, "\n")
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffLines trims the common head and tail, then finds a longest common
// subsequence of the middle. A very large middle becomes one replaced block.
func diffLines(a, b []string) []diffOp {
	head := 0
	for head < len(a) && head < len(b) && a[head] == b[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head &&
		a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	var ops []diffOp
	for _, l := range a[:head] {
		ops = append(ops, diffOp{' ', l})
	}
	ops = append(ops, middleDiff(a[head:len(a)-tail], b[head:len(b)-tail])...)
	for _, l := range a[len(a)-tail:] {
		ops = append(ops, diffOp{' ', l})
	}
	return ops
}

func middleDiff(a, b []string) []diffOp {
	var ops []diffOp
	if len(a)*len(b) > maxDiffCells {
		for _, l := range a {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range b {
			ops = append(ops, diffOp{'+', l})
		}
		return ops
	}
	// lcs[i][j] is the common length of a[i:] and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case j == len(b) || (i < len(a) && lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	return ops
}

// hunks groups changes with diffContext lines of context, one slice of
// output lines per hunk.
func hunks(ops []diffOp) [][]string {
	var out [][]string
	oldLine, newLine := 1, 1
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			oldLine++
			newLine++
			i++
			continue
		}
		// Start the hunk with leading context.
		start := i
		for start > 0 && i-start < diffContext && ops[start-1].kind == ' ' {
			start--
		}
		startOld, startNew := oldLine-(i-start), newLine-(i-start)
		// Extend until diffContext unchanged lines follow, or the end.
		end, quiet := i, 0
		for end < len(ops) && quiet <= 2*diffContext {
			if ops[end].kind == ' ' {
				quiet++
			} else {
				quiet = 0
			}
			end++
		}
		end -= max(quiet-diffContext, 0)
		var oldN, newN int
		lines := []string{""}
		for _, op := range ops[start:end] {
			lines = append(lines, string(op.kind)+op.text)
			if op.kind != '+' {
				oldN++
			}
			if op.kind != '-' {
				newN++
			}
		}
		// An empty side starts at the line before, as diff prints it.
		if oldN == 0 {
			startOld--
		}
		if newN == 0 {
			startNew--
		}
		lines[0] = fmt.Sprintf("@@ -%d,%d +%d,%d @@", startOld, oldN, startNew, newN)
		out = append(out, lines)
		for _, op := range ops[i:end] {
			if op.kind != '+' {
				oldLine++
			}
			if op.kind != '-' {
				newLine++
			}
		}
		i = end
	}
	return out
}
