package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	memcore "github.com/rehacktive/memorya/memorya"
	memstorage "github.com/rehacktive/memorya/storage"
	openai "github.com/sashabaranov/go-openai"
	log "github.com/sirupsen/logrus"

	"naima/internal/tools"
)

const maxToolRounds = 8
const maxToolLogChars = 220

type Agent struct {
	Name           string
	SystemPrompt   string
	Client         *openai.Client
	Model          string
	EmbeddingModel string
	Memory         *memcore.Memorya
	Tools          map[string]tools.Tool
	ToolEnabled    map[string]bool
	mu             sync.Mutex
}

type ToolState struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Immediate   bool   `json:"immediate"`
	Enabled     bool   `json:"enabled"`
}

func New(name string, systemPrompt string, client *openai.Client, model string, embeddingModel string, memory *memcore.Memorya, toolset []tools.Tool) *Agent {
	toolMap := make(map[string]tools.Tool, len(toolset))
	for _, tool := range toolset {
		if tool == nil {
			continue
		}
		toolMap[tool.GetName()] = tool
	}

	return &Agent{
		Name:           name,
		SystemPrompt:   strings.TrimSpace(systemPrompt),
		Client:         client,
		Model:          model,
		EmbeddingModel: embeddingModel,
		Memory:         memory,
		Tools:          toolMap,
		ToolEnabled:    enabledMap(toolMap),
	}
}

func (a *Agent) ProcessMessage(ctx context.Context, input string) (string, error) {
	return a.processMessage(ctx, input, nil, nil)
}

func (a *Agent) ProcessMessageStream(ctx context.Context, input string, onDelta func(string)) (string, error) {
	return a.processMessage(ctx, input, onDelta, nil)
}

func (a *Agent) ProcessMessageStreamWithOps(ctx context.Context, input string, onDelta func(string), onOp func(string)) (string, error) {
	return a.processMessage(ctx, input, onDelta, onOp)
}

func (a *Agent) processMessage(ctx context.Context, input string, onDelta func(string), onOp func(string)) (string, error) {
	startedAt := time.Now()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}

	if a.Client == nil {
		return "", fmt.Errorf("llm client is not configured")
	}
	if a.Model == "" {
		return "", fmt.Errorf("llm model is not configured")
	}
	if a.SystemPrompt == "" {
		return "", fmt.Errorf("system prompt is not configured")
	}
	if a.EmbeddingModel == "" {
		return "", fmt.Errorf("embedding model is not configured")
	}
	if a.Memory == nil {
		return "", fmt.Errorf("memory is not configured")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	normalizedInput := strings.TrimSpace(input)
	log.Infof("[agent] message received chars=%d", len(normalizedInput))
	emitOp(onOp, "message received")
	embResp, err := a.Client.CreateEmbeddings(ctx, openai.EmbeddingRequest{
		Input: []string{normalizedInput},
		Model: openai.EmbeddingModel(a.EmbeddingModel),
	})
	if err != nil {
		return "", fmt.Errorf("embedding request failed: %w", err)
	}
	if len(embResp.Data) == 0 {
		return "", fmt.Errorf("embedding response returned no vectors")
	}
	emb := append([]float32(nil), embResp.Data[0].Embedding...)
	log.Infof("[agent] embeddings generated dim=%d", len(emb))
	emitOp(onOp, fmt.Sprintf("embeddings generated (dim=%d)", len(emb)))

	now := time.Now().UTC()
	a.Memory.AddMessage(memstorage.Message{
		Role:       openai.ChatMessageRoleUser,
		Content:    normalizedInput,
		Embeddings: &emb,
		CreatedAt:  &now,
	}, false)
	log.Infof("[agent] user message saved to memory")
	emitOp(onOp, "user message saved")

	messages := make([]openai.ChatCompletionMessage, 0, len(a.Memory.GetMessages())+1)
	messages = append(messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: a.SystemPrompt,
	})
	for _, msg := range a.Memory.GetMessages() {
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    normalizeRole(msg.Role),
			Content: msg.Content,
		})
	}

	activeTools := a.enabledTools()
	req := openai.ChatCompletionRequest{
		Model:    a.Model,
		Messages: messages,
	}
	if len(activeTools) > 0 {
		req.Tools = toOpenAITools(activeTools)
	}
	log.Infof("[agent] sending model request context_messages=%d tools=%d", len(messages), len(req.Tools))
	emitOp(onOp, "model request started")

	msg, err := a.runModel(ctx, req, onDelta)
	if err != nil {
		return "", fmt.Errorf("llm request failed: %w", err)
	}

	for toolRound := 1; len(msg.ToolCalls) > 0; toolRound++ {
		if toolRound > maxToolRounds {
			return "", fmt.Errorf("tool execution exceeded max rounds (%d)", maxToolRounds)
		}

		log.Infof("[agent] model requested tool calls round=%d count=%d", toolRound, len(msg.ToolCalls))
		emitOp(onOp, fmt.Sprintf("tool calls requested (round=%d, count=%d)", toolRound, len(msg.ToolCalls)))

		for _, call := range msg.ToolCalls {
			tool, ok := activeTools[call.Function.Name]
			if !ok {
				return "", fmt.Errorf("tool not found: %s", call.Function.Name)
			}

			log.Infof("[agent] executing tool name=%s immediate=%t", call.Function.Name, tool.IsImmediate())
			emitOp(onOp, fmt.Sprintf("executing tool: %s", call.Function.Name))
			out := tool.GetFunction()(call.Function.Arguments)
			log.Infof("[agent] tool output name=%s chars=%d preview=%s", call.Function.Name, len(out), truncateForLog(out, maxToolLogChars))
			emitOp(onOp, fmt.Sprintf("tool output: %s (%d chars)", call.Function.Name, len(out)))
			if toolErr, ok := extractToolError(out); ok {
				log.Warnf("[agent] tool returned error name=%s error=%s", call.Function.Name, toolErr)
				emitOp(onOp, fmt.Sprintf("tool error: %s", toolErr))
			}
			if tool.IsImmediate() {
				now = time.Now().UTC()
				a.Memory.AddMessage(memstorage.Message{
					Role:      openai.ChatMessageRoleAssistant,
					Content:   out,
					CreatedAt: &now,
				}, false)
				log.Infof("[agent] replied via immediate tool name=%s elapsed=%s", call.Function.Name, time.Since(startedAt).Round(time.Millisecond))
				emitOp(onOp, "reply sent (immediate tool)")
				return out, nil
			}

			messages = append(messages, openai.ChatCompletionMessage{
				Role:      openai.ChatMessageRoleAssistant,
				ToolCalls: []openai.ToolCall{call},
			})
			messages = append(messages, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Name:       call.Function.Name,
				ToolCallID: call.ID,
				Content:    out,
			})
		}

		followReq := openai.ChatCompletionRequest{
			Model:    a.Model,
			Messages: messages,
		}
		if len(activeTools) > 0 {
			followReq.Tools = toOpenAITools(activeTools)
		}
		log.Infof("[agent] sending follow-up model request after tool execution round=%d", toolRound)
		emitOp(onOp, fmt.Sprintf("model follow-up after tools (round=%d)", toolRound))

		msg, err = a.runModel(ctx, followReq, onDelta)
		if err != nil {
			return "", fmt.Errorf("llm request failed after tool execution: %w", err)
		}
	}

	answer := strings.TrimSpace(msg.Content)
	if answer == "" {
		return "", fmt.Errorf("llm returned an empty response")
	}
	now = time.Now().UTC()
	a.Memory.AddMessage(memstorage.Message{
		Role:      openai.ChatMessageRoleAssistant,
		Content:   answer,
		CreatedAt: &now,
	}, false)
	log.Infof("[agent] assistant message saved; replied chars=%d elapsed=%s", len(answer), time.Since(startedAt).Round(time.Millisecond))
	emitOp(onOp, "assistant replied")

	return answer, nil
}

func (a *Agent) runModel(ctx context.Context, req openai.ChatCompletionRequest, onDelta func(string)) (openai.ChatCompletionMessage, error) {
	if onDelta == nil {
		response, err := a.Client.CreateChatCompletion(ctx, req)
		if err != nil {
			return openai.ChatCompletionMessage{}, err
		}
		if len(response.Choices) == 0 {
			return openai.ChatCompletionMessage{}, fmt.Errorf("llm returned no choices")
		}
		return response.Choices[0].Message, nil
	}

	stream, err := a.Client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return openai.ChatCompletionMessage{}, err
	}
	defer stream.Close()

	msg := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant}
	toolCallsByIndex := make(map[int]*openai.ToolCall)
	order := make([]int, 0)

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return openai.ChatCompletionMessage{}, err
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		for _, choice := range chunk.Choices {
			delta := choice.Delta
			if delta.Role != "" {
				msg.Role = delta.Role
			}
			if delta.Content != "" {
				msg.Content += delta.Content
				onDelta(delta.Content)
			}
			for _, deltaCall := range delta.ToolCalls {
				idx := 0
				if deltaCall.Index != nil {
					idx = *deltaCall.Index
				}

				existing, ok := toolCallsByIndex[idx]
				if !ok {
					tc := openai.ToolCall{
						Type:     deltaCall.Type,
						ID:       deltaCall.ID,
						Function: openai.FunctionCall{},
					}
					toolCallsByIndex[idx] = &tc
					existing = &tc
					order = append(order, idx)
				}

				if deltaCall.ID != "" {
					existing.ID = deltaCall.ID
				}
				if deltaCall.Type != "" {
					existing.Type = deltaCall.Type
				}
				if deltaCall.Function.Name != "" {
					existing.Function.Name += deltaCall.Function.Name
				}
				if deltaCall.Function.Arguments != "" {
					existing.Function.Arguments += deltaCall.Function.Arguments
				}
			}
		}
	}

	if len(order) > 0 {
		msg.ToolCalls = make([]openai.ToolCall, 0, len(order))
		for _, idx := range order {
			if tc := toolCallsByIndex[idx]; tc != nil {
				msg.ToolCalls = append(msg.ToolCalls, *tc)
			}
		}
	}

	return msg, nil
}

func (a *Agent) ResetMemory() error {
	if a.Memory == nil {
		return fmt.Errorf("memory is not configured")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.Memory.Reset()
	log.Infof("[agent] memory reset")
	return nil
}

func (a *Agent) ListTools() []ToolState {
	a.mu.Lock()
	defer a.mu.Unlock()

	names := make([]string, 0, len(a.Tools))
	for name := range a.Tools {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ToolState, 0, len(names))
	for _, name := range names {
		tool := a.Tools[name]
		out = append(out, ToolState{
			Name:        name,
			Description: tool.GetDescription(),
			Immediate:   tool.IsImmediate(),
			Enabled:     a.ToolEnabled[name],
		})
	}
	return out
}

func (a *Agent) SetToolEnabled(name string, enabled bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("tool name is required")
	}
	if _, ok := a.Tools[name]; !ok {
		return fmt.Errorf("tool not found: %s", name)
	}
	a.ToolEnabled[name] = enabled
	log.Infof("[agent] tool state changed name=%s enabled=%t", name, enabled)
	return nil
}

func normalizeRole(role string) string {
	switch role {
	case openai.ChatMessageRoleSystem, openai.ChatMessageRoleUser, openai.ChatMessageRoleAssistant:
		return role
	default:
		return openai.ChatMessageRoleUser
	}
}

func toOpenAITools(toolset map[string]tools.Tool) []openai.Tool {
	ret := make([]openai.Tool, 0, len(toolset))
	for _, tool := range toolset {
		params := tool.GetParameters()
		if params.Type == "" {
			params.Type = "object"
		}

		schema := map[string]any{
			"type":       params.Type,
			"properties": params.Properties,
			"required":   params.Required,
		}

		ret = append(ret, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        tool.GetName(),
				Description: tool.GetDescription(),
				Parameters:  schema,
			},
		})
	}

	return ret
}

func emitOp(onOp func(string), message string) {
	if onOp != nil {
		onOp(message)
	}
}

func truncateForLog(v string, max int) string {
	s := strings.TrimSpace(v)
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

func extractToolError(out string) (string, bool) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return "", false
	}
	raw, ok := payload["error"]
	if !ok {
		return "", false
	}
	msg, ok := raw.(string)
	if !ok {
		return "", false
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "", false
	}
	return msg, true
}

func enabledMap(toolMap map[string]tools.Tool) map[string]bool {
	out := make(map[string]bool, len(toolMap))
	for name := range toolMap {
		out[name] = true
	}
	return out
}

func (a *Agent) enabledTools() map[string]tools.Tool {
	out := make(map[string]tools.Tool, len(a.Tools))
	for name, tool := range a.Tools {
		if a.ToolEnabled[name] {
			out[name] = tool
		}
	}
	return out
}
