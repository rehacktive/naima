package pkb

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	playwright "github.com/playwright-community/playwright-go"
)

const (
	MaxFetchedContentSize = 2 * 1024 * 1024
	MaxStoredContentRunes = 20000
)

var (
	reScriptBlock    = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyleBlock     = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reHTMLTags       = regexp.MustCompile(`(?is)<[^>]+>`)
	reMultiSpace     = regexp.MustCompile(`\s+`)
	reHTMLTitle      = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reMDHeading      = regexp.MustCompile(`^#{1,6}\s+`)
	reMDBullet       = regexp.MustCompile(`^[-*+]\s+`)
	reMDNumbered     = regexp.MustCompile(`^\d+\.\s+`)
	blockedIPv4CGNAT = netip.MustParsePrefix("100.64.0.0/10")
)

var playwrightInstallOnce sync.Once
var playwrightInstallErr error

type IngestConfig struct {
	Mode                string
	TikaURL             string
	AllowFallback       bool
	PlaywrightHeadless  bool
	PlaywrightTimeoutMS int
}

type IngestResult struct {
	Title        string
	Content      string
	Method       string
	FallbackNote string
}

type extractVariant struct {
	Method  string
	Title   string
	Content string
}

type mergedBlock struct {
	Text      string
	Key       string
	Count     int
	BestRank  int
	BestIndex int
	IsHeading bool
}

func IngestURLContent(ctx context.Context, client *http.Client, cfg IngestConfig, rawURL string) (IngestResult, error) {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == "" {
		mode = "hybrid"
	}
	switch mode {
	case "fetch", "direct", "text":
		title, content, err := FetchURLContent(ctx, client, rawURL)
		if err != nil {
			return IngestResult{}, err
		}
		return IngestResult{Title: title, Content: content, Method: "direct_text"}, nil
	case "tika":
		title, content, err := FetchURLContentViaTika(ctx, client, cfg.TikaURL, rawURL)
		if err == nil {
			return IngestResult{Title: title, Content: content, Method: "tika_markdown"}, nil
		}
		if !cfg.AllowFallback {
			return IngestResult{}, err
		}
		title, content, fallbackErr := FetchURLContent(ctx, client, rawURL)
		if fallbackErr != nil {
			return IngestResult{}, fallbackErr
		}
		return IngestResult{Title: title, Content: content, Method: "fallback_text", FallbackNote: err.Error()}, nil
	case "playwright", "browser":
		title, content, err := FetchURLContentViaPlaywright(ctx, cfg, rawURL)
		if err == nil {
			return IngestResult{Title: title, Content: content, Method: "playwright_markdown"}, nil
		}
		if !cfg.AllowFallback {
			return IngestResult{}, err
		}
		title, content, fallbackErr := FetchURLContent(ctx, client, rawURL)
		if fallbackErr != nil {
			return IngestResult{}, fallbackErr
		}
		return IngestResult{Title: title, Content: content, Method: "fallback_text", FallbackNote: err.Error()}, nil
	case "hybrid":
		return ingestHybrid(ctx, client, cfg, rawURL)
	default:
		return IngestResult{}, fmt.Errorf("unsupported ingest mode: %s", mode)
	}
}

func ingestHybrid(ctx context.Context, client *http.Client, cfg IngestConfig, rawURL string) (IngestResult, error) {
	variants := make([]extractVariant, 0, 3)
	warnings := make([]string, 0, 2)

	if title, content, err := FetchURLContent(ctx, client, rawURL); err == nil {
		variants = append(variants, extractVariant{Method: "direct_text", Title: title, Content: content})
	} else {
		warnings = append(warnings, "direct_text: "+err.Error())
	}

	if strings.TrimSpace(cfg.TikaURL) != "" {
		if title, content, err := FetchURLContentViaTika(ctx, client, cfg.TikaURL, rawURL); err == nil {
			variants = append(variants, extractVariant{Method: "tika_markdown", Title: title, Content: content})
		} else {
			warnings = append(warnings, "tika_markdown: "+err.Error())
		}
	}

	if title, content, err := FetchURLContentViaPlaywright(ctx, cfg, rawURL); err == nil {
		variants = append(variants, extractVariant{Method: "playwright_markdown", Title: title, Content: content})
	} else {
		warnings = append(warnings, "playwright_markdown: "+err.Error())
	}

	if len(variants) == 0 {
		return IngestResult{}, fmt.Errorf("all ingestion methods failed: %s", strings.Join(warnings, " | "))
	}

	title, content := mergeVariantsToMarkdown(rawURL, variants)
	result := IngestResult{Title: title, Content: content}
	if len(variants) >= 2 {
		result.Method = "hybrid_markdown"
	} else {
		result.Method = variants[0].Method
	}
	if len(warnings) > 0 {
		result.FallbackNote = strings.Join(warnings, " | ")
	}
	return result, nil
}

func mergeVariantsToMarkdown(rawURL string, variants []extractVariant) (string, string) {
	preferred := []string{"playwright_markdown", "tika_markdown", "direct_text"}
	best := variants[0]
	bestRank := len(preferred) + 1
	for _, v := range variants {
		rank := methodRank(v.Method, preferred)
		if rank < bestRank || (rank == bestRank && len(v.Content) > len(best.Content)) {
			best = v
			bestRank = rank
		}
	}

	title := chooseBestTitle(rawURL, variants)
	blocks := collectMergedBlocks(variants)
	selected := make([]mergedBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Count >= 2 || (len(variants) == 1 && block.Count == 1) {
			selected = append(selected, block)
		}
	}
	if len(selected) == 0 {
		selected = blocksFromContent(best.Content, best.Method)
	} else if len(selected) < 4 {
		selected = mergeWithBestFallback(selected, blocksFromContent(best.Content, best.Method))
	}

	body := renderMarkdown(title, selected)
	if strings.TrimSpace(body) == "" {
		fallback := normalizeMarkdown(best.Content, best.Method)
		if title != "" && !strings.HasPrefix(fallback, "# ") {
			fallback = "# " + title + "\n\n" + fallback
		}
		return title, TruncateRunes(fallback, MaxStoredContentRunes)
	}
	return title, TruncateRunes(body, MaxStoredContentRunes)
}

func collectMergedBlocks(variants []extractVariant) []mergedBlock {
	merged := map[string]*mergedBlock{}
	for _, variant := range variants {
		seen := map[string]bool{}
		blocks := blocksFromContent(variant.Content, variant.Method)
		for idx, block := range blocks {
			if block.Key == "" || seen[block.Key] {
				continue
			}
			seen[block.Key] = true
			cur, ok := merged[block.Key]
			if !ok {
				cp := block
				cp.Count = 1
				cp.BestRank = methodRank(variant.Method, []string{"playwright_markdown", "tika_markdown", "direct_text"})
				cp.BestIndex = idx
				merged[block.Key] = &cp
				continue
			}
			cur.Count++
			rank := methodRank(variant.Method, []string{"playwright_markdown", "tika_markdown", "direct_text"})
			if rank < cur.BestRank || (rank == cur.BestRank && idx < cur.BestIndex) {
				cur.Text = block.Text
				cur.BestRank = rank
				cur.BestIndex = idx
				cur.IsHeading = block.IsHeading
			}
		}
	}
	out := make([]mergedBlock, 0, len(merged))
	for _, block := range merged {
		out = append(out, *block)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BestRank != out[j].BestRank {
			return out[i].BestRank < out[j].BestRank
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].BestIndex < out[j].BestIndex
	})
	return out
}

func mergeWithBestFallback(primary []mergedBlock, fallback []mergedBlock) []mergedBlock {
	seen := map[string]bool{}
	out := make([]mergedBlock, 0, len(primary)+len(fallback))
	for _, block := range primary {
		seen[block.Key] = true
		out = append(out, block)
	}
	for _, block := range fallback {
		if block.Key == "" || seen[block.Key] {
			continue
		}
		out = append(out, block)
		seen[block.Key] = true
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func blocksFromContent(content string, method string) []mergedBlock {
	normalized := normalizeMarkdown(content, method)
	parts := splitMarkdownBlocks(normalized)
	out := make([]mergedBlock, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || looksLikeBoilerplate(part) {
			continue
		}
		out = append(out, mergedBlock{
			Text:      part,
			Key:       blockKey(part),
			IsHeading: isHeadingBlock(part),
		})
	}
	return out
}

func normalizeMarkdown(content string, method string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			out = append(out, "")
			continue
		}
		if looksLikeBoilerplate(line) {
			continue
		}
		if method == "direct_text" || method == "playwright_markdown" {
			if !reMDHeading.MatchString(line) && !reMDBullet.MatchString(line) && !reMDNumbered.MatchString(line) && len(line) < 90 && mostlyTitleCase(line) {
				line = "## " + line
			}
		}
		out = append(out, line)
	}
	joined := strings.Join(out, "\n")
	joined = regexp.MustCompile(`\n{3,}`).ReplaceAllString(joined, "\n\n")
	return strings.TrimSpace(joined)
}

func splitMarkdownBlocks(content string) []string {
	parts := strings.Split(content, "\n\n")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func renderMarkdown(title string, blocks []mergedBlock) string {
	if len(blocks) == 0 {
		return ""
	}
	lines := make([]string, 0, len(blocks)+2)
	if strings.TrimSpace(title) != "" {
		lines = append(lines, "# "+strings.TrimSpace(title), "")
	}
	for _, block := range blocks {
		text := strings.TrimSpace(block.Text)
		if text == "" {
			continue
		}
		lines = append(lines, text, "")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func chooseBestTitle(rawURL string, variants []extractVariant) string {
	best := ""
	bestRank := 999
	for _, v := range variants {
		title := strings.TrimSpace(v.Title)
		if title == "" || title == rawURL || looksLikeBoilerplate(title) {
			continue
		}
		rank := methodRank(v.Method, []string{"playwright_markdown", "tika_markdown", "direct_text"})
		if rank < bestRank || (rank == bestRank && len(title) < len(best)) {
			best = title
			bestRank = rank
		}
	}
	if best != "" {
		return best
	}
	if u, err := url.Parse(rawURL); err == nil && strings.TrimSpace(u.Hostname()) != "" {
		return u.Hostname()
	}
	return rawURL
}

func methodRank(method string, order []string) int {
	for i, candidate := range order {
		if method == candidate {
			return i
		}
	}
	return len(order) + 1
}

func blockKey(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	var b strings.Builder
	prevSpace := false
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
			prevSpace = false
			continue
		}
		if unicode.IsSpace(r) && !prevSpace {
			b.WriteRune(' ')
			prevSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func isHeadingBlock(text string) bool {
	text = strings.TrimSpace(text)
	return reMDHeading.MatchString(text)
}

func mostlyTitleCase(text string) bool {
	words := strings.Fields(text)
	if len(words) == 0 || len(words) > 8 {
		return false
	}
	upperStarts := 0
	for _, word := range words {
		runes := []rune(word)
		if len(runes) == 0 {
			continue
		}
		if unicode.IsUpper(runes[0]) {
			upperStarts++
		}
	}
	return upperStarts >= max(1, len(words)-1)
}

func looksLikeBoilerplate(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return true
	}
	patterns := []string{
		"skip to content",
		"navigation menu",
		"create account",
		"log in",
		"sign up",
		"sign in",
		"cookie",
		"privacy policy",
		"terms of service",
		"share to",
		"copy link",
		"jump to comments",
		"report abuse",
		"powered by algolia",
		"search powered by",
		"community",
		"contact us",
		"recent changes",
		"upload file",
		"special pages",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	if len(lower) < 3 {
		return true
	}
	return false
}

func FetchURLContent(ctx context.Context, client *http.Client, rawURL string) (string, string, error) {
	title, contentType, body, err := fetchURLBytes(ctx, client, rawURL)
	if err != nil {
		return "", "", err
	}

	raw := string(body)
	content := raw
	if strings.Contains(contentType, "text/html") || strings.Contains(content, "<html") {
		content = htmlToText(content)
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return "", "", fmt.Errorf("no textual content extracted from url")
	}
	content = TruncateRunes(content, MaxStoredContentRunes)
	return title, content, nil
}

func fetchURLBytes(ctx context.Context, client *http.Client, rawURL string) (string, string, []byte, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", nil, fmt.Errorf("invalid url: %w", err)
	}
	if err := validateFetchURLStructure(u); err != nil {
		return "", "", nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", nil, fmt.Errorf("build url request failed: %w", err)
	}
	req.Header.Set("User-Agent", "naima-pkb/1.0")

	httpClient := http.DefaultClient
	if client != nil {
		httpClient = client
	}
	cloned := *httpClient
	// Use a custom transport that validates IPs at dial time to prevent DNS
	// rebinding: a pre-flight DNS check and the actual connection use separate
	// lookups, so an attacker-controlled resolver could return a public IP
	// during validation but then serve a private IP for the real connection.
	cloned.Transport = newSSRFSafeTransport()
	cloned.CheckRedirect = func(redirectReq *http.Request, _ []*http.Request) error {
		return validateFetchURLStructure(redirectReq.URL)
	}

	resp, err := cloned.Do(req)
	if err != nil {
		return "", "", nil, fmt.Errorf("fetch url failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", nil, fmt.Errorf("fetch url returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetchedContentSize+1))
	if err != nil {
		return "", "", nil, fmt.Errorf("read url content failed: %w", err)
	}
	if len(body) > MaxFetchedContentSize {
		return "", "", nil, fmt.Errorf("url content too large (max %d bytes)", MaxFetchedContentSize)
	}

	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	return extractHTMLTitle(string(body)), contentType, body, nil
}

func FetchURLContentViaTika(ctx context.Context, client *http.Client, tikaURL string, rawURL string) (string, string, error) {
	base := strings.TrimRight(strings.TrimSpace(tikaURL), "/")
	if base == "" {
		return "", "", fmt.Errorf("tika url is empty")
	}
	title, contentType, body, err := fetchURLBytes(ctx, client, rawURL)
	if err != nil {
		return "", "", err
	}
	endpoint := base + "/tika"
	if strings.Contains(contentType, "text/html") || strings.Contains(strings.ToLower(string(body)), "<html") {
		endpoint = base + "/tika/main"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("build tika request failed: %w", err)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "text/plain")

	httpClient := http.DefaultClient
	if client != nil {
		httpClient = client
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("tika request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = fmt.Sprintf("status %d", resp.StatusCode)
		}
		return "", "", fmt.Errorf("tika request returned %s", msg)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetchedContentSize+1))
	if err != nil {
		return "", "", fmt.Errorf("read tika response failed: %w", err)
	}
	if len(data) > MaxFetchedContentSize {
		return "", "", fmt.Errorf("tika response too large (max %d bytes)", MaxFetchedContentSize)
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return "", "", fmt.Errorf("tika returned empty content")
	}
	if title == "" {
		title = deriveTitleFromURL(rawURL)
	}
	return title, TruncateRunes(textToMarkdown(title, content), MaxStoredContentRunes), nil
}

func IngestFileContent(ctx context.Context, client *http.Client, tikaURL string, filename string, data []byte) (IngestResult, error) {
	title, content, err := FetchFileContentViaTika(ctx, client, tikaURL, filename, data)
	if err != nil {
		return IngestResult{}, err
	}
	return IngestResult{
		Title:   title,
		Content: content,
		Method:  "tika_file_markdown",
	}, nil
}

func detectContentType(filename string, data []byte) string {
	if ext := strings.ToLower(filepath.Ext(filename)); ext != "" {
		if contentType := mime.TypeByExtension(ext); contentType != "" {
			return contentType
		}
	}
	if len(data) > 0 {
		return http.DetectContentType(data)
	}
	return "application/octet-stream"
}

func deriveTitleFromURL(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		name := strings.TrimSpace(filepath.Base(u.Path))
		name = strings.TrimSuffix(name, filepath.Ext(name))
		name = strings.ReplaceAll(name, "-", " ")
		name = strings.ReplaceAll(name, "_", " ")
		if name != "" && name != "." && name != "/" {
			return name
		}
		if host := strings.TrimSpace(u.Hostname()); host != "" {
			return host
		}
	}
	return rawURL
}

func textToMarkdown(title string, content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	paragraphs := make([]string, 0, len(lines))
	buffer := make([]string, 0, 8)
	flush := func() {
		if len(buffer) == 0 {
			return
		}
		paragraphs = append(paragraphs, strings.Join(buffer, " "))
		buffer = buffer[:0]
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			flush()
			continue
		}
		if looksLikeBoilerplate(line) {
			continue
		}
		buffer = append(buffer, line)
	}
	flush()

	body := strings.Join(paragraphs, "\n\n")
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	if strings.TrimSpace(title) != "" {
		return "# " + strings.TrimSpace(title) + "\n\n" + body
	}
	return body
}

func FetchFileContentViaTika(ctx context.Context, client *http.Client, tikaURL string, filename string, data []byte) (string, string, error) {
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return "", "", fmt.Errorf("filename is required")
	}
	if len(data) == 0 {
		return "", "", fmt.Errorf("file is empty")
	}
	base := strings.TrimRight(strings.TrimSpace(tikaURL), "/")
	if base == "" {
		return "", "", fmt.Errorf("tika url is empty")
	}
	endpoint := base + "/tika"
	contentType := detectContentType(filename, data)
	if strings.Contains(contentType, "text/html") {
		endpoint = base + "/tika/main"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(data))
	if err != nil {
		return "", "", fmt.Errorf("build tika file request failed: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, strings.ReplaceAll(filename, `"`, "")))

	httpClient := http.DefaultClient
	if client != nil {
		httpClient = client
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("tika file request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = fmt.Sprintf("status %d", resp.StatusCode)
		}
		return "", "", fmt.Errorf("tika file request returned %s", msg)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetchedContentSize+1))
	if err != nil {
		return "", "", fmt.Errorf("read tika file response failed: %w", err)
	}
	if len(payload) > MaxFetchedContentSize {
		return "", "", fmt.Errorf("tika file response too large (max %d bytes)", MaxFetchedContentSize)
	}
	content := strings.TrimSpace(string(payload))
	if content == "" {
		return "", "", fmt.Errorf("tika returned empty content for file")
	}
	return filename, TruncateRunes(textToMarkdown(filename, content), MaxStoredContentRunes), nil
}

func FetchURLContentViaPlaywright(ctx context.Context, cfg IngestConfig, rawURL string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", fmt.Errorf("invalid url: %w", err)
	}
	if err := validateFetchURL(ctx, u); err != nil {
		return "", "", err
	}
	if err := ensurePlaywrightInstalled(); err != nil {
		return "", "", fmt.Errorf("playwright install failed: %w", err)
	}
	pw, err := playwright.Run()
	if err != nil {
		return "", "", fmt.Errorf("playwright start failed: %w", err)
	}
	defer pw.Stop()
	timeoutMS := cfg.PlaywrightTimeoutMS
	if timeoutMS <= 0 {
		timeoutMS = 30000
	}
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(cfg.PlaywrightHeadless),
		Timeout:  playwright.Float(float64(timeoutMS)),
	})
	if err != nil {
		return "", "", fmt.Errorf("playwright launch failed: %w", err)
	}
	defer browser.Close()
	page, err := browser.NewPage()
	if err != nil {
		return "", "", fmt.Errorf("playwright page failed: %w", err)
	}
	defer page.Close()
	page.SetDefaultTimeout(float64(timeoutMS))
	if _, err := page.Goto(rawURL, playwright.PageGotoOptions{Timeout: playwright.Float(float64(timeoutMS))}); err != nil {
		return "", "", fmt.Errorf("playwright navigate failed: %w", err)
	}
	page.WaitForTimeout(1200)

	result, err := page.Evaluate(`() => {
		const selectors = ['article', 'main article', 'main', '[role="main"]', '.article-body', '.post-content', '.entry-content', '.content', '.post', '.story', '.markdown-body'];
		const score = (node) => {
			if (!node) return -1;
			const text = (node.innerText || '').trim();
			if (text.length < 120) return -1;
			let value = text.length;
			if (node.tagName === 'ARTICLE') value += 400;
			if (node.matches && node.matches('main, article, [role="main"]')) value += 300;
			const links = node.querySelectorAll ? node.querySelectorAll('a').length : 0;
			const paras = node.querySelectorAll ? node.querySelectorAll('p').length : 0;
			value += paras * 40;
			value -= Math.min(links, 120) * 3;
			return value;
		};
		let root = null;
		for (const sel of selectors) {
			const node = document.querySelector(sel);
			if (score(node) > 0) { root = node; break; }
		}
		if (!root) {
			const nodes = [...document.body.querySelectorAll('article, main, section, div')];
			nodes.sort((a, b) => score(b) - score(a));
			root = nodes.find((node) => score(node) > 0) || document.body;
		}
		const heading = document.querySelector('h1');
		const title = (heading && heading.innerText.trim()) || document.title || '';
		const text = (root.innerText || '').split('\n').map((line) => line.trim()).filter(Boolean).join('\n\n');
		return { title, text };
	}`)
	if err != nil {
		return "", "", fmt.Errorf("playwright evaluate failed: %w", err)
	}
	payload, ok := result.(map[string]any)
	if !ok {
		return "", "", fmt.Errorf("playwright returned unexpected payload")
	}
	title, _ := payload["title"].(string)
	text, _ := payload["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return "", "", fmt.Errorf("playwright returned empty content")
	}
	return strings.TrimSpace(title), TruncateRunes(text, MaxStoredContentRunes), nil
}

func TruncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) <= limit {
		return string(r)
	}
	return strings.TrimSpace(string(r[:limit])) + "..."
}

// newSSRFSafeTransport returns an http.Transport whose dialer resolves host IPs
// and blocks private/loopback/multicast addresses at connection time.
// This prevents DNS rebinding: the IP is checked at the moment the TCP
// connection is established, not in a separate pre-flight lookup.
func newSSRFSafeTransport() *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("resolve host failed: %w", err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("host resolved to no addresses")
			}
			var dialer net.Dialer
			var lastErr error
			for _, ip := range ips {
				if !ip.IsValid() {
					continue
				}
				if ip.IsLoopback() || ip.IsPrivate() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || blockedIPv4CGNAT.Contains(ip) {
					lastErr = fmt.Errorf("url host resolves to disallowed address")
					continue
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("no usable addresses for %s", host)
			}
			return nil, lastErr
		},
	}
}

// validateFetchURLStructure checks URL scheme and structure without doing a DNS
// lookup. Used for initial validation and redirect checks when the SSRF-safe
// dialer already handles IP-level enforcement at connection time.
func validateFetchURLStructure(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("invalid url")
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("unsupported url scheme: %s", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("url userinfo is not allowed")
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return fmt.Errorf("url host is required")
	}
	return nil
}

func validateFetchURL(ctx context.Context, u *url.URL) error {
	if u == nil {
		return fmt.Errorf("invalid url")
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("unsupported url scheme: %s", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("url userinfo is not allowed")
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return fmt.Errorf("url host is required")
	}

	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve host failed: %w", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("resolve host returned no addresses")
	}
	for _, ip := range ips {
		if !ip.IsValid() {
			continue
		}
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || blockedIPv4CGNAT.Contains(ip) {
			return fmt.Errorf("url host resolves to disallowed address")
		}
	}
	return nil
}

func extractHTMLTitle(raw string) string {
	match := reHTMLTitle.FindStringSubmatch(raw)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(htmlToText(match[1]))
}

func htmlToText(raw string) string {
	s := reScriptBlock.ReplaceAllString(raw, " ")
	s = reStyleBlock.ReplaceAllString(s, " ")
	s = reHTMLTags.ReplaceAllString(s, " ")
	s = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&#39;", "'").Replace(s)
	s = reMultiSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func ensurePlaywrightInstalled() error {
	playwrightInstallOnce.Do(func() {
		playwrightInstallErr = playwright.Install(&playwright.RunOptions{SkipInstallBrowsers: false})
	})
	return playwrightInstallErr
}
