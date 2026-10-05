package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/money"
)

// How a session's app cost compares with Dashi's amount (decision 8).
const (
	agreementAgrees      = "agrees"
	agreementLower       = "lower"
	agreementHigher      = "higher"
	agreementNotCompared = "not compared"
)

// agreementTolerance is the larger of 1% of x and one cent.
func agreementTolerance(x money.Amount) money.Amount {
	return max(x/100, money.MustParseAmount("0.01"))
}

// compareCost compares a session's app cost with Dashi's amount for it,
// or its range. diff is the app cost less the nearest end of the range,
// zero inside it. A session with an unpriced or fast row is not compared.
func compareCost(dashi amount, app money.Amount) (agreement string, diff money.Amount) {
	if dashi.LowerBound {
		return agreementNotCompared, 0
	}
	switch {
	case app < dashi.Low:
		diff = app - dashi.Low
	case app > dashi.High:
		diff = app - dashi.High
	}
	switch {
	case app < dashi.Low-agreementTolerance(dashi.Low):
		return agreementLower, diff
	case app > dashi.High+agreementTolerance(dashi.High):
		return agreementHigher, diff
	}
	return agreementAgrees, diff
}

// formatMoney prints an amount in dollars to the cent, such as $1,234.56.
func formatMoney(a money.Amount) string {
	n := a.Nanos()
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	cents := (n + 5_000_000) / 10_000_000
	if cents == 0 {
		sign = ""
	}
	return fmt.Sprintf("%s$%s.%02d", sign, groupDigits(cents/100), cents%100)
}

// groupDigits prints n with thousands separators.
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// formatAmount prints a table cell: its high estimate, + for a lower
// bound, ? when nothing in it could be priced, and blank when empty.
func formatAmount(a amount) string {
	switch {
	case a.Rows == 0:
		return ""
	case a.High == 0 && a.LowerBound:
		return "?"
	case a.LowerBound:
		return formatMoney(a.High) + "+"
	}
	return formatMoney(a.High)
}

// writeTable prints aligned columns; columns from right on are numbers,
// aligned right.
func writeTable(w io.Writer, headers []string, rows [][]string, right int) {
	widths := make([]int, len(headers))
	for _, r := range append([][]string{headers}, rows...) {
		for i, c := range r {
			widths[i] = max(widths[i], utf8.RuneCountInString(c))
		}
	}
	for _, r := range append([][]string{headers}, rows...) {
		var b strings.Builder
		for i, c := range r {
			pad := strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c))
			if i > 0 {
				b.WriteString("  ")
			}
			if i >= right {
				b.WriteString(pad + c)
			} else if i < len(r)-1 {
				b.WriteString(c + pad)
			} else {
				b.WriteString(c)
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}

// rowName is what a spend row counts toward under --by.
func rowName(r spendRow, by string) string {
	switch by {
	case byApp:
		return r.Session.App
	case byProject:
		return r.Session.Project
	}
	return r.Model
}

// spendJSON is one row and period of --json output.
type spendJSON struct {
	By             string        `json:"by"`
	Name           string        `json:"name"`
	Period         string        `json:"period"`
	Amount         money.Amount  `json:"amount"`
	Low            *money.Amount `json:"low,omitempty"`
	High           *money.Amount `json:"high,omitempty"`
	LowerBound     bool          `json:"lower_bound"`
	UnpricedCalls  int           `json:"unpriced_calls"`
	PriceUpdatedAt string        `json:"price_updated_at,omitempty"`
}

// printSpend prints spend by model, app, or project per period, the
// unpriced models, and the agreement line.
func printSpend(w, warn io.Writer, d *reportData, o reportOptions) error {
	periods := o.periods()
	type tableRow struct {
		name    string
		cells   map[time.Time]*amount
		total   amount
		updated string
	}
	byName := map[string]*tableRow{}
	columnTotals := map[time.Time]*amount{}
	var total amount
	for _, r := range d.Rows {
		name := rowName(r, o.By)
		tr := byName[name]
		if tr == nil {
			tr = &tableRow{name: name, cells: map[time.Time]*amount{}, updated: r.PriceUpdatedAt}
			byName[name] = tr
		}
		p := periodStart(r.Day, o.Per)
		if tr.cells[p] == nil {
			tr.cells[p] = &amount{}
		}
		if columnTotals[p] == nil {
			columnTotals[p] = &amount{}
		}
		tr.cells[p].add(r.Amount)
		columnTotals[p].add(r.Amount)
		tr.total.add(r.Amount)
		total.add(r.Amount)
	}
	table := make([]*tableRow, 0, len(byName))
	for _, tr := range byName {
		table = append(table, tr)
	}
	slices.SortFunc(table, func(a, b *tableRow) int {
		return cmp.Or(cmp.Compare(b.total.High, a.total.High), cmp.Compare(a.name, b.name))
	})

	if o.JSON {
		enc := json.NewEncoder(w)
		for _, tr := range table {
			for _, p := range periods {
				a := tr.cells[p]
				if a == nil {
					continue
				}
				j := spendJSON{By: o.By, Name: tr.name, Period: periodLabel(p, o.Per), Amount: a.High,
					LowerBound: a.LowerBound, UnpricedCalls: a.UnpricedCalls}
				if a.isRange() {
					low, high := a.Low, a.High
					j.Low, j.High = &low, &high
				}
				if o.By == byModel {
					j.PriceUpdatedAt = tr.updated
				}
				if err := enc.Encode(j); err != nil {
					return err
				}
			}
		}
		fmt.Fprintln(warn, agreementLine(d, o))
		return nil
	}

	fmt.Fprintf(w, "List-price spend by %s per %s, %s to %s (local time)\n\n", o.By, o.Per,
		o.Since.Format(time.DateOnly), o.Until.Format(time.DateOnly))
	headers := []string{strings.ToUpper(o.By[:1]) + o.By[1:]}
	for _, p := range periods {
		headers = append(headers, periodLabel(p, o.Per))
	}
	headers = append(headers, "Total")
	var rows [][]string
	line := func(name string, cell func(time.Time) amount, t amount) {
		r := []string{name}
		for _, p := range periods {
			r = append(r, formatAmount(cell(p)))
		}
		rows = append(rows, append(r, formatAmount(t)))
	}
	for _, tr := range table {
		line(tr.name, func(p time.Time) amount { return deref(tr.cells[p]) }, tr.total)
	}
	line("Total", func(p time.Time) amount { return deref(columnTotals[p]) }, total)
	writeTable(w, headers, rows, 1)
	fmt.Fprintln(w)
	if total.LowerBound {
		fmt.Fprintln(w, "+ lower bound: some calls could not be priced (unpriced model, fast mode, or an unpriced tool request).")
	}
	if total.isRange() {
		fmt.Fprintf(w, "High estimates: Codex cache writes and Claude Code cache writes not in session files; --json gives the low end (%s in all).\n",
			formatMoney(total.Low))
	}
	if total.LowerBound || total.isRange() {
		fmt.Fprintln(w)
	}
	printUnpriced(w, d)
	fmt.Fprintln(w, agreementLine(d, o))
	return nil
}

func deref(a *amount) amount {
	if a == nil {
		return amount{}
	}
	return *a
}

// printUnpriced lists the models with no price and why (decision 3).
func printUnpriced(w io.Writer, d *reportData) {
	type unpriced struct {
		model, app, why string
		calls           int
		tokens          tokenCounts
	}
	byModel := map[string]*unpriced{}
	for _, r := range d.Rows {
		if r.Unpriced == "" {
			continue
		}
		k := r.Session.App + "\x00" + r.Model
		u := byModel[k]
		if u == nil {
			u = &unpriced{model: r.Model, app: r.Session.App, why: r.Unpriced}
			byModel[k] = u
		}
		u.calls += r.Amount.UnpricedCalls
		u.tokens.add(r.Tokens)
	}
	if len(byModel) == 0 {
		return
	}
	list := make([]*unpriced, 0, len(byModel))
	for _, u := range byModel {
		list = append(list, u)
	}
	slices.SortFunc(list, func(a, b *unpriced) int {
		return cmp.Or(cmp.Compare(b.calls, a.calls), cmp.Compare(a.model, b.model))
	})
	var rows [][]string
	for _, u := range list {
		calls := groupDigits(int64(u.calls))
		if u.calls == 0 {
			calls = "-" // calls not in session files are not counted
		}
		rows = append(rows, []string{u.model, u.app, u.why, calls, groupDigits(int64(u.tokens.Input)),
			groupDigits(int64(u.tokens.CacheRead)), groupDigits(int64(u.tokens.CacheWrite)),
			groupDigits(int64(u.tokens.Output))})
	}
	fmt.Fprintln(w, "Unpriced models, not in the amounts above:")
	fmt.Fprintln(w)
	writeTable(w, []string{"Model", "App", "Why", "Calls", "Input", "Cache read", "Cache write", "Output"}, rows, 3)
	fmt.Fprintln(w)
}

// sessionResult is one session in the report, compared with its app cost
// when it has one.
type sessionResult struct {
	s         *sessionInfo
	agreement string // empty without an app cost
	diff      money.Amount
}

// sessionResults are the sessions started on days the report includes.
func sessionResults(d *reportData, o reportOptions) []sessionResult {
	var out []sessionResult
	for _, s := range d.Sessions {
		if !o.includes(s.StartedDay) || (s.Total.Rows == 0 && s.AppCost == nil) {
			continue
		}
		r := sessionResult{s: s}
		if s.AppCost != nil {
			r.agreement, r.diff = compareCost(s.Total, *s.AppCost)
		}
		out = append(out, r)
	}
	return out
}

// agreementLine counts the sessions that agree with their app cost, are
// lower or higher, or were not compared.
func agreementLine(d *reportData, o reportOptions) string {
	n := map[string]int{}
	for _, r := range sessionResults(d, o) {
		n[r.agreement]++
	}
	return fmt.Sprintf("Claude Code's own session totals against Dashi's: %d agree, %d lower, %d higher, %d not compared",
		n[agreementAgrees], n[agreementLower], n[agreementHigher], n[agreementNotCompared])
}

// sessionJSON is one session of --by session --json output.
type sessionJSON struct {
	SessionID     string        `json:"session_id"`
	App           string        `json:"app"`
	Project       string        `json:"project"`
	StartedAt     string        `json:"started_at"`
	Amount        money.Amount  `json:"amount"`
	Low           *money.Amount `json:"low,omitempty"`
	High          *money.Amount `json:"high,omitempty"`
	LowerBound    bool          `json:"lower_bound"`
	UnpricedCalls int           `json:"unpriced_calls"`
	AppCost       *money.Amount `json:"app_cost,omitempty"`
	Difference    *money.Amount `json:"difference,omitempty"`
	Agreement     string        `json:"agreement,omitempty"`
}

// printSessions lists sessions with Dashi's amount, the app's own total,
// and the difference, largest difference first; sessions without an app
// cost follow, largest amount first.
func printSessions(w, warn io.Writer, d *reportData, o reportOptions) error {
	results := sessionResults(d, o)
	compared := func(r sessionResult) bool {
		return r.agreement != "" && r.agreement != agreementNotCompared
	}
	abs := func(a money.Amount) money.Amount { return max(a, -a) }
	slices.SortFunc(results, func(a, b sessionResult) int {
		if compared(a) != compared(b) {
			if compared(a) {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(abs(b.diff), abs(a.diff)), cmp.Compare(b.s.Total.High, a.s.Total.High),
			cmp.Compare(a.s.SessionID, b.s.SessionID))
	})
	if o.JSON {
		enc := json.NewEncoder(w)
		for _, r := range results {
			t := r.s.Total
			j := sessionJSON{SessionID: r.s.SessionID, App: r.s.App, Project: r.s.Project, StartedAt: r.s.StartedAt,
				Amount: t.High, LowerBound: t.LowerBound, UnpricedCalls: t.UnpricedCalls,
				AppCost: r.s.AppCost, Agreement: r.agreement}
			if t.isRange() {
				j.Low, j.High = &t.Low, &t.High
			}
			if compared(r) {
				diff := r.diff
				j.Difference = &diff
			}
			if err := enc.Encode(j); err != nil {
				return err
			}
		}
		fmt.Fprintln(warn, agreementLine(d, o))
		return nil
	}
	fmt.Fprintf(w, "Sessions started %s to %s (local time), largest difference first\n\n",
		o.Since.Format(time.DateOnly), o.Until.Format(time.DateOnly))
	var rows [][]string
	for _, r := range results {
		t := r.s.Total
		cost := formatAmount(t)
		if t.isRange() && !t.LowerBound {
			cost = formatMoney(t.Low) + "–" + formatMoney(t.High)
		}
		appCost, diff := "", ""
		if r.s.AppCost != nil {
			appCost = formatMoney(*r.s.AppCost)
		}
		if compared(r) {
			diff = formatMoney(r.diff)
		}
		rows = append(rows, []string{r.s.SessionID, r.s.App, r.s.Project, r.s.StartedDay.Format(time.DateOnly),
			cost, appCost, diff})
	}
	writeTable(w, []string{"Session", "App", "Project", "Started", "Dashi cost", "App cost", "Difference"}, rows, 4)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Difference: the app's own total less Dashi's amount, or the nearest end of its range.")
	fmt.Fprintln(w, agreementLine(d, o))
	return nil
}
