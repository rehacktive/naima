package pkb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const initSchemaQuery = `
CREATE TABLE IF NOT EXISTS pkb_topics (
	id BIGSERIAL PRIMARY KEY,
	title TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_pkb_topics_title_lower ON pkb_topics (LOWER(title));

CREATE TABLE IF NOT EXISTS pkb_documents (
	id BIGSERIAL PRIMARY KEY,
	topic_id BIGINT NOT NULL REFERENCES pkb_topics(id) ON DELETE CASCADE,
	kind TEXT NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	source_url TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_pkb_documents_topic_id ON pkb_documents(topic_id);
CREATE INDEX IF NOT EXISTS idx_pkb_documents_kind ON pkb_documents(kind);
`

type Storage struct {
	pool *pgxpool.Pool
}

type Topic struct {
	ID             int64      `json:"id"`
	Title          string     `json:"title"`
	Documents      int        `json:"documents"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
	LastDocumentAt *time.Time `json:"last_document_at,omitempty"`
}

type Document struct {
	ID        int64      `json:"id"`
	TopicID   int64      `json:"topic_id"`
	Kind      string     `json:"kind"`
	Title     string     `json:"title"`
	SourceURL string     `json:"source_url,omitempty"`
	Content   string     `json:"content"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

type CreateDocumentRequest struct {
	TopicID   int64
	Kind      string
	Title     string
	SourceURL string
	Content   string
}

type UpdateDocumentRequest struct {
	DocumentID int64
	Title      string
	SourceURL  string
	Content    string
}

func NewStorage(ctx context.Context, dsn string) (*Storage, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("pkb dsn is empty")
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pkb db connect failed: %w", err)
	}

	if _, err := pool.Exec(ctx, initSchemaQuery); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pkb init schema failed: %w", err)
	}

	return &Storage{pool: pool}, nil
}

func (s *Storage) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

func (s *Storage) CreateTopic(ctx context.Context, title string) (Topic, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Topic{}, errors.New("title is required")
	}

	query := `
INSERT INTO pkb_topics(title)
VALUES ($1)
RETURNING id, title, created_at, updated_at;
`
	var t Topic
	if err := s.pool.QueryRow(ctx, query, title).Scan(&t.ID, &t.Title, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if isUniqueViolation(err) {
			return Topic{}, fmt.Errorf("topic already exists: %s", title)
		}
		return Topic{}, fmt.Errorf("create topic failed: %w", err)
	}
	return t, nil
}

func (s *Storage) ListTopics(ctx context.Context) ([]Topic, error) {
	query := `
SELECT
	t.id,
	t.title,
	t.created_at,
	t.updated_at,
	COALESCE(COUNT(d.id), 0) AS documents,
	MAX(d.created_at) AS last_document_at
FROM pkb_topics t
LEFT JOIN pkb_documents d ON d.topic_id = t.id
GROUP BY t.id, t.title, t.created_at, t.updated_at
ORDER BY t.updated_at DESC, t.id DESC;
`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list topics failed: %w", err)
	}
	defer rows.Close()

	out := make([]Topic, 0)
	for rows.Next() {
		var t Topic
		if err := rows.Scan(&t.ID, &t.Title, &t.CreatedAt, &t.UpdatedAt, &t.Documents, &t.LastDocumentAt); err != nil {
			return nil, fmt.Errorf("scan topic failed: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate topics failed: %w", err)
	}
	return out, nil
}

func (s *Storage) UpdateTopic(ctx context.Context, topicID int64, title string) (Topic, error) {
	if topicID <= 0 {
		return Topic{}, errors.New("topic_id is required")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return Topic{}, errors.New("title is required")
	}

	query := `
UPDATE pkb_topics
SET title = $2, updated_at = NOW()
WHERE id = $1
RETURNING id, title, created_at, updated_at;
`
	var t Topic
	if err := s.pool.QueryRow(ctx, query, topicID, title).Scan(&t.ID, &t.Title, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return Topic{}, fmt.Errorf("topic not found: %d", topicID)
		}
		if isUniqueViolation(err) {
			return Topic{}, fmt.Errorf("topic already exists: %s", title)
		}
		return Topic{}, fmt.Errorf("update topic failed: %w", err)
	}
	return t, nil
}

func (s *Storage) DeleteTopic(ctx context.Context, topicID int64) error {
	if topicID <= 0 {
		return errors.New("topic_id is required")
	}

	tag, err := s.pool.Exec(ctx, `DELETE FROM pkb_topics WHERE id = $1`, topicID)
	if err != nil {
		return fmt.Errorf("delete topic failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("topic not found: %d", topicID)
	}
	return nil
}

func (s *Storage) CreateDocument(ctx context.Context, req CreateDocumentRequest) (Document, error) {
	if req.TopicID <= 0 {
		return Document{}, errors.New("topic_id is required")
	}
	req.Kind = strings.ToLower(strings.TrimSpace(req.Kind))
	if req.Kind == "" {
		req.Kind = "note"
	}
	if req.Kind != "note" && req.Kind != "url" {
		return Document{}, errors.New("kind must be note or url")
	}

	req.Title = strings.TrimSpace(req.Title)
	req.SourceURL = strings.TrimSpace(req.SourceURL)
	req.Content = strings.TrimSpace(req.Content)
	if req.Content == "" {
		return Document{}, errors.New("content is required")
	}
	if req.Kind == "url" && req.SourceURL == "" {
		return Document{}, errors.New("source_url is required for kind=url")
	}

	query := `
INSERT INTO pkb_documents(topic_id, kind, title, source_url, content)
VALUES ($1,$2,$3,$4,$5)
RETURNING id, topic_id, kind, title, source_url, content, created_at, updated_at;
`
	var d Document
	if err := s.pool.QueryRow(ctx, query, req.TopicID, req.Kind, req.Title, req.SourceURL, req.Content).Scan(
		&d.ID, &d.TopicID, &d.Kind, &d.Title, &d.SourceURL, &d.Content, &d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		if isForeignKeyViolation(err) {
			return Document{}, fmt.Errorf("topic not found: %d", req.TopicID)
		}
		return Document{}, fmt.Errorf("create document failed: %w", err)
	}
	return d, nil
}

func (s *Storage) ListDocuments(ctx context.Context, topicID int64) ([]Document, error) {
	if topicID <= 0 {
		return nil, errors.New("topic_id is required")
	}

	query := `
SELECT id, topic_id, kind, title, source_url, content, created_at, updated_at
FROM pkb_documents
WHERE topic_id = $1
ORDER BY created_at DESC, id DESC;
`
	rows, err := s.pool.Query(ctx, query, topicID)
	if err != nil {
		return nil, fmt.Errorf("list documents failed: %w", err)
	}
	defer rows.Close()

	out := make([]Document, 0)
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.TopicID, &d.Kind, &d.Title, &d.SourceURL, &d.Content, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan document failed: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate documents failed: %w", err)
	}
	return out, nil
}

func (s *Storage) UpdateDocument(ctx context.Context, req UpdateDocumentRequest) (Document, error) {
	if req.DocumentID <= 0 {
		return Document{}, errors.New("document_id is required")
	}
	req.Title = strings.TrimSpace(req.Title)
	req.SourceURL = strings.TrimSpace(req.SourceURL)
	req.Content = strings.TrimSpace(req.Content)
	if req.Content == "" {
		return Document{}, errors.New("content is required")
	}

	query := `
UPDATE pkb_documents
SET title = $2, source_url = $3, content = $4, updated_at = NOW()
WHERE id = $1
RETURNING id, topic_id, kind, title, source_url, content, created_at, updated_at;
`
	var d Document
	if err := s.pool.QueryRow(ctx, query, req.DocumentID, req.Title, req.SourceURL, req.Content).Scan(
		&d.ID, &d.TopicID, &d.Kind, &d.Title, &d.SourceURL, &d.Content, &d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return Document{}, fmt.Errorf("document not found: %d", req.DocumentID)
		}
		return Document{}, fmt.Errorf("update document failed: %w", err)
	}
	return d, nil
}

func (s *Storage) DeleteDocument(ctx context.Context, documentID int64) error {
	if documentID <= 0 {
		return errors.New("document_id is required")
	}

	tag, err := s.pool.Exec(ctx, `DELETE FROM pkb_documents WHERE id = $1`, documentID)
	if err != nil {
		return fmt.Errorf("delete document failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("document not found: %d", documentID)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
