package catalog

import (
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Cost returns what one model call cost. It prefers the provider-reported
// result.Cost, such as OpenRouter's. Otherwise it estimates the cost from the
// catalog's list price, but only when that price provably applies to the
// call; when it does not, or usage is missing, the error says why.
//
// The list price applies when all of these hold:
//   - client's model information for result.Requested.Model is the
//     unmodified catalog entry for its API and endpoint;
//   - the provider served that model or the snapshot the entry documents
//     (Entry.Serves);
//   - result.ServiceTier is a tier the catalog prices, which is the
//     provider's default tier.
//
// The client does not prove where a request ran. With a custom transport or
// proxy, check that routing and its price yourself instead.
func Cost(client llm.Client, result llm.Result) (llm.Cost, error) {
	if result.Cost != nil {
		return result.Cost.Clone(), nil
	}
	model := result.Requested.Model
	info, ok := client.ModelInfo(model)
	if !ok || info.Pricing == nil {
		return llm.Cost{}, fmt.Errorf("no price for model %q", model)
	}
	if !catalogPrice(client, result, info) {
		return llm.Cost{}, errors.New(
			"the served model or its price is not the catalog's",
		)
	}
	if !pricedServiceTier(client.Provider(), result.ServiceTier) {
		return llm.Cost{}, fmt.Errorf(
			"service tier %q has no catalog price",
			result.ServiceTier,
		)
	}
	if result.Usage == nil {
		return llm.Cost{}, errors.New("the provider reported no usage")
	}
	cost, ok := info.Pricing.Estimate(result.Usage)
	if !ok {
		return llm.Cost{}, errors.New("the reported usage is not enough to price")
	}
	return cost, nil
}

// catalogPrice reports whether info is the unmodified catalog entry for the
// client's API and endpoint, and whether the provider served that model or
// a snapshot the entry covers.
func catalogPrice(client llm.Client, result llm.Result, info llm.ModelInfo) bool {
	model := result.Requested.Model
	evidence := info.Evidence
	if evidence.Owner != Owner || evidence.Model != model ||
		evidence.API != client.API() ||
		result.Requested.Provider != client.Provider() ||
		result.Served.Provider != client.Provider() {
		return false
	}
	documented, ok := Lookup(evidence.API, evidence.Endpoint, model)
	if !ok || documented.Pricing == nil ||
		!reflect.DeepEqual(evidence, documented.Evidence) ||
		!reflect.DeepEqual(info.Pricing, documented.Pricing) {
		return false
	}
	entry, ok := Find(client.Provider(), model)
	return ok && reflect.DeepEqual(entry.Pricing, documented.Pricing) &&
		entry.Serves(result.Served.Model)
}

// pricedServiceTier reports whether the provider's catalog file lists tier
// as one its prices describe.
func pricedServiceTier(provider, tier string) bool {
	p, ok := find(provider)
	return ok && slices.Contains(p.pricedServiceTiers, tier)
}
