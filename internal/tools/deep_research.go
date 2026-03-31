package tools

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"naima/internal/research"
)

const defaultDeepResearchToolTimeout = 8 * time.Second

type DeepResearchManager interface {
	CreateRun(ctx context.Context, req research.CreateRunRequest) (research.Run, error)
	GetRun(ctx context.Context, id int64) (research.Run, error)
	ListRuns(ctx context.Context, limit int) ([]research.Run, error)
	CancelRun(ctx context.Context, id int64) (research.Run, error)
	DeleteRun(ctx context.Context, id int64) error
}

type DeepResearchTool struct {
	manager DeepResearchManager
}

type deepResearchParams struct {
	Operation   string `json:"operation"`
	ID          int64  `json:"id,omitempty"`
	Topic       string `json:"topic,omitempty"`
	Note        string `json:"note,omitempty"`
	GuideTitle  string `json:"guide_title,omitempty"`
	Language    string `json:"language,omitempty"`
	TimeRange   string `json:"time_range,omitempty"`
	MaxSources  int    `json:"max_sources,omitempty"`
	MaxQueries  int    `json:"max_queries,omitempty"`
	NotifyTg    *bool  `json:"notify_telegram,omitempty"`
	IncludeLogs bool   `json:"include_logs,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

func NewDeepResearchTool(manager DeepResearchManager) Tool {
	return &DeepResearchTool{manager: manager}
}

func (t *DeepResearchTool) GetName() string {
	return "deep_research"
}

func (t *DeepResearchTool) GetDescription() string {
	return "Creates persisted background research runs, lists their status, or fetches detailed results and logs."
}

func (t *DeepResearchTool) GetFunction() func(params string) string {
	return func(params string) string {
		if t.manager == nil {
			return errorJSON("deep research manager is not configured")
		}
		var in deepResearchParams
		if err := jsonUnmarshal(params, &in); err != nil {
			return errorJSON("invalid params: " + err.Error())
		}
		op := strings.ToLower(strings.TrimSpace(in.Operation))
		if op == "" {
			op = "create"
		}
		ctx, cancel := context.WithTimeout(context.Background(), defaultDeepResearchToolTimeout)
		defer cancel()

		switch op {
		case "create", "start":
			return t.create(ctx, in)
		case "get", "status":
			return t.get(ctx, in)
		case "list":
			return t.list(ctx, in)
		case "cancel", "stop":
			return t.cancel(ctx, in)
		case "delete":
			return t.delete(ctx, in)
		default:
			return errorJSON("unsupported operation: " + op)
		}
	}
}

func (t *DeepResearchTool) IsImmediate() bool {
	return false
}

func (t *DeepResearchTool) IsImmediateForParams(params string) bool {
	var in deepResearchParams
	if err := jsonUnmarshal(params, &in); err != nil {
		return false
	}
	op := strings.ToLower(strings.TrimSpace(in.Operation))
	if op == "" {
		op = "create"
	}
	return op == "create" || op == "start" || op == "cancel" || op == "stop" || op == "delete"
}

func (t *DeepResearchTool) MaxToolRounds() int {
	return 3 * 10
}

func (t *DeepResearchTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"operation": map[string]any{
				"type":        "string",
				"description": "Operation: create/start, get/status, list, cancel/stop, or delete.",
				"enum":        []string{"create", "start", "get", "status", "list", "cancel", "stop", "delete"},
			},
			"id": map[string]any{
				"type":        "integer",
				"description": "Research run id for get/status.",
				"minimum":     1,
			},
			"topic": map[string]any{
				"type":        "string",
				"description": "Topic title for a new research run.",
			},
			"note": map[string]any{
				"type":        "string",
				"description": "Research brief and guide for a new research run.",
			},
			"guide_title": map[string]any{
				"type":        "string",
				"description": "Optional title for the stored research brief document.",
			},
			"language": map[string]any{
				"type":        "string",
				"description": "Optional language hint for search planning.",
			},
			"time_range": map[string]any{
				"type":        "string",
				"description": "Optional recency hint.",
				"enum":        []string{"day", "week", "month", "year"},
			},
			"max_sources": map[string]any{
				"type":        "integer",
				"description": "Maximum number of sources to ingest.",
				"minimum":     1,
				"maximum":     10,
			},
			"max_queries": map[string]any{
				"type":        "integer",
				"description": "Maximum number of search queries to execute.",
				"minimum":     1,
				"maximum":     8,
			},
			"notify_telegram": map[string]any{
				"type":        "boolean",
				"description": "Send Telegram notification on completion if Telegram is linked.",
			},
			"include_logs": map[string]any{
				"type":        "boolean",
				"description": "For get/status, include persisted job logs.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "For list, number of runs to return.",
				"minimum":     1,
				"maximum":     50,
			},
		},
		Required: []string{"operation"},
	}
}

func (t *DeepResearchTool) create(ctx context.Context, in deepResearchParams) string {
	if strings.TrimSpace(in.Topic) == "" {
		return errorJSON("topic is required")
	}
	if strings.TrimSpace(in.Note) == "" {
		return errorJSON("note is required")
	}
	notifyTelegram := true
	if in.NotifyTg != nil {
		notifyTelegram = *in.NotifyTg
	}
	run, err := t.manager.CreateRun(ctx, research.CreateRunRequest{
		Topic:          strings.TrimSpace(in.Topic),
		Note:           strings.TrimSpace(in.Note),
		GuideTitle:     strings.TrimSpace(in.GuideTitle),
		Language:       strings.TrimSpace(in.Language),
		TimeRange:      strings.TrimSpace(in.TimeRange),
		MaxSources:     in.MaxSources,
		MaxQueries:     in.MaxQueries,
		NotifyTelegram: notifyTelegram,
	})
	if err != nil {
		return errorJSON("create research run failed: " + err.Error())
	}
	return mustJSON(map[string]any{
		"operation":    "create",
		"run":          run,
		"message":      "research queued in background; use deep_research get/list to check status later",
		"user_message": "Deep research started in background.\n\nRun ID: " + strconv.FormatInt(run.ID, 10) + "\nTopic: " + run.TopicTitle + "\n\nTo check status later:\n- ask me to show deep research run " + strconv.FormatInt(run.ID, 10) + "\n- or use the deep_research tool with operation=get and id=" + strconv.FormatInt(run.ID, 10) + "\n- or open /api/research/" + strconv.FormatInt(run.ID, 10),
	})
}

func (t *DeepResearchTool) get(ctx context.Context, in deepResearchParams) string {
	if in.ID <= 0 {
		return errorJSON("id is required")
	}
	run, err := t.manager.GetRun(ctx, in.ID)
	if err != nil {
		return errorJSON("get research run failed: " + err.Error())
	}
	if !in.IncludeLogs {
		run.Logs = ""
	}
	return mustJSON(map[string]any{
		"operation": "get",
		"run":       run,
	})
}

func (t *DeepResearchTool) list(ctx context.Context, in deepResearchParams) string {
	runs, err := t.manager.ListRuns(ctx, in.Limit)
	if err != nil {
		return errorJSON("list research runs failed: " + err.Error())
	}
	for i := range runs {
		runs[i].Logs = ""
	}
	out, err := json.Marshal(map[string]any{
		"operation": "list",
		"count":     len(runs),
		"runs":      runs,
	})
	if err != nil {
		return errorJSON("serialize research runs failed: " + err.Error())
	}
	return string(out)
}

func (t *DeepResearchTool) cancel(ctx context.Context, in deepResearchParams) string {
	if in.ID <= 0 {
		return errorJSON("id is required")
	}
	run, err := t.manager.CancelRun(ctx, in.ID)
	if err != nil {
		return errorJSON("cancel research run failed: " + err.Error())
	}
	return mustJSON(map[string]any{
		"operation":    "cancel",
		"run":          run,
		"user_message": "Deep research run " + strconv.FormatInt(run.ID, 10) + " canceled.",
	})
}

func (t *DeepResearchTool) delete(ctx context.Context, in deepResearchParams) string {
	if in.ID <= 0 {
		return errorJSON("id is required")
	}
	if err := t.manager.DeleteRun(ctx, in.ID); err != nil {
		return errorJSON("delete research run failed: " + err.Error())
	}
	return mustJSON(map[string]any{
		"operation":    "delete",
		"id":           in.ID,
		"user_message": "Deep research run " + strconv.FormatInt(in.ID, 10) + " deleted.",
	})
}
