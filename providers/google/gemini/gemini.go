// Package gemini implements Google's GenerateContent API with the Google GenAI
// SDK. Conversion is adapted from Dive's Google provider (65c393f),
// Apache-2.0.
//
// # Hosted tools
//
// Hosted tool constructors return a definition-only tool.Tool: Google runs it
// inside the model call, so it has no Executor and no hook sees its calls.
// Add it to a Runner with tool.List, or pass its Definition in llm.Input.Tools. A
// constructor returns an error only when its options can be invalid.
//
// Each options struct has a Provider field that binds the tool to one
// configured destination. Empty means "google". A tool without options
// (Google Search, URL context, code execution) bound to "google" is also
// accepted by the "google-vertex" destination, so the zero value also works
// with a Vertex client that uses the default provider name. Set Provider to
// the client's Provider() for a destination with a custom name.
package gemini

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/providercontent"
	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// API identifies GenerateContent independently of a Google API-key
// destination.
const API = "google.generate_content"

// DefaultEndpoint uses v1beta for client function calling. Streaming selects
// the paired streamGenerateContent path while preserving this replay scope.
const DefaultEndpoint = "https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent"

// Config describes one Gemini API or Vertex destination. Model evidence must
// name its resolved model-specific endpoint, not the endpoint template.
type Config struct {
	Provider string
	// APIKey defaults to GEMINI_API_KEY, then GOOGLE_API_KEY, read only for the
	// "google" provider so the key never reaches another destination, and never
	// for Vertex. With no key for the public endpoint, New and Prepare still
	// succeed and Send fails before dispatch with an error wrapping
	// llm.ErrNoAPIKey.
	APIKey string
	// Vertex selects Google's Vertex AI backend. Project and Location are
	// resolved at construction; CredentialScope identifies the credential
	// principal for private replay without storing the credential itself.
	Vertex           bool
	Project          string
	Location         string
	CredentialScope  string
	Endpoint         string
	Client           *http.Client
	Models           map[string]llm.ModelInfo
	CapabilityPolicy llm.CapabilityPolicy
}

// Client owns model evidence and configuration, not per-call state.
type Client struct {
	config Config
	models map[string]llm.ModelInfo
	noKey  error
}

var _ llm.Client = (*Client)(nil)

// New validates the endpoint template and each supplied model's exact scope.
func New(config Config) (*Client, error) {
	defaultEndpoint := DefaultEndpoint
	defaultProvider := "google"
	if config.Vertex {
		if config.Project == "" {
			config.Project = os.Getenv("GOOGLE_CLOUD_PROJECT")
		}
		if config.Location == "" {
			config.Location = os.Getenv("GOOGLE_CLOUD_LOCATION")
		}
		if config.Location == "" {
			config.Location = os.Getenv("GOOGLE_CLOUD_REGION")
		}
		if config.Location == "" {
			config.Location = "global"
		}
		if config.Project == "" ||
			config.CredentialScope == "" ||
			config.APIKey != "" {
			return nil, errors.New("gemini: Vertex requires project and credential scope, without API key")
		}
		host := "aiplatform.googleapis.com"
		if config.Location != "global" {
			host = config.Location + "-aiplatform.googleapis.com"
		}
		defaultEndpoint = fmt.Sprintf(
			"https://%s/v1/projects/%s/locations/%s/publishers/google/models/{model}:generateContent",
			host,
			url.PathEscape(config.Project),
			url.PathEscape(config.Location),
		)
		defaultProvider = "google-vertex"
	}
	if config.Endpoint == "" {
		config.Endpoint = defaultEndpoint
	}
	// The Gemini key is read only for the "google" destination, so a client
	// for another provider never sends it elsewhere.
	if !config.Vertex && config.APIKey == "" &&
		cmp.Or(config.Provider, defaultProvider) == "google" {
		config.APIKey = cmp.Or(
			os.Getenv("GEMINI_API_KEY"),
			os.Getenv("GOOGLE_API_KEY"),
		)
	}
	if config.Provider == "" {
		if config.Endpoint != defaultEndpoint {
			return nil, errors.New("gemini: custom endpoint requires provider identity")
		}
		config.Provider = defaultProvider
	}
	if !strings.Contains(config.Endpoint, "{model}:generateContent") {
		return nil, errors.New("gemini: endpoint must contain {model}:generateContent")
	}
	if _, _, err := sdkEndpoint(config.Endpoint, config.Vertex); err != nil {
		return nil, err
	}
	if config.Vertex {
		u, _ := url.Parse(config.Endpoint)
		scopePath := fmt.Sprintf(
			"/projects/%s/locations/%s/publishers/google/models/{model}:generateContent",
			url.PathEscape(config.Project),
			url.PathEscape(config.Location),
		)
		if !strings.HasSuffix(u.Path, scopePath) {
			return nil, errors.New("gemini: Vertex endpoint does not match project and location")
		}
	}
	if !config.CapabilityPolicy.Valid() {
		return nil, errors.New("gemini: invalid capability policy")
	}
	p := &Client{
		config: config,
		models: make(map[string]llm.ModelInfo, len(config.Models)),
		noKey: providerhttp.NoAPIKey(
			config.Provider,
			config.APIKey,
			!config.Vertex && config.Endpoint == DefaultEndpoint &&
				config.Client == nil,
			"GEMINI_API_KEY or GOOGLE_API_KEY",
		),
	}
	for model, info := range config.Models {
		if model == "" {
			return nil, errors.New("gemini: model name is required")
		}
		p.models[model] = info.Clone()
		if _, err := p.httpClient(model); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Provider returns the configured destination identity.
func (p *Client) Provider() string {
	return p.config.Provider
}

// API returns the GenerateContent wire format name.
func (*Client) API() string {
	return API
}

// ModelInfo returns independently owned model knowledge. Configured
// knowledge wins; otherwise the catalog supplies documented facts when the
// resolved endpoint is the public Gemini API, never for Vertex or a custom
// endpoint.
func (p *Client) ModelInfo(model string) (llm.ModelInfo, bool) {
	if info, ok := p.models[model]; ok {
		return info.Clone(), true
	}
	return catalog.Lookup(API, p.endpoint(model), model)
}

func (p *Client) endpoint(model string) string {
	return strings.ReplaceAll(p.config.Endpoint, "{model}", url.PathEscape(model))
}

func (p *Client) httpClient(model string) (providerhttp.Client, error) {
	models := map[string]llm.ModelInfo{}
	if info, ok := p.models[model]; ok {
		models[model] = info
	}
	credential := p.config.APIKey
	headers := make(http.Header)
	if p.config.Vertex {
		credential = p.config.CredentialScope
	} else {
		headers.Set("X-Goog-Api-Key", credential)
	}
	return providerhttp.NewClient(
		p.Provider(),
		API,
		p.endpoint(model),
		credential,
		p.config.Client,
		headers,
		models,
		p.config.CapabilityPolicy,
		llm.HTTPTimeouts{},
	)
}

// Prepare validates conversion, then captures the SDK's serialized body with
// an in-memory transport. Under the default capability policy, settings
// beyond known limits are fitted and recorded as warnings; unsupported content
// fails before real dispatch.
func (p *Client) Prepare(
	ctx context.Context,
	input llm.Input,
) (llm.Request, error) {
	if err := ctx.Err(); err != nil {
		return llm.Request{}, err
	}
	if err := input.Validate(); err != nil {
		return llm.Request{}, providerhttp.Failure(
			p.Provider(),
			llm.ErrorInvalidRequest,
			"%w",
			err,
		)
	}
	if len(input.OutputSchema) > 0 {
		return llm.Request{}, providerhttp.Failure(
			p.Provider(),
			llm.ErrorUnsupported,
			"output_schema is not supported by this route",
		)
	}
	if !p.config.Vertex {
		_, version, _ := sdkEndpoint(p.config.Endpoint, false)
		if version == "v1" && hasClientFunctionCalls(input) {
			return llm.Request{}, providerhttp.Failure(
				p.Provider(),
				llm.ErrorUnsupported,
				"Gemini API v1 does not support client function calling; use a v1beta endpoint",
			)
		}
	}
	httpClient, err := p.httpClient(input.Model)
	if err != nil {
		return llm.Request{}, err
	}
	wire := codec{origin: httpClient.Origin(input.Model)}
	requested := input
	input, changes, err := providercontent.Adapt(input, wire.origin)
	if err != nil {
		return llm.Request{}, err
	}
	input, adjustments, err := httpClient.Fit(
		input,
		limits(input, httpClient),
		nil,
	)
	if err != nil {
		return llm.Request{}, err
	}
	contents, err := wire.encodeMessages(input.Messages)
	if err != nil {
		return llm.Request{}, err
	}
	body := map[string]any{"contents": contents}
	if input.System != "" {
		body["systemInstruction"] = map[string]any{
			"parts": []any{map[string]any{"text": input.System}},
		}
	}
	config := map[string]any{}
	if n := input.Settings.MaxTokens; n > 0 {
		config["maxOutputTokens"] = n
	}
	if t := input.Settings.Temperature; t != nil {
		config["temperature"] = *t
	}
	if reasoning := input.Settings.Reasoning; reasoning != nil {
		if reasoning.Effort != "" {
			return llm.Request{}, wire.unsupported(
				"reasoning effort requires model-specific Gemini level evidence; " +
					"use a reasoning budget",
			)
		}
		config["thinkingConfig"] = map[string]any{
			"thinkingBudget":  reasoning.BudgetTokens,
			"includeThoughts": true,
		}
	}
	if len(config) > 0 {
		body["generationConfig"] = config
	}
	if input.Settings.ParallelTools != nil {
		return llm.Request{}, wire.unsupported("parallel tool setting; remove ParallelTools")
	}
	if len(input.Tools) > 0 {
		declarations := make([]any, 0, len(input.Tools))
		configs := make([]any, 0, len(input.Tools))
		for _, d := range input.Tools {
			if d.Kind == tool.KindProviderHosted {
				if d.Hosted.Type == "fileSearch" &&
					!strings.Contains(p.config.Endpoint, "/v1beta/") {
					return llm.Request{}, wire.unsupported(
						"file search requires a v1beta endpoint",
					)
				}
				config, err := wire.hostedTool(d)
				if err != nil {
					return llm.Request{}, err
				}
				configs = append(configs, config)
				continue
			}
			if raw := strings.TrimSpace(string(d.Schema)); !strings.HasPrefix(
				raw,
				"{",
			) {
				return llm.Request{}, wire.unsupported(
					"tool %q requires an object schema",
					d.Name,
				)
			}
			declarations = append(
				declarations,
				map[string]any{
					"name":                 d.Name,
					"description":          d.Description,
					"parametersJsonSchema": d.Schema,
				},
			)
		}
		if len(declarations) > 0 {
			configs = append(
				configs,
				map[string]any{
					"functionDeclarations": declarations,
				},
			)
		}
		body["tools"] = configs
	}
	if choice := input.Settings.ToolChoice; choice != nil {
		if choice.Mode == llm.ToolChoiceTool ||
			choice.Mode == llm.ToolChoiceAny {
			clientTools := false
			for _, definition := range input.Tools {
				if definition.Kind == tool.KindProviderHosted {
					if choice.Mode == llm.ToolChoiceTool &&
						definition.Name == choice.Name {
						return llm.Request{}, wire.unsupported(
							"named choice of hosted tool %q",
							choice.Name,
						)
					}
					continue
				}
				clientTools = true
			}
			if choice.Mode == llm.ToolChoiceAny && !clientTools {
				return llm.Request{}, wire.unsupported(
					"forced function calling requires a client tool",
				)
			}
		}
		calling := map[string]any{}
		switch choice.Mode {
		case llm.ToolChoiceAuto:
			calling["mode"] = "AUTO"
		case llm.ToolChoiceNone:
			calling["mode"] = "NONE"
		case llm.ToolChoiceAny:
			calling["mode"] = "ANY"
		case llm.ToolChoiceTool:
			calling["mode"] = "ANY"
			calling["allowedFunctionNames"] = []string{choice.Name}
		}
		body["toolConfig"] = map[string]any{"functionCallingConfig": calling}
	}
	info, known := p.ModelInfo(input.Model)
	unverified, err := info.Capabilities.Check(input, p.config.CapabilityPolicy)
	if err != nil {
		return llm.Request{}, providerhttp.Failure(
			p.Provider(),
			llm.ErrorUnsupported,
			"%w",
			err,
		)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return llm.Request{}, err
	}
	raw, err = p.sdkBody(ctx, input.Model, input.Stream, raw)
	if err != nil {
		return llm.Request{}, err
	}
	var evidence *llm.ModelInfo
	if known {
		evidence = &info
	}
	return llm.NewRequest(llm.RequestSnapshot{
		Origin:           wire.origin,
		Input:            requested,
		Effective:        input.Settings,
		Body:             raw,
		Adjustments:      adjustments,
		ContentChanges:   changes,
		ModelInfo:        evidence,
		CapabilityPolicy: p.config.CapabilityPolicy,
		Unverified:       unverified,
	})
}

// apiLimits states what this Gemini encoding accepts from any model. It has
// no parallel tool control, since Gemini decides that itself, and it maps
// reasoning only from token budgets. The API documents maxOutputTokens for
// every model and temperature as 0 to 2
// (https://ai.google.dev/api/generate-content).
var apiLimits = llm.Capabilities{
	Features: map[llm.Feature]llm.Support{
		llm.FeatureCitations:       llm.SupportUnsupported,
		llm.FeatureMaxTokens:       llm.SupportSupported,
		llm.FeatureParallelTools:   llm.SupportUnsupported,
		llm.FeatureReasoningEffort: llm.SupportUnsupported,
		// Implicit caching has no request field and cannot be turned off.
		llm.FeaturePromptCache: llm.SupportUnsupported,
	},
	MaxTemperature: 2,
}

// limits adds the per-request thinking cap to apiLimits. Gemini counts
// thinking tokens against maxOutputTokens, so a budget at or above the
// effective output limit could leave no room for an answer; the budget is
// kept below it, as Messages requires.
func limits(
	input llm.Input,
	client providerhttp.Client,
) llm.Capabilities {
	maxTokens := input.Settings.MaxTokens
	if maxTokens == 0 {
		maxTokens = llm.DefaultMaxTokens
	}
	info, _ := client.ModelInfo(input.Model)
	if limit := info.Capabilities.MaxOutputTokens; limit > 0 {
		maxTokens = min(maxTokens, limit)
	}
	api := apiLimits
	api.MaxReasoningBudget = max(maxTokens-1, 1)
	return api
}

func hasClientFunctionCalls(input llm.Input) bool {
	for _, definition := range input.Tools {
		if definition.Kind != tool.KindProviderHosted {
			return true
		}
	}
	for _, message := range input.Messages {
		for _, part := range message.Content {
			if part.Kind == llm.ContentToolCall || part.Kind == llm.ContentToolResult {
				return true
			}
		}
	}
	return false
}

// Send uses the Google SDK and guards its final HTTP request against the
// frozen
// body. A sink observes provisional stream deltas; the final Result is
// authoritative.
func (p *Client) Send(
	ctx context.Context,
	call llm.Call,
) (result llm.Result, callErr error) {
	if p.noKey != nil {
		return llm.Result{}, p.noKey
	}
	prepared, sink := call.Request, call.Sink
	model := prepared.Input().Model
	httpClient, err := p.httpClient(model)
	if err != nil {
		return llm.Result{}, providerhttp.NotDispatched(
			p.Provider(),
			llm.ErrorInvalidRequest,
			"%w",
			err,
		)
	}
	wire := codec{
		origin: httpClient.Origin(model),
		media:  providerhttp.HasMedia(prepared.Input()),
	}
	result = llm.Result{
		Requested: llm.ModelRef{Provider: p.Provider(), Model: model},
		Message:   llm.Message{Role: llm.RoleAssistant},
	}
	defer func() {
		// Warnings ride on every result, including partial ones, so a failed
		// call still explains how its request differed from the input.
		result.Warnings = prepared.Warnings()
		if callErr != nil {
			result.Stop.Kind = llm.StopIncomplete
		}
	}()
	if prepared.Origin() != wire.origin {
		return result, providerhttp.NotDispatched(
			p.Provider(),
			llm.ErrorInvalidRequest,
			"prepared request belongs to another destination",
		)
	}
	client, guard, err := p.sdkClient(
		ctx,
		model,
		prepared.Input().Stream,
		prepared.Body(),
	)
	if err != nil {
		return result, providerhttp.NotDispatched(
			p.Provider(),
			llm.ErrorInvalidRequest,
			"%w",
			err,
		)
	}
	contents, config, err := sdkArguments(prepared.Body())
	if err != nil {
		return result, providerhttp.NotDispatched(
			p.Provider(),
			llm.ErrorInvalidRequest,
			"%w",
			err,
		)
	}
	if prepared.Input().Stream {
		var streamErr error
		live := sdkStream{sink: sink}
		for chunk, err := range client.Models.GenerateContentStream(
			ctx,
			sdkModel(model),
			contents,
			config,
		) {
			if err != nil {
				streamErr = sdkError(p.Provider(), err, guard.responseBytes(), wire.media)
				break
			}
			live.emit(chunk)
		}
		partial, parseErr := wire.readStream(
			ctx,
			io.NopCloser(bytes.NewReader(guard.responseBytes())),
			result,
			nil,
		)
		if streamErr != nil {
			return partial, streamErr
		}
		return partial, parseErr
	}
	_, err = client.Models.GenerateContent(ctx, sdkModel(model), contents, config)
	if err != nil {
		return result, sdkError(p.Provider(), err, guard.responseBytes(), wire.media)
	}
	return wire.decode(guard.responseBytes(), result.Requested)
}

type codec struct {
	origin content.Origin
	// media reports whether the request being sent carries media, so errors
	// reported in its response body can be classified as media rejections.
	media bool
}

func (wire codec) unsupported(format string, args ...any) error {
	return providerhttp.Failure(
		wire.origin.Provider,
		llm.ErrorUnsupported,
		format,
		args...,
	)
}

func (wire codec) encodeMessages(messages []llm.Message) ([]any, error) {
	if len(messages) == 0 {
		return nil, wire.unsupported("at least one message is required")
	}
	var contents []any
	callNames := map[string]functionCallRef{}
	for _, message := range messages {
		if message.Role != llm.RoleUser && message.Role != llm.RoleAssistant {
			return nil, wire.unsupported("system or developer history; use Input.System")
		}
		role := "user"
		if message.Role == llm.RoleAssistant {
			role = "model"
		}
		parts := make([]any, 0, len(message.Content))
		for _, c := range message.Content {
			part, err := wire.encodeContent(c, message.Role, callNames)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}
	if len(callNames) != 0 {
		return nil, wire.unsupported("history has unanswered function calls")
	}
	return contents, nil
}

type functionCallRef struct {
	name          string
	providerHasID bool
}

func (wire codec) encodeContent(
	c llm.Content,
	role llm.Role,
	names map[string]functionCallRef,
) (any, error) {
	if c.Opaque != nil {
		if role != llm.RoleAssistant ||
			c.Opaque.Origin != wire.origin ||
			c.Opaque.Unit != llm.OpaqueUnitContentBlock {
			return nil, wire.unsupported("opaque part from another destination")
		}
		return c.Opaque.Raw, nil
	}
	if c.Replay != nil {
		if role != llm.RoleAssistant ||
			c.Replay.Origin != wire.origin ||
			c.Replay.Unit != llm.OpaqueUnitContentBlock {
			return nil, wire.unsupported("replay part from another destination")
		}
		var saved partWire
		if err := providerhttp.Decode(c.Replay.Raw, &saved); err != nil {
			return nil, wire.unsupported("invalid replay part")
		}
		switch {
		case c.HostedUse != nil && saved.ExecutableCode != nil:
			return c.Replay.Raw, nil
		case c.HostedResult != nil && saved.CodeExecutionResult != nil:
			return c.Replay.Raw, nil
		case c.Block != nil &&
			c.Block.Text != nil &&
			saved.Text == c.Block.Text.Text:
			return c.Replay.Raw, nil
		case c.Reasoning != nil && saved.Thought && saved.Text == c.Reasoning.Text:
			return c.Replay.Raw, nil
		case c.ToolCall != nil &&
			saved.FunctionCall != nil &&
			saved.FunctionCall.Name == c.ToolCall.Name:
			if sameGeminiReplayArgs(c.Replay.Raw, c.ToolCall.Args) {
				if saved.FunctionCall.ID == "" {
					fields, ok := unsignedFunctionCallReplay(c.Replay.Raw, false)
					if !ok {
						// Preserve signed ID-less calls exactly. Their generated
						// canonical IDs pair Dashi's call and result, but do not
						// authorize modifying the signed provider part.
						names[c.ToolCall.ID] = functionCallRef{name: c.ToolCall.Name}
						return c.Replay.Raw, nil
					}
					call := fields["functionCall"].(map[string]any)
					call["id"] = c.ToolCall.ID
					names[c.ToolCall.ID] = functionCallRef{
						name:          c.ToolCall.Name,
						providerHasID: true,
					}
					return fields, nil
				}
				if saved.FunctionCall.ID == c.ToolCall.ID {
					names[c.ToolCall.ID] = functionCallRef{
						name:          c.ToolCall.Name,
						providerHasID: true,
					}
					return c.Replay.Raw, nil
				}
				if fields, ok := unsignedFunctionCallReplay(c.Replay.Raw, true); ok {
					call := fields["functionCall"].(map[string]any)
					call["id"] = c.ToolCall.ID
					names[c.ToolCall.ID] = functionCallRef{
						name:          c.ToolCall.Name,
						providerHasID: true,
					}
					return fields, nil
				}
			}
		}
		return nil, wire.unsupported("edited signed or private Gemini part; remove replay data to edit")
	}
	switch c.Kind {
	case llm.ContentBlock:
		switch b := c.Block; b.Kind {
		case content.KindText:
			if len(b.Text.Citations) != 0 {
				return nil, wire.unsupported("text citations")
			}
			return map[string]any{"text": b.Text.Text}, nil
		case content.KindImage, content.KindDocument, content.KindAudio:
			var source content.Source
			switch b.Kind {
			case content.KindImage:
				source = b.Image.Source
				if b.Image.Detail != "" {
					return nil, wire.unsupported("image detail")
				}
			case content.KindDocument:
				source = b.Document.Source
			case content.KindAudio:
				source = b.Audio.Source
			}
			switch {
			case source.Data != nil:
				return map[string]any{
					"inlineData": map[string]any{
						"mimeType": source.MIMEType,
						"data":     base64.StdEncoding.EncodeToString(source.Data),
					},
				}, nil
			case source.URI != "" || source.URL != "":
				if source.MIMEType == "" {
					return nil, wire.unsupported("Gemini file URI requires MIME type")
				}
				uri := source.URI
				if uri == "" {
					uri = source.URL
				}
				return map[string]any{
					"fileData": map[string]any{
						"mimeType": source.MIMEType,
						"fileUri":  uri,
					},
				}, nil
			default:
				return nil, wire.unsupported("Gemini file ID requires its URI")
			}
		}
	case llm.ContentReasoning:
		return nil, wire.unsupported("reasoning requires its signed Gemini part")
	case llm.ContentToolCall:
		if role != llm.RoleAssistant {
			return nil, wire.unsupported("tool call outside model message")
		}
		var args map[string]any
		if err := json.Unmarshal(c.ToolCall.Args, &args); err != nil || args == nil {
			return nil, wire.unsupported("tool arguments require a JSON object")
		}
		names[c.ToolCall.ID] = functionCallRef{
			name:          c.ToolCall.Name,
			providerHasID: true,
		}
		return map[string]any{
			"functionCall": map[string]any{
				"id":   c.ToolCall.ID,
				"name": c.ToolCall.Name,
				"args": args,
			},
		}, nil
	case llm.ContentToolResult:
		ref, ok := names[c.ToolResult.CallID]
		if role != llm.RoleUser || !ok {
			return nil, wire.unsupported("tool result requires an earlier model call")
		}
		var output []string
		for _, b := range c.ToolResult.Output.Content {
			if b.Kind != content.KindText || len(b.Text.Citations) != 0 {
				return nil, wire.unsupported("non-text tool result")
			}
			output = append(output, b.Text.Text)
		}
		key := "output"
		if c.ToolResult.IsError {
			key = "error"
		}
		delete(names, c.ToolResult.CallID)
		response := map[string]any{
			"name":     ref.name,
			"response": map[string]any{key: strings.Join(output, "\n\n")},
		}
		if ref.providerHasID {
			response["id"] = c.ToolResult.CallID
		}
		return map[string]any{"functionResponse": response}, nil
	}
	return nil, wire.unsupported("content kind %q", c.Kind)
}

// Only an unsigned, fully understood function call can carry an edited ID.
// Signatures and unfamiliar fields may depend on its original value.
func unsignedFunctionCallReplay(
	raw json.RawMessage,
	requireID bool,
) (map[string]any, bool) {
	var fields map[string]any
	if providerhttp.Decode(raw, &fields) != nil || len(fields) != 1 {
		return nil, false
	}
	call, ok := fields["functionCall"].(map[string]any)
	if !ok || len(call) < 2 || len(call) > 3 {
		return nil, false
	}
	if id, exists := call["id"]; exists {
		if _, ok := id.(string); !ok {
			return nil, false
		}
	} else if requireID || len(call) != 2 {
		return nil, false
	}
	if _, ok := call["name"].(string); !ok {
		return nil, false
	}
	if _, ok := call["args"].(map[string]any); !ok {
		return nil, false
	}
	return fields, true
}

func sameGeminiReplayArgs(raw, args json.RawMessage) bool {
	var outer map[string]json.RawMessage
	var call map[string]json.RawMessage
	var original, current any
	return json.Unmarshal(raw, &outer) == nil &&
		json.Unmarshal(outer["functionCall"], &call) == nil &&
		providerhttp.Decode(call["args"], &original) == nil &&
		providerhttp.Decode(args, &current) == nil &&
		reflect.DeepEqual(original, current)
}
