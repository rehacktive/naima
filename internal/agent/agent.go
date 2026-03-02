package agent

import (
	"context"
	"fmt"
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

	now := time.Now().UTC()
	a.Memory.AddMessage(memstorage.Message{
		Role:       openai.ChatMessageRoleUser,
		Content:    normalizedInput,
		Embeddings: &emb,
		CreatedAt:  &now,
	}, false)
	log.Infof("[agent] user message saved to memory")

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

	response, err := a.Client.CreateChatCompletion(
		ctx,
		req,
	)
	if err != nil {
		return "", fmt.Errorf("llm request failed: %w", err)
	}
	if len(response.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices")
	}

	msg := response.Choices[0].Message
	if len(msg.ToolCalls) > 0 {
		log.Infof("[agent] model requested tool calls count=%d", len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			tool, ok := a.Tools[call.Function.Name]
			if !ok {
				return "", fmt.Errorf("tool not found: %s", call.Function.Name)
			}

			log.Infof("[agent] executing tool name=%s immediate=%t", call.Function.Name, tool.IsImmediate())
			out := tool.GetFunction()(call.Function.Arguments)
			if tool.IsImmediate() {
				now = time.Now().UTC()
				a.Memory.AddMessage(memstorage.Message{
					Role:      openai.ChatMessageRoleAssistant,
					Content:   out,
					CreatedAt: &now,
				}, false)
				log.Infof("[agent] replied via immediate tool name=%s elapsed=%s", call.Function.Name, time.Since(startedAt).Round(time.Millisecond))
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

		secondResponse, err := a.Client.CreateChatCompletion(ctx, secondReq)
		if err != nil {
			return "", fmt.Errorf("llm request failed after tool execution: %w", err)
		}
		if len(secondResponse.Choices) == 0 {
			return "", fmt.Errorf("llm returned no choices after tool execution")
		}
		msg = secondResponse.Choices[0].Message
	}

	answer := strings.TrimSpace(msg.Content)
	now = time.Now().UTC()
	a.Memory.AddMessage(memstorage.Message{
		Role:      openai.ChatMessageRoleAssistant,
		Content:   answer,
		CreatedAt: &now,
	}, false)
	log.Infof("[agent] assistant message saved; replied chars=%d elapsed=%s", len(answer), time.Since(startedAt).Round(time.Millisecond))

	return answer, nil
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
