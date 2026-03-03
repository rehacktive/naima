package telegram

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	log "github.com/sirupsen/logrus"

	"naima/internal/safeio"

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
	streamEnabled := isTelegramStreamEnabled()
	log.Infof("[telegram] draft streaming enabled=%t", streamEnabled)

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

			text := strings.TrimSpace(update.Message.Text)
			log.Infof("[telegram] message received user_id=%d chat_id=%d chars=%d", update.Message.From.ID, update.Message.Chat.ID, len(text))
			if text == "/new" || text == "/reset" {
				response := "Memory reset. Starting a new conversation."
				if err := agentInstance.ResetMemory(); err != nil {
					response = fmt.Sprintf("Error: %s", err.Error())
				}
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, response)
				_, _ = bot.Send(msg)
				continue
			}

			var (
				response string
				err      error
			)
			if streamEnabled {
				streamer := newDraftStreamer(bot, update.Message.Chat.ID)
				response, err = agentInstance.ProcessMessageStream(ctx, text, streamer.OnDelta)
				streamer.Flush()
			} else {
				response, err = agentInstance.ProcessMessage(ctx, text)
			}
			if err != nil {
				response = fmt.Sprintf("Error: %s", err.Error())
			}

			msg := tgbotapi.NewMessage(update.Message.Chat.ID, response)
			_, _ = bot.Send(msg)
		}
	}
}

func isTelegramStreamEnabled() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("NAIMA_TELEGRAM_STREAM")))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

type draftStreamer struct {
	bot      *tgbotapi.BotAPI
	chatID   int64
	draftID  int64
	buffer   strings.Builder
	lastSent time.Time
	enabled  bool
}

func newDraftStreamer(bot *tgbotapi.BotAPI, chatID int64) *draftStreamer {
	return &draftStreamer{
		bot:     bot,
		chatID:  chatID,
		draftID: time.Now().UnixMilli(),
		enabled: true,
	}
}

func (s *draftStreamer) OnDelta(delta string) {
	if strings.TrimSpace(delta) == "" {
		return
	}
	s.buffer.WriteString(delta)
	if !s.enabled {
		return
	}
	if !s.lastSent.IsZero() && time.Since(s.lastSent) < 250*time.Millisecond {
		return
	}
	if err := s.sendCurrent(); err != nil {
		log.Warnf("[telegram] sendMessageDraft failed: %v", err)
		s.enabled = false
		return
	}
	s.lastSent = time.Now()
}

func (s *draftStreamer) Flush() {
	if !s.enabled {
		return
	}
	if strings.TrimSpace(s.buffer.String()) == "" {
		return
	}
	if err := s.sendCurrent(); err != nil {
		log.Warnf("[telegram] final sendMessageDraft failed: %v", err)
		s.enabled = false
	}
}

func (s *draftStreamer) sendCurrent() error {
	text := strings.TrimSpace(s.buffer.String())
	if text == "" {
		return nil
	}

	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", s.chatID)
	params.AddNonZero64("draft_id", s.draftID)
	params["text"] = text

	resp, err := s.bot.MakeRequest("sendMessageDraft", params)
	if err != nil {
		return fmt.Errorf("telegram request failed: %w", err)
	}
	if !resp.Ok {
		return fmt.Errorf("telegram api error (%d): %s", resp.ErrorCode, resp.Description)
	}

	return nil
}

func sessionFilePath() string {
	if path := os.Getenv("NAIMA_SESSION_FILE"); path != "" {
		return path
	}

	return filepath.Join(".", defaultSessionFile)
}

func loadSession(path string) (sessionData, error) {
	data, err := safeio.ReadFile(path)
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
