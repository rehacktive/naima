package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	defaultNewsDigestTimeout = 15 * time.Second
	defaultNewsDigestLimit   = 6
	maxNewsDigestLimit       = 15
)

type NewsDigestTool struct {
	baseURL string
	client  *http.Client
}

type newsDigestParams struct {
	Topic     string `json:"topic"`
	Region    string `json:"region,omitempty"`
	TimeRange string `json:"time_range,omitempty"`
	Language  string `json:"language,omitempty"`
	MaxItems  int    `json:"max_items,omitempty"`
}

type searxNewsResponse struct {
	Results []searxNewsResult `json:"results"`
}

type searxNewsResult struct {
	Title         string `json:"title"`
	URL           string `json:"url"`
	Content       string `json:"content"`
	Engine        string `json:"engine"`
	PublishedDate string `json:"publishedDate"`
}

type newsItem struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Source      string `json:"source"`
	Snippet     string `json:"snippet"`
	PublishedAt string `json:"published_at,omitempty"`
}

func NewNewsDigestTool(baseURL string) Tool {
	return &NewsDigestTool{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		client:  &http.Client{Timeout: defaultNewsDigestTimeout},
	}
}

func (t *NewsDigestTool) GetName() string {
	return "news_digest"
}

func (t *NewsDigestTool) GetDescription() string {
	return "Builds a concise news digest for a topic from SearxNG news results."
}

func (t *NewsDigestTool) GetFunction() func(params string) string {
	return func(params string) string {
		var in newsDigestParams
		if err := json.Unmarshal([]byte(params), &in); err != nil {
			return errorJSON("invalid params: " + err.Error())
		}

		topic := strings.TrimSpace(in.Topic)
		if topic == "" {
			return errorJSON("topic is required")
		}
		if t.baseURL == "" {
			return errorJSON("searx base url is not configured")
		}

		maxItems := in.MaxItems
		if maxItems <= 0 {
			maxItems = defaultNewsDigestLimit
		}
		if maxItems > maxNewsDigestLimit {
			maxItems = maxNewsDigestLimit
		}

		query := topic
		if region := strings.TrimSpace(in.Region); region != "" {
			query = query + " " + region
		}

		searchURL := t.baseURL + "/search"
		u, err := url.Parse(searchURL)
		if err != nil {
			return errorJSON("invalid searx url: " + err.Error())
		}
		q := u.Query()
		q.Set("q", query)
		q.Set("format", "json")
		q.Set("categories", "news")
		if lang := strings.TrimSpace(in.Language); lang != "" {
			q.Set("language", lang)
		}
		if tr := normalizeNewsDigestTimeRange(in.TimeRange); tr != "" {
			q.Set("time_range", tr)
		}
		u.RawQuery = q.Encode()

		req, err := http.NewRequest(http.MethodGet, u.String(), nil)
		if err != nil {
			return errorJSON("build request failed: " + err.Error())
		}
		resp, err := t.client.Do(req)
		if err != nil {
			return errorJSON("news search request failed: " + err.Error())
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return errorJSON(fmt.Sprintf("news search returned status %d", resp.StatusCode))
		}

		var out searxNewsResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return errorJSON("decode news search response failed: " + err.Error())
		}

		items := normalizeNewsItems(out.Results, topic)
		if len(items) > maxItems {
			items = items[:maxItems]
		}

		digest := buildDigest(topic, strings.TrimSpace(in.Region), items)
		payload := map[string]any{
			"topic":      topic,
			"region":     strings.TrimSpace(in.Region),
			"time_range": normalizeNewsDigestTimeRange(in.TimeRange),
			"language":   strings.TrimSpace(in.Language),
			"max_items":  maxItems,
			"count":      len(items),
			"digest":     digest,
			"items":      items,
			"generated":  time.Now().UTC().Format(time.RFC3339),
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return errorJSON("serialize news digest failed: " + err.Error())
		}
		return string(data)
	}
}

func (t *NewsDigestTool) IsImmediate() bool {
	return false
}

func (t *NewsDigestTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"topic": map[string]any{
				"type":        "string",
				"description": "News topic to summarize.",
			},
			"region": map[string]any{
				"type":        "string",
				"description": "Optional region/country focus (for example: US, EU, Italy).",
			},
			"time_range": map[string]any{
				"type":        "string",
				"description": "Optional recency window.",
				"enum":        []string{"day", "week", "month", "year"},
			},
			"language": map[string]any{
				"type":        "string",
				"description": "Optional Searx language code, e.g. en-US.",
			},
			"max_items": map[string]any{
				"type":        "integer",
				"description": "Optional max number of headlines (1-15).",
				"minimum":     1,
				"maximum":     15,
			},
		},
		Required: []string{"topic"},
	}
}

func normalizeNewsDigestTimeRange(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "day":
		return "day"
	case "week", "month":
		// Searx supports day/month/year; week maps to month for best-effort recency filtering.
		return "month"
	case "year":
		return "year"
	default:
		return ""
	}
}

func normalizeNewsItems(results []searxNewsResult, topic string) []newsItem {
	if len(results) == 0 {
		return nil
	}

	type scored struct {
		item  newsItem
		score int
	}
	topicWords := tokenize(topic)
	seen := make(map[string]struct{}, len(results))
	scoredItems := make([]scored, 0, len(results))

	for _, r := range results {
		title := strings.TrimSpace(r.Title)
		link := strings.TrimSpace(r.URL)
		if title == "" || link == "" {
			continue
		}
		key := strings.ToLower(link)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		src := extractDomain(link)
		snippet := strings.TrimSpace(r.Content)
		if len(snippet) > 280 {
			snippet = snippet[:280] + "..."
		}

		item := newsItem{
			Title:       title,
			URL:         link,
			Source:      src,
			Snippet:     snippet,
			PublishedAt: strings.TrimSpace(r.PublishedDate),
		}
		score := relevanceScore(title+" "+snippet, topicWords)
		if item.PublishedAt != "" {
			score += 5
		}
		scoredItems = append(scoredItems, scored{item: item, score: score})
	}

	sort.SliceStable(scoredItems, func(i, j int) bool {
		return scoredItems[i].score > scoredItems[j].score
	})

	out := make([]newsItem, 0, len(scoredItems))
	for _, s := range scoredItems {
		out = append(out, s.item)
	}
	return out
}

func buildDigest(topic string, region string, items []newsItem) string {
	if len(items) == 0 {
		return "No relevant news items found for the requested topic."
	}

	lines := make([]string, 0, min(4, len(items))+2)
	header := "Top updates for " + topic
	if strings.TrimSpace(region) != "" {
		header += " (" + strings.TrimSpace(region) + ")"
	}
	lines = append(lines, header+":")

	n := min(4, len(items))
	for i := 0; i < n; i++ {
		line := fmt.Sprintf("%d. %s", i+1, items[i].Title)
		if items[i].Source != "" {
			line += " [" + items[i].Source + "]"
		}
		lines = append(lines, line)
	}
	lines = append(lines, "Sources are listed in the items array.")
	return strings.Join(lines, "\n")
}

func tokenize(text string) []string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return nil
	}
	parts := strings.FieldsFunc(text, func(r rune) bool {
		return !(r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) < 3 {
			continue
		}
		out = append(out, p)
	}
	return out
}

func relevanceScore(text string, topicWords []string) int {
	if len(topicWords) == 0 {
		return 0
	}
	lower := strings.ToLower(text)
	score := 0
	for _, w := range topicWords {
		if strings.Contains(lower, w) {
			score += 10
		}
	}
	return score
}

func extractDomain(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	host = strings.TrimPrefix(host, "www.")
	return host
}
