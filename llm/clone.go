package llm

import "slices"

// Clone returns an independent content value, including provider replay data.
func (c Content) Clone() Content {
	c.Replay = cloneOpaque(c.Replay)
	c.Opaque = cloneOpaque(c.Opaque)
	if c.Block != nil {
		v := c.Block.Clone()
		c.Block = &v
	}
	if c.ToolCall != nil {
		v := c.ToolCall.Clone()
		c.ToolCall = &v
	}
	if c.ToolResult != nil {
		v := *c.ToolResult
		v.Output = v.Output.Clone()
		c.ToolResult = &v
	}
	if c.ProviderToolUse != nil {
		v := c.ProviderToolUse.Clone()
		c.ProviderToolUse = &v
	}
	if c.ProviderToolResult != nil {
		v := c.ProviderToolResult.Clone()
		c.ProviderToolResult = &v
	}
	if c.ProviderToolApprovalRequest != nil {
		v := c.ProviderToolApprovalRequest.Clone()
		c.ProviderToolApprovalRequest = &v
	}
	if c.ProviderToolApprovalResponse != nil {
		v := *c.ProviderToolApprovalResponse
		c.ProviderToolApprovalResponse = &v
	}
	if c.Reasoning != nil {
		v := *c.Reasoning
		c.Reasoning = &v
	}
	if c.Refusal != nil {
		v := *c.Refusal
		c.Refusal = &v
	}
	if c.Reminder != nil {
		v := *c.Reminder
		c.Reminder = &v
	}
	return c
}

// Clone returns independently owned content, provenance, and context identity.
func (m Message) Clone() Message {
	parts := make([]Content, len(m.Content))
	for i, c := range m.Content {
		parts[i] = c.Clone()
	}
	m.Content = parts
	if m.Source != nil {
		v := *m.Source
		m.Source = &v
	}
	if m.Context != nil {
		v := *m.Context
		m.Context = &v
	}
	return m
}

func cloneOpaque(o *Opaque) *Opaque {
	if o == nil {
		return nil
	}
	v := o.Clone()
	return &v
}

// clone returns a snapshot that shares no memory with s.
func (s RequestSnapshot) clone() RequestSnapshot {
	s.Input = s.Input.clone()
	s.Effective = s.Effective.clone()
	s.Body = slices.Clone(s.Body)
	s.Adjustments = cloneAdjustments(s.Adjustments)
	s.Warnings = slices.Clone(s.Warnings)
	s.ContentChanges = slices.Clone(s.ContentChanges)
	if s.ModelInfo != nil {
		v := s.ModelInfo.Clone()
		s.ModelInfo = &v
	}
	s.Unverified = slices.Clone(s.Unverified)
	return s
}

// clone returns input that shares no memory with i. Unlike Message.Clone, it
// keeps a nil content list nil, so the copy encodes as the original does.
func (i Input) clone() Input {
	if i.Messages != nil {
		messages := make([]Message, len(i.Messages))
		for k, m := range i.Messages {
			messages[k] = m.Clone()
			if m.Content == nil {
				messages[k].Content = nil
			}
		}
		i.Messages = messages
	}
	i.Tools = slices.Clone(i.Tools)
	for k := range i.Tools {
		i.Tools[k] = i.Tools[k].Clone()
	}
	i.OutputSchema = slices.Clone(i.OutputSchema)
	i.Settings = i.Settings.clone()
	return i
}

// clone returns settings that share no memory with s.
func (s Settings) clone() Settings {
	s.Temperature = clonePointer(s.Temperature)
	s.Reasoning = clonePointer(s.Reasoning)
	s.ReasoningSummary = clonePointer(s.ReasoningSummary)
	s.ToolChoice = clonePointer(s.ToolChoice)
	s.ParallelTools = clonePointer(s.ParallelTools)
	return s
}

func cloneAdjustments(adjustments []Adjustment) []Adjustment {
	adjustments = slices.Clone(adjustments)
	for k := range adjustments {
		adjustments[k] = adjustments[k].Clone()
	}
	return adjustments
}

// clonePointer copies the value p points to; it suits types without
// pointers, slices, or maps of their own.
func clonePointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
