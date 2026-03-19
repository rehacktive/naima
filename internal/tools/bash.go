package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	defaultBashToolTimeout    = 125 * time.Second
	defaultBashCommandTimeout = 120000
	maxBashCommandTimeout     = 600000
)

type BashTool struct {
	baseURL string
	client  *http.Client
}

type bashParams struct {
	Command    string `json:"command"`
	WorkingDir string `json:"working_dir,omitempty"`
	TimeoutMS  int    `json:"timeout_ms,omitempty"`
}

func NewBashTool(baseURL string) Tool {
	return &BashTool{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		client:  &http.Client{Timeout: defaultBashToolTimeout},
	}
}

func (t *BashTool) GetName() string {
	return "bash"
}

func (t *BashTool) GetDescription() string {
	return "Executes bash commands inside an isolated Debian container with persistent workspace state."
}

func (t *BashTool) GetFunction() func(params string) string {
	return func(params string) string {
		var in bashParams
		if err := jsonUnmarshal(params, &in); err != nil {
			return errorJSON("invalid params: " + err.Error())
		}

		command := strings.TrimSpace(in.Command)
		if command == "" {
			return errorJSON("command is required")
		}
		if t.baseURL == "" {
			return errorJSON("bash tool base url is not configured")
		}

		timeoutMS := in.TimeoutMS
		if timeoutMS <= 0 {
			timeoutMS = defaultBashCommandTimeout
		}
		if timeoutMS > maxBashCommandTimeout {
			timeoutMS = maxBashCommandTimeout
		}

		payload := map[string]any{
			"command":     command,
			"working_dir": strings.TrimSpace(in.WorkingDir),
			"timeout_ms":  timeoutMS,
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return errorJSON("serialize bash request failed: " + err.Error())
		}

		req, err := http.NewRequest(http.MethodPost, t.baseURL+"/exec", bytes.NewReader(body))
		if err != nil {
			return errorJSON("build bash request failed: " + err.Error())
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := t.client.Do(req)
		if err != nil {
			return errorJSON("bash request failed: " + err.Error())
		}
		defer resp.Body.Close()

		var raw json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return errorJSON("decode bash response failed: " + err.Error())
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return errorJSON(fmt.Sprintf("bash service returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))))
		}
		return string(raw)
	}
}

func (t *BashTool) IsImmediate() bool {
	return false
}

func (t *BashTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Bash command to execute inside the Debian tool container.",
			},
			"working_dir": map[string]any{
				"type":        "string",
				"description": "Optional working directory inside the container. Default is /workspace.",
			},
			"timeout_ms": map[string]any{
				"type":        "integer",
				"description": "Optional command timeout in milliseconds (max 600000).",
				"minimum":     1,
				"maximum":     600000,
			},
		},
		Required: []string{"command"},
	}
}
