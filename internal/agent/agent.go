package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	memcore "github.com/rehacktive/memorya/memorya"
	memstorage "github.com/rehacktive/memorya/storage"
	openai "github.com/sashabaranov/go-openai"
	log "github.com/sirupsen/logrus"

	"naima/internal/tools"
)

type Agent struct {
	Name           string
	SystemPrompt   string
	Client         *openai.Client
	Model          string
	EmbeddingModel string
	Memory         *memcore.Memorya
	Tools          map[string]tools.Tool
	mu             sync.Mutex
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

	req := openai.ChatCompletionRequest{
		Model:    a.Model,
		Messages: messages,
	}
	if len(a.Tools) > 0 {
		req.Tools = toOpenAITools(a.Tools)
	}
	log.Infof("[agent] sending model request context_messages=%d tools=%d", len(messages), len(req.Tools))
	emitOp(onOp, "model request started")

	msg, err := a.runModel(ctx, req, onDelta)
	if err != nil {
		return "", fmt.Errorf("llm request failed: %w", err)
	}
	if len(msg.ToolCalls) > 0 {
		log.Infof("[agent] model requested tool calls count=%d", len(msg.ToolCalls))
		emitOp(onOp, fmt.Sprintf("tool calls requested (%d)", len(msg.ToolCalls)))
		for _, call := range msg.ToolCalls {
			tool, ok := a.Tools[call.Function.Name]
			if !ok {
				return "", fmt.Errorf("tool not found: %s", call.Function.Name)
			}

			log.Infof("[agent] executing tool name=%s immediate=%t", call.Function.Name, tool.IsImmediate())
			emitOp(onOp, fmt.Sprintf("executing tool: %s", call.Function.Name))
			out := tool.GetFunction()(call.Function.Arguments)
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

		secondReq := openai.ChatCompletionRequest{
			Model:    a.Model,
			Messages: messages,
		}
		if len(a.Tools) > 0 {
			secondReq.Tools = toOpenAITools(a.Tools)
		}
		log.Infof("[agent] sending follow-up model request after tool execution")
		emitOp(onOp, "model follow-up after tools")

		msg, err = a.runModel(ctx, secondReq, onDelta)
		if err != nil {
			return "", fmt.Errorf("llm request failed after tool execution: %w", err)
		}
	}

	answer := strings.TrimSpace(msg.Content)
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
