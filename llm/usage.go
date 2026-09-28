// Ported from Dive (65c393f), Apache-2.0; adapted for Kernel.

package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
)

// ModalityTokenUsage is a disjoint token breakdown for one provider modality.
// Its input buckets follow the same convention as Usage; OutputTokens includes
// any reasoning tokens when the provider attributes them to the modality.
type ModalityTokenUsage struct {
	// InputTokens counts input excluding cache reads and cache creation.
	InputTokens int `json:"input_tokens,omitempty"`

	// OutputTokens counts generated output, including any separately reported
	// reasoning.
	OutputTokens int `json:"output_tokens,omitempty"`

	// CacheCreationInputTokens counts input written into the prompt cache.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`

	// CacheReadInputTokens counts input read from the prompt cache.
	CacheReadInputTokens int `json:"cache_read_input_tokens,omitempty"`
}

// Usage contains token usage information for an LLM response.
//
// The input-side token buckets are mutually disjoint. InputTokens counts
// uncached input; CacheCreationInputTokens counts cache writes; and
// CacheReadInputTokens counts cache hits. When a provider does not
// expose cache writes, InputTokens contains all uncached input and
// CacheCreationInputTokensUnavailable is true. The full input size is their
// sum, exposed by TotalInputTokens.
// This differs deliberately from ReasoningTokens, which is a subset of
// OutputTokens rather than an additive bucket.
type Usage struct {
	// Requests counts billable provider-hosted operations by price-sheet key.
	Requests map[string]int `json:"requests,omitempty"`
	// InputTokens counts input excluding cache reads and cache creation.
	InputTokens int `json:"input_tokens"`

	// OutputTokens counts generated output, including any separately reported
	// reasoning.
	OutputTokens int `json:"output_tokens"`

	// CacheCreationInputTokens counts input written into the prompt cache.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`

	// CacheReadInputTokens counts input read from the prompt cache.
	CacheReadInputTokens int `json:"cache_read_input_tokens,omitempty"`
	// ToolUseInputTokens is the subset of InputTokens attributable to results
	// from provider-managed tool executions that were supplied back to the
	// model. It is not additive, just as ReasoningTokens is not additive.
	ToolUseInputTokens int `json:"tool_use_input_tokens,omitempty"`
	// ReasoningTokens is the number of output tokens spent on reasoning, when
	// the provider reports it separately (e.g. OpenAI o-series, Grok reasoning
	// models, and Anthropic extended thinking). It is a subset of OutputTokens,
	// not additive.
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	// ModalityTokens preserves provider-reported modality detail using the same
	// disjoint buckets as the aggregate fields. Keys are lower-case provider
	// modality names such as "text", "audio", "image", and "video".
	ModalityTokens map[string]ModalityTokenUsage `json:"modality_tokens,omitempty"`

	// InputModalityTokenDetailsIncomplete marks missing modality detail for
	// known aggregate input.
	InputModalityTokenDetailsIncomplete bool `json:"input_modality_token_details_incomplete,omitempty"`

	// OutputModalityTokenDetailsIncomplete marks missing modality detail for
	// known aggregate output.
	OutputModalityTokenDetailsIncomplete bool `json:"output_modality_token_details_incomplete,omitempty"`

	// CacheReadModalityTokenDetailsIncomplete marks missing modality detail for
	// known cache reads.
	CacheReadModalityTokenDetailsIncomplete bool `json:"cache_read_modality_token_details_incomplete,omitempty"`
	// CacheCreationInputTokensUnavailable distinguishes a measured zero from a
	// provider that exposes cache reads but no cache-creation token metric.
	CacheCreationInputTokensUnavailable bool `json:"cache_creation_input_tokens_unavailable,omitempty"`
	// TotalTokenCountMismatch marks provider totals that disagree with the
	// reported buckets; a list-price estimate would be unreliable.
	TotalTokenCountMismatch bool `json:"total_token_count_mismatch,omitempty"`
}

// TotalInputTokens returns the full input size across the disjoint input-side
// token buckets. Invalid values saturate; Validate rejects them for accounting.
func (u Usage) TotalInputTokens() int {
	total, ok := sumNonnegative(
		u.InputTokens,
		u.CacheCreationInputTokens,
		u.CacheReadInputTokens,
	)
	if !ok {
		return math.MaxInt
	}
	return total
}

// Validate rejects counters that cannot be safely summed for pricing or limits.
func (u Usage) Validate() error {
	for _, count := range []int{
		u.InputTokens,
		u.OutputTokens,
		u.CacheCreationInputTokens,
		u.CacheReadInputTokens,
		u.ToolUseInputTokens,
		u.ReasoningTokens,
	} {
		if count < 0 {
			return errors.New("llm: usage counters cannot be negative")
		}
	}
	for _, count := range u.Requests {
		if count < 0 {
			return errors.New("llm: request counts cannot be negative")
		}
	}
	for _, detail := range u.ModalityTokens {
		if detail.InputTokens < 0 || detail.OutputTokens < 0 ||
			detail.CacheCreationInputTokens < 0 ||
			detail.CacheReadInputTokens < 0 {
			return errors.New("llm: modality counters cannot be negative")
		}
		if _, ok := sumNonnegative(
			detail.InputTokens,
			detail.CacheCreationInputTokens,
			detail.CacheReadInputTokens,
			detail.OutputTokens,
		); !ok {
			return errors.New("llm: modality token total overflows")
		}
	}
	if _, ok := sumNonnegative(
		u.InputTokens,
		u.CacheCreationInputTokens,
		u.CacheReadInputTokens,
		u.OutputTokens,
	); !ok {
		return errors.New("llm: usage token total overflows")
	}
	return nil
}

// UnmarshalJSON accepts provider-native usage shapes and maps their cache and
// reasoning breakdowns onto Kernel's canonical token buckets.
func (u *Usage) UnmarshalJSON(data []byte) error {
	type usageAlias Usage
	type inputTokensDetails struct {
		CachedTokens     int `json:"cached_tokens,omitempty"`
		CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	}
	var raw struct {
		usageAlias
		InputTokensDetails  *inputTokensDetails `json:"input_tokens_details,omitempty"`
		PromptTokens        *int                `json:"prompt_tokens,omitempty"`
		PromptTokensDetails *inputTokensDetails `json:"prompt_tokens_details,omitempty"`
		OutputTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens,omitempty"`
			ThinkingTokens  int `json:"thinking_tokens,omitempty"`
		} `json:"output_tokens_details,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, field := range []string{
		"cost",
		"cost_estimate_unavailable",
		"service_tier",
		"speed",
	} {
		if _, present := fields[field]; present {
			return fmt.Errorf(
				"llm: usage field %q requires record migration",
				field,
			)
		}
	}
	*u = Usage(raw.usageAlias)

	// An explicit canonical cache bucket, including zero, declares that the
	// input fields are already disjoint. Provider-native detail objects must not
	// reinterpret that data.
	_, hasCanonicalCacheRead := fields["cache_read_input_tokens"]
	_, hasCanonicalCacheCreation := fields["cache_creation_input_tokens"]
	if !hasCanonicalCacheRead && !hasCanonicalCacheCreation {
		normalize := func(prompt, cached, written int) {
			prompt = max(0, prompt)
			cached = min(max(0, cached), prompt)
			written = min(max(0, written), prompt-cached)
			input := prompt - cached - written
			u.InputTokens = input
			u.CacheReadInputTokens = cached
			u.CacheCreationInputTokens = written
		}

		_, hasInputTokens := fields["input_tokens"]
		_, hasInputDetails := fields["input_tokens_details"]
		_, hasPromptTokens := fields["prompt_tokens"]
		_, hasPromptDetails := fields["prompt_tokens_details"]
		switch {
		case hasInputTokens &&
			hasInputDetails &&
			raw.InputTokensDetails != nil:
			normalize(
				u.InputTokens,
				raw.InputTokensDetails.CachedTokens,
				raw.InputTokensDetails.CacheWriteTokens,
			)
		case hasPromptTokens &&
			hasPromptDetails &&
			raw.PromptTokens != nil &&
			raw.PromptTokensDetails != nil:
			normalize(
				*raw.PromptTokens,
				raw.PromptTokensDetails.CachedTokens,
				raw.PromptTokensDetails.CacheWriteTokens,
			)
		}
	}

	if u.ReasoningTokens == 0 && raw.OutputTokensDetails != nil {
		switch {
		case raw.OutputTokensDetails.ReasoningTokens != 0:
			u.ReasoningTokens = raw.OutputTokensDetails.ReasoningTokens
		case raw.OutputTokensDetails.ThinkingTokens != 0:
			u.ReasoningTokens = raw.OutputTokensDetails.ThinkingTokens
		}
	}
	return nil
}

// Copy returns a deep copy of the usage data.
func (u *Usage) Copy() *Usage {
	cp := &Usage{
		Requests:                                maps.Clone(u.Requests),
		InputTokens:                             u.InputTokens,
		OutputTokens:                            u.OutputTokens,
		CacheCreationInputTokens:                u.CacheCreationInputTokens,
		CacheReadInputTokens:                    u.CacheReadInputTokens,
		ToolUseInputTokens:                      u.ToolUseInputTokens,
		ReasoningTokens:                         u.ReasoningTokens,
		InputModalityTokenDetailsIncomplete:     u.InputModalityTokenDetailsIncomplete,
		OutputModalityTokenDetailsIncomplete:    u.OutputModalityTokenDetailsIncomplete,
		CacheReadModalityTokenDetailsIncomplete: u.CacheReadModalityTokenDetailsIncomplete,
		CacheCreationInputTokensUnavailable:     u.CacheCreationInputTokensUnavailable,
		TotalTokenCountMismatch:                 u.TotalTokenCountMismatch,
	}
	if len(u.ModalityTokens) > 0 {
		cp.ModalityTokens = maps.Clone(u.ModalityTokens)
	}
	return cp
}

// Absorb merges a cumulative usage frame into this usage object. Streaming
// providers report running totals for the whole message rather than
// increments (Anthropic documents message_delta usage as cumulative), so a
// later frame supersedes an earlier one field by field. A zero token count
// means the frame omitted that field, so token buckets merge by max rather
// than wholesale replacement — message_delta frames that carry only
// output_tokens must not erase the input buckets seeded by message_start.
// Use Add for aggregating usage across separate requests.
func (u *Usage) Absorb(other *Usage) {
	if other == nil {
		return
	}
	u.InputTokens = max(u.InputTokens, other.InputTokens)
	u.OutputTokens = max(u.OutputTokens, other.OutputTokens)
	u.CacheCreationInputTokens = max(
		u.CacheCreationInputTokens,
		other.CacheCreationInputTokens,
	)
	u.CacheReadInputTokens = max(
		u.CacheReadInputTokens,
		other.CacheReadInputTokens,
	)
	u.ToolUseInputTokens = max(
		u.ToolUseInputTokens,
		other.ToolUseInputTokens,
	)
	u.ReasoningTokens = max(u.ReasoningTokens, other.ReasoningTokens)
	if len(other.Requests) > 0 {
		if u.Requests == nil {
			u.Requests = map[string]int{}
		}
		for name, count := range other.Requests {
			u.Requests[name] = max(u.Requests[name], count)
		}
	}
	if len(other.ModalityTokens) > 0 {
		if u.ModalityTokens == nil {
			u.ModalityTokens = make(
				map[string]ModalityTokenUsage,
				len(other.ModalityTokens),
			)
		}
		for modality, frame := range other.ModalityTokens {
			current := u.ModalityTokens[modality]
			current.InputTokens = max(
				current.InputTokens,
				frame.InputTokens,
			)
			current.OutputTokens = max(
				current.OutputTokens,
				frame.OutputTokens,
			)
			current.CacheCreationInputTokens = max(
				current.CacheCreationInputTokens,
				frame.CacheCreationInputTokens,
			)
			current.CacheReadInputTokens = max(
				current.CacheReadInputTokens,
				frame.CacheReadInputTokens,
			)
			u.ModalityTokens[modality] = current
		}
	}
	u.mergeFlags(other)
}

// Add sums usage from a separate request into u. It rejects overflow without
// changing u. Use Absorb for cumulative frames within one streamed response.
func (u *Usage) Add(other *Usage) error {
	if other == nil {
		return nil
	}
	if err := u.Validate(); err != nil {
		return err
	}
	if err := other.Validate(); err != nil {
		return err
	}
	next := u.Copy()
	for _, field := range []struct {
		value *int
		add   int
	}{
		{&next.InputTokens, other.InputTokens},
		{&next.OutputTokens, other.OutputTokens},
		{
			&next.CacheCreationInputTokens,
			other.CacheCreationInputTokens,
		},
		{&next.CacheReadInputTokens, other.CacheReadInputTokens},
		{&next.ToolUseInputTokens, other.ToolUseInputTokens},
		{&next.ReasoningTokens, other.ReasoningTokens},
	} {
		value, ok := sumNonnegative(*field.value, field.add)
		if !ok {
			return errors.New("llm: usage counters overflow")
		}
		*field.value = value
	}
	if len(other.Requests) > 0 {
		if next.Requests == nil {
			next.Requests = map[string]int{}
		}
		for name, count := range other.Requests {
			value, ok := sumNonnegative(next.Requests[name], count)
			if !ok {
				return errors.New("llm: request count overflows")
			}
			next.Requests[name] = value
		}
	}
	if len(other.ModalityTokens) > 0 {
		if next.ModalityTokens == nil {
			next.ModalityTokens = make(
				map[string]ModalityTokenUsage,
				len(other.ModalityTokens),
			)
		}
		for modality, addition := range other.ModalityTokens {
			current := next.ModalityTokens[modality]
			values := []*int{
				&current.InputTokens,
				&current.OutputTokens,
				&current.CacheCreationInputTokens,
				&current.CacheReadInputTokens,
			}
			counts := []int{
				addition.InputTokens,
				addition.OutputTokens,
				addition.CacheCreationInputTokens,
				addition.CacheReadInputTokens,
			}
			for i, field := range values {
				value, ok := sumNonnegative(*field, counts[i])
				if !ok {
					return errors.New("llm: modality counters overflow")
				}
				*field = value
			}
			next.ModalityTokens[modality] = current
		}
	}
	next.mergeFlags(other)
	if err := next.Validate(); err != nil {
		return err
	}
	*u = *next
	return nil
}

func sumNonnegative(counts ...int) (int, bool) {
	total := 0
	for _, count := range counts {
		if count < 0 || count > math.MaxInt-total {
			return 0, false
		}
		total += count
	}
	return total, true
}

// mergeFlags carries forward flags that mark data as missing. Once any part of
// an aggregate lacks a detail, the aggregate lacks it too.
func (u *Usage) mergeFlags(other *Usage) {
	u.CacheCreationInputTokensUnavailable = u.CacheCreationInputTokensUnavailable ||
		other.CacheCreationInputTokensUnavailable
	u.InputModalityTokenDetailsIncomplete = u.InputModalityTokenDetailsIncomplete ||
		other.InputModalityTokenDetailsIncomplete
	u.OutputModalityTokenDetailsIncomplete = u.OutputModalityTokenDetailsIncomplete ||
		other.OutputModalityTokenDetailsIncomplete
	u.CacheReadModalityTokenDetailsIncomplete = u.CacheReadModalityTokenDetailsIncomplete ||
		other.CacheReadModalityTokenDetailsIncomplete
	u.TotalTokenCountMismatch = u.TotalTokenCountMismatch ||
		other.TotalTokenCountMismatch
}
