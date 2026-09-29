package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// PromptCache asks the provider to keep the start of the request for reuse.
// The zero value caches where Dashi places cache points (Anthropic Messages
// on catalog destinations marked for it), with the provider's default
// lifetime. See design/prompt-cache.md.
type PromptCache struct {
	// Disable sends no cache request. A provider that caches on its own,
	// such as OpenAI or Gemini, may still cache.
	Disable bool
	// TTL is the shortest lifetime wanted for unused entries. Zero uses the
	// provider's default. Dashi sends the shortest lifetime the route offers
	// that is at least TTL; above the longest it sends the longest, with a
	// warning. JSON encodes it as a Go duration string such as "1h".
	TTL time.Duration
	// Key groups calls that share a prefix, such as one session's, so the
	// provider can route them to the same cache. It is a hint: Dashi sends
	// it where the API has a key field (hashed if too long) and ignores it
	// elsewhere, without a warning.
	Key string
}

type promptCacheJSON struct {
	Disable bool   `json:"disable,omitempty"`
	TTL     string `json:"ttl,omitempty"`
	Key     string `json:"key,omitempty"`
}

// MarshalJSON writes TTL as a duration string.
func (c PromptCache) MarshalJSON() ([]byte, error) {
	v := promptCacheJSON{Disable: c.Disable, Key: c.Key}
	if c.TTL != 0 {
		v.TTL = formatDuration(c.TTL)
	}
	return json.Marshal(v)
}

// UnmarshalJSON reads TTL as a duration string.
func (c *PromptCache) UnmarshalJSON(data []byte) error {
	var v promptCacheJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	out := PromptCache{Disable: v.Disable, Key: v.Key}
	if v.TTL != "" {
		ttl, err := time.ParseDuration(v.TTL)
		if err != nil {
			return fmt.Errorf("llm: prompt_cache ttl: %w", err)
		}
		out.TTL = ttl
	}
	*c = out
	return nil
}

func (c PromptCache) validate() error {
	switch {
	case c.TTL < 0:
		return errors.New("llm: prompt_cache: ttl cannot be negative")
	case c.Disable && c.TTL != 0:
		return errors.New(
			"llm: prompt_cache: disable and ttl are both set; remove one",
		)
	}
	return nil
}

// Durations is a list of durations that JSON encodes as duration strings,
// such as ["5m","1h"].
type Durations []time.Duration

// MarshalJSON writes each duration as a string.
func (d Durations) MarshalJSON() ([]byte, error) {
	out := make([]string, len(d))
	for i, v := range d {
		out[i] = formatDuration(v)
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads duration strings.
func (d *Durations) UnmarshalJSON(data []byte) error {
	var in []string
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	out := make(Durations, len(in))
	for i, s := range in {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("llm: duration: %w", err)
		}
		out[i] = v
	}
	*d = out
	return nil
}

// CacheLifetime returns the lifetime a route sends for a requested minimum
// ttl: the shortest offered lifetime that is at least ttl, or the longest
// when none is. offered lists a route's lifetimes shortest first, as
// Capabilities.CacheTTLs does; with none offered it returns zero. Providers
// use it to encode a fitted request.
func CacheLifetime(ttl time.Duration, offered []time.Duration) time.Duration {
	for _, v := range offered {
		if v >= ttl {
			return v
		}
	}
	if len(offered) == 0 {
		return 0
	}
	return offered[len(offered)-1]
}

// fitPromptCache omits a lifetime the destination rejects and clamps one
// beyond the longest offered. A lifetime within the offered range is met as
// asked: TTL is a minimum, so a longer lifetime satisfies it.
func (c Capabilities) fitPromptCache(f *fitter, s *PromptCache) error {
	if s.TTL <= 0 || s.Disable {
		return nil
	}
	requested := formatDuration(s.TTL)
	if c.Features[FeaturePromptCache] == SupportUnsupported {
		err := f.omit(
			SettingPromptCacheTTL,
			requested,
			"a prompt cache lifetime cannot be requested here; "+
				"the provider's own caching applies",
			unsupportedError(f.model, FeaturePromptCache),
		)
		if err == nil {
			s.TTL = 0
		}
		return err
	}
	if len(c.CacheTTLs) == 0 {
		return nil
	}
	longest := slices.Max(c.CacheTTLs)
	if s.TTL <= longest {
		return nil
	}
	sent := formatDuration(longest)
	err := f.clamp(
		SettingPromptCacheTTL,
		requested,
		sent,
		fmt.Sprintf(
			"prompt cache ttl %s exceeds the longest lifetime offered, %s; sent %s",
			requested,
			sent,
			sent,
		),
		fmt.Errorf(
			"llm: prompt cache ttl %s exceeds the longest lifetime offered, %s",
			requested,
			sent,
		),
	)
	if err == nil {
		s.TTL = longest
	}
	return err
}

// formatDuration writes d without trailing zero units: "1h", not "1h0m0s".
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
