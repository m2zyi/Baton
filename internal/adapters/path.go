package adapters

import (
	"os"
	"path/filepath"
	"strings"
)

// normalizeDir produces a comparable absolute directory without a trailing
// separator. It resolves symlinks when the path exists, but still accepts a
// normalized absolute path for fixture and not-yet-created directories.
func normalizeDir(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path
}

func sameDir(a, b string) bool {
	na, nb := normalizeDir(a), normalizeDir(b)
	return na != "" && na == nb
}

func envPath(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	if fallback == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return fallback
	}
	return filepath.Join(home, fallback)
}
