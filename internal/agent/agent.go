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
)

type Agent struct {
	Name           string
	Client         *openai.Client
	Model          string
	EmbeddingModel string
	Memory         *memcore.Memorya
	mu             sync.Mutex
}

func New(name string, client *openai.Client, model string, embeddingModel string, memory *memcore.Memorya) *Agent {
	return &Agent{Name: name, Client: client, Model: model, EmbeddingModel: embeddingModel, Memory: memory}
}

func (a *Agent) ProcessMessage(ctx context.Context, input string) (string, error) {
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
	if a.EmbeddingModel == "" {
		return "", fmt.Errorf("embedding model is not configured")
	}
	if a.Memory == nil {
		return "", fmt.Errorf("memory is not configured")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	normalizedInput := strings.TrimSpace(input)
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

	now := time.Now().UTC()
	a.Memory.AddMessage(memstorage.Message{
		Role:       openai.ChatMessageRoleUser,
		Content:    normalizedInput,
		Embeddings: &emb,
		CreatedAt:  &now,
	}, false)

	messages := make([]openai.ChatCompletionMessage, 0, len(a.Memory.GetMessages())+1)
	messages = append(messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: fmt.Sprintf("You are %s, a helpful AI agent.", strings.TrimSpace(a.Name)),
	})
	for _, msg := range a.Memory.GetMessages() {
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    normalizeRole(msg.Role),
			Content: msg.Content,
		})
	}

	response, err := a.Client.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model:    a.Model,
			Messages: messages,
		},
	)
	if err != nil {
		return "", fmt.Errorf("llm request failed: %w", err)
	}
	if len(response.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices")
	}

	answer := strings.TrimSpace(response.Choices[0].Message.Content)
	now = time.Now().UTC()
	a.Memory.AddMessage(memstorage.Message{
		Role:      openai.ChatMessageRoleAssistant,
		Content:   answer,
		CreatedAt: &now,
	}, false)

	return answer, nil
}

func (a *Agent) ResetMemory() error {
	if a.Memory == nil {
		return fmt.Errorf("memory is not configured")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.Memory.Reset()
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
