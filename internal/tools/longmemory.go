package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memstorage "github.com/rehacktive/memorya/storage"
	openai "github.com/sashabaranov/go-openai"
)

const defaultLongMemoryMatches = 8

type LongMemoryTool struct {
	client         *openai.Client
	chatModel      string
	embeddingModel string
	storage        memstorage.Storage
}

type longMemoryParams struct {
	Something string `json:"something"`
}

func NewLongMemoryTool(client *openai.Client, chatModel string, embeddingModel string, storage memstorage.Storage) Tool {
	return &LongMemoryTool{
		client:         client,
		chatModel:      strings.TrimSpace(chatModel),
		embeddingModel: strings.TrimSpace(embeddingModel),
		storage:        storage,
	}
}

func (t *LongMemoryTool) GetName() string {
	return "long_memory"
}

func (t *LongMemoryTool) GetDescription() string {
	return "Searches saved conversation memory by topic and returns a summary of related previous messages."
}

func (t *LongMemoryTool) GetFunction() func(params string) string {
	return func(params string) string {
		var in longMemoryParams
		if err := json.Unmarshal([]byte(params), &in); err != nil {
			return errorJSON("invalid params: " + err.Error())
		}

		topic := strings.TrimSpace(in.Something)
		if topic == "" {
			return errorJSON("something is required")
		}
		if t.client == nil || t.chatModel == "" || t.embeddingModel == "" || t.storage == nil {
			return errorJSON("long memory is not configured")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()

		embResp, err := t.client.CreateEmbeddings(ctx, openai.EmbeddingRequest{
			Input: []string{topic},
			Model: openai.EmbeddingModel(t.embeddingModel),
		})
		if err != nil {
			return errorJSON("embedding request failed: " + err.Error())
		}
		if len(embResp.Data) == 0 {
			return errorJSON("embedding response returned no vectors")
		}

		queryEmb := append([]float32(nil), embResp.Data[0].Embedding...)
		matches, err := t.storage.SearchRelatedMessages(queryEmb)
		if err != nil {
			return errorJSON("memory search failed: " + err.Error())
		}

		summary, err := t.summarizeWithLLM(ctx, topic, matches, defaultLongMemoryMatches)
		if err != nil {
			summary = summarizeMemoryFallback(matches, defaultLongMemoryMatches)
		}
		payload := map[string]any{
			"something": topic,
			"matches":   min(len(matches), defaultLongMemoryMatches),
			"summary":   summary,
		}
		out, err := json.Marshal(payload)
		if err != nil {
			return errorJSON("serialize long memory result failed: " + err.Error())
		}

		return string(out)
	}
}

func (t *LongMemoryTool) IsImmediate() bool {
	return false
}

func (t *LongMemoryTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"something": map[string]any{
				"type":        "string",
				"description": "Topic to remember from previous discussions.",
			},
		},
		Required: []string{"something"},
	}
}

func (t *LongMemoryTool) summarizeWithLLM(ctx context.Context, topic string, messages []memstorage.Message, maxItems int) (string, error) {
	if len(messages) == 0 {
		return "No relevant previous discussion found.", nil
	}
	if maxItems <= 0 {
		maxItems = 8
	}

	lines := make([]string, 0, maxItems)
	for _, msg := range messages {
		if len(lines) >= maxItems {
			break
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "unknown"
		}
		if len(content) > 500 {
			content = content[:500] + "..."
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", role, content))
	}
	if len(lines) == 0 {
		return "No relevant previous discussion found.", nil
	}

	req := openai.ChatCompletionRequest{
		Model: t.chatModel,
		Messages: []openai.ChatCompletionMessage{
			{
				Role: openai.ChatMessageRoleSystem,
				Content: "You summarize prior conversation snippets. " +
					"Return a concise summary in plain text with key points only.",
			},
			{
				Role: openai.ChatMessageRoleUser,
				Content: fmt.Sprintf(
					"Topic: %s\n\nPrevious messages:\n%s\n\nWrite a concise summary.",
					topic,
					strings.Join(lines, "\n"),
				),
			},
		},
	}
	resp, err := t.client.CreateChatCompletion(ctx, req)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices for long memory summary")
	}

	summary := strings.TrimSpace(resp.Choices[0].Message.Content)
	if summary == "" {
		return "", fmt.Errorf("llm returned empty long memory summary")
	}
	return summary, nil
}

func summarizeMemoryFallback(messages []memstorage.Message, maxItems int) string {
	if len(messages) == 0 {
		return "No relevant previous discussion found."
	}
	if maxItems <= 0 {
		maxItems = 5
	}

	seen := make(map[string]struct{}, maxItems)
	lines := make([]string, 0, maxItems)
	for _, msg := range messages {
		if len(lines) >= maxItems {
			break
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "unknown"
		}

		key := role + "::" + content
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		if len(content) > 240 {
			content = content[:240] + "..."
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", role, content))
	}
	if len(lines) == 0 {
		return "No relevant previous discussion found."
	}

	return strings.Join(lines, "\n")
}

func min(a int, b int) int {
	if a < b {
		return a
	}
	return b
}
