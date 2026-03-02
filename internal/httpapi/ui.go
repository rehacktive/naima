package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func serveUI(w http.ResponseWriter, _ *http.Request) {
	path := filepath.Join(uiDir(), "index.html")
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "ui file not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func uiDir() string {
	if v := strings.TrimSpace(os.Getenv("NAIMA_UI_DIR")); v != "" {
		return v
	}
	return filepath.Join(".", "internal", "httpapi", "ui")
}
