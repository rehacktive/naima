package memory

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	memstorage "github.com/rehacktive/memorya/storage"
	log "github.com/sirupsen/logrus"
)

const (
	defaultSearchLimit = 5
	queryTimeout       = 5 * time.Second
)

type PGVectorStorage struct {
	pool        *pgxpool.Pool
	searchLimit int
	vectorDims  int
}

func NewPGVectorStorage(ctx context.Context, dsn string, searchLimit int, vectorDims int) (*PGVectorStorage, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("pgvector dsn is empty")
	}
	if searchLimit <= 0 {
		searchLimit = defaultSearchLimit
	}
	if vectorDims < 0 {
		vectorDims = 0
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("create pgx pool failed: %w", err)
	}

	storage := &PGVectorStorage{pool: pool, searchLimit: searchLimit, vectorDims: vectorDims}
	if err := storage.initSchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}

	return storage, nil
}

func (s *PGVectorStorage) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

func (s *PGVectorStorage) StoreMessage(message memstorage.Message) error {
	if message.Id == "" {
		id, err := randomID()
		if err != nil {
			return err
		}
		message.Id = memstorage.ID(id)
	}
	if message.CreatedAt == nil {
		now := time.Now().UTC()
		message.CreatedAt = &now
	}

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	query := `
		INSERT INTO memory_messages (id, created_at, role, content, cost, pinned, embeddings)
		VALUES ($1, $2, $3, $4, $5, $6, $7::vector)
		ON CONFLICT (id) DO NOTHING
	`

	var embeddings any
	if message.Embeddings != nil && len(*message.Embeddings) > 0 {
		embeddings = vectorLiteral(*message.Embeddings)
	}

	if _, err := s.pool.Exec(
		ctx,
		query,
		string(message.Id),
		*message.CreatedAt,
		message.Role,
		message.Content,
		message.Cost,
		message.Pinned,
		embeddings,
	); err != nil {
		return fmt.Errorf("store message failed: %w", err)
	}

	return nil
}

func (s *PGVectorStorage) SearchRelatedMessages(queryEmbeddings []float32) ([]memstorage.Message, error) {
	if len(queryEmbeddings) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	rows, err := s.pool.Query(
		ctx,
		`
			SELECT id, created_at, role, content, cost, pinned
			FROM memory_messages
			WHERE embeddings IS NOT NULL
			  AND vector_dims(embeddings) = $2
			ORDER BY embeddings <=> $1::vector
			LIMIT $3
		`,
		vectorLiteral(queryEmbeddings),
		len(queryEmbeddings),
		s.searchLimit,
	)
	if err != nil {
		return nil, fmt.Errorf("search related messages failed: %w", err)
	}
	defer rows.Close()

	result := make([]memstorage.Message, 0, s.searchLimit)
	for rows.Next() {
		var (
			id        string
			createdAt time.Time
			msg       memstorage.Message
		)
		if err := rows.Scan(&id, &createdAt, &msg.Role, &msg.Content, &msg.Cost, &msg.Pinned); err != nil {
			return nil, fmt.Errorf("scan related message failed: %w", err)
		}
		msg.Id = memstorage.ID(id)
		msg.CreatedAt = &createdAt
		result = append(result, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate related messages failed: %w", err)
	}

	return result, nil
}

func (s *PGVectorStorage) initSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		`
			CREATE TABLE IF NOT EXISTS memory_messages (
				id TEXT PRIMARY KEY,
				created_at TIMESTAMPTZ NOT NULL,
				role TEXT NOT NULL,
				content TEXT NOT NULL,
				cost INTEGER NOT NULL DEFAULT 0,
				pinned BOOLEAN NOT NULL DEFAULT FALSE,
				embeddings VECTOR
			)
		`,
	}

	for _, stmt := range stmts {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("init pgvector schema failed: %w", err)
		}
	}

	// Always keep the column flexible first so changing embedding models/dims
	// does not break startup or inserts.
	if _, err := s.pool.Exec(ctx, `DROP INDEX IF EXISTS memory_messages_embeddings_idx`); err != nil {
		return fmt.Errorf("init pgvector schema failed: %w", err)
	}
	if _, err := s.pool.Exec(
		ctx,
		`ALTER TABLE memory_messages
		 ALTER COLUMN embeddings TYPE vector
		 USING embeddings::vector`,
	); err != nil {
		return fmt.Errorf("init pgvector schema failed: %w", err)
	}

	if s.vectorDims <= 0 {
		return nil
	}

	var mismatched int
	if err := s.pool.QueryRow(
		ctx,
		`SELECT COUNT(*) FROM memory_messages
		 WHERE embeddings IS NOT NULL
		   AND vector_dims(embeddings) <> $1`,
		s.vectorDims,
	).Scan(&mismatched); err != nil {
		return fmt.Errorf("init pgvector schema failed: %w", err)
	}
	if mismatched > 0 {
		log.Warnf(
			"[memory] detected %d messages with embedding dims different from %d; running without ivfflat index",
			mismatched,
			s.vectorDims,
		)
		return nil
	}

	if _, err := s.pool.Exec(
		ctx,
		fmt.Sprintf(
			`ALTER TABLE memory_messages
			 ALTER COLUMN embeddings TYPE vector(%d)
			 USING embeddings::vector(%d)`,
			s.vectorDims,
			s.vectorDims,
		),
	); err != nil {
		return fmt.Errorf("init pgvector schema failed: %w", err)
	}

	if _, err := s.pool.Exec(
		ctx,
		`CREATE INDEX IF NOT EXISTS memory_messages_embeddings_idx
		 ON memory_messages USING ivfflat (embeddings vector_cosine_ops)
		 WITH (lists = 100)`,
	); err != nil {
		return fmt.Errorf("init pgvector schema failed: %w", err)
	}
	log.Infof("[memory] pgvector ivfflat index enabled for embeddings dim=%d", s.vectorDims)

	return nil
}

func vectorLiteral(values []float32) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.FormatFloat(float64(v), 'f', -1, 32)
	}

	return "[" + strings.Join(parts, ",") + "]"
}
