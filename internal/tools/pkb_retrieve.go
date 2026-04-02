package tools

import (
	"context"
	"strings"
	"time"

	"naima/internal/pkb"

	openai "github.com/sashabaranov/go-openai"
)

const (
	defaultPKBRetrieveTimeout    = 12 * time.Second
	defaultPKBRetrieveDocLimit   = 3
	defaultPKBRetrieveChunkLimit = 3
	maxPKBRetrieveDocLimit       = 8
	maxPKBRetrieveChunkLimit     = 8
)

type PKBRetrieveSearcher interface {
	SearchRelevantDocuments(ctx context.Context, queryEmbeddings []float32, docLimit int, chunkLimit int) ([]pkb.RelevantDocument, error)
}

type PKBRetrieveTool struct {
	client         *openai.Client
	embeddingModel string
	searcher       PKBRetrieveSearcher
}

type pkbRetrieveParams struct {
	Query             string `json:"query"`
	DocumentLimit     int    `json:"document_limit,omitempty"`
	ChunksPerDocument int    `json:"chunks_per_document,omitempty"`
}

func NewPKBRetrieveTool(client *openai.Client, embeddingModel string, searcher PKBRetrieveSearcher) Tool {
	return &PKBRetrieveTool{
		client:         client,
		embeddingModel: strings.TrimSpace(embeddingModel),
		searcher:       searcher,
	}
}

func (t *PKBRetrieveTool) GetName() string {
	return "pkb_retrieve"
}

func (t *PKBRetrieveTool) GetDescription() string {
	return "Searches the personal knowledge base semantically and returns the nearest documents and chunks for a query."
}

func (t *PKBRetrieveTool) GetFunction() func(params string) string {
	return func(params string) string {
		return t.Execute(context.Background(), params)
	}
}

func (t *PKBRetrieveTool) Execute(ctx context.Context, params string) string {
	var in pkbRetrieveParams
	if err := jsonUnmarshal(params, &in); err != nil {
		return errorJSON("invalid params: " + err.Error())
	}

	query := strings.TrimSpace(in.Query)
	if query == "" {
		return errorJSON("query is required")
	}
	if t.client == nil || t.embeddingModel == "" || t.searcher == nil {
		return errorJSON("pkb retrieval is not configured")
	}

	docLimit := in.DocumentLimit
	if docLimit <= 0 {
		docLimit = defaultPKBRetrieveDocLimit
	}
	if docLimit > maxPKBRetrieveDocLimit {
		docLimit = maxPKBRetrieveDocLimit
	}

	chunkLimit := in.ChunksPerDocument
	if chunkLimit <= 0 {
		chunkLimit = defaultPKBRetrieveChunkLimit
	}
	if chunkLimit > maxPKBRetrieveChunkLimit {
		chunkLimit = maxPKBRetrieveChunkLimit
	}

	ctx, cancel := context.WithTimeout(ctx, defaultPKBRetrieveTimeout)
	defer cancel()

	embResp, err := t.client.CreateEmbeddings(ctx, openai.EmbeddingRequest{
		Input: []string{query},
		Model: openai.EmbeddingModel(t.embeddingModel),
	})
	if err != nil {
		return errorJSON("embedding request failed: " + err.Error())
	}
	if len(embResp.Data) == 0 {
		return errorJSON("embedding response returned no vectors")
	}

	docs, err := t.searcher.SearchRelevantDocuments(ctx, append([]float32(nil), embResp.Data[0].Embedding...), docLimit, chunkLimit)
	if err != nil {
		return errorJSON("pkb search failed: " + err.Error())
	}

	return mustJSON(map[string]any{
		"query":               query,
		"document_limit":      docLimit,
		"chunks_per_document": chunkLimit,
		"count":               len(docs),
		"documents":           docs,
	})
}

func (t *PKBRetrieveTool) IsImmediate() bool {
	return false
}

func (t *PKBRetrieveTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Semantic question or lookup to search in the personal knowledge base.",
			},
			"document_limit": map[string]any{
				"type":        "integer",
				"description": "Optional number of documents to return (1-8).",
				"minimum":     1,
				"maximum":     8,
			},
			"chunks_per_document": map[string]any{
				"type":        "integer",
				"description": "Optional number of relevant chunks to return for each document (1-8).",
				"minimum":     1,
				"maximum":     8,
			},
		},
		Required: []string{"query"},
	}
}
