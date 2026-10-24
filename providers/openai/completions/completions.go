// Package completions implements the Chat Completions wire format. Conversion
// and wire types are adapted from Dive's openaicompletions provider (65c393f),
// Apache-2.0. This adapter owns one API call, not agent or retry state.
package completions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/outputschema"
	"github.com/deepnoodle-ai/dashi/internal/providercontent"
	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// API identifies Chat Completions independently of the configured destination.
const API = "openai.completions"

const openAIEndpoint = "https://api.openai.com/v1/chat/completions"

// Config describes one Chat Completions destination. Compatible services must
// supply their own stable Provider name and Endpoint.
type Config struct {
	Provider string
	// APIKey defaults to OPENAI_API_KEY, read only when Provider is "openai"
	// so the key never reaches another destination. With no key
	// for the public endpoint, New and Prepare still succeed and Send fails
	// before dispatch with an error wrapping llm.ErrNoAPIKey.
	APIKey   string
	Endpoint string
	Client   *http.Client
	// Timeouts bounds this client's HTTP model calls.
	Timeouts llm.HTTPTimeouts
	// Headers adds destination-specific headers such as OpenRouter attribution.
	// Authorization is always set from APIKey after these values are copied.
	Headers          http.Header
	Models           map[string]llm.ModelInfo
	CapabilityPolicy llm.CapabilityPolicy
	// ReportedCostField selects a service-reported usage cost: "cost" for a
	// charge or "estimated_cost" for an estimate. Empty ignores both.
	ReportedCostField string
	// CostCurrency is required with ReportedCostField.
	CostCurrency string
	// SystemRole is the role for Input.System. Dive uses developer by default.
	SystemRole llm.Role
}

// Client is safe for concurrent calls; its configured HTTP transport is
// shared.
type Client struct {
	client       providerhttp.Client
	systemRole   llm.Role
	costField    string
	costCurrency string
}

var _ llm.Client = (*Client)(nil)

// New snapshots destination identity and model evidence. Custom endpoints
// require a provider name so replay cannot confuse separate services.
func New(config Config) (*Client, error) {
	if config.Endpoint == "" {
		config.Endpoint = openAIEndpoint
	}
	public := config.Endpoint == openAIEndpoint && config.Client == nil
	if config.Provider == "" {
		if config.Endpoint != openAIEndpoint {
			return nil, errors.New("completions: custom endpoint requires provider identity")
		}
		config.Provider = "openai"
	}
	// The openai key is read only for the openai destination, so a
	// client for another provider never sends it elsewhere.
	if config.APIKey == "" && config.Provider == "openai" {
		config.APIKey = os.Getenv("OPENAI_API_KEY")
	}
	if config.SystemRole == "" {
		config.SystemRole = llm.RoleDeveloper
	}
	if config.SystemRole != llm.RoleDeveloper &&
		config.SystemRole != llm.RoleSystem {
		return nil, errors.New("completions: system role must be developer or system")
	}
	if config.ReportedCostField != "" &&
		config.ReportedCostField != "cost" &&
		config.ReportedCostField != "estimated_cost" ||
		(config.ReportedCostField == "") != (config.CostCurrency == "") {
		return nil, errors.New("completions: cost field must be cost or estimated_cost with a currency")
	}
	headers := config.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Authorization", "Bearer "+config.APIKey)
	client, err := providerhttp.NewClient(
		config.Provider,
		API,
		config.Endpoint,
		config.APIKey,
		config.Client,
		headers,
		config.Models,
		config.CapabilityPolicy,
		config.Timeouts,
	)
	if err != nil {
		return nil, err
	}
	client.Classify = providerhttp.OpenAIReason
	client.NoKey = providerhttp.NoAPIKey(
		config.Provider,
		config.APIKey,
		public,
		"OPENAI_API_KEY",
	)
	return &Client{
		client:       client,
		systemRole:   config.SystemRole,
		costField:    config.ReportedCostField,
		costCurrency: config.CostCurrency,
	}, nil
}

// Provider returns the configured destination identity.
func (p *Client) Provider() string {
	return p.client.Provider
}

// API returns the wire format name.
func (*Client) API() string {
	return API
}

// ModelInfo returns independently owned knowledge, if supplied.
func (p *Client) ModelInfo(model string) (llm.ModelInfo, bool) {
	return p.client.ModelInfo(model)
}

// Prepare validates and freezes exactly the body Send will dispatch. Under the
// default capability policy, settings beyond known limits are fitted to the
// nearest accepted value and recorded as warnings; unsupported content and
// controls without a safe substitute fail instead of being silently dropped.
func (p *Client) Prepare(
	ctx context.Context,
	r llm.Input,
) (llm.Request, error) {
	requested := r
	if err := ctx.Err(); err != nil {
		return llm.Request{}, err
	}
	if err := r.Validate(); err != nil {
		return llm.Request{}, providerhttp.Failure(
			p.Provider(),
			llm.ErrorInvalidRequest,
			"%w",
			err,
		)
	}
	wire := codec{origin: p.client.Origin(r.Model), provider: p.Provider()}
	if len(r.OutputSchema) > 0 {
		// Compatible services differ in json_schema support, so a custom
		// endpoint opts in only through endpoint-scoped model evidence.
		if p.client.Endpoint != openAIEndpoint {
			info, known := p.client.ModelInfo(r.Model)
			if !known || info.Capabilities.Features[llm.FeatureStructuredOutput] != llm.SupportSupported {
				return llm.Request{}, wire.unsupported(
					"output_schema on a custom Chat Completions endpoint requires endpoint-scoped ModelInfo support",
				)
			}
		}
		if len(r.Tools) > 0 || r.Settings.ToolChoice != nil ||
			r.Settings.ParallelTools != nil {
			return llm.Request{}, wire.unsupported(
				"output_schema cannot be combined with current-call tools, tool_choice, or parallel_tools",
			)
		}
		if err := outputschema.CheckOpenAIStrict(r.OutputSchema); err != nil {
			return llm.Request{}, wire.unsupported("%v", err)
		}
	}
	var changes []llm.ContentChange
	var err error
	r, changes, err = providercontent.Adapt(
		r,
		wire.origin,
		providercontent.AdaptOptions{
			ReplayPlainReasoning: wire.supportsPlainReasoning(),
		},
	)
	if err != nil {
		return llm.Request{}, err
	}
	r, adjustments, err := p.client.Fit(r, apiLimits, nil)
	if err != nil {
		return llm.Request{}, err
	}
	body := map[string]any{"model": r.Model}
	if len(r.OutputSchema) > 0 {
		body["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "dashi_output",
				"strict": true,
				"schema": r.OutputSchema,
			},
		}
	}
	if n := r.Settings.MaxTokens; n > 0 {
		body[maxTokensField(p.client.Endpoint, r.Model)] = n
	}
	if r.Settings.Temperature != nil {
		if *r.Settings.Temperature > 2 {
			return llm.Request{}, wire.unsupported("temperature exceeds Chat Completions range")
		}
		body["temperature"] = *r.Settings.Temperature
	}
	if s := r.Settings.Reasoning; s != nil {
		if s.BudgetTokens != 0 {
			return llm.Request{}, wire.unsupported("reasoning token budget; use a reasoning effort")
		}
		body["reasoning_effort"] = s.Effort
	}
	if r.Settings.ParallelTools != nil {
		body["parallel_tool_calls"] = *r.Settings.ParallelTools
	}
	if c := r.Settings.ToolChoice; c != nil {
		switch c.Mode {
		case llm.ToolChoiceAuto, llm.ToolChoiceNone:
			body["tool_choice"] = c.Mode
		case llm.ToolChoiceAny:
			body["tool_choice"] = "required"
		case llm.ToolChoiceTool:
			body["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": c.Name},
			}
		}
	}
	defs := make([]any, 0, len(r.Tools))
	for _, d := range r.Tools {
		if d.Kind == tool.KindProviderTool {
			return llm.Request{}, wire.unsupported("provider tool %q", d.Name)
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
				"type": "function",
				"function": map[string]any{
					"name":        d.Name,
					"description": d.Description,
					"parameters":  d.Schema,
				},
			},
		)
	}
	if len(defs) > 0 {
		body["tools"] = defs
	}
	if r.Stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	messages := make([]any, 0, len(r.Messages)+1)
	if r.System != "" {
		messages = append(
			messages,
			map[string]any{
				"role":    p.systemRole,
				"content": r.System,
			},
		)
	}
	for _, message := range r.Messages {
		encoded, err := wire.encodeMessage(message)
		if err != nil {
			return llm.Request{}, err
		}
		messages = append(messages, encoded...)
	}
	body["messages"] = messages
	return p.client.Prepare(requested, r, body, adjustments, changes)
}

// apiLimits states what Chat Completions accepts from any model. Every model
// takes an output limit. Effort names pass through because compatible
// services define their own.
var apiLimits = llm.Capabilities{
	Features: map[llm.Feature]llm.Support{
		llm.FeatureReasoningSummary: llm.SupportUnsupported,
		llm.FeatureCitations:        llm.SupportUnsupported,
		llm.FeatureTaskBudget:       llm.SupportUnsupported,
		llm.FeatureMaxTokens:        llm.SupportSupported,
		llm.FeatureReasoningBudget:  llm.SupportUnsupported,
		// Custom endpoints also need model evidence; see Prepare.
		llm.FeatureStructuredOutput: llm.SupportSupported,
		// No cache lifetime is sent; the service's own caching applies.
		llm.FeaturePromptCache: llm.SupportUnsupported,
	},
	MaxTemperature: 2,
}

// Send performs at most one dispatch. A stream sink observes provisional data;
// the returned Result is authoritative and may be partial on an error.
func (p *Client) Send(
	ctx context.Context,
	call llm.Call,
) (result llm.Result, callErr error) {
	prepared, sink := call.Request, call.Sink
	wire := codec{
		origin:       p.client.Origin(prepared.Input().Model),
		provider:     p.Provider(),
		costField:    p.costField,
		costCurrency: p.costCurrency,
		media:        providerhttp.HasMedia(prepared.Input()),
	}
	result = llm.Result{
		Requested: llm.ModelRef{
			Provider: prepared.Origin().Provider,
			Model:    prepared.Input().Model,
		},
		Message: llm.Message{Role: llm.RoleAssistant},
	}
	maybeSent := false
	defer func() {
		// Provenance belongs to provider output, including a partial message
		// returned with an error. An empty response is not a generated message.
		if len(result.Message.Content) > 0 && result.Message.Source == nil {
			result.Message.Source = &llm.MessageSource{
				Requested:  result.Requested,
				Served:     result.Served,
				API:        wire.origin.API,
				ResponseID: result.ID,
			}
		}
		// Warnings ride on every result, including partial ones, so a failed
		// call still explains how its request differed from the input.
		result.Warnings = prepared.Warnings()
		// A stop kind marks a call that may have run, so a call that was
		// definitely not sent carries none.
		if callErr != nil && maybeSent {
			result.Stop.Kind = llm.StopIncomplete
		}
	}()
	resp, err := p.client.Post(ctx, prepared)
	if err != nil {
		// Only a failure marked not sent rules out a provider effect; an
		// HTTP status or a connection dropped after the request was
		// written keeps a stop kind.
		maybeSent = providerhttp.MaybeSent(err)
		return result, err
	}
	maybeSent = true
	defer resp.Body.Close()
	if prepared.Input().Stream {
		result, callErr = wire.readStream(ctx, resp.Body, result, sink)
		if callErr == nil {
			callErr = providerhttp.Complete(p.Provider(), resp.Body)
		}
		if callErr == nil && len(prepared.Input().OutputSchema) > 0 {
			callErr = wire.validateOutput(&result, prepared.Input().OutputSchema)
		}
		return result, callErr
	}
	raw, err := providerhttp.ReadResponse(p.Provider(), resp.Body)
	if err != nil {
		return result, err
	}
	result, callErr = wire.decode(raw, result.Requested)
	if callErr == nil {
		callErr = providerhttp.Complete(p.Provider(), resp.Body)
	}
	if callErr == nil && len(prepared.Input().OutputSchema) > 0 {
		callErr = wire.validateOutput(&result, prepared.Input().OutputSchema)
	}
	return result, callErr
}

// validateOutput checks a finished reply against the requested schema and
// records it as Result.Output. It runs only after transport completion. A
// refusal or limit stop is returned as is, without Output.
func (wire codec) validateOutput(
	result *llm.Result,
	rawSchema json.RawMessage,
) error {
	if result.Stop.Kind != llm.StopFinished {
		return nil
	}
	var text strings.Builder
	found := false
	for _, part := range result.Message.Content {
		if part.ToolCall != nil ||
			(part.Opaque != nil && part.Opaque.Type == "tool_call") {
			return providerhttp.Failure(
				wire.provider,
				llm.ErrorProtocol,
				"output_schema: final response contains a tool call",
			)
		}
		if part.Block == nil || part.Block.Text == nil {
			continue
		}
		// Array-shaped content arrives as several text parts of one message.
		text.WriteString(part.Block.Text.Text)
		found = true
	}
	if !found {
		return providerhttp.Failure(
			wire.provider,
			llm.ErrorProtocol,
			"output_schema: final response contains no text object",
		)
	}
	raw := json.RawMessage(strings.TrimSpace(text.String()))
	schema, err := outputschema.Compile(rawSchema)
	if err == nil {
		err = outputschema.Validate(schema, raw)
	}
	if err != nil {
		return providerhttp.Failure(
			wire.provider,
			llm.ErrorProtocol,
			"output_schema: %w",
			err,
		)
	}
	result.Output = slices.Clone(raw)
	return nil
}

type codec struct {
	origin       content.Origin
	provider     string
	costField    string
	costCurrency string
	// media reports whether the request being sent carries media, so errors
	// reported in its stream can be classified as media rejections.
	media bool
}

func (wire codec) unsupported(format string, args ...any) error {
	return providerhttp.Failure(
		wire.provider,
		llm.ErrorUnsupported,
		format,
		args...,
	)
}

// maxTokensField names the wire output limit. OpenAI deprecated max_tokens in
// favor of max_completion_tokens, which its reasoning models require and its
// other chat models accept. Compatible services such as Mistral, OpenRouter,
// and DeepInfra document max_tokens, so other endpoints keep it unless the
// model is named like an OpenAI reasoning model, as behind a proxy. Matching
// "o" plus a digit keeps IDs such as "openai/gpt-4o" and "open-mistral-nemo"
// on max_tokens.
func maxTokensField(endpoint, model string) string {
	reasoning := strings.HasPrefix(model, "gpt-5") ||
		len(model) > 1 && model[0] == 'o' && '0' <= model[1] && model[1] <= '9'
	if endpoint == openAIEndpoint || reasoning {
		return "max_completion_tokens"
	}
	return "max_tokens"
}

// supportsPlainReasoning keeps adaptation aligned with the encodings below.
// This is wire support, independent of application-supplied model evidence.
func (wire codec) supportsPlainReasoning() bool {
	return wire.provider == "openrouter" || wire.provider == "mistral"
}

func (wire codec) encodeMessage(m llm.Message) ([]any, error) {
	var parts []ContentPart
	var calls []ToolCall
	var results []llm.ToolResult
	var reasoning, refusals strings.Builder
	var reasoningDetails json.RawMessage
	for _, c := range m.Content {
		if c.Replay != nil {
			return nil, wire.unsupported("Chat Completions private replay data")
		}
		switch c.Kind {
		case llm.ContentBlock:
			part, err := wire.encodeBlock(*c.Block)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case llm.ContentToolCall:
			if m.Role != llm.RoleAssistant {
				return nil, wire.unsupported("tool call in %s message", m.Role)
			}
			calls = append(
				calls,
				ToolCall{
					ID:   c.ToolCall.ID,
					Type: "function",
					Function: ToolCallFunction{
						Name:      c.ToolCall.Name,
						Arguments: string(c.ToolCall.Args),
					},
				},
			)
		case llm.ContentToolResult:
			if m.Role != llm.RoleUser {
				return nil, wire.unsupported("tool result in %s message", m.Role)
			}
			results = append(results, *c.ToolResult)
		case llm.ContentReasoning:
			if m.Role != llm.RoleAssistant || !wire.supportsPlainReasoning() {
				return nil, wire.unsupported("reasoning without supported Chat Completions replay")
			}
			if wire.provider == "mistral" {
				parts = append(
					parts,
					ContentPart{
						Type:     "thinking",
						Thinking: []ContentPart{{Type: "text", Text: c.Reasoning.Text}},
					},
				)
				continue
			}
			reasoning.WriteString(c.Reasoning.Text)
		case llm.ContentRefusal:
			if m.Role != llm.RoleAssistant {
				return nil, wire.unsupported("refusal in %s message", m.Role)
			}
			// OpenAI documents refusal on assistant messages. Compatible
			// services may reject fields they do not know, so they receive
			// the refusal as the assistant's text.
			if wire.provider != "openai" {
				parts = append(parts, ContentPart{Type: "text", Text: *c.Refusal})
				continue
			}
			if refusals.Len() > 0 {
				refusals.WriteString("\n\n")
			}
			refusals.WriteString(*c.Refusal)
		case llm.ContentOpaque:
			if c.Opaque.Origin != wire.origin ||
				c.Opaque.Unit != llm.OpaqueUnitItem ||
				c.Opaque.Type != "reasoning_details" {
				return nil, wire.unsupported("opaque Chat Completions content")
			}
			var v struct {
				Type    string          `json:"type"`
				Details json.RawMessage `json:"details"`
			}
			if err := json.Unmarshal(
				c.Opaque.Raw,
				&v,
			); err != nil ||
				v.Type != "reasoning_details" ||
				len(v.Details) == 0 ||
				reasoningDetails != nil {
				return nil, wire.unsupported("invalid reasoning details replay")
			}
			reasoningDetails = v.Details
		default:
			return nil, wire.unsupported("content kind %q", c.Kind)
		}
	}
	if len(results) > 0 &&
		(len(parts) > 0 ||
			len(calls) > 0 ||
			reasoning.Len() > 0 ||
			refusals.Len() > 0 ||
			reasoningDetails != nil) {
		return nil, wire.unsupported("mixed tool results and message content")
	}
	var encoded []any
	if len(parts) > 0 ||
		len(calls) > 0 ||
		reasoning.Len() > 0 ||
		refusals.Len() > 0 ||
		reasoningDetails != nil ||
		len(results) == 0 {
		message := map[string]any{"role": m.Role}
		if len(parts) > 0 {
			if allText(parts) {
				message["content"] = joinTextParts(parts)
			} else {
				message["content"] = parts
			}
		} else {
			message["content"] = ""
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
		if refusals.Len() > 0 {
			message["refusal"] = refusals.String()
		}
		if reasoning.Len() > 0 {
			message["reasoning"] = reasoning.String()
		}
		if reasoningDetails != nil {
			message["reasoning_details"] = reasoningDetails
		}
		encoded = append(encoded, message)
	}
	for _, tr := range results {
		var text []string
		// A tool message has no error flag, so failure is stated to the model
		// in text ahead of the output, as the Responses adapter does.
		if tr.IsError {
			text = append(text, toolFailedPrefix)
		}
		for _, b := range tr.Output.Content {
			if b.Kind != content.KindText || len(b.Text.Citations) != 0 {
				return nil, wire.unsupported("non-text or cited tool result")
			}
			text = append(text, b.Text.Text)
		}
		encoded = append(
			encoded,
			map[string]any{
				"role":         "tool",
				"tool_call_id": tr.CallID,
				"content":      strings.Join(text, "\n"),
			},
		)
	}
	return encoded, nil
}

// toolFailedPrefix leads the text of a tool result flagged as an error.
const toolFailedPrefix = "Tool failed:"

func (wire codec) encodeBlock(b content.Block) (ContentPart, error) {
	switch b.Kind {
	case content.KindText:
		if len(b.Text.Citations) != 0 {
			return ContentPart{}, wire.unsupported("text citations")
		}
		return ContentPart{Type: "text", Text: b.Text.Text}, nil
	case content.KindImage:
		if b.Image.Source.URI != "" {
			return ContentPart{}, wire.unsupported("provider-managed image URI")
		}
		if b.Image.Source.ID != "" {
			return ContentPart{}, wire.unsupported("image file ID")
		}
		url := b.Image.Source.URL
		if b.Image.Source.Data != nil {
			url = b.Image.Source.DataURL()
		}
		return ContentPart{
			Type:     "image_url",
			ImageURL: &ImageURLPart{URL: url, Detail: b.Image.Detail},
		}, nil
	case content.KindDocument:
		if b.Document.Source.URI != "" {
			return ContentPart{}, wire.unsupported("provider-managed document URI")
		}
		if b.Document.Source.URL != "" {
			return ContentPart{}, wire.unsupported("document URL")
		}
		file := &FilePart{Filename: b.Document.Title, FileID: b.Document.Source.ID}
		if b.Document.Source.Data != nil {
			file.FileData = b.Document.Source.DataURL()
			if file.Filename == "" {
				file.Filename = "document"
			}
		}
		return ContentPart{Type: "file", File: file}, nil
	default:
		return ContentPart{}, wire.unsupported("content kind %q", b.Kind)
	}
}

func allText(parts []ContentPart) bool {
	for _, part := range parts {
		if part.Type != "text" {
			return false
		}
	}
	return true
}

func joinTextParts(parts []ContentPart) string {
	var text []string
	for _, part := range parts {
		text = append(text, part.Text)
	}
	return strings.Join(text, "\n\n")
}

func (wire codec) decode(
	raw []byte,
	requested llm.ModelRef,
) (llm.Result, error) {
	result := llm.Result{
		Requested: requested,
		Message:   llm.Message{Role: llm.RoleAssistant},
		Raw:       slices.Clone(raw),
	}
	var response Response
	if err := json.Unmarshal(raw, &response); err != nil {
		return result, providerhttp.Failure(
			wire.provider,
			llm.ErrorProtocol,
			"decode response: %w",
			err,
		)
	}
	result.ID = response.ID
	result.Served = llm.ModelRef{Provider: wire.provider, Model: response.Model}
	if response.Model == "" {
		result.Served = llm.ModelRef{}
	}
	result.ServiceTier = response.ServiceTier
	if response.Usage.present {
		usage := response.Usage.toLLMUsage()
		result.Usage = &usage
		result.Cost = wire.reportedCost(response.Usage)
	}
	if len(response.Choices) == 0 {
		return result, providerhttp.Failure(
			wire.provider,
			llm.ErrorProtocol,
			"response has no choices",
		)
	}
	choice := response.Choices[0]
	if choice.Index != 0 {
		return result, providerhttp.Failure(
			wire.provider,
			llm.ErrorProtocol,
			"first choice has nonzero index",
		)
	}
	result.Stop.Raw = choice.FinishReason
	if result.Stop.Raw == "" {
		if len(choice.Message.ToolCalls) > 0 {
			result.Stop.Raw = "tool_calls"
		} else {
			result.Stop.Raw = "stop"
		}
	}
	result.Stop.Kind = llm.ClassifyStopReason(result.Stop.Raw)
	message := choice.Message
	if message.Reasoning != "" {
		result.Message.Content = append(
			result.Message.Content,
			llm.Content{
				Kind:      llm.ContentReasoning,
				Reasoning: &llm.Reasoning{Text: message.Reasoning},
			},
		)
	}
	if len(message.ReasoningDetails) > 0 {
		details, err := wire.reasoningDetails(message.ReasoningDetails)
		if err != nil {
			return result, err
		}
		result.Message.Content = append(result.Message.Content, details)
	}
	if len(message.ContentParts) > 0 {
		for _, part := range message.ContentParts {
			switch part.Type {
			case "text":
				result.Message.Content = append(
					result.Message.Content,
					llm.Text(part.Text),
				)
			case "thinking":
				result.Message.Content = append(
					result.Message.Content,
					llm.Content{
						Kind:      llm.ContentReasoning,
						Reasoning: &llm.Reasoning{Text: joinTextParts(part.Thinking)},
					},
				)
			default:
				return result, wire.unsupported("response content part %q", part.Type)
			}
		}
	} else if message.Content != "" {
		result.Message.Content = append(
			result.Message.Content,
			llm.Text(message.Content),
		)
	}
	if message.Refusal != "" {
		result.Message.Content = append(result.Message.Content, refusal(message.Refusal))
		result.Stop.Kind = refusalStop(result.Stop.Kind)
	}
	validatedCalls := make([]tool.Call, 0, len(message.ToolCalls))
	seenCallIDs := make(map[string]bool, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		if call.Type != "function" {
			return result, wire.unsupported("tool call type %q", call.Type)
		}
		v := tool.Call{
			ID:   call.ID,
			Name: call.Function.Name,
			Args: json.RawMessage(call.Function.Arguments),
		}
		if err := v.Validate(); err != nil {
			result.Message.Content = append(
				result.Message.Content,
				wire.partialToolCall(call),
			)
			return result, providerhttp.Failure(
				wire.provider,
				llm.ErrorProtocol,
				"invalid tool call: %w",
				err,
			)
		}
		if seenCallIDs[v.ID] {
			result.Message.Content = append(
				result.Message.Content,
				wire.partialToolCall(call),
			)
			return result, providerhttp.Failure(
				wire.provider,
				llm.ErrorProtocol,
				"duplicate tool call ID %q",
				v.ID,
			)
		}
		seenCallIDs[v.ID] = true
		validatedCalls = append(validatedCalls, v)
	}
	for _, v := range validatedCalls {
		result.Message.Content = append(
			result.Message.Content,
			llm.Content{
				Kind:     llm.ContentToolCall,
				ToolCall: &v,
			},
		)
	}
	return result, nil
}

// reasoningDetails keeps OpenRouter's structured reasoning verbatim as
// same-origin opaque content, which encodeMessage replays.
func (wire codec) reasoningDetails(details json.RawMessage) (llm.Content, error) {
	raw, err := json.Marshal(map[string]any{
		"type":    "reasoning_details",
		"details": details,
	})
	if err != nil {
		return llm.Content{}, err
	}
	return llm.Content{
		Kind: llm.ContentOpaque,
		Opaque: &llm.Opaque{
			Version: 1,
			Origin:  wire.origin,
			Type:    "reasoning_details",
			Unit:    llm.OpaqueUnitItem,
			Raw:     raw,
		},
	}, nil
}

func refusal(text string) llm.Content {
	return llm.Content{Kind: llm.ContentRefusal, Refusal: &text}
}

// refusalStop classifies a response that carries a refusal. OpenAI ends a
// refusal with finish_reason "stop", which alone does not distinguish it from
// a normal end, as the Responses adapter also finds. A limit or other reason
// is kept.
func refusalStop(kind llm.StopKind) llm.StopKind {
	if kind == llm.StopFinished {
		return llm.StopRefusal
	}
	return kind
}

func (wire codec) reportedCost(reported Usage) *llm.Cost {
	if wire.costField == "" {
		return nil
	}
	value := reported.Cost
	source := llm.CostSourceProviderReported
	if wire.costField == "estimated_cost" {
		value = reported.EstimatedCost
		source = llm.CostSourceProviderEstimate
	}
	if value == nil || *value < 0 {
		return nil
	}
	return &llm.Cost{
		Total:                *value,
		Currency:             wire.costCurrency,
		Source:               source,
		BreakdownUnavailable: true,
	}
}
