package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BaronBonet/rig/internal/core"
	"github.com/BaronBonet/rig/internal/pkg/transcript"
)

// interruptMarkerPrefix starts the text Claude Code records as a user entry
// when the user interrupts a turn with Esc, "[Request interrupted by user]" or
// "[Request interrupted by user for tool use]". An interrupted turn sends no
// hook, so the session keeps reading as working until the next prompt.
const interruptMarkerPrefix = "[Request interrupted by user"

// LastTurnInterrupted reports whether the user interrupted the session's last
// turn: the newest user entry of its transcript is Claude Code's interruption
// marker. Only the end of the transcript is read, since transcripts of long
// sessions run to hundreds of megabytes. A session without a transcript was
// not interrupted.
//
// The transcript is the one rig recorded for the session when its hooks
// reported it. Only when none was recorded, or the file is gone, is it looked
// for in the Claude configuration the task runs with.
func (r *repository) LastTurnInterrupted(
	_ context.Context,
	task *core.Task,
	session core.TaskProviderSession,
) (bool, error) {
	if recorded := strings.TrimSpace(session.TranscriptPath); recorded != "" {
		interrupted, err := transcriptTailInterrupted(recorded)
		if !errors.Is(err, fs.ErrNotExist) {
			return interrupted, err
		}
	}

	path, err := r.sessionTranscriptPath(task, session.ProviderSessionID)
	if err != nil || path == "" {
		return false, err
	}
	interrupted, err := transcriptTailInterrupted(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return interrupted, err
}

// transcriptTailInterrupted reads the end of the transcript at path.
func transcriptTailInterrupted(path string) (bool, error) {
	lines, err := transcript.Tail(path)
	if err != nil {
		return false, err
	}
	return lastUserEntryInterrupted(lines), nil
}

// sessionTranscriptPath finds the transcript of one of the task's sessions in
// the project folders of the configuration the task runs with, or "" when
// there is none. Claude Code files a session under the folder it was started
// in, which is the task's workspace unless the session has been moved, so
// that folder is tried first and the others after it.
func (r *repository) sessionTranscriptPath(task *core.Task, sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || sessionID != filepath.Base(sessionID) {
		return "", fmt.Errorf("invalid session ID %q", sessionID)
	}
	configDir, err := r.resolveConfigDir(task.ProviderEnv)
	if err != nil {
		return "", err
	}
	projectsDir := filepath.Join(configDir, "projects")
	file := sessionID + ".jsonl"

	if folder := strings.TrimSpace(task.WorktreePath); folder != "" {
		own := filepath.Join(projectsDir, claudeProjectDirName(filepath.Clean(folder)), file)
		if _, err := os.Stat(own); err == nil {
			return own, nil
		}
	}
	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("list claude projects in %q: %w", projectsDir, err)
	}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		candidate := filepath.Join(projectsDir, project.Name(), file)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", nil
}

// lastUserEntryInterrupted reads transcript lines, oldest first, from the
// end: the first main-conversation user entry it meets decides.
func lastUserEntryInterrupted(lines [][]byte) bool {
	for i := len(lines) - 1; i >= 0; i-- {
		var entry claudeActivityLine
		if json.Unmarshal(lines[i], &entry) != nil || entry.Type != "user" {
			continue
		}
		// Meta entries are injected context and sidechain entries belong to
		// subagents; neither is the user speaking to this session.
		if entry.IsMeta || entry.IsSidechain {
			continue
		}
		return isInterruptMarker(entry.Message.Content)
	}
	return false
}

// isInterruptMarker reports whether a user entry's content is the
// interruption marker, as plain text or as a text block.
func isInterruptMarker(content json.RawMessage) bool {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return strings.HasPrefix(strings.TrimSpace(text), interruptMarkerPrefix)
	}
	for _, block := range claudeContentBlocks(content) {
		if block.Type == "text" && strings.HasPrefix(strings.TrimSpace(block.Text), interruptMarkerPrefix) {
			return true
		}
	}
	return false
}
