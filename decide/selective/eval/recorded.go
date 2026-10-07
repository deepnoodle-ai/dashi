package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/internal/compactionrules"
	"github.com/deepnoodle-ai/dashi/llm"
	dashisession "github.com/deepnoodle-ai/dashi/session"
)

// Recorded sessions are dashi CLI session folders, as in ~/.dashi/sessions.
// Each turn end whose history is between minCaseTokens and maxCaseTokens is
// one case. The window of a case is its size over caseFill, so the history
// fills the window as much as a generated session fills its 16,000 tokens.
const (
	minCaseTokens = 20_000
	maxCaseTokens = 120_000
	caseFill      = 0.8

	// chunkBytes bounds the text of one needle-mining request.
	chunkBytes = 100_000
	// needlesPerChunk is how many needles one mining request asks for, and
	// needlesPerCase how many a case keeps, spread over its history.
	needlesPerChunk = 8
	needlesPerCase  = 6
)

// recordedSessions loads the cases of every session folder in dirs. A
// folder that holds session folders, such as ~/.dashi/sessions, counts as
// all of them.
func recordedSessions(ctx context.Context, dirs []string) ([]session, error) {
	var folders []string
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "turns")); err == nil {
			folders = append(folders, dir)
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "turns")); e.IsDir() && err == nil {
				folders = append(folders, filepath.Join(dir, e.Name()))
			}
		}
	}
	var out []session
	for _, f := range folders {
		cases, err := sessionCases(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, cases...)
	}
	return out, nil
}

// sessionCases returns one case per turn end of the session in folder
// whose history fits the case size limits.
func sessionCases(ctx context.Context, folder string) ([]session, error) {
	id := filepath.Base(folder)
	r, err := filerecorder.OpenReader(filepath.Join(folder, "turns"))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	last, err := r.LastTurn(ctx, id)
	if err != nil {
		return nil, err
	}
	var turns []dashi.Turn
	var out []session
	for n := 1; n <= last; n++ {
		t, err := r.Load(ctx, dashisession.TurnID(id, n))
		if err != nil {
			continue
		}
		turns = append(turns, t)
		hist, warnings := dashi.BuildHistory(turns)
		if len(warnings) > 0 {
			continue // an unreadable turn, such as one that did not finish
		}
		// Every arm gets the history without reasoning, so the size, the
		// limits, and the window are measured on it.
		hist = withoutReasoning(hist)
		size := keptTokens(hist, &dashi.CompactionFact{SummaryAt: -1})
		if size < minCaseTokens || size > maxCaseTokens {
			continue
		}
		view, err := t.View()
		if err != nil {
			continue
		}
		out = append(out, session{
			model:    view.Agent.Model,
			provider: view.Agent.Provider,
			name:     fmt.Sprintf("%s/t%d", id, n),
			messages: hist,
			latest:   latestOwnWords(hist),
			window:   int(float64(size) / caseFill),
		})
	}
	return out, nil
}

// withoutReasoning drops reasoning from hist. A recorded session's
// reasoning can be replayed only to the model that wrote it, and the
// summaries and probes use another model. Every arm gets the same history.
func withoutReasoning(hist []llm.Message) []llm.Message {
	var out []llm.Message
	for _, m := range hist {
		kept := m
		kept.Content = slices.DeleteFunc(slices.Clone(m.Content), func(c llm.Content) bool {
			return c.Kind == llm.ContentReasoning
		})
		if len(kept.Content) > 0 {
			out = append(out, kept)
		}
	}
	return out
}

// ownWords reports whether m is the user's own words, as selective pins
// them: a user message of text only, once reminders are left out.
func ownWords(m llm.Message) bool {
	if m.Role != llm.RoleUser {
		return false
	}
	text := false
	for _, c := range m.Content {
		switch {
		case c.Kind == llm.ContentReminder:
		case c.Block != nil && c.Block.Text != nil:
			text = true
		default:
			return false
		}
	}
	return text
}

func latestOwnWords(hist []llm.Message) string {
	for _, m := range slices.Backward(hist) {
		if ownWords(m) {
			return m.Text()
		}
	}
	return ""
}

// render writes messages as text for needle mining and checks: role,
// text, tool calls with their arguments, and tool result text.
func render(messages []llm.Message) string {
	var b strings.Builder
	for _, m := range messages {
		fmt.Fprintf(&b, "\n[%s]\n", m.Role)
		for _, c := range m.Content {
			switch {
			case c.Block != nil && c.Block.Text != nil:
				b.WriteString(c.Block.Text.Text)
			case c.ToolCall != nil:
				fmt.Fprintf(&b, "<call %s %s>", c.ToolCall.Name, c.ToolCall.Args)
			case c.ToolResult != nil:
				b.WriteString("<result>\n")
				for _, bl := range c.ToolResult.Output.Content {
					if bl.Text != nil {
						b.WriteString(bl.Text.Text)
					}
				}
				b.WriteString("\n</result>")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// minedNeedles maps a case name to its needles. The file is written once,
// before any arm runs, and read by later runs.
type minedNeedles map[string][]needle

func readNeedles(path string) (minedNeedles, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return minedNeedles{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m minedNeedles
	return m, json.Unmarshal(b, &m)
}

// writeNeedles saves m readable by its owner only: the needles quote
// private sessions.
func writeNeedles(path string, m minedNeedles) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600) // WriteFile keeps an older file's mode
}

const mineIntro = `Below is part of a coding-agent session. Its exact text will be summarized away soon.
Pick up to %d exact details from it that the developer or the agent could need later in this work:
an error message or code, a file path, a function or type name with a specific role, a number or
setting, or a decision with its reason. Prefer details that appear in tool results or in the
assistant's own findings. Avoid generic names that someone could guess without the session.

For each, write a question that asks for the detail without containing it, and the answer: a
string copied character for character from the text below, 4 to 80 characters, specific enough
that nobody could guess it.

Reply with only a JSON array: [{"kind": "error|path|name|value|decision", "question": "...", "answer": "..."}]

<session_part>
%s
</session_part>`

// mine picks the needles of s with the mining model: up to
// needlesPerChunk from each chunk of the history before stock's cut, then
// at most needlesPerCase, spread over the history. Each answer must be
// word for word in its chunk and nowhere in the tail, the user's own
// words, or a system message, so no arm keeps it by construction.
func (e *env) mine(ctx context.Context, s session) ([]needle, error) {
	region, forbidden := split(s)
	var chunks []string
	var cur strings.Builder
	for _, m := range region {
		text := render([]llm.Message{m})
		if len(text) > chunkBytes {
			text = text[:chunkBytes]
		}
		if cur.Len()+len(text) > chunkBytes && cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
		cur.WriteString(text)
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	var all []needle
	for i, chunk := range chunks {
		reply, err := e.ask(ctx, fmt.Sprintf("mine-%s-%d", strings.ReplaceAll(s.name, "/", "-"), i),
			fmt.Sprintf(mineIntro, needlesPerChunk, chunk))
		if err != nil {
			return nil, err
		}
		start, end := strings.Index(reply, "["), strings.LastIndex(reply, "]")
		if start < 0 || end < start {
			continue
		}
		var got []needle
		if err := json.Unmarshal([]byte(reply[start:end+1]), &got); err != nil {
			continue
		}
		taken := 0
		for _, n := range got {
			low := strings.ToLower(n.Answer)
			switch {
			case taken == needlesPerChunk, len(n.Answer) < 4, len(n.Answer) > 80,
				!strings.Contains(chunk, n.Answer),
				strings.Contains(strings.ToLower(n.Question), low),
				strings.Contains(forbidden, low),
				slices.ContainsFunc(all, func(o needle) bool { return overlap(o.Answer, n.Answer) }):
				continue
			}
			all = append(all, n)
			taken++
		}
	}
	if len(all) <= needlesPerCase {
		return all, nil
	}
	spread := make([]needle, needlesPerCase)
	for i := range spread {
		spread[i] = all[i*len(all)/needlesPerCase]
	}
	return spread, nil
}

// overlap reports whether one answer holds the other, ignoring case, so
// one detail counts once.
func overlap(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	return strings.Contains(a, b) || strings.Contains(b, a)
}

// split returns the messages of s that needles come from, before stock's
// cut and not the user's own words or instructions, and the lower-case
// text of the rest, which must hold no answer.
func split(s session) (region []llm.Message, forbidden string) {
	cut := compactionrules.Cut(s.messages, int(0.25*float64(s.window)))
	excluded := slices.Clone(s.messages[cut:])
	for _, m := range s.messages[:cut] {
		if m.Role == llm.RoleSystem || m.Role == llm.RoleDeveloper || ownWords(m) {
			excluded = append(excluded, m)
			continue
		}
		region = append(region, m)
	}
	return region, strings.ToLower(render(excluded))
}

// stillValid keeps the needles of s whose answers are still only in the
// part every arm compacts, in case the history changed after mining.
func stillValid(s session, needles []needle) []needle {
	region, forbidden := split(s)
	text := render(region)
	return slices.DeleteFunc(slices.Clone(needles), func(n needle) bool {
		return !strings.Contains(text, n.Answer) || strings.Contains(forbidden, strings.ToLower(n.Answer))
	})
}

// ask sends one prompt to the mining model and returns its reply.
func (e *env) ask(ctx context.Context, id, prompt string) (string, error) {
	runner := dashi.Runner{Client: e.client("anthropic", ""), Recorder: dashi.NewMemoryRecorder()}
	out, err := runner.Run(ctx, dashi.NewTurn(id), dashi.Start{
		CommandID: "run:" + id,
		Agent:     dashi.Agent{Model: e.cfg.mineModel, Settings: llm.Settings{MaxTokens: 1500}},
		Input:     []llm.Message{llm.UserMessage(llm.Text(prompt))},
	})
	if err != nil {
		return "", err
	}
	if out.Stop.Kind != dashi.StopCompleted {
		return "", fmt.Errorf("mining %s stopped %s: %s", id, out.Stop.Kind, out.Stop.Reason)
	}
	return out.Reply, nil
}

// toolNames returns every tool name called in sessions.
func toolNames(sessions []session) []string {
	seen := map[string]bool{}
	for _, s := range sessions {
		for _, m := range s.messages {
			for _, c := range m.Content {
				if c.ToolCall != nil {
					seen[c.ToolCall.Name] = true
				}
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// recordedWithNeedles loads the recorded cases and their needles. Needles
// missing from the needles file are mined (live only) and saved before
// any arm runs. A case with no needle is left out.
func (e *env) recordedWithNeedles(ctx context.Context) ([]session, error) {
	cases, err := recordedSessions(ctx, e.cfg.recorded)
	if err != nil {
		return nil, err
	}
	mined, err := readNeedles(e.cfg.needles)
	if err != nil {
		return nil, err
	}
	if e.checkProvider != nil {
		for _, s := range cases {
			if err := e.checkProvider(e.providerFor(s)); err != nil {
				return nil, fmt.Errorf("case %s: %w", s.name, err)
			}
		}
	}
	changed := false
	var out []session
	for i, s := range cases {
		n, ok := mined[s.name]
		if !ok {
			if !e.cfg.live || !e.cfg.mineNeedles {
				return nil, fmt.Errorf("recorded case %d has no saved needles; mining requires -live -mine-needles", i+1)
			}
			if n, err = e.mine(ctx, s); err != nil {
				return nil, fmt.Errorf("case %s: %w", s.name, err)
			}
			mined[s.name] = n
			changed = true
		}
		valid := stillValid(s, n)
		if dropped := len(n) - len(valid); dropped > 0 {
			fmt.Fprintf(os.Stderr, "%d of %d saved needles for a recorded case are no longer only in the compacted part; left out\n", dropped, len(n))
		}
		if len(valid) == 0 {
			continue
		}
		s.needles = valid
		out = append(out, s)
	}
	if changed {
		if err := writeNeedles(e.cfg.needles, mined); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no recorded case has needles")
	}
	return out, nil
}
