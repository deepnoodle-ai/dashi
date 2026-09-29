package llm

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
	if c.HostedUse != nil {
		v := c.HostedUse.Clone()
		c.HostedUse = &v
	}
	if c.HostedResult != nil {
		v := c.HostedResult.Clone()
		c.HostedResult = &v
	}
	if c.HostedApprovalRequest != nil {
		v := c.HostedApprovalRequest.Clone()
		c.HostedApprovalRequest = &v
	}
	if c.HostedApprovalResponse != nil {
		v := *c.HostedApprovalResponse
		c.HostedApprovalResponse = &v
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
