package promptfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStoreWritePromptFile_WritesThePromptUnderTheDataDirReadableByTheUserAlone(t *testing.T) {
	dataDir := t.TempDir()
	store := New(dataDir)
	prompt := "- review the open PRs\n\n$(rm -rf /) 'quoted' \"double\"\n"

	path, err := store.WritePromptFile("1760000000000", prompt)

	require.NoError(t, err)
	require.Equal(t, filepath.Join(dataDir, "prompts", "1760000000000.txt"), path)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, prompt, string(content), "the text is stored as it is")
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dirInfo, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
}

func TestStoreWritePromptFile_OverwritesThePreviousRunsPrompt(t *testing.T) {
	store := New(t.TempDir())

	first, err := store.WritePromptFile("task-1", "first run, which is the longer prompt")
	require.NoError(t, err)
	second, err := store.WritePromptFile("task-1", "second")
	require.NoError(t, err)

	require.Equal(t, first, second, "one file per task")
	content, err := os.ReadFile(second)
	require.NoError(t, err)
	require.Equal(t, "second", string(content))
}

func TestStoreWritePromptFile_ReplacesAFileThatWasReadableByOthers(t *testing.T) {
	dataDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "prompts"), 0o755))
	existing := filepath.Join(dataDir, "prompts", "task-1.txt")
	require.NoError(t, os.WriteFile(existing, []byte("old"), 0o644))
	require.NoError(t, os.Chmod(existing, 0o644))

	path, err := New(dataDir).WritePromptFile("task-1", "new")

	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestStoreWritePromptFile_KeepsEachTasksPromptApart(t *testing.T) {
	store := New(t.TempDir())

	one, err := store.WritePromptFile("task-1", "one")
	require.NoError(t, err)
	two, err := store.WritePromptFile("task-2", "two")
	require.NoError(t, err)

	require.NotEqual(t, one, two)
	content, err := os.ReadFile(one)
	require.NoError(t, err)
	require.Equal(t, "one", string(content))
}

func TestStoreWritePromptFile_LeavesNoTemporaryFilesBehind(t *testing.T) {
	dataDir := t.TempDir()
	store := New(dataDir)

	for range 3 {
		_, err := store.WritePromptFile("task-1", "prompt")
		require.NoError(t, err)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "prompts"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "task-1.txt", entries[0].Name())
}

func TestStoreWritePromptFile_HoldsAPromptOfAnySize(t *testing.T) {
	store := New(t.TempDir())
	prompt := strings.Repeat("a long prompt line\n", 100_000)

	path, err := store.WritePromptFile("task-1", prompt)

	require.NoError(t, err)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, prompt, string(content))
}

func TestStoreWritePromptFile_RefusesATaskIDThatIsNotAFileName(t *testing.T) {
	dataDir := t.TempDir()
	store := New(dataDir)

	for _, taskID := range []string{"", " ", ".", "..", "../escape", "a/b"} {
		_, err := store.WritePromptFile(taskID, "prompt")

		require.ErrorContains(t, err, "invalid task ID", taskID)
	}
	_, err := os.Stat(filepath.Join(dataDir, "prompts"))
	require.True(t, os.IsNotExist(err), "nothing is created for a refused ID")
}

func TestStoreWritePromptFile_NeedsADataDir(t *testing.T) {
	_, err := New("").WritePromptFile("task-1", "prompt")

	require.ErrorContains(t, err, "data dir")
}

func TestStoreWritePromptFile_ReportsAnUnwritableDataDir(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))

	_, err := New(blocker).WritePromptFile("task-1", "prompt")

	require.ErrorContains(t, err, "prompts directory")
}
