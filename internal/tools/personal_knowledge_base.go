package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"naima/internal/pkb"
)

const (
	defaultPKBToolTimeout = 15 * time.Second
	maxFetchedContentSize = 2 * 1024 * 1024
	maxStoredContentRunes = 20000
)

var (
	reScriptBlock = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyleBlock  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reHTMLTags    = regexp.MustCompile(`(?is)<[^>]+>`)
	reMultiSpace  = regexp.MustCompile(`\s+`)
	reHTMLTitle   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

	blockedIPv4CGNAT = netip.MustParsePrefix("100.64.0.0/10")
)

type PKBStorage interface {
	CreateTopic(ctx context.Context, title string) (pkb.Topic, error)
	ListTopics(ctx context.Context) ([]pkb.Topic, error)
	UpdateTopic(ctx context.Context, topicID int64, title string) (pkb.Topic, error)
	DeleteTopic(ctx context.Context, topicID int64) error
	CreateDocument(ctx context.Context, req pkb.CreateDocumentRequest) (pkb.Document, error)
	ListDocuments(ctx context.Context, topicID int64) ([]pkb.Document, error)
	UpdateDocument(ctx context.Context, req pkb.UpdateDocumentRequest) (pkb.Document, error)
	DeleteDocument(ctx context.Context, documentID int64) error
}

type PersonalKnowledgeBaseTool struct {
	store  PKBStorage
	client *http.Client
}

type pkbParams struct {
	Operation  string `json:"operation"`
	TopicID    int64  `json:"topic_id,omitempty"`
	Topic      string `json:"topic,omitempty"`
	DocumentID int64  `json:"document_id,omitempty"`
	Title      string `json:"title,omitempty"`
	URL        string `json:"url,omitempty"`
	Note       string `json:"note,omitempty"`
	Content    string `json:"content,omitempty"`
}

func NewPersonalKnowledgeBaseTool(store PKBStorage) Tool {
	return &PersonalKnowledgeBaseTool{
		store:  store,
		client: &http.Client{Timeout: defaultPKBToolTimeout},
	}
}

func (t *PersonalKnowledgeBaseTool) GetName() string {
	return "personal_knowledge_base"
}

func (t *PersonalKnowledgeBaseTool) GetDescription() string {
	return "Manages personal knowledge topics and documents (notes or URL-based content)."
}

func (t *PersonalKnowledgeBaseTool) GetFunction() func(params string) string {
	return func(params string) string {
		if t.store == nil {
			return errorJSON("personal knowledge base storage is not configured")
		}

		var in pkbParams
		if err := jsonUnmarshal(params, &in); err != nil {
			return errorJSON("invalid params: " + err.Error())
		}

		op := strings.ToLower(strings.TrimSpace(in.Operation))
		if op == "" {
			return errorJSON("operation is required")
		}

		ctx, cancel := context.WithTimeout(context.Background(), defaultPKBToolTimeout)
		defer cancel()

		switch op {
		case "create_topic":
			title := strings.TrimSpace(in.Topic)
			if title == "" {
				title = strings.TrimSpace(in.Title)
			}
			if title == "" {
				return errorJSON("topic is required")
			}
			topic, err := t.store.CreateTopic(ctx, title)
			if err != nil {
				return errorJSON(err.Error())
			}
			return mustJSON(map[string]any{"operation": op, "topic": topic})
		case "list_topics":
			topics, err := t.store.ListTopics(ctx)
			if err != nil {
				return errorJSON(err.Error())
			}
			return mustJSON(map[string]any{"operation": op, "count": len(topics), "topics": topics})
		case "update_topic":
			if in.TopicID <= 0 {
				return errorJSON("topic_id is required")
			}
			title := strings.TrimSpace(in.Topic)
			if title == "" {
				title = strings.TrimSpace(in.Title)
			}
			if title == "" {
				return errorJSON("topic is required")
			}
			topic, err := t.store.UpdateTopic(ctx, in.TopicID, title)
			if err != nil {
				return errorJSON(err.Error())
			}
			return mustJSON(map[string]any{"operation": op, "topic": topic})
		case "delete_topic":
			if in.TopicID <= 0 {
				return errorJSON("topic_id is required")
			}
			if err := t.store.DeleteTopic(ctx, in.TopicID); err != nil {
				return errorJSON(err.Error())
			}
			return mustJSON(map[string]any{"operation": op, "deleted_topic_id": in.TopicID})
		case "add_content":
			if in.TopicID <= 0 {
				return errorJSON("topic_id is required")
			}
			trimmedURL := strings.TrimSpace(in.URL)
			note := strings.TrimSpace(in.Note)
			content := strings.TrimSpace(in.Content)
			title := strings.TrimSpace(in.Title)

			kind := "note"
			sourceURL := ""
			if trimmedURL != "" {
				kind = "url"
				sourceURL = trimmedURL
				fetchedTitle, fetchedContent, err := t.fetchURLContent(ctx, trimmedURL)
				if err != nil {
					return errorJSON(err.Error())
				}
				if title == "" {
					title = fetchedTitle
				}
				if content == "" {
					content = fetchedContent
				}
			}
			if note != "" {
				if content != "" {
					content += "\n\n" + note
				} else {
					content = note
				}
			}
			content = strings.TrimSpace(content)
			if content == "" {
				return errorJSON("note or url is required")
			}
			content = truncateRunes(content, maxStoredContentRunes)
			if title == "" {
				if kind == "url" {
					title = sourceURL
				} else {
					title = "Note " + time.Now().UTC().Format("2006-01-02 15:04")
				}
			}

			doc, err := t.store.CreateDocument(ctx, pkb.CreateDocumentRequest{
				TopicID:   in.TopicID,
				Kind:      kind,
				Title:     title,
				SourceURL: sourceURL,
				Content:   content,
			})
			if err != nil {
				return errorJSON(err.Error())
			}
			return mustJSON(map[string]any{"operation": op, "document": doc})
		case "list_documents":
			if in.TopicID <= 0 {
				return errorJSON("topic_id is required")
			}
			docs, err := t.store.ListDocuments(ctx, in.TopicID)
			if err != nil {
				return errorJSON(err.Error())
			}
			return mustJSON(map[string]any{"operation": op, "topic_id": in.TopicID, "count": len(docs), "documents": docs})
		case "update_document":
			if in.DocumentID <= 0 {
				return errorJSON("document_id is required")
			}
			content := strings.TrimSpace(in.Content)
			if content == "" {
				content = strings.TrimSpace(in.Note)
			}
			if content == "" {
				return errorJSON("content is required")
			}
			doc, err := t.store.UpdateDocument(ctx, pkb.UpdateDocumentRequest{
				DocumentID: in.DocumentID,
				Title:      strings.TrimSpace(in.Title),
				SourceURL:  strings.TrimSpace(in.URL),
				Content:    truncateRunes(content, maxStoredContentRunes),
			})
			if err != nil {
				return errorJSON(err.Error())
			}
			return mustJSON(map[string]any{"operation": op, "document": doc})
		case "delete_document":
			if in.DocumentID <= 0 {
				return errorJSON("document_id is required")
			}
			if err := t.store.DeleteDocument(ctx, in.DocumentID); err != nil {
				return errorJSON(err.Error())
			}
			return mustJSON(map[string]any{"operation": op, "deleted_document_id": in.DocumentID})
		default:
			return errorJSON("unsupported operation: " + op)
		}
	}
}

func (t *PersonalKnowledgeBaseTool) IsImmediate() bool {
	return false
}

func (t *PersonalKnowledgeBaseTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"operation": map[string]any{
				"type":        "string",
				"description": "Operation to execute.",
				"enum": []string{
					"create_topic",
					"list_topics",
					"update_topic",
					"delete_topic",
					"add_content",
					"list_documents",
					"update_document",
					"delete_document",
				},
			},
			"topic_id": map[string]any{
				"type":        "integer",
				"description": "Topic id for topic/document operations.",
			},
			"topic": map[string]any{
				"type":        "string",
				"description": "Topic title (for create/update topic).",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Document title (optional for add_content, used for update_document).",
			},
			"document_id": map[string]any{
				"type":        "integer",
				"description": "Document id for update/delete document.",
			},
			"url": map[string]any{
				"type":        "string",
				"description": "Source URL for add_content from website.",
			},
			"note": map[string]any{
				"type":        "string",
				"description": "Manual note content for add_content.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Explicit document content for add/update document.",
			},
		},
		Required: []string{"operation"},
	}
}

func (t *PersonalKnowledgeBaseTool) fetchURLContent(ctx context.Context, rawURL string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", fmt.Errorf("invalid url: %w", err)
	}
	if err := validateFetchURL(ctx, u); err != nil {
		return "", "", err
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("unsupported url scheme: %s", u.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", fmt.Errorf("build url request failed: %w", err)
	}
	req.Header.Set("User-Agent", "naima-pkb/1.0")

	client := *t.client
	client.CheckRedirect = func(redirectReq *http.Request, _ []*http.Request) error {
		if err := validateFetchURL(redirectReq.Context(), redirectReq.URL); err != nil {
			return err
		}
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch url failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("fetch url returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchedContentSize+1))
	if err != nil {
		return "", "", fmt.Errorf("read url content failed: %w", err)
	}
	if len(body) > maxFetchedContentSize {
		return "", "", fmt.Errorf("url content too large (max %d bytes)", maxFetchedContentSize)
	}

	raw := string(body)
	title := extractHTMLTitle(raw)
	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	content := raw
	if strings.Contains(contentType, "text/html") || strings.Contains(content, "<html") {
		content = htmlToText(content)
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return "", "", fmt.Errorf("no textual content extracted from url")
	}
	content = truncateRunes(content, maxStoredContentRunes)
	return title, content, nil
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
	if isBlockedHostname(host) {
		return fmt.Errorf("blocked host")
	}

	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("blocked target ip")
		}
		return nil
	}

	resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve host failed: %w", err)
	}
	if len(resolved) == 0 {
		return fmt.Errorf("resolve host failed: no ips")
	}
	for _, addr := range resolved {
		if isBlockedIP(addr.IP) {
			return fmt.Errorf("blocked target ip")
		}
	}

	return nil
}

func isBlockedHostname(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return true
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") {
		return true
	}
	return false
}

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}

	if addr, ok := netip.AddrFromSlice(ip); ok {
		if blockedIPv4CGNAT.Contains(addr.Unmap()) {
			return true
		}
	}

	return false
}

func extractHTMLTitle(raw string) string {
	m := reHTMLTitle.FindStringSubmatch(raw)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(htmlToText(m[1]))
}

func htmlToText(raw string) string {
	out := reScriptBlock.ReplaceAllString(raw, " ")
	out = reStyleBlock.ReplaceAllString(out, " ")
	out = reHTMLTags.ReplaceAllString(out, " ")
	out = reMultiSpace.ReplaceAllString(out, " ")
	return strings.TrimSpace(out)
}

func truncateRunes(v string, max int) string {
	if max <= 0 {
		return v
	}
	r := []rune(v)
	if len(r) <= max {
		return v
	}
	return string(r[:max]) + "..."
}

func jsonUnmarshal(raw string, out any) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "{}"
	}
	return json.Unmarshal([]byte(raw), out)
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return `{"error":"serialize response failed"}`
	}
	return string(data)
}
