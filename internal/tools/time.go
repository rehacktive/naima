package tools

import (
	"encoding/json"
	"os"
	"strings"
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
		loc := resolveTimeLocation()
		now := time.Now().In(loc)
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

func resolveTimeLocation() *time.Location {
	if loc := loadLocationFromEnv("NAIMA_TASK_TIMEZONE"); loc != nil {
		return loc
	}
	if loc := loadLocationFromEnv("TZ"); loc != nil {
		return loc
	}
	return time.Local
}

func loadLocationFromEnv(key string) *time.Location {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	loc, err := time.LoadLocation(raw)
	if err != nil {
		return nil
	}
	return loc
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
