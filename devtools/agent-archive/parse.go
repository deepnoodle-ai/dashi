package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
)

// parserVersion changes whenever the parser's output changes, so copies
// parsed by an older parser are parsed again. Version 2 adds phase 2:
// design/agent-archive-parse.md. Version 3 reads Codex code mode tool
// errors. Version 4 adds app_usage and finds what older Codex forks repeat.
// Version 5 fills usage.called_at.
const parserVersion = 5

// Parse statuses.
const (
	parseStatusParsed  = "parsed"
	parseStatusFailed  = "failed"
	parseStatusSkipped = "skipped" // not a session file
)

// Keys in line_types for lines with no type of their own.
const (
	lineTypeBadLine = "(bad line)" // not a JSON object
	lineTypeNoType  = "(no type)"  // a JSON object with no "type"
)

// factLines is how many lines at the start of a session file are read for
// the facts in the sessions table. Every line is still read for line_types.
const factLines = 50

// sessionFacts is one row of the sessions table.
type sessionFacts struct {
	SessionID  string
	AgentID    string
	WorkingDir string
	StartedAt  string
	AppVersion string
	// ParentThreadID is the thread that started a Codex subagent. Pass 1
	// stores it in parent_agent_id so linkCodexSessions can find the root.
	ParentThreadID string
	// ForkedFromSessionID is the session a Codex root was forked from.
	// Pass 2 reads that session's root to find the lines the fork repeats.
	ForkedFromSessionID string
}

// parseResult is what parsing one copy found.
type parseResult struct {
	Status    string
	Error     string
	LineTypes map[string]int
	Session   *sessionFacts
}

// parseCopy parses one copy. A bad line is counted and skipped; an
// unreadable file is failed with its error. It never returns an error.
func parseCopy(name, app, kind string) parseResult {
	if kind != kindSession && kind != kindSubagent {
		return parseResult{Status: parseStatusSkipped}
	}
	r, err := openCopy(name)
	if err != nil {
		return parseResult{Status: parseStatusFailed, Error: err.Error()}
	}
	defer r.Close()
	res := parseResult{Status: parseStatusParsed, LineTypes: map[string]int{}}
	facts := &sessionFacts{}
	if app == appClaudeCode && kind == kindSubagent {
		// The fallback when no line names the agent.
		facts.AgentID = strings.TrimSuffix(strings.TrimPrefix(path.Base(strings.TrimSuffix(name, ".gz")), "agent-"), ".jsonl")
	}
	br := bufio.NewReaderSize(r, 1<<20)
	for n := 0; ; n++ {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			countLine(res.LineTypes, line, n < factLines, app, facts)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return parseResult{Status: parseStatusFailed, Error: err.Error()}
		}
	}
	if app == appClaudeCode && kind == kindSession {
		facts.AgentID = "" // the root: any agentId belongs to a sidechain line
	}
	res.Session = facts
	return res
}

// claudeCodeLine holds the fields of a Claude Code line that phase 1 reads.
type claudeCodeLine struct {
	Type      *string `json:"type"`
	SessionID string  `json:"sessionId"`
	AgentID   string  `json:"agentId"`
	Cwd       string  `json:"cwd"`
	Version   string  `json:"version"`
	Timestamp string  `json:"timestamp"`
}

// codexLine holds the fields of a Codex line that phase 1 reads; payload is
// only decoded for the session_meta line.
type codexLine struct {
	Type      *string          `json:"type"`
	Timestamp string           `json:"timestamp"`
	Payload   *json.RawMessage `json:"payload"`
}

type codexSessionMeta struct {
	ID             string          `json:"id"`
	SessionID      string          `json:"session_id"`
	ParentThreadID string          `json:"parent_thread_id"`
	ForkedFromID   string          `json:"forked_from_id"`
	Cwd            string          `json:"cwd"`
	CLIVersion     string          `json:"cli_version"`
	Timestamp      string          `json:"timestamp"`
	Source         json.RawMessage `json:"source"`
}

type typeOnly struct {
	Type *string `json:"type"`
}

func countLine(counts map[string]int, line []byte, readFacts bool, app string, facts *sessionFacts) {
	line = trimSpace(line)
	if len(line) == 0 {
		return
	}
	var typ *string
	var err error
	switch {
	case !readFacts:
		var l typeOnly
		err = json.Unmarshal(line, &l)
		typ = l.Type
	case app == appClaudeCode:
		var l claudeCodeLine
		if err = json.Unmarshal(line, &l); err == nil {
			typ = l.Type
			fillClaudeCodeFacts(facts, l)
		}
	default:
		var l codexLine
		if err = json.Unmarshal(line, &l); err == nil {
			typ = l.Type
			if typ != nil && *typ == "session_meta" && l.Payload != nil && facts.SessionID == "" {
				fillCodexFacts(facts, *l.Payload)
			}
		}
	}
	switch {
	case err != nil:
		counts[lineTypeBadLine]++
	case typ == nil || *typ == "":
		counts[lineTypeNoType]++
	default:
		counts[*typ]++
	}
}

// fillClaudeCodeFacts takes each fact from the first line that has it.
// Subagent lines carry the parent's sessionId and their own agentId.
func fillClaudeCodeFacts(f *sessionFacts, l claudeCodeLine) {
	setIfEmpty(&f.SessionID, l.SessionID)
	if l.AgentID != "" {
		f.AgentID = l.AgentID
	}
	setIfEmpty(&f.WorkingDir, l.Cwd)
	setIfEmpty(&f.StartedAt, l.Timestamp)
	setIfEmpty(&f.AppVersion, l.Version)
}

// fillCodexFacts reads Codex's session_meta payload. A subagent's file
// (source.subagent) has its own id; its session is the root's session_id,
// or failing that its parent thread.
func fillCodexFacts(f *sessionFacts, payload json.RawMessage) {
	var m codexSessionMeta
	if json.Unmarshal(payload, &m) != nil {
		return
	}
	var source struct {
		Subagent json.RawMessage `json:"subagent"`
	}
	isSubagent := json.Unmarshal(m.Source, &source) == nil && len(source.Subagent) > 0
	f.SessionID = m.ID
	if isSubagent {
		f.AgentID = m.ID
		f.SessionID = firstNonEmpty(m.SessionID, m.ParentThreadID, m.ID)
		var spawn struct {
			ThreadSpawn struct {
				ParentThreadID string `json:"parent_thread_id"`
			} `json:"thread_spawn"`
		}
		_ = json.Unmarshal(source.Subagent, &spawn)
		f.ParentThreadID = firstNonEmpty(spawn.ThreadSpawn.ParentThreadID, m.ParentThreadID)
	} else {
		f.ForkedFromSessionID = m.ForkedFromID
	}
	f.WorkingDir = m.Cwd
	f.AppVersion = m.CLIVersion
	f.StartedAt = m.Timestamp
}

func setIfEmpty(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}
