package telegram

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type Notifier struct {
	token       string
	sessionPath string
}

func NewNotifier(token string, sessionPath string) *Notifier {
	path := strings.TrimSpace(sessionPath)
	if path == "" {
		path = filepath.Join(".", defaultSessionFile)
	}
	return &Notifier{
		token:       strings.TrimSpace(token),
		sessionPath: path,
	}
}

func (n *Notifier) Enabled() bool {
	return n != nil && n.token != ""
}

func (n *Notifier) Send(text string) error {
	if n == nil || n.token == "" {
		return fmt.Errorf("telegram notifier is not configured")
	}
	body := strings.TrimSpace(text)
	if body == "" {
		return fmt.Errorf("telegram message is empty")
	}

	s, err := n.readSession()
	if err != nil {
		return err
	}
	if s.UserID == 0 {
		return fmt.Errorf("telegram session is not linked")
	}

	bot, err := tgbotapi.NewBotAPI(n.token)
	if err != nil {
		return fmt.Errorf("telegram bot init failed: %w", err)
	}
	msg := tgbotapi.NewMessage(s.UserID, body)
	if _, err := bot.Send(msg); err != nil {
		return fmt.Errorf("telegram send failed: %w", err)
	}
	return nil
}

func (n *Notifier) readSession() (sessionData, error) {
	data, err := os.ReadFile(n.sessionPath)
	if err != nil {
		if os.IsNotExist(err) {
			return sessionData{}, fmt.Errorf("telegram session file not found: %s", n.sessionPath)
		}
		return sessionData{}, fmt.Errorf("read telegram session failed: %w", err)
	}
	var s sessionData
	if err := json.Unmarshal(data, &s); err != nil {
		return sessionData{}, fmt.Errorf("parse telegram session failed: %w", err)
	}
	return s, nil
}
