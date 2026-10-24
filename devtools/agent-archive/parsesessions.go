package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/claudecode"
	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/codex"
	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/sessionfile"
)

// parsePending parses every copy not yet parsed by this parser version and
// stores the results, which are recomputed from the copies, never logged.
// It runs in two passes (design/agent-archive-parse.md, decision 1):
//
//   - Pass 1 reads each pending copy alone for phase 1's facts and
//     line_types, then links each Codex subagent to its root's session.
//   - Pass 2 parses each session with a copy still pending, all its
//     current copies together, and writes them in one transaction. A
//     session copy's parser_version is set here, so a crash between the
//     passes parses the session again.
//
// The summary's warnings belong to the run, such as new unknown line types.
func parsePending(db *sql.DB, archiveDir string) (parseSummary, error) {
	var sum parseSummary
	a := archive{Dir: archiveDir}
	pending, err := pendingCopies(db)
	if err != nil {
		return sum, err
	}
	titlesChanged := false
	for _, p := range pending {
		res := parseCopy(a.abs(p.copyPath), p.app, p.kind)
		// A current session copy that parsed waits for pass 2.
		done := res.Session == nil || p.superseded
		if err := storeParse(db, p.id, p.app, res, done); err != nil {
			return sum, err
		}
		titlesChanged = titlesChanged || p.kind == kindSessionTitles
		if p.kind == kindSubagentMeta && !p.superseded {
			// A new or changed .meta.json changes its subagent's facts, so
			// pass 2 parses that subagent's session again.
			if _, err := db.Exec(`UPDATE copies SET parser_version = NULL WHERE copy_path = ? AND superseded_at IS NULL`,
				strings.TrimSuffix(p.copyPath, ".meta.json")+".jsonl.gz"); err != nil {
				return sum, err
			}
		}
	}
	sum.Copies = len(pending)
	if err := linkCodexSessions(db); err != nil {
		return sum, err
	}
	p := &sessionParser{db: db, archive: a, unknown: map[string]int{}}
	if err := p.parseSessions(titlesChanged); err != nil {
		return sum, err
	}
	sum.Sessions, sum.SessionCopies, sum.UsageRows = p.sessions, p.copies, p.usageRows
	sum.Warnings = p.warnings()
	return sum, nil
}

// parseSummary is what parsePending did.
type parseSummary struct {
	Copies        int // pass 1: copies read alone
	Sessions      int // pass 2: sessions parsed
	SessionCopies int // pass 2: their copies
	UsageRows     int
	Warnings      []string
}

type pendingCopy struct {
	id                  int64
	app, kind, copyPath string
	superseded          bool
}

func pendingCopies(db *sql.DB) ([]pendingCopy, error) {
	rows, err := db.Query(`SELECT copy_id, app, kind, copy_path, superseded_at IS NOT NULL FROM copies
		WHERE parser_version IS NULL OR parser_version != ? ORDER BY copy_id`, parserVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingCopy
	for rows.Next() {
		var p pendingCopy
		if err := rows.Scan(&p.id, &p.app, &p.kind, &p.copyPath, &p.superseded); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// storeParse stores pass 1's facts for one copy. done sets the parser
// version; otherwise pass 2 sets it.
func storeParse(db *sql.DB, copyID int64, app string, res parseResult, done bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var lineTypes, parseError, version any
	if res.LineTypes != nil {
		b, err := json.Marshal(res.LineTypes) // map keys are sorted
		if err != nil {
			return err
		}
		lineTypes = string(b)
	}
	if res.Error != "" {
		parseError = res.Error
	}
	if done {
		version = parserVersion
	}
	if _, err := tx.Exec(`UPDATE copies SET parse_status = ?, parse_error = ?, parser_version = ?, line_types = ?
		WHERE copy_id = ?`, res.Status, parseError, version, lineTypes, copyID); err != nil {
		return err
	}
	if err := deleteFacts(tx, copyID); err != nil {
		return err
	}
	if s := res.Session; s != nil {
		if _, err := tx.Exec(`INSERT INTO sessions (copy_id, app, session_id, agent_id, working_dir, started_at, app_version,
			parent_agent_id, forked_from_session_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, copyID, app,
			nullIfEmpty(s.SessionID), s.AgentID, nullIfEmpty(s.WorkingDir), nullIfEmpty(s.StartedAt),
			nullIfEmpty(s.AppVersion), nullIfEmpty(s.ParentThreadID), nullIfEmpty(s.ForkedFromSessionID)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// linkCodexSessions sets each Codex subagent's session_id to its root's
// id. A nested subagent's file names only its parent, which pass 1 stores
// in parent_agent_id; linking walks those parents up to the root. A
// subagent whose session changes is parsed again.
func linkCodexSessions(db *sql.DB) error {
	rows, err := db.Query(`SELECT s.copy_id, COALESCE(s.session_id, ''), s.agent_id, COALESCE(s.parent_agent_id, '')
		FROM sessions s JOIN copies c USING (copy_id)
		WHERE s.app = ? AND c.superseded_at IS NULL ORDER BY s.started_at, s.copy_id`, appCodex)
	if err != nil {
		return err
	}
	type agent struct {
		copyID                   int64
		session, agentID, parent string
	}
	var agents []*agent
	for rows.Next() {
		a := &agent{}
		if err := rows.Scan(&a.copyID, &a.session, &a.agentID, &a.parent); err != nil {
			rows.Close()
			return err
		}
		agents = append(agents, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	byThread := map[string]*agent{}
	for _, a := range agents {
		if a.agentID == "" {
			byThread[a.session] = a
		} else {
			byThread[a.agentID] = a
		}
	}
	// rootSession walks up the parents, so the result does not depend on
	// start order; seen stops a cycle.
	rootSession := func(a *agent) string {
		seen := map[*agent]bool{}
		for a.agentID != "" && a.parent != "" && !seen[a] {
			seen[a] = true
			p := byThread[a.parent]
			if p == nil {
				if a.session == a.agentID {
					return a.parent // an older file names itself as its session
				}
				return a.session
			}
			a = p
		}
		return a.session
	}
	for _, a := range agents {
		if a.agentID == "" || a.parent == "" {
			continue
		}
		session := rootSession(a)
		if session == a.session {
			continue
		}
		a.session = session
		if _, err := db.Exec(`UPDATE sessions SET session_id = ? WHERE copy_id = ?`, session, a.copyID); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE copies SET parser_version = NULL WHERE copy_id = ?`, a.copyID); err != nil {
			return err
		}
	}
	return nil
}

// sessionParser runs pass 2.
type sessionParser struct {
	db      *sql.DB
	archive archive
	// unknown counts copies by unknown line type, for one warning each.
	unknown map[string]int
	failed  []string

	sessions, copies, usageRows int
}

// sessionCopy is a current, parsed session copy.
type sessionCopy struct {
	id                              int64
	app, copyPath, session, agentID string
	startedAt                       string
	pending                         bool
}

// parseSessions parses every session with a pending copy. titlesChanged
// refreshes every Codex title from session_index.jsonl.
func (p *sessionParser) parseSessions(titlesChanged bool) error {
	rows, err := p.db.Query(`SELECT c.copy_id, c.app, c.copy_path, COALESCE(s.session_id, ''), s.agent_id,
			COALESCE(s.started_at, ''), c.parser_version IS NULL OR c.parser_version != ?
		FROM copies c JOIN sessions s USING (copy_id)
		WHERE c.superseded_at IS NULL AND c.parse_status = ? ORDER BY c.copy_id`, parserVersion, parseStatusParsed)
	if err != nil {
		return err
	}
	groups := map[string][]sessionCopy{}
	var keys []string
	for rows.Next() {
		var c sessionCopy
		if err := rows.Scan(&c.id, &c.app, &c.copyPath, &c.session, &c.agentID, &c.startedAt, &c.pending); err != nil {
			rows.Close()
			return err
		}
		key := c.app + "/" + c.session
		if c.session == "" {
			key = fmt.Sprintf("%s/copy-%d", c.app, c.id) // no session id: a session of one
		}
		if groups[key] == nil {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	metas, err := p.metaCopies()
	if err != nil {
		return err
	}
	titles, err := p.codexTitles()
	if err != nil {
		return err
	}
	for _, key := range keys {
		g := groups[key]
		if !titlesChanged && !slices.ContainsFunc(g, func(c sessionCopy) bool { return c.pending }) {
			continue
		}
		if !slices.ContainsFunc(g, func(c sessionCopy) bool { return c.pending }) {
			if err := p.storeTitles(g, titles); err != nil {
				return err
			}
			continue
		}
		if err := p.parseSession(g, metas, titles); err != nil {
			return err
		}
	}
	return nil
}

// metaCopies maps a Claude Code subagent copy's path to its .meta.json
// copy's path.
func (p *sessionParser) metaCopies() (map[string]string, error) {
	rows, err := p.db.Query(`SELECT copy_path FROM copies WHERE kind = ? AND superseded_at IS NULL`, kindSubagentMeta)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out[strings.TrimSuffix(m, ".meta.json")+".jsonl.gz"] = m
	}
	return out, rows.Err()
}

// codexTitles maps a Codex thread id to its last thread_name in
// session_index.jsonl.
func (p *sessionParser) codexTitles() (map[string]string, error) {
	var copyPath string
	err := p.db.QueryRow(`SELECT copy_path FROM copies WHERE app = ? AND kind = ? AND superseded_at IS NULL
		ORDER BY copy_id DESC LIMIT 1`, appCodex, kindSessionTitles).Scan(&copyPath)
	if err == sql.ErrNoRows {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	r, err := openCopy(p.archive.abs(copyPath))
	if err != nil {
		p.failed = append(p.failed, fmt.Sprintf("read %s: %v", copyPath, err))
		return out, nil
	}
	defer r.Close()
	err = sessionfile.Lines(r, func(_ int, line []byte) {
		var e struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(line, &e) == nil && e.ID != "" && e.ThreadName != "" {
			out[e.ID] = e.ThreadName
		}
	})
	if err != nil {
		p.failed = append(p.failed, fmt.Sprintf("read %s: %v", copyPath, err))
	}
	return out, nil
}

// threadID is a Codex copy's own thread id.
func (c sessionCopy) threadID() string {
	if c.agentID != "" {
		return c.agentID
	}
	return c.session
}

func (p *sessionParser) storeTitles(g []sessionCopy, titles map[string]string) error {
	for _, c := range g {
		if t, ok := titles[c.threadID()]; ok && c.app == appCodex {
			if _, err := p.db.Exec(`UPDATE sessions SET title = ? WHERE copy_id = ?`, t, c.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseSession parses one session's copies and stores them in one
// transaction.
func (p *sessionParser) parseSession(g []sessionCopy, metas, titles map[string]string) error {
	copies := make([]sessionfile.Copy, len(g))
	for i, c := range g {
		name := p.archive.abs(c.copyPath)
		copies[i] = sessionfile.Copy{
			ID: c.id, Name: c.copyPath, AgentID: c.agentID, StartedAt: c.startedAt,
			Open: func() (io.ReadCloser, error) { return openCopy(name) },
		}
		if m, ok := metas[c.copyPath]; ok {
			copies[i].Meta, _ = os.ReadFile(p.archive.abs(m)) // a missing meta leaves its facts empty
		}
	}
	parse := claudecode.Parse
	if g[0].app == appCodex {
		parse = codex.Parse
		parents, err := forkParents(p.db, p.archive, g[0].session)
		if err != nil {
			return err
		}
		copies = append(copies, parents...)
	}
	parsed, err := parse(copies)
	if err != nil {
		return p.storeFailed(g, err)
	}
	if g[0].app == appCodex {
		for i := range parsed {
			c := g[slices.IndexFunc(g, func(c sessionCopy) bool { return c.id == parsed[i].CopyID })]
			if t, ok := titles[c.threadID()]; ok {
				parsed[i].Session.Title = t
			}
		}
	}
	for _, pc := range parsed {
		for _, t := range sessionfile.UnknownTypes(pc.SkippedLines) {
			p.unknown[t]++
		}
	}
	p.sessions++
	p.copies += len(parsed)
	for _, pc := range parsed {
		p.usageRows += len(pc.Usage)
	}
	return p.store(parsed)
}

// forkParents returns the root copies of the Codex session that a root of
// session was forked from, as fork parents: read only to find the lines
// the fork repeats.
func forkParents(db *sql.DB, a archive, session string) ([]sessionfile.Copy, error) {
	rows, err := db.Query(`SELECT c.copy_id, c.copy_path, COALESCE(s.started_at, '')
		FROM copies c JOIN sessions s USING (copy_id)
		WHERE s.app = ? AND s.agent_id = '' AND c.superseded_at IS NULL AND c.parse_status = ?
			AND s.session_id IN (SELECT forked_from_session_id FROM sessions
				WHERE app = ? AND session_id = ? AND agent_id = '' AND forked_from_session_id != '')
		ORDER BY c.copy_id`, appCodex, parseStatusParsed, appCodex, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionfile.Copy
	for rows.Next() {
		c := sessionfile.Copy{ForkParent: true}
		if err := rows.Scan(&c.ID, &c.Name, &c.StartedAt); err != nil {
			return nil, err
		}
		name := a.abs(c.Name)
		c.Open = func() (io.ReadCloser, error) { return openCopy(name) }
		out = append(out, c)
	}
	return out, rows.Err()
}

// storeFailed marks a session's pending copies failed when one of its
// copies cannot be read.
func (p *sessionParser) storeFailed(g []sessionCopy, err error) error {
	p.failed = append(p.failed, err.Error())
	for _, c := range g {
		if !c.pending {
			continue
		}
		if _, err := p.db.Exec(`UPDATE copies SET parse_status = ?, parse_error = ?, parser_version = ? WHERE copy_id = ?`,
			parseStatusFailed, err.Error(), parserVersion, c.id); err != nil {
			return err
		}
	}
	return nil
}

func (p *sessionParser) store(parsed []sessionfile.ParsedCopy) error {
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, pc := range parsed {
		s := pc.Session
		skipped, err := json.Marshal(pc.SkippedLines)
		if err != nil {
			return err
		}
		var cost any
		if s.AppCostUSD != nil {
			cost = *s.AppCostUSD
		}
		if _, err := tx.Exec(`UPDATE sessions SET agent_kind = ?, parent_agent_id = ?, parent_tool_call_id = ?,
				forked_from_session_id = ?, entrypoint = ?, git_branch = ?, git_remote_url = ?, title = ?, ended_at = ?,
				user_prompts = ?, tool_calls = ?, tool_errors = ?, compactions = ?, app_cost_usd = ?, usage_source = ?,
				skipped_lines = ?
			WHERE copy_id = ?`,
			s.AgentKind, s.ParentAgentID, s.ParentToolCallID, s.ForkedFromSessionID, s.Entrypoint, s.GitBranch,
			s.GitRemoteURL, s.Title, nullIfEmpty(s.EndedAt), s.UserPrompts, s.ToolCalls, s.ToolErrors, s.Compactions,
			cost, s.UsageSource, string(skipped), pc.CopyID); err != nil {
			return err
		}
		if err := deleteUsage(tx, pc.CopyID); err != nil {
			return err
		}
		for _, u := range pc.AppUsage {
			x := u.Usage
			if _, err := tx.Exec(`INSERT INTO app_usage (copy_id, model, input_tokens, cache_read_input_tokens,
					cache_creation_input_tokens, output_tokens, reasoning_tokens, web_search_requests, cost_usd)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				pc.CopyID, u.Model, x.InputTokens, x.CacheReadInputTokens, x.CacheCreationInputTokens, x.OutputTokens,
				x.ReasoningTokens, x.Requests["web_search"], u.CostUSD); err != nil {
				return err
			}
		}
		for _, u := range pc.Usage {
			x := u.Usage
			if _, err := tx.Exec(`INSERT INTO usage (copy_id, line, model_call_id, model, input_tokens,
					cache_read_input_tokens, cache_creation_input_tokens, cache_creation_1h_input_tokens, output_tokens,
					reasoning_tokens, web_search_requests, speed, called_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				pc.CopyID, u.Line, u.ModelCallID, u.Model, x.InputTokens, x.CacheReadInputTokens,
				x.CacheCreationInputTokens, x.CacheCreation1hInputTokens, x.OutputTokens, x.ReasoningTokens,
				x.Requests["web_search"], u.Speed, nullIfEmpty(u.CalledAt)); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE copies SET parser_version = ? WHERE copy_id = ?`, parserVersion, pc.CopyID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// warnings returns one warning per unknown line type and one per failure.
func (p *sessionParser) warnings() []string {
	var out []string
	types := make([]string, 0, len(p.unknown))
	for t := range p.unknown {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		out = append(out, fmt.Sprintf("unknown line type %q in %d session files; counted in skipped_lines", t, p.unknown[t]))
	}
	for _, f := range p.failed {
		out = append(out, "parse: "+f)
	}
	return out
}
