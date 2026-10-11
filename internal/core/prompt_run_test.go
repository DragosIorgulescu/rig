package core

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// promptRunFixture is a ready Task whose pane is a bare shell, so that a
// prompt run launches the provider unless a test says otherwise. Its waits are
// short.
func promptRunFixture(t *testing.T) (*testTaskServiceHarness, *Task) {
	t.Helper()

	svc := newTestTaskService(t)
	task := readyTaskFixture()
	svc.taskRepo.listTasks = []*Task{task}
	svc.sessionClient.inspectState = idlePane()
	svc.service.promptRunTiming = promptRunTiming{
		recheckInterval: time.Millisecond,
		takenWait:       40 * time.Millisecond,
		takenPoll:       time.Millisecond,
	}
	// A launch does not wait for a provider that says nothing.
	svc.launcher.providerSessionWait = 0
	return svc, task
}

func runPrompt(t *testing.T, svc *testTaskServiceHarness, input RunTaskPromptInput) RunTaskPromptResult {
	t.Helper()

	if input.TaskID == "" {
		input.TaskID = "task-1"
	}
	if input.Prompt == "" {
		input.Prompt = "triage the open issues"
	}
	result, err := svc.service.RunTaskPrompt(t.Context(), input)
	require.NoError(t, err)
	return result
}

// runningProvider makes the Task's provider run in its pane, with the given
// latest status; a zero status records none.
func runningProvider(svc *testTaskServiceHarness, status TaskStatusUpdate) {
	svc.sessionClient.inspectState = TaskSessionRuntimeState{
		Exists:         true,
		ActiveCommands: []string{"zsh", "codex"},
	}
	if status.Phase == "" {
		return
	}
	status.TaskID = "task-1"
	status.Provider = ProviderCodex
	status.ObservedAt = time.Now().UTC().Add(-time.Minute)
	svc.taskRepo.mu.Lock()
	defer svc.taskRepo.mu.Unlock()
	svc.taskRepo.latestByTask["task-1"] = status
}

// idleProvider is a provider whose turn ended a minute ago.
func idleProvider(svc *testTaskServiceHarness) {
	runningProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop})
}

// reportStatus records a status as if a hook had just reported it.
func reportStatus(svc *testTaskServiceHarness, status TaskStatusUpdate) {
	status.TaskID = "task-1"
	status.Provider = ProviderCodex
	status.ObservedAt = time.Now().UTC().Add(time.Second)
	svc.taskRepo.mu.Lock()
	defer svc.taskRepo.mu.Unlock()
	svc.taskRepo.latestByTask["task-1"] = status
}

// providerTakesPrompts makes the provider report a turn as soon as a prompt is
// sent to it.
func providerTakesPrompts(svc *testTaskServiceHarness) {
	svc.sessionClient.onSubmitPrompt = func(*Task, string) {
		reportStatus(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking, RawEventName: HookEventUserPromptSubmit})
	}
}

func requireNothingDone(t *testing.T, svc *testTaskServiceHarness) {
	t.Helper()

	require.Nil(t, svc.sessionClient.startedTask, "nothing is started")
	require.Empty(t, svc.sessionClient.submittedPrompts, "nothing is sent")
	require.Empty(t, svc.sessionClient.respawnedTasks, "nothing is ended")
	require.Empty(t, svc.promptFiles.writes, "no prompt is written")
}

func requireSkipped(t *testing.T, svc *testTaskServiceHarness, result RunTaskPromptResult, detail string) {
	t.Helper()

	require.Equal(t, RunTaskPromptResult{Outcome: PromptRunSkipped, Detail: detail}, withoutStartTime(t, result))
	requireNothingDone(t, svc)
}

// withoutStartTime checks that a started run says when it started, and
// returns the result without it, for comparing the rest.
func withoutStartTime(t *testing.T, result RunTaskPromptResult) RunTaskPromptResult {
	t.Helper()

	if result.Outcome == PromptRunStarted {
		require.False(t, result.StartedAt.IsZero(), "a started run says when it started")
		require.WithinDuration(t, time.Now(), result.StartedAt, time.Minute)
	} else {
		require.True(t, result.StartedAt.IsZero(), "only a started run has a start time")
	}
	result.StartedAt = time.Time{}
	return result
}

func requireStarted(t *testing.T, svc *testTaskServiceHarness, result RunTaskPromptResult, detail string) {
	t.Helper()

	require.Equal(t, RunTaskPromptResult{Outcome: PromptRunStarted, Detail: detail}, withoutStartTime(t, result))
	require.NotNil(t, svc.sessionClient.startedTask, "a started run launches the provider")
	require.Empty(t, svc.sessionClient.submittedPrompts, "a launch types nothing into the provider")
}

func requireSent(t *testing.T, svc *testTaskServiceHarness, result RunTaskPromptResult, prompt string) {
	t.Helper()

	require.Equal(
		t,
		RunTaskPromptResult{Outcome: PromptRunStarted, Detail: "sent to the running session"},
		withoutStartTime(t, result),
	)
	require.Equal(t, []string{prompt}, svc.sessionClient.submittedPrompts)
	require.Nil(t, svc.sessionClient.startedTask, "the running session is not launched again")
	require.Empty(t, svc.sessionClient.respawnedTasks, "the running session is not ended")
	require.Empty(t, svc.promptFiles.writes, "a sent prompt needs no file")
}

func requireFailed(t *testing.T, result RunTaskPromptResult, detail string) {
	t.Helper()

	require.Equal(t, PromptRunFailed, result.Outcome)
	require.Contains(t, result.Detail, detail)
}

func (s *sessionClientState) paneCheckCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.paneChecks)
}

// --- the Task

func TestTaskServiceRunTaskPrompt_SkipsATaskThatIsNotReady(t *testing.T) {
	for _, status := range []TaskCreationStatus{TaskCreationStatusCreating, TaskCreationStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			svc, task := promptRunFixture(t)
			task.CreationStatus = status

			result := runPrompt(t, svc, RunTaskPromptInput{})

			requireSkipped(t, svc, result, "task is not ready ("+string(status)+")")
		})
	}
}

func TestTaskServiceRunTaskPrompt_SkipsAShelvedTask(t *testing.T) {
	svc, task := promptRunFixture(t)
	task.ShelvedAt = time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireSkipped(t, svc, result, "task is shelved")
}

func TestTaskServiceRunTaskPrompt_SkipsATaskThatDoesNotExist(t *testing.T) {
	svc, _ := promptRunFixture(t)

	result := runPrompt(t, svc, RunTaskPromptInput{TaskID: "task-404"})

	requireSkipped(t, svc, result, "task not found")
}

func TestTaskServiceRunTaskPrompt_SkipsWhileAnotherOperationHoldsTheTask(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)

	var result RunTaskPromptResult
	err := svc.service.operations.Run(
		t.Context(), "task-1", taskOperationRename, false,
		func(ctx context.Context) error {
			var runErr error
			result, runErr = svc.service.RunTaskPrompt(ctx, RunTaskPromptInput{TaskID: "task-1", Prompt: "triage"})
			return runErr
		},
	)

	require.NoError(t, err)
	requireSkipped(t, svc, result, "another operation is running")
	require.Zero(t, svc.sessionClient.paneCheckCount(), "the lock is taken before the window is looked at")
}

func TestTaskServiceRunTaskPrompt_HoldsTheTaskWhileItLaunches(t *testing.T) {
	svc, _ := promptRunFixture(t)
	var duringRun error
	svc.sessionClient.onStart = func(*Task) {
		duringRun = svc.service.operations.Run(
			t.Context(), "task-1", taskOperationRename, false, func(context.Context) error { return nil },
		)
	}

	runPrompt(t, svc, RunTaskPromptInput{})

	require.ErrorIs(t, duringRun, ErrTaskOperationInProgress)
	var inProgress *taskOperationInProgressError
	require.ErrorAs(t, duringRun, &inProgress)
	require.Equal(t, taskOperationPromptRun, inProgress.Active)
	require.Equal(t, "running a prompt", inProgress.Active.progressLabel())
}

func TestTaskServiceRunTaskPrompt_HoldsTheTaskWhileASentPromptIsBeingTaken(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)
	var duringRun error
	svc.sessionClient.onSubmitPrompt = func(*Task, string) {
		duringRun = svc.service.operations.Run(
			t.Context(), "task-1", taskOperationRename, false, func(context.Context) error { return nil },
		)
		reportStatus(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking})
	}

	runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

	require.ErrorIs(t, duringRun, ErrTaskOperationInProgress)
}

func TestTaskServiceRunTaskPrompt_ChecksTheTaskAgainOnceItHoldsTheLock(t *testing.T) {
	tests := []struct {
		name   string
		change func(svc *testTaskServiceHarness, task *Task)
		skip   string
		launch func(t *testing.T, svc *testTaskServiceHarness)
	}{
		{
			name:   "shelved",
			change: func(_ *testTaskServiceHarness, task *Task) { task.ShelvedAt = time.Now() },
			skip:   "task is shelved",
		},
		{
			name:   "deleted",
			change: func(svc *testTaskServiceHarness, _ *Task) { svc.taskRepo.listTasks = nil },
			skip:   "task not found",
		},
		{
			name:   "no longer ready",
			change: func(_ *testTaskServiceHarness, task *Task) { task.CreationStatus = TaskCreationStatusFailed },
			skip:   "task is not ready (failed)",
		},
		{
			name:   "renamed",
			change: func(_ *testTaskServiceHarness, task *Task) { task.TmuxSession = "renamed_session" },
			launch: func(t *testing.T, svc *testTaskServiceHarness) {
				require.Equal(t, "renamed_session", svc.sessionClient.startedTask.TmuxSession,
					"the session the lock found, not the one read before it")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, task := promptRunFixture(t)
			// The first read of the task is the look before the lock; the
			// second is the run's own, with the lock held.
			svc.taskRepo.onListTasks = func(call int) {
				if call == 2 {
					tt.change(svc, task)
				}
			}

			result := runPrompt(t, svc, RunTaskPromptInput{})

			if tt.skip != "" {
				requireSkipped(t, svc, result, tt.skip)
				return
			}
			requireStarted(t, svc, result, "started fresh")
			tt.launch(t, svc)
		})
	}
}

// --- is the session active

func TestTaskServiceRunTaskPrompt_SkipsWhileTheWindowShowsABusyMarker(t *testing.T) {
	tests := []struct {
		name  string
		shows []paneShow
	}{
		{"on the first look", []paneShow{{shows: true}}},
		{"on the second look only", []paneShow{{shows: false}, {shows: true}}},
	}
	for _, mode := range []PromptRunSessionMode{PromptRunSessionNew, PromptRunSessionSame} {
		for _, tt := range tests {
			t.Run(string(mode)+" "+tt.name, func(t *testing.T) {
				svc, _ := promptRunFixture(t)
				idleProvider(svc)
				svc.sessionClient.paneShows = tt.shows

				result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: mode})

				requireSkipped(t, svc, result, "session busy")
				require.Equal(t, [][]string{{"esc to interrupt"}}, svc.sessionClient.paneChecks[:1],
					"the window is searched for the provider's own markers")
			})
		}
	}
}

func TestTaskServiceRunTaskPrompt_AskTheProviderWhichMarkersShowABusyTurn(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)
	svc.providerRepo.busyMarkers = []string{"working…", "esc to stop"}
	svc.sessionClient.paneShows = []paneShow{{shows: true}}

	runPrompt(t, svc, RunTaskPromptInput{})

	require.Equal(t, []string{"working…", "esc to stop"}, svc.sessionClient.paneChecks[0])
}

func TestTaskServiceRunTaskPrompt_LooksAtTheWindowTwiceAnIntervalApart(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)
	const interval = 40 * time.Millisecond
	svc.service.promptRunTiming.recheckInterval = interval

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireStarted(t, svc, result, "started fresh")
	svc.sessionClient.mu.Lock()
	defer svc.sessionClient.mu.Unlock()
	require.Len(t, svc.sessionClient.paneCheckTimes, 2, "a window that looks idle is looked at again, once")
	require.GreaterOrEqual(t, svc.sessionClient.paneCheckTimes[1].Sub(svc.sessionClient.paneCheckTimes[0]), interval)
}

func TestTaskServiceRunTaskPrompt_RunsWhenTheStatusSaysWorkingButTheWindowShowsNothing(t *testing.T) {
	for _, phase := range []TaskStatusPhase{TaskStatusPhaseWorking, TaskStatusPhaseWorkingInBackground} {
		t.Run(string(phase)+" new", func(t *testing.T) {
			svc, _ := promptRunFixture(t)
			runningProvider(svc, TaskStatusUpdate{Phase: phase, RawEventName: HookEventPostToolUse})

			result := runPrompt(t, svc, RunTaskPromptInput{})

			requireStarted(t, svc, result, "started fresh")
			require.Len(t, svc.sessionClient.respawnedTasks, 1, "the stale session is ended")
		})
		t.Run(string(phase)+" same", func(t *testing.T) {
			svc, _ := promptRunFixture(t)
			runningProvider(svc, TaskStatusUpdate{Phase: phase, RawEventName: HookEventPostToolUse})
			providerTakesPrompts(svc)

			result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

			requireSent(t, svc, result, "triage the open issues")
		})
	}
}

func TestTaskServiceRunTaskPrompt_FallsBackToTheHookStatusWhenTheWindowCannotBeRead(t *testing.T) {
	broken := errors.New("tmux capture failed")
	looks := map[string][]paneShow{
		"first look fails":  {{err: broken}},
		"second look fails": {{shows: false}, {err: broken}},
		"every look fails":  {{err: broken}, {err: broken}},
	}
	for name, shows := range looks {
		for _, phase := range []TaskStatusPhase{TaskStatusPhaseWorking, TaskStatusPhaseWorkingInBackground} {
			t.Run(name+" "+string(phase), func(t *testing.T) {
				svc, _ := promptRunFixture(t)
				runningProvider(svc, TaskStatusUpdate{Phase: phase, RawEventName: HookEventPostToolUse})
				svc.sessionClient.paneShows = shows

				result := runPrompt(t, svc, RunTaskPromptInput{})

				requireSkipped(t, svc, result, "session busy")
			})
		}
		t.Run(name+" idle", func(t *testing.T) {
			svc, _ := promptRunFixture(t)
			idleProvider(svc)
			svc.sessionClient.paneShows = shows

			result := runPrompt(t, svc, RunTaskPromptInput{})

			requireStarted(t, svc, result, "started fresh")
		})
	}
}

func TestTaskServiceRunTaskPrompt_ACancelledLookAtTheWindowIsAnErrorNotAGuess(t *testing.T) {
	svc, _ := promptRunFixture(t)
	// The hook status would have the session busy, and so skipped.
	runningProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking, RawEventName: HookEventPostToolUse})
	ctx, cancel := context.WithCancel(t.Context())
	svc.sessionClient.onPaneCheck = cancel
	svc.sessionClient.paneShows = []paneShow{{err: context.Canceled}}

	_, err := svc.service.RunTaskPrompt(ctx, RunTaskPromptInput{TaskID: "task-1", Prompt: "triage"})

	require.ErrorIs(t, err, context.Canceled)
	requireNothingDone(t, svc)
}

func TestTaskServiceRunTaskPrompt_RunsWhenTheWindowCannotBeReadAndNoStatusWasRecorded(t *testing.T) {
	svc, _ := promptRunFixture(t)
	runningProvider(svc, TaskStatusUpdate{})
	svc.sessionClient.paneShows = []paneShow{{err: errors.New("tmux capture failed")}}

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireStarted(t, svc, result, "started fresh")
}

func TestTaskServiceRunTaskPrompt_SkipsAProviderWaitingOnAPermissionPrompt(t *testing.T) {
	for _, event := range []string{HookEventNotification, HookEventPermissionRequest} {
		for _, mode := range []PromptRunSessionMode{PromptRunSessionNew, PromptRunSessionSame} {
			t.Run(event+" "+string(mode), func(t *testing.T) {
				svc, _ := promptRunFixture(t)
				runningProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: event})

				result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: mode})

				requireSkipped(t, svc, result, "waiting on a permission prompt")
				require.Zero(t, svc.sessionClient.paneCheckCount(), "a permission prompt needs no second opinion")
			})
		}
	}
}

func TestTaskServiceRunTaskPrompt_ATurnThatEndedIsNotAPermissionPrompt(t *testing.T) {
	for _, event := range []string{HookEventStop, HookEventStopFailure, HookEventSessionStart} {
		t.Run(event, func(t *testing.T) {
			svc, _ := promptRunFixture(t)
			runningProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: event})

			result := runPrompt(t, svc, RunTaskPromptInput{})

			requireStarted(t, svc, result, "started fresh")
		})
	}
}

func TestTaskServiceRunTaskPrompt_OnlyAWaitingStatusIsAPermissionPrompt(t *testing.T) {
	svc, _ := promptRunFixture(t)
	// A status that kept the name of the event that raised it after the
	// provider moved on.
	runningProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking, RawEventName: HookEventNotification})

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireStarted(t, svc, result, "started fresh")
}

func TestTaskServiceRunTaskPrompt_ARunningProviderWithNoStatusRecordedIsNotActive(t *testing.T) {
	svc, _ := promptRunFixture(t)
	runningProvider(svc, TaskStatusUpdate{})
	providerTakesPrompts(svc)

	result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

	requireSent(t, svc, result, "triage the open issues")
}

func TestTaskServiceRunTaskPrompt_ADeadProvidersLastPermissionPromptDoesNotBlockTheRun(t *testing.T) {
	svc, _ := promptRunFixture(t)
	// The pane is a bare shell: the provider that asked is gone.
	setPromptRunStatus(
		svc,
		TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventPermissionRequest},
	)
	svc.sessionClient.paneShows = []paneShow{{shows: true}}

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireStarted(t, svc, result, "started fresh")
	require.Zero(t, svc.sessionClient.paneCheckCount(), "there is no session to look at")
}

func TestTaskServiceRunTaskPrompt_ReadsTheStatusAfresh(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)
	// Something just looked at the status, so the observer holds a view of it.
	status, err := svc.service.LatestTaskStatus(t.Context(), "task-1")
	require.NoError(t, err)
	require.Equal(t, HookEventStop, status.RawEventName)
	// Then the provider asks for a permission.
	svc.taskRepo.mu.Lock()
	svc.taskRepo.latestByTask["task-1"] = TaskStatusUpdate{
		TaskID:       "task-1",
		Provider:     ProviderCodex,
		Phase:        TaskStatusPhaseWaitingForInput,
		RawEventName: HookEventPermissionRequest,
		ObservedAt:   time.Now().UTC().Add(time.Second),
	}
	svc.taskRepo.mu.Unlock()

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireSkipped(t, svc, result, "waiting on a permission prompt")
}

// --- same

func TestTaskServiceRunTaskPrompt_SameSendsThePromptToTheRunningSession(t *testing.T) {
	svc, task := promptRunFixture(t)
	task.TmuxWindow = "s2"
	idleProvider(svc)
	providerTakesPrompts(svc)
	svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderCodex, SessionID: "sess-1",
	}
	prompt := "triage the issues\n\n- first\n- second\n"

	result := runPrompt(t, svc, RunTaskPromptInput{Prompt: prompt, SessionMode: PromptRunSessionSame})

	requireSent(t, svc, result, prompt)
	require.Equal(t, []string{"submit_task_prompt"}, svc.events, "nothing but the prompt reaches the window")
}

func TestTaskServiceRunTaskPrompt_SameConfirmsThePromptWasTaken(t *testing.T) {
	tests := []struct {
		name  string
		taken func(svc *testTaskServiceHarness)
	}{
		{"working", func(svc *testTaskServiceHarness) {
			reportStatus(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking, RawEventName: HookEventUserPromptSubmit})
		}},
		{"working in the background", func(svc *testTaskServiceHarness) {
			reportStatus(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorkingInBackground, RawEventName: HookEventStop})
		}},
		{"a turn so short only its end was seen", func(svc *testTaskServiceHarness) {
			reportStatus(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop})
		}},
		{"a turn that failed", func(svc *testTaskServiceHarness) {
			reportStatus(
				svc,
				TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStopFailure},
			)
		}},
		{"the window shows the busy marker", func(svc *testTaskServiceHarness) {
			svc.sessionClient.mu.Lock()
			defer svc.sessionClient.mu.Unlock()
			svc.sessionClient.paneShows = []paneShow{{shows: true}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := promptRunFixture(t)
			idleProvider(svc)
			svc.sessionClient.onSubmitPrompt = func(*Task, string) { tt.taken(svc) }

			result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

			requireSent(t, svc, result, "triage the open issues")
		})
	}
}

func TestTaskServiceRunTaskPrompt_SameFailsWhenTheSessionDoesNotTakeThePrompt(t *testing.T) {
	tests := []struct {
		name   string
		report func(svc *testTaskServiceHarness)
	}{
		{"nothing is reported", func(*testTaskServiceHarness) {}},
		{"only the status from before the send", func(svc *testTaskServiceHarness) {
			svc.taskRepo.mu.Lock()
			defer svc.taskRepo.mu.Unlock()
			svc.taskRepo.latestByTask["task-1"] = TaskStatusUpdate{
				TaskID: "task-1", Provider: ProviderCodex, Phase: TaskStatusPhaseWorking,
				RawEventName: HookEventPostToolUse, ObservedAt: time.Now().UTC().Add(-time.Second),
			}
		}},
		{"a session start is not a prompt", func(svc *testTaskServiceHarness) {
			reportStatus(
				svc,
				TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventSessionStart},
			)
		}},
		{"a permission prompt is not the prompt taken", func(svc *testTaskServiceHarness) {
			reportStatus(
				svc,
				TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventNotification},
			)
		}},
		{"the session ended", func(svc *testTaskServiceHarness) {
			reportStatus(svc, TaskStatusUpdate{Phase: TaskStatusPhaseStopped, RawEventName: HookEventSessionEnd})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := promptRunFixture(t)
			idleProvider(svc)
			svc.sessionClient.onSubmitPrompt = func(*Task, string) { tt.report(svc) }

			result, err := svc.service.RunTaskPrompt(t.Context(), RunTaskPromptInput{
				TaskID: "task-1", Prompt: "triage", SessionMode: PromptRunSessionSame,
			})

			require.NoError(t, err, "a prompt that is not taken is a failed run, not an error")
			require.Equal(t, RunTaskPromptResult{
				Outcome: PromptRunFailed, Detail: "the session didn't take the prompt",
			}, result)
			require.Equal(t, []string{"triage"}, svc.sessionClient.submittedPrompts)
		})
	}
}

func TestTaskServiceRunTaskPrompt_SameWaitsForThePromptToBeTaken(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)
	svc.service.promptRunTiming.takenWait = 10 * time.Second
	svc.sessionClient.onSubmitPrompt = func(*Task, string) {
		go func() {
			time.Sleep(30 * time.Millisecond)
			reportStatus(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking, RawEventName: HookEventUserPromptSubmit})
		}()
	}

	started := time.Now()
	result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

	requireSent(t, svc, result, "triage the open issues")
	require.GreaterOrEqual(t, time.Since(started), 30*time.Millisecond, "the run waited for the provider")
}

func TestTaskServiceRunTaskPrompt_SameCountsAStatusReportedWhileThePromptIsStillBeingSent(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)
	// The hook fires as soon as Enter is pressed, which is before the send
	// returns.
	svc.sessionClient.onSubmitPrompt = func(*Task, string) {
		time.Sleep(2 * time.Millisecond)
		svc.taskRepo.mu.Lock()
		defer svc.taskRepo.mu.Unlock()
		svc.taskRepo.latestByTask["task-1"] = TaskStatusUpdate{
			TaskID: "task-1", Provider: ProviderCodex, Phase: TaskStatusPhaseWorking,
			RawEventName: HookEventUserPromptSubmit, ObservedAt: time.Now().UTC(),
		}
		time.Sleep(2 * time.Millisecond)
	}

	result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

	requireSent(t, svc, result, "triage the open issues")
}

func TestTaskServiceRunTaskPrompt_SameWaitsForThePromptAtTheConfiguredPace(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)
	svc.service.promptRunTiming.takenWait = 60 * time.Millisecond
	svc.service.promptRunTiming.takenPoll = 20 * time.Millisecond

	result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

	requireFailed(t, result, "the session didn't take the prompt")
	// Two looks decide the session is idle; the rest are polls of the wait.
	require.LessOrEqual(t, svc.sessionClient.paneCheckCount(), 2+60/20+2, "the wait polls, it does not spin")
	require.GreaterOrEqual(t, svc.sessionClient.paneCheckCount(), 2+2, "the wait polls more than once")
}

func TestPromptRunTiming_DefaultsAreTheDocumentedOnes(t *testing.T) {
	var timing promptRunTiming

	require.Equal(t, time.Second, timing.recheck(), "the two looks at the window are about a second apart")
	require.Equal(t, 15*time.Second, timing.takeWait(), "a session has fifteen seconds to take the prompt")
	require.Equal(t, 250*time.Millisecond, timing.takePoll())

	timing = promptRunTiming{recheckInterval: time.Millisecond, takenWait: time.Minute, takenPoll: time.Hour}
	require.Equal(t, time.Millisecond, timing.recheck())
	require.Equal(t, time.Minute, timing.takeWait())
	require.Equal(t, time.Hour, timing.takePoll())
}

func TestTaskServiceRunTaskPrompt_SameFailsWhenThePromptCannotBeSent(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)
	svc.sessionClient.submitPromptErr = errors.New("tmux paste failed")

	result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

	requireFailed(t, result, "tmux paste failed")
}

func TestTaskServiceRunTaskPrompt_SameLaunchesWithTheLastConversationWhenNoProviderRuns(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderCodex, SessionID: "3f2a9c1e-77aa-4c1b-9d00-0123456789ab",
	}

	result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

	requireStarted(t, svc, result, "resumed 3f2a…")
	require.Equal(t, "3f2a9c1e-77aa-4c1b-9d00-0123456789ab", svc.providerRepo.promptRunLaunches[0].ResumeSessionID)
	require.Contains(t, svc.sessionClient.startedLaunch.Command, "3f2a9c1e-77aa-4c1b-9d00-0123456789ab",
		"the resume id reaches the launched argv")
	require.Empty(t, svc.sessionClient.respawnedTasks, "there is no session to end")
	require.Zero(t, svc.sessionClient.paneCheckCount(), "there is no session to look at")
}

func TestTaskServiceRunTaskPrompt_SameWithoutAPreviousConversationStartsFresh(t *testing.T) {
	svc, _ := promptRunFixture(t)

	result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

	requireStarted(t, svc, result, "no previous session, started fresh")
	require.Empty(t, svc.providerRepo.promptRunLaunches[0].ResumeSessionID)
}

func TestTaskServiceRunTaskPrompt_SameLaunchesWhenTheWindowIsMissing(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.sessionClient.inspectState = TaskSessionRuntimeState{}

	result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

	requireStarted(t, svc, result, "no previous session, started fresh")
}

func TestTaskServiceRunTaskPrompt_SameIgnoresAnotherProvidersSession(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderClaude, SessionID: "claude-session",
	}

	result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

	requireStarted(t, svc, result, "no previous session, started fresh")
	require.Empty(t, svc.providerRepo.promptRunLaunches[0].ResumeSessionID)
	require.NotContains(t, svc.sessionClient.startedLaunch.Command, "claude-session")
}

// --- new

func TestTaskServiceRunTaskPrompt_NewEndsAnIdleSessionThenLaunches(t *testing.T) {
	svc, task := promptRunFixture(t)
	task.TmuxWindow = "s2"
	idleProvider(svc)
	svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderCodex, SessionID: "sess-1",
	}

	result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionNew})

	requireStarted(t, svc, result, "started fresh")
	require.Len(t, svc.sessionClient.respawnedTasks, 1)
	require.Equal(t, "s2", svc.sessionClient.respawnedTasks[0].TmuxWindow, "the task's own window")
	require.Equal(t, []string{"ensure_task_session_environment", "respawn_task_pane", "start_task_session"}, svc.events,
		"the pane is respawned, then the provider is started in it")
	require.Empty(t, svc.providerRepo.promptRunLaunches[0].ResumeSessionID, "a new session never resumes")
}

func TestTaskServiceRunTaskPrompt_NewLaunchesStraightIntoAPaneWithoutTheProvider(t *testing.T) {
	for _, state := range []TaskSessionRuntimeState{
		idlePane(),
		{},
		{Exists: true, ActiveCommands: []string{"zsh", "vim"}},
	} {
		svc, _ := promptRunFixture(t)
		svc.sessionClient.inspectState = state

		result := runPrompt(t, svc, RunTaskPromptInput{})

		requireStarted(t, svc, result, "started fresh")
		require.Empty(t, svc.sessionClient.respawnedTasks, "there is no provider session to end")
		require.Equal(t, []string{"ensure_task_session_environment", "start_task_session"}, svc.events)
	}
}

func TestTaskServiceRunTaskPrompt_NewNeverResumesOrSendsToARunningSession(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)
	svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderCodex, SessionID: "sess-1",
	}

	for _, mode := range []PromptRunSessionMode{PromptRunSessionNew, ""} {
		svc.sessionClient.startedTask = nil
		result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: mode})

		requireStarted(t, svc, result, "started fresh")
	}
	for _, launch := range svc.providerRepo.promptRunLaunches {
		require.Empty(t, launch.ResumeSessionID)
	}
}

// --- failures

func TestTaskServiceRunTaskPrompt_ALaunchThatFailsIsAFailedRunNotAnError(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(svc *testTaskServiceHarness)
		detail string
	}{
		{"the window cannot be inspected", func(svc *testTaskServiceHarness) {
			svc.sessionClient.inspectErr = errors.New("tmux exploded")
		}, "tmux exploded"},
		{"the session cannot be started", func(svc *testTaskServiceHarness) {
			svc.sessionClient.startErr = errors.New("tmux refused")
		}, "tmux refused"},
		{"the prompt cannot be written", func(svc *testTaskServiceHarness) {
			svc.promptFiles.err = errors.New("disk full")
		}, "disk full"},
		{"the provider cannot build the launch", func(svc *testTaskServiceHarness) {
			svc.providerRepo.promptRunErr = errors.New("accept_edits is not supported by codex")
		}, "not supported by codex"},
		{"the provider environment cannot be ensured", func(svc *testTaskServiceHarness) {
			svc.providerRepo.sessionEnvErr = errors.New("cannot write the hooks")
		}, "cannot write the hooks"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := promptRunFixture(t)
			tt.break_(svc)

			result, err := svc.service.RunTaskPrompt(
				t.Context(),
				RunTaskPromptInput{TaskID: "task-1", Prompt: "triage"},
			)

			require.NoError(t, err)
			requireFailed(t, result, tt.detail)
		})
	}
}

func TestTaskServiceRunTaskPrompt_DoesNotEndTheSessionWhenTheRunCannotBeBuilt(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)
	svc.providerRepo.promptRunErr = errors.New("not supported by codex")

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireFailed(t, result, "not supported by codex")
	require.Empty(t, svc.sessionClient.respawnedTasks)
	require.Nil(t, svc.sessionClient.startedTask)
}

func TestTaskServiceRunTaskPrompt_DoesNotLaunchWhenTheSessionCannotBeEnded(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)
	svc.sessionClient.respawnErr = errors.New("processes did not exit")

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireFailed(t, result, "processes did not exit")
	require.Nil(t, svc.sessionClient.startedTask)
}

func TestTaskServiceRunTaskPrompt_ACancelledRunIsAnErrorNotAFailedRun(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)
	svc.service.promptRunTiming.takenWait = time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	svc.sessionClient.onSubmitPrompt = func(*Task, string) { cancel() }

	_, err := svc.service.RunTaskPrompt(ctx, RunTaskPromptInput{
		TaskID: "task-1", Prompt: "triage", SessionMode: PromptRunSessionSame,
	})

	require.ErrorIs(t, err, context.Canceled)
}

func TestTaskServiceRunTaskPrompt_ReturnsAnUnreadableTaskAsAnError(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.taskRepo.listErr = errors.New("database is locked")

	_, err := svc.service.RunTaskPrompt(t.Context(), RunTaskPromptInput{TaskID: "task-1", Prompt: "triage"})

	require.ErrorContains(t, err, "database is locked")
	requireNothingDone(t, svc)
}

// --- the launch

func TestTaskServiceRunTaskPrompt_LaunchesTheProvidersArgvThroughThePromptWrapper(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.providerRepo.promptRunSpec = TaskSessionLaunchSpec{
		Command:     []string{"env", "CODEX_HOME=/h", "codex", "--full-auto"},
		ReadyMarker: "›",
	}

	result := runPrompt(t, svc, RunTaskPromptInput{Prompt: "- fix everything"})

	requireStarted(t, svc, result, "started fresh")
	require.Equal(t, []string{
		"sh", "-c", `p=$(cat "$0" && printf x) || exit; exec "$@" -- "${p%x}"`,
		"/data/prompts/task-1.txt",
		"env", "CODEX_HOME=/h", "codex", "--full-auto",
	}, svc.sessionClient.startedLaunch.Command)
	require.Equal(t, []promptFileWrite{{taskID: "task-1", text: "- fix everything"}}, svc.promptFiles.writes)
	require.Equal(t, "task-1", svc.sessionClient.startedTask.ID)
}

func TestTaskServiceRunTaskPrompt_TypesNothingIntoTheProviderItLaunches(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.providerRepo.promptRunSpec = TaskSessionLaunchSpec{
		Command:      []string{"codex"},
		ReadyMarker:  "›",
		PrefillInput: []string{"typed into the provider"},
	}

	runPrompt(t, svc, RunTaskPromptInput{})

	require.Empty(t, svc.sessionClient.startedLaunch.PrefillInput)
	require.NotContains(t, svc.events, "prefill_task_session")
	require.Empty(t, svc.sessionClient.prefilledLaunch.PrefillInput)
	require.Empty(t, svc.sessionClient.submittedInputs)
	require.Empty(t, svc.sessionClient.submittedPrompts)
}

func TestTaskServiceRunTaskPrompt_LeavesTheTaskRecordAndLaunchDefaultsAlone(t *testing.T) {
	svc, task := promptRunFixture(t)
	task.Model, task.Effort = "opus", "high"
	svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderCodex, SessionID: "sess-1",
	}

	runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame, PermissionMode: TaskPermissionModeBypass})

	require.Zero(t, svc.taskRepo.updateCount, "the task record is not written")
	require.Empty(t, svc.taskRepo.updatedTasks)
	require.Nil(t, svc.providerConfig.savedLaunchDefaults, "the remembered launch options are not written")
	require.Equal(t, "opus", svc.sessionClient.startedTask.Model, "the run uses the task's stored options")
	require.Equal(t, "high", svc.sessionClient.startedTask.Effort)
}

func TestTaskServiceRunTaskPrompt_LaunchesWithTheTasksProviderEnvironment(t *testing.T) {
	svc, task := promptRunFixture(t)
	task.ProviderEnv = ProviderEnv{"CODEX_HOME": "/home/me/.codex-acme"}

	runPrompt(t, svc, RunTaskPromptInput{})

	require.Equal(t, task.ProviderEnv, svc.providerRepo.sessionEnvEnv)
	require.Equal(t, task.ProviderEnv, svc.sessionClient.startedTask.ProviderEnv)
}

func TestTaskServiceRunTaskPrompt_PassesThePermissionModeToTheProvider(t *testing.T) {
	tests := []struct {
		given TaskPermissionMode
		want  TaskPermissionMode
	}{
		{"", TaskPermissionModeAuto},
		{TaskPermissionModeAuto, TaskPermissionModeAuto},
		{TaskPermissionModeDefault, TaskPermissionModeDefault},
		{TaskPermissionModeAcceptEdits, TaskPermissionModeAcceptEdits},
		{TaskPermissionModeBypass, TaskPermissionModeBypass},
	}
	for _, tt := range tests {
		t.Run(string(tt.given), func(t *testing.T) {
			svc, _ := promptRunFixture(t)

			runPrompt(t, svc, RunTaskPromptInput{PermissionMode: tt.given})

			require.Equal(t, tt.want, svc.providerRepo.promptRunLaunches[0].PermissionMode)
		})
	}
}

func TestTaskServiceRunTaskPrompt_RejectsInputThatCannotRun(t *testing.T) {
	tests := []struct {
		name  string
		input RunTaskPromptInput
		want  string
	}{
		{"no task", RunTaskPromptInput{Prompt: "triage"}, "task ID is required"},
		{"no prompt", RunTaskPromptInput{TaskID: "task-1"}, "prompt is required"},
		{"blank prompt", RunTaskPromptInput{TaskID: "task-1", Prompt: " \n\t"}, "prompt is required"},
		{
			"unknown session mode",
			RunTaskPromptInput{TaskID: "task-1", Prompt: "x", SessionMode: "resume"},
			"unknown session mode",
		},
		{
			"unknown permission mode",
			RunTaskPromptInput{TaskID: "task-1", Prompt: "x", PermissionMode: "yolo"},
			"unknown permission mode",
		},
		{
			"prompt too long for an argument",
			RunTaskPromptInput{TaskID: "task-1", Prompt: strings.Repeat("x", maxPromptRunPromptBytes+1)},
			"prompt is too long",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := promptRunFixture(t)

			_, err := svc.service.RunTaskPrompt(t.Context(), tt.input)

			require.ErrorContains(t, err, tt.want)
			requireNothingDone(t, svc)
		})
	}
}

func TestPromptRunCommand_WrapsTheArgvSoThePromptIsItsLastArgument(t *testing.T) {
	got := promptRunCommand(
		"/data/prompts/task-1.txt",
		[]string{"env", "CLAUDE_CONFIG_DIR=/c", "claude", "--model", "opus"},
	)

	require.Equal(t, []string{
		"sh", "-c", `p=$(cat "$0" && printf x) || exit; exec "$@" -- "${p%x}"`, "/data/prompts/task-1.txt",
		"env", "CLAUDE_CONFIG_DIR=/c", "claude", "--model", "opus",
	}, got)
}

func TestPromptRunCommand_DoesNotShareTheProvidersArgv(t *testing.T) {
	argv := make([]string, 1, 8)
	argv[0] = "codex"

	got := promptRunCommand("/p", argv)
	got[4] = "changed"

	require.Equal(t, "codex", argv[0])
}

func TestParseTaskPermissionMode(t *testing.T) {
	for name, want := range map[string]TaskPermissionMode{
		"":             TaskPermissionModeAuto,
		" ":            TaskPermissionModeAuto,
		"auto":         TaskPermissionModeAuto,
		"default":      TaskPermissionModeDefault,
		"accept_edits": TaskPermissionModeAcceptEdits,
		"bypass":       TaskPermissionModeBypass,
	} {
		got, err := ParseTaskPermissionMode(name)
		require.NoError(t, err, name)
		require.Equal(t, want, got, name)
	}

	_, err := ParseTaskPermissionMode("accept-edits")
	require.Error(t, err)
}

func TestShortSessionID(t *testing.T) {
	require.Equal(t, "3f2a…", shortSessionID("3f2a9c1e-77aa"))
	require.Equal(t, "3f2a", shortSessionID("3f2a"))
	require.Equal(t, "ab", shortSessionID("ab"))
}

func TestTaskServiceRunTaskPrompt_EndsNothingForAPromptThatIsTooLong(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)

	_, err := svc.service.RunTaskPrompt(t.Context(), RunTaskPromptInput{
		TaskID: "task-1",
		Prompt: strings.Repeat("x", maxPromptRunPromptBytes+1),
	})

	require.ErrorContains(t, err, "prompt is too long")
	requireNothingDone(t, svc)
}

func TestTaskServiceRunTaskPrompt_TakesAPromptOfTheLargestSize(t *testing.T) {
	svc, _ := promptRunFixture(t)

	result := runPrompt(t, svc, RunTaskPromptInput{Prompt: strings.Repeat("x", maxPromptRunPromptBytes)})

	requireStarted(t, svc, result, "started fresh")
}

func TestTaskServiceRunTaskPrompt_SendsALargePromptToARunningSessionToo(t *testing.T) {
	svc, _ := promptRunFixture(t)
	idleProvider(svc)
	providerTakesPrompts(svc)

	_, err := svc.service.RunTaskPrompt(t.Context(), RunTaskPromptInput{
		TaskID: "task-1", Prompt: strings.Repeat("x", maxPromptRunPromptBytes+1), SessionMode: PromptRunSessionSame,
	})

	require.ErrorContains(t, err, "prompt is too long", "one cap whichever way the run goes")
}

func setPromptRunStatus(svc *testTaskServiceHarness, status TaskStatusUpdate) {
	status.TaskID = "task-1"
	status.Provider = ProviderCodex
	status.ObservedAt = time.Now().UTC()
	svc.taskRepo.mu.Lock()
	defer svc.taskRepo.mu.Unlock()
	svc.taskRepo.latestByTask["task-1"] = status
}

func TestTaskServiceRunTaskPrompt_HoldsTheTaskUntilTheProviderHasTakenThePrompt(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.launcher.providerSessionWait = 10 * time.Second
	svc.launcher.sessionPollInterval = time.Millisecond
	// The status the session before left behind: not the new provider's.
	setPromptRunStatus(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop})
	typed := make(chan struct{})
	svc.sessionClient.onStart = func(*Task) { close(typed) }

	type outcome struct {
		result RunTaskPromptResult
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		result, err := svc.service.RunTaskPrompt(t.Context(), RunTaskPromptInput{TaskID: "task-1", Prompt: "triage"})
		finished <- outcome{result, err}
	}()
	isDone := func() bool {
		select {
		case done := <-finished:
			finished <- done
			return true
		default:
			return false
		}
	}

	// The command is typed, and the provider has said nothing yet.
	<-typed
	other, err := svc.service.RunTaskPrompt(t.Context(), RunTaskPromptInput{TaskID: "task-1", Prompt: "second"})
	require.NoError(t, err)
	require.Equal(t, promptRunSkipped("another operation is running"), other,
		"a second run would see a bare shell, or the old session's status, and type over this one")
	require.Len(t, svc.promptFiles.writes, 1, "the second run left the first one's prompt alone")
	require.False(t, isDone())

	// The provider starts its session but has not taken the prompt yet: a
	// session that has just started reads as idle, and would be ended.
	setPromptRunStatus(svc, TaskStatusUpdate{Phase: TaskStatusPhaseStarting, RawEventName: HookEventSessionStart})
	require.Never(t, isDone, 50*time.Millisecond, 5*time.Millisecond)
	setPromptRunStatus(
		svc,
		TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventSessionStart},
	)
	require.Never(t, isDone, 50*time.Millisecond, 5*time.Millisecond, "a resumed session reads as idle")
	// The end of the session before can arrive late.
	setPromptRunStatus(svc, TaskStatusUpdate{Phase: TaskStatusPhaseStopped, RawEventName: HookEventSessionEnd})
	require.Never(t, isDone, 50*time.Millisecond, 5*time.Millisecond)

	// It takes the prompt, and the task is free again.
	setPromptRunStatus(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking, RawEventName: HookEventUserPromptSubmit})
	require.Eventually(t, isDone, 5*time.Second, time.Millisecond)
	done := <-finished
	require.NoError(t, done.err)
	require.Equal(
		t,
		RunTaskPromptResult{Outcome: PromptRunStarted, Detail: "started fresh"},
		withoutStartTime(t, done.result),
	)
	require.NoError(t, svc.service.operations.Run(
		t.Context(), "task-1", taskOperationRename, false, func(context.Context) error { return nil },
	))
}

func TestTaskServiceRunTaskPrompt_LetsGoOfTheTaskWhenTheProviderStaysSilent(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.launcher.providerSessionWait = 20 * time.Millisecond
	svc.launcher.sessionPollInterval = time.Millisecond

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireStarted(t, svc, result, "started fresh")
	require.NoError(t, svc.service.operations.Run(
		t.Context(), "task-1", taskOperationRename, false, func(context.Context) error { return nil },
	), "a provider held up by a startup dialog does not hold the task for good")
}

func TestTaskServiceRunTaskPrompt_WithdrawsAPromptAnEarlierLaunchLeftWaiting(t *testing.T) {
	svc, task := promptRunFixture(t)
	svc.launcher.providerSessionWait = 5 * time.Millisecond
	svc.launcher.providerSessionLateWait = 10 * time.Second
	svc.launcher.sessionPollInterval = time.Millisecond
	// The user's own launch has a prompt, and its provider is held up by a
	// startup dialog, so the prompt waits for the session start in the
	// background.
	_, err := svc.launcher.launchSession(t.Context(), task, false)
	require.NoError(t, err)
	svc.sessionClient.startedTask = nil

	// A run then replaces that session with one of its own.
	idleProvider(svc)
	result := runPrompt(t, svc, RunTaskPromptInput{})
	requireStarted(t, svc, result, "started fresh")

	// The run's provider reports its session. It is not the launch's: the
	// task's prompt is not typed into it.
	svc.taskRepo.mu.Lock()
	svc.taskRepo.providerSessionsByTask["task-1"] = append(svc.taskRepo.providerSessionsByTask["task-1"],
		TaskProviderSession{TaskID: "task-1", Provider: ProviderCodex, ProviderSessionID: "the-runs-session"})
	svc.taskRepo.mu.Unlock()
	prefilled := func() bool {
		svc.sessionClient.mu.Lock()
		defer svc.sessionClient.mu.Unlock()
		return svc.sessionClient.prefillCalls > 0
	}
	require.Never(t, prefilled, 150*time.Millisecond, 5*time.Millisecond)
}

func TestTaskServiceRunTaskPrompt_LeavesAPromptWaitingWhenTheRunIsSkipped(t *testing.T) {
	svc, task := promptRunFixture(t)
	svc.launcher.providerSessionWait = 5 * time.Millisecond
	svc.launcher.providerSessionLateWait = 10 * time.Second
	svc.launcher.sessionPollInterval = time.Millisecond
	_, err := svc.launcher.launchSession(t.Context(), task, false)
	require.NoError(t, err)

	// A run that does nothing leaves the user's launch to type its prompt.
	runningProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking})
	svc.sessionClient.paneShows = []paneShow{{shows: true}}
	result := runPrompt(t, svc, RunTaskPromptInput{})
	require.Equal(t, PromptRunSkipped, result.Outcome)
	svc.taskRepo.mu.Lock()
	svc.taskRepo.providerSessionsByTask["task-1"] = []TaskProviderSession{
		{TaskID: "task-1", Provider: ProviderCodex, ProviderSessionID: "the-launchs-session"},
	}
	svc.taskRepo.mu.Unlock()

	require.Eventually(t, func() bool {
		svc.sessionClient.mu.Lock()
		defer svc.sessionClient.mu.Unlock()
		return svc.sessionClient.prefillCalls == 1
	}, 5*time.Second, time.Millisecond)
}

func TestPromptRunCommand_DeliversThePromptToTheProviderByteForByte(t *testing.T) {
	for name, prompt := range map[string]string{
		"plain":                       "triage the open issues",
		"leading dash":                "- fix everything",
		"just a flag":                 "--help",
		"trailing newline":            "triage the issues\n",
		"trailing newlines":           "triage the issues\n\n\n",
		"only newlines in the middle": "a\n\nb",
		"ends with the marker letter": "relax",
		"quotes and shell":            "it's \"quoted\" $HOME ${HOME} `id` $(id) \\ ; && | > < \t tab é",
		"one space":                   " ",
	} {
		t.Run(name, func(t *testing.T) {
			promptFile := filepath.Join(t.TempDir(), "task 1.txt")
			require.NoError(t, os.WriteFile(promptFile, []byte(prompt), 0o600))
			// The provider stands in as a shell that prints its last argument.
			command := promptRunCommand(promptFile, []string{"sh", "-c", `printf %s "$2"`, "provider"})

			out, err := exec.CommandContext(t.Context(), command[0], command[1:]...).Output()

			require.NoError(t, err)
			require.Equal(t, prompt, string(out))
		})
	}
}

func TestPromptRunCommand_DoesNotStartTheProviderWhenThePromptCannotBeRead(t *testing.T) {
	command := promptRunCommand(
		filepath.Join(t.TempDir(), "missing.txt"),
		[]string{"sh", "-c", `echo started`, "provider"},
	)

	out, err := exec.CommandContext(t.Context(), command[0], command[1:]...).CombinedOutput()

	require.Error(t, err)
	require.NotContains(t, string(out), "started", "no provider on an empty prompt")
}
