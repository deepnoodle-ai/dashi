package llm

import (
	"encoding/json"
	"errors"
	"slices"

	"github.com/deepnoodle-ai/dashi/content"
)

// HostedUse reports provider-executed work. Output is populated only when the
// API puts a completed result in the same item as the call. It is never a
// client tool call and must not be dispatched by an application.
type HostedUse struct {
	// ID identifies one provider-run invocation within its response.
	ID string `json:"id"`
	// Name is the provider's tool name, which need not match a local tool.
	Name string `json:"name"`
	// ServerName identifies the remote MCP server when one ran the tool.
	ServerName string `json:"server_name,omitempty"`
	// Type is the original provider item or block type.
	Type string `json:"type"`
	// Origin restricts replay to the source destination.
	Origin content.Origin `json:"origin"`
	// Status is the provider-reported execution state when available.
	Status string `json:"status,omitempty"`
	// Input is a display projection; Replay remains authoritative.
	Input json.RawMessage `json:"input,omitempty"`
	// Output exposes recognized result blocks without replacing Replay.
	Output []content.Block `json:"output,omitempty"`
	// IsError marks a provider-reported failure.
	IsError bool `json:"is_error,omitempty"`
}

// Validate requires provenance and keeps display fields from standing in for
// a provider's original replay data.
func (u HostedUse) Validate() error {
	if u.ID == "" ||
		u.Name == "" ||
		u.Type == "" ||
		u.Origin.Validate() != nil ||
		u.Origin.Model == "" ||
		(len(u.Input) > 0 &&
			!json.Valid(u.Input)) {
		return errors.New(
			"llm: hosted use requires ID, name, type, origin, and valid JSON input if provided",
		)
	}
	for _, block := range u.Output {
		if err := block.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Clone detaches JSON and output blocks from a result or input.
func (u HostedUse) Clone() HostedUse {
	u.Input = slices.Clone(u.Input)
	u.Output = cloneBlocks(u.Output)
	return u
}

// HostedResult records a separate provider result block paired with a use.
// Anthropic emits this shape; some Responses tools combine it with the use.
type HostedResult struct {
	// CallID pairs this block with a preceding HostedUse.
	CallID string `json:"call_id"`
	// Type retains the provider's result-block type.
	Type string `json:"type"`
	// Origin restricts replay to the source destination.
	Origin content.Origin `json:"origin"`
	// Output exposes recognized result blocks without replacing Replay.
	Output []content.Block `json:"output,omitempty"`
	// IsError marks a provider-reported failure.
	IsError bool `json:"is_error,omitempty"`
}

// Validate requires an identifiable provider result and valid output blocks.
func (r HostedResult) Validate() error {
	if r.CallID == "" || r.Type == "" || r.Origin.Validate() != nil ||
		r.Origin.Model == "" {
		return errors.New("llm: hosted result requires call ID, type, and origin")
	}
	for _, block := range r.Output {
		if err := block.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Clone detaches the result's output blocks.
func (r HostedResult) Clone() HostedResult {
	r.Output = cloneBlocks(r.Output)
	return r
}

// HostedApprovalRequest exposes a provider's request for application approval.
// It is never approved by decoding or by a default policy.
type HostedApprovalRequest struct {
	// ID identifies the decision the provider awaits.
	ID string `json:"id"`
	// Name is the requested remote tool.
	Name string `json:"name"`
	// ServerName identifies the MCP server that would execute it.
	ServerName string `json:"server_name,omitempty"`
	// Arguments are the provider's JSON proposal, not local call arguments.
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Origin restricts the decision to its original destination.
	Origin content.Origin `json:"origin"`
}

// Validate requires provider identity and any arguments to be JSON.
func (r HostedApprovalRequest) Validate() error {
	if r.ID == "" || r.Name == "" || r.Origin.Validate() != nil ||
		r.Origin.Model == "" ||
		(len(r.Arguments) > 0 && !json.Valid(r.Arguments)) {
		return errors.New(
			"llm: hosted approval request requires ID, name, origin, and valid JSON arguments if provided",
		)
	}
	return nil
}

// Clone detaches the provider's argument JSON.
func (r HostedApprovalRequest) Clone() HostedApprovalRequest {
	r.Arguments = slices.Clone(r.Arguments)
	return r
}

// HostedApprovalResponse carries the application's explicit decision for a
// provider request. The provider adapter verifies that the request is in
// history before encoding this response.
type HostedApprovalResponse struct {
	// RequestID identifies the approval request being answered.
	RequestID string `json:"request_id"`
	// Approve is the caller's explicit decision; false records denial.
	Approve bool `json:"approve"`
	// Reason optionally explains the decision to the provider.
	Reason string `json:"reason,omitempty"`
	// Origin must match the approval request's destination.
	Origin content.Origin `json:"origin"`
}

// Validate requires a provider-bound approval request ID.
func (r HostedApprovalResponse) Validate() error {
	if r.RequestID == "" ||
		r.Origin.Validate() != nil ||
		r.Origin.Model == "" {
		return errors.New(
			"llm: hosted approval response requires request ID and origin",
		)
	}
	return nil
}

func cloneBlocks(blocks []content.Block) []content.Block {
	if blocks == nil {
		return nil
	}
	out := make([]content.Block, len(blocks))
	for i, block := range blocks {
		out[i] = block.Clone()
	}
	return out
}
