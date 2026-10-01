package dashi

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Agent is reusable, serializable behavior: what to ask which model, with
// which settings, tools, and limits. It goes on Start; clients, credentials,
// tool implementations, and storage are supplied separately on a Runner. The
// zero value works: the Runner fills empty fields before pinning the agent at
// Start, and the turn records the result, which TurnView.Agent shows. Later
// commands on the turn run with the recorded agent.
type Agent struct {
	// ID names the agent for the host, for example in reports. It may be
	// empty; nothing in Dashi reads it.
	ID string `json:"id"`
	// Version pins behavior so a continued turn cannot silently change it.
	// When empty, it is derived from the agent's behavior fields as
	// DerivedVersionPrefix and 16 hex digits, so it changes exactly when
	// they do, including the tool selection. The derivation leaves out
	// Limits.Deadline and cannot see tool implementations, which are not
	// part of the agent; set Version yourself to version those.
	Version      string `json:"version"`
	Instructions string `json:"instructions,omitempty"`
	// Provider and API default to the Runner client's.
	Provider string `json:"provider"`
	API      string `json:"api"`
	// Model defaults to the provider's catalog.Default when the client knows
	// that model; otherwise it is required.
	Model    string       `json:"model"`
	Settings llm.Settings `json:"settings"`
	// NoInheritedSettings makes Settings a complete set when a host, such
	// as a session.Session, carries an agent forward. Nil and zero fields do
	// not inherit lower-priority Agent values. Higher-priority partial
	// settings may still override this set. Other Agent fields retain their
	// ordinary inheritance rules.
	NoInheritedSettings bool `json:"no_inherited_settings,omitempty"`
	// OutputSchema is the default final-result schema for turns using this
	// agent. Start.OutputSchema may override it for one turn. A JSON null
	// means none.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	// Tools names the tools this agent may call, from those the Runner's
	// Tools offer. Empty means every tool the runner offers. Order and
	// duplicates do not matter: the runner records the names sorted. The
	// runner filters by name before each model call, before BeforeModel
	// hooks hide tools; a name the runner does not offer records
	// WarningToolNotOffered once per turn.
	Tools []string `json:"tools,omitempty"`
	// NoTools offers no application tools, whatever Tools says.
	// dashi_submit_output is still offered when the turn has an output
	// schema.
	NoTools bool   `json:"no_tools,omitempty"`
	Limits  Limits `json:"limits"`
	// ActiveCompaction protects a long single Turn. Nil disables it.
	ActiveCompaction *ActiveCompactionPolicy `json:"active_compaction,omitempty"`
}

// Limits bounds model calls, interruptions, pauses, tool concurrency, and time
// for turns using an agent. The zero value gives the default limits, so
// set only the fields you want to change. For counts where "none" is a
// meaningful setting, a negative value means none.
//
// Dashi resolves defaults when it accepts an agent and records the
// resolved values, so a recorded turn keeps its limits even if the library
// defaults change later.
type Limits struct {
	// ModelCalls counts finished calls; interrupted calls have their own cap.
	// Zero means 8. It must not be negative.
	ModelCalls int `json:"model_calls"`
	// Resumes bounds how many times a stopped or crashed turn may be
	// resumed. Zero means 10; a negative value allows no resumes.
	Resumes int `json:"resumes"`
	// ProviderPauses bounds provider-requested continuations. Zero means 3;
	// a negative value allows none.
	ProviderPauses int `json:"provider_pauses"`
	// ToolConcurrency bounds how many tool calls run at once. Zero means 1.
	// It must not be negative.
	ToolConcurrency int `json:"tool_concurrency"`
	// ToolResultBytes limits the text of one tool result in the messages the
	// model sees. Zero means 64 KiB. A negative value turns the limit off. A
	// value below 1 KiB is raised to 1 KiB, with a warning at Start. Dashi
	// keeps the full result in the record; see ToolFact.Shortened.
	ToolResultBytes int `json:"tool_result_bytes"`
	// NoReservedFinalReply turns off the reserved final-reply call. By
	// default, the last allowed model call, after earlier calls, asks for a
	// reply without advertising application tools.
	NoReservedFinalReply bool `json:"no_reserved_final_reply,omitempty"`
	// Deadline stops the turn at a wall-clock time. Zero means no deadline.
	Deadline time.Time `json:"deadline,omitzero"`
}

// Default turn limits, applied when a Limits field is zero.
const (
	defaultModelCalls      = 8
	defaultResumes         = 10
	defaultProviderPauses  = 3
	defaultToolConcurrency = 1
)

// Validate checks limits and output settings. Every identity field may be
// empty, since the Runner fills them; a zero Agent is valid.
func (a Agent) Validate() error {
	if err := a.ActiveCompaction.validate(); err != nil {
		return err
	}
	if a.Limits.ModelCalls < 0 || a.Limits.ToolConcurrency < 0 {
		return errors.New(
			"dashi: model attempts and tool concurrency cannot be negative",
		)
	}
	if schema := absentNull(a.OutputSchema); len(schema) > 0 {
		if _, err := compileOutputSchema(schema); err != nil {
			return err
		}
		if err := validateOutputToolChoice(a.Settings); err != nil {
			return err
		}
	}
	return nil
}

// resolved returns a with every zero limit replaced by its default, every
// negative "none" count normalized to -1, Tools sorted without duplicates,
// and a JSON null output schema dropped. Dashi records the result, so the
// same agent always compares equal to its record, whatever order a host
// built its tool list in. Runtime checks read -1 as zero allowed, through
// max(count, 0).
func (a Agent) resolved() Agent {
	a.OutputSchema = absentNull(a.OutputSchema)
	a.ActiveCompaction = a.ActiveCompaction.resolved()
	if len(a.Tools) > 0 {
		a.Tools = slices.Compact(slices.Sorted(slices.Values(a.Tools)))
	}
	l := &a.Limits
	l.ModelCalls = cmp.Or(l.ModelCalls, defaultModelCalls)
	l.ToolConcurrency = cmp.Or(l.ToolConcurrency, defaultToolConcurrency)
	l.Resumes = resolveCount(l.Resumes, defaultResumes)
	l.ProviderPauses = resolveCount(l.ProviderPauses, defaultProviderPauses)
	l.ToolResultBytes = resolveToolResultBytes(l.ToolResultBytes)
	return a
}

// resolveCount applies the default to a zero count and normalizes any
// negative count to -1, which means none.
func resolveCount(n, def int) int {
	switch {
	case n < 0:
		return -1
	case n == 0:
		return def
	}
	return n
}

// DerivedVersionPrefix begins every Agent.Version the runner derived
// from the agent. A host that carries a recorded agent into a new turn
// clears such a version, so the runner derives it again from the agent
// the new turn runs.
const DerivedVersionPrefix = "auto:"

// agentVersion derives a version from a resolved agent's behavior. ID,
// Version, Limits.Deadline, and Settings.PromptCache.Key are left out: the
// first two name the agent rather than describe it, a deadline is a per-turn
// time that would make every version unique, and a cache key usually names
// a conversation.
func agentVersion(a Agent) string {
	a.ID, a.Version = "", ""
	a.Limits.Deadline = time.Time{}
	a.Settings.PromptCache.Key = ""
	sum := strings.TrimPrefix(canonjson.Hash(a), canonjson.HashPrefix)
	return DerivedVersionPrefix + sum[:16]
}

// agentFor returns the resolved agent a command runs with. A Start resolves
// its own agent: an empty Provider and API come from the client, an empty
// Model is the catalog default, and an empty Version is derived from the
// agent. Without a client, the agent must name all three. Every
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
	if r.Client == nil &&
		(agent.Provider == "" || agent.API == "" || agent.Model == "") {
		return Agent{}, errors.New("dashi: client is required to fill the agent's provider, API, or model")
	}
	if r.Client != nil {
		agent.Provider = cmp.Or(agent.Provider, r.Client.Provider())
		agent.API = cmp.Or(agent.API, r.Client.API())
	}
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
	if agent.Version == "" {
		agent.Version = agentVersion(agent)
	}
	if r.Client != nil &&
		(r.Client.Provider() != agent.Provider || r.Client.API() != agent.API) {
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
		if !canonjson.SameValue(a[k], b[k]) {
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

// sameAgent compares agents after both were resolved, so a caller's zero limit
// matches the default recorded for it.
func sameAgent(a, b Agent) bool {
	return canonjson.SameValue(a, b)
}
