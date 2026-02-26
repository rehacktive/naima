package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/joho/godotenv"
	memcore "github.com/rehacktive/memorya/memorya"

	"naima/internal/agent"
	"naima/internal/httpapi"
	"naima/internal/llm"
	"naima/internal/memory"
	"naima/internal/telegram"
	"naima/internal/tools"
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

	memStore, err := memory.NewPGVectorStorage(
		ctx,
		pgvectorDSN(),
		envInt("NAIMA_PGVECTOR_SEARCH_LIMIT", 5),
		envInt("NAIMA_PGVECTOR_EMBEDDING_DIMS", 0),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	defer memStore.Close()
	memSize := envInt("NAIMA_MEMORY_MAX_CONTEXT", 20)
	memoryInstance := memcore.InitMemorya(memSize, memStore)

	agentInstance := agent.New(
		*name,
		client,
		llmConfig.Model,
		llmConfig.EmbeddingModel,
		memoryInstance,
		[]tools.Tool{
			tools.NewTimeTool(),
			tools.NewWebSearchTool(searxURL()),
		},
	)

	apiEnabled := httpapi.IsEnabled()
	telegramEnabled := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")) != ""
	if !apiEnabled && !telegramEnabled {
		fmt.Fprintln(os.Stderr, "no integrations enabled: set TELEGRAM_BOT_TOKEN or NAIMA_API_TOKEN")
		os.Exit(1)
	}

	var (
		errCh   = make(chan error, 2)
		running = 0
	)

	if apiEnabled {
		running++
		go func() {
			errCh <- httpapi.RunServer(ctx, agentInstance)
		}()
	}

	if telegramEnabled {
		running++
		go func() {
			errCh <- telegram.RunBot(ctx, agentInstance)
		}()
	}

	var firstErr error
	for i := 0; i < running; i++ {
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	if firstErr != nil {
		fmt.Fprintln(os.Stderr, firstErr.Error())
		os.Exit(1)
	}
}

func pgvectorDSN() string {
	if p := strings.TrimSpace(os.Getenv("NAIMA_PGVECTOR_DSN")); p != "" {
		return p
	}

	return "postgres://naima:naima@localhost:5432/naima?sslmode=disable"
}

func searxURL() string {
	if p := strings.TrimSpace(os.Getenv("NAIMA_SEARX_URL")); p != "" {
		return p
	}

	return "http://localhost:8081"
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}

	return n
}

func loadEnv() error {
	if err := godotenv.Load(); err != nil {
		return fmt.Errorf("load .env failed: %w", err)
	}

	return nil
}
