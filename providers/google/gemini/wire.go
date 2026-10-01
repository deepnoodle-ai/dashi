package gemini

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

type responseWire struct {
	ResponseID   string `json:"responseId"`
	ModelVersion string `json:"modelVersion"`
	Candidates   []struct {
		Content      *contentWire `json:"content"`
		FinishReason string       `json:"finishReason"`
		// The metadata fields stay raw so a stream can carry them into its
		// assembled response unchanged; metadataMeters decodes them.
		GroundingMetadata  json.RawMessage `json:"groundingMetadata"`
		URLContextMetadata json.RawMessage `json:"urlContextMetadata"`
	} `json:"candidates"`
	UsageMetadata *usageWire `json:"usageMetadata"`
}

type contentWire struct {
	Parts []json.RawMessage `json:"parts"`
}

type functionCallWire struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type partWire struct {
	Text             string            `json:"text"`
	Thought          bool              `json:"thought"`
	ThoughtSignature string            `json:"thoughtSignature"`
	FunctionCall     *functionCallWire `json:"functionCall"`
	InlineData       json.RawMessage   `json:"inlineData"`
	FileData         json.RawMessage   `json:"fileData"`
	ExecutableCode   *struct {
		ID       string `json:"id"`
		Language string `json:"language"`
		Code     string `json:"code"`
	} `json:"executableCode"`
	CodeExecutionResult *struct {
		ID      string `json:"id"`
		Outcome string `json:"outcome"`
		Output  string `json:"output"`
	} `json:"codeExecutionResult"`
}

type usageWire struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	ToolUsePromptTokenCount int `json:"toolUsePromptTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
}

func (usage usageWire) normalize() (*llm.Usage, error) {
	for _, count := range []int{
		usage.PromptTokenCount,
		usage.CandidatesTokenCount,
		usage.CachedContentTokenCount,
		usage.ThoughtsTokenCount,
		usage.ToolUsePromptTokenCount,
		usage.TotalTokenCount,
	} {
		if count < 0 {
			return nil, fmt.Errorf("gemini: negative usage count")
		}
	}
	if usage.CachedContentTokenCount > usage.PromptTokenCount {
		return nil, fmt.Errorf("gemini: cached tokens exceed prompt tokens")
	}
	// Gemini's prompt count includes cached tokens; provider-managed tool input
	// and thought output are separate counts. Follow Dive's disjoint buckets.
	out := &llm.Usage{
		InputTokens:                         usage.PromptTokenCount - usage.CachedContentTokenCount + usage.ToolUsePromptTokenCount,
		CacheReadInputTokens:                usage.CachedContentTokenCount,
		OutputTokens:                        usage.CandidatesTokenCount + usage.ThoughtsTokenCount,
		ToolUseInputTokens:                  usage.ToolUsePromptTokenCount,
		ReasoningTokens:                     usage.ThoughtsTokenCount,
		CacheCreationInputTokensUnavailable: true,
	}
	if usage.TotalTokenCount != 0 &&
		usage.TotalTokenCount != usage.PromptTokenCount+
			usage.CandidatesTokenCount+usage.ThoughtsTokenCount+usage.ToolUsePromptTokenCount {
		out.TotalTokenCountMismatch = true
	}
	return out, nil
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
	var response responseWire
	if err := json.Unmarshal(raw, &response); err != nil {
		return result, providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorProtocol,
			"decode response: %w",
			err,
		)
	}
	result.ID = response.ResponseID
	result.Served = llm.ModelRef{
		Provider: wire.origin.Provider,
		Model:    response.ModelVersion,
	}
	if response.UsageMetadata != nil {
		usage, err := response.UsageMetadata.normalize()
		if err != nil {
			return result, providerhttp.Failure(
				wire.origin.Provider,
				llm.ErrorProtocol,
				"%w",
				err,
			)
		}
		result.Usage = usage
	}
	if len(response.Candidates) == 0 {
		return result, providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorProtocol,
			"response has no candidates",
		)
	}
	candidate := response.Candidates[0]
	// Grounding metadata is complete only in a finished response; a stream
	// cut off before its finishReason may lack it.
	if result.Usage != nil {
		result.Usage.RequestsComplete = candidate.FinishReason != ""
	}
	result.Stop.Raw = strings.ToLower(candidate.FinishReason)
	if result.Stop.Raw == "" {
		result.Stop.Raw = "other"
	}
	result.Stop.Kind = llm.ClassifyStopReason(result.Stop.Raw)
	meters := metadataMeters(
		candidate.GroundingMetadata,
		candidate.URLContextMetadata,
	)
	if candidate.Content == nil {
		recordMeters(result.Usage, meters)
		return result, nil
	}
	seed := response.ResponseID
	if seed == "" {
		hash := sha256.Sum256(raw)
		seed = fmt.Sprintf("%x", hash[:12])
	}
	var codeID string
	for index, rawPart := range candidate.Content.Parts {
		part, err := wire.decodePartWithCodeID(
			rawPart,
			seed,
			index,
			codeID,
		)
		if err != nil {
			return result, err
		}
		result.Message.Content = append(result.Message.Content, part)
		if part.ProviderToolUse != nil {
			codeID = part.ProviderToolUse.ID
			meters[llm.MeterCodeExecution]++
		}
	}
	recordMeters(result.Usage, meters)
	result.Message.Source = &llm.MessageSource{
		Requested:  requested,
		Served:     result.Served,
		API:        wire.origin.API,
		ResponseID: result.ID,
	}
	return result, nil
}

func (wire codec) decodePart(
	raw json.RawMessage,
	responseID string,
	index int,
) (llm.Content, error) {
	return wire.decodePartWithCodeID(raw, responseID, index, "")
}

func (wire codec) decodePartWithCodeID(
	raw json.RawMessage,
	responseID string,
	index int,
	codeID string,
) (llm.Content, error) {
	var part partWire
	if err := providerhttp.Decode(raw, &part); err != nil {
		return llm.Content{}, providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorProtocol,
			"decode part: %w",
			err,
		)
	}
	kind := "unknown"
	switch {
	case part.ExecutableCode != nil:
		kind = "code_execution"
	case part.CodeExecutionResult != nil:
		kind = "code_execution_result"
	case part.FunctionCall != nil:
		kind = "function_call"
	case part.Thought:
		kind = "thinking"
	case part.Text != "" || part.ThoughtSignature != "":
		kind = "text"
	}
	replay := &llm.Opaque{
		Version: 1,
		Origin:  wire.origin,
		Type:    kind,
		Unit:    llm.OpaqueUnitContentBlock,
		Raw:     slices.Clone(raw),
	}
	switch kind {
	case "code_execution":
		id := part.ExecutableCode.ID
		if id == "" {
			id = callID(responseID, "code_execution", index)
		}
		input, _ := json.Marshal(part.ExecutableCode)
		use := llm.ProviderToolUse{
			ID:     id,
			Name:   "code_execution",
			Type:   "codeExecution",
			Origin: wire.origin,
			Input:  input,
		}
		return llm.Content{
			Kind:            llm.ContentProviderToolUse,
			ProviderToolUse: &use,
			Replay:          replay,
		}, nil
	case "code_execution_result":
		id := part.CodeExecutionResult.ID
		if id == "" {
			id = codeID
		}
		if id == "" {
			return llm.Content{Kind: llm.ContentOpaque, Opaque: replay}, nil
		}
		output := []content.Block{}
		if part.CodeExecutionResult.Output != "" {
			output = append(
				output,
				content.TextBlock(
					part.CodeExecutionResult.Output,
				),
			)
		}
		result := llm.ProviderToolResult{
			CallID:  id,
			Type:    "codeExecutionResult",
			Origin:  wire.origin,
			Output:  output,
			IsError: part.CodeExecutionResult.Outcome != "OUTCOME_OK",
		}
		return llm.Content{
			Kind:               llm.ContentProviderToolResult,
			ProviderToolResult: &result,
			Replay:             replay,
		}, nil
	case "text":
		b := content.TextBlock(part.Text)
		return llm.Content{Kind: llm.ContentBlock, Block: &b, Replay: replay}, nil
	case "thinking":
		return llm.Content{
			Kind:      llm.ContentReasoning,
			Reasoning: &llm.Reasoning{Text: part.Text},
			Replay:    replay,
		}, nil
	case "function_call":
		if part.FunctionCall.Name == "" {
			return llm.Content{}, providerhttp.Failure(
				wire.origin.Provider,
				llm.ErrorProtocol,
				"function call requires name",
			)
		}
		args, err := json.Marshal(part.FunctionCall.Args)
		if err != nil {
			return llm.Content{}, err
		}
		id := part.FunctionCall.ID
		if id == "" {
			id = callID(responseID, part.FunctionCall.Name, index)
		}
		call := tool.Call{ID: id, Name: part.FunctionCall.Name, Args: args}
		return llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &call,
			Replay:   replay,
		}, nil
	default:
		return llm.Content{Kind: llm.ContentOpaque, Opaque: replay}, nil
	}
}

func callID(responseID, name string, index int) string {
	seed := fmt.Sprintf("%s:%s:%d", responseID, name, index)
	hash := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("call_%x", hash[:12])
}
