package dashi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Command carries a CommandID. A repeated ID with identical content
// acknowledges the first application without advancing again. An empty
// CommandID gets a generated one (see NewID), recorded on the command's
// steps. A generated ID cannot recognize a retry, since the retry cannot know
// it: to make a retry after a crash safe, choose the ID, save it before the
// call, and reuse it. Without that, a retry from an old turn snapshot is still
// refused by the recorder, so no model call or tool runs twice. A host that
// saves whole commands, such as an outbox run again after a crash, encodes
// them with MarshalCommand and finds them by CommandIDOf and CommandKindOf.
type Command interface{ commandID() string }

// Start begins a turn with caller-owned prior history and initial input.
type Start struct {
	CommandID string `json:"command_id,omitempty"`
	// Agent is the behavior this turn runs with. Its zero fields take
	// defaults (see Agent), and the turn records the resolved agent, so
	// Continue, Resolve, and Cancel need none.
	Agent  Agent       `json:"agent"`
	Origin StartOrigin `json:"origin,omitempty"`
	// Budget applies to this turn, independently of its agent.
	Budget *Budget `json:"budget,omitempty"`
	// OutputSchema overrides the agent's default for this turn. The
	// accepted schema is pinned in the start fact, so later runs and
	// recovery use the same schema.
	// A JSON null means none.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	// History is caller-owned prior conversation and is not stored in the turn.
	History []llm.Message `json:"history,omitempty"`
	Input   []llm.Message `json:"input,omitempty"`
	// QueuedInputCount is the number of leading messages in Input that a
	// host took from its own queue and put ahead of the caller's input, as
	// session.Session does with queued user input. Dashi records it so the
	// host can tell the caller's input apart later; it does not change the
	// turn. It must be between 0 and len(Input).
	QueuedInputCount int `json:"queued_input_count,omitempty"`
	// Items are context items, such as reminders, that the Start step
	// records with Input. The first model request sends them after Input,
	// and later requests keep them like items an AddMessages adds. They are
	// checked as AddMessages items are: only ContextOperator items may use
	// the system or developer role or an operator reminder.
	Items []ContextItem `json:"items,omitempty"`
	// Parent links this turn to the tool call of another turn that started
	// it. Dashi records it and shows it in TurnView.Parent; a second Start
	// with a different Parent is a command conflict. Dashi does not check
	// that the call exists.
	Parent *CallRef `json:"parent,omitempty"`
	// Compaction records what a compaction turn replaces. It is required
	// with origin StartCompaction and allowed only with it.
	Compaction *CompactionFact `json:"compaction,omitempty"`
}

// AddMessages adds context to a turn between runs. To reach a turn while it
// runs, send a Steer through its Inbox instead. An item with an ID
// already recorded with the same content is skipped; with different content,
// Run returns an error.
type AddMessages struct {
	CommandID string        `json:"command_id,omitempty"`
	Items     []ContextItem `json:"items,omitempty"`
}

// Continue resumes a turn using the prior conversation supplied for this
// call. Pass no History when Start had no prior history.
type Continue struct {
	CommandID string        `json:"command_id,omitempty"`
	History   []llm.Message `json:"history,omitempty"`
	// Budget carries the complete raised cap after host authorization.
	Budget *Budget `json:"budget,omitempty"`
	// ModelCalls raises the turn's model call limit to this total. Zero
	// keeps the current limit. It must exceed the limit now in force, which
	// is the agent's Limits.ModelCalls until an earlier Continue raises
	// it. A turn that stopped at its limit continues only when this raise
	// leaves room for another call. The raise applies to this turn from then
	// on, and the recorded agent does not change.
	ModelCalls int `json:"model_calls,omitempty"`
	// OnHistoryMismatch controls what happens when History differs from the
	// hash recorded at Start. The default warns and proceeds.
	//
	// It is encoded with the command but left out of its hash, because it
	// says how to handle a mismatch, not what the command asks for. A
	// rejected Continue commits nothing, and a retry of an applied one
	// returns the recorded result, so changing it on a retry is not a
	// conflicting command.
	OnHistoryMismatch HistoryMismatch `json:"on_history_mismatch,omitempty"`
}

// Resolve records a result the host supplies for a pending item: a final
// tool outcome, a decision on a tool or provider tool approval, or a model
// result or verified failure. Exactly one fact is set.
type Resolve struct {
	CommandID            string                            `json:"command_id,omitempty"`
	CallID               string                            `json:"call_id,omitempty"`
	Outcome              *tool.Outcome                     `json:"outcome,omitempty"`
	Approval             *ApprovalDecision                 `json:"approval,omitempty"`
	ProviderToolApproval *llm.ProviderToolApprovalResponse `json:"provider_tool_approval,omitempty"`
	// ModelResult finishes a pending model call without sending it again.
	ModelResult *llm.Result `json:"model_result,omitempty"`
	// ModelFailure supplies verified evidence that a model call did not run.
	ModelFailure *ModelFailure `json:"model_failure,omitempty"`
}

// WithRules records, in the approval decision r carries, the rules that
// informed it and what they found, such as a check that ran before a person
// decided. The record keeps both for later review; the model never sees
// them. They are not part of the command's identity: a retry with other
// rules, such as a check run again, is a duplicate, and the first recorded
// rules stay. It copies the decision, so a Resolve the caller kept does not
// change. A command with no approval decision, such as a provider tool
// approval, whose answer goes to the provider, is returned unchanged.
func (r Resolve) WithRules(version, finding string) Resolve {
	if r.Approval == nil {
		return r
	}
	d := *r.Approval
	d.RulesVersion, d.RulesFinding = version, finding
	r.Approval = &d
	return r
}

// Cancel ends a turn with a caller-supplied reason.
type Cancel struct {
	CommandID string `json:"command_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func (c Start) commandID() string {
	return c.CommandID
}

// Say returns a Start holding one user text message.
func Say(text string) Start {
	return Start{Input: []llm.Message{llm.UserMessage(llm.Text(text))}}
}

func (c AddMessages) commandID() string {
	return c.CommandID
}

func (c Continue) commandID() string {
	return c.CommandID
}

func (c Resolve) commandID() string {
	return c.CommandID
}

func (c Cancel) commandID() string {
	return c.CommandID
}

// CommandKind names a command type in MarshalCommand's encoding.
type CommandKind string

const (
	CommandStart       CommandKind = "start"
	CommandAddMessages CommandKind = "add_messages"
	CommandContinue    CommandKind = "continue"
	CommandResolve     CommandKind = "resolve"
	CommandCancel      CommandKind = "cancel"
	CommandSteer       CommandKind = "steer"
)

// CommandKindOf returns the kind of c, or "" when c is not one of Dashi's
// command values. A pointer to a command, including a nil one, gives "".
func CommandKindOf(c Command) CommandKind {
	switch c.(type) {
	case Start:
		return CommandStart
	case AddMessages:
		return CommandAddMessages
	case Continue:
		return CommandContinue
	case Resolve:
		return CommandResolve
	case Cancel:
		return CommandCancel
	case Steer:
		return CommandSteer
	}
	return ""
}

// CommandIDOf returns c's CommandID, or "" when c is not one of Dashi's
// command values. A pointer to a command, including a nil one, gives "".
func CommandIDOf(c Command) string {
	switch v := c.(type) {
	case Start:
		return v.CommandID
	case AddMessages:
		return v.CommandID
	case Continue:
		return v.CommandID
	case Resolve:
		return v.CommandID
	case Cancel:
		return v.CommandID
	case Steer:
		return v.Item.ID
	}
	return ""
}

// commandEnvelope is MarshalCommand's encoding.
type commandEnvelope struct {
	Kind    CommandKind     `json:"kind"`
	Command json.RawMessage `json:"command"`
}

// MarshalCommand encodes c with its kind, as
// {"kind":"start","command":{...}}, for a host that saves commands before it
// runs them. UnmarshalCommand returns an equal command with the same hash,
// so a saved command run again after a crash is a retry, not a conflict. Raw
// JSON values, such as an output schema or tool arguments, come back
// compacted, which keeps their meaning and the hash.
func MarshalCommand(c Command) ([]byte, error) {
	kind := CommandKindOf(c)
	if kind == "" {
		return nil, fmt.Errorf("dashi: cannot encode command of type %T", c)
	}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return json.Marshal(commandEnvelope{Kind: kind, Command: body})
}

// UnmarshalCommand decodes MarshalCommand's encoding. It returns the command
// as a value, such as Start rather than *Start, ready to pass to Run.
func UnmarshalCommand(data []byte) (Command, error) {
	var env commandEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("dashi: decode command: %w", err)
	}
	if len(env.Command) == 0 || string(env.Command) == "null" {
		return nil, errors.New("dashi: decode command: no command")
	}
	switch env.Kind {
	case CommandStart:
		return decodeCommand[Start](env.Command)
	case CommandAddMessages:
		return decodeCommand[AddMessages](env.Command)
	case CommandContinue:
		return decodeCommand[Continue](env.Command)
	case CommandResolve:
		return decodeCommand[Resolve](env.Command)
	case CommandCancel:
		return decodeCommand[Cancel](env.Command)
	case CommandSteer:
		return decodeCommand[Steer](env.Command)
	}
	return nil, fmt.Errorf("dashi: decode command: unknown kind %q", env.Kind)
}

func decodeCommand[T Command](raw json.RawMessage) (Command, error) {
	var c T
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("dashi: decode command: %w", err)
	}
	return c, nil
}

// normalizeCommand returns c with an empty CommandID replaced by a new ID,
// and a JSON null output schema treated as absent, so a command decoded from
// JSON runs exactly like the original. The ID is part of the command hash,
// so it is filled before the hash is computed.
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
	case AddMessages:
		v.CommandID = id
		return v
	case Continue:
		v.CommandID = id
		return v
	case Resolve:
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

// commandHash is the hash of a command, reporting values that cannot be
// encoded. Continue.OnHistoryMismatch and the rules of a Resolve's approval
// decision are left out (see their docs).
func commandHash(c Command) (string, error) {
	switch v := c.(type) {
	case Continue:
		v.OnHistoryMismatch = ""
		c = v
	case Resolve:
		// The rules behind an approval are audit data, not what the
		// command asks for. A retry that ran its check again, with a new
		// request ID, is a duplicate; the first recorded values stay.
		if v.Approval != nil && (v.Approval.RulesVersion != "" || v.Approval.RulesFinding != "") {
			c = v.WithRules("", "")
		}
	}
	if _, err := json.Marshal(c); err != nil {
		return "", err
	}
	return canonjson.Hash(c), nil
}
