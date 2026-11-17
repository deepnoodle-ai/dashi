package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// listedModel is one model a provider's model-list endpoint returned.
type listedModel struct {
	ID string
	// Aliases are other IDs the provider says name this model (Mistral).
	Aliases []string
	// NotText is true when the provider's own metadata says the model does
	// not generate text: Mistral's capabilities or Google's
	// supportedGenerationMethods.
	NotText bool
}

// providerSpec says how to list one provider's models.
type providerSpec struct {
	// Name is the catalog's provider name.
	Name string
	// KeyEnv lists the environment variables that may hold the API key, in
	// order of preference.
	KeyEnv []string
	// BaseURL is the API root, such as https://api.openai.com. Tests point
	// it at an httptest server.
	BaseURL string
	// List fetches the model list.
	List func(c *apiClient) ([]listedModel, error)
	// Exists checks one ID directly, for providers whose list omits
	// aliases (Anthropic). Nil when the provider has no such check.
	Exists func(c *apiClient, id string) (bool, error)
}

// providers returns the specs in report order.
func providers() []providerSpec {
	return []providerSpec{
		{Name: "anthropic", KeyEnv: []string{"ANTHROPIC_API_KEY"}, BaseURL: "https://api.anthropic.com",
			List: listAnthropic, Exists: anthropicExists},
		{Name: "openai", KeyEnv: []string{"OPENAI_API_KEY"}, BaseURL: "https://api.openai.com",
			List: listOpenAIStyle},
		{Name: "grok", KeyEnv: []string{"XAI_API_KEY", "GROK_API_KEY"}, BaseURL: "https://api.x.ai",
			List: listOpenAIStyle},
		{Name: "mistral", KeyEnv: []string{"MISTRAL_API_KEY"}, BaseURL: "https://api.mistral.ai",
			List: listMistral},
		{Name: "meta", KeyEnv: []string{"META_API_KEY"}, BaseURL: "https://api.meta.ai",
			List: listOpenAIStyle},
		{Name: "google", KeyEnv: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
			BaseURL: "https://generativelanguage.googleapis.com", List: listGoogle},
	}
}

// key returns the first set key variable's value, or "".
func (p providerSpec) key() string {
	for _, name := range p.KeyEnv {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}

// apiClient sends authenticated GET requests to one provider. Its errors
// never contain the key or the request URL, because Google's key travels in
// the query string.
type apiClient struct {
	http     *http.Client
	provider string
	baseURL  string
	key      string
}

// maxPages bounds pagination in case a provider keeps returning a cursor.
const maxPages = 20

// get fetches path (with query) and decodes the JSON body into out. It
// returns the HTTP status alongside any error, so a caller can tell a 404
// from other failures.
func (c *apiClient) get(path string, query url.Values, out any) (int, error) {
	if c.provider == "google" {
		if query == nil {
			query = url.Values{}
		}
		query.Set("key", c.key)
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return 0, errors.New("building request failed")
	}
	switch c.provider {
	case "anthropic":
		req.Header.Set("x-api-key", c.key)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "google":
	default:
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// *url.Error repeats the URL; keep only the underlying cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, fmt.Errorf("GET %s: %s", path, c.redact(err.Error()))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("GET %s: reading body: %s", path, c.redact(err.Error()))
	}
	if resp.StatusCode != http.StatusOK {
		// Redact before truncating, so a cut cannot split the key.
		snippet := c.redact(strings.Join(strings.Fields(string(body)), " "))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		return resp.StatusCode, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, snippet)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return resp.StatusCode, fmt.Errorf("GET %s: decoding JSON: %s", path, c.redact(err.Error()))
	}
	return resp.StatusCode, nil
}

// redact removes the key from s, in case a provider echoes it.
func (c *apiClient) redact(s string) string {
	if c.key == "" {
		return s
	}
	s = strings.ReplaceAll(s, c.key, "[redacted]")
	return strings.ReplaceAll(s, url.QueryEscape(c.key), "[redacted]")
}

func newClient(p providerSpec, key string) *apiClient {
	return &apiClient{
		http:     &http.Client{Timeout: 30 * time.Second},
		provider: p.Name,
		baseURL:  strings.TrimRight(p.BaseURL, "/"),
		key:      key,
	}
}

// fineTuned reports a user's fine-tuned model, which the catalog never has.
func fineTuned(id string) bool { return strings.HasPrefix(id, "ft:") }

// listOpenAIStyle reads GET /v1/models returning {"data":[{"id":...}]}, as
// OpenAI, xAI, and Meta do.
func listOpenAIStyle(c *apiClient) ([]listedModel, error) {
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if _, err := c.get("/v1/models", nil, &resp); err != nil {
		return nil, err
	}
	var out []listedModel
	for _, m := range resp.Data {
		if m.ID != "" && !fineTuned(m.ID) {
			out = append(out, listedModel{ID: m.ID})
		}
	}
	return out, nil
}

func listAnthropic(c *apiClient) ([]listedModel, error) {
	var out []listedModel
	after := ""
	for range maxPages {
		q := url.Values{"limit": {"1000"}}
		if after != "" {
			q.Set("after_id", after)
		}
		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if _, err := c.get("/v1/models", q, &resp); err != nil {
			return nil, err
		}
		for _, m := range resp.Data {
			if m.ID != "" && !fineTuned(m.ID) {
				out = append(out, listedModel{ID: m.ID})
			}
		}
		if !resp.HasMore || resp.LastID == "" {
			break
		}
		after = resp.LastID
	}
	return out, nil
}

// anthropicExists asks GET /v1/models/{id}, which resolves aliases the list
// leaves out. The call is free.
func anthropicExists(c *apiClient, id string) (bool, error) {
	var resp struct {
		ID string `json:"id"`
	}
	status, err := c.get("/v1/models/"+url.PathEscape(id), nil, &resp)
	if status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

func listMistral(c *apiClient) ([]listedModel, error) {
	var resp struct {
		Data []struct {
			ID           string   `json:"id"`
			Aliases      []string `json:"aliases"`
			Type         string   `json:"type"`
			Capabilities *struct {
				CompletionChat *bool `json:"completion_chat"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if _, err := c.get("/v1/models", nil, &resp); err != nil {
		return nil, err
	}
	var out []listedModel
	for _, m := range resp.Data {
		if m.ID == "" || fineTuned(m.ID) || strings.Contains(m.Type, "fine-tuned") {
			continue
		}
		notText := m.Capabilities != nil && m.Capabilities.CompletionChat != nil &&
			!*m.Capabilities.CompletionChat
		out = append(out, listedModel{ID: m.ID, Aliases: m.Aliases, NotText: notText})
	}
	return out, nil
}

func listGoogle(c *apiClient) ([]listedModel, error) {
	var out []listedModel
	token := ""
	for range maxPages {
		q := url.Values{"pageSize": {"1000"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var resp struct {
			Models []struct {
				Name    string   `json:"name"`
				Methods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
		}
		if _, err := c.get("/v1beta/models", q, &resp); err != nil {
			return nil, err
		}
		for _, m := range resp.Models {
			id := strings.TrimPrefix(m.Name, "models/")
			if id == "" {
				continue
			}
			notText := len(m.Methods) > 0 && !slices.Contains(m.Methods, "generateContent")
			out = append(out, listedModel{ID: id, NotText: notText})
		}
		if resp.NextPageToken == "" {
			break
		}
		token = resp.NextPageToken
	}
	return out, nil
}
