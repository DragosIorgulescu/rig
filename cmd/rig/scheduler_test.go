package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/BaronBonet/rig/internal/adapters/repository/sqlite"
	"github.com/BaronBonet/rig/internal/adapters/repository/userconfig"
	"github.com/BaronBonet/rig/internal/adapters/taskdaemon"
	"github.com/BaronBonet/rig/internal/core"
	"github.com/BaronBonet/rig/internal/infrastructure"
)

// TestServeTaskDaemon_RunsTheSchedulerOnTheConfiguredTick starts the daemon on
// a database whose Scheduled task is due and whose provider is not set up. The
// run cannot start, so the schedule records why; that it does at all shows the
// scheduler runs in the daemon, on the tick the config gives it, and works out
// the next run with the cron calculator.
func TestServeTaskDaemon_RunsTheSchedulerOnTheConfiguredTick(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	socketDir, err := os.MkdirTemp("/tmp", "rigd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })

	cfg := &infrastructure.ApplicationConfig{
		SQLite:     sqlite.Config{Path: filepath.Join(t.TempDir(), "tasks.db")},
		UserConfig: userconfig.Config{Path: filepath.Join(t.TempDir(), "config.json")},
		Daemon: taskdaemon.Config{
			SocketPath:     filepath.Join(socketDir, "d.sock"),
			HookListenAddr: "127.0.0.1:0",
		},
		Scheduler: infrastructure.SchedulerConfig{Tick: 50 * time.Millisecond, MinInterval: time.Minute},
	}

	ctx := context.Background()
	repo, err := sqlite.New(cfg.SQLite)
	require.NoError(t, err)
	created := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, repo.CreateTask(ctx, &core.Task{
		ID: "task-1", Slug: "triage", DisplayName: "triage", RepoRoot: "/src/code", RepoName: "code",
		WorktreePath: "/src/code", TmuxSession: "code_triage", Provider: core.ProviderClaude,
		CreationStatus: core.TaskCreationStatusReady, WorkspaceKind: core.WorkspaceKindFolder,
		Kind: core.TaskKindScheduled, CreatedAt: created, UpdatedAt: created,
	}))
	// Not due when the daemon starts, so only the scheduler's own tick finds it.
	due := time.Now().Add(1500 * time.Millisecond).UTC()
	require.NoError(t, repo.CreateScheduledTask(ctx, core.ScheduledTask{
		TaskID:        "task-1",
		ScheduleInput: core.ScheduleInput{Cron: "@every 1h", Prompt: "triage the open issues"},
		Enabled:       true,
		NextRunAt:     due,
		CreatedAt:     created,
		UpdatedAt:     created,
	}))

	serveCtx, cancel := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- serveTaskDaemon(serveCtx, cfg, cancel) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("the daemon did not stop")
		}
	})

	require.Eventually(t, func() bool {
		schedule, err := repo.GetScheduledTask(ctx, "task-1")
		return err == nil && schedule != nil && schedule.LastOutcome == core.ScheduledRunFailed
	}, 10*time.Second, 20*time.Millisecond, "the daemon makes the due run")

	schedule, err := repo.GetScheduledTask(ctx, "task-1")
	require.NoError(t, err)
	require.Contains(t, schedule.LastDetail, "provider setup required")
	require.True(t, schedule.NextRunAt.After(time.Now().Add(50*time.Minute)),
		"the next run is an hour on, worked out by the cron calculator: %s", schedule.NextRunAt)
}
