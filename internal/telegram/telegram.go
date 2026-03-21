package telegram

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	openai "github.com/sashabaranov/go-openai"
	log "github.com/sirupsen/logrus"

	"naima/internal/safeio"

	"naima/internal/agent"
)

const defaultSessionFile = ".naima_session.json"

const (
	maxTelegramAudioBytes = 25 * 1024 * 1024
	defaultAudioTimeout   = 90 * time.Second
)

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

			audioInput, hasAudio := extractIncomingAudio(update.Message)
			if text == "" && !hasAudio {
				continue
			}

			inputText := text
			audioLanguage := ""
			if hasAudio && text == "" {
				audioCtx, cancel := context.WithTimeout(ctx, defaultAudioTimeout)
				transcript, detectedLang, trErr := transcribeTelegramAudio(audioCtx, bot, agentInstance.Client, audioInput)
				cancel()
				if trErr != nil {
					msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("Error: audio transcription failed: %s", trErr.Error()))
					_, _ = bot.Send(msg)
					continue
				}
				inputText = transcript
				audioLanguage = detectedLang
				log.Infof("[telegram] audio transcribed kind=%s lang=%s chars=%d", audioInput.Kind, audioLanguage, len(strings.TrimSpace(inputText)))
			}

			var (
				response string
				err      error
			)
			opReporter := newTelegramOpReporter(bot, update.Message.Chat.ID)
			if streamEnabled {
				streamer := newDraftStreamer(bot, update.Message.Chat.ID)
				response, err = agentInstance.ProcessMessageStreamWithOps(ctx, inputText, streamer.OnDelta, opReporter.OnOp)
				streamer.Flush()
			} else {
				response, err = agentInstance.ProcessMessageStreamWithOps(ctx, inputText, nil, opReporter.OnOp)
			}
			if err != nil {
				opReporter.OnOp("processing failed: " + err.Error())
				response = fmt.Sprintf("Error: %s", err.Error())
			}

			msg := tgbotapi.NewMessage(update.Message.Chat.ID, response)
			_, _ = bot.Send(msg)

			if hasAudio && err == nil {
				audioCtx, cancel := context.WithTimeout(ctx, defaultAudioTimeout)
				voiceData, ext, ttsErr := synthesizeSpeech(audioCtx, agentInstance.Client, response, audioLanguage)
				cancel()
				if ttsErr != nil {
					log.Warnf("[telegram] tts failed: %v", ttsErr)
					continue
				}
				if sendErr := sendVoiceReply(bot, update.Message.Chat.ID, voiceData, ext); sendErr != nil {
					log.Warnf("[telegram] send voice reply failed: %v", sendErr)
				}
			}
		}
	}
}

type incomingAudio struct {
	Kind     string
	FileID   string
	FileName string
}

func extractIncomingAudio(msg *tgbotapi.Message) (incomingAudio, bool) {
	if msg == nil {
		return incomingAudio{}, false
	}
	if msg.Voice != nil && strings.TrimSpace(msg.Voice.FileID) != "" {
		return incomingAudio{
			Kind:     "voice",
			FileID:   strings.TrimSpace(msg.Voice.FileID),
			FileName: "voice.ogg",
		}, true
	}
	if msg.Audio != nil && strings.TrimSpace(msg.Audio.FileID) != "" {
		name := strings.TrimSpace(msg.Audio.FileName)
		if name == "" {
			name = "audio.mp3"
		}
		return incomingAudio{
			Kind:     "audio",
			FileID:   strings.TrimSpace(msg.Audio.FileID),
			FileName: name,
		}, true
	}
	return incomingAudio{}, false
}

func transcribeTelegramAudio(ctx context.Context, bot *tgbotapi.BotAPI, client *openai.Client, audio incomingAudio) (string, string, error) {
	if client == nil {
		return "", "", errors.New("llm client is not configured")
	}
	if strings.TrimSpace(audio.FileID) == "" {
		return "", "", errors.New("telegram audio file id is empty")
	}

	filePath, err := telegramFilePath(ctx, bot, audio.FileID)
	if err != nil {
		return "", "", err
	}
	content, err := downloadTelegramFile(ctx, bot, filePath, maxTelegramAudioBytes)
	if err != nil {
		return "", "", err
	}

	fileName := normalizeAudioFilename(audio.FileName, filePath)
	req := openai.AudioRequest{
		Model:    transcriptionModel(),
		FilePath: fileName,
		Reader:   bytes.NewReader(content),
		Format:   openai.AudioResponseFormatVerboseJSON,
	}
	resp, err := client.CreateTranscription(ctx, req)
	if err != nil {
		return "", "", fmt.Errorf("openai transcription failed: %w", err)
	}

	text := strings.TrimSpace(resp.Text)
	if text == "" {
		return "", "", errors.New("empty transcription")
	}
	lang := strings.TrimSpace(resp.Language)
	return text, lang, nil
}

func telegramFilePath(ctx context.Context, bot *tgbotapi.BotAPI, fileID string) (string, error) {
	if strings.TrimSpace(fileID) == "" {
		return "", errors.New("telegram file id is empty")
	}
	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("get telegram file failed: %w", err)
	}
	filePath := strings.TrimSpace(file.FilePath)
	if filePath == "" {
		return "", errors.New("telegram file path is empty")
	}
	return filePath, nil
}

func downloadTelegramFile(ctx context.Context, bot *tgbotapi.BotAPI, filePath string, maxBytes int64) ([]byte, error) {
	if strings.TrimSpace(filePath) == "" {
		return nil, errors.New("telegram file path is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.telegram.org/", nil)
	if err != nil {
		return nil, fmt.Errorf("build telegram download request failed: %w", err)
	}
	req.URL.Path = fmt.Sprintf("/file/bot%s/%s", bot.Token, strings.TrimLeft(filePath, "/"))

	httpClient := bot.Client
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download telegram audio failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("telegram file download returned status %d", resp.StatusCode)
	}

	if maxBytes <= 0 {
		maxBytes = maxTelegramAudioBytes
	}
	limited := io.LimitReader(resp.Body, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read telegram audio failed: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("audio file is too large (max %d bytes)", maxBytes)
	}
	return data, nil
}

func normalizeAudioFilename(fileName string, filePath string) string {
	name := strings.TrimSpace(fileName)
	if name == "" {
		name = "audio-input"
	}

	if base := strings.TrimSpace(filepath.Base(filePath)); base != "" && base != "." && base != "/" {
		name = base
	}

	ext := strings.ToLower(strings.TrimSpace(filepath.Ext(name)))
	if ext == "" {
		name = name + ".ogg"
	}
	return name
}

func synthesizeSpeech(ctx context.Context, client *openai.Client, text string, language string) ([]byte, string, error) {
	if client == nil {
		return nil, "", errors.New("llm client is not configured")
	}
	input := strings.TrimSpace(text)
	if input == "" {
		return nil, "", errors.New("speech input is empty")
	}
	_ = strings.TrimSpace(language)

	req := openai.CreateSpeechRequest{
		Model:          speechModel(),
		Input:          input,
		Voice:          speechVoice(),
		ResponseFormat: speechResponseFormat(),
	}

	raw, err := client.CreateSpeech(ctx, req)
	if err != nil {
		return nil, "", fmt.Errorf("openai speech failed: %w", err)
	}
	defer raw.Close()

	data, err := io.ReadAll(raw)
	if err != nil {
		return nil, "", fmt.Errorf("read speech response failed: %w", err)
	}
	if len(data) == 0 {
		return nil, "", errors.New("empty speech response")
	}
	return data, speechFileExt(req.ResponseFormat), nil
}

func sendVoiceReply(bot *tgbotapi.BotAPI, chatID int64, data []byte, ext string) error {
	if len(data) == 0 {
		return errors.New("empty voice data")
	}
	name := "reply" + ext
	if ext == "" {
		name = "reply.mp3"
	}

	voice := tgbotapi.NewVoice(chatID, tgbotapi.FileBytes{Name: name, Bytes: data})
	if _, err := bot.Send(voice); err == nil {
		return nil
	}

	// Fallback to regular audio if the format is rejected as voice note.
	audio := tgbotapi.NewAudio(chatID, tgbotapi.FileBytes{Name: name, Bytes: data})
	if _, err := bot.Send(audio); err != nil {
		return err
	}
	return nil
}

func transcriptionModel() string {
	v := strings.TrimSpace(os.Getenv("NAIMA_TRANSCRIPTION_MODEL"))
	if v == "" {
		return openai.Whisper1
	}
	return v
}

func speechModel() openai.SpeechModel {
	switch strings.TrimSpace(os.Getenv("NAIMA_TTS_MODEL")) {
	case string(openai.TTSModel1HD):
		return openai.TTSModel1HD
	case string(openai.TTSModelCanary):
		return openai.TTSModelCanary
	default:
		return openai.TTSModel1
	}
}

func speechVoice() openai.SpeechVoice {
	switch strings.TrimSpace(os.Getenv("NAIMA_TTS_VOICE")) {
	case string(openai.VoiceEcho):
		return openai.VoiceEcho
	case string(openai.VoiceFable):
		return openai.VoiceFable
	case string(openai.VoiceOnyx):
		return openai.VoiceOnyx
	case string(openai.VoiceNova):
		return openai.VoiceNova
	case string(openai.VoiceShimmer):
		return openai.VoiceShimmer
	default:
		return openai.VoiceAlloy
	}
}

func speechResponseFormat() openai.SpeechResponseFormat {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NAIMA_TTS_FORMAT"))) {
	case string(openai.SpeechResponseFormatOpus):
		return openai.SpeechResponseFormatOpus
	case string(openai.SpeechResponseFormatAac):
		return openai.SpeechResponseFormatAac
	case string(openai.SpeechResponseFormatFlac):
		return openai.SpeechResponseFormatFlac
	case string(openai.SpeechResponseFormatWav):
		return openai.SpeechResponseFormatWav
	case string(openai.SpeechResponseFormatPcm):
		return openai.SpeechResponseFormatPcm
	default:
		return openai.SpeechResponseFormatMp3
	}
}

func speechFileExt(format openai.SpeechResponseFormat) string {
	switch format {
	case openai.SpeechResponseFormatOpus:
		return ".opus"
	case openai.SpeechResponseFormatAac:
		return ".aac"
	case openai.SpeechResponseFormatFlac:
		return ".flac"
	case openai.SpeechResponseFormatWav:
		return ".wav"
	case openai.SpeechResponseFormatPcm:
		return ".pcm"
	default:
		return ".mp3"
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

type telegramOpReporter struct {
	bot      *tgbotapi.BotAPI
	chatID   int64
	lastText string
	lastSent time.Time
}

func newTelegramOpReporter(bot *tgbotapi.BotAPI, chatID int64) *telegramOpReporter {
	return &telegramOpReporter{bot: bot, chatID: chatID}
}

func (r *telegramOpReporter) OnOp(op string) {
	if r == nil || r.bot == nil || r.chatID == 0 {
		return
	}
	text := normalizeTelegramOp(op)
	if text == "" {
		return
	}
	now := time.Now()
	if text == r.lastText && now.Sub(r.lastSent) < 3*time.Second {
		return
	}
	if !r.lastSent.IsZero() && now.Sub(r.lastSent) < 1200*time.Millisecond {
		return
	}

	msg := tgbotapi.NewMessage(r.chatID, "Naima update: "+text)
	if _, err := r.bot.Send(msg); err != nil {
		log.Warnf("[telegram] progress update failed: %v", err)
		return
	}
	r.lastText = text
	r.lastSent = now
}

func normalizeTelegramOp(op string) string {
	value := strings.TrimSpace(op)
	if value == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(value, "executing tool:"):
		name := strings.TrimSpace(strings.TrimPrefix(value, "executing tool:"))
		if name == "" {
			return "using a tool"
		}
		return "using tool " + name
	case strings.HasPrefix(value, "tool error:"):
		return value
	case strings.HasPrefix(value, "processing failed:"):
		return value
	case strings.HasPrefix(value, "pkb"):
		return value
	case strings.HasPrefix(value, "model follow-up"):
		return "continuing after tool results"
	case strings.HasPrefix(value, "model request started"):
		return "thinking"
	case strings.HasPrefix(value, "message received"):
		return "received your message"
	case strings.HasPrefix(value, "reply sent") || strings.HasPrefix(value, "assistant replied"):
		return "finalizing response"
	default:
		// Ignore noisy internal steps like embeddings/tool-output counters.
		return ""
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
