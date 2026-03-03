package safeio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadFile scopes file access to the file's parent directory via os.Root.
// This prevents path traversal outside that directory tree.
func ReadFile(path string) ([]byte, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return nil, fmt.Errorf("path is required")
	}

	dir := filepath.Dir(clean)
	base := filepath.Base(clean)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return nil, fmt.Errorf("invalid file path: %s", path)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	return root.ReadFile(base)
}
