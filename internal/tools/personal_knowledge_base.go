package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"naima/internal/pkb"
)

const (
	defaultPKBToolTimeout = 15 * time.Second
	maxStoredContentRunes = pkb.MaxStoredContentRunes
)

type PKBStorage interface {
	CreateTopic(ctx context.Context, title string) (pkb.Topic, error)
	ListTopics(ctx context.Context) ([]pkb.Topic, error)
	UpdateTopic(ctx context.Context, topicID int64, title string) (pkb.Topic, error)
	DeleteTopic(ctx context.Context, topicID int64) error
	CreateDocument(ctx context.Context, req pkb.CreateDocumentRequest) (pkb.Document, error)
	ListDocuments(ctx context.Context, topicID int64) ([]pkb.Document, error)
	ListDocumentsByTimeRange(ctx context.Context, req pkb.ListDocumentsRangeRequest) ([]pkb.Document, error)
	UpdateDocument(ctx context.Context, req pkb.UpdateDocumentRequest) (pkb.Document, error)
	DeleteDocument(ctx context.Context, documentID int64) error
}

type PersonalKnowledgeBaseTool struct {
	store     PKBStorage
	client    *http.Client
	ingestCfg pkb.IngestConfig
}

type pkbParams struct {
	Operation    string `json:"operation"`
	TopicID      int64  `json:"topic_id,omitempty"`
	Topic        string `json:"topic,omitempty"`
	DocumentID   int64  `json:"document_id,omitempty"`
	Title        string `json:"title,omitempty"`
	URL          string `json:"url,omitempty"`
	Note         string `json:"note,omitempty"`
	Content      string `json:"content,omitempty"`
	IngestMethod string `json:"ingest_method,omitempty"`
	Timeframe    string `json:"timeframe,omitempty"`
	Since        string `json:"since,omitempty"`
	Until        string `json:"until,omitempty"`
}

func NewPersonalKnowledgeBaseTool(store PKBStorage, ingestCfg pkb.IngestConfig) Tool {
	return &PersonalKnowledgeBaseTool{
		store:     store,
		client:    &http.Client{Timeout: defaultPKBToolTimeout},
		ingestCfg: ingestCfg,
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
			ingestMethod := ""
			if trimmedURL != "" {
				kind = "url"
				sourceURL = trimmedURL
				ingested, err := pkb.IngestURLContent(ctx, t.client, t.ingestCfg, trimmedURL)
				if err != nil {
					return errorJSON(err.Error())
				}
				if ingested.FallbackNote != "" {
					log.Warnf("[pkb] docling failed for tool url=%s fallback=%s", trimmedURL, ingested.FallbackNote)
				}
				if title == "" {
					title = ingested.Title
				}
				if content == "" {
					content = ingested.Content
				}
				ingestMethod = ingested.Method
			}
			if note != "" {
				if content != "" {
					content += "\n\n" + note
				} else {
					content = note
				}
				if kind == "note" {
					ingestMethod = "manual_note"
				} else if ingestMethod != "" {
					ingestMethod += "+note"
				}
			}
			content = strings.TrimSpace(content)
			if content == "" {
				return errorJSON("note or url is required")
			}
			content = pkb.TruncateRunes(content, maxStoredContentRunes)
			if title == "" {
				if kind == "url" {
					title = sourceURL
				} else {
					title = "Note " + time.Now().UTC().Format("2006-01-02 15:04")
				}
			}

			doc, err := t.store.CreateDocument(ctx, pkb.CreateDocumentRequest{
				TopicID:      in.TopicID,
				Kind:         kind,
				Title:        title,
				SourceURL:    sourceURL,
				IngestMethod: strings.TrimSpace(ingestMethod),
				Content:      content,
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
		case "temporal_search":
			since, until, label, err := resolveTemporalRange(in.Timeframe, in.Since, in.Until, time.Now().UTC())
			if err != nil {
				return errorJSON(err.Error())
			}
			docs, err := t.store.ListDocumentsByTimeRange(ctx, pkb.ListDocumentsRangeRequest{
				TopicID: in.TopicID,
				Since:   since,
				Until:   until,
			})
			if err != nil {
				return errorJSON(err.Error())
			}
			structured := buildTemporalStructuredDocument(label, in.TopicID, docs, since, until)
			return mustJSON(map[string]any{
				"operation":           op,
				"timeframe":           label,
				"topic_id":            in.TopicID,
				"since":               since.Format(time.RFC3339),
				"until":               until.Format(time.RFC3339),
				"count":               len(docs),
				"documents":           docs,
				"structured_document": structured,
			})
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
				Content:    pkb.TruncateRunes(content, maxStoredContentRunes),
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
					"temporal_search",
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
			"timeframe": map[string]any{
				"type":        "string",
				"description": "For temporal_search: preset time window.",
				"enum":        []string{"today", "week", "month"},
			},
			"since": map[string]any{
				"type":        "string",
				"description": "For temporal_search: custom RFC3339 start time.",
			},
			"until": map[string]any{
				"type":        "string",
				"description": "For temporal_search: custom RFC3339 end time.",
			},
		},
		Required: []string{"operation"},
	}
}

func resolveTemporalRange(timeframe string, sinceRaw string, untilRaw string, now time.Time) (time.Time, time.Time, string, error) {
	timeframe = strings.ToLower(strings.TrimSpace(timeframe))
	sinceRaw = strings.TrimSpace(sinceRaw)
	untilRaw = strings.TrimSpace(untilRaw)

	if sinceRaw != "" || untilRaw != "" {
		if sinceRaw == "" || untilRaw == "" {
			return time.Time{}, time.Time{}, "", fmt.Errorf("both since and until are required for custom range")
		}
		since, err := time.Parse(time.RFC3339, sinceRaw)
		if err != nil {
			return time.Time{}, time.Time{}, "", fmt.Errorf("invalid since (expected RFC3339)")
		}
		until, err := time.Parse(time.RFC3339, untilRaw)
		if err != nil {
			return time.Time{}, time.Time{}, "", fmt.Errorf("invalid until (expected RFC3339)")
		}
		if !since.Before(until) {
			return time.Time{}, time.Time{}, "", fmt.Errorf("since must be before until")
		}
		return since.UTC(), until.UTC(), "custom", nil
	}

	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch timeframe {
	case "", "today":
		return startOfDay, now.UTC(), "today", nil
	case "week":
		return startOfDay.AddDate(0, 0, -6), now.UTC(), "week", nil
	case "month":
		return startOfDay.AddDate(0, 0, -29), now.UTC(), "month", nil
	default:
		return time.Time{}, time.Time{}, "", fmt.Errorf("unsupported timeframe: %s", timeframe)
	}
}

func buildTemporalStructuredDocument(label string, topicID int64, docs []pkb.Document, since time.Time, until time.Time) map[string]any {
	title := "PKB temporal summary"
	if label != "" {
		title += " - " + label
	}

	sections := make([]map[string]any, 0, len(docs))
	builder := strings.Builder{}
	builder.WriteString(title)
	builder.WriteString("\n")
	builder.WriteString("Range: ")
	builder.WriteString(since.Format(time.RFC3339))
	builder.WriteString(" -> ")
	builder.WriteString(until.Format(time.RFC3339))
	if topicID > 0 {
		builder.WriteString("\nTopic ID: ")
		builder.WriteString(fmt.Sprintf("%d", topicID))
	}
	builder.WriteString("\n\n")

	for _, doc := range docs {
		sectionTitle := strings.TrimSpace(doc.Title)
		if sectionTitle == "" {
			sectionTitle = fmt.Sprintf("Document %d", doc.ID)
		}
		builder.WriteString("## ")
		builder.WriteString(sectionTitle)
		builder.WriteString("\n")
		builder.WriteString("Topic: ")
		if strings.TrimSpace(doc.TopicTitle) != "" {
			builder.WriteString(doc.TopicTitle)
		} else {
			builder.WriteString(fmt.Sprintf("%d", doc.TopicID))
		}
		builder.WriteString("\nKind: ")
		builder.WriteString(doc.Kind)
		if strings.TrimSpace(doc.SourceURL) != "" {
			builder.WriteString("\nSource: ")
			builder.WriteString(doc.SourceURL)
		}
		if doc.CreatedAt != nil {
			builder.WriteString("\nCreated: ")
			builder.WriteString(doc.CreatedAt.UTC().Format(time.RFC3339))
		}
		builder.WriteString("\n\n")
		builder.WriteString(doc.Content)
		builder.WriteString("\n\n")

		sections = append(sections, map[string]any{
			"document_id": doc.ID,
			"title":       sectionTitle,
			"topic_id":    doc.TopicID,
			"topic_title": doc.TopicTitle,
			"kind":        doc.Kind,
			"source_url":  doc.SourceURL,
			"created_at":  formatOptionalTime(doc.CreatedAt),
			"content":     doc.Content,
		})
	}

	return map[string]any{
		"title":     title,
		"timeframe": label,
		"since":     since.Format(time.RFC3339),
		"until":     until.Format(time.RFC3339),
		"topic_id":  topicID,
		"documents": len(docs),
		"sections":  sections,
		"full_text": strings.TrimSpace(builder.String()),
	}
}

func formatOptionalTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
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
