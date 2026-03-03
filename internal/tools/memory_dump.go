package tools

import (
	"encoding/json"
	"strings"

	memstorage "github.com/rehacktive/memorya/storage"
)

type MemoryMessagesProvider interface {
	GetMessages() []memstorage.Message
}

type MemoryDumpTool struct {
	memory MemoryMessagesProvider
}

type memoryDumpParams struct {
	Limit int `json:"limit,omitempty"`
}

type memoryDumpMessage struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Pinned    bool   `json:"pinned"`
	CreatedAt string `json:"created_at,omitempty"`
}

func NewMemoryDumpTool(memory MemoryMessagesProvider) Tool {
	return &MemoryDumpTool{memory: memory}
}

func (t *MemoryDumpTool) GetName() string {
	return "memory_dump"
}

func (t *MemoryDumpTool) GetDescription() string {
	return "Debug tool: returns the current in-memory conversation messages."
}

func (t *MemoryDumpTool) GetFunction() func(params string) string {
	return func(params string) string {
		if t.memory == nil {
			return errorJSON("memory is not configured")
		}

		limit := 0
		if strings.TrimSpace(params) != "" {
			var in memoryDumpParams
			if err := json.Unmarshal([]byte(params), &in); err != nil {
				return errorJSON("invalid params: " + err.Error())
			}
			limit = max(in.Limit, 0)
		}

		messages := t.memory.GetMessages()
		if limit > 0 && len(messages) > limit {
			messages = messages[len(messages)-limit:]
		}

		out := make([]memoryDumpMessage, 0, len(messages))
		for _, msg := range messages {
			item := memoryDumpMessage{
				Role:    msg.Role,
				Content: msg.Content,
				Pinned:  msg.Pinned,
			}
			if msg.CreatedAt != nil {
				item.CreatedAt = msg.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
			}
			out = append(out, item)
		}

		payload := map[string]any{
			"count":    len(out),
			"messages": out,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return errorJSON("serialize memory dump failed: " + err.Error())
		}
		return string(data)
	}
}

func (t *MemoryDumpTool) IsImmediate() bool {
	return true
}

func (t *MemoryDumpTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"limit": map[string]any{
				"type":        "integer",
				"description": "Optional max number of latest messages to return.",
				"minimum":     0,
			},
		},
		Required: []string{},
	}
}
