package llm

import (
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

func NewOpenAIClient(cfg Config) *openai.Client {
	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = "local"
	}

	clientConfig := openai.DefaultConfig(apiKey)
	if cfg.BaseURL != "" {
		base := strings.TrimRight(cfg.BaseURL, "/")
		if !strings.HasSuffix(base, "/v1") {
			base = base + "/v1"
		}
		clientConfig.BaseURL = base
	}

	return openai.NewClientWithConfig(clientConfig)
}
