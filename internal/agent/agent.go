package agent

import (
	"context"
	"fmt"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

type Agent struct {
	Name   string
	Client *openai.Client
	Model  string
}

func New(name string, client *openai.Client, model string) *Agent {
	return &Agent{Name: name, Client: client, Model: model}
}

func (a *Agent) Run(ctx context.Context) error {
	fmt.Printf("%s is ready. Press Ctrl+C to stop.\n", a.Name)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Printf("%s shutting down.\n", a.Name)
			return nil
		case <-ticker.C:
			fmt.Printf("%s is thinking...\n", a.Name)
		}
	}
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

	response, err := a.Client.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model: a.Model,
			Messages: []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleSystem, Content: "You are Naima, a helpful AI agent."},
				{Role: openai.ChatMessageRoleUser, Content: input},
			},
		},
	)
	if err != nil {
		return "", fmt.Errorf("llm request failed: %w", err)
	}
	if len(response.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices")
	}

	return response.Choices[0].Message.Content, nil
}
