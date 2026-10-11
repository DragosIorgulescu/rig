package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BaronBonet/rig/internal/core"
	"github.com/BaronBonet/rig/internal/pkg/subprocess"

	"github.com/stretchr/testify/require"
)

func TestRepositoryBuildPromptRunLaunchSpec_MapsEveryPermissionModeFreshAndResumed(t *testing.T) {
	repo, _ := newTestRepository(t, subprocess.NewMockRunner(t))
	task := &core.Task{Model: "opus", Effort: "high"}

	tests := []struct {
		mode core.TaskPermissionMode
		flag []string
	}{
		{core.TaskPermissionModeAuto, []string{"--permission-mode", "auto"}},
		{core.TaskPermissionModeAcceptEdits, []string{"--permission-mode", "acceptEdits"}},
		{core.TaskPermissionModeBypass, []string{"--permission-mode", "bypassPermissions"}},
		{core.TaskPermissionModeDefault, nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode)+" fresh", func(t *testing.T) {
			launch, err := repo.BuildPromptRunLaunchSpec(task, core.PromptRunLaunch{PermissionMode: tt.mode})

			require.NoError(t, err)
			want := append([]string{"claude", "--model", "opus", "--effort", "high"}, tt.flag...)
			require.Equal(t, want, launch.Command)
			require.Empty(t, launch.PrefillInput, "the prompt is the caller's to append, never typed")
		})
		t.Run(string(tt.mode)+" resumed", func(t *testing.T) {
			launch, err := repo.BuildPromptRunLaunchSpec(task, core.PromptRunLaunch{
				ResumeSessionID: " sess-1 ",
				PermissionMode:  tt.mode,
			})

			require.NoError(t, err)
			want := append([]string{"claude", "--resume", "sess-1", "--model", "opus", "--effort", "high"}, tt.flag...)
			require.Equal(t, want, launch.Command)
		})
	}
}

func TestRepositoryBuildPromptRunLaunchSpec_RunsUnderTheTasksProviderConfiguration(t *testing.T) {
	repo, _ := newTestRepository(t, subprocess.NewMockRunner(t))
	task := &core.Task{ProviderEnv: core.ProviderEnv{"CLAUDE_CONFIG_DIR": "/home/me/.claude-work"}}

	launch, err := repo.BuildPromptRunLaunchSpec(task, core.PromptRunLaunch{
		ResumeSessionID: "sess-1",
		PermissionMode:  core.TaskPermissionModeAuto,
	})

	require.NoError(t, err)
	require.Equal(t, []string{
		"env", "CLAUDE_CONFIG_DIR=/home/me/.claude-work",
		"claude", "--resume", "sess-1", "--permission-mode", "auto",
	}, launch.Command)
	require.Equal(t, "❯", launch.ReadyMarker)
}

func TestRepositoryBuildPromptRunLaunchSpec_RefusesAnUnknownPermissionMode(t *testing.T) {
	repo, _ := newTestRepository(t, subprocess.NewMockRunner(t))

	for _, mode := range []core.TaskPermissionMode{"", "yolo"} {
		_, err := repo.BuildPromptRunLaunchSpec(&core.Task{}, core.PromptRunLaunch{PermissionMode: mode})

		require.ErrorContains(t, err, "unknown permission mode", string(mode))
	}
}

// transcriptLine is one line of a Claude Code transcript.
func transcriptLine(kind string, content string, extra string) string {
	return fmt.Sprintf(`{"type":%q,%s"message":{"role":%q,"content":%s}}`, kind, extra, kind, content)
}

func writeSessionTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}

// sessionNamed is a provider session rig knows by its ID alone.
func sessionNamed(id string) core.TaskProviderSession {
	return core.TaskProviderSession{ProviderSessionID: id}
}

// interruptFixture is a repository whose Claude configuration lives in a
// temporary directory, and the task that runs with it in /work/repo.
func interruptFixture(t *testing.T) (*repository, *core.Task, string) {
	t.Helper()
	repo, _ := newTestRepository(t, subprocess.NewMockRunner(t))
	configDir := t.TempDir()
	task := &core.Task{
		WorktreePath: "/work/repo",
		ProviderEnv:  core.ProviderEnv{"CLAUDE_CONFIG_DIR": configDir},
	}
	projectDir := filepath.Join(configDir, "projects", "-work-repo")
	return repo, task, projectDir
}

func TestRepositoryLastTurnInterrupted_ReadsTheNewestUserEntryOfTheTranscript(t *testing.T) {
	prompt := transcriptLine("user", `"fix the bug"`, "")
	assistant := transcriptLine("assistant", `[{"type":"text","text":"on it"}]`, "")
	toolResult := transcriptLine("user", `[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]`, "")
	interrupted := transcriptLine("user", `[{"type":"text","text":"[Request interrupted by user]"}]`, "")
	interruptedForTool := transcriptLine("user",
		`[{"type":"text","text":"[Request interrupted by user for tool use]"}]`, "")
	interruptedAsString := transcriptLine("user", `"[Request interrupted by user]"`, "")
	bookkeeping := `{"type":"last-prompt","lastPrompt":"fix the bug"}`

	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"interrupted", []string{prompt, assistant, interrupted}, true},
		{"interrupted while a tool ran", []string{prompt, assistant, toolResult, interruptedForTool}, true},
		{"interrupted, as plain text", []string{prompt, assistant, interruptedAsString}, true},
		{"interrupted, then bookkeeping entries", []string{prompt, interrupted, bookkeeping}, true},
		{"interrupted, then a half-written line", []string{prompt, interrupted, `{"type":"assis`}, true},
		{"turn ended normally", []string{prompt, assistant}, false},
		{"mid-turn, after a tool result", []string{prompt, assistant, toolResult, assistant}, false},
		{"prompted again after an interruption", []string{prompt, interrupted, assistant, prompt}, false},
		{"a subagent was interrupted", []string{prompt, assistant,
			transcriptLine(
				"user",
				`[{"type":"text","text":"[Request interrupted by user]"}]`,
				`"isSidechain":true,`,
			)}, false},
		{"injected context after an interruption", []string{prompt, interrupted,
			transcriptLine("user", `"<system-reminder>x</system-reminder>"`, `"isMeta":true,`)}, true},
		{"the marker quoted inside a prompt", []string{
			transcriptLine("user", `"why does it print [Request interrupted by user]?"`, "")}, false},
		{"no user entry at all", []string{assistant, bookkeeping}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, task, projectDir := interruptFixture(t)
			writeSessionTranscript(t, filepath.Join(projectDir, "sess-1.jsonl"), tt.lines...)

			got, err := repo.LastTurnInterrupted(t.Context(), task, sessionNamed("sess-1"))

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestRepositoryLastTurnInterrupted_EmptyOrMissingTranscriptIsNotInterrupted(t *testing.T) {
	repo, task, projectDir := interruptFixture(t)
	require.NoError(t, os.MkdirAll(projectDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "empty.jsonl"), nil, 0o600))

	got, err := repo.LastTurnInterrupted(t.Context(), task, sessionNamed("empty"))
	require.NoError(t, err)
	require.False(t, got, "an empty transcript")

	got, err = repo.LastTurnInterrupted(t.Context(), task, sessionNamed("never-written"))
	require.NoError(t, err)
	require.False(t, got, "a session without a transcript")
}

func TestRepositoryLastTurnInterrupted_OnlyReadsTheTailOfALargeTranscript(t *testing.T) {
	repo, task, projectDir := interruptFixture(t)
	require.NoError(t, os.MkdirAll(projectDir, 0o700))
	marker := transcriptLine("user", `[{"type":"text","text":"[Request interrupted by user]"}]`, "")
	// Each line is 100 KB, so thirty of them are far more than the window
	// that is read from the end.
	filler := transcriptLine("assistant", `[{"type":"text","text":"`+strings.Repeat("x", 100_000)+`"}]`, "")
	fillers := make([]string, 30)
	for i := range fillers {
		fillers[i] = filler
	}

	// The marker is the last user entry of the whole transcript, but sits in
	// the part that is not read: a reader that went through the whole file
	// would call the turn interrupted.
	writeSessionTranscript(t, filepath.Join(projectDir, "head.jsonl"), append([]string{marker}, fillers...)...)
	got, err := repo.LastTurnInterrupted(t.Context(), task, sessionNamed("head"))
	require.NoError(t, err)
	require.False(t, got, "only the tail of the transcript is read")

	// At the end, the same marker is seen.
	writeSessionTranscript(t, filepath.Join(projectDir, "tail.jsonl"), slices.Concat(fillers, []string{marker})...)
	got, err = repo.LastTurnInterrupted(t.Context(), task, sessionNamed("tail"))
	require.NoError(t, err)
	require.True(t, got)

	// And a later prompt, in the tail, outweighs an earlier marker.
	writeSessionTranscript(t, filepath.Join(projectDir, "later.jsonl"),
		append(append([]string{marker}, fillers...), transcriptLine("user", `"carry on"`, ""))...)
	got, err = repo.LastTurnInterrupted(t.Context(), task, sessionNamed("later"))
	require.NoError(t, err)
	require.False(t, got)
}

func TestRepositoryLastTurnInterrupted_FindsASessionFiledUnderAnotherProjectFolder(t *testing.T) {
	repo, task, _ := interruptFixture(t)
	configDir := task.ProviderEnv["CLAUDE_CONFIG_DIR"]
	writeSessionTranscript(t, filepath.Join(configDir, "projects", "-work-repo-subdir", "moved.jsonl"),
		transcriptLine("user", `[{"type":"text","text":"[Request interrupted by user]"}]`, ""))

	got, err := repo.LastTurnInterrupted(t.Context(), task, sessionNamed("moved"))

	require.NoError(t, err)
	require.True(t, got)
}

func TestRepositoryLastTurnInterrupted_PrefersTheTasksOwnProjectFolder(t *testing.T) {
	repo, task, projectDir := interruptFixture(t)
	configDir := task.ProviderEnv["CLAUDE_CONFIG_DIR"]
	writeSessionTranscript(t, filepath.Join(projectDir, "dup.jsonl"), transcriptLine("user", `"carry on"`, ""))
	writeSessionTranscript(t, filepath.Join(configDir, "projects", "-aaa-other", "dup.jsonl"),
		transcriptLine("user", `"[Request interrupted by user]"`, ""))

	got, err := repo.LastTurnInterrupted(t.Context(), task, sessionNamed("dup"))

	require.NoError(t, err)
	require.False(t, got)
}

func TestRepositoryLastTurnInterrupted_RefusesASessionIDThatIsAPath(t *testing.T) {
	repo, task, _ := interruptFixture(t)

	for _, id := range []string{"", "../escape", "a/b"} {
		_, err := repo.LastTurnInterrupted(t.Context(), task, sessionNamed(id))

		require.ErrorContains(t, err, "invalid session ID", id)
	}
}

func TestRepositoryLastTurnInterrupted_ReadsTheTranscriptRigRecordedForTheSession(t *testing.T) {
	repo, task, _ := interruptFixture(t)
	// The session was filed under a Claude configuration the task no longer
	// runs with, where neither the task's folder nor a scan would look.
	recorded := filepath.Join(t.TempDir(), "other-config", "projects", "-elsewhere", "sess-9.jsonl")
	writeSessionTranscript(t, recorded,
		transcriptLine("user", `[{"type":"text","text":"[Request interrupted by user]"}]`, ""))

	got, err := repo.LastTurnInterrupted(t.Context(), task, core.TaskProviderSession{
		ProviderSessionID: "sess-9",
		TranscriptPath:    recorded,
	})

	require.NoError(t, err)
	require.True(t, got)
}

func TestRepositoryLastTurnInterrupted_TheRecordedTranscriptWinsOverAGuessedOne(t *testing.T) {
	repo, task, projectDir := interruptFixture(t)
	writeSessionTranscript(t, filepath.Join(projectDir, "sess-9.jsonl"),
		transcriptLine("user", `[{"type":"text","text":"[Request interrupted by user]"}]`, ""))
	recorded := filepath.Join(t.TempDir(), "sess-9.jsonl")
	writeSessionTranscript(t, recorded, transcriptLine("user", `"carry on"`, ""))

	got, err := repo.LastTurnInterrupted(t.Context(), task, core.TaskProviderSession{
		ProviderSessionID: "sess-9",
		TranscriptPath:    recorded,
	})

	require.NoError(t, err)
	require.False(t, got)
}

func TestRepositoryLastTurnInterrupted_LooksForTheSessionWhenItsRecordedTranscriptIsGone(t *testing.T) {
	repo, task, projectDir := interruptFixture(t)
	writeSessionTranscript(t, filepath.Join(projectDir, "sess-9.jsonl"),
		transcriptLine("user", `[{"type":"text","text":"[Request interrupted by user]"}]`, ""))

	got, err := repo.LastTurnInterrupted(t.Context(), task, core.TaskProviderSession{
		ProviderSessionID: "sess-9",
		TranscriptPath:    filepath.Join(t.TempDir(), "gone.jsonl"),
	})

	require.NoError(t, err)
	require.True(t, got)
}
