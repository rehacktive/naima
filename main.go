package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"naima/internal/agent"
	"naima/internal/llm"
	"naima/internal/telegram"
)

const banner = `
	░▒▓███████▓▒░ ░▒▓██████▓▒░░▒▓█▓▒░▒▓██████████████▓▒░ ░▒▓██████▓▒░  
	░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░ 
	░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░ next
	░▒▓█▓▒░░▒▓█▓▒░▒▓████████▓▒░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░▒▓████████▓▒░ artificial
	░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░ intelligence
	░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░ modular
	░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░ agent                                                           
`

func main() {
	name := flag.String("name", "Naima", "agent name")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := loadEnv(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	fmt.Print(banner)

	llmConfig, err := llm.LoadConfigFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	client := llm.NewOpenAIClient(llmConfig)
	agentInstance := agent.New(*name, client, llmConfig.Model)
	if err := telegram.RunBot(ctx, agentInstance); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func loadEnv() error {
	if err := godotenv.Load(); err != nil {
		return fmt.Errorf("load .env failed: %w", err)
	}

	return nil
}
