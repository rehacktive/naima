package memory

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type FileStorage struct {
	path string
	mu   sync.Mutex
	data persistedData
}

type persistedData struct {
	Messages []Message `json:"messages"`
}

func NewFileStorage(path string) (*FileStorage, error) {
	s := &FileStorage{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *FileStorage) StoreMessage(message Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if message.Id == "" {
		id, err := randomID()
		if err != nil {
			return err
		}
		message.Id = ID(id)
	}
	if message.CreatedAt == nil {
		now := time.Now().UTC()
		message.CreatedAt = &now
	}

	for _, existing := range s.data.Messages {
		if existing.Id == message.Id {
			return nil
		}
	}

	s.data.Messages = append(s.data.Messages, message)
	return s.persistLocked()
}

func (s *FileStorage) SearchRelatedMessages(_ []float32) ([]Message, error) {
	// This implementation intentionally disables semantic recall.
	return nil, nil
}

func (s *FileStorage) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.data = persistedData{Messages: make([]Message, 0)}
			return nil
		}
		return fmt.Errorf("read memory file failed: %w", err)
	}

	var parsed persistedData
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("parse memory file failed: %w", err)
	}
	if parsed.Messages == nil {
		parsed.Messages = make([]Message, 0)
	}
	s.data = parsed
	return nil
}

func (s *FileStorage) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("create memory dir failed: %w", err)
	}

	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize memory file failed: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp memory file failed: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace memory file failed: %w", err)
	}

	return nil
}

func randomID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate message id failed: %w", err)
	}

	return hex.EncodeToString(b[:]), nil
}
