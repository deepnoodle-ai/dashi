// Pricing and cost values are adapted from Dive (65c393f), Apache-2.0.

package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// Rates prices disjoint token buckets per million tokens. Every value is
// literal: zero means free, never inherited from another rate.
type Rates struct {
	// Input prices uncached input not classified as a cache write.
	Input float64 `json:"input"`
	// Output prices generated tokens, including reasoning tokens.
	Output float64 `json:"output"`
	// CacheRead prices input served from the prompt cache.
	CacheRead float64 `json:"cache_read"`
	// CacheWrite prices input written to the prompt cache.
	CacheWrite float64 `json:"cache_write"`
}

// UnmarshalJSON requires all four literal rates. An absent rate must not
// silently acquire the meaning of a free rate in a stored sheet.
func (r *Rates) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, key := range []string{
		"input", "output", "cache_read", "cache_write",
	} {
		if len(fields[key]) == 0 || bytes.Equal(fields[key], []byte("null")) {
			return fmt.Errorf("llm: pricing requires explicit %s rate", key)
		}
	}
	type alias Rates
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded alias
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("llm: decode rates: %w", err)
	}
	value := Rates(decoded)
	if err := value.validate(); err != nil {
		return err
	}
	*r = value
	return nil
}

func (r Rates) validate() error {
	for _, rate := range []float64{
		r.Input, r.Output, r.CacheRead, r.CacheWrite,
	} {
		if rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return errors.New("llm: pricing requires finite nonnegative rates")
		}
	}
	return nil
}

// PriceTier replaces all base rates when total input reaches MinInputTokens.
// Tiers must be strictly increasing; the highest eligible tier wins.
type PriceTier struct {
	// MinInputTokens is inclusive and counts all disjoint input buckets.
	MinInputTokens int `json:"min_input_tokens"`
	// Rates completely replace Base above the threshold.
	Rates Rates `json:"rates"`
}

// UnmarshalJSON requires an explicit replacement rate set. Missing tier rates
// must not silently turn a stored threshold into a free tier.
func (t *PriceTier) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields["rates"]) == 0 ||
		bytes.Equal(fields["rates"], []byte("null")) {
		return errors.New("llm: pricing tier requires explicit rates")
	}
	type alias PriceTier
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded alias
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("llm: decode pricing tier: %w", err)
	}
	value := PriceTier(decoded)
	if value.MinInputTokens <= 0 {
		return errors.New("llm: pricing tier requires a positive threshold")
	}
	*t = value
	return nil
}

// Pricing is one complete token price sheet. Its caller qualifies the model,
// destination, and service variant to which it applies. It does not choose a
// regional, speed, or service-tier variant by itself.
type Pricing struct {
	// Currency identifies the unit of every rate and estimated amount.
	Currency string `json:"currency"`
	// UpdatedAt dates the sheet without implying that prices remain current.
	UpdatedAt string `json:"updated_at"`
	// Reference names the price page or catalog version used for these rates.
	Reference string `json:"reference,omitempty"`
	// Base applies below the first tier and to requests without tiers.
	Base Rates `json:"base"`
	// Tiers replace Base at inclusive thresholds in increasing order.
	Tiers []PriceTier `json:"tiers,omitempty"`
	// Modality supplies complete rates for named modalities, independent of
	// input-size tiers. Other tokens retain the selected base or tier rates.
	Modality map[string]Rates `json:"modality,omitempty"`
	// Requests prices provider-hosted operations in currency units per call.
	Requests map[string]float64 `json:"requests,omitempty"`
}

// Validate rejects incomplete or ambiguous sheets before they are used as
// model knowledge. Reference identifies where rates came from, not their
// qualification for a particular call.
func (p Pricing) Validate() error {
	if strings.TrimSpace(p.Currency) == "" ||
		strings.TrimSpace(p.Currency) != p.Currency {
		return errors.New("llm: pricing requires currency")
	}
	date, err := time.Parse("2006-01-02", p.UpdatedAt)
	if err != nil || date.Format("2006-01-02") != p.UpdatedAt {
		return errors.New("llm: pricing requires an updated_at date")
	}
	if err := p.Base.validate(); err != nil {
		return err
	}
	previous := 0
	for _, tier := range p.Tiers {
		if tier.MinInputTokens <= previous {
			return errors.New("llm: pricing tiers require increasing thresholds")
		}
		if err := tier.Rates.validate(); err != nil {
			return err
		}
		previous = tier.MinInputTokens
	}
	for modality, rates := range p.Modality {
		if strings.TrimSpace(modality) == "" ||
			modality != strings.ToLower(strings.TrimSpace(modality)) {
			return errors.New("llm: pricing requires lowercase modality names")
		}
		if err := rates.validate(); err != nil {
			return err
		}
	}
	for name, rate := range p.Requests {
		if name == "" || rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return errors.New("llm: invalid hosted request price")
		}
	}
	return nil
}

// Clone returns a sheet whose tier slice and modality map are independently
// owned, so a prepared model snapshot cannot change with caller mutations.
func (p Pricing) Clone() Pricing {
	p.Tiers = append([]PriceTier(nil), p.Tiers...)
	p.Modality = maps.Clone(p.Modality)
	p.Requests = maps.Clone(p.Requests)
	return p
}

// UnmarshalJSON refuses legacy flat fields and requires an explicit base.
// Otherwise an old price record could silently become a zero-price sheet.
func (p *Pricing) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields["base"]) == 0 ||
		bytes.Equal(fields["base"], []byte("null")) {
		return errors.New("llm: pricing requires an explicit base rate set")
	}
	type alias Pricing
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded alias
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("llm: decode pricing: %w", err)
	}
	value := Pricing(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*p = value
	return nil
}

// Estimate calculates list-price cost from a sheet already qualified for the
// call. The result is unavailable when token detail cannot support a faithful
// estimate; nil usage never means known zero usage.
func (p Pricing) Estimate(u *Usage) (Cost, bool) {
	if u == nil || p.Validate() != nil {
		return Cost{}, false
	}
	unattributed, valid := unattributedUsageBuckets(u)
	if !valid {
		return Cost{}, false
	}
	rates := p.Base
	for _, tier := range p.Tiers {
		if u.TotalInputTokens() >= tier.MinInputTokens {
			rates = tier.Rates
		}
	}
	if !pricingDetailAvailable(p, rates, u, unattributed) {
		return Cost{}, false
	}
	const perMillion = 1_000_000.0
	cost := Cost{
		Currency: p.Currency,
		Source:   CostSourceListPriceEstimate,
	}
	applyRates := func(tokens ModalityTokenUsage, rates Rates) {
		cost.Input += float64(tokens.InputTokens) * rates.Input / perMillion
		cost.Output += float64(tokens.OutputTokens) * rates.Output / perMillion
		cost.CacheRead += float64(tokens.CacheReadInputTokens) *
			rates.CacheRead / perMillion
		cost.CacheWrite += float64(tokens.CacheCreationInputTokens) *
			rates.CacheWrite / perMillion
	}
	applyRates(unattributed, rates)
	for _, modality := range slices.Sorted(maps.Keys(u.ModalityTokens)) {
		tokens := u.ModalityTokens[modality]
		modalityRates, ok := p.Modality[modality]
		if !ok {
			modalityRates = rates
		}
		applyRates(tokens, modalityRates)
	}
	for name, count := range u.Requests {
		rate, ok := p.Requests[name]
		if !ok || count < 0 {
			return Cost{}, false
		}
		cost.Hosted += float64(count) * rate
	}
	cost.Total = cost.Input + cost.Output + cost.CacheRead + cost.CacheWrite + cost.Hosted
	if cost.Validate() != nil {
		return Cost{}, false
	}
	return cost, true
}

// CostSource distinguishes the origin of a monetary amount. A reported
// estimate is not a provider-reported charge.
type CostSource string

const (
	// CostSourceListPriceEstimate identifies a calculation using supplied rates.
	CostSourceListPriceEstimate CostSource = "list_price_estimate"
	// CostSourceProviderReported identifies an amount reported as a charge.
	CostSourceProviderReported CostSource = "provider_reported"
	// CostSourceProviderEstimate identifies a provider-supplied estimate.
	CostSourceProviderEstimate CostSource = "provider_estimate"
)

// Cost values one model call. A missing *Cost means unknown, independently of
// whether usage is zero or unavailable. Breakdowns may be unknown even when a
// provider reports an exact total.
type Cost struct {
	// Input values uncached input excluding cache writes and cache reads.
	Input float64 `json:"input"`
	// Output values generated output.
	Output float64 `json:"output"`
	// CacheRead values cached input.
	CacheRead float64 `json:"cache_read"`
	// CacheWrite values cache creation input.
	CacheWrite float64 `json:"cache_write"`
	// Hosted values priced provider-hosted requests.
	Hosted float64 `json:"hosted,omitempty"`
	// Total may be available even when the category breakdown is not.
	Total float64 `json:"total"`
	// Currency identifies the unit shared by the monetary fields.
	Currency string `json:"currency"`
	// Source distinguishes reported charges, reported estimates, and rates.
	Source CostSource `json:"source"`
	// BreakdownUnavailable means category zeros are unknown rather than free.
	BreakdownUnavailable bool `json:"breakdown_unavailable,omitempty"`
}

// Validate requires a nonnegative finite amount with a known source and
// currency. It does not equate a provider total with Kernel's breakdown.
func (c Cost) Validate() error {
	if strings.TrimSpace(c.Currency) == "" ||
		strings.TrimSpace(c.Currency) != c.Currency {
		return errors.New("llm: cost requires currency")
	}
	switch c.Source {
	case CostSourceListPriceEstimate, CostSourceProviderReported,
		CostSourceProviderEstimate:
	default:
		return errors.New("llm: cost requires a known source")
	}
	for _, amount := range []float64{
		c.Input, c.Output, c.CacheRead, c.CacheWrite, c.Hosted, c.Total,
	} {
		if amount < 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
			return errors.New("llm: cost requires finite nonnegative amounts")
		}
	}
	return nil
}

// UnmarshalJSON requires a total and rejects legacy model and aggregation
// fields rather than silently dropping them from an attempt-level amount.
func (c *Cost) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields["total"]) == 0 ||
		bytes.Equal(fields["total"], []byte("null")) {
		return errors.New("llm: cost requires an explicit total")
	}
	type alias Cost
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded alias
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("llm: decode cost: %w", err)
	}
	value := Cost(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*c = value
	return nil
}

// unattributedUsageBuckets checks disjoint buckets and returns the tokens that
// have no provider-reported modality. They cannot receive a modality rate.
func unattributedUsageBuckets(u *Usage) (ModalityTokenUsage, bool) {
	if u.InputTokens < 0 || u.OutputTokens < 0 ||
		u.CacheReadInputTokens < 0 || u.CacheCreationInputTokens < 0 {
		return ModalityTokenUsage{}, false
	}
	if u.InputTokens > math.MaxInt-u.CacheCreationInputTokens ||
		u.InputTokens+u.CacheCreationInputTokens >
			math.MaxInt-u.CacheReadInputTokens {
		return ModalityTokenUsage{}, false
	}
	remaining := ModalityTokenUsage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
	}
	for modality, tokens := range u.ModalityTokens {
		if strings.TrimSpace(modality) == "" ||
			modality != strings.ToLower(strings.TrimSpace(modality)) {
			return ModalityTokenUsage{}, false
		}
		if tokens.InputTokens < 0 || tokens.InputTokens > remaining.InputTokens ||
			tokens.OutputTokens < 0 || tokens.OutputTokens > remaining.OutputTokens ||
			tokens.CacheReadInputTokens < 0 ||
			tokens.CacheReadInputTokens > remaining.CacheReadInputTokens ||
			tokens.CacheCreationInputTokens < 0 ||
			tokens.CacheCreationInputTokens > remaining.CacheCreationInputTokens {
			return ModalityTokenUsage{}, false
		}
		remaining.InputTokens -= tokens.InputTokens
		remaining.OutputTokens -= tokens.OutputTokens
		remaining.CacheReadInputTokens -= tokens.CacheReadInputTokens
		remaining.CacheCreationInputTokens -= tokens.CacheCreationInputTokens
	}
	return remaining, true
}

func pricingDetailAvailable(
	p Pricing,
	base Rates,
	u *Usage,
	unattributed ModalityTokenUsage,
) bool {
	if u.TotalTokenCountMismatch {
		return false
	}
	if u.CacheCreationInputTokensUnavailable && base.CacheWrite != base.Input {
		return false
	}
	for _, rates := range p.Modality {
		// Unattributed tokens could belong to this modality, so pricing
		// them at the base rate would invent a billing allocation.
		switch {
		case rates.Input != base.Input &&
			(u.InputModalityTokenDetailsIncomplete ||
				unattributed.InputTokens != 0):
			return false
		case rates.Output != base.Output &&
			(u.OutputModalityTokenDetailsIncomplete ||
				unattributed.OutputTokens != 0):
			return false
		case rates.CacheRead != base.CacheRead &&
			(u.CacheReadModalityTokenDetailsIncomplete ||
				unattributed.CacheReadInputTokens != 0):
			return false
		case rates.CacheWrite != base.CacheWrite &&
			unattributed.CacheCreationInputTokens != 0:
			return false
		}
		if u.CacheCreationInputTokensUnavailable && rates.CacheWrite != rates.Input {
			return false
		}
	}
	return true
}
