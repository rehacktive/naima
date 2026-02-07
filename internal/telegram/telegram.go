package telegram

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"naima/internal/agent"
)

const defaultSessionFile = ".naima_session.json"

type sessionData struct {
	UserID      int64  `json:"user_id"`
	PendingCode string `json:"pending_code"`
}

func RunBot(ctx context.Context, agentInstance *agent.Agent) error {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return errors.New("TELEGRAM_BOT_TOKEN is not set")
	}

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return fmt.Errorf("telegram bot init failed: %w", err)
	}

	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 30
	updates := bot.GetUpdatesChan(updateConfig)

	sessionPath := sessionFilePath()
	session, err := loadSession(sessionPath)
	if err != nil {
		return err
	}

	if session.UserID == 0 {
		code, generated, err := ensurePendingCode(&session)
		if err != nil {
			return err
		}
		if generated {
			if err := saveSession(sessionPath, session); err != nil {
				return err
			}
			fmt.Printf("Link code (send via Telegram): %s\n", code)
		} else {
			fmt.Printf("Awaiting link code (send via Telegram): %s\n", code)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case update, ok := <-updates:
			if !ok {
				return errors.New("telegram updates channel closed")
			}
			if update.Message == nil {
				continue
			}
			if update.Message.From == nil {
				continue
			}

			if session.UserID == 0 {
				response := "Send the verification code to link this agent."
				if update.Message.Text == session.PendingCode {
					session.UserID = update.Message.From.ID
					session.PendingCode = ""
					if err := saveSession(sessionPath, session); err != nil {
						response = fmt.Sprintf("Error: %s", err.Error())
					} else {
						response = "Linked. You can now interact with this agent."
					}
				}

				msg := tgbotapi.NewMessage(update.Message.Chat.ID, response)
				_, _ = bot.Send(msg)
				continue
			}

			if update.Message.From.ID != session.UserID {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Not authorized for this agent.")
				_, _ = bot.Send(msg)
				continue
			}

			response, err := agentInstance.ProcessMessage(ctx, update.Message.Text)
			if err != nil {
				response = fmt.Sprintf("Error: %s", err.Error())
			}

			msg := tgbotapi.NewMessage(update.Message.Chat.ID, response)
			_, _ = bot.Send(msg)
		}
	}
}

func sessionFilePath() string {
	if path := os.Getenv("NAIMA_SESSION_FILE"); path != "" {
		return path
	}

	return filepath.Join(".", defaultSessionFile)
}

func loadSession(path string) (sessionData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sessionData{}, nil
		}
		return sessionData{}, fmt.Errorf("read session file failed: %w", err)
	}

	var session sessionData
	if err := json.Unmarshal(data, &session); err != nil {
		return sessionData{}, fmt.Errorf("parse session file failed: %w", err)
	}

	return session, nil
}

func saveSession(path string, session sessionData) error {
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize session file failed: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write session file failed: %w", err)
	}

	return nil
}

func ensurePendingCode(session *sessionData) (string, bool, error) {
	if session.UserID != 0 {
		return "", false, nil
	}
	if session.PendingCode != "" {
		return session.PendingCode, false, nil
	}

	code, err := generateCode(10)
	if err != nil {
		return "", false, err
	}
	session.PendingCode = code
	return code, true, nil
}

func generateCode(length int) (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	if length <= 0 {
		return "", errors.New("invalid code length")
	}

	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("random code generation failed: %w", err)
	}

	for i, b := range bytes {
		bytes[i] = alphabet[int(b)%len(alphabet)]
	}

	return string(bytes), nil
}
