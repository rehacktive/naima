package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	memcore "github.com/rehacktive/memorya/memorya"
	memstorage "github.com/rehacktive/memorya/storage"
	openai "github.com/sashabaranov/go-openai"
	log "github.com/sirupsen/logrus"

	"naima/internal/pkb"
	"naima/internal/safeio"
	"naima/internal/tools"
)

const maxToolRounds = 10
const maxToolLogChars = 220

type Agent struct {
	Name           string
	SystemPrompt   string
	ToolPromptDir  string
	Client         *openai.Client
	Model          string
	EmbeddingModel string
	Memory         ConversationMemory
	PKB            PKBRetriever
	Tools          map[string]tools.Tool
	ToolEnabled    map[string]bool
	mu             sync.Mutex
}

type ConversationMemory interface {
	AddMessage(message memstorage.Message, pinned bool)
	GetMessages() []memstorage.Message
	GetStatus() memcore.Status
	Reset()
}

type ToolState struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Immediate   bool   `json:"immediate"`
	Enabled     bool   `json:"enabled"`
}

type PKBRetriever interface {
	SearchRelevantDocuments(ctx context.Context, queryEmbeddings []float32, docLimit int, chunkLimit int) ([]pkb.RelevantDocument, error)
}

type MemoryStatusView struct {
	memcore.Status
	SummaryMessages int `json:"summary_messages"`
	RecallMessages  int `json:"recall_messages"`
}

func New(name string, systemPrompt string, toolPromptDir string, client *openai.Client, model string, embeddingModel string, memory ConversationMemory, pkb PKBRetriever, toolset []tools.Tool) *Agent {
	toolMap := make(map[string]tools.Tool, len(toolset))
	for _, tool := range toolset {
		if tool == nil {
			continue
		}
		toolMap[tool.GetName()] = tool
	}

	return &Agent{
		Name:           name,
		SystemPrompt:   strings.TrimSpace(systemPrompt),
		ToolPromptDir:  strings.TrimSpace(toolPromptDir),
		Client:         client,
		Model:          model,
		EmbeddingModel: embeddingModel,
		Memory:         memory,
		PKB:            pkb,
		Tools:          toolMap,
		ToolEnabled:    enabledMap(toolMap),
	}
}

func (a *Agent) ProcessMessage(ctx context.Context, input string) (string, error) {
	return a.processMessage(ctx, input, nil, nil)
}

func (a *Agent) ProcessMessageStream(ctx context.Context, input string, onDelta func(string)) (string, error) {
	return a.processMessage(ctx, input, onDelta, nil)
}

func (a *Agent) ProcessMessageStreamWithOps(ctx context.Context, input string, onDelta func(string), onOp func(string)) (string, error) {
	return a.processMessage(ctx, input, onDelta, onOp)
}

func (a *Agent) processMessage(ctx context.Context, input string, onDelta func(string), onOp func(string)) (string, error) {
	startedAt := time.Now()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}

	normalizedInput := strings.TrimSpace(input)
	if normalizedInput == "" {
		return "", fmt.Errorf("message is required")
	}

	a.mu.Lock()
	client := a.Client
	model := a.Model
	embeddingModel := a.EmbeddingModel
	systemPrompt := a.SystemPrompt
	toolPromptDir := a.ToolPromptDir
	memoryStore := a.Memory
	pkbRetriever := a.PKB
	activeTools := a.enabledToolsLocked()
	a.mu.Unlock()

	systemPrompt = composeSystemPrompt(systemPrompt, toolPromptDir, activeTools)

	if client == nil {
		return "", fmt.Errorf("llm client is not configured")
	}
	if model == "" {
		return "", fmt.Errorf("llm model is not configured")
	}
	if systemPrompt == "" {
		return "", fmt.Errorf("system prompt is not configured")
	}
	if embeddingModel == "" {
		return "", fmt.Errorf("embedding model is not configured")
	}
	if memoryStore == nil {
		return "", fmt.Errorf("memory is not configured")
	}

	log.Infof("[agent] message received chars=%d", len(normalizedInput))
	emitOp(onOp, "message received")
	embResp, err := client.CreateEmbeddings(ctx, openai.EmbeddingRequest{
		Input: []string{normalizedInput},
		Model: openai.EmbeddingModel(embeddingModel),
	})
	if err != nil {
		return "", fmt.Errorf("embedding request failed: %w", err)
	}
	if len(embResp.Data) == 0 {
		return "", fmt.Errorf("embedding response returned no vectors")
	}
	emb := append([]float32(nil), embResp.Data[0].Embedding...)
	log.Infof("[agent] embeddings generated dim=%d", len(emb))
	emitOp(onOp, fmt.Sprintf("embeddings generated (dim=%d)", len(emb)))

	pkbContext := ""
	if pkbRetriever != nil && shouldUsePKBRetrieval(normalizedInput) {
		log.Infof("[agent] pkb retrieval triggered")
		emitOp(onOp, "pkb retrieval started")
		docs, err := pkbRetriever.SearchRelevantDocuments(ctx, emb, 3, 3)
		if err != nil {
			log.Warnf("[agent] pkb retrieval failed: %v", err)
			emitOp(onOp, "pkb retrieval failed")
		} else if len(docs) > 0 {
			pkbContext = buildPKBContext(docs)
			summary := summarizePKBRetrieval(docs)
			log.Infof("[agent] pkb context selected documents=%d details=%s", len(docs), summary)
			emitOp(onOp, fmt.Sprintf("pkb chunks retrieved: %s", summary))
			emitOp(onOp, fmt.Sprintf("pkb context loaded (%d documents)", len(docs)))
		} else {
			log.Infof("[agent] pkb retrieval returned no matching documents")
			emitOp(onOp, "pkb retrieval found no matching documents")
		}
	}

	now := time.Now().UTC()
	a.mu.Lock()
	memoryStore.AddMessage(memstorage.Message{
		Role:       openai.ChatMessageRoleUser,
		Content:    normalizedInput,
		Embeddings: &emb,
		CreatedAt:  &now,
	}, false)
	memoryMessages := memoryStore.GetMessages()
	a.mu.Unlock()
	log.Infof("[agent] user message saved to memory")
	emitOp(onOp, "user message saved")

	messages := make([]openai.ChatCompletionMessage, 0, len(memoryMessages)+1)
	messages = append(messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: systemPrompt,
	})
	if pkbContext != "" {
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleSystem,
			Content: pkbContext,
		})
	}
	for _, msg := range memoryMessages {
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    normalizeRole(msg.Role),
			Content: msg.Content,
		})
	}

	req := openai.ChatCompletionRequest{
		Model:    model,
		Messages: messages,
	}
	if len(activeTools) > 0 {
		req.Tools = toOpenAITools(activeTools)
	}
	log.Infof("[agent] sending model request context_messages=%d tools=%d", len(messages), len(req.Tools))
	emitOp(onOp, "model request started")

	msg, err := a.runModel(ctx, client, req, onDelta)
	if err != nil {
		return "", fmt.Errorf("llm request failed: %w", err)
	}

	toolRoundLimit := maxToolRoundsFor(activeTools)
	for toolRound := 1; len(msg.ToolCalls) > 0; toolRound++ {
		if toolRound > toolRoundLimit {
			return "", fmt.Errorf("tool execution exceeded max rounds (%d)", toolRoundLimit)
		}

		log.Infof("[agent] model requested tool calls round=%d count=%d", toolRound, len(msg.ToolCalls))
		emitOp(onOp, fmt.Sprintf("tool calls requested (round=%d, count=%d)", toolRound, len(msg.ToolCalls)))

		for _, call := range msg.ToolCalls {
			tool, ok := activeTools[call.Function.Name]
			if !ok {
				return "", fmt.Errorf("tool not found: %s", call.Function.Name)
			}

			immediate := tool.IsImmediate()
			if provider, ok := tool.(tools.ImmediateToolProvider); ok {
				immediate = provider.IsImmediateForParams(call.Function.Arguments)
			}
			log.Infof("[agent] executing tool name=%s immediate=%t", call.Function.Name, immediate)
			emitOp(onOp, fmt.Sprintf("executing tool: %s", call.Function.Name))
			out := tool.GetFunction()(call.Function.Arguments)
			log.Infof("[agent] tool output name=%s chars=%d preview=%s", call.Function.Name, len(out), truncateForLog(out, maxToolLogChars))
			emitOp(onOp, fmt.Sprintf("tool output: %s (%d chars)", call.Function.Name, len(out)))
			if toolErr, ok := extractToolError(out); ok {
				log.Warnf("[agent] tool returned error name=%s error=%s", call.Function.Name, toolErr)
				emitOp(onOp, fmt.Sprintf("tool error: %s", toolErr))
			}
			if immediate {
				reply := out
				if msg, ok := extractImmediateUserMessage(out); ok {
					reply = msg
				}
				now = time.Now().UTC()
				a.mu.Lock()
				memoryStore.AddMessage(memstorage.Message{
					Role:      openai.ChatMessageRoleAssistant,
					Content:   reply,
					CreatedAt: &now,
				}, false)
				a.mu.Unlock()
				log.Infof("[agent] replied via immediate tool name=%s elapsed=%s", call.Function.Name, time.Since(startedAt).Round(time.Millisecond))
				emitOp(onOp, "reply sent (immediate tool)")
				return reply, nil
			}

			messages = append(messages, openai.ChatCompletionMessage{
				Role:      openai.ChatMessageRoleAssistant,
				ToolCalls: []openai.ToolCall{call},
			})
			messages = append(messages, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Name:       call.Function.Name,
				ToolCallID: call.ID,
				Content:    out,
			})
		}

		followReq := openai.ChatCompletionRequest{
			Model:    model,
			Messages: messages,
		}
		if len(activeTools) > 0 {
			followReq.Tools = toOpenAITools(activeTools)
		}
		log.Infof("[agent] sending follow-up model request after tool execution round=%d", toolRound)
		emitOp(onOp, fmt.Sprintf("model follow-up after tools (round=%d)", toolRound))

		msg, err = a.runModel(ctx, client, followReq, onDelta)
		if err != nil {
			return "", fmt.Errorf("llm request failed after tool execution: %w", err)
		}
	}

	answer := strings.TrimSpace(msg.Content)
	if answer == "" {
		return "", fmt.Errorf("llm returned an empty response")
	}
	now = time.Now().UTC()
	a.mu.Lock()
	memoryStore.AddMessage(memstorage.Message{
		Role:      openai.ChatMessageRoleAssistant,
		Content:   answer,
		CreatedAt: &now,
	}, false)
	a.mu.Unlock()
	log.Infof("[agent] assistant message saved; replied chars=%d elapsed=%s", len(answer), time.Since(startedAt).Round(time.Millisecond))
	emitOp(onOp, "assistant replied")

	return answer, nil
}

func composeSystemPrompt(base string, toolPromptDir string, activeTools map[string]tools.Tool) string {
	base = strings.TrimSpace(base)
	if toolPromptDir == "" || len(activeTools) == 0 {
		return base
	}

	names := make([]string, 0, len(activeTools))
	for name := range activeTools {
		names = append(names, name)
	}
	sort.Strings(names)

	sections := make([]string, 0, len(names))
	for _, name := range names {
		path := filepath.Join(toolPromptDir, name+".md")
		data, err := safeio.ReadFile(path)
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}
		sections = append(sections, fmt.Sprintf("Tool `%s` guidance:\n%s", name, content))
	}

	if len(sections) == 0 {
		return base
	}
	if base == "" {
		return strings.Join(sections, "\n\n")
	}
	return base + "\n\n" + strings.Join(sections, "\n\n")
}

func shouldUsePKBRetrieval(input string) bool {
	lower := strings.ToLower(strings.TrimSpace(input))
	if lower == "" {
		return false
	}
	blocked := []string{
		"current document selection",
		"what's the current document selection",
		"what is the current document selection",
		"selected documents for chat",
	}
	for _, pattern := range blocked {
		if strings.Contains(lower, pattern) {
			return false
		}
	}
	patterns := []string{
		"pkb",
		"knowledge base",
		"my notes",
		"my documents",
		"my files",
		"my uploads",
		"uploaded",
		"ingested",
		"invoice",
		"receipt",
		"contract",
		"agreement",
		"report",
		"paper",
		"pdf",
		"topic ",
		"topic:",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func buildPKBContext(docs []pkb.RelevantDocument) string {
	if len(docs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Relevant personal knowledge base documents were retrieved for this request.\n")
	b.WriteString("Use them when answering. Prefer them over unsupported guesses. If they are insufficient, say so.\n\n")
	totalChars := 0
	for i, doc := range docs {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		title := strings.TrimSpace(doc.Document.Title)
		if title == "" {
			title = fmt.Sprintf("Document %d", doc.Document.ID)
		}
		b.WriteString(fmt.Sprintf("Document %d\n", i+1))
		b.WriteString(fmt.Sprintf("Title: %s\n", title))
		if doc.Document.TopicTitle != "" {
			b.WriteString(fmt.Sprintf("Topic: %s\n", doc.Document.TopicTitle))
		}
		if doc.Document.CreatedAt != nil {
			b.WriteString(fmt.Sprintf("Created: %s\n", doc.Document.CreatedAt.UTC().Format(time.RFC3339)))
		}
		if doc.Document.SourceURL != "" {
			b.WriteString(fmt.Sprintf("Source: %s\n", doc.Document.SourceURL))
		}
		if len(doc.Chunks) > 0 {
			b.WriteString("Most relevant chunks:\n")
			for _, chunk := range doc.Chunks {
				b.WriteString(fmt.Sprintf("- chunk %d (distance %.4f): %s\n", chunk.ChunkIndex+1, chunk.Distance, truncateForContext(chunk.Content, 350)))
			}
		}
		b.WriteString("\nFull content:\n")
		content := truncateForContext(doc.Document.Content, 6000)
		totalChars += len(content)
		if totalChars > 16000 {
			content = truncateForContext(content, max(0, 16000-(totalChars-len(content))))
		}
		b.WriteString(content)
		if totalChars >= 16000 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func summarizePKBRetrieval(docs []pkb.RelevantDocument) string {
	if len(docs) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(docs))
	for _, doc := range docs {
		title := strings.TrimSpace(doc.Document.Title)
		if title == "" {
			title = fmt.Sprintf("document %d", doc.Document.ID)
		}
		parts = append(parts, fmt.Sprintf("%s (%d chunks, min_distance=%.4f)", title, len(doc.Chunks), doc.MinDistance))
	}
	return strings.Join(parts, "; ")
}

func truncateForContext(value string, maxChars int) string {
	value = strings.TrimSpace(value)
	if maxChars <= 0 || len(value) <= maxChars {
		return value
	}
	return strings.TrimSpace(value[:maxChars]) + "..."
}

func (a *Agent) runModel(ctx context.Context, client *openai.Client, req openai.ChatCompletionRequest, onDelta func(string)) (openai.ChatCompletionMessage, error) {
	if onDelta == nil {
		response, err := client.CreateChatCompletion(ctx, req)
		if err != nil {
			return openai.ChatCompletionMessage{}, err
		}
		if len(response.Choices) == 0 {
			return openai.ChatCompletionMessage{}, fmt.Errorf("llm returned no choices")
		}
		return response.Choices[0].Message, nil
	}

	stream, err := client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return openai.ChatCompletionMessage{}, err
	}
	defer stream.Close()

	msg := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant}
	toolCallsByIndex := make(map[int]*openai.ToolCall)
	order := make([]int, 0)

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return openai.ChatCompletionMessage{}, err
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		for _, choice := range chunk.Choices {
			delta := choice.Delta
			if delta.Role != "" {
				msg.Role = delta.Role
			}
			if delta.Content != "" {
				msg.Content += delta.Content
				onDelta(delta.Content)
			}
			for _, deltaCall := range delta.ToolCalls {
				idx := 0
				if deltaCall.Index != nil {
					idx = *deltaCall.Index
				}

				existing, ok := toolCallsByIndex[idx]
				if !ok {
					tc := openai.ToolCall{
						Type:     deltaCall.Type,
						ID:       deltaCall.ID,
						Function: openai.FunctionCall{},
					}
					toolCallsByIndex[idx] = &tc
					existing = &tc
					order = append(order, idx)
				}

				if deltaCall.ID != "" {
					existing.ID = deltaCall.ID
				}
				if deltaCall.Type != "" {
					existing.Type = deltaCall.Type
				}
				if deltaCall.Function.Name != "" {
					existing.Function.Name += deltaCall.Function.Name
				}
				if deltaCall.Function.Arguments != "" {
					existing.Function.Arguments += deltaCall.Function.Arguments
				}
			}
		}
	}

	if len(order) > 0 {
		msg.ToolCalls = make([]openai.ToolCall, 0, len(order))
		for _, idx := range order {
			if tc := toolCallsByIndex[idx]; tc != nil {
				msg.ToolCalls = append(msg.ToolCalls, *tc)
			}
		}
	}

	return msg, nil
}

func (a *Agent) ResetMemory() error {
	if a.Memory == nil {
		return fmt.Errorf("memory is not configured")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.Memory.Reset()
	log.Infof("[agent] memory reset")
	return nil
}

func (a *Agent) MemoryStatus() (MemoryStatusView, error) {
	if a.Memory == nil {
		return MemoryStatusView{}, fmt.Errorf("memory is not configured")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	base := a.Memory.GetStatus()
	view := MemoryStatusView{Status: base}
	for _, msg := range a.Memory.GetMessages() {
		content := strings.TrimSpace(msg.Content)
		if strings.HasPrefix(content, "Summary:") || strings.HasPrefix(content, "Summary (fallback):") {
			view.SummaryMessages++
		}
		if strings.HasPrefix(content, "Recalled context:") {
			view.RecallMessages++
		}
	}
	return view, nil
}

func (a *Agent) ListTools() []ToolState {
	a.mu.Lock()
	defer a.mu.Unlock()

	names := make([]string, 0, len(a.Tools))
	for name := range a.Tools {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ToolState, 0, len(names))
	for _, name := range names {
		tool := a.Tools[name]
		out = append(out, ToolState{
			Name:        name,
			Description: tool.GetDescription(),
			Immediate:   tool.IsImmediate(),
			Enabled:     a.ToolEnabled[name],
		})
	}
	return out
}

func (a *Agent) SetToolEnabled(name string, enabled bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("tool name is required")
	}
	if _, ok := a.Tools[name]; !ok {
		return fmt.Errorf("tool not found: %s", name)
	}
	a.ToolEnabled[name] = enabled
	log.Infof("[agent] tool state changed name=%s enabled=%t", name, enabled)
	return nil
}

func normalizeRole(role string) string {
	switch role {
	case openai.ChatMessageRoleSystem, openai.ChatMessageRoleUser, openai.ChatMessageRoleAssistant:
		return role
	default:
		return openai.ChatMessageRoleUser
	}
}

func toOpenAITools(toolset map[string]tools.Tool) []openai.Tool {
	ret := make([]openai.Tool, 0, len(toolset))
	for _, tool := range toolset {
		params := tool.GetParameters()
		if params.Type == "" {
			params.Type = "object"
		}

		schema := map[string]any{
			"type":       params.Type,
			"properties": params.Properties,
			"required":   params.Required,
		}

		ret = append(ret, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        tool.GetName(),
				Description: tool.GetDescription(),
				Parameters:  schema,
			},
		})
	}

	return ret
}

func emitOp(onOp func(string), message string) {
	if onOp != nil {
		onOp(message)
	}
}

func truncateForLog(v string, max int) string {
	s := strings.TrimSpace(v)
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

func extractToolError(out string) (string, bool) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return "", false
	}
	raw, ok := payload["error"]
	if !ok {
		return "", false
	}
	msg, ok := raw.(string)
	if !ok {
		return "", false
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "", false
	}
	return msg, true
}

func extractImmediateUserMessage(out string) (string, bool) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return "", false
	}
	raw, ok := payload["user_message"]
	if !ok {
		return "", false
	}
	msg, ok := raw.(string)
	if !ok {
		return "", false
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "", false
	}
	return msg, true
}

func enabledMap(toolMap map[string]tools.Tool) map[string]bool {
	out := make(map[string]bool, len(toolMap))
	for name := range toolMap {
		out[name] = true
	}
	return out
}

func maxToolRoundsFor(toolset map[string]tools.Tool) int {
	limit := maxToolRounds
	maxAllowed := 3 * maxToolRounds
	for _, tool := range toolset {
		provider, ok := tool.(tools.MaxToolRoundsProvider)
		if !ok {
			continue
		}
		candidate := provider.MaxToolRounds()
		if candidate > maxAllowed {
			candidate = maxAllowed
		}
		if candidate > limit {
			limit = candidate
		}
	}
	return limit
}

func (a *Agent) enabledToolsLocked() map[string]tools.Tool {
	out := make(map[string]tools.Tool, len(a.Tools))
	for name, tool := range a.Tools {
		if a.ToolEnabled[name] {
			out[name] = tool
		}
	}
	return out
}
