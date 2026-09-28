// Package messages implements the Messages API. Request encoding and response
// handling are adapted from Dive (65c393f), Apache-2.0, for Dashi's
// contracts.
// This package owns API conversion and dispatch, not agent or session state.
//
// # Hosted tools
//
// Hosted tool constructors return a definition-only tool.Tool: Anthropic runs
// it inside the model call, so it has no Executor and needs no Policy. Add it
// to a Runner with tool.List, or pass its Definition in llm.Input.Tools. A
// constructor returns an error only when its options can be invalid.
//
// Each options struct has a Provider field that binds the tool to one
// configured destination. Empty means "anthropic", the default name of
// Config.Provider; set it to the client's Provider() for a destination with a
// custom name, since a destination refuses hosted tools bound to another.
package messages

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/providercontent"
	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// API identifies the wire format independently of provider identity.
const API = "anthropic.messages"

// Config describes one destination using the Messages API format.
type Config struct {
	// Provider is the application's stable name for this destination. It
	// defaults to "anthropic" for the public endpoint; custom endpoints
	// require an explicit name.
	Provider string

	// APIKey defaults to ANTHROPIC_API_KEY, read only when Provider is
	// "anthropic" so the key never reaches another destination. With no key
	// for the public endpoint, New and Prepare still succeed and Send fails
	// before dispatch with an error wrapping llm.ErrNoAPIKey.
	APIKey string

	// Endpoint defaults to the public API URL.
	Endpoint string

	// Client is shared and supplies transport and timeout behavior. Redirects
	// are disabled; its transport must not retry requests internally.
	Client *http.Client

	// Models supplies evidence for this API and endpoint, keyed by exact model
	// ID. New validates scope and snapshots these values. An entry replaces
	// the catalog's for that model; the catalog applies only to the public
	// endpoint.
	Models map[string]llm.ModelInfo

	// CapabilityPolicy defaults to adjust: unknown features are sent and
	// recorded, and values beyond known limits are fitted with a warning.
	// CapabilityStrict rejects both instead.
	CapabilityPolicy llm.CapabilityPolicy
}

// Client implements llm.Client for a configured Messages destination.
// It holds no per-call state and is safe for concurrent calls. Model knowledge
// is owned; the configured HTTP client and transport remain shared.
type Client struct {
	client providerhttp.Client
}

var _ llm.Client = (*Client)(nil)

// New snapshots configuration. Custom endpoints require a provider identity.
// Sharing a name never authorizes replay across endpoints or credentials.
func New(config Config) (*Client, error) {
	const endpoint = "https://api.anthropic.com/v1/messages"
	if config.Endpoint == "" {
		config.Endpoint = endpoint
	}
	public := config.Endpoint == endpoint && config.Client == nil
	if config.Provider == "" {
		if config.Endpoint != endpoint {
			return nil, errors.New("messages: custom endpoint requires provider identity")
		}
		config.Provider = "anthropic"
	}
	// The anthropic key is read only for the anthropic destination, so a
	// client for another provider never sends it elsewhere.
	if config.APIKey == "" && config.Provider == "anthropic" {
		config.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	client, err := providerhttp.NewClient(
		config.Provider,
		API,
		config.Endpoint,
		config.APIKey,
		config.Client,
		http.Header{
			"X-Api-Key":         {config.APIKey},
			"Anthropic-Version": {"2023-06-01"},
		},
		config.Models,
		config.CapabilityPolicy,
	)
	if err != nil {
		return nil, err
	}
	client.Classify = errorReason
	client.NoKey = providerhttp.NoAPIKey(
		config.Provider,
		config.APIKey,
		public,
		"ANTHROPIC_API_KEY",
	)
	return &Client{client: client}, nil
}

// Provider identifies the configured destination in model references and
// errors.
func (p *Client) Provider() string {
	return p.client.Provider
}

// API identifies the format this client sends and receives.
func (*Client) API() string {
	return API
}

// ModelInfo returns an independent snapshot; false means the model is unknown.
func (p *Client) ModelInfo(model string) (llm.ModelInfo, bool) {
	return p.client.ModelInfo(model)
}

// Prepare validates r and encodes it as a Messages request without contacting
// the API. Under the default capability policy, settings beyond the API's or
// model's known limits are fitted to the nearest accepted value and recorded
// as warnings; content the API cannot express is rejected as Unsupported
// rather than dropped. Defaults and content adaptations are recorded on the
// request.
func (p *Client) Prepare(
	ctx context.Context,
	r llm.Input,
) (llm.Request, error) {
	wire := codec{origin: p.client.Origin(r.Model)}
	requested := r
	if err := ctx.Err(); err != nil {
		return llm.Request{}, err
	}
	if err := r.Validate(); err != nil {
		return llm.Request{}, providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorInvalidRequest,
			"%w",
			err,
		)
	}
	r, changes, err := providercontent.Adapt(r, wire.origin)
	if err != nil {
		return llm.Request{}, err
	}
	r, adjustments, err := p.client.Fit(r, p.limits(r), nil)
	if err != nil {
		return llm.Request{}, err
	}
	// Fit leaves MaxTokens unset only when the model rejects an output limit
	// or, under the strict policy, when support for one is unknown.
	if r.Settings.MaxTokens == 0 {
		return llm.Request{}, wire.unsupported(
			"Messages requires max_tokens and model support for it is not " +
				"known; add ModelInfo evidence of max_tokens support " +
				"or use the adjust capability policy",
		)
	}
	body := map[string]any{
		"model":      r.Model,
		"max_tokens": r.Settings.MaxTokens,
	}
	if t := r.Settings.Temperature; t != nil {
		if *t > 1 {
			return llm.Request{}, wire.unsupported("temperature must be at most 1")
		}
		body["temperature"] = *t
	}
	if s := r.Settings.Reasoning; s != nil {
		for _, m := range slices.Backward(r.Messages) {
			if m.Role == llm.RoleSystem || m.Role == llm.RoleDeveloper {
				continue
			}
			if m.Role == llm.RoleAssistant {
				return llm.Request{}, wire.unsupported(
					"assistant prefill with reasoning; " +
						"end the history with a user message or remove Reasoning",
				)
			}
			break
		}
		if r.Settings.Temperature != nil {
			return llm.Request{}, wire.unsupported(
				"temperature with reasoning; remove Temperature or Reasoning",
			)
		}
		if s.BudgetTokens > 0 {
			if s.BudgetTokens < 1024 || s.BudgetTokens >= r.Settings.MaxTokens {
				return llm.Request{}, wire.unsupported(
					"reasoning budget must be at least 1024 and below max tokens; " +
						"raise max_tokens above 1024",
				)
			}
			body["thinking"] = map[string]any{
				"type":          "enabled",
				"budget_tokens": s.BudgetTokens,
			}
		} else {
			switch s.Effort {
			case "low", "medium", "high", "max":
			default:
				return llm.Request{}, wire.unsupported("reasoning effort %q", s.Effort)
			}
			body["thinking"] = map[string]any{"type": "adaptive"}
			body["output_config"] = map[string]any{"effort": s.Effort}
		}
	}
	var defs []any
	var mcpServers []any
	for _, d := range r.Tools {
		if d.Kind == tool.KindProviderHosted {
			config, err := wire.hostedTool(d)
			if err != nil {
				return llm.Request{}, err
			}
			defs = append(defs, config)
			if d.Hosted.Type == MCPToolsetType {
				var options MCPToolOptions
				if err := decodeHostedOptions(d.Hosted.Options, &options); err != nil {
					return llm.Request{}, wire.unsupported("MCP toolset options: %v", err)
				}
				mcpServers = append(
					mcpServers,
					map[string]any{
						"type": "url",
						"name": options.Name,
						"url":  options.URL,
					},
				)
			}
			continue
		}
		if schema := bytes.TrimSpace(d.Schema); len(schema) == 0 ||
			schema[0] != '{' {
			return llm.Request{}, wire.unsupported(
				"tool %q requires an object schema",
				d.Name,
			)
		}
		defs = append(
			defs,
			map[string]any{
				"name":         d.Name,
				"description":  d.Description,
				"input_schema": d.Schema,
			},
		)
	}
	if len(defs) > 0 {
		body["tools"] = defs
	}
	if len(mcpServers) > 0 {
		body["mcp_servers"] = mcpServers
	}
	if c := r.Settings.ToolChoice; c != nil {
		if c.Mode == llm.ToolChoiceTool {
			for _, definition := range r.Tools {
				if definition.Name == c.Name &&
					definition.Kind == tool.KindProviderHosted {
					return llm.Request{}, wire.unsupported(
						"named choice of hosted tool %q",
						c.Name,
					)
				}
			}
		}
		if r.Settings.Reasoning != nil &&
			(c.Mode == llm.ToolChoiceAny ||
				c.Mode == llm.ToolChoiceTool) {
			return llm.Request{}, wire.unsupported(
				"forced tool choice with reasoning; " +
					"use auto or none tool choice, or remove Reasoning",
			)
		}
		choice := map[string]any{"type": c.Mode}
		if c.Name != "" {
			choice["name"] = c.Name
		}
		body["tool_choice"] = choice
	}
	// Messages controls parallel tool use only through tool_choice, so an
	// auto choice is created when the request did not set one.
	if parallel := r.Settings.ParallelTools; parallel != nil {
		choice, _ := body["tool_choice"].(map[string]any)
		if choice == nil {
			choice = map[string]any{"type": "auto"}
			r.Settings.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceAuto}
			adjustments = append(
				adjustments,
				llm.Adjustment{
					Field:     llm.SettingToolChoice,
					Action:    llm.AdjustmentDefault,
					Requested: json.RawMessage("null"),
					Effective: json.RawMessage(`{"mode":"auto"}`),
					Reason:    "Messages requires tool choice to encode parallel tool use",
				},
			)
		}
		choice["disable_parallel_tool_use"] = !*parallel
		body["tool_choice"] = choice
	}
	var system []any
	if r.System != "" {
		system = append(system, map[string]any{"type": "text", "text": r.System})
	}
	var messages []any
	for _, m := range r.Messages {
		var blocks []any
		for _, c := range m.Content {
			// Messages has no system or developer role; their text joins the
			// top-level system blocks in order.
			if m.Role == llm.RoleSystem || m.Role == llm.RoleDeveloper {
				if c.Block == nil || c.Block.Text == nil {
					return llm.Request{}, wire.unsupported("non-text system content")
				}
				b, err := wire.encode(c)
				if err != nil {
					return llm.Request{}, err
				}
				system = append(system, b)
				continue
			}
			if (c.ToolCall != nil ||
				c.Reasoning != nil ||
				c.Refusal != nil) &&
				m.Role != llm.RoleAssistant {
				return llm.Request{}, wire.unsupported(
					"assistant-only content in %s message",
					m.Role,
				)
			}
			if c.ToolResult != nil && m.Role != llm.RoleUser {
				return llm.Request{}, wire.unsupported("tool results require user role")
			}
			b, err := wire.encode(c)
			if err != nil {
				return llm.Request{}, err
			}
			blocks = append(blocks, b)
		}
		if m.Role == llm.RoleUser || m.Role == llm.RoleAssistant {
			messages = append(
				messages,
				map[string]any{
					"role":    m.Role,
					"content": blocks,
				},
			)
		}
	}
	if len(system) > 0 {
		body["system"] = system
	}
	body["messages"] = messages
	return p.client.Prepare(requested, r, body, adjustments, changes)
}

// limits states what the Messages API accepts from any model, so Fit can
// adjust a request toward it. Every model takes, and requires, max_tokens.
// Temperature is rejected alongside thinking; the adjust policy omits it
// there, and strict keeps the specific error below.
func (p *Client) limits(r llm.Input) llm.Capabilities {
	maxTokens := r.Settings.MaxTokens
	if maxTokens == 0 {
		maxTokens = llm.DefaultMaxTokens
	}
	info, _ := p.client.ModelInfo(r.Model)
	if limit := info.Capabilities.MaxOutputTokens; limit > 0 {
		maxTokens = min(maxTokens, limit)
	}
	api := llm.Capabilities{
		ReasoningEfforts:   []string{"low", "medium", "high", "max"},
		MinReasoningBudget: 1024,
		// The thinking budget must stay below max_tokens.
		MaxReasoningBudget: max(maxTokens-1, 1),
		MaxTemperature:     1,
		Features: map[llm.Feature]llm.Support{
			llm.FeatureMaxTokens: llm.SupportSupported,
		},
	}
	if r.Settings.Reasoning != nil && p.client.Policy() == llm.CapabilityAdjust {
		api.Features[llm.FeatureTemperature] = llm.SupportUnsupported
	}
	return api
}

// Send dispatches at most one Messages request. Stream mode is pinned by
// Prepare; a nil sink discards previews while still accumulating the result.
// Whenever an error is returned, the result's stop kind is incomplete and the
// result may still carry the content received before the failure.
func (p *Client) Send(
	ctx context.Context,
	call llm.Call,
) (result llm.Result, callErr error) {
	prepared, sink := call.Request, call.Sink
	wire := codec{
		origin: p.client.Origin(prepared.Input().Model),
		media:  providerhttp.HasMedia(prepared.Input()),
	}
	defer func() {
		// Warnings ride on every result, including partial ones, so a failed
		// call still explains how its request differed from the input.
		result.Warnings = prepared.Warnings()
		if callErr != nil {
			result.Stop.Kind = llm.StopIncomplete
		}
	}()
	r := llm.Result{
		Requested: llm.ModelRef{
			Provider: prepared.Origin().Provider,
			Model:    prepared.Input().Model,
		},
		Message: llm.Message{Role: llm.RoleAssistant},
	}
	client := p.client
	for _, definition := range prepared.Input().Tools {
		if definition.Hosted != nil &&
			definition.Hosted.Type == MCPToolsetType {
			client.Headers = client.Headers.Clone()
			client.Headers.Set("Anthropic-Beta", "mcp-client-2025-11-20")
			break
		}
	}
	resp, err := client.Post(ctx, prepared)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	if prepared.Input().Stream {
		return wire.readStream(resp.Body, r, sink)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return r, providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorTransport,
			"read response: %w",
			err,
		)
	}
	decoded, err := wire.decode(raw, r.Requested)
	if err == nil && decoded.Stop.Raw == "" {
		err = providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorProtocol,
			"message has no stop reason",
		)
	}
	if err != nil {
		decoded.Stop.Kind = llm.StopIncomplete
	}
	return decoded, err
}

// codec carries call-specific provenance through encoding and decoding.
type codec struct {
	origin content.Origin
	// media reports whether the request being sent carries media, so errors
	// reported in its response body can be classified as media rejections.
	media bool
}

// encodeData encodes a shared content block as a Messages content block.
// Citations are sent only as their original Anthropic objects, because display
// fields alone cannot reconstruct them.
func (wire codec) encodeData(b content.Block) (map[string]any, error) {
	switch b.Kind {
	case content.KindText:
		m := map[string]any{"type": "text", "text": b.Text.Text}
		if len(b.Text.Citations) > 0 {
			cs := make([]json.RawMessage, 0, len(b.Text.Citations))
			for _, c := range b.Text.Citations {
				if c.Origin != wire.origin || len(c.Raw) == 0 {
					return nil, wire.unsupported("citation without Anthropic replay data")
				}
				cs = append(cs, c.Raw)
			}
			m["citations"] = cs
		}
		return m, nil
	case content.KindImage:
		if b.Image.Detail != "" {
			return nil, wire.unsupported("image detail")
		}
		if b.Image.Source.URI != "" {
			return nil, wire.unsupported("provider-managed image URI")
		}
		return map[string]any{"type": "image", "source": source(b.Image.Source)}, nil
	case content.KindDocument:
		if b.Document.Source.URI != "" {
			return nil, wire.unsupported("provider-managed document URI")
		}
		m := map[string]any{"type": "document", "source": source(b.Document.Source)}
		if b.Document.Title != "" {
			m["title"] = b.Document.Title
		}
		return m, nil
	default:
		return nil, wire.unsupported("content kind %q", b.Kind)
	}
}

// encode encodes c as a Messages content block. When c carries replay data
// from a previous response, the current public fields are overlaid on that
// original object so private fields survive the round trip.
func (wire codec) encode(c llm.Content) (map[string]any, error) {
	for _, replay := range []*llm.Opaque{c.Replay, c.Opaque} {
		if replay != nil && replay.Unit != llm.OpaqueUnitContentBlock {
			return nil, wire.unsupported("replay unit %q", replay.Unit)
		}
	}
	var fields map[string]any
	var err error
	switch c.Kind {
	case llm.ContentBlock:
		fields, err = wire.encodeData(*c.Block)
	case llm.ContentToolCall:
		fields = map[string]any{
			"type":  "tool_use",
			"id":    c.ToolCall.ID,
			"name":  c.ToolCall.Name,
			"input": c.ToolCall.Args,
		}
	case llm.ContentToolResult:
		blocks := make([]any, 0, len(c.ToolResult.Output.Content))
		for _, b := range c.ToolResult.Output.Content {
			v, e := wire.encodeData(b)
			if e != nil {
				return nil, e
			}
			blocks = append(blocks, v)
		}
		fields = map[string]any{
			"type":        "tool_result",
			"tool_use_id": c.ToolResult.CallID,
			"is_error":    c.ToolResult.IsError,
			"content":     blocks,
		}
	case llm.ContentHostedUse, llm.ContentHostedResult,
		llm.ContentHostedApprovalRequest:
		if c.Replay == nil || c.Replay.Unit != llm.OpaqueUnitContentBlock {
			return nil, wire.unsupported("hosted activity needs original replay data")
		}
		return providerhttp.Overlay(wire.origin, c.Replay, map[string]any{})
	case llm.ContentHostedApprovalResponse:
		return nil, wire.unsupported("Anthropic hosted approval response")
	case llm.ContentReasoning:
		if c.Replay == nil || c.Replay.Unit != llm.OpaqueUnitContentBlock {
			return nil, wire.unsupported("reasoning needs its original replay data")
		}
		// Signed reasoning must be replayed unchanged. Text is for display.
		return providerhttp.Overlay(wire.origin, c.Replay, map[string]any{})
	case llm.ContentRefusal:
		fields = map[string]any{"type": "refusal", "refusal": *c.Refusal}
	case llm.ContentOpaque:
		return providerhttp.Overlay(wire.origin, c.Opaque, map[string]any{})
	}
	if err != nil {
		return nil, err
	}
	merged, err := providerhttp.Overlay(wire.origin, c.Replay, fields)
	if err != nil {
		return nil, err
	}
	// Overlay keeps replay fields the caller did not set, so citations or a
	// title the caller removed must be deleted here or they would replay stale.
	if c.Block != nil && c.Block.Text != nil && len(c.Block.Text.Citations) == 0 {
		delete(merged, "citations")
	}
	if c.Block != nil && c.Block.Document != nil && c.Block.Document.Title == "" {
		delete(merged, "title")
	}
	return merged, nil
}

// decode converts a complete Messages response into a Result. Streaming builds
// the same response object and decodes it here, so both paths agree.
func (wire codec) decode(
	raw []byte,
	requested llm.ModelRef,
) (llm.Result, error) {
	r := llm.Result{
		Requested: requested,
		Raw:       slices.Clone(raw),
		Message:   llm.Message{Role: llm.RoleAssistant},
	}
	var v struct {
		ID         string
		Model      string
		Type       string
		Content    []json.RawMessage
		StopReason string `json:"stop_reason"`
		Usage      json.RawMessage
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return r, providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorProtocol,
			"decode response: %w",
			err,
		)
	}
	if v.Type != "message" {
		return r, providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorProtocol,
			"expected message, received %q",
			v.Type,
		)
	}
	r.ID = v.ID
	r.Served = llm.ModelRef{Provider: wire.origin.Provider, Model: v.Model}
	if v.Model == "" {
		r.Served = llm.ModelRef{}
	}
	r.Message.Source = &llm.MessageSource{
		Requested:  requested,
		Served:     r.Served,
		API:        wire.origin.API,
		ResponseID: r.ID,
	}
	usage, tier, err := decodeUsage(v.Usage)
	if err != nil {
		return r, providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorProtocol,
			"decode usage: %w",
			err,
		)
	}
	r.Usage, r.ServiceTier = usage, tier
	r.Stop = llm.Stop{
		Kind: llm.ClassifyStopReason(v.StopReason),
		Raw:  v.StopReason,
	}
	for _, raw := range v.Content {
		c, err := wire.decodeContent(raw)
		if err != nil {
			return r, err
		}
		r.Message.Content = append(r.Message.Content, c)
	}
	return r, nil
}

// Anthropic reports service_tier inside usage. Dashi records it on Result,
// where callers can select the applicable price sheet, while Usage keeps only
// token measurements. Keep the original response separately in Result.Raw.
func decodeUsage(raw json.RawMessage) (*llm.Usage, string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, "", nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, "", err
	}
	var tier string
	if value, ok := fields["service_tier"]; ok {
		if err := json.Unmarshal(value, &tier); err != nil {
			return nil, "", err
		}
		delete(fields, "service_tier")
	}
	tokens, err := json.Marshal(fields)
	if err != nil {
		return nil, "", err
	}
	var usage llm.Usage
	if err := json.Unmarshal(tokens, &usage); err != nil {
		return nil, "", err
	}
	return &usage, tier, nil
}

// decodeContent converts one Messages content block. Every block keeps its
// original JSON as replay data, and unknown block types become opaque content
// so they can be replayed rather than lost.
func (wire codec) decodeContent(raw json.RawMessage) (llm.Content, error) {
	var v struct {
		Type      string
		Text      string
		ID        string
		Name      string
		Input     json.RawMessage
		Thinking  string
		Data      json.RawMessage
		Refusal   string
		Citations []json.RawMessage
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return llm.Content{}, err
	}
	replay := &llm.Opaque{
		Version: 1,
		Origin:  wire.origin,
		Type:    v.Type,
		Unit:    llm.OpaqueUnitContentBlock,
		Raw:     slices.Clone(raw),
	}
	switch v.Type {
	case "text":
		b := content.TextBlock(v.Text)
		for _, raw := range v.Citations {
			var c struct {
				URL       string
				Title     string
				CitedText string `json:"cited_text"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return llm.Content{}, err
			}
			b.Text.Citations = append(
				b.Text.Citations,
				content.Citation{
					URL:    c.URL,
					Title:  c.Title,
					Text:   c.CitedText,
					Origin: wire.origin,
					Raw:    slices.Clone(raw),
				},
			)
		}
		return llm.Content{Kind: llm.ContentBlock, Block: &b, Replay: replay}, nil
	case "tool_use":
		call := tool.Call{ID: v.ID, Name: v.Name, Args: v.Input}
		if err := call.Validate(); err != nil {
			return llm.Content{}, providerhttp.Failure(
				wire.origin.Provider,
				llm.ErrorProtocol,
				"invalid tool call: %w",
				err,
			)
		}
		return llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &call,
			Replay:   replay,
		}, nil
	case "server_tool_use", "mcp_tool_use":
		return wire.decodeHostedUse(raw, replay)
	case "web_search_tool_result", "web_fetch_tool_result",
		"code_execution_tool_result", "bash_code_execution_tool_result",
		"text_editor_code_execution_tool_result", "mcp_tool_result":
		return wire.decodeHostedResult(raw, replay)
	case "thinking":
		return llm.Content{
			Kind:      llm.ContentReasoning,
			Reasoning: &llm.Reasoning{Text: v.Thinking},
			Replay:    replay,
		}, nil
	case "redacted_thinking":
		// The encrypted data has no display text. Its original block is the
		// only representation that can be replayed to the same origin.
		var encrypted string
		if json.Unmarshal(v.Data, &encrypted) != nil || encrypted == "" {
			return llm.Content{Kind: llm.ContentOpaque, Opaque: replay}, nil
		}
		return llm.Content{
			Kind:      llm.ContentReasoning,
			Reasoning: &llm.Reasoning{},
			Replay:    replay,
		}, nil
	case "refusal":
		return llm.Content{
			Kind:    llm.ContentRefusal,
			Refusal: &v.Refusal,
			Replay:  replay,
		}, nil
	case "":
		return llm.Content{}, errors.New("anthropic: content has no type")
	default:
		return llm.Content{Kind: llm.ContentOpaque, Opaque: replay}, nil
	}
}

// unsupported reports a request feature the Messages API cannot express.
func (wire codec) unsupported(format string, args ...any) error {
	return providerhttp.Failure(
		wire.origin.Provider,
		llm.ErrorUnsupported,
		format,
		args...,
	)
}

// source encodes s as a Messages source object.
func source(s content.Source) map[string]any {
	if s.URL != "" {
		return map[string]any{"type": "url", "url": s.URL}
	}
	if s.ID != "" {
		return map[string]any{"type": "file", "file_id": s.ID}
	}
	return map[string]any{
		"type":       "base64",
		"media_type": s.MIMEType,
		"data":       base64.StdEncoding.EncodeToString(s.Data),
	}
}
