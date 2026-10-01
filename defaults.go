package dashi

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/catalog"
)

// NewID returns a new random UUIDv7 string, such as
// "0199a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b". The first 48 bits are the Unix time
// in milliseconds, so IDs sort by creation time to the millisecond (IDs from
// the same millisecond are in random order), and the string fits a
// Postgres uuid column. The runner uses it for an empty turn ID or CommandID.
// To make retries safe across restarts, call NewID yourself, save the ID
// before the call, and reuse it on retry.
func NewID() string {
	var b [16]byte
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(time.Now().UnixMilli()))
	copy(b[:6], ms[2:])
	_, _ = rand.Read(b[6:])
	b[6] = b[6]&0x0f | 0x70 // version 7
	b[8] = b[8]&0x3f | 0x80 // RFC 9562 variant
	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:], b[10:])
	return string(s[:])
}

// normalizeCommand returns c with an empty CommandID replaced by a new ID,
// and a JSON null output schema treated as absent, so a command decoded from
// JSON runs exactly like the original. The ID is part of the command digest,
// so it is filled before the digest is computed.
func normalizeCommand(c Command) Command {
	id := c.commandID()
	if id == "" {
		id = NewID()
	}
	switch v := c.(type) {
	case Start:
		v.CommandID = id
		v.OutputSchema = absentNull(v.OutputSchema)
		v.Agent.OutputSchema = absentNull(v.Agent.OutputSchema)
		return v
	case Deliver:
		v.CommandID = id
		return v
	case Continue:
		v.CommandID = id
		return v
	case Settle:
		v.CommandID = id
		return v
	case Cancel:
		v.CommandID = id
		return v
	}
	return c
}

// absentNull returns nil for a JSON null, which encoding/json leaves in a
// json.RawMessage field rather than clearing it.
func absentNull(raw json.RawMessage) json.RawMessage {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	return raw
}

// revisionPrefix marks a revision the runner derived from the agent.
const revisionPrefix = "auto:"

// agentRevision derives a revision from a resolved agent's behavior. ID,
// Revision, Limits.Deadline, and Settings.PromptCache.Key are left out: the
// first two name the agent rather than describe it, a deadline is a per-turn
// time that would make every revision unique, and a cache key usually names
// a conversation.
func agentRevision(a Agent) string {
	a.ID, a.Revision = "", ""
	a.Limits.Deadline = time.Time{}
	a.Settings.PromptCache.Key = ""
	sum := strings.TrimPrefix(digest(a), digestPrefix)
	return revisionPrefix + sum[:16]
}

// agentFor returns the resolved agent a command runs with. A Start resolves
// its own agent: Provider and API come from the client, an empty Model is the
// catalog default, and an empty Revision is derived from the agent. Every
// other command runs with the agent recorded in the caller's snapshot; run
// checks it against a Loader's record.
func (r Runner) agentFor(command Command, state *replayState) (Agent, error) {
	start, ok := command.(Start)
	if !ok {
		agent := state.Agent
		if r.Client != nil && agent.Provider != "" &&
			(r.Client.Provider() != agent.Provider ||
				r.Client.API() != agent.API) {
			return Agent{}, errors.New("dashi: client does not match the turn's recorded provider and API")
		}
		return agent, nil
	}
	agent := start.Agent
	agent.Provider = cmp.Or(agent.Provider, r.Client.Provider())
	agent.API = cmp.Or(agent.API, r.Client.API())
	if agent.Model == "" {
		model, ok := catalog.Default(agent.Provider)
		if ok {
			_, ok = r.Client.ModelInfo(model)
		}
		if !ok {
			return Agent{}, fmt.Errorf(
				"dashi: agent has no model, and the catalog has no default for provider %q at this client's endpoint; set Start.Agent.Model",
				agent.Provider,
			)
		}
		agent.Model = model
	}
	if err := agent.Validate(); err != nil {
		return Agent{}, err
	}
	agent = agent.resolved()
	if agent.Revision == "" {
		agent.Revision = agentRevision(agent)
	}
	if r.Client.Provider() != agent.Provider || r.Client.API() != agent.API {
		return Agent{}, errors.New("dashi: client does not match agent provider and API")
	}
	return agent, nil
}

// agentChanged returns nil when agent matches the recorded one, and otherwise
// an ErrAgentChanged that names the differing fields and the fix.
func agentChanged(agent, recorded Agent) error {
	if sameAgent(agent, recorded) {
		return nil
	}
	var a, b map[string]json.RawMessage
	ab, _ := json.Marshal(agent)
	bb, _ := json.Marshal(recorded)
	_ = json.Unmarshal(ab, &a)
	_ = json.Unmarshal(bb, &b)
	var fields []string
	for _, k := range slices.Sorted(maps.Keys(a)) {
		if !sameJSONValue(a[k], b[k]) {
			fields = append(fields, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(b)) {
		if _, ok := a[k]; !ok {
			fields = append(fields, k)
		}
	}
	return fmt.Errorf(
		"%w: %s differ from the recorded agent; retry the Start with the agent it recorded (TurnView.Agent), or start a new turn",
		ErrAgentChanged,
		strings.Join(fields, ", "),
	)
}
