package workspace

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Printable returns a copy of r that is safe to show in a terminal. Every
// text field (Tool, Summary, Preview, Dir, Command) goes through
// PrintableText. Args is set to nil, because raw JSON is not for display.
//
// The fields hold text that the model controls. A host that prints them
// raw lets the model clear or rewrite the approval prompt with escape
// sequences or carriage returns. A host must show this form, not r.
func (r ApprovalRequest) Printable() ApprovalRequest {
	return ApprovalRequest{
		Tool:          PrintableText(r.Tool),
		Summary:       PrintableText(r.Summary),
		Preview:       PrintableText(r.Preview),
		Dir:           PrintableText(r.Dir),
		Command:       PrintableText(r.Command),
		RememberScope: PrintableText(r.RememberScope),
	}
}

// PrintableText replaces each character that can change what a terminal
// shows with a visible mark. It keeps newline and tab. Each C0 control
// (including ESC and carriage return) and DEL become the matching
// Unicode control picture (for example U+241B for ESC and U+240D for
// carriage return). A C1 control, a bidirectional override, and a byte
// that is not valid UTF-8 become a mark such as <U+0085> or <0xFF>.
func PrintableText(s string) string {
	clean := true
	for _, r := range s {
		if unsafeRune(r) {
			clean = false
			break
		}
	}
	if clean && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, "<0x%02X>", s[i])
		case r < 0x20 && r != '\n' && r != '\t':
			b.WriteRune(0x2400 + r)
		case r == 0x7f:
			b.WriteRune(0x2421)
		case unsafeRune(r):
			fmt.Fprintf(&b, "<U+%04X>", r)
		default:
			b.WriteRune(r)
		}
		i += n
	}
	return b.String()
}

func unsafeRune(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20, r >= 0x7f && r <= 0x9f:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true // bidirectional overrides and isolates
	case r == 0x2028 || r == 0x2029 || r == 0x200e || r == 0x200f:
		return true
	}
	return false
}
