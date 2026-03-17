package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"naima/internal/agent"
	"naima/internal/pkb"
)

const defaultAddr = ":8080"

const (
	authHeader      = "Authorization"
	bearerPrefix    = "Bearer "
	contentTypeJSON = "application/json"
)

type messageRequest struct {
	Message         string `json:"message"`
	NewConversation bool   `json:"new_conversation"`
}

type messageResponse struct {
	Response string `json:"response"`
}

type streamEvent struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

type toolUpdateRequest struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type pkbIngestRequest struct {
	TopicID  int64  `json:"topic_id,omitempty"`
	NewTopic string `json:"new_topic,omitempty"`
	URL      string `json:"url"`
}

type pkbReader interface {
	ListTopics(ctx context.Context) ([]pkb.Topic, error)
	ListDocuments(ctx context.Context, topicID int64) ([]pkb.Document, error)
	CreateTopic(ctx context.Context, title string) (pkb.Topic, error)
	CreateDocument(ctx context.Context, req pkb.CreateDocumentRequest) (pkb.Document, error)
	DeleteTopic(ctx context.Context, topicID int64) error
	DeleteDocument(ctx context.Context, documentID int64) error
}

func IsEnabled() bool {
	return strings.TrimSpace(os.Getenv("NAIMA_API_TOKEN")) != "" || strings.TrimSpace(os.Getenv("NAIMA_API_ADDR")) != ""
}

func RunServer(ctx context.Context, agentInstance *agent.Agent, pkbStore pkbReader) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ingestCfg := pkbIngestConfigFromEnv()

	mux := http.NewServeMux()
	mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.Dir(uiDir()))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !authorizeUIRequest(r, cfg.UIBasicAuthUser, cfg.UIBasicAuthPass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="Naima UI", charset="UTF-8"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		serveUI(w, r)
	})
	mux.HandleFunc("/api/tools", func(w http.ResponseWriter, r *http.Request) {
		if !authorizeRequest(r, cfg.Token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{"tools": agentInstance.ListTools()})
			return
		case http.MethodPost:
			var req toolUpdateRequest
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid request body")
				return
			}
			if err := agentInstance.SetToolEnabled(req.Name, req.Enabled); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"tools": agentInstance.ListTools()})
			return
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
	})
	mux.HandleFunc("/api/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizeRequest(r, cfg.Token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		var req messageRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.TrimSpace(req.Message) == "" {
			writeError(w, http.StatusBadRequest, "message is required")
			return
		}
		log.Infof("[http] message received remote=%s chars=%d new_conversation=%t", r.RemoteAddr, len(strings.TrimSpace(req.Message)), req.NewConversation)
		if req.NewConversation {
			if err := agentInstance.ResetMemory(); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}

		response, err := agentInstance.ProcessMessage(r.Context(), req.Message)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, messageResponse{Response: response})
	})
	mux.HandleFunc("/api/messages/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizeRequest(r, cfg.Token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		var req messageRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.TrimSpace(req.Message) == "" {
			writeError(w, http.StatusBadRequest, "message is required")
			return
		}
		log.Infof("[http] stream message received remote=%s chars=%d new_conversation=%t", r.RemoteAddr, len(strings.TrimSpace(req.Message)), req.NewConversation)
		if req.NewConversation {
			if err := agentInstance.ResetMemory(); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "streaming is not supported")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		if err := writeSSE(w, "start", streamEvent{Type: "start"}); err != nil {
			return
		}
		flusher.Flush()

		onDelta := func(delta string) {
			if strings.TrimSpace(delta) == "" {
				return
			}
			_ = writeSSE(w, "delta", streamEvent{Type: "delta", Content: delta})
			flusher.Flush()
		}
		onOp := func(op string) {
			if strings.TrimSpace(op) == "" {
				return
			}
			_ = writeSSE(w, "op", streamEvent{Type: "op", Content: op})
			flusher.Flush()
		}

		response, err := agentInstance.ProcessMessageStreamWithOps(r.Context(), req.Message, onDelta, onOp)
		if err != nil {
			log.Errorf("[http] stream request failed remote=%s err=%v", r.RemoteAddr, err)
			errMsg := err.Error()
			if errors.Is(err, context.Canceled) || strings.Contains(strings.ToLower(errMsg), "context canceled") {
				errMsg = "request canceled while waiting for model response; please retry"
			}
			_ = writeSSE(w, "error", streamEvent{Type: "error", Error: errMsg})
			flusher.Flush()
			return
		}

		log.Infof("[http] stream request completed remote=%s chars=%d", r.RemoteAddr, len(strings.TrimSpace(response)))
		_ = writeSSE(w, "done", streamEvent{Type: "done", Content: response})
		flusher.Flush()
	})
	mux.HandleFunc("/api/memory/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizeRequest(r, cfg.Token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		if err := agentInstance.ResetMemory(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/api/memory/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizeRequest(r, cfg.Token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		status, err := agentInstance.MemoryStatus()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": status})
	})
	mux.HandleFunc("/api/pkb/graph", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizeRequest(r, cfg.Token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if pkbStore == nil {
			writeError(w, http.StatusServiceUnavailable, "personal knowledge base is not configured")
			return
		}

		topics, err := pkbStore.ListTopics(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		type topicWithDocuments struct {
			pkb.Topic
			DocumentsList []pkb.Document `json:"documents_list"`
		}
		out := make([]topicWithDocuments, 0, len(topics))
		for _, topic := range topics {
			docs, err := pkbStore.ListDocuments(r.Context(), topic.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			out = append(out, topicWithDocuments{
				Topic:         topic,
				DocumentsList: docs,
			})
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"topics": out,
			"count":  len(out),
		})
	})
	mux.HandleFunc("/api/pkb/topics/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizeRequest(r, cfg.Token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if pkbStore == nil {
			writeError(w, http.StatusServiceUnavailable, "personal knowledge base is not configured")
			return
		}
		id, err := parsePKBID(r.URL.Path, "/api/pkb/topics/")
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := pkbStore.DeleteTopic(r.Context(), id); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "deleted_topic_id": id})
	})
	mux.HandleFunc("/api/pkb/documents/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizeRequest(r, cfg.Token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if pkbStore == nil {
			writeError(w, http.StatusServiceUnavailable, "personal knowledge base is not configured")
			return
		}
		id, err := parsePKBID(r.URL.Path, "/api/pkb/documents/")
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := pkbStore.DeleteDocument(r.Context(), id); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "deleted_document_id": id})
	})
	mux.HandleFunc("/api/pkb/ingest", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizeRequest(r, cfg.Token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if pkbStore == nil {
			writeError(w, http.StatusServiceUnavailable, "personal knowledge base is not configured")
			return
		}

		var req pkbIngestRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		req.URL = strings.TrimSpace(req.URL)
		req.NewTopic = strings.TrimSpace(req.NewTopic)
		if req.URL == "" {
			writeError(w, http.StatusBadRequest, "url is required")
			return
		}
		if req.TopicID <= 0 && req.NewTopic == "" {
			writeError(w, http.StatusBadRequest, "topic_id or new_topic is required")
			return
		}

		topicID := req.TopicID
		var topic pkb.Topic
		if req.NewTopic != "" {
			created, err := pkbStore.CreateTopic(r.Context(), req.NewTopic)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			topic = created
			topicID = created.ID
		}

		ingested, err := pkb.IngestURLContent(r.Context(), &http.Client{Timeout: 20 * time.Second}, ingestCfg, req.URL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if ingested.FallbackNote != "" {
			log.Warnf("[http] pkb ingest fell back to direct text url=%s reason=%s", req.URL, ingested.FallbackNote)
		}
		title := ingested.Title
		if title == "" {
			title = req.URL
		}
		doc, err := pkbStore.CreateDocument(r.Context(), pkb.CreateDocumentRequest{
			TopicID:      topicID,
			Kind:         "url",
			Title:        title,
			SourceURL:    req.URL,
			IngestMethod: ingested.Method,
			Content:      ingested.Content,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"topic":         topic,
			"topic_id":      topicID,
			"document":      doc,
			"ingest_method": ingested.Method,
			"fallback_note": ingested.FallbackNote,
		})
	})
	mux.HandleFunc("/api/pkb/ingest/file", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizeRequest(r, cfg.Token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if pkbStore == nil {
			writeError(w, http.StatusServiceUnavailable, "personal knowledge base is not configured")
			return
		}
		if strings.TrimSpace(ingestCfg.TikaURL) == "" {
			writeError(w, http.StatusServiceUnavailable, "tika is not configured for file ingestion")
			return
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "invalid multipart form")
			return
		}
		topicID, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("topic_id")), 10, 64)
		newTopic := strings.TrimSpace(r.FormValue("new_topic"))
		if topicID <= 0 && newTopic == "" {
			writeError(w, http.StatusBadRequest, "topic_id or new_topic is required")
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, "file is required")
			return
		}
		defer file.Close()

		filename := strings.TrimSpace(header.Filename)
		if filename == "" {
			writeError(w, http.StatusBadRequest, "file name is required")
			return
		}
		data, err := io.ReadAll(io.LimitReader(file, 25<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, "read file failed")
			return
		}
		if len(data) == 0 {
			writeError(w, http.StatusBadRequest, "file is empty")
			return
		}

		if err := os.MkdirAll(pkbUploadDir(), 0o750); err != nil {
			writeError(w, http.StatusInternalServerError, "prepare upload dir failed")
			return
		}
		storedName := time.Now().UTC().Format("20060102T150405") + "_" + filepath.Base(filename)
		storedPath := filepath.Join(pkbUploadDir(), storedName)
		if err := os.WriteFile(storedPath, data, 0o600); err != nil {
			writeError(w, http.StatusInternalServerError, "save uploaded file failed")
			return
		}

		var topic pkb.Topic
		if newTopic != "" {
			created, err := pkbStore.CreateTopic(r.Context(), newTopic)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			topic = created
			topicID = created.ID
		}

		ingested, err := pkb.IngestFileContent(r.Context(), &http.Client{Timeout: time.Duration(envInt("NAIMA_TIKA_FILE_TIMEOUT_MS", 180000)) * time.Millisecond}, ingestCfg.TikaURL, filename, data)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		doc, err := pkbStore.CreateDocument(r.Context(), pkb.CreateDocumentRequest{
			TopicID:      topicID,
			Kind:         "file",
			Title:        ingested.Title,
			SourceURL:    storedPath,
			IngestMethod: ingested.Method,
			Content:      ingested.Content,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"topic":         topic,
			"topic_id":      topicID,
			"document":      doc,
			"stored_path":   storedPath,
			"ingest_method": ingested.Method,
		})
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// Keep SSE streams alive for long-running tool/model flows.
		// Per-request contexts still handle cancellation on client disconnect.
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	shutdownErr := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		shutdownErr <- srv.Shutdown(shutdownCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("rest api server failed: %w", err)
	}

	return <-shutdownErr
}

func parsePKBID(path string, prefix string) (int64, error) {
	raw := strings.TrimSpace(strings.TrimPrefix(path, prefix))
	if raw == "" || raw == path || strings.Contains(raw, "/") {
		return 0, errors.New("invalid resource id")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid resource id")
	}
	return id, nil
}

type config struct {
	Addr            string
	Token           string
	UIBasicAuthUser string
	UIBasicAuthPass string
}

func loadConfig() (config, error) {
	addr := strings.TrimSpace(os.Getenv("NAIMA_API_ADDR"))
	if addr == "" {
		addr = defaultAddr
	}
	token := strings.TrimSpace(os.Getenv("NAIMA_API_TOKEN"))
	if token == "" {
		return config{}, errors.New("NAIMA_API_TOKEN is not set")
	}
	uiUser := strings.TrimSpace(os.Getenv("NAIMA_UI_BASIC_AUTH_USER"))
	uiPass := strings.TrimSpace(os.Getenv("NAIMA_UI_BASIC_AUTH_PASS"))
	if (uiUser == "") != (uiPass == "") {
		return config{}, errors.New("both NAIMA_UI_BASIC_AUTH_USER and NAIMA_UI_BASIC_AUTH_PASS must be set")
	}
	if uiPass != "" && !isSHA256Hex(uiPass) {
		return config{}, errors.New("NAIMA_UI_BASIC_AUTH_PASS must be a lowercase SHA256 hex digest")
	}

	return config{
		Addr:            addr,
		Token:           token,
		UIBasicAuthUser: uiUser,
		UIBasicAuthPass: uiPass,
	}, nil
}

func tikaAllowFallback() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("NAIMA_TIKA_ALLOW_FALLBACK")))
	switch raw {
	case "", "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func pkbIngestConfigFromEnv() pkb.IngestConfig {
	return pkb.IngestConfig{
		Mode:                strings.TrimSpace(os.Getenv("NAIMA_PKB_INGEST_MODE")),
		TikaURL:             strings.TrimSpace(os.Getenv("NAIMA_TIKA_URL")),
		AllowFallback:       tikaAllowFallback(),
		PlaywrightHeadless:  envBool("NAIMA_PLAYWRIGHT_HEADLESS", true),
		PlaywrightTimeoutMS: envInt("NAIMA_PLAYWRIGHT_TIMEOUT_MS", 30000),
	}
}

func pkbUploadDir() string {
	if p := strings.TrimSpace(os.Getenv("NAIMA_PKB_UPLOAD_DIR")); p != "" {
		return p
	}
	return filepath.Join(".", "data", "pkb_uploads")
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

func envBool(name string, fallback bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch raw {
	case "":
		return fallback
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func authorizeRequest(r *http.Request, token string) bool {
	header := r.Header.Get(authHeader)
	if !strings.HasPrefix(header, bearerPrefix) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(header, bearerPrefix))
	return subtle.ConstantTimeCompare([]byte(token), []byte(provided)) == 1
}

func authorizeUIRequest(r *http.Request, user string, pass string) bool {
	if user == "" && pass == "" {
		return true
	}

	u, p, ok := r.BasicAuth()
	if !ok {
		return false
	}

	if subtle.ConstantTimeCompare([]byte(user), []byte(u)) != 1 {
		return false
	}
	sum := sha256.Sum256([]byte(p))
	providedHash := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(pass), []byte(providedHash)) != 1 {
		return false
	}

	return true
}

func isSHA256Hex(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, r := range v {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeSSE(w http.ResponseWriter, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	return nil
}
