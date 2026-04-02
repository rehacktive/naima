package memory

import "time"

type ID string

type Message struct {
	Id         ID         `json:"id"`
	CreatedAt  *time.Time `json:"created_at"`
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Cost       int        `json:"cost"`
	Embeddings *[]float32 `json:"-"`
}

type Status struct {
	MaxContextTokens int  `json:"max_context_tokens"`
	CurrentTokens    int  `json:"current_tokens"`
	CurrentSize      int  `json:"current_size"`
	HasSummarizer    bool `json:"has_summarizer"`
	HasPendingRecall bool `json:"has_pending_recall"`
	OverCapacity     bool `json:"over_capacity"`
}

type Storage interface {
	StoreMessage(message Message) error
	SearchRelatedMessages(query []float32) ([]Message, error)
}

type Summarizer interface {
	Summarize(messages []Message) (Message, error)
}
