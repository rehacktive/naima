package agent

import (
	"context"
	"fmt"
	"time"
)

type Agent struct {
	Name string
}

func New(name string) *Agent {
	return &Agent{Name: name}
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

	if input == "" {
		return "Send a message and I'll process it.", nil
	}

	return fmt.Sprintf("%s processed: %s", a.Name, input), nil
}
