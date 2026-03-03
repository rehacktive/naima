package tools

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

const defaultTaskToolTimeout = 8 * time.Second

type ScheduledTask struct {
	ID           int64      `json:"id"`
	Kind         string     `json:"kind"`
	Title        string     `json:"title"`
	Content      string     `json:"content"`
	ScheduleType string     `json:"schedule_type"`
	RunAt        *time.Time `json:"run_at,omitempty"`
	CronExpr     string     `json:"cron,omitempty"`
	SendTelegram bool       `json:"send_telegram"`
	Active       bool       `json:"active"`
	LastRunAt    *time.Time `json:"last_run_at,omitempty"`
	CreatedAt    *time.Time `json:"created_at,omitempty"`
}

type CreateScheduledTaskRequest struct {
	Kind         string
	Title        string
	Content      string
	RunAt        *time.Time
	CronExpr     string
	SendTelegram bool
}

type TaskScheduler interface {
	CreateTask(ctx context.Context, req CreateScheduledTaskRequest) (ScheduledTask, error)
	ListTasks(ctx context.Context) ([]ScheduledTask, error)
	CancelTask(ctx context.Context, id int64) (ScheduledTask, error)
}

type TaskSchedulerTool struct {
	scheduler TaskScheduler
}

type taskSchedulerParams struct {
	Operation    string `json:"operation"`
	ID           int64  `json:"id,omitempty"`
	TaskID       int64  `json:"task_id,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Title        string `json:"title,omitempty"`
	Content      string `json:"content,omitempty"`
	Prompt       string `json:"prompt,omitempty"`
	Message      string `json:"message,omitempty"`
	Text         string `json:"text,omitempty"`
	RunAt        string `json:"run_at,omitempty"`
	In           string `json:"in,omitempty"`
	Cron         string `json:"cron,omitempty"`
	SendTelegram *bool  `json:"send_telegram,omitempty"`
}

func NewTaskSchedulerTool(scheduler TaskScheduler) Tool {
	return &TaskSchedulerTool{scheduler: scheduler}
}

func (t *TaskSchedulerTool) GetName() string {
	return "task_scheduler"
}

func (t *TaskSchedulerTool) GetDescription() string {
	return "Schedules tasks for future execution (one-time or recurring cron), lists tasks, or cancels a task. Use for alarms and timed notifications."
}

func (t *TaskSchedulerTool) GetFunction() func(params string) string {
	return func(params string) string {
		if t.scheduler == nil {
			return errorJSON("task scheduler is not configured")
		}

		var in taskSchedulerParams
		if err := json.Unmarshal([]byte(params), &in); err != nil {
			return errorJSON("invalid params: " + err.Error())
		}

		op := strings.ToLower(strings.TrimSpace(in.Operation))
		if op == "" {
			op = "create"
		}

		ctx, cancel := context.WithTimeout(context.Background(), defaultTaskToolTimeout)
		defer cancel()

		switch op {
		case "create":
			return t.createTask(ctx, in)
		case "list":
			return t.listTasks(ctx)
		case "cancel", "delete", "disable":
			return t.cancelTask(ctx, in)
		default:
			return errorJSON("unsupported operation: " + op)
		}
	}
}

func (t *TaskSchedulerTool) IsImmediate() bool {
	return false
}

func (t *TaskSchedulerTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"operation": map[string]any{
				"type":        "string",
				"description": "Operation to execute.",
				"enum":        []string{"create", "list", "cancel"},
			},
			"id": map[string]any{
				"type":        "integer",
				"description": "Task ID to cancel.",
			},
			"kind": map[string]any{
				"type":        "string",
				"description": "Task kind: alarm (fixed message) or agent (run prompt through model).",
				"enum":        []string{"alarm", "agent"},
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Short task title.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Task content: alarm message or agent prompt.",
			},
			"run_at": map[string]any{
				"type":        "string",
				"description": "One-time execution timestamp in RFC3339.",
			},
			"in": map[string]any{
				"type":        "string",
				"description": "Relative duration for one-time task (e.g. 5m, 2h, 24h).",
			},
			"cron": map[string]any{
				"type":        "string",
				"description": "Recurring cron expression (5 fields: minute hour day month weekday).",
			},
			"send_telegram": map[string]any{
				"type":        "boolean",
				"description": "Send execution result to Telegram. Default true.",
			},
		},
		Required: []string{"operation"},
	}
}

func (t *TaskSchedulerTool) createTask(ctx context.Context, in taskSchedulerParams) string {
	content := firstNonEmpty(in.Content, in.Prompt, in.Message, in.Text)
	content = strings.TrimSpace(content)
	if content == "" {
		return errorJSON("content (or prompt/message/text) is required")
	}

	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if kind == "" {
		kind = "agent"
	}
	if kind != "agent" && kind != "alarm" {
		return errorJSON("kind must be alarm or agent")
	}

	sendTelegram := true
	if in.SendTelegram != nil {
		sendTelegram = *in.SendTelegram
	}

	var (
		runAt    *time.Time
		cronExpr string
	)
	if c := strings.TrimSpace(in.Cron); c != "" {
		cronExpr = c
	} else {
		if d := strings.TrimSpace(in.In); d != "" {
			parsed, err := time.ParseDuration(d)
			if err != nil || parsed <= 0 {
				return errorJSON("invalid 'in' duration")
			}
			ts := time.Now().Add(parsed).UTC()
			runAt = &ts
		} else if raw := strings.TrimSpace(in.RunAt); raw != "" {
			ts, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return errorJSON("invalid run_at (expected RFC3339)")
			}
			ut := ts.UTC()
			runAt = &ut
		} else {
			return errorJSON("one schedule is required: cron or run_at or in")
		}
	}

	task, err := t.scheduler.CreateTask(ctx, CreateScheduledTaskRequest{
		Kind:         kind,
		Title:        strings.TrimSpace(in.Title),
		Content:      content,
		RunAt:        runAt,
		CronExpr:     cronExpr,
		SendTelegram: sendTelegram,
	})
	if err != nil {
		return errorJSON("create task failed: " + err.Error())
	}

	out, err := json.Marshal(map[string]any{
		"operation": "create",
		"task":      task,
	})
	if err != nil {
		return errorJSON("serialize task failed: " + err.Error())
	}
	return string(out)
}

func (t *TaskSchedulerTool) listTasks(ctx context.Context) string {
	tasks, err := t.scheduler.ListTasks(ctx)
	if err != nil {
		return errorJSON("list tasks failed: " + err.Error())
	}
	out, err := json.Marshal(map[string]any{
		"operation": "list",
		"count":     len(tasks),
		"tasks":     tasks,
	})
	if err != nil {
		return errorJSON("serialize tasks failed: " + err.Error())
	}
	return string(out)
}

func (t *TaskSchedulerTool) cancelTask(ctx context.Context, in taskSchedulerParams) string {
	id := in.ID
	if id == 0 {
		id = in.TaskID
	}
	if id == 0 {
		return errorJSON("id is required for cancel")
	}

	task, err := t.scheduler.CancelTask(ctx, id)
	if err != nil {
		return errorJSON("cancel task failed: " + err.Error())
	}
	out, err := json.Marshal(map[string]any{
		"operation": "cancel",
		"task":      task,
	})
	if err != nil {
		return errorJSON("serialize task failed: " + err.Error())
	}
	return string(out)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
