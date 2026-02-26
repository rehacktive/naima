package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"naima/internal/agent"
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

func IsEnabled() bool {
	return strings.TrimSpace(os.Getenv("NAIMA_API_TOKEN")) != "" || strings.TrimSpace(os.Getenv("NAIMA_API_ADDR")) != ""
}

func RunServer(ctx context.Context, agentInstance *agent.Agent) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
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

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	shutdownErr := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr <- srv.Shutdown(shutdownCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("rest api server failed: %w", err)
	}

	return <-shutdownErr
}

type config struct {
	Addr  string
	Token string
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

	return config{Addr: addr, Token: token}, nil
}

func authorizeRequest(r *http.Request, token string) bool {
	header := r.Header.Get(authHeader)
	if !strings.HasPrefix(header, bearerPrefix) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(header, bearerPrefix))
	return subtle.ConstantTimeCompare([]byte(token), []byte(provided)) == 1
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
