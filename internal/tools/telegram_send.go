package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const defaultTelegramSessionFile = ".naima_session.json"

type TelegramSendTool struct {
	botToken    string
	sessionPath string
}

type telegramSendParams struct {
	Message string `json:"message"`
}

type telegramSession struct {
	UserID      int64  `json:"user_id"`
	PendingCode string `json:"pending_code"`
}

func NewTelegramSendTool(botToken string, sessionPath string) Tool {
	path := strings.TrimSpace(sessionPath)
	if path == "" {
		path = filepath.Join(".", defaultTelegramSessionFile)
	}
	return &TelegramSendTool{
		botToken:    strings.TrimSpace(botToken),
		sessionPath: path,
	}
}

func (t *TelegramSendTool) GetName() string {
	return "telegram_send"
}

func (t *TelegramSendTool) GetDescription() string {
	return "Sends a text message to the linked Telegram user. Use when user asks to forward/send a result to Telegram."
}

func (t *TelegramSendTool) GetFunction() func(params string) string {
	return func(params string) string {
		var in telegramSendParams
		if err := json.Unmarshal([]byte(params), &in); err != nil {
			return errorJSON("invalid params: " + err.Error())
		}

		text := strings.TrimSpace(in.Message)
		if text == "" {
			return errorJSON("message is required")
		}
		if t.botToken == "" {
			return errorJSON("telegram bot token is not configured")
		}

		session, err := t.readSession()
		if err != nil {
			return errorJSON(err.Error())
		}
		if session.UserID == 0 {
			return errorJSON("telegram session is not linked yet")
		}

		bot, err := tgbotapi.NewBotAPI(t.botToken)
		if err != nil {
			return errorJSON("telegram bot init failed: " + err.Error())
		}

		msg := tgbotapi.NewMessage(session.UserID, text)
		if _, err := bot.Send(msg); err != nil {
			return errorJSON("telegram send failed: " + err.Error())
		}

		payload := map[string]any{
			"status":  "sent",
			"chat_id": session.UserID,
			"chars":   len(text),
		}
		out, err := json.Marshal(payload)
		if err != nil {
			return errorJSON("serialize telegram_send result failed: " + err.Error())
		}
		return string(out)
	}
}

func (t *TelegramSendTool) IsImmediate() bool {
	return false
}

func (t *TelegramSendTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"message": map[string]any{
				"type":        "string",
				"description": "Text to send to Telegram.",
			},
		},
		Required: []string{"message"},
	}
}

func (t *TelegramSendTool) readSession() (telegramSession, error) {
	data, err := os.ReadFile(t.sessionPath)
	if err != nil {
		if os.IsNotExist(err) {
			return telegramSession{}, fmt.Errorf("telegram session file not found: %s", t.sessionPath)
		}
		return telegramSession{}, fmt.Errorf("read telegram session failed: %w", err)
	}

	var out telegramSession
	if err := json.Unmarshal(data, &out); err != nil {
		return telegramSession{}, fmt.Errorf("parse telegram session failed: %w", err)
	}
	return out, nil
}
