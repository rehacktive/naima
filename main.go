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
	"time"

	"github.com/joho/godotenv"
	memcore "github.com/rehacktive/memorya/memorya"
	log "github.com/sirupsen/logrus"

	"naima/internal/agent"
	"naima/internal/httpapi"
	"naima/internal/llm"
	"naima/internal/memory"
	"naima/internal/tasks"
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
	log.SetFormatter(&log.TextFormatter{
		ForceColors:            true,
		FullTimestamp:          true,
		TimestampFormat:        time.RFC3339,
		DisableLevelTruncation: true,
	})
	log.SetOutput(os.Stdout)
	log.SetLevel(log.InfoLevel)

	fmt.Print(banner)

	llmConfig, err := llm.LoadConfigFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	client := llm.NewOpenAIClient(llmConfig)
	systemPrompt, err := loadSystemPrompt()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

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
	memSize := envInt("NAIMA_MEMORY_MAX_CONTEXT", 50)
	memorySummarizer := memory.NewLLMSummarizer(client, llmConfig.Model, time.Duration(envInt("NAIMA_MEMORY_SUMMARY_TIMEOUT_MS", 8000))*time.Millisecond)
	memoryInstance := memcore.InitMemoryaWithSummarizer(memSize, memStore, memorySummarizer)
	telegramNotifier := telegram.NewNotifier(strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")), os.Getenv("NAIMA_SESSION_FILE"))
	taskManager, err := tasks.NewManager(ctx, pgvectorDSN(), telegramNotifier, taskLocation())
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	toolset := []tools.Tool{
		tools.NewTimeTool(),
		tools.NewWebSearchTool(searxURL()),
		tools.NewPlaywrightTool(playwrightHeadless(), envInt("NAIMA_PLAYWRIGHT_TIMEOUT_MS", 30000)),
		tools.NewTaskSchedulerTool(taskManager),
		tools.NewLongMemoryTool(client, llmConfig.Model, llmConfig.EmbeddingModel, memStore),
		tools.NewMemoryDumpTool(memoryInstance),
	}
	if token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")); token != "" {
		toolset = append(toolset, tools.NewTelegramSendTool(token, os.Getenv("NAIMA_SESSION_FILE")))
	}

	agentInstance := agent.New(
		*name,
		systemPrompt,
		client,
		llmConfig.Model,
		llmConfig.EmbeddingModel,
		memoryInstance,
		toolset,
	)
	taskManager.SetAgent(agentInstance)
	if err := taskManager.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

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
			log.Infof("[agent] starting web interface")
			errCh <- httpapi.RunServer(ctx, agentInstance)
		}()
	}

	if telegramEnabled {
		running++
		go func() {
			log.Infof("[agent] starting telegram interface")
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

func promptPath() string {
	if p := strings.TrimSpace(os.Getenv("NAIMA_PROMPT_FILE")); p != "" {
		return p
	}

	return "prompt.txt"
}

func playwrightHeadless() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("NAIMA_PLAYWRIGHT_HEADLESS")))
	switch raw {
	case "", "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func loadSystemPrompt() (string, error) {
	path := promptPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read system prompt file failed (%s): %w", path, err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", fmt.Errorf("system prompt file is empty: %s", path)
	}

	return prompt, nil
}

func taskLocation() *time.Location {
	raw := strings.TrimSpace(os.Getenv("NAIMA_TASK_TIMEZONE"))
	if raw == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(raw)
	if err != nil {
		return time.UTC
	}
	return loc
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
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("load .env failed: %w", err)
	}

	return nil
}
