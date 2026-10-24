package realtimewire

import (
	"maps"
	"regexp"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/realtime"
)

// maxUsageTokens bounds any one provider count, so sums cannot overflow.
const maxUsageTokens = 1e9

// modalityName keeps provider modality names short map keys.
var modalityName = regexp.MustCompile(`^[a-z][a-z_]{0,31}$`)

// count reads a token count; ok is false when it is absent.
func count(m map[string]any, k string) (int, bool, error) {
	v, ok := m[k].(float64)
	if !ok {
		return 0, false, nil
	}
	if v < 0 || v > maxUsageTokens || v != float64(int(v)) {
		return 0, false, realtime.Failf(realtime.ErrorProtocol, "usage %s is %v", k, v)
	}
	return int(v), true, nil
}

// modalities collects per-modality buckets and checks them against the
// aggregate. Detail that exceeds its aggregate is dropped as a whole, since
// pricing must not attribute more tokens than were counted.
type modalities struct {
	tokens  map[string]llm.ModalityTokenUsage
	invalid bool
}

func (d *modalities) add(name string, n int, bucket func(*llm.ModalityTokenUsage) *int) {
	name = strings.ToLower(name)
	_, known := d.tokens[name]
	if !modalityName.MatchString(name) || len(d.tokens) >= 8 && !known {
		d.invalid = true
		return
	}
	if d.tokens == nil {
		d.tokens = map[string]llm.ModalityTokenUsage{}
	}
	t := d.tokens[name]
	*bucket(&t) += n
	d.tokens[name] = t
}

// apply sets u's modality detail and its completeness flags.
func (d *modalities) apply(u *llm.Usage) {
	var sum llm.ModalityTokenUsage
	for _, t := range d.tokens {
		sum.InputTokens += t.InputTokens
		sum.OutputTokens += t.OutputTokens
		sum.CacheReadInputTokens += t.CacheReadInputTokens
	}
	if d.invalid || sum.InputTokens > u.InputTokens || sum.OutputTokens > u.OutputTokens || sum.CacheReadInputTokens > u.CacheReadInputTokens {
		u.InputModalityTokenDetailsIncomplete = u.InputTokens > 0
		u.OutputModalityTokenDetailsIncomplete = u.OutputTokens > 0
		u.CacheReadModalityTokenDetailsIncomplete = u.CacheReadInputTokens > 0
		return
	}
	if len(d.tokens) > 0 {
		u.ModalityTokens = maps.Clone(d.tokens)
	}
	u.InputModalityTokenDetailsIncomplete = sum.InputTokens != u.InputTokens
	u.OutputModalityTokenDetailsIncomplete = sum.OutputTokens != u.OutputTokens
	u.CacheReadModalityTokenDetailsIncomplete = sum.CacheReadInputTokens != u.CacheReadInputTokens
}

func input(t *llm.ModalityTokenUsage) *int     { return &t.InputTokens }
func output(t *llm.ModalityTokenUsage) *int    { return &t.OutputTokens }
func cacheRead(t *llm.ModalityTokenUsage) *int { return &t.CacheReadInputTokens }

// grokUsage reads xAI's usage: input and output totals with text, audio and
// grok token details. grok_tokens is kept as its own "grok" modality rather
// than folded into text or audio, so a price list can rate it or leave it at
// the base rates. Usage without both totals is not reported.
func grokUsage(m map[string]any) (*llm.Usage, error) {
	in, inOK, e := count(m, "input_tokens")
	if e != nil {
		return nil, e
	}
	out, outOK, e := count(m, "output_tokens")
	if e != nil || !inOK || !outOK {
		return nil, e
	}
	u := &llm.Usage{InputTokens: in, OutputTokens: out}
	var d modalities
	present := false
	for _, side := range []struct {
		key    string
		bucket func(*llm.ModalityTokenUsage) *int
	}{{"input_token_details", input}, {"output_token_details", output}} {
		details := object(m, side.key)
		for _, k := range []string{"text_tokens", "audio_tokens", "grok_tokens"} {
			n, ok, e := count(details, k)
			if e != nil {
				return nil, e
			}
			if ok {
				present = true
			}
			if n > 0 {
				d.add(strings.TrimSuffix(k, "_tokens"), n, side.bucket)
			}
		}
	}
	if !present {
		u.InputModalityTokenDetailsIncomplete = true
		u.OutputModalityTokenDetailsIncomplete = true
		return u, nil
	}
	d.apply(u)
	return u, nil
}

// googleUsage reads Gemini Live's usageMetadata into the buckets the batch
// Gemini provider uses: cached prompt tokens are cache reads, tool-use prompt
// tokens are input, and thoughts are output reported again as
// ReasoningTokens. Modality detail comes from promptTokensDetails,
// cacheTokensDetails, toolUsePromptTokensDetails, and responseTokensDetails
// (Developer API) or candidatesTokensDetails (Vertex). Thoughts carry no
// modality and are attributed to text, which is how Gemini bills them.
func googleUsage(m map[string]any) (*llm.Usage, error) {
	counts := map[string]int{}
	found := map[string]bool{}
	for _, k := range []string{"promptTokenCount", "responseTokenCount", "candidatesTokenCount", "cachedContentTokenCount", "toolUsePromptTokenCount", "thoughtsTokenCount", "totalTokenCount"} {
		n, ok, e := count(m, k)
		if e != nil {
			return nil, e
		}
		counts[k], found[k] = n, ok
	}
	response, responseKey := counts["responseTokenCount"], "responseTokensDetails"
	if !found["responseTokenCount"] {
		response, responseKey = counts["candidatesTokenCount"], "candidatesTokensDetails"
	}
	if !found["promptTokenCount"] || !found["responseTokenCount"] && !found["candidatesTokenCount"] {
		return nil, nil
	}
	prompt, cached, toolUse, thoughts := counts["promptTokenCount"], counts["cachedContentTokenCount"], counts["toolUsePromptTokenCount"], counts["thoughtsTokenCount"]
	if cached > prompt {
		return nil, realtime.Failf(realtime.ErrorProtocol, "usage has %d cached of %d prompt tokens", cached, prompt)
	}
	u := &llm.Usage{
		InputTokens:                         prompt - cached + toolUse,
		CacheReadInputTokens:                cached,
		OutputTokens:                        response + thoughts,
		ToolUseInputTokens:                  toolUse,
		ReasoningTokens:                     thoughts,
		CacheCreationInputTokensUnavailable: true,
	}
	if total := counts["totalTokenCount"]; found["totalTokenCount"] && total != prompt+response+thoughts+toolUse {
		u.TotalTokenCountMismatch = true
	}
	var d modalities
	// Prompt detail includes cached tokens; cache detail moves them out.
	if e := googleDetails(m, "promptTokensDetails", &d, input, 1); e != nil {
		return nil, e
	}
	if e := googleDetails(m, "cacheTokensDetails", &d, input, -1); e != nil {
		return nil, e
	}
	if e := googleDetails(m, "cacheTokensDetails", &d, cacheRead, 1); e != nil {
		return nil, e
	}
	if e := googleDetails(m, "toolUsePromptTokensDetails", &d, input, 1); e != nil {
		return nil, e
	}
	if e := googleDetails(m, responseKey, &d, output, 1); e != nil {
		return nil, e
	}
	if thoughts > 0 && len(array(m, responseKey)) > 0 {
		d.add("text", thoughts, output)
	}
	for _, t := range d.tokens {
		if t.InputTokens < 0 {
			d.invalid = true
		}
	}
	d.apply(u)
	return u, nil
}

// googleDetails adds sign times each ModalityTokenCount in m[key].
func googleDetails(m map[string]any, key string, d *modalities, bucket func(*llm.ModalityTokenUsage) *int, sign int) error {
	for _, v := range array(m, key) {
		entry, _ := v.(map[string]any)
		n, _, e := count(entry, "tokenCount")
		if e != nil {
			return e
		}
		d.add(str(entry, "modality"), sign*n, bucket)
	}
	return nil
}
