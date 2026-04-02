package memory

import "strings"

type Manager struct {
	storage          Storage
	maxContextTokens int
	sequentialMemory []Message
	summarizer       Summarizer
	pendingRecall    []float32
}

func NewManager(maxTokens int, st Storage, summarizer Summarizer) *Manager {
	return &Manager{
		storage:          st,
		maxContextTokens: maxTokens,
		sequentialMemory: make([]Message, 0),
		summarizer:       summarizer,
	}
}

func (m *Manager) SetSummarizer(summarizer Summarizer) {
	m.summarizer = summarizer
}

func (m *Manager) Reset() {
	m.sequentialMemory = make([]Message, 0)
}

func (m *Manager) AddMessage(message Message) {
	if message.Embeddings != nil && len(*message.Embeddings) > 0 {
		m.pendingRecall = append([]float32(nil), (*message.Embeddings)...)
	}
	if message.Cost <= 0 {
		message.Cost = estimateTokens(message.Content)
	}
	if err := m.storage.StoreMessage(message); err != nil {
		panic(err)
	}
	message.Embeddings = nil
	m.sequentialMemory = append(m.sequentialMemory, message)
	m.refresh()
}

func (m *Manager) GetMessages() []Message {
	return append([]Message(nil), m.sequentialMemory...)
}

func (m *Manager) GetStatus() Status {
	currentTokens := totalTokens(m.sequentialMemory)
	return Status{
		MaxContextTokens: m.maxContextTokens,
		CurrentTokens:    currentTokens,
		CurrentSize:      len(m.sequentialMemory),
		HasSummarizer:    m.summarizer != nil,
		HasPendingRecall: len(m.pendingRecall) > 0,
		OverCapacity:     m.maxContextTokens > 0 && currentTokens > m.maxContextTokens,
	}
}

func (m *Manager) refresh() {
	workingMemory := append([]Message(nil), m.sequentialMemory...)
	if len(m.pendingRecall) > 0 {
		recalled := m.remember(m.pendingRecall)
		if recallMessage, ok := buildRecallMessage(recalled, workingMemory); ok {
			workingMemory = append(workingMemory, recallMessage)
		}
		m.pendingRecall = nil
	}
	if m.maxContextTokens <= 0 {
		m.sequentialMemory = workingMemory
		return
	}
	if totalTokens(workingMemory) <= m.maxContextTokens {
		m.sequentialMemory = workingMemory
		return
	}

	budget := m.maxContextTokens
	condensed := fitMessagesToBudget(workingMemory, budget)

	if m.summarizer != nil && len(condensed) < len(workingMemory) {
		recent := fitMessagesToBudget(workingMemory, max(1, budget/2))
		recentMap := make(map[ID]struct{}, len(recent))
		for _, msg := range recent {
			recentMap[msg.Id] = struct{}{}
		}
		summaryCandidates := make([]Message, 0, len(workingMemory))
		for _, msg := range workingMemory {
			if _, ok := recentMap[msg.Id]; ok {
				continue
			}
			summaryCandidates = append(summaryCandidates, msg)
		}
		if len(summaryCandidates) > 0 {
			summary, err := m.summarizer.Summarize(summaryCandidates)
			if err == nil {
				if summary.Role == "" {
					summary.Role = "system"
				}
				if summary.Cost <= 0 {
					summary.Cost = estimateTokens(summary.Content)
				}
				_ = m.storage.StoreMessage(summary)
				withSummary := append([]Message{summary}, recent...)
				condensed = fitMessagesToBudget(withSummary, budget)
			}
		}
	}

	m.sequentialMemory = condensed
}

func (m *Manager) remember(queryEmbeddings []float32) []Message {
	if len(queryEmbeddings) == 0 {
		return nil
	}
	messages, err := m.storage.SearchRelatedMessages(queryEmbeddings)
	if err != nil {
		return nil
	}
	return messages
}

func buildRecallMessage(recalled []Message, current []Message) (Message, bool) {
	if len(recalled) == 0 {
		return Message{}, false
	}
	seen := make(map[string]bool, len(current))
	for _, msg := range current {
		seen[msg.Role+"::"+msg.Content] = true
	}
	lines := make([]string, 0, 3)
	for _, msg := range recalled {
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}
		key := msg.Role + "::" + msg.Content
		if seen[key] {
			continue
		}
		role := msg.Role
		if role == "" {
			role = "unknown"
		}
		lines = append(lines, role+": "+msg.Content)
		if len(lines) == 3 {
			break
		}
	}
	if len(lines) == 0 {
		return Message{}, false
	}
	content := "Recalled context:\n- " + strings.Join(lines, "\n- ")
	return Message{
		Role:    "system",
		Content: content,
		Cost:    estimateTokens(content),
	}, true
}

func fitMessagesToBudget(messages []Message, budget int) []Message {
	if budget <= 0 {
		return nil
	}
	total := 0
	selected := make([]Message, 0, len(messages))
	for i := len(messages) - 1; i >= 0; i-- {
		cost := messages[i].Cost
		if cost <= 0 {
			cost = estimateTokens(messages[i].Content)
			messages[i].Cost = cost
		}
		if total+cost > budget && len(selected) > 0 {
			break
		}
		if total+cost > budget && len(selected) == 0 {
			selected = append(selected, messages[i])
			break
		}
		total += cost
		selected = append(selected, messages[i])
	}
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}
	return selected
}

func totalTokens(messages []Message) int {
	total := 0
	for _, msg := range messages {
		cost := msg.Cost
		if cost <= 0 {
			cost = estimateTokens(msg.Content)
		}
		total += cost
	}
	return total
}

func EstimateTokens(content string) int {
	content = strings.TrimSpace(content)
	if content == "" {
		return 0
	}
	runes := len([]rune(content))
	if runes <= 0 {
		return 0
	}
	return max(1, (runes+3)/4)
}

func estimateTokens(content string) int {
	return EstimateTokens(content)
}
