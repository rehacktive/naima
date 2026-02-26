package tools

import (
	"encoding/json"
	"time"
)

type TimeTool struct{}

func NewTimeTool() Tool {
	return &TimeTool{}
}

func (t *TimeTool) GetName() string {
	return "time"
}

func (t *TimeTool) GetDescription() string {
	return "Returns the current date and time."
}

func (t *TimeTool) GetFunction() func(params string) string {
	return func(_ string) string {
		now := time.Now()
		payload := map[string]string{
			"local": now.Format(time.RFC3339),
			"utc":   now.UTC().Format(time.RFC3339),
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return "{\"error\":\"failed to serialize time\"}"
		}
		return string(data)
	}
}

func (t *TimeTool) IsImmediate() bool {
	return false
}

func (t *TimeTool) GetParameters() Parameters {
	return Parameters{
		Type:       "object",
		Properties: map[string]any{},
		Required:   []string{},
	}
}
