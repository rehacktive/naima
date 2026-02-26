package llm

import (
	"errors"
	"os"
	"strings"
)

type Config struct {
	APIKey         string
	Model          string
	EmbeddingModel string
	BaseURL        string
}

func LoadConfigFromEnv() (Config, error) {
	cfg := Config{
		APIKey:         strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		Model:          strings.TrimSpace(os.Getenv("OPENAI_MODEL")),
		EmbeddingModel: strings.TrimSpace(os.Getenv("OPENAI_EMBEDDING_MODEL")),
		BaseURL:        strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
	}

	if cfg.Model == "" {
		return Config{}, errors.New("OPENAI_MODEL is not set")
	}
	if cfg.EmbeddingModel == "" {
		return Config{}, errors.New("OPENAI_EMBEDDING_MODEL is not set")
	}
	if cfg.APIKey == "" && cfg.BaseURL == "" {
		return Config{}, errors.New("OPENAI_API_KEY is not set")
	}

	return cfg, nil
}
