package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/joho/godotenv"
	memcore "github.com/rehacktive/memorya/memorya"
	openai "github.com/sashabaranov/go-openai"
	log "github.com/sirupsen/logrus"

	"naima/internal/agent"
	"naima/internal/httpapi"
	"naima/internal/llm"
	"naima/internal/memory"
	"naima/internal/persona"
	"naima/internal/pkb"
	"naima/internal/research"
	"naima/internal/safeio"
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
	pkbStorage, err := pkb.NewStorage(ctx, pgvectorDSN(), pkb.Config{
		Embedder:            newPKBEmbeddingGenerator(client),
		Tagger:              newPKBTagExtractor(client, llmConfig.Model),
		EmbeddingModel:      llmConfig.EmbeddingModel,
		ChunkSize:           envInt("NAIMA_PKB_CHUNK_SIZE", 2000),
		TagLimit:            envInt("NAIMA_PKB_TAG_LIMIT", 12),
		VectorDims:          envInt("NAIMA_PGVECTOR_EMBEDDING_DIMS", 0),
		RetrievalDocLimit:   envInt("NAIMA_PKB_RETRIEVAL_DOC_LIMIT", 3),
		RetrievalChunkLimit: envInt("NAIMA_PKB_RETRIEVAL_CHUNK_LIMIT", 4),
		RetrievalThreshold:  envFloat("NAIMA_PKB_RETRIEVAL_THRESHOLD", 0.35),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	defer pkbStorage.Close()
	personaManager, err := persona.NewManager(ctx, pgvectorDSN(), client, llmConfig.Model, time.Duration(envInt("NAIMA_PERSONA_EXTRACT_INTERVAL_SEC", 120))*time.Second, envInt("NAIMA_PERSONA_LOOKBACK_MESSAGES", 24), envInt("NAIMA_PERSONA_MAX_FACTS", 12))
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	researchManager, err := research.NewManager(ctx, pgvectorDSN(), pkbStorage, client, llmConfig.Model, pkbIngestConfig(), searxURL(), telegramNotifier)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	toolset := []tools.Tool{
		tools.NewTimeTool(),
		tools.NewWeatherTool(),
		tools.NewWebSearchTool(searxURL()),
		tools.NewNewsDigestTool(searxURL()),
		tools.NewPersonalKnowledgeBaseTool(pkbStorage, pkbIngestConfig()),
		tools.NewPersonaTool(personaManager),
		tools.NewDeepResearchTool(researchManager),
		tools.NewPKBRetrieveTool(client, llmConfig.EmbeddingModel, pkbStorage),
		tools.NewBashTool(bashToolURL()),
		tools.NewPlaywrightTool(playwrightHeadless(), envInt("NAIMA_PLAYWRIGHT_TIMEOUT_MS", 30000)),
		tools.NewTaskSchedulerTool(taskManager),
		tools.NewLongMemoryTool(client, llmConfig.Model, llmConfig.EmbeddingModel, memStore),
		tools.NewMemoryDumpTool(memoryInstance),
	}
	if emailTool := tools.NewEmailToolFromEnv(personaManager); emailTool != nil {
		toolset = append(toolset, emailTool)
	}
	if token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")); token != "" {
		toolset = append(toolset, tools.NewTelegramSendTool(token, os.Getenv("NAIMA_SESSION_FILE")))
	}

	agentInstance := agent.New(
		*name,
		systemPrompt,
		toolPromptsDir(),
		client,
		llmConfig.Model,
		llmConfig.EmbeddingModel,
		memoryInstance,
		pkbStorage,
		personaManager,
		toolset,
	)
	applyDefaultToolStates(agentInstance)
	taskManager.SetAgent(agentInstance)
	if err := taskManager.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	if err := researchManager.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	if err := personaManager.Start(ctx, memoryInstance); err != nil {
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
			errCh <- httpapi.RunServer(ctx, agentInstance, pkbStorage, researchManager, personaManager, telegramNotifier)
		}()
	}

	if telegramEnabled {
		running++
		go func() {
			log.Infof("[agent] starting telegram interface")
			errCh <- telegram.RunBot(ctx, agentInstance, personaManager)
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

func bashToolURL() string {
	if p := strings.TrimSpace(os.Getenv("NAIMA_BASH_TOOL_URL")); p != "" {
		return p
	}

	return ""
}

func tikaURL() string {
	if p := strings.TrimSpace(os.Getenv("NAIMA_TIKA_URL")); p != "" {
		return p
	}

	return ""
}

func tikaAllowFallback() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("NAIMA_TIKA_ALLOW_FALLBACK")))
	switch raw {
	case "", "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func pkbIngestConfig() pkb.IngestConfig {
	return pkb.IngestConfig{
		Mode:                strings.TrimSpace(os.Getenv("NAIMA_PKB_INGEST_MODE")),
		TikaURL:             tikaURL(),
		AllowFallback:       tikaAllowFallback(),
		PlaywrightHeadless:  playwrightHeadless(),
		PlaywrightTimeoutMS: envInt("NAIMA_PLAYWRIGHT_TIMEOUT_MS", 30000),
	}
}

func promptPath() string {
	if p := strings.TrimSpace(os.Getenv("NAIMA_PROMPT_FILE")); p != "" {
		return p
	}

	return "prompt.txt"
}

func applyDefaultToolStates(agentInstance *agent.Agent) {
	if agentInstance == nil {
		return
	}

	for _, tool := range agentInstance.ListTools() {
		desired, ok := toolEnabledFromEnv(tool.Name)
		if !ok {
			continue
		}
		if err := agentInstance.SetToolEnabled(tool.Name, desired); err != nil {
			log.Warnf("[agent] set default tool state failed name=%s err=%v", tool.Name, err)
		}
	}
}

func toolEnabledFromEnv(toolName string) (bool, bool) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(toolStateEnvKey(toolName))))
	switch raw {
	case "":
		return false, false
	case "enabled", "enable", "true", "1", "yes", "on":
		return true, true
	case "disabled", "disable", "false", "0", "no", "off":
		return false, true
	default:
		log.Warnf("[agent] invalid tool state env %s=%q", toolStateEnvKey(toolName), raw)
		return false, false
	}
}

func toolStateEnvKey(toolName string) string {
	normalized := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			return unicode.ToUpper(r)
		default:
			return '_'
		}
	}, strings.TrimSpace(toolName))
	return "NAIMA_TOOL_" + normalized
}

func toolPromptsDir() string {
	if p := strings.TrimSpace(os.Getenv("NAIMA_TOOL_PROMPTS_DIR")); p != "" {
		return p
	}

	return filepath.Join(".", "internal", "tools")
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
	data, err := safeio.ReadFile(path)
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

func newPKBEmbeddingGenerator(client *openai.Client) pkb.EmbeddingGenerator {
	if client == nil {
		return nil
	}
	return func(ctx context.Context, inputs []string, model string) ([][]float32, error) {
		resp, err := client.CreateEmbeddings(ctx, openai.EmbeddingRequest{
			Input: inputs,
			Model: openai.EmbeddingModel(model),
		})
		if err != nil {
			return nil, err
		}
		out := make([][]float32, len(resp.Data))
		for _, item := range resp.Data {
			if item.Index < 0 || item.Index >= len(out) {
				continue
			}
			out[item.Index] = append([]float32(nil), item.Embedding...)
		}
		for i := range out {
			if len(out[i]) == 0 {
				return nil, fmt.Errorf("embedding response returned no vector for input %d", i)
			}
		}
		return out, nil
	}
}

func newPKBTagExtractor(client *openai.Client, model string) pkb.TagExtractor {
	if client == nil || strings.TrimSpace(model) == "" {
		return nil
	}
	return func(ctx context.Context, content string, limit int) ([]pkb.ExtractedTag, error) {
		if limit <= 0 {
			limit = 5
		}
		if limit > 20 {
			limit = 20
		}

		payload := strings.TrimSpace(content)
		if payload == "" {
			return nil, nil
		}
		if len(payload) > 16000 {
			payload = payload[:16000]
		}

		req := openai.ChatCompletionRequest{
			Model: model,
			Messages: []openai.ChatCompletionMessage{
				{
					Role: openai.ChatMessageRoleSystem,
					Content: `You are an expert Natural Language Processing system specialized in Named Entity Recognition (NER).

Your task is to analyze the provided text and extract all named entities. Identify and classify each entity into one of the following categories:

PERSON
ORGANIZATION
LOCATION
DATE
TIME
MONEY
PERCENT
PRODUCT
EVENT
OTHER (if none of the above apply)
Instructions:
Read the input text carefully.
Extract all named entities exactly as they appear (do not modify wording).
Assign the most appropriate category to each entity.
Avoid duplicates unless the same entity appears multiple times in different contexts.
If no entities are found, return an empty list.`,
				},
				{
					Role: openai.ChatMessageRoleUser,
					Content: fmt.Sprintf(
						"Analyze the following text and extract up to %d named entities.\n"+
							"Return ONLY valid JSON using this schema:\n"+
							"{\"entities\":[{\"text\":\"entity\",\"category\":\"PERSON\"}]}\n\n"+
							"Text:\n%s",
						limit,
						payload,
					),
				},
			},
		}

		resp, err := client.CreateChatCompletion(ctx, req)
		if err != nil {
			return nil, err
		}
		if len(resp.Choices) == 0 {
			return nil, fmt.Errorf("tag extractor returned no choices")
		}
		raw := strings.TrimSpace(resp.Choices[0].Message.Content)
		raw = trimJSONCodeFence(raw)
		if raw == "" {
			return nil, fmt.Errorf("tag extractor returned empty content")
		}

		var parsed struct {
			Entities []struct {
				Text     string `json:"text"`
				Category string `json:"category"`
			} `json:"entities"`
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			// fallback: allow a direct JSON array
			var arr []string
			if err2 := json.Unmarshal([]byte(raw), &arr); err2 != nil {
				return nil, err
			}
			out := make([]pkb.ExtractedTag, 0, len(arr))
			for _, item := range arr {
				text := strings.TrimSpace(item)
				if text == "" {
					continue
				}
				out = append(out, pkb.ExtractedTag{
					Text:     text,
					Category: "OTHER",
				})
			}
			return out, nil
		}

		if len(parsed.Entities) > 0 {
			out := make([]pkb.ExtractedTag, 0, len(parsed.Entities))
			for _, entity := range parsed.Entities {
				text := strings.TrimSpace(entity.Text)
				if text == "" {
					continue
				}
				out = append(out, pkb.ExtractedTag{
					Text:     text,
					Category: strings.ToUpper(strings.TrimSpace(entity.Category)),
				})
			}
			return out, nil
		}
		if len(parsed.Tags) > 0 {
			out := make([]pkb.ExtractedTag, 0, len(parsed.Tags))
			for _, item := range parsed.Tags {
				text := strings.TrimSpace(item)
				if text == "" {
					continue
				}
				out = append(out, pkb.ExtractedTag{
					Text:     text,
					Category: "OTHER",
				})
			}
			return out, nil
		}
		return nil, nil
	}
}

func trimJSONCodeFence(raw string) string {
	value := strings.TrimSpace(raw)
	if strings.HasPrefix(value, "```") {
		value = strings.TrimPrefix(value, "```json")
		value = strings.TrimPrefix(value, "```JSON")
		value = strings.TrimPrefix(value, "```")
		value = strings.TrimSuffix(value, "```")
		value = strings.TrimSpace(value)
	}
	return value
}

func envFloat(name string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return v
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
