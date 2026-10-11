package compaction

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Replacement holds validated summary history for application-owned
// activation. History owns its content. The application must still check
// its bound Turn ID, source revision, account, and deadline, and activate
// atomically. Validation does not prove that activation committed.
type Replacement struct {
	TurnID         string `json:"turn_id"`
	SourceHash     string `json:"source_hash"`
	SourceMessages int    `json:"source_messages"`
	// Summary is the extracted summary, without its framing prefix.
	Summary string        `json:"summary"`
	History []llm.Message `json:"history"`
}

// Clone returns a replacement with independent message content.
func (r Replacement) Clone() Replacement {
	r.History = llmclone.Messages(r.History)
	return r
}

// ValidateReplacement checks a completed summary against the exact saved
// compaction Start, including its initial Context, and source history. It
// returns a zero value on error and performs no execution or writes.
// Later AddMessages, steering input, and recorded continuation-history
// mismatches invalidate the replacement.
// Same-history recovery and authoritative model-result resolution remain
// valid. Recorded policy-hook context is permitted, but hook additions and
// settings changes lie outside the frozen-input proof.
//
// The caller owns durable jobs, summary execution, required versus optional
// summarization, activation transactions, and reload after unknown writes.
// Old history remains active until the caller commits activation.
func ValidateReplacement(
	frozen dashi.Start,
	source []llm.Message,
	summary dashi.Turn,
) (Replacement, error) {
	r, err := validateReplacement(frozen, source, summary)
	if err != nil {
		return Replacement{}, fmt.Errorf(
			"compaction: summary turn %s: %w", summary.ID, err,
		)
	}
	return r, nil
}

func validateReplacement(
	frozen dashi.Start,
	source []llm.Message,
	summary dashi.Turn,
) (Replacement, error) {
	f := frozen.Compaction
	if frozen.Origin != dashi.StartCompaction || f == nil {
		return Replacement{}, errors.New("frozen start requires compaction origin and metadata")
	}
	if f.Hash == "" {
		return Replacement{}, errors.New("frozen source requires a history hash")
	}
	if err := validateReplacementHistory(source); err != nil {
		return Replacement{}, fmt.Errorf("frozen source: %w", err)
	}
	hash, err := dashi.HistoryHash(source)
	if err != nil {
		return Replacement{}, err
	}
	if hash != f.Hash {
		return Replacement{}, errors.New("frozen source must match the compaction hash")
	}
	view, err := summary.View()
	if err != nil {
		return Replacement{}, err
	}
	if view.Origin != dashi.StartCompaction || view.Stop.Kind != dashi.StopCompleted {
		return Replacement{}, errors.New("summary requires a completed compaction turn")
	}
	started := summary.Steps[0]
	if err := equalReplacementValue(f, view.Compaction); err != nil {
		return Replacement{}, fmt.Errorf(
			"compaction metadata must match frozen start: %w",
			err,
		)
	}
	if err := equalReplacementValue(
		frozen.Input,
		started.Started.Input,
	); err != nil {
		return Replacement{}, fmt.Errorf(
			"initial input must match frozen start: %w",
			err,
		)
	}
	// Nil and empty Context both mean no initial items. Admission and JSON
	// storage can change that slice spelling without changing its meaning.
	if len(frozen.Context) != 0 || len(started.Started.Context) != 0 {
		if err := equalReplacementValue(
			frozen.Context,
			started.Started.Context,
		); err != nil {
			return Replacement{}, fmt.Errorf(
				"initial context must match frozen start: %w", err,
			)
		}
	}
	if err := equalReplacementValue(
		replacementInput(frozen),
		view.Input,
	); err != nil {
		return Replacement{}, fmt.Errorf("user input must remain frozen: %w", err)
	}
	if err := validateReplacementHistory(frozen.History); err != nil {
		return Replacement{}, fmt.Errorf("frozen prior history: %w", err)
	}
	priorHash, err := dashi.HistoryHash(frozen.History)
	if err != nil {
		return Replacement{}, err
	}
	if priorHash != started.Started.HistoryHash {
		return Replacement{}, errors.New("initial prior history must match frozen start")
	}
	if frozen.CommandID != "" && frozen.CommandID != started.CommandID {
		return Replacement{}, errors.New("start command ID must match frozen start")
	}
	for _, step := range summary.Steps {
		if c := step.Context; c != nil && len(c.Items) != 0 &&
			(c.Steering || step.Kind == dashi.StepMessagesAdded && step.CommandID != "") {
			return Replacement{}, errors.New("summary input must not contain later application context")
		}
		for _, warning := range step.Warnings {
			if warning.Code == dashi.WarningHistoryMismatch {
				return Replacement{}, errors.New("summary continuation must retain frozen prior history")
			}
		}
		if step.Kind == dashi.StepTurnStopped && step.Stop != nil && step.Stop.Ended {
			break
		}
	}
	history, ok := view.ReplacementHistory()
	if !ok {
		return Replacement{}, errors.New("completed summary requires nonempty summary text")
	}
	return Replacement{
		TurnID:         summary.ID,
		SourceHash:     hash,
		SourceMessages: len(source),
		Summary:        compactiontext.Summary(view.Reply),
		History:        llmclone.Messages(history),
	}, nil
}

// Validate messages before hashing so a summary cannot erase malformed
// history. Provider input validation allows empty messages and does not
// enforce Dashi's named-context authority; check those rules as well.
func validateReplacementHistory(history []llm.Message) error {
	for i, message := range history {
		if len(message.Content) == 0 {
			return fmt.Errorf("history message %d: empty message", i+1)
		}
		if err := (llm.Input{
			Model: "validation", Messages: []llm.Message{message},
		}).Validate(); err != nil {
			return fmt.Errorf("history message %d: %w", i+1, err)
		}
		if identity := message.Context; identity != nil {
			message.Context = nil
			if err := (dashi.ContextItem{
				Origin: dashi.ContextOrigin(identity.Origin),
				Name:   identity.Name, Message: message,
			}).Validate(); err != nil {
				return fmt.Errorf("history message %d: %w", i+1, err)
			}
		}
	}
	return nil
}

// Initial user items are part of TurnView.Input even after a next-call item
// expires. Match its recorded projection, including a named user slot.
func replacementInput(start dashi.Start) []llm.Message {
	input := llmclone.Messages(start.Input)
	for _, item := range start.Context {
		if item.Origin != "" && item.Origin != dashi.ContextUser {
			continue
		}
		message := item.Message.Clone()
		message.Context = nil
		if item.Name != "" {
			message.Context = &llm.MessageContext{
				Origin: string(dashi.ContextUser), Name: item.Name,
			}
		}
		input = append(input, message)
	}
	return input
}

// Compare full JSON meaning, including descriptive context identity that
// HistoryHash deliberately excludes. Raw provider values can change JSON
// spelling when a recorder stores them.
func equalReplacementValue(a, b any) error {
	x, err := canonjson.Marshal(a)
	if err != nil {
		return err
	}
	y, err := canonjson.Marshal(b)
	if err != nil {
		return err
	}
	if !bytes.Equal(x, y) {
		return errors.New("values differ")
	}
	return nil
}
