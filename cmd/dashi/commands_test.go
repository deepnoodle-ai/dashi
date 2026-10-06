package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCommandHelpDescribesCopyControlsForEachMode(t *testing.T) {
	// Request inline help first to catch changes to the shared fullscreen rows.
	for _, inline := range []bool{true, false} {
		name := "fullscreen"
		if inline {
			name = "inline"
		}
		t.Run(name, func(t *testing.T) {
			s := newScreen("fixture")
			c := newHarness(t).app.ctrl
			c.ui = s
			if inline {
				c.ui = newInlineScreen(s, &bytes.Buffer{})
			}
			block := c.helpBlock() // The block emitted by /help and /?.
			want := []string{
				"Ctrl+C        copy selection; otherwise interrupt or press twice to exit",
				"F2            toggle native selection without changing the draft",
			}
			if inline {
				for _, forbidden := range want {
					if strings.Contains(block.Text, forbidden) {
						t.Errorf("inline help promises fullscreen behavior %q", forbidden)
					}
				}
				want = []string{
					"Ctrl+C        interrupt or press twice to exit",
					"F2            native terminal selection is already enabled",
					"Use your terminal's selection and copy controls in inline mode.",
				}
			}
			for _, text := range want {
				if !strings.Contains(block.Text, text) {
					t.Errorf("help lacks %q:\n%s", text, block.Text)
				}
			}
		})
	}
}
