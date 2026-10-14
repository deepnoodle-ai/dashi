package listtool

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/decide/internal/state"
	"github.com/deepnoodle-ai/decide"
)

// judgeOutput is what one judge_items call found, ready to write for the
// model.
type judgeOutput struct {
	list, model  string
	took         time.Duration
	questions    []question
	conds        []condition
	items        []judged
	maxItemBytes int
	// show is how many items the model asked to see.
	show int
}

// Examples named in the result's last lines.
const (
	closeExamples = 5
	failExamples  = 3
)

// fit writes the result with at most v.show items, listing fewer until
// it fits under limit bytes, so Dashi neither shortens it nor drops its
// Data. A limit of zero or less means no limit.
func (v judgeOutput) fit(limit int) string {
	show := v.show
	for {
		out := v.render(show, closeExamples, failExamples)
		if limit <= 0 || len(out) <= limit {
			return out
		}
		if show == 0 {
			break
		}
		show /= 2
	}
	return state.Cut(v.render(0, 0, 0), limit)
}

// match is a judged item that meets every find condition.
type match struct {
	judged
	probs []float64 // one per condition
	rank  float64   // the product of probs
}

func (v judgeOutput) render(show, closeShown, failShown int) string {
	var b strings.Builder
	var ok []judged
	for _, it := range v.items {
		if it.err == nil {
			ok = append(ok, it)
		}
	}
	fmt.Fprintf(&b, "Judged %s from %s", itemCount(len(v.items)), v.list)
	if v.model != "" {
		fmt.Fprintf(&b, " with %s", v.model)
	}
	fmt.Fprintf(&b, " in %.1fs.\n", v.took.Seconds())

	tallies := make([]string, len(v.questions))
	for i, q := range v.questions {
		parts := make([]string, len(q.answers()))
		for j, answer := range q.answers() {
			n := 0
			for _, it := range ok {
				if a, _ := likely(it.answers[q.Key]); a == answer {
					n++
				}
			}
			parts[j] = fmt.Sprintf("%s %d", answer, n)
		}
		tallies[i] = q.Key + " " + strings.Join(parts, ", ")
	}
	// The counts are of most likely answers. A match needs P above 0.5,
	// which a choice's most likely answer may not reach.
	b.WriteString("Most likely answers: " + strings.Join(tallies, "; ") + ".\n")

	var close []string
	if len(v.conds) > 0 {
		var matches []match
		for _, it := range ok {
			m := match{judged: it, rank: 1}
			hit, near := true, ""
			for _, c := range v.conds {
				p := probability(it.answers[c.q.Key], c.answer)
				m.probs = append(m.probs, p)
				m.rank *= p
				hit = hit && p > 0.5
				// A close call is a P near 0.5, or a most likely answer
				// that is not above 0.5, which a choice can have. Either
				// way the item is listed, never left out.
				leads := p <= 0.5 && likelyIs(it.answers[c.q.Key], c.answer)
				if near == "" && (p >= closeLow && p <= closeHigh || leads) {
					near = fmt.Sprintf("%s (%s %.2f)", it.id, c.q.Key, p)
				}
			}
			if hit {
				matches = append(matches, m)
			}
			if near != "" {
				close = append(close, near)
			}
		}
		slices.SortStableFunc(matches, func(x, y match) int { return cmp.Compare(y.rank, x.rank) })
		wanted := make([]string, len(v.conds))
		for i, c := range v.conds {
			wanted[i] = c.q.Key + "=" + c.answer
		}
		fmt.Fprintf(&b, "Matching %s: %s", strings.Join(wanted, ", "), itemCount(len(matches)))
		if len(matches) == 0 {
			b.WriteString(".\n")
		} else {
			b.WriteString(", best first:\n")
		}
		for _, m := range matches[:min(show, len(matches))] {
			b.WriteString("  " + m.id)
			for i, c := range v.conds {
				fmt.Fprintf(&b, "  %s %.2f", c.q.Key, m.probs[i])
			}
			b.WriteString("\n")
		}
		if more := len(matches) - show; more > 0 {
			fmt.Fprintf(&b, "  ... and %d more. %s\n", more, moreHint)
		}
	} else {
		b.WriteString("Items in list order, with each most likely answer:\n")
		for i, it := range ok {
			near := ""
			if i < show {
				b.WriteString("  " + it.id)
			}
			for _, q := range v.questions {
				answer, p := likely(it.answers[q.Key])
				if i < show {
					fmt.Fprintf(&b, "  %s %s %.2f", q.Key, answer, p)
				}
				if near == "" && p <= closeHigh {
					near = fmt.Sprintf("%s (%s %s %.2f)", it.id, q.Key, answer, p)
				}
			}
			if i < show {
				b.WriteString("\n")
			}
			if near != "" {
				close = append(close, near)
			}
		}
		if more := len(ok) - show; more > 0 {
			fmt.Fprintf(&b, "  ... and %d more. %s\n", more, moreHint)
		}
	}

	var last []string
	if len(close) > 0 {
		line := fmt.Sprintf("Close calls: %d", len(close))
		if closeShown > 0 {
			line += ", such as " + strings.Join(close[:min(closeShown, len(close))], ", ")
		}
		last = append(last, line+".")
	}
	if cut := countFunc(v.items, func(it judged) bool { return it.cut }); cut > 0 {
		last = append(last, fmt.Sprintf("Judged on their first %s bytes only: %d.", count(v.maxItemBytes), cut))
	}
	if failed := len(v.items) - len(ok); failed > 0 {
		line := fmt.Sprintf("Failed: %d.", failed)
		shown := 0
		for _, it := range v.items {
			if it.err == nil {
				continue
			}
			if shown == failShown {
				line += " ..."
				break
			}
			line += fmt.Sprintf(" %s: %s.", it.id, reason(it.err))
			shown++
		}
		last = append(last, line)
	}
	if len(last) > 0 {
		b.WriteString(strings.Join(last, " ") + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// moreHint tells the model how to see items the result left out.
var moreHint = fmt.Sprintf("Set show (at most %d), or narrow the query.", maxShow)

// likelyIs says whether answer is a's most likely answer.
func likelyIs(a decide.Answer, answer string) bool {
	got, _ := likely(a)
	return got == answer
}

func countFunc[T any](s []T, f func(T) bool) int {
	n := 0
	for _, v := range s {
		if f(v) {
			n++
		}
	}
	return n
}

// itemCount writes n items, such as "1 item" or "12 items".
func itemCount(n int) string {
	if n == 1 {
		return "1 item"
	}
	return count(n) + " items"
}
