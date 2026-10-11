package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BaronBonet/rig/internal/core"

	"github.com/stretchr/testify/require"
)

var scheduleNow = time.Date(2026, time.October, 12, 8, 0, 0, 123456789, time.UTC)

// createScheduledTask stores a Scheduled task and its schedule, due at 09:00.
func createScheduledTask(t *testing.T, repo *repository, id string) core.ScheduledTask {
	t.Helper()

	ctx := context.Background()
	require.NoError(t, repo.CreateTask(ctx, &core.Task{
		ID: id, Slug: id, DisplayName: id, RepoRoot: "/src/code", RepoName: "code", WorktreePath: "/src/code",
		TmuxSession: "code_" + id, Provider: core.ProviderClaude, Kind: core.TaskKindScheduled,
		WorkspaceKind: core.WorkspaceKindFolder, CreatedAt: scheduleNow, UpdatedAt: scheduleNow,
	}))
	schedule := core.ScheduledTask{
		TaskID: id,
		ScheduleInput: core.ScheduleInput{
			Cron:           "0 9 * * 1-5",
			Prompt:         "triage the open issues\nthen \"summarise\"\n",
			SessionMode:    core.PromptRunSessionSame,
			PermissionMode: core.TaskPermissionModeAcceptEdits,
		},
		Enabled:   true,
		NextRunAt: scheduleNow.Add(time.Hour),
		CreatedAt: scheduleNow,
		UpdatedAt: scheduleNow,
	}
	require.NoError(t, repo.CreateScheduledTask(ctx, schedule))
	return schedule
}

func getSchedule(t *testing.T, repo *repository, id string) core.ScheduledTask {
	t.Helper()

	schedule, err := repo.GetScheduledTask(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, schedule, "no schedule for %s", id)
	return *schedule
}

func TestRepositoryScheduledTasks_RoundTripEveryField(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()
	stored := createScheduledTask(t, repo, "task-1")

	got := getSchedule(t, repo, "task-1")
	require.Equal(t, stored, got)

	// A schedule carrying a run in flight and the record of the last run.
	require.NoError(t, repo.CreateTask(ctx, &core.Task{
		ID: "task-2", Slug: "task-2", RepoName: "code", Provider: core.ProviderCodex, CreatedAt: scheduleNow,
		UpdatedAt: scheduleNow,
	}))
	busy := core.ScheduledTask{
		TaskID:        "task-2",
		ScheduleInput: core.ScheduleInput{Cron: "@every 2h", Prompt: "p", SessionMode: core.PromptRunSessionNew},
		Enabled:       false,
		NextRunAt:     scheduleNow.Add(2 * time.Hour),
		RunState:      core.ScheduledRunStateRunning,
		RunStartedAt:  scheduleNow.Add(-2 * time.Minute),
		LastRunAt:     scheduleNow.Add(-time.Minute),
		LastOutcome:   core.ScheduledRunRunning,
		LastDetail:    "resumed 3f2a…",
		CreatedAt:     scheduleNow.Add(-time.Hour),
		UpdatedAt:     scheduleNow.Add(-30 * time.Minute),
	}
	require.NoError(t, repo.CreateScheduledTask(ctx, busy))
	require.Equal(t, busy, getSchedule(t, repo, "task-2"), "a paused schedule and its run record survive")

	schedules, err := repo.ListScheduledTasks(ctx)
	require.NoError(t, err)
	require.Equal(t, []core.ScheduledTask{busy, stored}, schedules, "listed oldest first")
}

func TestRepositoryScheduledTasks_GetIsNilForATaskWithoutOne(t *testing.T) {
	repo := newTestRepository(t)

	schedule, err := repo.GetScheduledTask(context.Background(), "task-404")

	require.NoError(t, err)
	require.Nil(t, schedule)
	schedules, err := repo.ListScheduledTasks(context.Background())
	require.NoError(t, err)
	require.Empty(t, schedules)
}

func TestRepositoryScheduledTasks_NeedAnExistingTaskAndTaskID(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()

	err := repo.CreateScheduledTask(ctx, core.ScheduledTask{
		TaskID: "no-such-task", ScheduleInput: core.ScheduleInput{Cron: "@daily", Prompt: "p"},
	})
	require.Error(t, err, "a schedule cannot outlive or precede its task")

	err = repo.CreateScheduledTask(ctx, core.ScheduledTask{TaskID: " "})
	require.ErrorContains(t, err, "task ID is required")

	createScheduledTask(t, repo, "task-1")
	err = repo.CreateScheduledTask(ctx, core.ScheduledTask{TaskID: "task-1"})
	require.Error(t, err, "a task has one schedule")
}

func TestRepositoryTasks_PersistTheirKind(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()
	createScheduledTask(t, repo, "task-scheduled")
	require.NoError(t, repo.CreateTask(ctx, &core.Task{
		ID: "task-plain", Slug: "plain", RepoName: "code", Provider: core.ProviderClaude, CreatedAt: scheduleNow,
		UpdatedAt: scheduleNow,
	}))

	kinds := func() map[string]core.TaskKind {
		tasks, err := repo.ListTasks(ctx)
		require.NoError(t, err)
		byID := map[string]core.TaskKind{}
		for _, task := range tasks {
			byID[task.ID] = task.Kind
		}
		return byID
	}
	require.Equal(t, map[string]core.TaskKind{"task-scheduled": core.TaskKindScheduled, "task-plain": ""}, kinds())

	// An update of the record, a rename say, keeps the kind.
	tasks, err := repo.ListTasks(ctx)
	require.NoError(t, err)
	for _, task := range tasks {
		task.DisplayName = "renamed " + task.ID
		require.NoError(t, repo.UpdateTask(ctx, task))
	}
	require.Equal(t, map[string]core.TaskKind{"task-scheduled": core.TaskKindScheduled, "task-plain": ""}, kinds())
}

func TestRepositoryScheduledTasks_DeleteTaskDeletesItsSchedule(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()
	createScheduledTask(t, repo, "task-1")
	createScheduledTask(t, repo, "task-2")

	require.NoError(t, repo.DeleteTask(ctx, "task-1"))

	schedule, err := repo.GetScheduledTask(ctx, "task-1")
	require.NoError(t, err)
	require.Nil(t, schedule)
	schedules, err := repo.ListScheduledTasks(ctx)
	require.NoError(t, err)
	require.Len(t, schedules, 1)
	require.Equal(t, "task-2", schedules[0].TaskID, "another task's schedule is kept")
}

func TestRepositoryScheduledTasks_UpdateReplacesTheEditableFieldsOnly(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()
	stored := createScheduledTask(t, repo, "task-1")
	require.NoError(t, repo.StartScheduledRun(ctx, "task-1", scheduleNow.Add(time.Minute), "started fresh"))
	running := getSchedule(t, repo, "task-1")

	edit := stored
	edit.Cron, edit.Prompt = "@hourly", "check the deploy"
	edit.SessionMode, edit.PermissionMode = core.PromptRunSessionNew, core.TaskPermissionModeBypass
	edit.Enabled = false
	edit.NextRunAt = scheduleNow.Add(3 * time.Hour)
	edit.UpdatedAt = scheduleNow.Add(time.Second)
	// What the editor read the run record as is not written back.
	edit.RunState, edit.LastOutcome, edit.LastDetail = "", core.ScheduledRunSkipped, "stale"
	require.NoError(t, repo.UpdateScheduledTask(ctx, edit, stored.UpdatedAt))

	got := getSchedule(t, repo, "task-1")
	want := running
	want.ScheduleInput = edit.ScheduleInput
	want.Enabled = false
	want.NextRunAt = edit.NextRunAt
	want.UpdatedAt = edit.UpdatedAt
	require.Equal(t, want, got)
	require.Equal(t, core.ScheduledRunStateRunning, got.RunState, "a run in flight is not disturbed by an edit")
	require.Equal(t, "started fresh", got.LastDetail)
	require.True(t, got.CreatedAt.Equal(stored.CreatedAt))

	// And back again: the modes are written, not left as a default.
	back := edit
	back.SessionMode, back.PermissionMode = core.PromptRunSessionSame, core.TaskPermissionModeDefault
	back.Enabled = true
	back.UpdatedAt = scheduleNow.Add(2 * time.Second)
	require.NoError(t, repo.UpdateScheduledTask(ctx, back, edit.UpdatedAt))
	got = getSchedule(t, repo, "task-1")
	require.Equal(t, core.PromptRunSessionSame, got.SessionMode)
	require.Equal(t, core.TaskPermissionModeDefault, got.PermissionMode)
	require.True(t, got.Enabled)
}

func TestRepositoryScheduledTasks_UpdateFromAnOldVersionIsAConflict(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()
	stored := createScheduledTask(t, repo, "task-1")

	first := stored
	first.Prompt = "first edit"
	first.UpdatedAt = scheduleNow.Add(time.Second)
	require.NoError(t, repo.UpdateScheduledTask(ctx, first, stored.UpdatedAt))

	second := stored
	second.Prompt = "second edit"
	second.UpdatedAt = scheduleNow.Add(2 * time.Second)
	err := repo.UpdateScheduledTask(ctx, second, stored.UpdatedAt)

	require.ErrorIs(t, err, core.ErrScheduleConflict)
	require.Equal(t, "first edit", getSchedule(t, repo, "task-1").Prompt, "the first edit is not overwritten")

	// Down to the nanosecond: a version a hair off is another version.
	err = repo.UpdateScheduledTask(ctx, second, first.UpdatedAt.Add(time.Nanosecond))
	require.ErrorIs(t, err, core.ErrScheduleConflict)
	require.NoError(t, repo.UpdateScheduledTask(ctx, second, first.UpdatedAt))
	require.Equal(t, "second edit", getSchedule(t, repo, "task-1").Prompt)
}

func TestRepositoryScheduledTasks_UpdateOfAMissingScheduleIsNotAConflict(t *testing.T) {
	repo := newTestRepository(t)

	err := repo.UpdateScheduledTask(context.Background(), core.ScheduledTask{TaskID: "task-404"}, scheduleNow)

	require.ErrorIs(t, err, core.ErrScheduleNotFound)
	require.NotErrorIs(t, err, core.ErrScheduleConflict)
}

func TestRepositoryScheduledTasks_ClaimMovesTheNextRunOnceForWhoeverAsksFirst(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()
	stored := createScheduledTask(t, repo, "task-1")
	next := stored.NextRunAt.Add(24 * time.Hour)

	won, err := repo.ClaimScheduledRun(ctx, "task-1", stored.NextRunAt, next)
	require.NoError(t, err)
	require.True(t, won)
	require.True(t, getSchedule(t, repo, "task-1").NextRunAt.Equal(next))

	lost, err := repo.ClaimScheduledRun(ctx, "task-1", stored.NextRunAt, next.Add(24*time.Hour))
	require.NoError(t, err)
	require.False(t, lost, "the second caller claiming the same run loses")
	require.True(t, getSchedule(t, repo, "task-1").NextRunAt.Equal(next), "and changes nothing")
}

func TestRepositoryScheduledTasks_ClaimsRaceToASingleWinner(t *testing.T) {
	repo := newTestRepository(t)
	stored := createScheduledTask(t, repo, "task-1")

	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, err := repo.ClaimScheduledRun(
				context.Background(), "task-1", stored.NextRunAt, stored.NextRunAt.Add(time.Duration(i+1)*time.Hour),
			)
			if err == nil && won {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()

	require.EqualValues(t, 1, wins.Load())
}

func TestRepositoryScheduledTasks_ClaimIgnoresAPausedScheduleAndAWrongTime(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()
	stored := createScheduledTask(t, repo, "task-1")

	won, err := repo.ClaimScheduledRun(
		ctx,
		"task-1",
		stored.NextRunAt.Add(time.Nanosecond),
		stored.NextRunAt.Add(time.Hour),
	)
	require.NoError(t, err)
	require.False(t, won, "the due time has to be the stored one exactly")

	paused := stored
	paused.Enabled = false
	paused.UpdatedAt = scheduleNow.Add(time.Second)
	require.NoError(t, repo.UpdateScheduledTask(ctx, paused, stored.UpdatedAt))
	won, err = repo.ClaimScheduledRun(ctx, "task-1", stored.NextRunAt, stored.NextRunAt.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, won, "a paused schedule cannot be claimed")

	won, err = repo.ClaimScheduledRun(ctx, "task-404", stored.NextRunAt, stored.NextRunAt.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, won)
	require.True(t, getSchedule(t, repo, "task-1").NextRunAt.Equal(stored.NextRunAt))
}

func TestRepositoryScheduledTasks_ClaimWorksForAScheduleWithNoNextRunYet(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()
	stored := createScheduledTask(t, repo, "task-1")
	stored.NextRunAt = time.Time{}
	stored.UpdatedAt = scheduleNow.Add(time.Second)
	require.NoError(t, repo.UpdateScheduledTask(ctx, stored, scheduleNow))

	won, err := repo.ClaimScheduledRun(ctx, "task-1", time.Time{}, scheduleNow.Add(time.Hour))

	require.NoError(t, err)
	require.True(t, won)
}

func TestRepositoryScheduledTasks_ARunIsRecordedAsStartedSkippedFailedAndResolved(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()
	createScheduledTask(t, repo, "task-1")
	at := scheduleNow.Add(time.Hour)

	require.NoError(t, repo.StartScheduledRun(ctx, "task-1", at, "resumed 3f2a…"))
	got := getSchedule(t, repo, "task-1")
	require.Equal(t, core.ScheduledRunStateRunning, got.RunState)
	require.Equal(t, core.ScheduledRunRunning, got.LastOutcome)
	require.Equal(t, "resumed 3f2a…", got.LastDetail)
	require.True(t, got.RunStartedAt.Equal(at))
	require.True(t, got.LastRunAt.Equal(at))

	resolved, err := repo.ResolveScheduledRun(ctx, "task-1", core.ScheduledRunCompleted, "")
	require.NoError(t, err)
	require.True(t, resolved)
	got = getSchedule(t, repo, "task-1")
	require.Equal(t, core.ScheduledRunStateIdle, got.RunState)
	require.Equal(t, core.ScheduledRunCompleted, got.LastOutcome)
	require.Equal(t, "resumed 3f2a…", got.LastDetail, "an empty detail keeps how the run began")
	require.True(t, got.LastRunAt.Equal(at), "and the run is still the one that began then")

	later := at.Add(24 * time.Hour)
	require.NoError(t, repo.SkipScheduledRun(ctx, "task-1", later, "session busy"))
	got = getSchedule(t, repo, "task-1")
	require.Equal(t, core.ScheduledRunSkipped, got.LastOutcome)
	require.Equal(t, "session busy", got.LastDetail)
	require.True(t, got.LastRunAt.Equal(later))

	require.NoError(t, repo.FailScheduledRun(ctx, "task-1", later.Add(time.Minute), "start task session: tmux is gone"))
	got = getSchedule(t, repo, "task-1")
	require.Equal(t, core.ScheduledRunFailed, got.LastOutcome)
	require.Equal(t, "start task session: tmux is gone", got.LastDetail)
	require.True(t, got.LastRunAt.Equal(later.Add(time.Minute)))
}

func TestRepositoryScheduledTasks_ResolveNeedsARunInFlightAndSetsItsDetail(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()
	createScheduledTask(t, repo, "task-1")

	resolved, err := repo.ResolveScheduledRun(ctx, "task-1", core.ScheduledRunFailed, "the session ended")
	require.NoError(t, err)
	require.False(t, resolved, "nothing to end")
	require.Equal(t, core.ScheduledRunNone, getSchedule(t, repo, "task-1").LastOutcome)

	require.NoError(t, repo.StartScheduledRun(ctx, "task-1", scheduleNow, "started fresh"))
	resolved, err = repo.ResolveScheduledRun(ctx, "task-1", core.ScheduledRunFailed, "the session ended")
	require.NoError(t, err)
	require.True(t, resolved)
	got := getSchedule(t, repo, "task-1")
	require.Equal(t, core.ScheduledRunFailed, got.LastOutcome)
	require.Equal(t, "the session ended", got.LastDetail)

	resolved, err = repo.ResolveScheduledRun(ctx, "task-1", core.ScheduledRunCompleted, "")
	require.NoError(t, err)
	require.False(t, resolved, "a run ends once")
	require.Equal(t, core.ScheduledRunFailed, getSchedule(t, repo, "task-1").LastOutcome)
}

func TestRepositoryScheduledTasks_ASkipLeavesARunInFlightAlone(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()
	createScheduledTask(t, repo, "task-1")
	require.NoError(t, repo.StartScheduledRun(ctx, "task-1", scheduleNow, "sent to the running session"))
	before := getSchedule(t, repo, "task-1")

	require.NoError(t, repo.SkipScheduledRun(ctx, "task-1", scheduleNow.Add(time.Hour), "session busy"))

	require.Equal(t, before, getSchedule(t, repo, "task-1"))
}

func TestRepositoryScheduledTasks_AFailureEndsARunInFlight(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()
	createScheduledTask(t, repo, "task-1")
	require.NoError(t, repo.StartScheduledRun(ctx, "task-1", scheduleNow, "started fresh"))

	require.NoError(
		t,
		repo.FailScheduledRun(ctx, "task-1", scheduleNow.Add(time.Hour), "the session didn't take the prompt"),
	)

	got := getSchedule(t, repo, "task-1")
	require.Equal(t, core.ScheduledRunStateIdle, got.RunState)
	require.Equal(t, core.ScheduledRunFailed, got.LastOutcome)
}

func TestRepositoryScheduledTasks_RunRecordsOfOneScheduleLeaveTheOthersAlone(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()
	createScheduledTask(t, repo, "task-1")
	createScheduledTask(t, repo, "task-2")
	before := getSchedule(t, repo, "task-2")

	require.NoError(t, repo.StartScheduledRun(ctx, "task-1", scheduleNow, "started fresh"))
	require.NoError(t, repo.FailScheduledRun(ctx, "task-1", scheduleNow, "x"))
	require.NoError(t, repo.SkipScheduledRun(ctx, "task-1", scheduleNow, "x"))
	_, err := repo.ResolveScheduledRun(ctx, "task-1", core.ScheduledRunCompleted, "x")
	require.NoError(t, err)
	_, err = repo.ClaimScheduledRun(ctx, "task-1", scheduleNow, scheduleNow)
	require.NoError(t, err)
	edit := before
	edit.TaskID = "task-1"
	edit.UpdatedAt = scheduleNow.Add(time.Second)
	require.NoError(t, repo.UpdateScheduledTask(ctx, edit, scheduleNow))

	require.Equal(t, before, getSchedule(t, repo, "task-2"))
}

func TestRepositoryScheduledTasks_SurviveReopeningTheDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	repo := newTestRepositoryAtPath(t, path)
	stored := createScheduledTask(t, repo, "task-1")
	require.NoError(t, repo.db.Close())

	reopened := newTestRepositoryAtPath(t, path)

	require.Equal(t, stored, getSchedule(t, reopened, "task-1"))
}

func TestHealthCheckRepository_RefusesTheScheduleMethods(t *testing.T) {
	repo := NewHealthCheckRepository(Config{Path: filepath.Join(t.TempDir(), "state.db")})
	ctx := context.Background()

	require.ErrorIs(t, repo.CreateScheduledTask(ctx, core.ScheduledTask{}), errHealthCheckRepositoryOnly)
	_, err := repo.GetScheduledTask(ctx, "x")
	require.ErrorIs(t, err, errHealthCheckRepositoryOnly)
	_, err = repo.ListScheduledTasks(ctx)
	require.ErrorIs(t, err, errHealthCheckRepositoryOnly)
	require.ErrorIs(t, repo.UpdateScheduledTask(ctx, core.ScheduledTask{}, time.Time{}), errHealthCheckRepositoryOnly)
	_, err = repo.ClaimScheduledRun(ctx, "x", time.Time{}, time.Time{})
	require.ErrorIs(t, err, errHealthCheckRepositoryOnly)
	require.ErrorIs(t, repo.StartScheduledRun(ctx, "x", time.Time{}, ""), errHealthCheckRepositoryOnly)
	require.ErrorIs(t, repo.SkipScheduledRun(ctx, "x", time.Time{}, ""), errHealthCheckRepositoryOnly)
	require.ErrorIs(t, repo.FailScheduledRun(ctx, "x", time.Time{}, ""), errHealthCheckRepositoryOnly)
	_, err = repo.ResolveScheduledRun(ctx, "x", core.ScheduledRunCompleted, "")
	require.ErrorIs(t, err, errHealthCheckRepositoryOnly)
}

func TestRepositoryNew_MigratesADatabaseWithTasksToScheduledTasks(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	repo := newTestRepositoryAtPath(t, path)
	require.NoError(t, repo.CreateTask(ctx, &core.Task{
		ID: "task-old", Slug: "old", RepoName: "code", Provider: core.ProviderClaude, CreatedAt: scheduleNow,
		UpdatedAt: scheduleNow,
	}))
	// Rewind to the database before this migration, with its task in it.
	for _, statement := range []string{
		"drop table scheduled_tasks",
		"alter table tasks drop column kind",
		"delete from goose_db_version where version_id = 15",
	} {
		_, err := repo.db.ExecContext(ctx, statement)
		require.NoError(t, err, statement)
	}
	require.NoError(t, repo.db.Close())

	reopened := newTestRepositoryAtPath(t, path)

	tasks, err := reopened.ListTasks(ctx)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, core.TaskKind(""), tasks[0].Kind, "an existing task is an ordinary one")
	require.NoError(t, reopened.CreateScheduledTask(ctx, core.ScheduledTask{
		TaskID: "task-old", ScheduleInput: core.ScheduleInput{Cron: "@daily", Prompt: "p"}, CreatedAt: scheduleNow,
		UpdatedAt: scheduleNow,
	}))
}

func TestRepositoryScheduledTasks_ColumnsDefaultToANewSessionAutoPermissionsEnabledAndNoRun(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()
	createScheduledTask(t, repo, "task-1")
	require.NoError(t, repo.CreateTask(ctx, &core.Task{
		ID: "task-2", Slug: "two", RepoName: "code", Provider: core.ProviderClaude, CreatedAt: scheduleNow,
		UpdatedAt: scheduleNow,
	}))

	_, err := repo.db.ExecContext(ctx, `
		insert into scheduled_tasks (task_id, cron, prompt, created_at, updated_at)
		values ('task-2', '@daily', 'p', ?, ?)`, formatTime(scheduleNow), formatTime(scheduleNow))
	require.NoError(t, err)

	got := getSchedule(t, repo, "task-2")
	require.Equal(t, core.PromptRunSessionNew, got.SessionMode)
	require.Equal(t, core.TaskPermissionModeAuto, got.PermissionMode)
	require.True(t, got.Enabled)
	require.True(t, got.NextRunAt.IsZero())
	require.Equal(t, core.ScheduledRunStateIdle, got.RunState)
	require.Equal(t, core.ScheduledRunNone, got.LastOutcome)
	require.Empty(t, got.LastDetail)
	require.True(t, got.RunStartedAt.IsZero())
	require.True(t, got.LastRunAt.IsZero())
}
