// Package promptfile keeps the prompts of prompt runs in files under rig's data
// dir. A prompt reaches the provider by being read from its file, so it can be
// any text, of any length, without being quoted or typed.
package promptfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BaronBonet/rig/internal/core"
)

const (
	promptsDirName = "prompts"
	dirMode        = 0o700
)

type store struct {
	dataDir string
}

// New returns a store that keeps its files in <dataDir>/prompts.
func New(dataDir string) core.PromptFileStore {
	return &store{dataDir: dataDir}
}

// WritePromptFile writes text to <dataDir>/prompts/<taskID>.txt, replacing what
// the task's previous run left there, and returns the path. The file is
// readable by the user alone: a prompt may quote anything the user pasted.
// The new file is renamed over the old, so a reader never sees a half-written
// prompt.
func (s *store) WritePromptFile(taskID string, text string) (string, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" || taskID == "." || taskID == ".." || taskID != filepath.Base(taskID) {
		return "", fmt.Errorf("invalid task ID %q for a prompt file", taskID)
	}
	if strings.TrimSpace(s.dataDir) == "" {
		return "", fmt.Errorf("rig data dir is required")
	}

	dir := filepath.Join(s.dataDir, promptsDirName)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return "", fmt.Errorf("create prompts directory: %w", err)
	}

	// The temporary file is created readable by the owner alone.
	file, err := os.CreateTemp(dir, taskID+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("create prompt file: %w", err)
	}
	tmpPath := file.Name()
	_, writeErr := file.WriteString(text)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write prompt file: %w", err)
	}

	path := filepath.Join(dir, taskID+".txt")
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("replace prompt file: %w", err)
	}
	return path, nil
}
