package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// scheduledCreationFixture is a harness that creates a Scheduled task as a
// folder task, on a clock that reads 08:00.
func scheduledCreationFixture(t *testing.T) (*testTaskServiceHarness, *fakeClock) {
	t.Helper()

	svc := newTestTaskService(t)
	svc.providerRepo.suggestedName = "issue triage"
	clock := &fakeClock{now: scheduleStart}
	svc.service.schedules.now = clock.Now
	return svc, clock
}

func triageSchedule() *ScheduleInput {
	return &ScheduleInput{
		Cron:           "daily 09:00",
		Prompt:         "triage the open issues",
		SessionMode:    PromptRunSessionSame,
		PermissionMode: TaskPermissionModeAcceptEdits,
	}
}

func createScheduledTask(t *testing.T, svc *testTaskServiceHarness, schedule *ScheduleInput) (*Task, error) {
	t.Helper()

	return svc.service.CreateTaskWithProgress(t.Context(), CreateTaskInput{
		Cwd:       "/src/api",
		Prompt:    "triage the open issues",
		Workspace: WorkspaceKindFolder,
		Schedule:  schedule,
	}, nil)
}

func (s *testTaskServiceHarness) storedSchedule(taskID string) (ScheduledTask, bool) {
	s.taskRepo.mu.Lock()
	defer s.taskRepo.mu.Unlock()
	schedule, ok := s.taskRepo.schedules[taskID]
	return schedule, ok
}

// --- creation

func TestTaskServiceCreateTask_ScheduledTaskIsKeptWithItsScheduleAndAnIdleSession(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)

	task, err := createScheduledTask(t, svc, triageSchedule())

	require.NoError(t, err)
	require.Equal(t, TaskKindScheduled, task.Kind)
	require.True(t, task.IsScheduled())
	require.Equal(t, TaskCreationStatusReady, svc.taskRepo.updatedTask.CreationStatus)
	require.Equal(t, TaskKindScheduled, svc.taskRepo.createdTask.Kind, "the kind is stored with the task")
	require.Empty(t, task.Prompt, "the prompt lives in the schedule, which an edit changes")

	schedule, ok := svc.storedSchedule(task.ID)
	require.True(t, ok, "the schedule is stored for the task")
	require.Equal(t, ScheduledTask{
		TaskID: task.ID,
		ScheduleInput: ScheduleInput{
			Cron: "daily 09:00", Prompt: "triage the open issues",
			SessionMode: PromptRunSessionSame, PermissionMode: TaskPermissionModeAcceptEdits,
		},
		Enabled:   true,
		NextRunAt: scheduleNine,
		CreatedAt: scheduleStart,
		UpdatedAt: scheduleStart,
	}, schedule)

	require.NotNil(t, svc.sessionClient.startedTask, "the tmux session is created")
	require.Equal(t, TaskSessionLaunchSpec{}, svc.sessionClient.startedLaunch, "with a shell and no provider in it")
	require.Zero(t, svc.sessionClient.prefillCalls, "and no prompt typed into it")
	require.True(t, svc.workspace.bootstrapCalled, "provider hooks are registered for the runs to come")
}

func TestTaskServiceCreateTask_ScheduledTaskInARepositoryGetsAWorktree(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)
	schedule := triageSchedule()
	schedule.Prompt = "check the nightly build"

	task, err := svc.service.CreateTaskWithProgress(t.Context(), CreateTaskInput{
		Cwd:      "/src/api",
		Prompt:   "the composer prompt",
		Schedule: schedule,
	}, nil)

	require.NoError(t, err)
	require.False(t, task.UsesFolderWorkspace())
	require.NotNil(t, svc.repoClient.createdTask, "the worktree is created")
	require.Equal(t, TaskKindScheduled, svc.repoClient.createdTask.Kind)
	require.Equal(t, TaskKindScheduled, svc.taskRepo.createdTask.Kind)
	require.Empty(t, task.Prompt)
	require.Equal(t, TaskSessionLaunchSpec{}, svc.sessionClient.startedLaunch)
	require.Equal(t, []string{"check the nightly build"}, svc.providerRepo.suggestPrompts)
	stored, ok := svc.storedSchedule(task.ID)
	require.True(t, ok)
	require.Equal(t, "check the nightly build", stored.Prompt)
	require.Equal(t, scheduleNine, stored.NextRunAt)
}

func TestTaskServiceCreateTask_ScheduledTaskInARepositoryTakesTheComposerPromptWhenTheScheduleHasNone(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)
	schedule := triageSchedule()
	schedule.Prompt = ""

	task, err := svc.service.CreateTaskWithProgress(t.Context(), CreateTaskInput{
		Cwd: "/src/api", Prompt: "the composer prompt", Schedule: schedule,
	}, nil)

	require.NoError(t, err)
	stored, _ := svc.storedSchedule(task.ID)
	require.Equal(t, "the composer prompt", stored.Prompt)
}

func TestTaskServiceCreateTask_RemovesAWorktreeTasksRecordWhenItsScheduleCannotBeStored(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)
	svc.taskRepo.scheduleErr = errors.New("database is locked")

	_, err := svc.service.CreateTaskWithProgress(t.Context(), CreateTaskInput{
		Cwd: "/src/api", Prompt: "triage", Schedule: triageSchedule(),
	}, nil)

	require.ErrorContains(t, err, "store the schedule")
	require.Len(t, svc.taskRepo.deletedTaskIDs, 1)
	require.Nil(t, svc.repoClient.createdTask, "no worktree is made for it")
}

func TestTaskServiceCreateTask_ScheduledTaskIsNamedAfterAndRunsTheSchedulesPrompt(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)
	schedule := triageSchedule()
	schedule.Prompt = "check the nightly build"

	task, err := createScheduledTask(t, svc, schedule)

	require.NoError(t, err)
	require.Equal(t, []string{"check the nightly build"}, svc.providerRepo.suggestPrompts)
	stored, _ := svc.storedSchedule(task.ID)
	require.Equal(t, "check the nightly build", stored.Prompt)
}

func TestTaskServiceCreateTask_ScheduledTaskTakesTheComposerPromptWhenTheScheduleHasNone(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)
	schedule := triageSchedule()
	schedule.Prompt = "  "

	task, err := createScheduledTask(t, svc, schedule)

	require.NoError(t, err)
	require.Equal(t, []string{"triage the open issues"}, svc.providerRepo.suggestPrompts)
	stored, _ := svc.storedSchedule(task.ID)
	require.Equal(t, "triage the open issues", stored.Prompt)
}

func TestTaskServiceCreateTask_ScheduledTaskKeepsTheScheduleWithoutItsPadding(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)

	task, err := createScheduledTask(t, svc, &ScheduleInput{Cron: "  daily 09:00 \n", Prompt: "triage"})

	require.NoError(t, err)
	stored, _ := svc.storedSchedule(task.ID)
	require.Equal(t, "daily 09:00", stored.Cron)
	require.Equal(t, scheduleNine, stored.NextRunAt)
}

func TestTaskServiceCreateTask_AskingTheCalculatorToValidateIsNotLeftToItsNext(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)

	_, err := createScheduledTask(t, svc, &ScheduleInput{Cron: "zoned", Prompt: "triage"})

	require.ErrorContains(t, err, `schedule "zoned": time zones are not supported`)
	require.Nil(t, svc.taskRepo.createdTask)
}

func TestTaskServiceCreateTask_RefusesAScheduleWithNoNextTimeThatValidated(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)

	_, err := createScheduledTask(t, svc, &ScheduleInput{Cron: "late", Prompt: "triage"})

	require.ErrorContains(t, err, `schedule "late": it never fires`)
	require.Nil(t, svc.taskRepo.createdTask)
}

func TestTaskServiceCreateTask_ScheduledTaskDefaultsItsModes(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)

	task, err := createScheduledTask(t, svc, &ScheduleInput{Cron: "daily 09:00", Prompt: "triage"})

	require.NoError(t, err)
	stored, _ := svc.storedSchedule(task.ID)
	require.Equal(t, PromptRunSessionNew, stored.SessionMode)
	require.Equal(t, TaskPermissionModeAuto, stored.PermissionMode)
}

func TestTaskServiceCreateTask_RefusesABadScheduleBeforeCreatingAnything(t *testing.T) {
	tests := map[string]struct {
		schedule ScheduleInput
		want     string
	}{
		"no schedule": {ScheduleInput{Prompt: "p"}, "schedule is required"},
		"not a cron":  {ScheduleInput{Cron: "bad", Prompt: "p"}, `schedule "bad": not a cron schedule`},
		"never fires": {ScheduleInput{Cron: "never", Prompt: "p"}, `schedule "never": it never fires`},
		"too often":   {ScheduleInput{Cron: "every 1m", Prompt: "p"}, "runs must be at least 5m0s apart"},
		"no prompt":   {ScheduleInput{Cron: "daily 09:00"}, "prompt is required"},
		"unknown session": {
			ScheduleInput{Cron: "daily 09:00", Prompt: "p", SessionMode: "forever"},
			"unknown session mode",
		},
		"unknown permission": {
			ScheduleInput{Cron: "daily 09:00", Prompt: "p", PermissionMode: "yolo"},
			"unknown permission mode",
		},
		"prompt too long": {
			ScheduleInput{Cron: "daily 09:00", Prompt: strings.Repeat("x", maxPromptRunPromptBytes+1)},
			"prompt is too long",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			svc, _ := scheduledCreationFixture(t)
			// The composer prompt must not stand in for a missing schedule prompt
			// here: the case is the schedule's own.
			input := CreateTaskInput{Cwd: "/src/api", Workspace: WorkspaceKindFolder, Schedule: &tt.schedule}

			_, err := svc.service.CreateTaskWithProgress(t.Context(), input, nil)

			require.ErrorContains(t, err, tt.want)
			require.Nil(t, svc.taskRepo.createdTask, "no task is created")
			require.Empty(t, svc.providerRepo.suggestPrompts, "no provider is asked to name it")
			require.Empty(t, svc.taskRepo.schedules)
			require.Nil(t, svc.sessionClient.startedTask)
		})
	}
}

func TestTaskServiceCreateTask_RefusesAPermissionModeTheProviderCannotHonour(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)
	svc.providerRepo.promptRunErr = errors.New(`permission mode "accept_edits" is not supported by codex`)

	_, err := createScheduledTask(t, svc, triageSchedule())

	require.ErrorContains(t, err, `permission mode "accept_edits" is not supported by codex`)
	require.Nil(t, svc.taskRepo.createdTask)
	require.Empty(t, svc.providerRepo.suggestPrompts)
	require.Equal(
		t,
		[]PromptRunLaunch{{PermissionMode: TaskPermissionModeAcceptEdits}},
		svc.providerRepo.promptRunLaunches,
	)
}

func TestTaskServiceCreateTask_RefusesAScheduledTaskFromAPullRequest(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)

	_, err := svc.service.CreateTaskWithProgress(t.Context(), CreateTaskInput{
		Cwd:      "/src/api",
		Source:   CreateTaskSource{PullRequest: &RepoPullRequest{Number: 7, BranchName: "retries"}},
		Schedule: triageSchedule(),
	}, nil)

	require.ErrorContains(t, err, "cannot be created from a pull request")
	require.Nil(t, svc.taskRepo.createdTask)
}

func TestTaskServiceCreateTask_RefusesAScheduleWhenSchedulingIsNotConfigured(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)
	svc.service.schedules.calculator = nil

	_, err := createScheduledTask(t, svc, triageSchedule())

	require.ErrorIs(t, err, errSchedulingNotConfigured)
	require.Nil(t, svc.taskRepo.createdTask)
}

func TestTaskServiceCreateTask_RemovesTheTaskWhenItsScheduleCannotBeStored(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)
	svc.taskRepo.scheduleErr = errors.New("database is locked")

	_, err := createScheduledTask(t, svc, triageSchedule())

	require.ErrorContains(t, err, "store the schedule: database is locked")
	require.Len(t, svc.taskRepo.deletedTaskIDs, 1, "no scheduled task is left without a schedule")
	require.Equal(t, svc.taskRepo.createdTask.ID, svc.taskRepo.deletedTaskIDs[0])
	require.Nil(t, svc.sessionClient.startedTask, "and no session is made for it")
}

func TestTaskServiceCreateTask_OrdinaryTaskHasNoScheduleAndTypesItsPrompt(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)
	svc.providerRepo.suggestedName = "retries"

	task, err := svc.service.CreateTaskWithProgress(t.Context(), CreateTaskInput{
		Cwd: "/src/api", Prompt: "add retries", Workspace: WorkspaceKindFolder,
	}, nil)

	require.NoError(t, err)
	require.Empty(t, task.Kind)
	require.False(t, task.IsScheduled())
	require.Equal(t, "add retries", task.Prompt)
	require.Empty(t, svc.taskRepo.schedules)
	require.NotEmpty(t, svc.sessionClient.startedLaunch.Command, "an ordinary task starts its provider")
	require.Empty(t, svc.service.schedules.pokes, "and does not wake the scheduler")
}

func TestTaskServiceCreateTask_WakesTheSchedulerWhenAScheduledTaskIsReady(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)

	_, err := createScheduledTask(t, svc, triageSchedule())

	require.NoError(t, err)
	require.Len(t, svc.service.schedules.pokes, 1)
}

func TestTaskServiceRetryTaskCreation_RestartsAScheduledTasksIdleSession(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)
	task := readyTaskFixture()
	task.Kind = TaskKindScheduled
	task.Prompt = ""
	task.WorkspaceKind = WorkspaceKindFolder
	task.CreationStatus = TaskCreationStatusFailed
	task.CreationStep = TaskCreateProgressStartingSession
	task.CreationError = "start task session: tmux is not running"
	svc.taskRepo.listTasks = []*Task{task}
	// A retried ordinary task would find its provider running and only type its
	// prompt; a Scheduled task has no provider to find and no prompt to type.
	svc.sessionClient.inspectState = TaskSessionRuntimeState{Exists: true, ActiveCommands: []string{"codex"}}

	retried, err := svc.service.RetryTaskCreationWithProgress(t.Context(), task.ID, nil)

	require.NoError(t, err)
	require.Equal(t, TaskCreationStatusReady, retried.CreationStatus)
	require.NotNil(t, svc.sessionClient.startedTask)
	require.Equal(t, TaskSessionLaunchSpec{}, svc.sessionClient.startedLaunch)
	require.Zero(t, svc.sessionClient.prefillCalls)
}

// --- what a scheduled task refuses

func TestTaskServiceShelveTask_RefusesAScheduledTask(t *testing.T) {
	svc := newTestTaskService(t)
	task := readyTaskFixture()
	task.Kind = TaskKindScheduled
	svc.taskRepo.listTasks = []*Task{task}

	err := svc.service.ShelveTask(t.Context(), "task-1")

	require.ErrorIs(t, err, ErrTaskScheduled)
	require.ErrorContains(t, err, "cannot be shelved")
	require.Nil(t, svc.sessionClient.deletedTask, "its session is kept")
	require.Empty(t, svc.taskRepo.updatedTasks)
}

func TestTaskServiceNewTaskSession_RefusesAScheduledTask(t *testing.T) {
	for _, newWindow := range []bool{false, true} {
		svc := newTestTaskService(t)
		task := readyTaskFixture()
		task.Kind = TaskKindScheduled
		svc.taskRepo.listTasks = []*Task{task}
		svc.sessionClient.inspectState = idlePane()

		_, err := svc.service.NewTaskSessionWithProgress(t.Context(), NewTaskSessionInput{
			TaskID: "task-1", Prompt: "keep going", NewWindow: newWindow,
		}, nil)

		require.ErrorIs(t, err, ErrTaskScheduled, "new window %v", newWindow)
		require.Nil(t, svc.sessionClient.startedTask)
		require.Nil(t, svc.taskRepo.createdTask, "no child task is made")
	}
}

func TestTaskServiceShelveTask_StillShelvesAnOrdinaryTask(t *testing.T) {
	svc := newTestTaskService(t)
	svc.taskRepo.listTasks = []*Task{readyTaskFixture()}

	require.NoError(t, svc.service.ShelveTask(t.Context(), "task-1"))
	require.NotNil(t, svc.sessionClient.deletedTask)
}

// --- listing, editing, pausing

func TestTaskServiceListTaskSchedules_ReturnsEverySchedule(t *testing.T) {
	f := newScheduleFixture(t)
	other := ScheduledTask{TaskID: "task-2", ScheduleInput: ScheduleInput{Cron: "@daily", Prompt: "p"}}
	f.putSchedule(other)

	schedules, err := f.service.ListTaskSchedules(t.Context())

	require.NoError(t, err)
	require.Equal(t, []ScheduledTask{f.schedule(), other}, schedules)
}

func TestTaskServiceUpdateTaskSchedule_ReplacesTheEditableFieldsAndWorksOutTheNextRunFromNow(t *testing.T) {
	f := newScheduleFixture(t)
	f.edit(func(s *ScheduledTask) { s.NextRunAt = scheduleNine.AddDate(0, 0, 3) })
	read := f.schedule()
	f.clock.Set(scheduleStart.Add(2*time.Hour + 3*time.Minute)) // 10:03

	updated, err := f.service.UpdateTaskSchedule(t.Context(), "task-1", ScheduleInput{
		Cron: "every 10m", Prompt: "check the deploy", SessionMode: PromptRunSessionNew,
		PermissionMode: TaskPermissionModeBypass,
	}, read.UpdatedAt)

	require.NoError(t, err)
	want := read
	want.Cron, want.Prompt = "every 10m", "check the deploy"
	want.SessionMode, want.PermissionMode = PromptRunSessionNew, TaskPermissionModeBypass
	want.NextRunAt = scheduleDay.Add(10*time.Hour + 10*time.Minute)
	want.UpdatedAt = f.clock.Now()
	require.Equal(t, want, *updated)
	require.Equal(t, want, f.schedule())
	require.Len(t, f.service.schedules.pokes, 1, "the scheduler looks again at once")
}

func TestTaskServiceUpdateTaskSchedule_LeavesARunInFlightAlone(t *testing.T) {
	f := newScheduleFixture(t)
	f.edit(func(s *ScheduledTask) {
		s.RunState = ScheduledRunStateRunning
		s.RunStartedAt = scheduleStart
		s.LastRunAt = scheduleStart
		s.LastOutcome = ScheduledRunRunning
		s.LastDetail = "sent to the running session"
	})
	read := f.schedule()

	updated, err := f.service.UpdateTaskSchedule(t.Context(), "task-1", ScheduleInput{
		Cron: "daily 10:00", Prompt: "new prompt",
	}, read.UpdatedAt)

	require.NoError(t, err)
	require.True(t, updated.IsRunning())
	require.Equal(t, read.RunStartedAt, updated.RunStartedAt)
	require.Equal(t, read.LastOutcome, updated.LastOutcome)
	require.Equal(t, read.LastDetail, updated.LastDetail)
	require.Equal(t, read.LastRunAt, updated.LastRunAt)
}

func TestTaskServiceUpdateTaskSchedule_KeepsAPausedScheduleAndAnEmptyPromptStaysRefused(t *testing.T) {
	f := newScheduleFixture(t)
	f.edit(func(s *ScheduledTask) { s.Enabled = false })
	read := f.schedule()

	_, err := f.service.UpdateTaskSchedule(t.Context(), "task-1", ScheduleInput{Cron: "daily 10:00"}, read.UpdatedAt)
	require.ErrorContains(t, err, "prompt is required")

	updated, err := f.service.UpdateTaskSchedule(
		t.Context(), "task-1", ScheduleInput{Cron: "daily 10:00", Prompt: "p"}, read.UpdatedAt,
	)
	require.NoError(t, err)
	require.False(t, updated.Enabled, "an edit does not resume a paused schedule")
}

func TestTaskServiceUpdateTaskSchedule_RefusesAnEditMadeFromAnOldVersion(t *testing.T) {
	f := newScheduleFixture(t)
	read := f.schedule()
	_, err := f.service.UpdateTaskSchedule(
		t.Context(), "task-1", ScheduleInput{Cron: "daily 10:00", Prompt: "first edit"}, read.UpdatedAt,
	)
	require.NoError(t, err)
	afterFirst := f.schedule()

	_, err = f.service.UpdateTaskSchedule(
		t.Context(), "task-1", ScheduleInput{Cron: "daily 11:00", Prompt: "second edit"}, read.UpdatedAt,
	)

	require.ErrorIs(t, err, ErrScheduleConflict)
	require.Equal(t, afterFirst, f.schedule(), "the first edit is not overwritten")
	require.NotEqual(t, read.UpdatedAt, afterFirst.UpdatedAt, "an edit is a new version even on a standing clock")
}

func TestTaskServiceUpdateTaskSchedule_TwoEditsInTheSameInstantAreTwoVersions(t *testing.T) {
	f := newScheduleFixture(t)
	v1 := f.schedule()
	first, err := f.service.UpdateTaskSchedule(
		t.Context(), "task-1", ScheduleInput{Cron: "daily 10:00", Prompt: "first"}, v1.UpdatedAt,
	)
	require.NoError(t, err)

	// The clock has not moved since the first edit.
	second, err := f.service.UpdateTaskSchedule(
		t.Context(), "task-1", ScheduleInput{Cron: "daily 11:00", Prompt: "second"}, first.UpdatedAt,
	)
	require.NoError(t, err)
	require.True(t, second.UpdatedAt.After(first.UpdatedAt), "so the second edit is stamped just after the first")

	_, err = f.service.UpdateTaskSchedule(
		t.Context(), "task-1", ScheduleInput{Cron: "daily 12:00", Prompt: "third"}, first.UpdatedAt,
	)
	require.ErrorIs(t, err, ErrScheduleConflict, "an editor holding the first version cannot overwrite the second")
	require.Equal(t, "second", f.schedule().Prompt)
}

func TestTaskServiceUpdateTaskSchedule_StampsAnEditNoEarlierThanTheVersionItReplaces(t *testing.T) {
	f := newScheduleFixture(t)
	f.clock.Set(scheduleStart.Add(-48 * time.Hour)) // a clock set back
	v1 := f.schedule()

	updated, err := f.service.UpdateTaskSchedule(
		t.Context(), "task-1", ScheduleInput{Cron: "daily 10:00", Prompt: "p"}, v1.UpdatedAt,
	)

	require.NoError(t, err)
	require.True(t, updated.UpdatedAt.After(v1.UpdatedAt))
}

func TestTaskServiceUpdateTaskSchedule_RefusesWhatTheScheduleWouldNotBeCreatedWith(t *testing.T) {
	tests := map[string]struct {
		update ScheduleInput
		setup  func(*scheduleFixture)
		want   string
	}{
		"not a cron":  {ScheduleInput{Cron: "bad", Prompt: "p"}, nil, `schedule "bad"`},
		"too often":   {ScheduleInput{Cron: "every 2m", Prompt: "p"}, nil, "runs must be at least 5m0s apart"},
		"no cron":     {ScheduleInput{Prompt: "p"}, nil, "schedule is required"},
		"bad session": {ScheduleInput{Cron: "@daily", Prompt: "p", SessionMode: "x"}, nil, "unknown session mode"},
		"refused by the provider": {
			ScheduleInput{Cron: "daily 10:00", Prompt: "p", PermissionMode: TaskPermissionModeAcceptEdits},
			func(f *scheduleFixture) {
				f.providerRepo.promptRunErr = errors.New("permission mode is not supported by codex")
			},
			"not supported by codex",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := newScheduleFixture(t)
			if tt.setup != nil {
				tt.setup(f)
			}
			before := f.schedule()

			_, err := f.service.UpdateTaskSchedule(t.Context(), "task-1", tt.update, before.UpdatedAt)

			require.ErrorContains(t, err, tt.want)
			require.Equal(t, before, f.schedule())
		})
	}
}

func TestTaskServiceUpdateTaskSchedule_NeedsTheTasksProviderToBeConfigured(t *testing.T) {
	f := newScheduleFixture(t)
	f.providerConfig.setup = &ProviderSetup{Configured: []Provider{ProviderClaude}, Default: ProviderClaude}
	before := f.schedule()

	_, err := f.service.UpdateTaskSchedule(
		t.Context(), "task-1", ScheduleInput{Cron: "daily 10:00", Prompt: "p"}, before.UpdatedAt,
	)

	require.ErrorContains(t, err, `provider "codex" is not configured`)
	require.Equal(t, before, f.schedule())
}

func TestTaskServiceUpdateTaskSchedule_NeedsAScheduledTask(t *testing.T) {
	f := newScheduleFixture(t)
	update := ScheduleInput{Cron: "daily 10:00", Prompt: "p"}

	_, err := f.service.UpdateTaskSchedule(t.Context(), "task-404", update, time.Now())
	require.ErrorIs(t, err, ErrTaskNotFound)

	f.taskRepo.mu.Lock()
	delete(f.taskRepo.schedules, "task-1")
	f.taskRepo.mu.Unlock()
	_, err = f.service.UpdateTaskSchedule(t.Context(), "task-1", update, time.Now())
	require.ErrorIs(t, err, ErrScheduleNotFound)
}

func TestTaskServiceSetTaskScheduleEnabled_PausesAndResumesWithoutMakingUpMissedRuns(t *testing.T) {
	f := newScheduleFixture(t)

	paused, err := f.service.SetTaskScheduleEnabled(t.Context(), "task-1", false)
	require.NoError(t, err)
	require.False(t, paused.Enabled)
	require.Equal(t, scheduleNine, paused.NextRunAt, "pausing leaves the next run as it was")
	require.True(t, paused.UpdatedAt.After(scheduleStart.Add(-time.Hour)), "pausing is a new version")

	// Days pass while it is paused, and many runs would have been due.
	back := scheduleNine.AddDate(0, 0, 5).Add(time.Hour)
	f.clock.Set(back)
	resumed, err := f.service.SetTaskScheduleEnabled(t.Context(), "task-1", true)

	require.NoError(t, err)
	require.True(t, resumed.Enabled)
	require.Equal(t, scheduleNine.AddDate(0, 0, 6), resumed.NextRunAt, "the next run is the first after now")
	require.Len(t, f.service.schedules.pokes, 1)
	f.tick(back.Add(time.Minute))
	require.Empty(t, f.runner.runs(), "nothing runs for the days it was paused")
}

func TestTaskServiceSetTaskScheduleEnabled_ChangesNothingWhenItIsAlreadySo(t *testing.T) {
	f := newScheduleFixture(t)
	before := f.schedule()

	schedule, err := f.service.SetTaskScheduleEnabled(t.Context(), "task-1", true)

	require.NoError(t, err)
	require.Equal(t, before, *schedule)
	require.Equal(t, before, f.schedule(), "no new version")
	require.Empty(t, f.service.schedules.pokes)
}

func TestTaskServiceSetTaskScheduleEnabled_KeepsTheRecordOfTheLastRun(t *testing.T) {
	f := newScheduleFixture(t)
	f.edit(func(s *ScheduledTask) {
		s.RunState = ScheduledRunStateRunning
		s.LastOutcome = ScheduledRunRunning
		s.LastRunAt = scheduleStart
	})

	paused, err := f.service.SetTaskScheduleEnabled(t.Context(), "task-1", false)

	require.NoError(t, err)
	require.True(t, paused.IsRunning())
	require.Equal(t, ScheduledRunRunning, paused.LastOutcome)
	require.Equal(t, scheduleStart, paused.LastRunAt)
}

func TestTaskServiceSetTaskScheduleEnabled_NeedsAScheduledTask(t *testing.T) {
	f := newScheduleFixture(t)

	_, err := f.service.SetTaskScheduleEnabled(t.Context(), "task-404", false)
	require.ErrorIs(t, err, ErrTaskNotFound)
}

// --- the minimum interval

func TestTaskSchedules_RefusesASchedulesRunsCloserThanTheMinimumInterval(t *testing.T) {
	tests := map[string]struct {
		cron    string
		minimum time.Duration
		wantErr string
	}{
		"exactly the minimum":         {"every 5m", 0, ""},
		"just under the minimum":      {"every 4m59s", 0, "runs again after 4m59s"},
		"a day apart":                 {"daily 09:00", 0, ""},
		"a short gap after the first": {"minutes 0,10,12", 0, "runs again after 2m0s"},
		"a short gap further on":      {"minutes 0,10,20,22", 0, "runs again after 2m0s"},
		"close at the very start":     {"minutes 0,3", 0, "runs again after 3m0s"},
		"far enough apart":            {"minutes 0,10,20", 0, ""},
		"a sandbox allows a minute":   {"every 1m", time.Minute, ""},
		"but not less":                {"every 30s", time.Minute, "runs must be at least 1m0s apart"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := newScheduleFixture(t)
			if tt.minimum != 0 {
				f.service.schedules.minInterval = tt.minimum
			}
			read := f.schedule()

			_, err := f.service.UpdateTaskSchedule(
				t.Context(), "task-1", ScheduleInput{Cron: tt.cron, Prompt: "p"}, read.UpdatedAt,
			)

			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestNewTaskService_DefaultsTheSchedulerPacing(t *testing.T) {
	svc := newTestTaskService(t)

	require.Equal(t, 5*time.Minute, svc.service.schedules.minInterval)
	require.Equal(t, 30*time.Second, svc.service.schedules.tick)
}

// --- resolving a run from hook events

// scheduledHookFixture is a scheduled task whose run is in flight.
func scheduledHookFixture(t *testing.T) *scheduleFixture {
	t.Helper()

	f := newScheduleFixture(t)
	f.edit(func(s *ScheduledTask) {
		s.RunState = ScheduledRunStateRunning
		s.RunStartedAt = scheduleStart
		s.LastRunAt = scheduleStart
		s.LastOutcome = ScheduledRunRunning
		s.LastDetail = "sent to the running session"
	})
	return f
}

// deliver hands the service a hook event that drives the given status, or none.
func (f *scheduleFixture) deliver(update *TaskStatusUpdate, input HookEventInput) error {
	if update != nil {
		update.TaskID, update.Provider = "task-1", ProviderCodex
	}
	f.providerRepo.hookUpdate = update
	input.TaskID, input.Provider = "task-1", ProviderCodex
	if input.OccurredAt.IsZero() {
		input.OccurredAt = time.Now()
	}
	return f.service.HandleHookEvent(context.Background(), input)
}

func TestHandleHookEvent_EndsTheRunInFlightByWhatTheHookSays(t *testing.T) {
	tests := []struct {
		name        string
		update      *TaskStatusUpdate
		errorText   string
		wantOutcome ScheduledRunOutcome
		wantDetail  string
		wantRunning bool
	}{
		{
			name:        "stop completes it and keeps how it began",
			update:      &TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop},
			wantOutcome: ScheduledRunCompleted,
			wantDetail:  "sent to the running session",
		},
		{
			name:        "stop failure fails it with the error the event gave",
			update:      &TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStopFailure},
			errorText:   "API Error: Rate limit reached",
			wantOutcome: ScheduledRunFailed,
			wantDetail:  "API Error: Rate limit reached",
		},
		{
			name:        "stop failure without an error text",
			update:      &TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStopFailure},
			wantOutcome: ScheduledRunFailed,
			wantDetail:  "the agent stopped on an error",
		},
		{
			name:        "stop failure with a blank error text",
			update:      &TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStopFailure},
			errorText:   " \n ",
			wantOutcome: ScheduledRunFailed,
			wantDetail:  "the agent stopped on an error",
		},
		{
			name:        "an error text is put on one line",
			update:      &TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStopFailure},
			errorText:   "API Error:\n  overloaded,\n\ttry again",
			wantOutcome: ScheduledRunFailed,
			wantDetail:  "API Error: overloaded, try again",
		},
		{
			name:        "session end fails it as the session ended",
			update:      &TaskStatusUpdate{Phase: TaskStatusPhaseStopped, RawEventName: HookEventSessionEnd},
			wantOutcome: ScheduledRunFailed,
			wantDetail:  "the session ended",
		},
		{
			name:        "a session end fails it whatever status it drove",
			update:      &TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventSessionEnd},
			wantOutcome: ScheduledRunFailed,
			wantDetail:  "the session ended",
		},
		{
			name:        "a stopped status fails it whichever event made it",
			update:      &TaskStatusUpdate{Phase: TaskStatusPhaseStopped, RawEventName: "TaskSessionStopped"},
			wantOutcome: ScheduledRunFailed,
			wantDetail:  "the session ended",
		},
		{
			name: "a stop that leaves subagents to wake the agent is not the end",
			update: &TaskStatusUpdate{
				Phase: TaskStatusPhaseWorkingInBackground, RawEventName: HookEventStop,
				BackgroundWork: TaskBackgroundWork{Subagents: 1},
			},
			wantOutcome: ScheduledRunRunning,
			wantDetail:  "sent to the running session",
			wantRunning: true,
		},
		{
			name:        "the agent working on is not the end",
			update:      &TaskStatusUpdate{Phase: TaskStatusPhaseWorking, RawEventName: HookEventPreToolUse},
			wantOutcome: ScheduledRunRunning,
			wantDetail:  "sent to the running session",
			wantRunning: true,
		},
		{
			name:        "a permission prompt is not the end",
			update:      &TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventNotification},
			wantOutcome: ScheduledRunRunning,
			wantDetail:  "sent to the running session",
			wantRunning: true,
		},
		{
			name:        "a session start is not the end",
			update:      &TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventSessionStart},
			wantOutcome: ScheduledRunRunning,
			wantDetail:  "sent to the running session",
			wantRunning: true,
		},
		{
			name:        "an event that drives no status is not the end",
			update:      nil,
			wantOutcome: ScheduledRunRunning,
			wantDetail:  "sent to the running session",
			wantRunning: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := scheduledHookFixture(t)
			eventName := HookEventStop
			if tt.update != nil {
				eventName = tt.update.RawEventName
			}

			err := f.deliver(tt.update, HookEventInput{EventName: eventName, ErrorText: tt.errorText})

			require.NoError(t, err)
			stored := f.schedule()
			require.Equal(t, tt.wantOutcome, stored.LastOutcome)
			require.Equal(t, tt.wantDetail, stored.LastDetail)
			require.Equal(t, tt.wantRunning, stored.IsRunning())
			require.Equal(t, scheduleStart, stored.LastRunAt, "it is still the run that began then")
		})
	}
}

func TestHandleHookEvent_CapsALongErrorText(t *testing.T) {
	f := scheduledHookFixture(t)

	err := f.deliver(
		&TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStopFailure},
		HookEventInput{EventName: HookEventStopFailure, ErrorText: strings.Repeat("é", 1000)},
	)

	require.NoError(t, err)
	detail := f.schedule().LastDetail
	require.Equal(t, strings.Repeat("é", maxRunDetailRunes-1)+"…", detail)
}

func TestHandleHookEvent_LeavesAFinishedRunFinished(t *testing.T) {
	f := scheduledHookFixture(t)
	stop := &TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop}
	require.NoError(t, f.deliver(stop, HookEventInput{EventName: HookEventStop}))
	require.Equal(t, ScheduledRunCompleted, f.schedule().LastOutcome)

	// The user leaves the session later; no run is in flight to fail.
	end := &TaskStatusUpdate{Phase: TaskStatusPhaseStopped, RawEventName: HookEventSessionEnd}
	require.NoError(t, f.deliver(end, HookEventInput{EventName: HookEventSessionEnd}))

	require.Equal(t, ScheduledRunCompleted, f.schedule().LastOutcome)
	require.Equal(t, "sent to the running session", f.schedule().LastDetail)
}

func TestHandleHookEvent_IgnoresAHookFromAProviderThatIsNotTheTasks(t *testing.T) {
	f := scheduledHookFixture(t)
	f.providerConfig.setup = &ProviderSetup{
		Configured: []Provider{ProviderCodex, ProviderClaude}, Default: ProviderCodex,
	}
	f.claudeRepo.hookUpdate = &TaskStatusUpdate{
		TaskID: "task-1", Provider: ProviderClaude, Phase: TaskStatusPhaseStopped, RawEventName: HookEventSessionEnd,
	}

	err := f.service.HandleHookEvent(t.Context(), HookEventInput{
		TaskID: "task-1", Provider: ProviderClaude, EventName: HookEventSessionEnd, OccurredAt: time.Now(),
	})

	require.NoError(t, err)
	require.True(t, f.schedule().IsRunning(), "a late hook of the provider before cannot end the run")
}

func TestHandleHookEvent_AnOrdinaryTaskNeedsNoRunToEnd(t *testing.T) {
	svc := newTestTaskService(t)
	svc.taskRepo.listTasks = []*Task{readyTaskFixture()}
	svc.providerRepo.hookUpdate = &TaskStatusUpdate{
		TaskID: "task-1", Provider: ProviderCodex, Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop,
	}

	err := svc.service.HandleHookEvent(t.Context(), HookEventInput{
		TaskID: "task-1", Provider: ProviderCodex, EventName: HookEventStop, OccurredAt: time.Now(),
	})

	require.NoError(t, err)
	require.Empty(t, svc.taskRepo.schedules)
}

func TestHandleHookEvent_ReportsARunItCouldNotEnd(t *testing.T) {
	f := scheduledHookFixture(t)
	f.service.schedules.tasks = failingResolveRepository{TaskRepository: f.service.schedules.tasks}

	err := f.deliver(
		&TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop},
		HookEventInput{EventName: HookEventStop},
	)

	require.NoError(t, err, "the hook itself was handled")
	require.Len(t, f.reported(), 1)
	require.ErrorContains(t, f.reported()[0], "resolve the scheduled run of task task-1: database is locked")
}

// failingResolveRepository is a task repository that cannot end a run.
type failingResolveRepository struct{ TaskRepository }

func (failingResolveRepository) ResolveScheduledRun(
	context.Context, string, ScheduledRunOutcome, string,
) (bool, error) {
	return false, errors.New("database is locked")
}

func TestNewTaskService_TakesTheSchedulerPacingAndErrorReporterFromItsDependencies(t *testing.T) {
	svc := newTestTaskService(t)
	var reported []error

	configured := NewTaskService(TaskServiceDependencies{
		Tasks:               svc.taskRepoMock,
		ProviderConfig:      svc.providerConfigMock,
		ScheduleCalculator:  fakeCalculator{},
		ScheduleMinInterval: 2 * time.Minute,
		SchedulerTick:       7 * time.Second,
		ReportError:         func(err error) { reported = append(reported, err) },
	})

	require.Equal(t, 2*time.Minute, configured.schedules.minInterval)
	require.Equal(t, 7*time.Second, configured.schedules.tick)
	configured.schedules.report(errors.New("boom"))
	require.Len(t, reported, 1)
	require.NotNil(t, configured.schedules.calculator)
	require.NotNil(t, configured.schedules.run, "scheduled runs are the service's prompt runs")
}

func TestScheduler_ReportsSchedulesItCannotList(t *testing.T) {
	f := newScheduleFixture(t)
	f.service.schedules.tasks = failingListRepository{TaskRepository: f.service.schedules.tasks}

	f.tick(scheduleNine)

	require.Empty(t, f.runner.runs())
	require.Len(t, f.reported(), 1)
	require.ErrorContains(t, f.reported()[0], "list schedules: database is locked")
}

// failingListRepository is a task repository that cannot list schedules.
type failingListRepository struct{ TaskRepository }

func (failingListRepository) ListScheduledTasks(context.Context) ([]ScheduledTask, error) {
	return nil, errors.New("database is locked")
}

func TestTask_IsScheduledOnlyForAScheduledKind(t *testing.T) {
	require.True(t, (&Task{Kind: TaskKindScheduled}).IsScheduled())
	require.False(t, (&Task{}).IsScheduled())
	require.False(t, (&Task{Kind: "other"}).IsScheduled(), "a kind this Rig does not know is an ordinary task")
	require.False(t, (*Task)(nil).IsScheduled())
}

func TestHandleHookEvent_AnErrorTextOfExactlyTheCapIsKeptWhole(t *testing.T) {
	f := scheduledHookFixture(t)
	text := strings.Repeat("é", maxRunDetailRunes)

	err := f.deliver(
		&TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStopFailure},
		HookEventInput{EventName: HookEventStopFailure, ErrorText: text},
	)

	require.NoError(t, err)
	require.Equal(t, text, f.schedule().LastDetail)
}

func TestHandleHookEvent_ARunIsNotEndedByAStatusThatWasNotStored(t *testing.T) {
	f := scheduledHookFixture(t)
	f.taskRepo.upsertStatusErr = errors.New("database is locked")

	err := f.deliver(
		&TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop},
		HookEventInput{EventName: HookEventStop},
	)

	require.ErrorContains(t, err, "database is locked")
	require.True(t, f.schedule().IsRunning())
}

func TestTaskServiceCreateTask_ScheduledTaskRecordFailureIsReturned(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)
	svc.taskRepo.createErr = errors.New("database is locked")

	_, err := createScheduledTask(t, svc, triageSchedule())

	require.ErrorContains(t, err, "database is locked")
	require.Empty(t, svc.taskRepo.schedules, "no schedule is stored for a task that was not")
	require.Nil(t, svc.sessionClient.startedTask)
}

func TestTaskServiceCreateTask_ScheduledTaskSessionFailureFailsTheCreationAtThatStep(t *testing.T) {
	svc, _ := scheduledCreationFixture(t)
	svc.sessionClient.startErr = errors.New("tmux is not running")

	task, err := createScheduledTask(t, svc, triageSchedule())

	require.ErrorContains(t, err, "start task session: tmux is not running")
	require.NotNil(t, task)
	require.Equal(t, TaskCreationStatusFailed, svc.taskRepo.updatedTask.CreationStatus)
	require.Equal(t, TaskCreateProgressStartingSession, svc.taskRepo.updatedTask.CreationStep)
	_, stored := svc.storedSchedule(task.ID)
	require.True(t, stored, "the schedule stays for the retry")
}
