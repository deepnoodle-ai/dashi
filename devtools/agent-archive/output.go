package main

import (
	"fmt"
	"io"

	"github.com/deepnoodle-ai/wonton/color"
	"github.com/deepnoodle-ai/wonton/tui"
)

// Output follows Wonton's color rule: color.Enabled is off when NO_COLOR is
// set, --no-color is given, or stdout is not a terminal, as when the
// LaunchAgent writes to a log. color.Apply honors it; tui styles do not, so
// printTable adds styles only when it is on.

// printTable prints a table with a header row.
func printTable(w io.Writer, headers []string, rows [][]string) error {
	cols := make([]tui.TableColumn, len(headers))
	for i, h := range headers {
		cols[i] = tui.TableColumn{Title: h}
	}
	header := tui.NewStyle()
	if color.Enabled {
		header = header.WithBold()
	}
	// Columns are already padded by two spaces, and no row is selected.
	t := tui.Table(cols, nil).Rows(rows).ColumnGap(0).
		HeaderStyle(header).SelectedStyle(tui.NewStyle())
	if err := tui.Fprint(w, t); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w) // Fprint leaves the last line open
	return err
}

// printCount prints "label: n", in c when n is not zero.
func printCount(w io.Writer, indent, label string, n int, c color.Color) {
	line := fmt.Sprintf("%s: %d", label, n)
	if n > 0 {
		line = c.Apply(line)
	}
	fmt.Fprintf(w, "%s%s\n", indent, line)
}
