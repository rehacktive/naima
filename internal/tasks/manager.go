package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"
	log "github.com/sirupsen/logrus"

	"naima/internal/agent"
	"naima/internal/telegram"
	"naima/internal/tools"
)

const initSchemaQuery = `
CREATE TABLE IF NOT EXISTS scheduled_tasks (
	id BIGSERIAL PRIMARY KEY,
	kind TEXT NOT NULL DEFAULT 'agent',
	title TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL,
	schedule_type TEXT NOT NULL,
	run_at TIMESTAMPTZ NULL,
	cron_expr TEXT NULL,
	send_telegram BOOLEAN NOT NULL DEFAULT TRUE,
	active BOOLEAN NOT NULL DEFAULT TRUE,
	last_run_at TIMESTAMPTZ NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_scheduled_tasks_active ON scheduled_tasks(active);
`

type Manager struct {
	pool     *pgxpool.Pool
	agent    *agent.Agent
	notifier *telegram.Notifier

	cronEngine *cron.Cron
	parser     cron.Parser

	mu          sync.Mutex
	cronEntries map[int64]cron.EntryID
	timers      map[int64]*time.Timer
}

func NewManager(ctx context.Context, dsn string, notifier *telegram.Notifier, location *time.Location) (*Manager, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("task manager dsn is empty")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("task manager db connect failed: %w", err)
	}
	if _, err := pool.Exec(ctx, initSchemaQuery); err != nil {
		pool.Close()
		return nil, fmt.Errorf("task manager init schema failed: %w", err)
	}
	if location == nil {
		location = time.Local
	}

	engine := cron.New(cron.WithLocation(location))
	return &Manager{
		pool:       pool,
		notifier:   notifier,
		cronEngine: engine,
		parser: cron.NewParser(
			cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
		),
		cronEntries: make(map[int64]cron.EntryID),
		timers:      make(map[int64]*time.Timer),
	}, nil
}

func (m *Manager) SetAgent(a *agent.Agent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.agent = a
}

func (m *Manager) Start(ctx context.Context) error {
	if err := m.reload(ctx); err != nil {
		return err
	}
	m.cronEngine.Start()

	go func() {
		<-ctx.Done()
		stopCtx := m.cronEngine.Stop()
		select {
		case <-stopCtx.Done():
		case <-time.After(2 * time.Second):
		}
		m.mu.Lock()
		for id, t := range m.timers {
			t.Stop()
			delete(m.timers, id)
		}
		m.mu.Unlock()
		m.pool.Close()
	}()
	return nil
}

func (m *Manager) CreateTask(ctx context.Context, req tools.CreateScheduledTaskRequest) (tools.ScheduledTask, error) {
	req.Kind = strings.ToLower(strings.TrimSpace(req.Kind))
	if req.Kind == "" {
		req.Kind = "agent"
	}
	if req.Kind != "agent" && req.Kind != "alarm" {
		return tools.ScheduledTask{}, fmt.Errorf("invalid kind: %s", req.Kind)
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		return tools.ScheduledTask{}, errors.New("content is required")
	}

	scheduleType := "once"
	if strings.TrimSpace(req.CronExpr) != "" {
		scheduleType = "cron"
		if _, err := m.parser.Parse(req.CronExpr); err != nil {
			return tools.ScheduledTask{}, fmt.Errorf("invalid cron expression: %w", err)
		}
	} else if req.RunAt == nil {
		return tools.ScheduledTask{}, errors.New("run_at is required when cron is empty")
	}
	if req.RunAt != nil && req.RunAt.Before(time.Now().UTC()) && scheduleType == "once" {
		return tools.ScheduledTask{}, errors.New("run_at must be in the future")
	}

	sendTg := req.SendTelegram
	task := tools.ScheduledTask{}
	query := `
INSERT INTO scheduled_tasks(kind, title, content, schedule_type, run_at, cron_expr, send_telegram, active)
VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE)
RETURNING id, kind, title, content, schedule_type, run_at, cron_expr, send_telegram, active, last_run_at, created_at;
`
	err := m.pool.QueryRow(ctx, query,
		req.Kind,
		strings.TrimSpace(req.Title),
		content,
		scheduleType,
		req.RunAt,
		strings.TrimSpace(req.CronExpr),
		sendTg,
	).Scan(
		&task.ID,
		&task.Kind,
		&task.Title,
		&task.Content,
		&task.ScheduleType,
		&task.RunAt,
		&task.CronExpr,
		&task.SendTelegram,
		&task.Active,
		&task.LastRunAt,
		&task.CreatedAt,
	)
	if err != nil {
		return tools.ScheduledTask{}, fmt.Errorf("insert task failed: %w", err)
	}

	m.scheduleTask(task)
	log.Infof("[tasks] task created id=%d kind=%s schedule=%s", task.ID, task.Kind, task.ScheduleType)
	return task, nil
}

func (m *Manager) ListTasks(ctx context.Context) ([]tools.ScheduledTask, error) {
	rows, err := m.pool.Query(ctx, `
SELECT id, kind, title, content, schedule_type, run_at, cron_expr, send_telegram, active, last_run_at, created_at
FROM scheduled_tasks
ORDER BY id DESC
LIMIT 100;
`)
	if err != nil {
		return nil, fmt.Errorf("query tasks failed: %w", err)
	}
	defer rows.Close()

	out := make([]tools.ScheduledTask, 0)
	for rows.Next() {
		var t tools.ScheduledTask
		if err := rows.Scan(
			&t.ID, &t.Kind, &t.Title, &t.Content, &t.ScheduleType, &t.RunAt, &t.CronExpr, &t.SendTelegram, &t.Active, &t.LastRunAt, &t.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan task failed: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks failed: %w", err)
	}
	return out, nil
}

func (m *Manager) CancelTask(ctx context.Context, id int64) (tools.ScheduledTask, error) {
	if id <= 0 {
		return tools.ScheduledTask{}, errors.New("invalid task id")
	}

	task := tools.ScheduledTask{}
	err := m.pool.QueryRow(ctx, `
UPDATE scheduled_tasks
SET active = FALSE, updated_at = NOW()
WHERE id = $1
RETURNING id, kind, title, content, schedule_type, run_at, cron_expr, send_telegram, active, last_run_at, created_at;
`, id).Scan(
		&task.ID, &task.Kind, &task.Title, &task.Content, &task.ScheduleType, &task.RunAt, &task.CronExpr, &task.SendTelegram, &task.Active, &task.LastRunAt, &task.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return tools.ScheduledTask{}, fmt.Errorf("task not found: %d", id)
		}
		return tools.ScheduledTask{}, fmt.Errorf("cancel task failed: %w", err)
	}
	m.unscheduleTask(id)
	log.Infof("[tasks] task canceled id=%d", id)
	return task, nil
}

func (m *Manager) reload(ctx context.Context) error {
	rows, err := m.pool.Query(ctx, `
SELECT id, kind, title, content, schedule_type, run_at, cron_expr, send_telegram, active, last_run_at, created_at
FROM scheduled_tasks
WHERE active = TRUE;
`)
	if err != nil {
		return fmt.Errorf("load active tasks failed: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var t tools.ScheduledTask
		if err := rows.Scan(
			&t.ID, &t.Kind, &t.Title, &t.Content, &t.ScheduleType, &t.RunAt, &t.CronExpr, &t.SendTelegram, &t.Active, &t.LastRunAt, &t.CreatedAt,
		); err != nil {
			return fmt.Errorf("scan active task failed: %w", err)
		}
		m.scheduleTask(t)
	}
	return rows.Err()
}

func (m *Manager) scheduleTask(task tools.ScheduledTask) {
	if !task.Active {
		return
	}
	switch task.ScheduleType {
	case "cron":
		if strings.TrimSpace(task.CronExpr) == "" {
			return
		}
		id, err := m.cronEngine.AddFunc(task.CronExpr, func() {
			m.executeTask(task.ID)
		})
		if err != nil {
			log.Warnf("[tasks] add cron task failed id=%d err=%v", task.ID, err)
			return
		}
		m.mu.Lock()
		m.cronEntries[task.ID] = id
		m.mu.Unlock()
	case "once":
		if task.RunAt == nil {
			return
		}
		d := time.Until(task.RunAt.UTC())
		if d <= 0 {
			go m.executeTask(task.ID)
			return
		}
		timer := time.AfterFunc(d, func() {
			m.executeTask(task.ID)
		})
		m.mu.Lock()
		m.timers[task.ID] = timer
		m.mu.Unlock()
	}
}

func (m *Manager) unscheduleTask(taskID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entryID, ok := m.cronEntries[taskID]; ok {
		m.cronEngine.Remove(entryID)
		delete(m.cronEntries, taskID)
	}
	if timer, ok := m.timers[taskID]; ok {
		timer.Stop()
		delete(m.timers, taskID)
	}
}

func (m *Manager) executeTask(taskID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	task, err := m.getTask(ctx, taskID)
	if err != nil {
		log.Warnf("[tasks] load task for execution failed id=%d err=%v", taskID, err)
		return
	}
	if !task.Active {
		m.unscheduleTask(taskID)
		return
	}

	var result string
	switch task.Kind {
	case "alarm":
		result = strings.TrimSpace(task.Content)
	default:
		m.mu.Lock()
		a := m.agent
		m.mu.Unlock()
		if a == nil {
			result = "Task execution failed: agent is not ready."
		} else {
			resp, err := a.ProcessMessage(ctx, task.Content)
			if err != nil {
				result = "Task execution failed: " + err.Error()
			} else {
				result = resp
			}
		}
	}

	if task.SendTelegram {
		if m.notifier == nil || !m.notifier.Enabled() {
			log.Warnf("[tasks] telegram send skipped id=%d reason=not configured", taskID)
		} else {
			text := result
			if strings.TrimSpace(task.Title) != "" {
				text = fmt.Sprintf("%s\n\n%s", task.Title, result)
			}
			if err := m.notifier.Send(text); err != nil {
				log.Warnf("[tasks] telegram send failed id=%d err=%v", taskID, err)
			}
		}
	}

	now := time.Now().UTC()
	if task.ScheduleType == "once" {
		_, err = m.pool.Exec(ctx, `
UPDATE scheduled_tasks
SET active = FALSE, last_run_at = $2, updated_at = NOW()
WHERE id = $1;
`, taskID, now)
		m.unscheduleTask(taskID)
	} else {
		_, err = m.pool.Exec(ctx, `
UPDATE scheduled_tasks
SET last_run_at = $2, updated_at = NOW()
WHERE id = $1;
`, taskID, now)
	}
	if err != nil {
		log.Warnf("[tasks] update task run state failed id=%d err=%v", taskID, err)
	}
	log.Infof("[tasks] task executed id=%d kind=%s schedule=%s", taskID, task.Kind, task.ScheduleType)
}

func (m *Manager) getTask(ctx context.Context, id int64) (tools.ScheduledTask, error) {
	var t tools.ScheduledTask
	err := m.pool.QueryRow(ctx, `
SELECT id, kind, title, content, schedule_type, run_at, cron_expr, send_telegram, active, last_run_at, created_at
FROM scheduled_tasks
WHERE id = $1;
`, id).Scan(
		&t.ID, &t.Kind, &t.Title, &t.Content, &t.ScheduleType, &t.RunAt, &t.CronExpr, &t.SendTelegram, &t.Active, &t.LastRunAt, &t.CreatedAt,
	)
	if err != nil {
		return tools.ScheduledTask{}, err
	}
	return t, nil
}
