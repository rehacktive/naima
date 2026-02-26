package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultWebSearchTimeout = 12 * time.Second
	defaultWebSearchLimit   = 5
	maxWebSearchLimit       = 8
)

type WebSearchTool struct {
	baseURL string
	client  *http.Client
}

type webSearchParams struct {
	Query    string `json:"query"`
	Language string `json:"language,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

type searxSearchResponse struct {
	Results []searxResult `json:"results"`
}

type searxResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
	Engine  string `json:"engine"`
}

func NewWebSearchTool(baseURL string) Tool {
	return &WebSearchTool{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		client:  &http.Client{Timeout: defaultWebSearchTimeout},
	}
}

func (t *WebSearchTool) GetName() string {
	return "web_search"
}

func (t *WebSearchTool) GetDescription() string {
	return "Searches the web through a local Searx instance and returns top results."
}

func (t *WebSearchTool) GetFunction() func(params string) string {
	return func(params string) string {
		var in webSearchParams
		if err := json.Unmarshal([]byte(params), &in); err != nil {
			return errorJSON("invalid params: " + err.Error())
		}

		in.Query = strings.TrimSpace(in.Query)
		if in.Query == "" {
			return `{"error":"query is required"}`
		}

		limit := in.Limit
		if limit <= 0 {
			limit = defaultWebSearchLimit
		}
		if limit > maxWebSearchLimit {
			limit = maxWebSearchLimit
		}

		if t.baseURL == "" {
			return errorJSON("searx base url is not configured")
		}

		searchURL := t.baseURL + "/search"
		u, err := url.Parse(searchURL)
		if err != nil {
			return errorJSON("invalid searx url: " + err.Error())
		}

		q := u.Query()
		q.Set("q", in.Query)
		q.Set("format", "json")
		if strings.TrimSpace(in.Language) != "" {
			q.Set("language", strings.TrimSpace(in.Language))
		}
		q.Set("safesearch", "1")
		u.RawQuery = q.Encode()

		req, err := http.NewRequest(http.MethodGet, u.String(), nil)
		if err != nil {
			return errorJSON("build request failed: " + err.Error())
		}

		resp, err := t.client.Do(req)
		if err != nil {
			return errorJSON("searx request failed: " + err.Error())
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return errorJSON(fmt.Sprintf("searx returned status %d", resp.StatusCode))
		}

		var out searxSearchResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return errorJSON("decode searx response failed: " + err.Error())
		}

		if len(out.Results) > limit {
			out.Results = out.Results[:limit]
		}

		payload := map[string]any{
			"query":   in.Query,
			"limit":   limit,
			"results": out.Results,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return errorJSON("serialize web search result failed: " + err.Error())
		}

		return string(data)
	}
}

func (t *WebSearchTool) IsImmediate() bool {
	return false
}

func (t *WebSearchTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Search query",
			},
			"language": map[string]any{
				"type":        "string",
				"description": "Optional Searx language code, e.g. en-US",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Optional number of results to return (1-8)",
				"minimum":     1,
				"maximum":     8,
			},
		},
		Required: []string{"query"},
	}
}

func errorJSON(message string) string {
	data, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		return `{"error":"unexpected tool error"}`
	}
	return string(data)
}
