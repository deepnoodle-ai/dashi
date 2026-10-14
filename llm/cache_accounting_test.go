package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPricingEstimatePricesOneHourWritesSeparately(t *testing.T) {
	p := pricedSheet()
	oneHour := amt("2")
	p.Base.CacheWrite1h = &oneHour
	u := &Usage{
		InputTokens:                1_000_000,
		CacheCreationInputTokens:   3_000_000,
		CacheCreation1hInputTokens: 1_000_000,
	}
	cost, ok := p.Estimate(u)
	if !ok {
		t.Fatal("mixed cache writes could not be priced")
	}
	// 2M × 1.25 + 1M × 2 = 4.5; totals keep the disjoint buckets.
	if cost.CacheWrite != amt("4.5") || cost.Total != amt("5.5") {
		t.Fatalf("mixed cache writes priced wrong: %+v", cost)
	}
	if u.TotalInputTokens() != 4_000_000 {
		t.Fatalf("one-hour subset changed the input total: %d", u.TotalInputTokens())
	}

	p.Base.CacheWrite1h = nil
	if _, ok := p.Estimate(u); ok {
		t.Fatal("one-hour writes were priced without a one-hour rate")
	}
	u.CacheCreation1hInputTokens = 0
	if _, ok := p.Estimate(u); !ok {
		t.Fatal("a sheet without a one-hour rate could not price 5m writes")
	}
}

func TestUsageOneHourWritesAreASubset(t *testing.T) {
	bad := Usage{CacheCreationInputTokens: 1, CacheCreation1hInputTokens: 2}
	if err := bad.Validate(); err == nil {
		t.Fatal("one-hour writes above all writes were accepted")
	}
	total := Usage{CacheCreationInputTokens: 5, CacheCreation1hInputTokens: 2}
	if err := total.Add(&Usage{
		CacheCreationInputTokens:   3,
		CacheCreation1hInputTokens: 3,
	}); err != nil || total.CacheCreation1hInputTokens != 5 {
		t.Fatalf("add lost one-hour writes: %+v, %v", total, err)
	}
	if cp := total.Clone(); cp.CacheCreation1hInputTokens != 5 {
		t.Fatalf("copy lost one-hour writes: %+v", cp)
	}
	frame := Usage{CacheCreationInputTokens: 7, CacheCreation1hInputTokens: 4}
	frame.Absorb(&Usage{OutputTokens: 9})
	if frame.CacheCreation1hInputTokens != 4 {
		t.Fatalf("absorb erased one-hour writes: %+v", frame)
	}
}

func TestRatesOneHourWriteIsOptionalInJSON(t *testing.T) {
	var r Rates
	if err := json.Unmarshal(
		[]byte(`{"input":"1","output":"2","cache_read":"0.1","cache_write":"1.25"}`),
		&r,
	); err != nil || r.CacheWrite1h != nil {
		t.Fatalf("a sheet without a one-hour rate: %+v, %v", r, err)
	}
	if err := json.Unmarshal(
		[]byte(`{"input":"1","output":"2","cache_read":"0.1","cache_write":"1.25","cache_write_1h":"2"}`),
		&r,
	); err != nil || r.CacheWrite1h == nil || *r.CacheWrite1h != amt("2") {
		t.Fatalf("one-hour rate not decoded: %+v, %v", r, err)
	}
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), `"cache_write_1h":"2"`) {
		t.Fatalf("one-hour rate not encoded: %s", raw)
	}
	if err := json.Unmarshal(
		[]byte(`{"input":"1","output":"2","cache_read":"0.1","cache_write":"1.25","cache_write_1h":"-1"}`),
		&r,
	); err == nil {
		t.Fatal("a negative one-hour rate was accepted")
	}

	p := pricedSheet()
	rate := amt("2")
	p.Base.CacheWrite1h = &rate
	clone := p.Clone()
	*clone.Base.CacheWrite1h = amt("3")
	if *p.Base.CacheWrite1h != amt("2") {
		t.Fatal("clone shares the one-hour rate")
	}
}
