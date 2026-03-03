package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	memstorage "github.com/rehacktive/memorya/storage"
	openai "github.com/sashabaranov/go-openai"
	log "github.com/sirupsen/logrus"
)

const (
	defaultSummaryTimeout = 8 * time.Second
	maxSummaryInputItems  = 20
	maxSummaryItemChars   = 500
)

type LLMSummarizer struct {
	client  *openai.Client
	model   string
	timeout time.Duration
}

func NewLLMSummarizer(client *openai.Client, model string, timeout time.Duration) *LLMSummarizer {
	if timeout <= 0 {
		timeout = defaultSummaryTimeout
	}
	return &LLMSummarizer{
		client:  client,
		model:   strings.TrimSpace(model),
		timeout: timeout,
	}
}

func (s *LLMSummarizer) Summarize(messages []memstorage.Message) (memstorage.Message, error) {
	if s == nil || s.client == nil || s.model == "" {
		return memstorage.Message{}, fmt.Errorf("llm summarizer is not configured")
	}
	if len(messages) == 0 {
		return memstorage.Message{
			Role:    "system",
			Content: "Summary: no previous messages to summarize.",
		}, nil
	}

	items := make([]string, 0, min(len(messages), maxSummaryInputItems))
	for _, m := range messages {
		if len(items) >= maxSummaryInputItems {
			break
		}
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		if len(content) > maxSummaryItemChars {
			content = content[:maxSummaryItemChars] + "...(truncated)"
		}
		role := strings.TrimSpace(m.Role)
		if role == "" {
			role = "unknown"
		}
		items = append(items, fmt.Sprintf("- %s: %s", role, content))
	}
	if len(items) == 0 {
		return memstorage.Message{}, fmt.Errorf("no non-empty messages to summarize")
	}
	log.Infof("[memory] summarizer invoked items=%d model=%s timeout=%s", len(items), s.model, s.timeout)

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	req := openai.ChatCompletionRequest{
		Model: s.model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role: openai.ChatMessageRoleSystem,
				Content: "You summarize conversation history for memory compression. " +
					"Return concise plain text preserving key decisions, facts, and pending actions.",
			},
			{
				Role: openai.ChatMessageRoleUser,
				Content: "Summarize the following messages:\n\n" +
					strings.Join(items, "\n") +
					"\n\nConstraints: keep it factual, concise, and useful for future context.",
			},
		},
	}

	resp, err := s.client.CreateChatCompletion(ctx, req)
	if err != nil {
		log.Warnf("[memory] llm summarizer failed: %v; using fallback summary", err)
		return memstorage.Message{
			Role:    "system",
			Content: buildFallbackSummary(items),
		}, nil
	}
	if len(resp.Choices) == 0 {
		log.Warnf("[memory] llm summarizer returned no choices; using fallback summary")
		return memstorage.Message{
			Role:    "system",
			Content: buildFallbackSummary(items),
		}, nil
	}
	summary := strings.TrimSpace(resp.Choices[0].Message.Content)
	if summary == "" {
		log.Warnf("[memory] llm summarizer returned empty content; using fallback summary")
		return memstorage.Message{
			Role:    "system",
			Content: buildFallbackSummary(items),
		}, nil
	}
	log.Infof("[memory] llm summary generated chars=%d", len(summary))

	return memstorage.Message{
		Role:    "system",
		Content: "Summary: " + summary,
	}, nil
}

func buildFallbackSummary(items []string) string {
	if len(items) == 0 {
		return "Summary: no previous messages to summarize."
	}
	max := min(len(items), 6)
	return "Summary (fallback): key recent context:\n" + strings.Join(items[:max], "\n")
}
