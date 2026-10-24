package main

import (
	"database/sql"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/deepnoodle-ai/dashi/money"
)

// What each row of a report is (--by).
const (
	byModel   = "model"
	byApp     = "app"
	byProject = "project"
	bySession = "session"
)

// What each column of a report is (--per).
const (
	perDay   = "day"
	perWeek  = "week"
	perMonth = "month"
)

// defaultPeriods is how many periods a report shows without --since,
// counting the current one.
const defaultPeriods = 8

// notInSessionFiles names the rows of calls Claude Code counts in its own
// total but never writes to a session file (decision 7).
const notInSessionFiles = " (not in session files)"

// reportOptions are a report's flags, resolved. Since and Until are
// midnights in Location, and both days are included.
type reportOptions struct {
	By, Per      string
	Since, Until time.Time
	JSON         bool
	Location     *time.Location
	Home         string // printed as ~ in projects
}

// newReportOptions resolves the flags: since and until are dates
// (YYYY-MM-DD) or empty for the defaults, eight periods ending today.
func newReportOptions(by, per, since, until string, now time.Time, loc *time.Location) (reportOptions, error) {
	o := reportOptions{By: by, Per: per, Location: loc}
	o.Until = dayOf(now, loc)
	if until != "" {
		t, err := time.ParseInLocation(time.DateOnly, until, loc)
		if err != nil {
			return o, fmt.Errorf("--until: want a date like 2026-10-05: %w", err)
		}
		o.Until = t
	}
	o.Since = defaultSince(o.Until, per)
	if since != "" {
		t, err := time.ParseInLocation(time.DateOnly, since, loc)
		if err != nil {
			return o, fmt.Errorf("--since: want a date like 2026-08-17: %w", err)
		}
		o.Since = t
	}
	if o.Since.After(o.Until) {
		return o, fmt.Errorf("--since %s is after --until %s", o.Since.Format(time.DateOnly), o.Until.Format(time.DateOnly))
	}
	return o, nil
}

// dayOf is the midnight, in loc, of the day t falls on there.
func dayOf(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// periodStart is the first day of the period day falls in: the day, the
// Monday of its week, or the first of its month.
func periodStart(day time.Time, per string) time.Time {
	switch per {
	case perWeek:
		back := (int(day.Weekday()) + 6) % 7 // days since Monday
		return day.AddDate(0, 0, -back)
	case perMonth:
		return time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, day.Location())
	}
	return day
}

// nextPeriod is the start of the period after the one starting at start.
func nextPeriod(start time.Time, per string) time.Time {
	switch per {
	case perWeek:
		return start.AddDate(0, 0, 7)
	case perMonth:
		return start.AddDate(0, 1, 0)
	}
	return start.AddDate(0, 0, 1)
}

// defaultSince is the start of the eighth period back from until's,
// counting until's own.
func defaultSince(until time.Time, per string) time.Time {
	s := periodStart(until, per)
	for range defaultPeriods - 1 {
		switch per {
		case perWeek:
			s = s.AddDate(0, 0, -7)
		case perMonth:
			s = s.AddDate(0, -1, 0)
		default:
			s = s.AddDate(0, 0, -1)
		}
	}
	return s
}

// periodLabel names a period: its first day, or its month.
func periodLabel(start time.Time, per string) string {
	if per == perMonth {
		return start.Format("2006-01")
	}
	return start.Format(time.DateOnly)
}

// periods lists the periods from Since's to Until's.
func (o reportOptions) periods() []time.Time {
	var out []time.Time
	for p := periodStart(o.Since, o.Per); !p.After(o.Until); p = nextPeriod(p, o.Per) {
		out = append(out, p)
	}
	return out
}

func (o reportOptions) includes(day time.Time) bool {
	return !day.Before(o.Since) && !day.After(o.Until)
}

// parseTimestamp reads an RFC 3339 timestamp as written in a session file.
func parseTimestamp(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// openReadOnly opens the database read-only, so a report runs while copy
// does and never changes it (decision 9).
func openReadOnly(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no database at %s; run agent-archive copy first", path)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('usage') WHERE name = 'called_at'`).Scan(&n)
	if err == nil && n == 0 {
		err = fmt.Errorf("usage has no called_at column; run agent-archive parse")
	}
	if err == nil {
		err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'app_usage'`).Scan(&n)
		if err == nil && n == 0 {
			err = fmt.Errorf("no app_usage table; run agent-archive parse")
		}
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

// sessionInfo is one session: a root and its subagents, in one app.
type sessionInfo struct {
	App, SessionID string
	WorkingDir     string // the root's
	Project        string
	Remotes        map[string]bool
	StartedAt      string
	StartedDay     time.Time
	// AppCost is Claude Code's own total, from its root file; nil when
	// it recorded none.
	AppCost *money.Amount
	// Total is every priced row of the session on any day, plus its calls
	// not in session files: the Dashi side of decision 8.
	Total amount
	// LastCallDay is the day of its latest model call.
	LastCallDay time.Time
	// Archived sums its usage rows by model, to subtract from app_usage.
	Archived map[string]tokenCounts
}

// spendRow is some priced model calls on one day: a usage row, or a
// session's calls not in session files for one model.
type spendRow struct {
	Session *sessionInfo
	Model   string // as shown
	Day     time.Time
	Amount  amount
	Tokens  tokenCounts
	// Unpriced says why the model has no price, empty when it has one.
	Unpriced       string
	PriceUpdatedAt string
}

// reportData is everything a report shows.
type reportData struct {
	Sessions map[string]*sessionInfo
	// Rows are the spend rows on days the report includes.
	Rows []spendRow
	// OlderCopies counts session files parsed by an older parser.
	OlderCopies int
}

func sessionKey(app, sessionID string) string { return app + "\x00" + sessionID }

// loadReport reads and prices the archive.
func loadReport(db *sql.DB, o reportOptions) (*reportData, error) {
	d := &reportData{Sessions: map[string]*sessionInfo{}}
	if err := db.QueryRow(`SELECT COUNT(*) FROM copies WHERE superseded_at IS NULL AND kind IN (?, ?)
		AND parse_status = ? AND parser_version < ?`, kindSession, kindSubagent, parseStatusParsed, parserVersion).
		Scan(&d.OlderCopies); err != nil {
		return nil, err
	}
	bySession, err := loadSessions(db, d.Sessions, o)
	if err != nil {
		return nil, err
	}
	book := priceBook{}
	if err := loadUsage(db, d, bySession, book, o); err != nil {
		return nil, err
	}
	if err := loadAppUsage(db, d, bySession, book, o); err != nil {
		return nil, err
	}
	return d, nil
}

// loadSessions fills sessions and returns each current copy's session.
func loadSessions(db *sql.DB, sessions map[string]*sessionInfo, o reportOptions) (map[int64]*sessionInfo, error) {
	rows, err := db.Query(`SELECT s.copy_id, s.app, COALESCE(s.session_id, ''), s.agent_id, COALESCE(s.working_dir, ''),
			COALESCE(s.started_at, ''), COALESCE(s.git_remote_url, ''), s.app_cost_usd
		FROM sessions s JOIN copies c USING (copy_id)
		WHERE c.superseded_at IS NULL
		ORDER BY s.started_at, s.copy_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byCopy := map[int64]*sessionInfo{}
	for rows.Next() {
		var (
			id                                    int64
			app, sid, agent, dir, started, remote string
			cost                                  sql.NullFloat64
		)
		if err := rows.Scan(&id, &app, &sid, &agent, &dir, &started, &remote, &cost); err != nil {
			return nil, err
		}
		key := sessionKey(app, sid)
		if sid == "" {
			key = sessionKey(app, fmt.Sprintf("copy-%d", id)) // no session id: a session of one, as in parse
		}
		s := sessions[key]
		if s == nil {
			s = &sessionInfo{App: app, SessionID: sid, WorkingDir: dir, Remotes: map[string]bool{},
				Archived: map[string]tokenCounts{}}
			sessions[key] = s
		}
		byCopy[id] = s
		if started != "" && (s.StartedAt == "" || started < s.StartedAt) {
			s.StartedAt = started
		}
		if agent == "" && dir != "" {
			s.WorkingDir = dir // the root's
		}
		if remote != "" {
			s.Remotes[remote] = true
		}
		if agent == "" && cost.Valid {
			c := money.AmountFromNanos(int64(math.Round(cost.Float64 * 1e9)))
			if s.AppCost != nil {
				c += *s.AppCost
			}
			s.AppCost = &c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	dirs := map[string]*projectDir{}
	for _, s := range sessions {
		if t, ok := parseTimestamp(s.StartedAt); ok {
			s.StartedDay = dayOf(t, o.Location)
		}
		pd := dirs[s.WorkingDir]
		if pd == nil {
			pd = &projectDir{Dir: s.WorkingDir, Remotes: map[string]bool{}}
			dirs[s.WorkingDir] = pd
		}
		for r := range s.Remotes {
			pd.Remotes[r] = true
		}
	}
	list := make([]projectDir, 0, len(dirs))
	for _, pd := range dirs {
		list = append(list, *pd)
	}
	names := projects(list, o.Home)
	for _, s := range sessions {
		s.Project = names[s.WorkingDir]
	}
	return byCopy, nil
}

// loadUsage prices every usage row (decision 2) and keeps those on days
// the report includes. A row with no called_at falls on its session's
// start day (decision 5).
func loadUsage(db *sql.DB, d *reportData, byCopy map[int64]*sessionInfo, book priceBook, o reportOptions) error {
	rows, err := db.Query(`SELECT copy_id, model, input_tokens, cache_read_input_tokens, cache_creation_input_tokens,
			cache_creation_1h_input_tokens, output_tokens, reasoning_tokens, web_search_requests, speed,
			COALESCE(called_at, '')
		FROM usage ORDER BY copy_id, line`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id               int64
			model, speed, at string
			t                tokenCounts
		)
		if err := rows.Scan(&id, &model, &t.Input, &t.CacheRead, &t.CacheWrite, &t.CacheWrite1h,
			&t.Output, &t.Reasoning, &t.WebSearch, &speed, &at); err != nil {
			return err
		}
		s := byCopy[id]
		if s == nil {
			continue // a superseded copy keeps no usage rows
		}
		day := s.StartedDay
		if ts, ok := parseTimestamp(at); ok {
			day = dayOf(ts, o.Location)
		}
		p := book.lookup(s.App, model)
		a, why := priceCall(p, s.App, t, speed)
		s.Total.add(a)
		if day.After(s.LastCallDay) {
			s.LastCallDay = day
		}
		if s.App == appClaudeCode {
			sum := s.Archived[model]
			sum.add(t)
			s.Archived[model] = sum
		}
		if !o.includes(day) {
			continue
		}
		if model == "" {
			model = "(none)"
		}
		d.Rows = append(d.Rows, spendRow{Session: s, Model: model, Day: day, Amount: a, Tokens: t,
			Unpriced: why, PriceUpdatedAt: updatedAt(p)})
	}
	return rows.Err()
}

func updatedAt(p modelPrice) string {
	if p.Pricing == nil {
		return ""
	}
	return p.Pricing.UpdatedAt
}

// loadAppUsage adds, per Claude Code session and model, the calls not in
// session files: app_usage less the session's usage rows, each column
// clamped at zero (decision 7). They fall on the session's last call day.
func loadAppUsage(db *sql.DB, d *reportData, byCopy map[int64]*sessionInfo, book priceBook, o reportOptions) error {
	rows, err := db.Query(`SELECT a.copy_id, a.model, a.input_tokens, a.cache_read_input_tokens,
			a.cache_creation_input_tokens, a.output_tokens, a.reasoning_tokens, a.web_search_requests
		FROM app_usage a JOIN copies c USING (copy_id) WHERE c.superseded_at IS NULL`)
	if err != nil {
		return err
	}
	type sessionModel struct {
		s     *sessionInfo
		model string
	}
	totals := map[sessionModel]tokenCounts{}
	var order []sessionModel
	for rows.Next() {
		var (
			id    int64
			model string
			t     tokenCounts
		)
		if err := rows.Scan(&id, &model, &t.Input, &t.CacheRead, &t.CacheWrite, &t.Output, &t.Reasoning, &t.WebSearch); err != nil {
			rows.Close()
			return err
		}
		s := byCopy[id]
		if s == nil {
			continue
		}
		k := sessionModel{s, appUsageModel(model)}
		sum, seen := totals[k]
		if !seen {
			order = append(order, k)
		}
		sum.add(t)
		totals[k] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range order {
		left := totals[k].minus(k.s.Archived[k.model])
		if left == (tokenCounts{}) {
			continue
		}
		p := book.lookup(k.s.App, k.model)
		a, why := priceNotInSessionFiles(p, left)
		k.s.Total.add(a)
		day := k.s.LastCallDay
		if day.IsZero() {
			day = k.s.StartedDay
		}
		if !o.includes(day) {
			continue
		}
		d.Rows = append(d.Rows, spendRow{Session: k.s, Model: k.model + notInSessionFiles, Day: day,
			Amount: a, Tokens: left, Unpriced: why, PriceUpdatedAt: updatedAt(p)})
	}
	return nil
}

// runReport writes the report on the archive in dir.
func runReport(dir string, o reportOptions, w, warn io.Writer) error {
	db, err := openReadOnly(filepath.Join(dir, databaseName))
	if err != nil {
		return err
	}
	defer db.Close()
	d, err := loadReport(db, o)
	if err != nil {
		return err
	}
	if d.OlderCopies > 0 {
		fmt.Fprintf(warn, "warning: %d session files were parsed by an older agent-archive; run agent-archive parse\n", d.OlderCopies)
	}
	if o.By == bySession {
		return printSessions(w, warn, d, o)
	}
	return printSpend(w, warn, d, o)
}
