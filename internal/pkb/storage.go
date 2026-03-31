package pkb

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	log "github.com/sirupsen/logrus"
)

const initSchemaQuery = `
CREATE EXTENSION IF NOT EXISTS vector;

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
	ingest_method TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE pkb_documents ADD COLUMN IF NOT EXISTS ingest_method TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_pkb_documents_topic_id ON pkb_documents(topic_id);
CREATE INDEX IF NOT EXISTS idx_pkb_documents_kind ON pkb_documents(kind);

CREATE TABLE IF NOT EXISTS pkb_embeddings (
	id BIGSERIAL PRIMARY KEY,
	document_id BIGINT NOT NULL REFERENCES pkb_documents(id) ON DELETE CASCADE,
	chunk_index INTEGER NOT NULL,
	chunk_content TEXT NOT NULL,
	embeddings VECTOR,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_pkb_embeddings_document_id ON pkb_embeddings(document_id);

CREATE TABLE IF NOT EXISTS pkb_tags (
	id BIGSERIAL PRIMARY KEY,
	name TEXT NOT NULL,
	category TEXT NOT NULL DEFAULT 'OTHER',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE pkb_tags ADD COLUMN IF NOT EXISTS category TEXT NOT NULL DEFAULT 'OTHER';
DROP INDEX IF EXISTS idx_pkb_tags_name_lower;
CREATE UNIQUE INDEX IF NOT EXISTS idx_pkb_tags_name_category_lower ON pkb_tags (LOWER(name), category);

CREATE TABLE IF NOT EXISTS pkb_document_tags (
	document_id BIGINT NOT NULL REFERENCES pkb_documents(id) ON DELETE CASCADE,
	tag_id BIGINT NOT NULL REFERENCES pkb_tags(id) ON DELETE CASCADE,
	occurrences INTEGER NOT NULL DEFAULT 1,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (document_id, tag_id)
);
CREATE INDEX IF NOT EXISTS idx_pkb_document_tags_tag_id ON pkb_document_tags(tag_id);
CREATE INDEX IF NOT EXISTS idx_pkb_document_tags_document_id ON pkb_document_tags(document_id);
`

const (
	defaultChunkSize           = 2000
	defaultRetrievalDocLimit   = 3
	defaultRetrievalChunkLimit = 4
	defaultRetrievalThreshold  = 0.35
	defaultTagLimit            = 12
)

type Storage struct {
	pool                *pgxpool.Pool
	embedder            EmbeddingGenerator
	tagger              TagExtractor
	embeddingModel      string
	chunkSize           int
	vectorDims          int
	retrievalDocLimit   int
	retrievalChunkLimit int
	retrievalThreshold  float64
	tagLimit            int
}

type EmbeddingGenerator func(ctx context.Context, inputs []string, model string) ([][]float32, error)
type TagExtractor func(ctx context.Context, content string, limit int) ([]ExtractedTag, error)

type ExtractedTag struct {
	Text     string
	Category string
}

type Config struct {
	Embedder            EmbeddingGenerator
	Tagger              TagExtractor
	EmbeddingModel      string
	ChunkSize           int
	VectorDims          int
	RetrievalDocLimit   int
	RetrievalChunkLimit int
	RetrievalThreshold  float64
	TagLimit            int
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
	ID           int64      `json:"id"`
	TopicID      int64      `json:"topic_id"`
	TopicTitle   string     `json:"topic_title,omitempty"`
	Kind         string     `json:"kind"`
	Title        string     `json:"title"`
	SourceURL    string     `json:"source_url,omitempty"`
	IngestMethod string     `json:"ingest_method,omitempty"`
	Content      string     `json:"content"`
	CreatedAt    *time.Time `json:"created_at,omitempty"`
	UpdatedAt    *time.Time `json:"updated_at,omitempty"`
}

type CreateDocumentRequest struct {
	TopicID      int64
	Kind         string
	Title        string
	SourceURL    string
	IngestMethod string
	Content      string
}

type UpdateDocumentRequest struct {
	DocumentID   int64
	Title        string
	SourceURL    string
	IngestMethod string
	Content      string
}

type ListDocumentsRangeRequest struct {
	TopicID int64
	Since   time.Time
	Until   time.Time
}

type RelevantChunk struct {
	ChunkIndex int     `json:"chunk_index"`
	Content    string  `json:"content"`
	Distance   float64 `json:"distance"`
}

type RelevantDocument struct {
	Document    Document        `json:"document"`
	Chunks      []RelevantChunk `json:"chunks"`
	MinDistance float64         `json:"min_distance"`
}

type Tag struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Category    string     `json:"category"`
	Documents   int        `json:"documents"`
	Occurrences int        `json:"occurrences"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
}

func NewStorage(ctx context.Context, dsn string, cfg Config) (*Storage, error) {
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

	storage := &Storage{
		pool:                pool,
		embedder:            cfg.Embedder,
		tagger:              cfg.Tagger,
		embeddingModel:      strings.TrimSpace(cfg.EmbeddingModel),
		chunkSize:           cfg.ChunkSize,
		vectorDims:          cfg.VectorDims,
		retrievalDocLimit:   cfg.RetrievalDocLimit,
		retrievalChunkLimit: cfg.RetrievalChunkLimit,
		retrievalThreshold:  cfg.RetrievalThreshold,
		tagLimit:            cfg.TagLimit,
	}
	if storage.chunkSize <= 0 {
		storage.chunkSize = defaultChunkSize
	}
	if storage.vectorDims < 0 {
		storage.vectorDims = 0
	}
	if storage.retrievalDocLimit <= 0 {
		storage.retrievalDocLimit = defaultRetrievalDocLimit
	}
	if storage.retrievalChunkLimit <= 0 {
		storage.retrievalChunkLimit = defaultRetrievalChunkLimit
	}
	if storage.retrievalThreshold <= 0 {
		storage.retrievalThreshold = defaultRetrievalThreshold
	}
	if storage.tagLimit <= 0 {
		storage.tagLimit = defaultTagLimit
	}
	if err := storage.initEmbeddingSchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	storage.startBackgroundBackfill(ctx)
	return storage, nil
}

func (s *Storage) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

func (s *Storage) startBackgroundBackfill(ctx context.Context) {
	go func() {
		started := time.Now()
		log.Infof("[pkb] background startup backfill started")
		if err := s.backfillMissingEmbeddings(ctx); err != nil {
			log.Errorf("[pkb] background embedding backfill failed: %v", err)
			return
		}
		if err := s.backfillMissingTags(ctx); err != nil {
			log.Errorf("[pkb] background tag backfill failed: %v", err)
			return
		}
		log.Infof("[pkb] background startup backfill completed in %s", time.Since(started).Truncate(time.Millisecond))
	}()
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
	if req.Kind != "note" && req.Kind != "url" && req.Kind != "file" {
		return Document{}, errors.New("kind must be note, url or file")
	}

	req.Title = strings.TrimSpace(req.Title)
	req.SourceURL = strings.TrimSpace(req.SourceURL)
	req.Content = strings.TrimSpace(req.Content)
	if req.Content == "" {
		return Document{}, errors.New("content is required")
	}
	if (req.Kind == "url" || req.Kind == "file") && req.SourceURL == "" {
		return Document{}, errors.New("source_url is required for kind=url/file")
	}

	query := `
INSERT INTO pkb_documents(topic_id, kind, title, source_url, ingest_method, content)
VALUES ($1,$2,$3,$4,$5,$6)
RETURNING id, topic_id, kind, title, source_url, ingest_method, content, created_at, updated_at;
`
	var d Document
	if err := s.pool.QueryRow(ctx, query, req.TopicID, req.Kind, req.Title, req.SourceURL, strings.TrimSpace(req.IngestMethod), req.Content).Scan(
		&d.ID, &d.TopicID, &d.Kind, &d.Title, &d.SourceURL, &d.IngestMethod, &d.Content, &d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		if isForeignKeyViolation(err) {
			return Document{}, fmt.Errorf("topic not found: %d", req.TopicID)
		}
		return Document{}, fmt.Errorf("create document failed: %w", err)
	}
	if err := s.rebuildEmbeddingsForDocument(ctx, d.ID, d.Content); err != nil {
		if _, delErr := s.pool.Exec(ctx, `DELETE FROM pkb_documents WHERE id = $1`, d.ID); delErr != nil {
			log.Warnf("[pkb] rollback document after embedding failure failed document_id=%d err=%v", d.ID, delErr)
		}
		return Document{}, err
	}
	if err := s.rebuildTagsForDocument(ctx, d.ID, d.Content); err != nil {
		if _, delErr := s.pool.Exec(ctx, `DELETE FROM pkb_documents WHERE id = $1`, d.ID); delErr != nil {
			log.Warnf("[pkb] rollback document after tag failure failed document_id=%d err=%v", d.ID, delErr)
		}
		return Document{}, err
	}
	return d, nil
}

func (s *Storage) ListDocuments(ctx context.Context, topicID int64) ([]Document, error) {
	if topicID <= 0 {
		return nil, errors.New("topic_id is required")
	}

	query := `
SELECT id, topic_id, kind, title, source_url, ingest_method, content, created_at, updated_at
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
		if err := rows.Scan(&d.ID, &d.TopicID, &d.Kind, &d.Title, &d.SourceURL, &d.IngestMethod, &d.Content, &d.CreatedAt, &d.UpdatedAt); err != nil {
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
SET title = $2, source_url = $3, ingest_method = CASE WHEN $4 = '' THEN ingest_method ELSE $4 END, content = $5, updated_at = NOW()
WHERE id = $1
RETURNING id, topic_id, kind, title, source_url, ingest_method, content, created_at, updated_at;
`
	var d Document
	if err := s.pool.QueryRow(ctx, query, req.DocumentID, req.Title, req.SourceURL, strings.TrimSpace(req.IngestMethod), req.Content).Scan(
		&d.ID, &d.TopicID, &d.Kind, &d.Title, &d.SourceURL, &d.IngestMethod, &d.Content, &d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return Document{}, fmt.Errorf("document not found: %d", req.DocumentID)
		}
		return Document{}, fmt.Errorf("update document failed: %w", err)
	}
	if err := s.rebuildEmbeddingsForDocument(ctx, d.ID, d.Content); err != nil {
		return Document{}, err
	}
	if err := s.rebuildTagsForDocument(ctx, d.ID, d.Content); err != nil {
		return Document{}, err
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

func (s *Storage) ListDocumentsByTimeRange(ctx context.Context, req ListDocumentsRangeRequest) ([]Document, error) {
	if req.Since.IsZero() || req.Until.IsZero() {
		return nil, errors.New("since and until are required")
	}
	if !req.Since.Before(req.Until) {
		return nil, errors.New("since must be before until")
	}

	query := `
SELECT d.id, d.topic_id, t.title, d.kind, d.title, d.source_url, d.ingest_method, d.content, d.created_at, d.updated_at
FROM pkb_documents d
JOIN pkb_topics t ON t.id = d.topic_id
WHERE d.created_at >= $1
  AND d.created_at < $2
  AND ($3::BIGINT = 0 OR d.topic_id = $3)
ORDER BY d.created_at DESC, d.id DESC;
`
	rows, err := s.pool.Query(ctx, query, req.Since.UTC(), req.Until.UTC(), req.TopicID)
	if err != nil {
		return nil, fmt.Errorf("list documents by time range failed: %w", err)
	}
	defer rows.Close()

	out := make([]Document, 0)
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.TopicID, &d.TopicTitle, &d.Kind, &d.Title, &d.SourceURL, &d.IngestMethod, &d.Content, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan ranged document failed: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ranged documents failed: %w", err)
	}
	return out, nil
}

func (s *Storage) ListTags(ctx context.Context) ([]Tag, error) {
	rows, err := s.pool.Query(ctx, `
SELECT
	t.id,
	t.name,
	t.category,
	t.created_at,
	COUNT(DISTINCT dt.document_id) AS documents,
	COALESCE(SUM(dt.occurrences), 0) AS occurrences
FROM pkb_tags t
LEFT JOIN pkb_document_tags dt ON dt.tag_id = t.id
GROUP BY t.id, t.name, t.category, t.created_at
ORDER BY occurrences DESC, documents DESC, t.name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list tags failed: %w", err)
	}
	defer rows.Close()

	out := make([]Tag, 0)
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&tag.ID, &tag.Name, &tag.Category, &tag.CreatedAt, &tag.Documents, &tag.Occurrences); err != nil {
			return nil, fmt.Errorf("scan tag failed: %w", err)
		}
		out = append(out, tag)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tags failed: %w", err)
	}
	return out, nil
}

func (s *Storage) ListDocumentsByTag(ctx context.Context, tagID int64) (Tag, []Document, error) {
	if tagID <= 0 {
		return Tag{}, nil, errors.New("tag_id is required")
	}

	var tag Tag
	if err := s.pool.QueryRow(ctx, `
SELECT
	t.id,
	t.name,
	t.category,
	t.created_at,
	COUNT(DISTINCT dt.document_id) AS documents,
	COALESCE(SUM(dt.occurrences), 0) AS occurrences
FROM pkb_tags t
LEFT JOIN pkb_document_tags dt ON dt.tag_id = t.id
WHERE t.id = $1
GROUP BY t.id, t.name, t.category, t.created_at
`, tagID).Scan(&tag.ID, &tag.Name, &tag.Category, &tag.CreatedAt, &tag.Documents, &tag.Occurrences); err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return Tag{}, nil, fmt.Errorf("tag not found: %d", tagID)
		}
		return Tag{}, nil, fmt.Errorf("load tag failed: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
SELECT
	d.id,
	d.topic_id,
	t.title,
	d.kind,
	d.title,
	d.source_url,
	d.ingest_method,
	d.content,
	d.created_at,
	d.updated_at
FROM pkb_document_tags dt
JOIN pkb_documents d ON d.id = dt.document_id
JOIN pkb_topics t ON t.id = d.topic_id
WHERE dt.tag_id = $1
ORDER BY dt.occurrences DESC, d.updated_at DESC, d.id DESC
`, tagID)
	if err != nil {
		return Tag{}, nil, fmt.Errorf("list documents by tag failed: %w", err)
	}
	defer rows.Close()

	docs := make([]Document, 0)
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.TopicID, &d.TopicTitle, &d.Kind, &d.Title, &d.SourceURL, &d.IngestMethod, &d.Content, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return Tag{}, nil, fmt.Errorf("scan document by tag failed: %w", err)
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		return Tag{}, nil, fmt.Errorf("iterate documents by tag failed: %w", err)
	}
	return tag, docs, nil
}

func (s *Storage) SearchRelevantDocuments(ctx context.Context, queryEmbeddings []float32, docLimit int, chunkLimit int) ([]RelevantDocument, error) {
	if len(queryEmbeddings) == 0 {
		return nil, nil
	}
	if docLimit <= 0 {
		docLimit = s.retrievalDocLimit
	}
	if chunkLimit <= 0 {
		chunkLimit = s.retrievalChunkLimit
	}

	rows, err := s.pool.Query(ctx, `
WITH compatible_embeddings AS (
	SELECT
		document_id,
		chunk_index,
		chunk_content,
		embeddings
	FROM pkb_embeddings
	WHERE embeddings IS NOT NULL
	  AND vector_dims(embeddings) = $2
)
SELECT
	e.document_id,
	e.chunk_index,
	e.chunk_content,
	e.embeddings <=> $1::vector AS distance,
	d.topic_id,
	t.title,
	d.kind,
	d.title,
d.source_url,
d.ingest_method,
d.content,
d.created_at,
d.updated_at
FROM compatible_embeddings e
JOIN pkb_documents d ON d.id = e.document_id
JOIN pkb_topics t ON t.id = d.topic_id
WHERE (e.embeddings <=> $1::vector) <= $3
ORDER BY e.embeddings <=> $1::vector
LIMIT $4
`, vectorLiteral(queryEmbeddings), len(queryEmbeddings), s.retrievalThreshold, max(docLimit*chunkLimit*4, 12))
	if err != nil {
		return nil, fmt.Errorf("search relevant pkb documents failed: %w", err)
	}
	defer rows.Close()

	type rowData struct {
		DocumentID   int64
		ChunkIndex   int
		ChunkContent string
		Distance     float64
		Document     Document
	}

	grouped := map[int64]*RelevantDocument{}
	order := make([]int64, 0, docLimit)
	for rows.Next() {
		var item rowData
		if err := rows.Scan(
			&item.DocumentID,
			&item.ChunkIndex,
			&item.ChunkContent,
			&item.Distance,
			&item.Document.TopicID,
			&item.Document.TopicTitle,
			&item.Document.Kind,
			&item.Document.Title,
			&item.Document.SourceURL,
			&item.Document.IngestMethod,
			&item.Document.Content,
			&item.Document.CreatedAt,
			&item.Document.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan relevant pkb row failed: %w", err)
		}
		item.Document.ID = item.DocumentID
		cur, ok := grouped[item.DocumentID]
		if !ok {
			if len(grouped) >= docLimit {
				continue
			}
			grouped[item.DocumentID] = &RelevantDocument{
				Document:    item.Document,
				MinDistance: item.Distance,
			}
			order = append(order, item.DocumentID)
			cur = grouped[item.DocumentID]
		}
		if item.Distance < cur.MinDistance {
			cur.MinDistance = item.Distance
		}
		if len(cur.Chunks) < chunkLimit {
			cur.Chunks = append(cur.Chunks, RelevantChunk{
				ChunkIndex: item.ChunkIndex,
				Content:    item.ChunkContent,
				Distance:   item.Distance,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate relevant pkb rows failed: %w", err)
	}

	result := make([]RelevantDocument, 0, len(grouped))
	for _, id := range order {
		doc := grouped[id]
		sort.Slice(doc.Chunks, func(i, j int) bool { return doc.Chunks[i].Distance < doc.Chunks[j].Distance })
		result = append(result, *doc)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].MinDistance < result[j].MinDistance })
	return result, nil
}

func (s *Storage) rebuildEmbeddingsForDocument(ctx context.Context, documentID int64, content string) error {
	if documentID <= 0 {
		return errors.New("document_id is required")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM pkb_embeddings WHERE document_id = $1`, documentID); err != nil {
		return fmt.Errorf("delete document embeddings failed: %w", err)
	}
	if s.embedder == nil || s.embeddingModel == "" {
		return nil
	}
	chunks := splitDocumentIntoChunks(content, s.chunkSize)
	if len(chunks) == 0 {
		return nil
	}
	vectors, err := s.embedder(ctx, chunks, s.embeddingModel)
	if err != nil {
		return fmt.Errorf("generate document chunk embeddings failed: %w", err)
	}
	if len(vectors) != len(chunks) {
		return fmt.Errorf("embedding response mismatch: expected %d vectors, got %d", len(chunks), len(vectors))
	}

	batch, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin document embedding batch failed: %w", err)
	}
	defer batch.Rollback(ctx)

	for i, chunk := range chunks {
		if _, err := batch.Exec(ctx, `
INSERT INTO pkb_embeddings(document_id, chunk_index, chunk_content, embeddings)
VALUES ($1, $2, $3, $4::vector)
`, documentID, i, chunk, vectorLiteral(vectors[i])); err != nil {
			return fmt.Errorf("store document chunk embedding failed: %w", err)
		}
	}
	if err := batch.Commit(ctx); err != nil {
		return fmt.Errorf("commit document chunk embeddings failed: %w", err)
	}
	log.Infof("[pkb] chunk embeddings updated document_id=%d chunks=%d dim=%d", documentID, len(chunks), len(vectors[0]))
	return nil
}

func (s *Storage) initEmbeddingSchema(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `DROP INDEX IF EXISTS pkb_embeddings_vector_idx`); err != nil {
		return fmt.Errorf("pkb init embedding schema failed: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
ALTER TABLE pkb_embeddings
ALTER COLUMN embeddings TYPE vector
USING embeddings::vector`); err != nil {
		return fmt.Errorf("pkb init embedding schema failed: %w", err)
	}
	if s.vectorDims <= 0 {
		return nil
	}

	var mismatched int
	if err := s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM pkb_embeddings
WHERE embeddings IS NOT NULL
  AND vector_dims(embeddings) <> $1
`, s.vectorDims).Scan(&mismatched); err != nil {
		return fmt.Errorf("pkb init embedding schema failed: %w", err)
	}
	if mismatched > 0 {
		log.Warnf("[pkb] detected %d chunk embeddings with dims different from %d; running without ivfflat index", mismatched, s.vectorDims)
		return nil
	}
	if _, err := s.pool.Exec(ctx, fmt.Sprintf(`
ALTER TABLE pkb_embeddings
ALTER COLUMN embeddings TYPE vector(%d)
USING embeddings::vector(%d)`, s.vectorDims, s.vectorDims)); err != nil {
		return fmt.Errorf("pkb init embedding schema failed: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
CREATE INDEX IF NOT EXISTS pkb_embeddings_vector_idx
ON pkb_embeddings USING ivfflat (embeddings vector_cosine_ops)
WITH (lists = 100)`); err != nil {
		return fmt.Errorf("pkb init embedding schema failed: %w", err)
	}
	return nil
}

func (s *Storage) backfillMissingEmbeddings(ctx context.Context) error {
	if s.embedder == nil || s.embeddingModel == "" {
		return nil
	}
	rows, err := s.pool.Query(ctx, `
SELECT d.id, d.content
FROM pkb_documents d
WHERE NOT EXISTS (
	SELECT 1 FROM pkb_embeddings e WHERE e.document_id = d.id
)
ORDER BY d.id ASC`)
	if err != nil {
		return fmt.Errorf("pkb backfill query failed: %w", err)
	}
	defer rows.Close()

	type docRow struct {
		ID      int64
		Content string
	}
	pending := make([]docRow, 0)
	for rows.Next() {
		var row docRow
		if err := rows.Scan(&row.ID, &row.Content); err != nil {
			return fmt.Errorf("pkb backfill scan failed: %w", err)
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pkb backfill iterate failed: %w", err)
	}
	for _, row := range pending {
		if err := s.rebuildEmbeddingsForDocument(ctx, row.ID, row.Content); err != nil {
			return fmt.Errorf("pkb backfill embeddings failed for document %d: %w", row.ID, err)
		}
	}
	if len(pending) > 0 {
		log.Infof("[pkb] backfilled chunk embeddings for %d existing documents", len(pending))
	}
	return nil
}

func (s *Storage) backfillMissingTags(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
SELECT d.id, d.content
FROM pkb_documents d
WHERE NOT EXISTS (
	SELECT 1 FROM pkb_document_tags dt WHERE dt.document_id = d.id
)
ORDER BY d.id ASC`)
	if err != nil {
		return fmt.Errorf("pkb tags backfill query failed: %w", err)
	}
	defer rows.Close()

	type docRow struct {
		ID      int64
		Content string
	}
	pending := make([]docRow, 0)
	for rows.Next() {
		var row docRow
		if err := rows.Scan(&row.ID, &row.Content); err != nil {
			return fmt.Errorf("pkb tags backfill scan failed: %w", err)
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pkb tags backfill iterate failed: %w", err)
	}
	for _, row := range pending {
		if err := s.rebuildTagsForDocument(ctx, row.ID, row.Content); err != nil {
			return fmt.Errorf("pkb tags backfill failed for document %d: %w", row.ID, err)
		}
	}
	if len(pending) > 0 {
		log.Infof("[pkb] rebuilt tags for %d existing documents", len(pending))
	}
	return nil
}

func (s *Storage) rebuildTagsForDocument(ctx context.Context, documentID int64, content string) error {
	if documentID <= 0 {
		return errors.New("document_id is required")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM pkb_document_tags WHERE document_id = $1`, documentID); err != nil {
		return fmt.Errorf("delete document tags failed: %w", err)
	}

	tagCounts, err := s.extractTagCounts(ctx, content, s.tagLimit)
	if err != nil {
		return err
	}
	if len(tagCounts) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin document tag transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)

	for _, item := range tagCounts {
		var tagID int64
		if err := tx.QueryRow(ctx, `
INSERT INTO pkb_tags(name, category)
VALUES ($1, $2)
ON CONFLICT ((LOWER(name)), category)
DO UPDATE SET name = EXCLUDED.name, category = EXCLUDED.category
RETURNING id
`, item.Tag, item.Category).Scan(&tagID); err != nil {
			return fmt.Errorf("upsert tag failed: %w", err)
		}

		if _, err := tx.Exec(ctx, `
INSERT INTO pkb_document_tags(document_id, tag_id, occurrences)
VALUES ($1, $2, $3)
ON CONFLICT (document_id, tag_id)
DO UPDATE SET occurrences = EXCLUDED.occurrences
`, documentID, tagID, item.Count); err != nil {
			return fmt.Errorf("associate document tag failed: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit document tags failed: %w", err)
	}
	log.Infof("[pkb] tags updated document_id=%d tags=%d", documentID, len(tagCounts))
	return nil
}

type tagCount struct {
	Tag      string
	Category string
	Count    int
}

func (s *Storage) extractTagCounts(ctx context.Context, content string, limit int) ([]tagCount, error) {
	if limit <= 0 {
		limit = defaultTagLimit
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, nil
	}
	if s.tagger == nil {
		return nil, nil
	}

	tags, err := s.tagger(ctx, content, limit)
	if err != nil {
		return nil, fmt.Errorf("extract tags with llm failed: %w", err)
	}
	normalized := normalizeLLMTags(tags, limit)
	out := make([]tagCount, 0, len(normalized))
	for _, tag := range normalized {
		out = append(out, tagCount{
			Tag:      tag.Text,
			Category: tag.Category,
			Count:    1,
		})
	}
	return out, nil
}

func normalizeLLMTags(tags []ExtractedTag, limit int) []ExtractedTag {
	if limit <= 0 {
		limit = defaultTagLimit
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]ExtractedTag, 0, min(len(tags), limit))
	for _, raw := range tags {
		value := strings.TrimSpace(raw.Text)
		if value == "" {
			continue
		}
		value = strings.ReplaceAll(value, "\n", " ")
		value = strings.ReplaceAll(value, "\t", " ")
		value = strings.Join(strings.Fields(value), " ")
		value = strings.Trim(value, ".,;:!?()[]{}<>\"`")
		if value == "" {
			continue
		}

		words := strings.Fields(value)
		if len(words) > 4 {
			value = strings.Join(words[:4], " ")
		}
		if len([]rune(value)) < 2 {
			continue
		}
		category := normalizeTagCategory(raw.Category)
		key := strings.ToLower(value) + "|" + category
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ExtractedTag{
			Text:     value,
			Category: category,
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func normalizeTagCategory(raw string) string {
	value := strings.ToUpper(strings.TrimSpace(raw))
	switch value {
	case "PERSON", "ORGANIZATION", "LOCATION", "DATE", "TIME", "MONEY", "PERCENT", "PRODUCT", "EVENT", "OTHER":
		return value
	default:
		return "OTHER"
	}
}

func splitDocumentIntoChunks(content string, chunkSize int) []string {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	paragraphs := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n\n")
	chunks := make([]string, 0, int(math.Max(1, float64(len(content)/chunkSize))))
	var current strings.Builder
	currentRunes := 0
	flush := func() {
		if currentRunes == 0 {
			return
		}
		chunks = append(chunks, strings.TrimSpace(current.String()))
		current.Reset()
		currentRunes = 0
	}
	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		paraRunes := len([]rune(para))
		if paraRunes > chunkSize {
			flush()
			runes := []rune(para)
			for start := 0; start < len(runes); start += chunkSize {
				end := start + chunkSize
				if end > len(runes) {
					end = len(runes)
				}
				chunks = append(chunks, strings.TrimSpace(string(runes[start:end])))
			}
			continue
		}
		if currentRunes > 0 && currentRunes+2+paraRunes > chunkSize {
			flush()
		}
		if currentRunes > 0 {
			current.WriteString("\n\n")
			currentRunes += 2
		}
		current.WriteString(para)
		currentRunes += paraRunes
	}
	flush()
	return chunks
}

func vectorLiteral(values []float32) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.FormatFloat(float64(v), 'f', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
