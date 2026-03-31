package research

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	log "github.com/sirupsen/logrus"

	"naima/internal/pkb"
)

const initSchemaQuery = `
CREATE TABLE IF NOT EXISTS deep_research_runs (
	id BIGSERIAL PRIMARY KEY,
	status TEXT NOT NULL DEFAULT 'queued',
	topic_title TEXT NOT NULL,
	topic_id BIGINT NOT NULL DEFAULT 0,
	brief TEXT NOT NULL,
	guide_title TEXT NOT NULL DEFAULT '',
	language TEXT NOT NULL DEFAULT '',
	time_range TEXT NOT NULL DEFAULT '',
	max_sources INTEGER NOT NULL DEFAULT 6,
	max_queries INTEGER NOT NULL DEFAULT 5,
	notify_telegram BOOLEAN NOT NULL DEFAULT TRUE,
	guide_document_id BIGINT NOT NULL DEFAULT 0,
	response_document_id BIGINT NOT NULL DEFAULT 0,
	source_documents_count INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	logs TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	started_at TIMESTAMPTZ NULL,
	completed_at TIMESTAMPTZ NULL,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_deep_research_runs_status ON deep_research_runs(status, created_at DESC);
`

const (
	StatusQueued      = "queued"
	StatusInProgress  = "in_progress"
	StatusCompleted   = "completed"
	StatusFailed      = "failed"
	StatusCanceled    = "canceled"
	defaultListLimit  = 50
	defaultRunTimeout = 25 * time.Minute
)

type Notifier interface {
	Enabled() bool
	Send(text string) error
}

type PKBStore interface {
	CreateTopic(ctx context.Context, title string) (pkb.Topic, error)
	ListTopics(ctx context.Context) ([]pkb.Topic, error)
	CreateDocument(ctx context.Context, req pkb.CreateDocumentRequest) (pkb.Document, error)
	ListDocuments(ctx context.Context, topicID int64) ([]pkb.Document, error)
}

type CreateRunRequest struct {
	Topic          string
	Note           string
	GuideTitle     string
	Language       string
	TimeRange      string
	MaxSources     int
	MaxQueries     int
	NotifyTelegram bool
}

type Run struct {
	ID                   int64      `json:"id"`
	Status               string     `json:"status"`
	TopicTitle           string     `json:"topic_title"`
	TopicID              int64      `json:"topic_id"`
	Brief                string     `json:"brief"`
	GuideTitle           string     `json:"guide_title,omitempty"`
	Language             string     `json:"language,omitempty"`
	TimeRange            string     `json:"time_range,omitempty"`
	MaxSources           int        `json:"max_sources"`
	MaxQueries           int        `json:"max_queries"`
	NotifyTelegram       bool       `json:"notify_telegram"`
	GuideDocumentID      int64      `json:"guide_document_id,omitempty"`
	ResponseDocumentID   int64      `json:"response_document_id,omitempty"`
	SourceDocumentsCount int        `json:"source_documents_count"`
	Error                string     `json:"error,omitempty"`
	Logs                 string     `json:"logs,omitempty"`
	CreatedAt            *time.Time `json:"created_at,omitempty"`
	StartedAt            *time.Time `json:"started_at,omitempty"`
	CompletedAt          *time.Time `json:"completed_at,omitempty"`
	UpdatedAt            *time.Time `json:"updated_at,omitempty"`
}

type Manager struct {
	pool     *pgxpool.Pool
	runner   *Runner
	notifier Notifier

	mu      sync.Mutex
	running map[int64]struct{}
	cancel  map[int64]context.CancelFunc
}

func NewManager(ctx context.Context, dsn string, store PKBStore, client ChatCompleter, chatModel string, ingestCfg pkb.IngestConfig, searxURL string, notifier Notifier) (*Manager, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("research manager dsn is empty")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("research manager db connect failed: %w", err)
	}
	if _, err := pool.Exec(ctx, initSchemaQuery); err != nil {
		pool.Close()
		return nil, fmt.Errorf("research manager init schema failed: %w", err)
	}
	return &Manager{
		pool:     pool,
		runner:   NewRunner(store, client, chatModel, ingestCfg, searxURL),
		notifier: notifier,
		running:  make(map[int64]struct{}),
		cancel:   make(map[int64]context.CancelFunc),
	}, nil
}

func (m *Manager) Start(ctx context.Context) error {
	rows, err := m.pool.Query(ctx, `
SELECT id
FROM deep_research_runs
WHERE status IN ('queued', 'in_progress')
ORDER BY id ASC;
`)
	if err != nil {
		return fmt.Errorf("load pending research runs failed: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan research run failed: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate research runs failed: %w", err)
	}

	for _, id := range ids {
		m.schedule(id)
	}

	go func() {
		<-ctx.Done()
		m.pool.Close()
	}()
	return nil
}

func (m *Manager) CreateRun(ctx context.Context, req CreateRunRequest) (Run, error) {
	req.Topic = strings.TrimSpace(req.Topic)
	req.Note = strings.TrimSpace(req.Note)
	if req.Topic == "" {
		return Run{}, errors.New("topic is required")
	}
	if req.Note == "" {
		return Run{}, errors.New("note is required")
	}
	if req.MaxSources <= 0 {
		req.MaxSources = 6
	}
	if req.MaxQueries <= 0 {
		req.MaxQueries = 5
	}

	var run Run
	err := m.pool.QueryRow(ctx, `
INSERT INTO deep_research_runs(status, topic_title, brief, guide_title, language, time_range, max_sources, max_queries, notify_telegram)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
RETURNING id, status, topic_title, topic_id, brief, guide_title, language, time_range, max_sources, max_queries, notify_telegram, guide_document_id, response_document_id, source_documents_count, error, logs, created_at, started_at, completed_at, updated_at;
`,
		StatusQueued,
		req.Topic,
		req.Note,
		strings.TrimSpace(req.GuideTitle),
		strings.TrimSpace(req.Language),
		strings.TrimSpace(req.TimeRange),
		req.MaxSources,
		req.MaxQueries,
		req.NotifyTelegram,
	).Scan(
		&run.ID, &run.Status, &run.TopicTitle, &run.TopicID, &run.Brief, &run.GuideTitle, &run.Language, &run.TimeRange, &run.MaxSources, &run.MaxQueries, &run.NotifyTelegram, &run.GuideDocumentID, &run.ResponseDocumentID, &run.SourceDocumentsCount, &run.Error, &run.Logs, &run.CreatedAt, &run.StartedAt, &run.CompletedAt, &run.UpdatedAt,
	)
	if err != nil {
		return Run{}, fmt.Errorf("create research run failed: %w", err)
	}
	m.schedule(run.ID)
	return run, nil
}

func (m *Manager) GetRun(ctx context.Context, id int64) (Run, error) {
	var run Run
	err := m.pool.QueryRow(ctx, `
SELECT id, status, topic_title, topic_id, brief, guide_title, language, time_range, max_sources, max_queries, notify_telegram, guide_document_id, response_document_id, source_documents_count, error, logs, created_at, started_at, completed_at, updated_at
FROM deep_research_runs
WHERE id = $1;
`, id).Scan(
		&run.ID, &run.Status, &run.TopicTitle, &run.TopicID, &run.Brief, &run.GuideTitle, &run.Language, &run.TimeRange, &run.MaxSources, &run.MaxQueries, &run.NotifyTelegram, &run.GuideDocumentID, &run.ResponseDocumentID, &run.SourceDocumentsCount, &run.Error, &run.Logs, &run.CreatedAt, &run.StartedAt, &run.CompletedAt, &run.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Run{}, fmt.Errorf("research run not found: %d", id)
		}
		return Run{}, fmt.Errorf("load research run failed: %w", err)
	}
	return run, nil
}

func (m *Manager) ListRuns(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > defaultListLimit {
		limit = defaultListLimit
	}
	rows, err := m.pool.Query(ctx, `
SELECT id, status, topic_title, topic_id, brief, guide_title, language, time_range, max_sources, max_queries, notify_telegram, guide_document_id, response_document_id, source_documents_count, error, logs, created_at, started_at, completed_at, updated_at
FROM deep_research_runs
ORDER BY id DESC
LIMIT $1;
`, limit)
	if err != nil {
		return nil, fmt.Errorf("list research runs failed: %w", err)
	}
	defer rows.Close()

	out := make([]Run, 0)
	for rows.Next() {
		var run Run
		if err := rows.Scan(
			&run.ID, &run.Status, &run.TopicTitle, &run.TopicID, &run.Brief, &run.GuideTitle, &run.Language, &run.TimeRange, &run.MaxSources, &run.MaxQueries, &run.NotifyTelegram, &run.GuideDocumentID, &run.ResponseDocumentID, &run.SourceDocumentsCount, &run.Error, &run.Logs, &run.CreatedAt, &run.StartedAt, &run.CompletedAt, &run.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan research run failed: %w", err)
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate research runs failed: %w", err)
	}
	return out, nil
}

func (m *Manager) CancelRun(ctx context.Context, id int64) (Run, error) {
	run, err := m.GetRun(ctx, id)
	if err != nil {
		return Run{}, err
	}
	switch run.Status {
	case StatusCompleted, StatusFailed, StatusCanceled:
		return Run{}, fmt.Errorf("research run is already finished: %s", run.Status)
	}

	now := time.Now().UTC()
	if _, err := m.pool.Exec(ctx, `
UPDATE deep_research_runs
SET status = $2, error = '', completed_at = $3, updated_at = NOW()
WHERE id = $1;
`, id, StatusCanceled, now); err != nil {
		return Run{}, fmt.Errorf("cancel research run failed: %w", err)
	}

	m.mu.Lock()
	cancelFn := m.cancel[id]
	m.mu.Unlock()
	if cancelFn != nil {
		cancelFn()
	}
	m.appendLog(ctx, id, "research canceled")
	return m.GetRun(ctx, id)
}

func (m *Manager) DeleteRun(ctx context.Context, id int64) error {
	run, err := m.GetRun(ctx, id)
	if err != nil {
		return err
	}
	if run.Status == StatusInProgress {
		return fmt.Errorf("cannot delete research run while it is in progress; cancel it first")
	}
	if _, err := m.pool.Exec(ctx, `DELETE FROM deep_research_runs WHERE id = $1;`, id); err != nil {
		return fmt.Errorf("delete research run failed: %w", err)
	}
	return nil
}

func (m *Manager) schedule(id int64) {
	m.mu.Lock()
	if _, ok := m.running[id]; ok {
		m.mu.Unlock()
		return
	}
	m.running[id] = struct{}{}
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.running, id)
			delete(m.cancel, id)
			m.mu.Unlock()
		}()
		m.execute(id)
	}()
}

func (m *Manager) execute(id int64) {
	baseCtx, baseCancel := context.WithTimeout(context.Background(), defaultRunTimeout)
	defer baseCancel()
	ctx, cancel := context.WithCancel(baseCtx)
	m.mu.Lock()
	m.cancel[id] = cancel
	m.mu.Unlock()
	defer cancel()

	run, err := m.GetRun(ctx, id)
	if err != nil {
		log.Warnf("[research] load run failed id=%d err=%v", id, err)
		return
	}

	startedAt := time.Now().UTC()
	if _, err := m.pool.Exec(ctx, `
UPDATE deep_research_runs
SET status = $2, started_at = COALESCE(started_at, $3), updated_at = NOW(), error = ''
WHERE id = $1;
`, id, StatusInProgress, startedAt); err != nil {
		log.Warnf("[research] mark in-progress failed id=%d err=%v", id, err)
		return
	}
	m.appendLog(ctx, id, "research started")

	result, err := m.runner.Execute(ctx, ExecuteRequest{
		Topic:      run.TopicTitle,
		Note:       run.Brief,
		GuideTitle: run.GuideTitle,
		Language:   run.Language,
		TimeRange:  run.TimeRange,
		MaxSources: run.MaxSources,
		MaxQueries: run.MaxQueries,
	}, func(message string) {
		m.appendLog(context.Background(), id, message)
	})
	if err != nil {
		status := StatusFailed
		errMsg := err.Error()
		if errors.Is(err, context.Canceled) {
			status = StatusCanceled
			errMsg = ""
		}
		completedAt := time.Now().UTC()
		_, updErr := m.pool.Exec(ctx, `
UPDATE deep_research_runs
SET status = $2, error = $3, completed_at = $4, updated_at = NOW()
WHERE id = $1;
`, id, status, errMsg, completedAt)
		if updErr != nil {
			log.Warnf("[research] update failure state failed id=%d err=%v", id, updErr)
		}
		if status == StatusCanceled {
			m.appendLog(context.Background(), id, "research canceled during execution")
			log.Infof("[research] execution canceled id=%d", id)
		} else {
			m.appendLog(context.Background(), id, "research failed: "+err.Error())
			log.Warnf("[research] execution failed id=%d err=%v", id, err)
		}
		return
	}

	completedAt := time.Now().UTC()
	_, err = m.pool.Exec(ctx, `
UPDATE deep_research_runs
SET status = $2, topic_id = $3, guide_document_id = $4, response_document_id = $5, source_documents_count = $6, completed_at = $7, updated_at = NOW(), error = ''
WHERE id = $1;
`, id, StatusCompleted, result.Topic.ID, result.GuideDocument.ID, result.ResponseDocument.ID, len(result.SourceDocuments), completedAt)
	if err != nil {
		log.Warnf("[research] update completion state failed id=%d err=%v", id, err)
	}
	m.appendLog(context.Background(), id, fmt.Sprintf("research completed with %d source documents", len(result.SourceDocuments)))

	if run.NotifyTelegram && m.notifier != nil && m.notifier.Enabled() {
		text := fmt.Sprintf("Deep research completed\n\nTopic: %s\nRun ID: %d\nStatus: %s\nResponse document: #%d", result.Topic.Title, id, StatusCompleted, result.ResponseDocument.ID)
		if err := m.notifier.Send(text); err != nil {
			m.appendLog(context.Background(), id, "telegram notification failed: "+err.Error())
		} else {
			m.appendLog(context.Background(), id, "telegram notification sent")
		}
	}
}

func (m *Manager) appendLog(ctx context.Context, id int64, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	line := time.Now().UTC().Format(time.RFC3339) + " " + message
	if _, err := m.pool.Exec(ctx, `
UPDATE deep_research_runs
SET logs = CASE WHEN logs = '' THEN $2 ELSE logs || E'\n' || $2 END, updated_at = NOW()
WHERE id = $1;
`, id, line); err != nil {
		log.Warnf("[research] append log failed id=%d err=%v", id, err)
		return
	}
	log.Infof("[research] id=%d %s", id, message)
}
