package llm

import (
	"encoding/json"
	"errors"
)

// UnmarshalJSON refuses obsolete replay locations instead of silently losing
// private data. Migrate prototype records explicitly before decoding them.
func (c *Content) UnmarshalJSON(data []byte) error {
	type plain Content
	var v struct {
		plain
		ProviderData json.RawMessage `json:"provider_data"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	var old struct {
		Reasoning struct {
			Replay json.RawMessage `json:"replay"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(data, &old); err != nil {
		return err
	}
	if len(v.ProviderData) != 0 || len(old.Reasoning.Replay) != 0 {
		return errors.New("llm: legacy replay fields require explicit migration to content.replay")
	}
	*c = Content(v.plain)
	return nil
}
