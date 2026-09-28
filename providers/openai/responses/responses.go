// Package responses implements the Responses API. Encoding, usage normalization,
// and replay handling are adapted from Dive's OpenAI provider (65c393f),
// Apache-2.0. Provider wire objects stay outside Kernel's public model types.
package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/internal/providercontent"
	"github.com/deepnoodle-ai/kernel/internal/providerhttp"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

// API identifies the wire format independently of provider identity.
const API = "openai.responses"

// Config describes one destination using the Responses API format.
type Config struct {
	// Provider is the application's stable name for this destination. It
	// defaults to "openai" for the public endpoint; custom endpoints
	// require an explicit name.
	Provider string

	// APIKey defaults to OPENAI_API_KEY at construction time.
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

// Client implements llm.Client for a configured Responses destination.
// It holds no per-call state and is safe for concurrent calls. Model knowledge
// is owned; the configured HTTP client and transport remain shared.
type Client struct {
	client providerhttp.Client
}

var _ llm.Client = (*Client)(nil)

// New snapshots configuration. Custom endpoints require a provider identity.
// Sharing a name never authorizes replay across endpoints or credentials.
func New(config Config) (*Client, error) {
	const endpoint = "https://api.openai.com/v1/responses"
	if config.APIKey == "" {
		config.APIKey = os.Getenv("OPENAI_API_KEY")
	}
	if config.Endpoint == "" {
		config.Endpoint = endpoint
	}
	if config.Provider == "" {
		if config.Endpoint != endpoint {
			return nil, errors.New("responses: custom endpoint requires provider identity")
		}
		config.Provider = "openai"
	}
	client, err := providerhttp.NewClient(config.Provider, API, config.Endpoint, config.APIKey,
		config.Client, http.Header{"Authorization": {"Bearer " + config.APIKey}}, config.Models, config.CapabilityPolicy)
	if err != nil {
		return nil, err
	}
	return &Client{client: client}, nil
}

// Provider identifies the configured destination in model references and errors.
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

// Prepare validates r and encodes it as a Responses request without contacting
// the API. Under the default capability policy, settings beyond the API's or
// model's known limits are fitted to the nearest accepted value and recorded
// as warnings; content the API cannot express is rejected as Unsupported
// rather than dropped.
func (p *Client) Prepare(ctx context.Context, r llm.Input) (llm.Request, error) {
	wire := codec{origin: p.client.Origin(r.Model)}
	requested := r
	if err := ctx.Err(); err != nil {
		return llm.Request{}, err
	}
	if err := r.Validate(); err != nil {
		return llm.Request{}, providerhttp.Failure(wire.origin.Provider, llm.ErrorInvalidRequest, "%w", err)
	}
	r, changes, err := providercontent.Adapt(r, wire.origin)
	if err != nil {
		return llm.Request{}, err
	}
	r, adjustments, err := p.client.Fit(r, apiLimits, nil)
	if err != nil {
		return llm.Request{}, err
	}
	// With store disabled the API keeps no reasoning state between turns, so
	// encrypted reasoning is requested in order to replay it on the next call.
	body := map[string]any{
		"model":   r.Model,
		"store":   false,
		"include": []string{"reasoning.encrypted_content"},
	}
	if r.System != "" {
		body["instructions"] = r.System
	}
	if n := r.Settings.MaxTokens; n > 0 {
		body["max_output_tokens"] = n
	}
	if t := r.Settings.Temperature; t != nil {
		if *t > 2 {
			return llm.Request{}, wire.unsupported("temperature must be at most 2")
		}
		body["temperature"] = *t
	}
	if s := r.Settings.Reasoning; s != nil {
		if s.BudgetTokens > 0 {
			return llm.Request{}, wire.unsupported("reasoning token budget; use a reasoning effort")
		}
		switch s.Effort {
		case "none", "minimal", "low", "medium", "high", "xhigh":
		default:
			return llm.Request{}, wire.unsupported("reasoning effort %q", s.Effort)
		}
		body["reasoning"] = map[string]any{"effort": s.Effort}
	}
	if p := r.Settings.ParallelTools; p != nil {
		body["parallel_tool_calls"] = *p
	}
	if c := r.Settings.ToolChoice; c != nil {
		if c.Mode == llm.ToolChoiceTool {
			for _, definition := range r.Tools {
				if definition.Name == c.Name &&
					definition.Kind == tool.KindProviderHosted {
					return llm.Request{}, wire.unsupported(
						"named choice of hosted tool %q", c.Name,
					)
				}
			}
		}
		switch c.Mode {
		case llm.ToolChoiceAuto, llm.ToolChoiceNone:
			body["tool_choice"] = c.Mode
		case llm.ToolChoiceAny:
			body["tool_choice"] = "required"
		case llm.ToolChoiceTool:
			body["tool_choice"] = map[string]any{"type": "function", "name": c.Name}
		}
	}
	var defs []any
	for _, d := range r.Tools {
		if d.Kind == tool.KindProviderHosted {
			config, includes, err := wire.hostedTool(d)
			if err != nil {
				return llm.Request{}, err
			}
			defs = append(defs, config)
			body["include"] = append(body["include"].([]string), includes...)
			continue
		}
		if schema := bytes.TrimSpace(d.Schema); len(schema) == 0 || schema[0] != '{' {
			return llm.Request{}, wire.unsupported("tool %q requires an object schema", d.Name)
		}
		// Explicit non-strict mode preserves the caller's schema unchanged.
		defs = append(defs, map[string]any{
			"type":        "function",
			"name":        d.Name,
			"description": d.Description,
			"parameters":  d.Schema,
			"strict":      false,
		})
	}
	if len(defs) > 0 {
		body["tools"] = defs
	}
	input := make([]any, 0)
	for _, m := range r.Messages {
		for _, c := range m.Content {
			if (c.ToolCall != nil || c.Reasoning != nil || c.Refusal != nil) && m.Role != llm.RoleAssistant {
				return llm.Request{}, wire.unsupported("assistant-only content in %s message", m.Role)
			}
			if c.ToolResult != nil && m.Role != llm.RoleUser {
				return llm.Request{}, wire.unsupported("tool results require user role")
			}
			item, err := wire.encode(c, m.Role)
			if err != nil {
				return llm.Request{}, err
			}
			// Reassemble parts from the same output message, preserving its ID,
			// phase, and content order instead of repeating a provider item ID.
			itemID, _ := item["id"].(string)
			if len(input) > 0 && item["type"] == "message" && itemID != "" {
				last, ok := input[len(input)-1].(map[string]any)
				lastID, _ := last["id"].(string)
				if ok && last["type"] == "message" && lastID == itemID {
					last["content"] = append(last["content"].([]any), item["content"].([]any)...)
					continue
				}
			}
			input = append(input, item)
		}
	}
	body["input"] = input
	return p.client.Prepare(requested, r, body, adjustments, changes)
}

// apiLimits states what the Responses API accepts from any model. It takes
// effort levels, not token budgets.
var apiLimits = llm.Capabilities{
	Features: map[llm.Feature]llm.Support{
		llm.FeatureReasoningBudget: llm.SupportUnsupported,
	},
	ReasoningEfforts: []string{"none", "minimal", "low", "medium", "high", "xhigh"},
	MaxTemperature:   2,
}

// Send dispatches at most one Responses request. Stream mode is pinned by
// Prepare; a nil sink discards previews while still accumulating the result.
// Whenever an error is returned, the result's stop kind is incomplete and the
// result may still carry the content received before the failure.
func (p *Client) Send(ctx context.Context, prepared llm.Request, sink llm.Sink) (result llm.Result, callErr error) {
	wire := codec{origin: p.client.Origin(prepared.Input().Model)}
	defer func() {
		// Warnings ride on every result, including partial ones, so a failed
		// call still explains how its request differed from the input.
		result.Warnings = prepared.Warnings()
		if callErr != nil {
			result.Stop.Kind = llm.StopIncomplete
		}
	}()
	r := llm.Result{
		Requested: llm.ModelRef{Provider: prepared.Origin().Provider, Model: prepared.Input().Model},
		Message:   llm.Message{Role: llm.RoleAssistant},
	}
	resp, err := p.client.Post(ctx, prepared)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	if prepared.Input().Stream {
		return wire.readStream(resp.Body, r, sink)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return r, providerhttp.Failure(wire.origin.Provider, llm.ErrorTransport, "read response: %w", err)
	}
	return wire.decode(raw, r.Requested)
}

// codec carries call-specific provenance through encoding and decoding.
type codec struct {
	origin content.Origin
}

// encodeData encodes a shared content block as a Responses content part.
// Assistant text uses output parts and may carry citations only as their
// original OpenAI annotations; user text uses input parts and cannot cite.
func (wire codec) encodeData(b content.Block, assistant bool) (map[string]any, error) {
	switch b.Kind {
	case content.KindText:
		t := "input_text"
		if assistant {
			t = "output_text"
		}
		m := map[string]any{"type": t, "text": b.Text.Text}
		if assistant {
			cs := make([]json.RawMessage, 0, len(b.Text.Citations))
			for _, c := range b.Text.Citations {
				if c.Origin != wire.origin || len(c.Raw) == 0 {
					return nil, wire.unsupported("citation without OpenAI replay data")
				}
				cs = append(cs, c.Raw)
			}
			m["annotations"] = cs
		} else if len(b.Text.Citations) > 0 {
			return nil, wire.unsupported("citations in input text")
		}
		return m, nil
	case content.KindImage:
		if assistant {
			return nil, wire.unsupported("assistant image requires provider replay data")
		}
		s := b.Image.Source
		if s.URI != "" {
			return nil, wire.unsupported("provider-managed image URI")
		}
		m := map[string]any{"type": "input_image", "detail": "auto"}
		switch b.Image.Detail {
		case "":
		case "auto", "low", "high":
			m["detail"] = b.Image.Detail
		default:
			return nil, wire.unsupported("image detail %q", b.Image.Detail)
		}
		switch {
		case s.ID != "":
			m["file_id"] = s.ID
		case s.URL != "":
			m["image_url"] = s.URL
		default:
			m["image_url"] = s.DataURL()
		}
		return m, nil
	case content.KindDocument:
		if assistant {
			return nil, wire.unsupported("document in assistant message")
		}
		s := b.Document.Source
		if s.URI != "" {
			return nil, wire.unsupported("provider-managed document URI")
		}
		m := map[string]any{"type": "input_file"}
		switch {
		case s.ID != "":
			m["file_id"] = s.ID
		case s.URL != "":
			m["file_url"] = s.URL
		default:
			m["file_data"] = s.DataURL()
			name := b.Document.Title
			if name == "" {
				name = "document.pdf"
			}
			m["filename"] = name
		}
		return m, nil
	default:
		return nil, wire.unsupported("content kind %q", b.Kind)
	}
}

// encode encodes c as a Responses input item. Shared blocks and refusals become
// single-part message items, which Prepare regroups by their original message
// ID. Replay data is overlaid so private fields survive the round trip.
func (wire codec) encode(c llm.Content, role llm.Role) (map[string]any, error) {
	switch c.Kind {
	case llm.ContentBlock:
		part, err := wire.encodeData(*c.Block, role == llm.RoleAssistant)
		if err != nil {
			return nil, err
		}
		return wire.encodePart(role, part, c.Replay)
	case llm.ContentRefusal:
		return wire.encodePart(role, map[string]any{"type": "refusal", "refusal": *c.Refusal}, c.Replay)
	case llm.ContentToolCall:
		if c.Replay != nil && c.Replay.Unit != llm.OpaqueUnitItem {
			return nil, wire.unsupported("tool call replay unit")
		}
		return providerhttp.Overlay(wire.origin, c.Replay, map[string]any{
			"type":      "function_call",
			"call_id":   c.ToolCall.ID,
			"name":      c.ToolCall.Name,
			"arguments": string(c.ToolCall.Args),
		})
	case llm.ContentToolResult:
		blocks := make([]any, 0, len(c.ToolResult.Output.Content))
		for _, b := range c.ToolResult.Output.Content {
			v, err := wire.encodeData(b, false)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, v)
		}
		// function_call_output has no error flag, so failure is stated to the
		// model in text ahead of the output.
		if c.ToolResult.IsError {
			failed := map[string]any{"type": "input_text", "text": "Tool failed:"}
			blocks = append([]any{failed}, blocks...)
		}
		return map[string]any{
			"type":    "function_call_output",
			"call_id": c.ToolResult.CallID,
			"output":  blocks,
		}, nil
	case llm.ContentReasoning:
		if c.Replay == nil || c.Replay.Unit != llm.OpaqueUnitItem {
			return nil, wire.unsupported("reasoning needs original replay data")
		}
		return providerhttp.Overlay(wire.origin, c.Replay, map[string]any{})
	case llm.ContentHostedUse, llm.ContentHostedResult,
		llm.ContentHostedApprovalRequest:
		if c.Replay == nil || c.Replay.Unit != llm.OpaqueUnitItem {
			return nil, wire.unsupported("hosted activity needs original item replay")
		}
		return providerhttp.Overlay(wire.origin, c.Replay, map[string]any{})
	case llm.ContentHostedApprovalResponse:
		item := map[string]any{
			"type":                "mcp_approval_response",
			"approval_request_id": c.HostedApprovalResponse.RequestID,
			"approve":             c.HostedApprovalResponse.Approve,
		}
		if c.HostedApprovalResponse.Reason != "" {
			item["reason"] = c.HostedApprovalResponse.Reason
		}
		return item, nil
	case llm.ContentOpaque:
		if c.Opaque.Unit == llm.OpaqueUnitMessagePart {
			return wire.encodePart(role, nil, c.Opaque)
		}
		if c.Opaque.Unit != llm.OpaqueUnitItem {
			return nil, wire.unsupported("opaque replay unit %q", c.Opaque.Unit)
		}
		return providerhttp.Overlay(wire.origin, c.Opaque, map[string]any{})
	}
	return nil, wire.unsupported("content kind %q", c.Kind)
}

// encodePart wraps part in a message item for role. With replay data, the
// original message metadata (such as its ID and phase) and the part's private
// fields are restored, and part's public fields are overlaid on them.
func (wire codec) encodePart(role llm.Role, part map[string]any, replay *llm.Opaque) (map[string]any, error) {
	message := map[string]any{"type": "message", "role": role}
	if replay != nil {
		if replay.Origin != wire.origin || replay.Unit != llm.OpaqueUnitMessagePart {
			return nil, wire.unsupported("foreign message replay data")
		}
		var v struct {
			Message map[string]any
			Part    map[string]any
		}
		if err := providerhttp.Decode(replay.Raw, &v); err != nil || v.Message == nil || v.Part == nil {
			return nil, errors.New("responses: message replay requires message and part objects")
		}
		message = v.Message
		message["role"] = role
		maps.Copy(v.Part, part)
		part = v.Part
	}
	message["content"] = []any{part}
	return message, nil
}

// decode converts a terminal Responses object into a Result. Streaming decodes
// the provider's final response object here as well, so both paths agree.
func (wire codec) decode(raw []byte, requested llm.ModelRef) (llm.Result, error) {
	r := llm.Result{
		Requested: requested,
		Raw:       slices.Clone(raw),
		Message:   llm.Message{Role: llm.RoleAssistant},
	}
	var v struct {
		ID                string
		Object            string
		Model             string
		Status            string
		Output            []json.RawMessage
		Usage             *llm.Usage
		IncompleteDetails struct{ Reason string } `json:"incomplete_details"`
		Error             json.RawMessage
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return r, providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "decode response: %w", err)
	}
	if v.Object != "response" {
		return r, providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "expected response, received %q", v.Object)
	}
	r.ID = v.ID
	r.Served = llm.ModelRef{Provider: wire.origin.Provider, Model: v.Model}
	if v.Model == "" {
		r.Served = llm.ModelRef{}
	}
	r.Message.Source = &llm.MessageSource{Requested: requested, Served: r.Served, API: wire.origin.API, ResponseID: r.ID}
	r.Usage = v.Usage
	switch v.Status {
	case "completed", "incomplete", "failed", "cancelled":
	default:
		return r, providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "response has no terminal status: %q", v.Status)
	}
	rawStop := v.Status
	stop := v.Status
	if stop == "completed" {
		stop = "end_turn"
	} else if v.Status == "incomplete" && v.IncompleteDetails.Reason != "" {
		stop = v.IncompleteDetails.Reason
		rawStop = v.IncompleteDetails.Reason
	}
	if stop == "max_output_tokens" {
		stop = "max_tokens"
	}
	for _, item := range v.Output {
		cs, err := wire.decodeItem(item)
		if err != nil {
			return r, err
		}
		r.Message.Content = append(r.Message.Content, cs...)
		if v.Status != "completed" {
			continue
		}
		// A completed status does not distinguish tool use or refusal from a
		// normal end, so those stop reasons come from the output. A partial
		// call is tolerated only in an unfinished response.
		for _, c := range cs {
			switch {
			case c.Opaque != nil && c.Opaque.Type == "function_call":
				return r, providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "completed response contains invalid tool arguments")
			case c.ToolCall != nil:
				stop = "tool_use"
			case c.Refusal != nil:
				stop = "refusal"
			}
		}
	}
	r.Stop = llm.Stop{Kind: llm.ClassifyStopReason(stop), Raw: rawStop}
	if v.Status == "failed" || (len(v.Error) > 0 && string(v.Error) != "null") {
		r.Stop.Kind = llm.StopIncomplete
		return r, &llm.Error{
			Provider: wire.origin.Provider,
			Category: llm.ErrorProtocol,
			Body:     slices.Clone(v.Error),
			Cause:    errors.New("responses: provider reported response failure"),
		}
	}
	return r, nil
}

// decodeItem converts one Responses output item. A message item yields one
// Content per part, each carrying the message metadata so Prepare can regroup
// them. Function calls whose arguments are not valid JSON become partial
// opaque content so they can never be executed. Unknown items stay opaque.
func (wire codec) decodeItem(raw json.RawMessage) ([]llm.Content, error) {
	var v struct{ Type string }
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	replay := &llm.Opaque{
		Version: 1,
		Origin:  wire.origin,
		Type:    v.Type,
		Unit:    llm.OpaqueUnitItem,
		Raw:     slices.Clone(raw),
	}
	switch v.Type {
	case "function_call":
		var callItem struct {
			CallID    string `json:"call_id"`
			Name      string
			Arguments string
		}
		if err := json.Unmarshal(raw, &callItem); err != nil {
			return nil, err
		}
		if !json.Valid([]byte(callItem.Arguments)) {
			replay.Partial = true
			return []llm.Content{{Kind: llm.ContentOpaque, Opaque: replay}}, nil
		}
		call := tool.Call{ID: callItem.CallID, Name: callItem.Name, Args: json.RawMessage(callItem.Arguments)}
		if err := call.Validate(); err != nil {
			return nil, providerhttp.Failure(wire.origin.Provider, llm.ErrorProtocol, "invalid tool call: %w", err)
		}
		return []llm.Content{{Kind: llm.ContentToolCall, ToolCall: &call, Replay: replay}}, nil
	case "reasoning":
		text, ok := reasoningSummary(raw)
		if !ok {
			return []llm.Content{{Kind: llm.ContentOpaque, Opaque: replay}}, nil
		}
		return []llm.Content{{
			Kind:      llm.ContentReasoning,
			Reasoning: &llm.Reasoning{Text: text},
			Replay:    replay,
		}}, nil
	case "web_search_call", "x_search_call", "file_search_call", "code_interpreter_call",
		"image_generation_call", "mcp_call":
		return wire.decodeHostedItem(raw, replay)
	case "mcp_approval_request":
		return wire.decodeHostedApproval(raw, replay)
	case "message":
		var message struct{ Content []json.RawMessage }
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, err
		}
		var metadata map[string]json.RawMessage
		_ = json.Unmarshal(raw, &metadata)
		delete(metadata, "content")
		var result []llm.Content
		for _, part := range message.Content {
			var p struct {
				Type        string
				Text        string
				Refusal     string
				Annotations []json.RawMessage
			}
			if err := json.Unmarshal(part, &p); err != nil {
				return nil, err
			}
			data, _ := json.Marshal(map[string]any{"message": metadata, "part": part})
			replay := &llm.Opaque{
				Version: 1,
				Origin:  wire.origin,
				Type:    p.Type,
				Unit:    llm.OpaqueUnitMessagePart,
				Raw:     data,
			}
			switch p.Type {
			case "output_text":
				b := content.TextBlock(p.Text)
				for _, annotation := range p.Annotations {
					var a struct {
						URL   string
						Title string
					}
					if err := json.Unmarshal(annotation, &a); err != nil {
						return nil, err
					}
					b.Text.Citations = append(b.Text.Citations, content.Citation{
						URL:    a.URL,
						Title:  a.Title,
						Origin: wire.origin,
						Raw:    slices.Clone(annotation),
					})
				}
				result = append(result, llm.Content{Kind: llm.ContentBlock, Block: &b, Replay: replay})
			case "refusal":
				result = append(result, llm.Content{Kind: llm.ContentRefusal, Refusal: &p.Refusal, Replay: replay})
			default:
				result = append(result, llm.Content{Kind: llm.ContentOpaque, Opaque: replay})
			}
		}
		return result, nil
	case "":
		return nil, errors.New("responses: output item requires type")
	default:
		return []llm.Content{{Kind: llm.ContentOpaque, Opaque: replay}}, nil
	}
}

// reasoningSummary recognizes only the summary and encrypted payload shapes
// this adapter understands. An unfamiliar reasoning item stays opaque so an
// omission policy cannot discard unknown semantics by assumption.
func reasoningSummary(raw json.RawMessage) (string, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return "", false
	}
	if content, exists := fields["content"]; exists && string(content) != "null" {
		return "", false
	}
	recognized := false
	if encrypted, exists := fields["encrypted_content"]; exists && string(encrypted) != "null" {
		var value string
		if json.Unmarshal(encrypted, &value) != nil || value == "" {
			return "", false
		}
		recognized = true
	}
	var text []string
	if summary, exists := fields["summary"]; exists && string(summary) != "null" {
		var parts []json.RawMessage
		if json.Unmarshal(summary, &parts) != nil || parts == nil {
			return "", false
		}
		recognized = true
		for _, part := range parts {
			var p struct {
				Type string
				Text *string
			}
			if json.Unmarshal(part, &p) != nil || p.Type != "summary_text" || p.Text == nil {
				return "", false
			}
			text = append(text, *p.Text)
		}
	}
	if !recognized {
		return "", false
	}
	return strings.Join(text, "\n\n"), true
}

// unsupported reports a request feature the Responses API cannot express.
func (wire codec) unsupported(format string, args ...any) error {
	return providerhttp.Failure(wire.origin.Provider, llm.ErrorUnsupported, format, args...)
}
