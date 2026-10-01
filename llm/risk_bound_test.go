package llm

import "testing"

func TestModelInfoCanBoundCost(t *testing.T) {
	priced := pricedSheet()
	invalid := pricedSheet()
	invalid.UpdatedAt = ""
	for _, test := range []struct {
		name     string
		pricing  *Pricing
		support  Support
		currency string
		want     bool
	}{
		{"priced", &priced, "", "USD", true},
		{"accepts max tokens", &priced, SupportSupported, "USD", true},
		{"unpriced", nil, "", "USD", false},
		{"other currency", &priced, "", "EUR", false},
		{"invalid sheet", &invalid, "", "USD", false},
		{"rejects max tokens", &priced, SupportUnsupported, "USD", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			info := ModelInfo{Pricing: test.pricing}
			if test.support != "" {
				info.Capabilities.Features = map[Feature]Support{
					FeatureMaxTokens: test.support,
				}
			}
			got := info.CanBoundCost(test.currency)
			if got != test.want {
				t.Fatalf("CanBoundCost=%v, want %v", got, test.want)
			}
			if test.pricing == nil || test.currency != test.pricing.Currency ||
				test.pricing.Validate() != nil {
				return
			}
			// The answer matches MaxCostAtRisk on a call with default
			// settings, as preparation fits them.
			fitted, _, err := info.Capabilities.Fit(
				Input{Model: "fixture"},
				CapabilityAdjust,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			request := riskRequest(t, fitted.Settings.MaxTokens, nil)
			if _, ok := WorstCaseCost(request, *test.pricing); ok != got {
				t.Fatalf("MaxCostAtRisk ok=%v, CanBoundCost=%v", ok, got)
			}
		})
	}
}
