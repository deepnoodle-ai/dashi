// Package messages implements the Messages API. Request encoding and response
// handling are adapted from Dive (65c393f), Apache-2.0, for Dashi's
// contracts.
// This package owns API conversion and dispatch, not agent or session state.
//
// # Provider tools
//
// Provider tool constructors return a definition-only tool.Tool: Anthropic runs
// it inside the model call, so it has no Executor and no hook sees its calls.
// Add it to a Runner with tool.List, or pass its Definition in llm.Input.Tools. A
// constructor returns an error only when its options can be invalid.
//
// Each options struct has a Provider field that binds the tool to one
// configured destination. Empty means "anthropic", the default name of
// Config.Provider; set it to the client's Provider() for a destination with a
// custom name, since a destination refuses provider tools bound to another.
package messages

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/catalog"
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
	// Timeouts bounds this client's HTTP model calls.
	Timeouts llm.HTTPTimeouts

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
		config.Timeouts,
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
	if summary := r.Settings.ReasoningSummary; summary != nil && *summary && r.Settings.Reasoning == nil {
		return llm.Request{}, wire.unsupported(
			"reasoning_summary requires configured reasoning; add a supported Reasoning effort or budget, or choose another route",
		)
	}
	if len(r.OutputSchema) > 0 {
		return llm.Request{}, wire.unsupported("output_schema is not supported by this route")
	}
	r, changes, err := providercontent.Adapt(r, wire.origin)
	if err != nil {
		return llm.Request{}, err
	}
	r, adjustments, err := p.client.Fit(r, p.limits(r), nil)
	if err != nil {
		return llm.Request{}, err
	}
	cachePoint, adjustments := p.cachePoint(&r, adjustments)
	wire.citations = r.Settings.Citations
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
	if summary := r.Settings.ReasoningSummary; summary != nil && r.Settings.Reasoning != nil {
		info, _ := p.client.ModelInfo(r.Model)
		if *summary {
			body["thinking"].(map[string]any)["display"] = "summarized"
		} else if info.Capabilities.Features[llm.FeatureReasoningSummary] == llm.SupportSupported {
			body["thinking"].(map[string]any)["display"] = "omitted"
		}
	}
	var defs []any
	var mcpServers []any
	for _, d := range r.Tools {
		if d.Kind == tool.KindProviderTool {
			config, err := wire.providerTool(d)
			if err != nil {
				return llm.Request{}, err
			}
			defs = append(defs, config)
			if d.ProviderTool.Type == MCPToolsetType {
				var options MCPToolOptions
				if err := decodeProviderToolOptions(d.ProviderTool.Options, &options); err != nil {
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
		if cachePoint != nil && r.System == "" {
			// Without system text the fixed cache point covers the tools.
			defs[len(defs)-1].(map[string]any)["cache_control"] = cachePoint
		}
		body["tools"] = defs
	}
	if len(mcpServers) > 0 {
		body["mcp_servers"] = mcpServers
	}
	if c := r.Settings.ToolChoice; c != nil {
		if c.Mode == llm.ToolChoiceTool {
			for _, definition := range r.Tools {
				if definition.Name == c.Name &&
					definition.Kind == tool.KindProviderTool {
					return llm.Request{}, wire.unsupported(
						"named choice of provider tool %q",
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
		block := map[string]any{"type": "text", "text": r.System}
		if cachePoint != nil {
			// The fixed cache point covers the tools and the system prompt.
			// System text hoisted from the history comes after it.
			block["cache_control"] = cachePoint
		}
		system = append(system, block)
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
	if cachePoint != nil {
		// The automatic cache point: Anthropic places it on the last
		// cacheable block and moves it forward as the conversation grows.
		body["cache_control"] = cachePoint
	}
	return p.client.Prepare(requested, r, body, adjustments, changes)
}

// cacheTTLs are the cache lifetimes Messages offers on every current model.
// The shortest is the default, sent without a ttl field.
var cacheTTLs = []time.Duration{5 * time.Minute, time.Hour}

// cachePoint returns the cache_control value for both cache points, or nil
// when the request caches nothing. PromptCache.Disable sends none. A zero
// TTL caches by default only on a catalog destination marked AutoCache,
// recorded as a default lifetime; it is skipped where the model rejects a
// cache request, and under strict policy where its support is unknown. A
// nonzero TTL is an explicit request, checked like any feature.
func (p *Client) cachePoint(
	r *llm.Input,
	adjustments []llm.Adjustment,
) (map[string]any, []llm.Adjustment) {
	cache := &r.Settings.PromptCache
	if cache.Disable {
		return nil, adjustments
	}
	if cache.TTL == 0 {
		if !catalog.AutoCache(API, p.client.Endpoint) {
			return nil, adjustments
		}
		info, _ := p.client.ModelInfo(r.Model)
		switch info.Capabilities.Features[llm.FeaturePromptCache] {
		case llm.SupportUnsupported:
			return nil, adjustments
		case llm.SupportUnknown:
			if p.client.Policy() == llm.CapabilityStrict {
				return nil, adjustments
			}
		}
		cache.TTL = cacheTTLs[0]
		adjustments = append(
			adjustments,
			llm.Adjustment{
				Field:     llm.SettingPromptCacheTTL,
				Action:    llm.AdjustmentDefault,
				Requested: json.RawMessage("null"),
				Effective: json.RawMessage(`"5m"`),
				Reason: "prompt caching is on by default at this destination; " +
					"using the default lifetime 5m",
			},
		)
	}
	point := map[string]any{"type": "ephemeral"}
	if llm.CacheLifetime(cache.TTL, cacheTTLs) == time.Hour {
		point["ttl"] = "1h"
	}
	return point, adjustments
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
		CacheTTLs:          cacheTTLs,
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
		if definition.ProviderTool != nil &&
			definition.ProviderTool.Type == MCPToolsetType {
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
		result, callErr = wire.readStream(resp.Body, r, sink)
		if callErr == nil {
			callErr = providerhttp.Complete(wire.origin.Provider, resp.Body)
		}
		return result, callErr
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return r, providerhttp.ReadFailure(wire.origin.Provider, err)
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
	} else {
		err = providerhttp.Complete(wire.origin.Provider, resp.Body)
	}
	return decoded, err
}

// codec carries call-specific provenance through encoding and decoding.
type codec struct {
	origin content.Origin
	// media reports whether the request being sent carries media, so errors
	// reported in its response body can be classified as media rejections.
	media bool
	// citations enables citations on every document the request encodes;
	// Messages accepts citations on all documents of a request or none.
	citations bool
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
		s := b.Image.Source
		if s.URI != "" {
			return nil, wire.unsupported("provider-managed image URI")
		}
		if s.Data != nil && !slices.Contains(imageTypes, s.MIMEType) {
			return nil, wire.unsupported(
				"image type %q; Messages accepts %s; set Source.MIMEType "+
					"if the type is wrong",
				s.MIMEType,
				strings.Join(imageTypes, ", "),
			)
		}
		return map[string]any{"type": "image", "source": source(s)}, nil
	case content.KindDocument:
		src, err := wire.documentSource(b.Document.Source)
		if err != nil {
			return nil, err
		}
		m := map[string]any{"type": "document", "source": src}
		if b.Document.Title != "" {
			m["title"] = b.Document.Title
		}
		if wire.citations {
			m["citations"] = map[string]any{"enabled": true}
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
	case llm.ContentProviderToolUse, llm.ContentProviderToolResult,
		llm.ContentProviderToolApprovalRequest:
		if c.Replay == nil || c.Replay.Unit != llm.OpaqueUnitContentBlock {
			return nil, wire.unsupported("provider tool activity needs original replay data")
		}
		return providerhttp.Overlay(wire.origin, c.Replay, map[string]any{})
	case llm.ContentProviderToolApprovalResponse:
		return nil, wire.unsupported("Anthropic provider tool approval response")
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
	usage, tier, reported, err := decodeUsage(v.Usage)
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
	// A response without usage gets no counts; the runner accounts for its
	// provider tool uses.
	if r.Usage != nil {
		if r.Usage.Requests == nil {
			r.Usage.Requests = map[string]int{}
		}
		countProviderToolBlocks(r.Usage.Requests, reported, v.Content)
		if len(r.Usage.Requests) == 0 {
			r.Usage.Requests = nil
		}
		// A stop reason arrives with the final usage (in message_delta when
		// streaming). Without one, the response was cut off and its counts
		// may be inferred from the blocks seen so far.
		r.Usage.RequestsComplete = v.StopReason != ""
	}
	return r, nil
}

// decodeUsage converts a Messages usage object. Anthropic reports
// service_tier inside usage; Dashi records it on Result, where callers can
// select the applicable price sheet. Fields are decoded from Anthropic's own
// names rather than through llm.Usage's JSON tags, which describe Dashi's
// encoding. It also returns the meters usage has a counter for, so block
// counting can skip them. Keep the original response separately in
// Result.Raw.
func decodeUsage(
	raw json.RawMessage,
) (*llm.Usage, string, map[string]bool, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, "", nil, nil
	}
	var v struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreation            struct {
			OneHour int `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
		ServiceTier   string          `json:"service_tier"`
		ServerToolUse json.RawMessage `json:"server_tool_use"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, "", nil, err
	}
	usage := &llm.Usage{
		InputTokens:              v.InputTokens,
		OutputTokens:             v.OutputTokens,
		CacheCreationInputTokens: v.CacheCreationInputTokens,
		CacheReadInputTokens:     v.CacheReadInputTokens,
		// The one-hour writes are part of cache_creation_input_tokens.
		CacheCreation1hInputTokens: v.CacheCreation.OneHour,
	}
	var reported map[string]bool
	if len(v.ServerToolUse) > 0 && string(v.ServerToolUse) != "null" {
		requests, counters, err := serverToolRequests(v.ServerToolUse)
		if err != nil {
			return nil, "", nil, err
		}
		reported = counters
		if len(requests) > 0 {
			usage.Requests = requests
		}
	}
	return usage, v.ServiceTier, reported, nil
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
			c, err := wire.decodeCitation(raw)
			if err != nil {
				return llm.Content{}, err
			}
			b.Text.Citations = append(b.Text.Citations, c)
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
		return wire.decodeProviderToolUse(raw, replay)
	case "web_search_tool_result", "web_fetch_tool_result",
		"code_execution_tool_result", "bash_code_execution_tool_result",
		"text_editor_code_execution_tool_result", "mcp_tool_result":
		return wire.decodeProviderToolResult(raw, replay)
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

// decodeCitation keeps the raw citation for replay and reads its display
// fields. Document citations carry document_title instead of title, and page
// and character locations are translated; other kinds stay in Raw only.
func (wire codec) decodeCitation(raw json.RawMessage) (content.Citation, error) {
	var v struct {
		Type            string
		URL             string
		Title           string
		DocumentTitle   string `json:"document_title"`
		CitedText       string `json:"cited_text"`
		DocumentIndex   int    `json:"document_index"`
		StartCharIndex  int    `json:"start_char_index"`
		EndCharIndex    int    `json:"end_char_index"`
		StartPageNumber int    `json:"start_page_number"`
		EndPageNumber   int    `json:"end_page_number"`
		Source          string
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return content.Citation{}, err
	}
	c := content.Citation{
		URL:    v.URL,
		Title:  cmp.Or(v.Title, v.DocumentTitle),
		Text:   v.CitedText,
		Origin: wire.origin,
		Raw:    slices.Clone(raw),
	}
	// A search result citation names its result's source, usually a URL.
	if v.Type == "search_result_location" && c.URL == "" {
		c.URL = v.Source
	}
	location := content.CitationLocation{RequestDocumentIndex: v.DocumentIndex}
	switch v.Type {
	case "char_location":
		location.Unit = content.LocationChar
		location.Start, location.End = v.StartCharIndex, v.EndCharIndex
	case "page_location":
		location.Unit = content.LocationPage
		location.Start, location.End = v.StartPageNumber, v.EndPageNumber
	default:
		return c, nil
	}
	// A malformed location is dropped rather than failing the response; Raw
	// still holds what the provider sent.
	if location.Validate() == nil {
		c.Location = &location
	}
	return c, nil
}

// imageTypes are the inline image types Messages accepts.
var imageTypes = []string{"image/jpeg", "image/png", "image/gif", "image/webp"}

// textDocumentTypes are non-text/* types that are plain text in practice and
// are sent as text documents.
var textDocumentTypes = []string{
	"application/json",
	"application/xml",
	"application/yaml",
	"application/x-yaml",
}

// documentSource encodes a document source. Messages takes inline PDFs as
// base64 and text as a text source; any text type, and JSON, XML, and YAML,
// is sent as text/plain, which is how Messages expects Markdown, CSV, and
// similar files. It fetches
// only PDFs by URL. Other types fail here rather than at send.
func (wire codec) documentSource(s content.Source) (map[string]any, error) {
	switch {
	case s.URI != "":
		return nil, wire.unsupported("provider-managed document URI")
	case s.URL != "" && s.MIMEType != "" && s.MIMEType != "application/pdf":
		return nil, wire.unsupported(
			"document URL of type %q; Messages fetches only PDFs by URL",
			s.MIMEType,
		)
	case s.Data == nil || s.MIMEType == "application/pdf":
		return source(s), nil
	case strings.HasPrefix(s.MIMEType, "text/") ||
		slices.Contains(textDocumentTypes, s.MIMEType):
		if !utf8.Valid(s.Data) {
			return nil, wire.unsupported(
				"text document of type %q is not valid UTF-8",
				s.MIMEType,
			)
		}
		return map[string]any{
			"type":       "text",
			"media_type": "text/plain",
			"data":       string(s.Data),
		}, nil
	}
	return nil, wire.unsupported(
		"document type %q; Messages accepts PDF and text documents; "+
			"set Source.MIMEType if the type is wrong",
		s.MIMEType,
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
