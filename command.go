package dashi

import (
	"encoding/json"
	"errors"
	"fmt"
)

// CommandKind names a command type in MarshalCommand's encoding.
type CommandKind string

const (
	CommandStart    CommandKind = "start"
	CommandDeliver  CommandKind = "deliver"
	CommandContinue CommandKind = "continue"
	CommandSettle   CommandKind = "settle"
	CommandCancel   CommandKind = "cancel"
)

// CommandKindOf returns the kind of c, or "" when c is not one of Dashi's
// command values.
func CommandKindOf(c Command) CommandKind {
	switch c.(type) {
	case Start:
		return CommandStart
	case Deliver:
		return CommandDeliver
	case Continue:
		return CommandContinue
	case Settle:
		return CommandSettle
	case Cancel:
		return CommandCancel
	}
	return ""
}

// CommandIDOf returns c's CommandID, or "" for a nil command.
func CommandIDOf(c Command) string {
	if c == nil {
		return ""
	}
	return c.commandID()
}

// commandEnvelope is MarshalCommand's encoding.
type commandEnvelope struct {
	Kind    CommandKind     `json:"kind"`
	Command json.RawMessage `json:"command"`
}

// MarshalCommand encodes c with its kind, as
// {"kind":"start","command":{...}}, for a host that saves commands before it
// runs them. UnmarshalCommand returns an equal command with the same digest,
// so a saved command replayed after a crash is a retry, not a conflict. Raw
// JSON values, such as an output schema or tool arguments, come back
// compacted, which keeps their meaning and the digest.
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
	case CommandDeliver:
		return decodeCommand[Deliver](env.Command)
	case CommandContinue:
		return decodeCommand[Continue](env.Command)
	case CommandSettle:
		return decodeCommand[Settle](env.Command)
	case CommandCancel:
		return decodeCommand[Cancel](env.Command)
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
