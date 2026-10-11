package claude

import (
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

func TestRepositoryBusyMarkers_AreWhatClaudeShowsOnlyWhileATurnRuns(t *testing.T) {
	repo, _ := newTestRepository(t, subprocess.NewMockRunner(t))

	require.Equal(t, []string{"esc to interrupt"}, repo.BusyMarkers())

	repo.BusyMarkers()[0] = "changed"
	require.Equal(t, []string{"esc to interrupt"}, repo.BusyMarkers(), "the list is the repository's own")
}
