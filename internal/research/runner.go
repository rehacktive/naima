package research

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"naima/internal/pkb"
)

const (
	defaultTimeout          = 10 * time.Minute
	defaultSummaryTimeout   = 3 * time.Minute
	defaultSources          = 6
	maxSources              = 10
	defaultQueries          = 5
	maxQueries              = 8
	defaultDocWindow        = 2200
	defaultSearchLimit      = 6
	defaultNewsDigestLimit  = 6
	defaultWebSearchTimeout = 12 * time.Second
	defaultNewsTimeout      = 15 * time.Second
	defaultHTTPTimeout      = 15 * time.Second
)

type ChatCompleter interface {
	CreateChatCompletion(ctx context.Context, request openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error)
}

type Runner struct {
	store      PKBStore
	client     ChatCompleter
	chatModel  string
	ingestCfg  pkb.IngestConfig
	searxURL   string
	httpClient *http.Client
}

type ExecuteRequest struct {
	Topic      string
	Note       string
	GuideTitle string
	Language   string
	TimeRange  string
	MaxSources int
	MaxQueries int
}

type ExecuteResult struct {
	Topic            pkb.Topic        `json:"topic"`
	GuideDocument    pkb.Document     `json:"guide_document"`
	ResponseDocument pkb.Document     `json:"response_document"`
	SourceDocuments  []pkb.Document   `json:"source_documents"`
	Queries          []QueryPlan      `json:"queries"`
	Searches         []map[string]any `json:"searches"`
}

type QueryPlan struct {
	Query     string `json:"query"`
	Type      string `json:"type"`
	TimeRange string `json:"time_range,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
	Engine  string `json:"engine"`
}

type newsItem struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Source      string `json:"source"`
	Snippet     string `json:"snippet"`
	PublishedAt string `json:"published_at,omitempty"`
}

type candidate struct {
	Title      string `json:"title"`
	URL        string `json:"url"`
	Snippet    string `json:"snippet"`
	Source     string `json:"source,omitempty"`
	Query      string `json:"query"`
	QueryType  string `json:"query_type"`
	TimeRange  string `json:"time_range,omitempty"`
	Why        string `json:"why,omitempty"`
	Preference int    `json:"preference,omitempty"`
}

type planResponse struct {
	Objective string      `json:"objective"`
	Queries   []QueryPlan `json:"queries"`
}

type selectionResponse struct {
	URLs []string `json:"urls"`
}

type documentValidation struct {
	Accept bool   `json:"accept"`
	Reason string `json:"reason,omitempty"`
}

type supplementalQueries struct {
	Queries []QueryPlan `json:"queries"`
}

type searxSearchResponse struct {
	Results []searchResult `json:"results"`
}

type searxNewsResponse struct {
	Results []struct {
		Title         string `json:"title"`
		URL           string `json:"url"`
		Content       string `json:"content"`
		Engine        string `json:"engine"`
		PublishedDate string `json:"publishedDate"`
	} `json:"results"`
}

func NewRunner(store PKBStore, client ChatCompleter, chatModel string, ingestCfg pkb.IngestConfig, searxURL string) *Runner {
	return &Runner{
		store:      store,
		client:     client,
		chatModel:  strings.TrimSpace(chatModel),
		ingestCfg:  ingestCfg,
		searxURL:   strings.TrimRight(strings.TrimSpace(searxURL), "/"),
		httpClient: &http.Client{Timeout: defaultHTTPTimeout},
	}
}

func (r *Runner) Execute(ctx context.Context, req ExecuteRequest, emit func(string)) (ExecuteResult, error) {
	if emit == nil {
		emit = func(string) {}
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	req.Topic = strings.TrimSpace(req.Topic)
	req.Note = strings.TrimSpace(req.Note)
	if req.Topic == "" {
		return ExecuteResult{}, fmt.Errorf("topic is required")
	}
	if req.Note == "" {
		return ExecuteResult{}, fmt.Errorf("note is required")
	}
	if req.MaxSources <= 0 {
		req.MaxSources = defaultSources
	}
	if req.MaxSources > maxSources {
		req.MaxSources = maxSources
	}
	if req.MaxQueries <= 0 {
		req.MaxQueries = defaultQueries
	}
	if req.MaxQueries > maxQueries {
		req.MaxQueries = maxQueries
	}

	emit("ensuring topic")
	topic, err := r.ensureTopic(ctx, req.Topic)
	if err != nil {
		return ExecuteResult{}, err
	}

	guideTitle := strings.TrimSpace(req.GuideTitle)
	if guideTitle == "" {
		guideTitle = "Research Brief"
	}
	emit("creating research brief document")
	guideDoc, err := r.store.CreateDocument(ctx, pkb.CreateDocumentRequest{
		TopicID:      topic.ID,
		Kind:         "note",
		Title:        guideTitle,
		IngestMethod: "deep_research_brief",
		Content:      pkb.TruncateRunes(req.Note, pkb.MaxStoredContentRunes),
	})
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("create research brief failed: %w", err)
	}

	emit("building research plan")
	plan, err := r.buildPlan(ctx, req)
	if err != nil {
		return ExecuteResult{}, err
	}

	emit(fmt.Sprintf("running %d search queries", len(plan.Queries)))
	candidates, searches, err := r.collectCandidates(ctx, plan.Queries, req.Language)
	if err != nil {
		return ExecuteResult{}, err
	}

	selected := r.fallbackSelection(candidates, req.MaxSources)
	if len(candidates) > 0 {
		emit("selecting source candidates")
		llmSelected, selErr := r.selectCandidates(ctx, req.Topic, req.Note, candidates, req.MaxSources)
		if selErr == nil && len(llmSelected) > 0 {
			selected = llmSelected
		}
	}

	emit(fmt.Sprintf("collecting up to %d source documents", req.MaxSources))
	sourceDocs, finalSearches, err := r.collectEnoughSources(ctx, topic.ID, req, plan.Queries, searches, candidates, selected, emit)
	if err != nil {
		return ExecuteResult{}, err
	}

	emit("writing final response document")
	summaryCtx, summaryCancel := r.summaryContext(ctx)
	defer summaryCancel()
	responseContent, err := r.buildResponseDocument(summaryCtx, req.Topic, req.Note, plan.Queries, finalSearches, sourceDocs)
	if err != nil {
		return ExecuteResult{}, err
	}
	responseDoc, err := r.store.CreateDocument(ctx, pkb.CreateDocumentRequest{
		TopicID:      topic.ID,
		Kind:         "note",
		Title:        "Deep Research Response",
		IngestMethod: "deep_research_response",
		Content:      pkb.TruncateRunes(responseContent, pkb.MaxStoredContentRunes),
	})
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("create research response failed: %w", err)
	}

	return ExecuteResult{
		Topic:            topic,
		GuideDocument:    guideDoc,
		ResponseDocument: responseDoc,
		SourceDocuments:  sourceDocs,
		Queries:          plan.Queries,
		Searches:         finalSearches,
	}, nil
}

func (r *Runner) collectEnoughSources(ctx context.Context, topicID int64, req ExecuteRequest, baseQueries []QueryPlan, searches []map[string]any, candidates []candidate, selected []candidate, emit func(string)) ([]pkb.Document, []map[string]any, error) {
	target := req.MaxSources
	if target <= 0 {
		target = defaultSources
	}

	used := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		used[strings.ToLower(strings.TrimSpace(c.URL))] = struct{}{}
	}

	pool := append([]candidate(nil), selected...)
	appendUnusedCandidates := func(items []candidate) {
		index := make(map[string]struct{}, len(pool))
		for _, item := range pool {
			index[strings.ToLower(strings.TrimSpace(item.URL))] = struct{}{}
		}
		for _, item := range items {
			key := strings.ToLower(strings.TrimSpace(item.URL))
			if key == "" {
				continue
			}
			if _, ok := index[key]; ok {
				continue
			}
			pool = append(pool, item)
			index[key] = struct{}{}
		}
	}
	appendUnusedCandidates(candidates)

	sourceDocs, acceptedURLs, err := r.ingestSelected(ctx, topicID, req.Topic, req.Note, pool, target, emit)
	if err != nil {
		return nil, searches, err
	}
	if len(sourceDocs) >= target {
		return sourceDocs, searches, nil
	}

	const maxSupplementalRounds = 2
	for round := 1; round <= maxSupplementalRounds && len(sourceDocs) < target; round++ {
		emit(fmt.Sprintf("source backfill round %d: generating more search queries", round))
		extraQueries, err := r.generateSupplementalQueries(ctx, req, baseQueries, searches, sourceDocs, target-len(sourceDocs))
		if err != nil || len(extraQueries) == 0 {
			break
		}
		extraCandidates, extraSearches, err := r.collectCandidates(ctx, extraQueries, req.Language)
		if err != nil {
			break
		}
		searches = append(searches, extraSearches...)
		filtered := make([]candidate, 0, len(extraCandidates))
		for _, c := range extraCandidates {
			key := strings.ToLower(strings.TrimSpace(c.URL))
			if key == "" {
				continue
			}
			if _, ok := used[key]; ok {
				continue
			}
			if _, ok := acceptedURLs[key]; ok {
				continue
			}
			used[key] = struct{}{}
			filtered = append(filtered, c)
		}
		if len(filtered) == 0 {
			continue
		}
		emit(fmt.Sprintf("source backfill round %d: selecting from %d new candidates", round, len(filtered)))
		chosen := r.fallbackSelection(filtered, target-len(sourceDocs))
		if llmChosen, err := r.selectCandidates(ctx, req.Topic, req.Note, filtered, target-len(sourceDocs)); err == nil && len(llmChosen) > 0 {
			chosen = llmChosen
		}
		newDocs, newAccepted, err := r.ingestSelected(ctx, topicID, req.Topic, req.Note, chosen, target-len(sourceDocs), emit)
		if err != nil {
			return nil, searches, err
		}
		for url := range newAccepted {
			acceptedURLs[url] = struct{}{}
		}
		sourceDocs = append(sourceDocs, newDocs...)
	}

	return sourceDocs, searches, nil
}

func (r *Runner) ensureTopic(ctx context.Context, title string) (pkb.Topic, error) {
	topic, err := r.store.CreateTopic(ctx, title)
	if err == nil {
		return topic, nil
	}
	if !strings.Contains(strings.ToLower(err.Error()), "topic already exists") {
		return pkb.Topic{}, err
	}
	topics, listErr := r.store.ListTopics(ctx)
	if listErr != nil {
		return pkb.Topic{}, err
	}
	for _, topic := range topics {
		if strings.EqualFold(strings.TrimSpace(topic.Title), title) {
			return topic, nil
		}
	}
	return pkb.Topic{}, err
}

func (r *Runner) buildPlan(ctx context.Context, req ExecuteRequest) (planResponse, error) {
	prompt := fmt.Sprintf(
		"Topic: %s\nTime range hint: %s\nLanguage: %s\nMax queries: %d\n\nResearch brief:\n%s\n\nReturn JSON only with this shape: {\"objective\":\"...\",\"queries\":[{\"query\":\"...\",\"type\":\"web|news\",\"time_range\":\"day|week|month|year|\",\"reason\":\"...\"}]}. Use %d queries or fewer.",
		req.Topic, req.TimeRange, req.Language, req.MaxQueries, req.Note, req.MaxQueries,
	)
	var plan planResponse
	if err := r.jsonCompletion(ctx, "You produce structured research plans. Respond with JSON only.", prompt, &plan); err != nil {
		return planResponse{}, fmt.Errorf("build research plan failed: %w", err)
	}
	queries := make([]QueryPlan, 0, len(plan.Queries))
	for _, q := range plan.Queries {
		q.Query = strings.TrimSpace(q.Query)
		if q.Query == "" {
			continue
		}
		q.Type = strings.ToLower(strings.TrimSpace(q.Type))
		if q.Type != "news" {
			q.Type = "web"
		}
		if q.TimeRange == "" {
			q.TimeRange = strings.TrimSpace(req.TimeRange)
		}
		queries = append(queries, q)
		if len(queries) >= req.MaxQueries {
			break
		}
	}
	if len(queries) == 0 {
		queries = append(queries, QueryPlan{Query: req.Topic + " overview", Type: "web", TimeRange: req.TimeRange, Reason: "fallback overview query"})
	}
	plan.Queries = queries
	return plan, nil
}

func (r *Runner) collectCandidates(ctx context.Context, queries []QueryPlan, language string) ([]candidate, []map[string]any, error) {
	candidates := make([]candidate, 0, 32)
	searches := make([]map[string]any, 0, len(queries))
	seen := make(map[string]struct{})

	for _, q := range queries {
		switch q.Type {
		case "news":
			items, err := r.newsSearch(ctx, q, language)
			if err != nil {
				continue
			}
			searches = append(searches, map[string]any{"query": q.Query, "type": q.Type, "time_range": q.TimeRange, "count": len(items)})
			for idx, item := range items {
				key := strings.ToLower(strings.TrimSpace(item.URL))
				if key == "" {
					continue
				}
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				candidates = append(candidates, candidate{
					Title: item.Title, URL: item.URL, Snippet: item.Snippet, Source: item.Source,
					Query: q.Query, QueryType: "news", TimeRange: q.TimeRange, Why: q.Reason, Preference: idx + 1,
				})
			}
		default:
			results, err := r.webSearch(ctx, q, language)
			if err != nil {
				continue
			}
			searches = append(searches, map[string]any{"query": q.Query, "type": q.Type, "time_range": q.TimeRange, "count": len(results)})
			for idx, item := range results {
				key := strings.ToLower(strings.TrimSpace(item.URL))
				if key == "" {
					continue
				}
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				candidates = append(candidates, candidate{
					Title: item.Title, URL: item.URL, Snippet: item.Content, Source: item.Engine,
					Query: q.Query, QueryType: "web", TimeRange: q.TimeRange, Why: q.Reason, Preference: idx + 1,
				})
			}
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].QueryType != candidates[j].QueryType {
			return candidates[i].QueryType == "web"
		}
		return candidates[i].Preference < candidates[j].Preference
	})
	return candidates, searches, nil
}

func (r *Runner) ingestSelected(ctx context.Context, topicID int64, topic string, brief string, selected []candidate, maxNeeded int, emit func(string)) ([]pkb.Document, map[string]struct{}, error) {
	existingDocs, err := r.store.ListDocuments(ctx, topicID)
	if err != nil {
		return nil, nil, fmt.Errorf("list topic documents failed: %w", err)
	}
	existingByURL := make(map[string]pkb.Document, len(existingDocs))
	for _, doc := range existingDocs {
		key := strings.TrimSpace(strings.ToLower(doc.SourceURL))
		if key != "" {
			existingByURL[key] = doc
		}
	}

	sourceDocs := make([]pkb.Document, 0, len(selected))
	accepted := make(map[string]struct{}, len(selected))
	for _, item := range selected {
		if maxNeeded > 0 && len(sourceDocs) >= maxNeeded {
			break
		}
		key := strings.ToLower(strings.TrimSpace(item.URL))
		if existing, ok := existingByURL[key]; ok {
			sourceDocs = append(sourceDocs, existing)
			accepted[key] = struct{}{}
			continue
		}
		emit("ingesting " + item.URL)
		ingested, err := pkb.IngestURLContent(ctx, r.httpClient, r.ingestCfg, item.URL)
		if err != nil {
			emit("ingest skipped for " + item.URL + ": " + err.Error())
			continue
		}
		title := strings.TrimSpace(ingested.Title)
		if title == "" {
			title = item.Title
		}
		content := pkb.TruncateRunes(strings.TrimSpace(ingested.Content), pkb.MaxStoredContentRunes)
		ok, reason := r.shouldKeepDocument(ctx, topic, brief, title, item.URL, content)
		if !ok {
			emit("document skipped for " + item.URL + ": " + reason)
			continue
		}
		doc, err := r.store.CreateDocument(ctx, pkb.CreateDocumentRequest{
			TopicID:      topicID,
			Kind:         "url",
			Title:        pkb.TruncateRunes(title, 240),
			SourceURL:    item.URL,
			IngestMethod: strings.TrimSpace(ingested.Method + "+deep_research"),
			Content:      content,
		})
		if err != nil {
			emit("document create skipped for " + item.URL + ": " + err.Error())
			continue
		}
		sourceDocs = append(sourceDocs, doc)
		accepted[key] = struct{}{}
		existingByURL[key] = doc
	}
	return sourceDocs, accepted, nil
}

func (r *Runner) buildResponseDocument(ctx context.Context, topic string, brief string, queries []QueryPlan, searches []map[string]any, docs []pkb.Document) (string, error) {
	sourceIndex := make([]string, 0, len(docs))
	docSections := make([]string, 0, len(docs))
	for i, doc := range docs {
		ref := fmt.Sprintf("S%d", i+1)
		sourceIndex = append(sourceIndex, fmt.Sprintf("- [%s] %s | %s | PKB document #%d", ref, doc.Title, doc.SourceURL, doc.ID))
		docSections = append(docSections, fmt.Sprintf("[%s] %s\nURL: %s\nPKB document: #%d\nContent:\n%s",
			ref, doc.Title, doc.SourceURL, doc.ID, pkb.TruncateRunes(strings.TrimSpace(doc.Content), defaultDocWindow)))
	}
	if len(docSections) == 0 {
		docSections = append(docSections, "No source documents were ingested successfully.")
	}

	queryLines := make([]string, 0, len(queries))
	for _, q := range queries {
		queryLines = append(queryLines, fmt.Sprintf("- %s (%s, %s)", q.Query, q.Type, firstNonEmpty(q.TimeRange, "any time")))
	}

	prompt := fmt.Sprintf(
		"Topic: %s\nResearch brief:\n%s\n\nResearch plan:\n%s\n\nSearch execution summary:\n%s\n\nAvailable source documents:\n%s\n\nWrite a concise markdown research summary with these sections: Overview, Key Findings, Open Questions, Recommended Next Steps. Cite claims inline using [S1], [S2] style only when supported by the provided source documents.",
		topic, brief, strings.Join(queryLines, "\n"), mustJSON(searches), strings.Join(docSections, "\n\n"),
	)
	summary, err := r.textCompletion(ctx, "You synthesize research findings into concise markdown with source citations.", prompt)
	if err != nil {
		return "", fmt.Errorf("build research summary failed: %w", err)
	}
	var out strings.Builder
	out.WriteString(strings.TrimSpace(summary))
	out.WriteString("\n\n## Sources\n")
	if len(sourceIndex) == 0 {
		out.WriteString("- No source documents were stored.\n")
	} else {
		for _, line := range sourceIndex {
			out.WriteString(line)
			out.WriteString("\n")
		}
	}
	return strings.TrimSpace(out.String()), nil
}

func (r *Runner) shouldKeepDocument(ctx context.Context, topic string, brief string, title string, rawURL string, content string) (bool, string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return false, "empty content"
	}
	if len(content) < 300 {
		return false, "content too short or malformed"
	}
	wordCount := len(strings.Fields(content))
	if wordCount < 60 {
		return false, "content too sparse or malformed"
	}
	lower := strings.ToLower(content)
	badMarkers := []string{
		"enable javascript",
		"access denied",
		"captcha",
		"sign in to continue",
		"page not found",
		"404 not found",
	}
	for _, marker := range badMarkers {
		if strings.Contains(lower, marker) {
			return false, "parsed page looks malformed or blocked"
		}
	}

	snippet := pkb.TruncateRunes(content, 2500)
	var verdict documentValidation
	prompt := fmt.Sprintf(
		"Research topic: %s\nResearch brief:\n%s\n\nCandidate document:\nTitle: %s\nURL: %s\nContent excerpt:\n%s\n\nReturn JSON only as {\"accept\":true|false,\"reason\":\"...\"}. Accept only if the document is coherent and materially relevant to the research scope. Reject malformed, navigation-only, blocked, spammy, or unrelated pages.",
		topic,
		brief,
		title,
		rawURL,
		snippet,
	)
	if err := r.jsonCompletion(ctx, "You validate whether scraped documents should be kept for a research knowledge base. Respond with JSON only.", prompt, &verdict); err != nil {
		return true, "validation fallback accepted"
	}
	if !verdict.Accept {
		reason := strings.TrimSpace(verdict.Reason)
		if reason == "" {
			reason = "not relevant to research scope"
		}
		return false, reason
	}
	return true, "accepted"
}

func (r *Runner) generateSupplementalQueries(ctx context.Context, req ExecuteRequest, baseQueries []QueryPlan, searches []map[string]any, docs []pkb.Document, needed int) ([]QueryPlan, error) {
	if needed <= 0 {
		return nil, nil
	}
	baseLines := make([]string, 0, len(baseQueries))
	for _, q := range baseQueries {
		baseLines = append(baseLines, fmt.Sprintf("- %s (%s)", q.Query, q.Type))
	}
	sourceLines := make([]string, 0, len(docs))
	for i, doc := range docs {
		sourceLines = append(sourceLines, fmt.Sprintf("- %d. %s | %s", i+1, doc.Title, doc.SourceURL))
	}
	var resp supplementalQueries
	prompt := fmt.Sprintf(
		"Topic: %s\nResearch brief:\n%s\n\nExisting queries:\n%s\n\nExecuted searches summary:\n%s\n\nAccepted source documents so far:\n%s\n\nNeed %d additional source candidates. Return JSON only as {\"queries\":[{\"query\":\"...\",\"type\":\"web|news\",\"time_range\":\"day|week|month|year|\",\"reason\":\"...\"}]}. Prefer queries that explore missing angles or alternative wording, not near-duplicates of existing queries.",
		req.Topic,
		req.Note,
		strings.Join(baseLines, "\n"),
		mustJSON(searches),
		strings.Join(sourceLines, "\n"),
		needed,
	)
	if err := r.jsonCompletion(ctx, "You generate supplemental research queries. Respond with JSON only.", prompt, &resp); err != nil {
		return nil, err
	}
	out := make([]QueryPlan, 0, len(resp.Queries))
	for _, q := range resp.Queries {
		q.Query = strings.TrimSpace(q.Query)
		if q.Query == "" {
			continue
		}
		q.Type = strings.ToLower(strings.TrimSpace(q.Type))
		if q.Type != "news" {
			q.Type = "web"
		}
		if q.TimeRange == "" {
			q.TimeRange = req.TimeRange
		}
		out = append(out, q)
		if len(out) >= min(needed*2, 4) {
			break
		}
	}
	return out, nil
}

func (r *Runner) summaryContext(parent context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return context.WithCancel(parent)
		}
		if remaining < defaultSummaryTimeout {
			// Still carve out a bounded child context so the final stage fails fast and predictably.
			return context.WithTimeout(parent, remaining)
		}
		return context.WithTimeout(parent, defaultSummaryTimeout)
	}
	return context.WithTimeout(parent, defaultSummaryTimeout)
}

func (r *Runner) selectCandidates(ctx context.Context, topic string, brief string, candidates []candidate, maxSources int) ([]candidate, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	if len(candidates) > 24 {
		candidates = candidates[:24]
	}
	var builder strings.Builder
	for i, c := range candidates {
		builder.WriteString(strconv.Itoa(i + 1))
		builder.WriteString(". ")
		builder.WriteString(c.Title)
		builder.WriteString(" | ")
		builder.WriteString(c.URL)
		builder.WriteString(" | ")
		builder.WriteString(c.QueryType)
		builder.WriteString(" | query=")
		builder.WriteString(c.Query)
		if c.Snippet != "" {
			builder.WriteString(" | snippet=")
			builder.WriteString(truncateText(c.Snippet, 240))
		}
		builder.WriteString("\n")
	}
	var sel selectionResponse
	prompt := fmt.Sprintf("Topic: %s\nBrief:\n%s\n\nCandidate sources:\n%s\nSelect up to %d sources that are the most relevant, credible, and non-duplicative. Return JSON only as {\"urls\":[\"...\"]}.", topic, brief, builder.String(), maxSources)
	if err := r.jsonCompletion(ctx, "You select source URLs for research. Respond with JSON only.", prompt, &sel); err != nil {
		return nil, err
	}
	index := make(map[string]candidate, len(candidates))
	for _, c := range candidates {
		index[strings.ToLower(strings.TrimSpace(c.URL))] = c
	}
	out := make([]candidate, 0, maxSources)
	for _, rawURL := range sel.URLs {
		c, ok := index[strings.ToLower(strings.TrimSpace(rawURL))]
		if !ok {
			continue
		}
		out = append(out, c)
		if len(out) >= maxSources {
			break
		}
	}
	return out, nil
}

func (r *Runner) fallbackSelection(candidates []candidate, maxSources int) []candidate {
	out := make([]candidate, 0, maxSources)
	for _, c := range candidates {
		out = append(out, c)
		if len(out) >= maxSources {
			break
		}
	}
	return out
}

func (r *Runner) webSearch(ctx context.Context, query QueryPlan, language string) ([]searchResult, error) {
	if r.searxURL == "" {
		return nil, fmt.Errorf("searx base url is not configured")
	}
	u, err := url.Parse(r.searxURL + "/search")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("q", query.Query)
	q.Set("format", "json")
	q.Set("categories", "web")
	if strings.TrimSpace(language) != "" {
		q.Set("language", strings.TrimSpace(language))
	}
	if tr := normalizeTimeRange(query.TimeRange); tr != "" {
		q.Set("time_range", tr)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: defaultWebSearchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("web search returned status %d", resp.StatusCode)
	}
	var out searxSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Results) > defaultSearchLimit {
		out.Results = out.Results[:defaultSearchLimit]
	}
	return out.Results, nil
}

func (r *Runner) newsSearch(ctx context.Context, query QueryPlan, language string) ([]newsItem, error) {
	if r.searxURL == "" {
		return nil, fmt.Errorf("searx base url is not configured")
	}
	u, err := url.Parse(r.searxURL + "/search")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("q", query.Query)
	q.Set("format", "json")
	q.Set("categories", "news")
	if strings.TrimSpace(language) != "" {
		q.Set("language", strings.TrimSpace(language))
	}
	if tr := normalizeNewsTimeRange(query.TimeRange); tr != "" {
		q.Set("time_range", tr)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: defaultNewsTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("news search returned status %d", resp.StatusCode)
	}
	var out searxNewsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	items := make([]newsItem, 0, len(out.Results))
	seen := make(map[string]struct{})
	for _, item := range out.Results {
		key := strings.ToLower(strings.TrimSpace(item.URL))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		snippet := strings.TrimSpace(item.Content)
		if len(snippet) > 280 {
			snippet = snippet[:280] + "..."
		}
		items = append(items, newsItem{
			Title: strings.TrimSpace(item.Title), URL: strings.TrimSpace(item.URL), Source: extractDomain(item.URL),
			Snippet: snippet, PublishedAt: strings.TrimSpace(item.PublishedDate),
		})
		if len(items) >= defaultNewsDigestLimit {
			break
		}
	}
	return items, nil
}

func (r *Runner) jsonCompletion(ctx context.Context, systemPrompt string, userPrompt string, out any) error {
	text, err := r.textCompletion(ctx, systemPrompt, userPrompt)
	if err != nil {
		return err
	}
	clean := strings.TrimSpace(text)
	clean = strings.TrimPrefix(clean, "```json")
	clean = strings.TrimPrefix(clean, "```")
	clean = strings.TrimSuffix(clean, "```")
	clean = strings.TrimSpace(clean)
	return json.Unmarshal([]byte(clean), out)
}

func (r *Runner) textCompletion(ctx context.Context, systemPrompt string, userPrompt string) (string, error) {
	resp, err := r.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: r.chatModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: strings.TrimSpace(systemPrompt)},
			{Role: openai.ChatMessageRoleUser, Content: strings.TrimSpace(userPrompt)},
		},
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices")
	}
	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	if content == "" {
		return "", fmt.Errorf("llm returned empty content")
	}
	return content, nil
}

func normalizeTimeRange(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "day", "month", "year":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func normalizeNewsTimeRange(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "day":
		return "day"
	case "week", "month":
		return "month"
	case "year":
		return "year"
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncateText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return strings.TrimSpace(value[:limit]) + "..."
}

func extractDomain(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Host), "www.")
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}
