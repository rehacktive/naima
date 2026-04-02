package persona

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	openai "github.com/sashabaranov/go-openai"
	log "github.com/sirupsen/logrus"

	"naima/internal/memory"
)

const initSchemaQuery = `
CREATE TABLE IF NOT EXISTS persona_facts (
	id BIGSERIAL PRIMARY KEY,
	fact_key TEXT NOT NULL,
	fact_value TEXT NOT NULL,
	source TEXT NOT NULL DEFAULT 'inferred',
	confidence DOUBLE PRECISION NOT NULL DEFAULT 0.5,
	reason TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_persona_facts_key_value_lower
  ON persona_facts (LOWER(fact_key), LOWER(fact_value));
CREATE INDEX IF NOT EXISTS idx_persona_facts_key ON persona_facts (LOWER(fact_key), updated_at DESC);

CREATE TABLE IF NOT EXISTS persona_state (
	id BOOLEAN PRIMARY KEY DEFAULT TRUE,
	last_fingerprint TEXT NOT NULL DEFAULT '',
	last_extracted_at TIMESTAMPTZ NULL,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO persona_state(id) VALUES (TRUE) ON CONFLICT (id) DO NOTHING;
`

const (
	defaultExtractLookback = 24
	defaultExtractFacts    = 12
)

type MemorySource interface {
	GetMessages() []memory.Message
}

type Fact struct {
	ID         int64      `json:"id"`
	Key        string     `json:"key"`
	Value      string     `json:"value"`
	Source     string     `json:"source"`
	Confidence float64    `json:"confidence"`
	Reason     string     `json:"reason,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
}

type extractedFact struct {
	Key        string  `json:"key"`
	Value      string  `json:"value"`
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

type extractionPayload struct {
	Facts []extractedFact `json:"facts"`
}

type Manager struct {
	pool            *pgxpool.Pool
	client          *openai.Client
	chatModel       string
	interval        time.Duration
	lookback        int
	maxFacts        int
	memory          MemorySource
	mu              sync.Mutex
	dirty           bool
	running         bool
	singleValueKeys map[string]struct{}
}

func NewManager(ctx context.Context, dsn string, client *openai.Client, chatModel string, interval time.Duration, lookback int, maxFacts int) (*Manager, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("persona manager dsn is empty")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("persona manager db connect failed: %w", err)
	}
	if _, err := pool.Exec(ctx, initSchemaQuery); err != nil {
		pool.Close()
		return nil, fmt.Errorf("persona manager init schema failed: %w", err)
	}
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	if lookback <= 0 {
		lookback = defaultExtractLookback
	}
	if maxFacts <= 0 {
		maxFacts = defaultExtractFacts
	}
	return &Manager{
		pool:      pool,
		client:    client,
		chatModel: strings.TrimSpace(chatModel),
		interval:  interval,
		lookback:  lookback,
		maxFacts:  maxFacts,
		singleValueKeys: map[string]struct{}{
			"email":    {},
			"name":     {},
			"location": {},
			"timezone": {},
			"role":     {},
			"company":  {},
		},
	}, nil
}

func (m *Manager) Start(ctx context.Context, memory MemorySource) error {
	m.mu.Lock()
	m.memory = memory
	m.mu.Unlock()

	ticker := time.NewTicker(m.interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				m.pool.Close()
				return
			case <-ticker.C:
				if err := m.extractIfDirty(ctx); err != nil {
					log.Warnf("[persona] background extraction failed: %v", err)
				}
			}
		}
	}()
	return nil
}

func (m *Manager) MarkDirty() {
	m.mu.Lock()
	m.dirty = true
	m.mu.Unlock()
}

func (m *Manager) ListFacts(ctx context.Context) ([]Fact, error) {
	rows, err := m.pool.Query(ctx, `
SELECT id, fact_key, fact_value, source, confidence, reason, created_at, updated_at
FROM persona_facts
ORDER BY LOWER(fact_key), confidence DESC, updated_at DESC, id DESC;
`)
	if err != nil {
		return nil, fmt.Errorf("list persona facts failed: %w", err)
	}
	defer rows.Close()

	out := make([]Fact, 0)
	for rows.Next() {
		var fact Fact
		if err := rows.Scan(&fact.ID, &fact.Key, &fact.Value, &fact.Source, &fact.Confidence, &fact.Reason, &fact.CreatedAt, &fact.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan persona fact failed: %w", err)
		}
		out = append(out, fact)
	}
	return out, rows.Err()
}

func (m *Manager) GetFactsByKey(ctx context.Context, key string) ([]Fact, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("key is required")
	}
	rows, err := m.pool.Query(ctx, `
SELECT id, fact_key, fact_value, source, confidence, reason, created_at, updated_at
FROM persona_facts
WHERE LOWER(fact_key) = LOWER($1)
ORDER BY confidence DESC, updated_at DESC, id DESC;
`, key)
	if err != nil {
		return nil, fmt.Errorf("get persona facts failed: %w", err)
	}
	defer rows.Close()

	out := make([]Fact, 0)
	for rows.Next() {
		var fact Fact
		if err := rows.Scan(&fact.ID, &fact.Key, &fact.Value, &fact.Source, &fact.Confidence, &fact.Reason, &fact.CreatedAt, &fact.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan persona fact failed: %w", err)
		}
		out = append(out, fact)
	}
	return out, rows.Err()
}

func (m *Manager) BestFact(ctx context.Context, key string) (Fact, bool, error) {
	facts, err := m.GetFactsByKey(ctx, key)
	if err != nil {
		return Fact{}, false, err
	}
	if len(facts) == 0 {
		return Fact{}, false, nil
	}
	return facts[0], true, nil
}

func (m *Manager) IsEmpty(ctx context.Context) (bool, error) {
	var count int
	if err := m.pool.QueryRow(ctx, `SELECT COUNT(*) FROM persona_facts`).Scan(&count); err != nil {
		return false, fmt.Errorf("count persona facts failed: %w", err)
	}
	return count == 0, nil
}

func (m *Manager) SetFact(ctx context.Context, fact Fact) (Fact, error) {
	fact.Key = normalizeFactKey(fact.Key)
	fact.Value = strings.TrimSpace(fact.Value)
	if fact.Key == "" || fact.Value == "" {
		return Fact{}, errors.New("key and value are required")
	}
	fact.Source = normalizeFactSource(fact.Source)
	if fact.Confidence <= 0 {
		fact.Confidence = sourceDefaultConfidence(fact.Source)
	}
	if _, ok := m.singleValueKeys[fact.Key]; ok {
		if _, err := m.pool.Exec(ctx, `
DELETE FROM persona_facts
WHERE LOWER(fact_key) = LOWER($1)
  AND LOWER(fact_value) <> LOWER($2)
  AND (source <> 'explicit' OR $3 = 'explicit');
`, fact.Key, fact.Value, fact.Source); err != nil {
			return Fact{}, fmt.Errorf("prune persona facts failed: %w", err)
		}
	}

	var out Fact
	err := m.pool.QueryRow(ctx, `
INSERT INTO persona_facts(fact_key, fact_value, source, confidence, reason)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT ((LOWER(fact_key)), (LOWER(fact_value)))
DO UPDATE SET
	source = CASE
		WHEN persona_facts.source = 'explicit' AND EXCLUDED.source <> 'explicit' THEN persona_facts.source
		ELSE EXCLUDED.source
	END,
	confidence = GREATEST(persona_facts.confidence, EXCLUDED.confidence),
	reason = CASE
		WHEN persona_facts.source = 'explicit' AND EXCLUDED.source <> 'explicit' THEN persona_facts.reason
		WHEN EXCLUDED.reason <> '' THEN EXCLUDED.reason
		ELSE persona_facts.reason
	END,
	updated_at = NOW()
RETURNING id, fact_key, fact_value, source, confidence, reason, created_at, updated_at;
`, fact.Key, fact.Value, fact.Source, fact.Confidence, strings.TrimSpace(fact.Reason)).Scan(
		&out.ID, &out.Key, &out.Value, &out.Source, &out.Confidence, &out.Reason, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return Fact{}, fmt.Errorf("upsert persona fact failed: %w", err)
	}
	return out, nil
}

func (m *Manager) DeleteFact(ctx context.Context, key string, value string) error {
	key = normalizeFactKey(key)
	value = strings.TrimSpace(value)
	if key == "" {
		return errors.New("key is required")
	}
	query := `DELETE FROM persona_facts WHERE LOWER(fact_key) = LOWER($1)`
	args := []any{key}
	if value != "" {
		query += ` AND LOWER(fact_value) = LOWER($2)`
		args = append(args, value)
	}
	if _, err := m.pool.Exec(ctx, query, args...); err != nil {
		return fmt.Errorf("delete persona fact failed: %w", err)
	}
	return nil
}

func (m *Manager) extractIfDirty(ctx context.Context) error {
	m.mu.Lock()
	if !m.dirty || m.running {
		m.mu.Unlock()
		return nil
	}
	m.running = true
	m.dirty = false
	memory := m.memory
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()

	if memory == nil || m.client == nil || m.chatModel == "" {
		return nil
	}
	messages := recentMessages(memory.GetMessages(), m.lookback)
	if len(messages) == 0 {
		return nil
	}

	fingerprint := fingerprintMessages(messages)
	same, err := m.isFingerprintCurrent(ctx, fingerprint)
	if err != nil || same {
		return err
	}

	extractCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	facts, err := m.extractFacts(extractCtx, messages)
	if err != nil {
		return err
	}
	for _, fact := range facts {
		if _, err := m.SetFact(extractCtx, Fact{
			Key:        fact.Key,
			Value:      fact.Value,
			Source:     fact.Source,
			Confidence: fact.Confidence,
			Reason:     fact.Reason,
		}); err != nil {
			log.Warnf("[persona] store fact failed key=%s err=%v", fact.Key, err)
		}
	}
	if _, err := m.pool.Exec(extractCtx, `
UPDATE persona_state
SET last_fingerprint = $1, last_extracted_at = NOW(), updated_at = NOW()
WHERE id = TRUE;
`, fingerprint); err != nil {
		return fmt.Errorf("update persona state failed: %w", err)
	}
	log.Infof("[persona] extracted %d facts from recent conversation", len(facts))
	return nil
}

func (m *Manager) extractFacts(ctx context.Context, messages []memory.Message) ([]extractedFact, error) {
	lines := make([]string, 0, len(messages))
	for _, msg := range messages {
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "unknown"
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", role, truncate(content, 400)))
	}
	if len(lines) == 0 {
		return nil, nil
	}

	systemPrompt := "You extract stable or useful persona facts about the human user from conversation snippets. " +
		"Return JSON only as {\"facts\":[...]} with keys like email, name, location, timezone, role, company, interest, news_interest, preference, goal. " +
		"Only include facts that are about the user, not the assistant. Skip guesses. Mark source as explicit if the user directly stated it; otherwise inferred."
	userPrompt := fmt.Sprintf("Recent conversation:\n%s\n\nReturn at most %d facts.", strings.Join(lines, "\n"), m.maxFacts)

	resp, err := m.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: m.chatModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userPrompt},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("persona extractor returned no choices")
	}

	raw := strings.TrimSpace(resp.Choices[0].Message.Content)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var payload extractionPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("parse persona extraction failed: %w", err)
	}

	out := make([]extractedFact, 0, len(payload.Facts))
	seen := make(map[string]struct{}, len(payload.Facts))
	for _, fact := range payload.Facts {
		fact.Key = normalizeFactKey(fact.Key)
		fact.Value = strings.TrimSpace(fact.Value)
		if fact.Key == "" || fact.Value == "" {
			continue
		}
		fact.Source = normalizeFactSource(fact.Source)
		if fact.Confidence <= 0 {
			fact.Confidence = sourceDefaultConfidence(fact.Source)
		}
		key := strings.ToLower(fact.Key) + "::" + strings.ToLower(fact.Value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, fact)
		if len(out) >= m.maxFacts {
			break
		}
	}
	return out, nil
}

func (m *Manager) isFingerprintCurrent(ctx context.Context, fingerprint string) (bool, error) {
	var current string
	if err := m.pool.QueryRow(ctx, `SELECT last_fingerprint FROM persona_state WHERE id = TRUE`).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("load persona state failed: %w", err)
	}
	return strings.TrimSpace(current) == strings.TrimSpace(fingerprint), nil
}

func recentMessages(messages []memory.Message, limit int) []memory.Message {
	if limit <= 0 || len(messages) <= limit {
		return append([]memory.Message(nil), messages...)
	}
	out := append([]memory.Message(nil), messages[len(messages)-limit:]...)
	sort.SliceStable(out, func(i, j int) bool {
		a := out[i].CreatedAt
		b := out[j].CreatedAt
		if a == nil || b == nil {
			return i < j
		}
		return a.Before(*b)
	})
	return out
}

func fingerprintMessages(messages []memory.Message) string {
	var b strings.Builder
	for _, msg := range messages {
		b.WriteString(strings.TrimSpace(msg.Role))
		b.WriteString("|")
		if msg.CreatedAt != nil {
			b.WriteString(msg.CreatedAt.UTC().Format(time.RFC3339Nano))
		}
		b.WriteString("|")
		b.WriteString(strings.TrimSpace(msg.Content))
		b.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func normalizeFactKey(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "email", "name", "location", "timezone", "role", "company", "interest", "news_interest", "preference", "goal":
		return v
	default:
		return v
	}
}

func normalizeFactSource(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "explicit":
		return "explicit"
	default:
		return "inferred"
	}
}

func sourceDefaultConfidence(source string) float64 {
	if source == "explicit" {
		return 0.95
	}
	return 0.65
}

func truncate(v string, limit int) string {
	v = strings.TrimSpace(v)
	if limit <= 0 || len(v) <= limit {
		return v
	}
	return v[:limit] + "..."
}
