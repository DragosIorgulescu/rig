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

// promptRunFixture is a ready Task whose pane is an idle shell, so that a
// prompt run launches unless a test says otherwise.
func promptRunFixture(t *testing.T) (*testTaskServiceHarness, *Task) {
	t.Helper()

	svc := newTestTaskService(t)
	task := readyTaskFixture()
	svc.taskRepo.listTasks = []*Task{task}
	svc.sessionClient.inspectState = idlePane()
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

// setWindowedProvider makes the Task's provider run in its pane, with the
// given latest status.
func setWindowedProvider(svc *testTaskServiceHarness, status TaskStatusUpdate) {
	svc.sessionClient.inspectState = TaskSessionRuntimeState{
		Exists:         true,
		ActiveCommands: []string{"zsh", "codex"},
	}
	status.TaskID = "task-1"
	status.Provider = ProviderCodex
	status.ObservedAt = time.Now().UTC()
	svc.taskRepo.latestByTask["task-1"] = status
}

func requireSkipped(t *testing.T, svc *testTaskServiceHarness, result RunTaskPromptResult, detail string) {
	t.Helper()

	require.Equal(t, RunTaskPromptResult{Outcome: PromptRunSkipped, Detail: detail}, result)
	require.Nil(t, svc.sessionClient.startedTask, "a skipped run starts nothing")
	require.Empty(t, svc.sessionClient.closedTasks, "a skipped run closes nothing")
	require.Empty(t, svc.promptFiles.writes, "a skipped run writes no prompt")
}

func requireLaunched(t *testing.T, svc *testTaskServiceHarness, result RunTaskPromptResult, detail string) {
	t.Helper()

	require.Equal(t, RunTaskPromptResult{Outcome: PromptRunLaunched, Detail: detail}, result)
	require.NotNil(t, svc.sessionClient.startedTask, "a launched run starts the session")
}

func TestTaskServiceRunTaskPrompt_SkipsATaskThatIsNotReady(t *testing.T) {
	for _, status := range []TaskCreationStatus{TaskCreationStatusCreating, TaskCreationStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			svc, task := promptRunFixture(t)
			task.CreationStatus = status

			result := runPrompt(t, svc, RunTaskPromptInput{})

			requireSkipped(t, svc, result, "skipped: task is not ready ("+string(status)+")")
		})
	}
}

func TestTaskServiceRunTaskPrompt_SkipsAShelvedTask(t *testing.T) {
	svc, task := promptRunFixture(t)
	task.ShelvedAt = time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireSkipped(t, svc, result, "skipped: task is shelved")
}

func TestTaskServiceRunTaskPrompt_SkipsATaskThatDoesNotExist(t *testing.T) {
	svc, _ := promptRunFixture(t)

	result := runPrompt(t, svc, RunTaskPromptInput{TaskID: "task-404"})

	requireSkipped(t, svc, result, "skipped: task not found")
}

func TestTaskServiceRunTaskPrompt_SkipsWhileAnotherOperationHoldsTheTask(t *testing.T) {
	svc, _ := promptRunFixture(t)

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
	requireSkipped(t, svc, result, "skipped: another operation is running")
	require.Zero(t, svc.sessionClient.viewedChecks, "the lock is taken before anything else is checked")
}

func TestTaskServiceRunTaskPrompt_HoldsTheTaskWhileItRuns(t *testing.T) {
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

func TestTaskServiceRunTaskPrompt_SkipsWhileTheUserIsViewingTheWindow(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.sessionClient.viewed = true

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireSkipped(t, svc, result, "skipped: you're viewing it")
}

func TestTaskServiceRunTaskPrompt_ChecksViewingBeforeTouchingTheWindow(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.sessionClient.viewed = true
	setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop})

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireSkipped(t, svc, result, "skipped: you're viewing it")
}

func TestTaskServiceRunTaskPrompt_ReturnsATmuxFailureAsAnError(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.sessionClient.viewedErr = errors.New("tmux exploded")

	_, err := svc.service.RunTaskPrompt(t.Context(), RunTaskPromptInput{TaskID: "task-1", Prompt: "triage"})

	require.ErrorContains(t, err, "tmux exploded")
	require.Nil(t, svc.sessionClient.startedTask)
}

func TestTaskServiceRunTaskPrompt_LaunchesWhenTheWindowIsMissing(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.sessionClient.inspectState = TaskSessionRuntimeState{}

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireLaunched(t, svc, result, "started fresh")
	require.Empty(t, svc.sessionClient.closedTasks, "there is no window to close")
}

func TestTaskServiceRunTaskPrompt_ALaunchIntoAMissingWindowIgnoresStaleProcessEvidence(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.sessionClient.inspectState = TaskSessionRuntimeState{
		ActiveCommands:                  []string{"node"},
		ChildProcessEvidenceUnavailable: true,
	}

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireLaunched(t, svc, result, "started fresh")
}

func TestTaskServiceRunTaskPrompt_LaunchesIntoABareShell(t *testing.T) {
	for _, commands := range [][]string{
		{"zsh"},
		{"-zsh"},
		{"bash", "-bash"},
		{"/bin/zsh"},
		{"fish"},
		{"sh"},
		nil,
	} {
		t.Run(commandsName(commands), func(t *testing.T) {
			svc, _ := promptRunFixture(t)
			svc.sessionClient.inspectState = TaskSessionRuntimeState{Exists: true, ActiveCommands: commands}

			result := runPrompt(t, svc, RunTaskPromptInput{})

			requireLaunched(t, svc, result, "started fresh")
			require.Empty(t, svc.sessionClient.closedTasks, "a shell is not a provider session to close")
		})
	}
}

func commandsName(commands []string) string {
	if len(commands) == 0 {
		return "no commands"
	}
	name := ""
	for _, command := range commands {
		name += command + " "
	}
	return name
}

func TestTaskServiceRunTaskPrompt_SkipsAPaneRunningSomethingOtherThanAShell(t *testing.T) {
	tests := []struct {
		name     string
		commands []string
		detail   string
	}{
		{"dev server", []string{"node"}, "skipped: pane is running node"},
		{"editor under a shell", []string{"zsh", "vim"}, "skipped: pane is running vim"},
		{"a command by path", []string{"zsh", "/usr/bin/python3"}, "skipped: pane is running python3"},
		{"a shell look-alike", []string{"zsh", "zshell"}, "skipped: pane is running zshell"},
		{"another provider", []string{"zsh", "claude"}, "skipped: pane is running claude"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := promptRunFixture(t)
			svc.sessionClient.inspectState = TaskSessionRuntimeState{Exists: true, ActiveCommands: tt.commands}

			result := runPrompt(t, svc, RunTaskPromptInput{})

			requireSkipped(t, svc, result, tt.detail)
		})
	}
}

func TestTaskServiceRunTaskPrompt_SkipsAShellWhoseChildrenCannotBeSeen(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.sessionClient.inspectState = TaskSessionRuntimeState{
		Exists:                          true,
		ActiveCommands:                  []string{"zsh"},
		ChildProcessEvidenceUnavailable: true,
	}

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireSkipped(t, svc, result, "skipped: pane is running zsh and what it started cannot be seen")
}

func TestTaskServiceRunTaskPrompt_SkipsAPaneWithNoEvidenceAtAll(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.sessionClient.inspectState = TaskSessionRuntimeState{Exists: true, ChildProcessEvidenceUnavailable: true}

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireSkipped(t, svc, result, "skipped: pane state unknown")
}

func TestTaskServiceRunTaskPrompt_SkipsWhileTheProviderIsWorking(t *testing.T) {
	for _, phase := range []TaskStatusPhase{TaskStatusPhaseWorking, TaskStatusPhaseWorkingInBackground} {
		t.Run(string(phase), func(t *testing.T) {
			svc, _ := promptRunFixture(t)
			setWindowedProvider(svc, TaskStatusUpdate{Phase: phase, RawEventName: HookEventPostToolUse})
			svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
				TaskID: "task-1", Provider: ProviderCodex, SessionID: "sess-1",
			}

			result := runPrompt(t, svc, RunTaskPromptInput{})

			requireSkipped(t, svc, result, "skipped: still working")
			require.Equal(t, []string{"sess-1"}, svc.providerRepo.interruptChecks,
				"the provider is asked about the session the status belongs to")
		})
	}
}

func TestTaskServiceRunTaskPrompt_SkipsAWorkingProviderWithNoSessionToAskAbout(t *testing.T) {
	svc, _ := promptRunFixture(t)
	setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking})

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireSkipped(t, svc, result, "skipped: still working")
	require.Empty(t, svc.providerRepo.interruptChecks)
}

func TestTaskServiceRunTaskPrompt_IgnoresAnInterruptionOfAnotherProvidersSession(t *testing.T) {
	svc, _ := promptRunFixture(t)
	setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking})
	svc.providerRepo.interrupted = true
	svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderClaude, SessionID: "claude-sess",
	}

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireSkipped(t, svc, result, "skipped: still working")
	require.Empty(t, svc.providerRepo.interruptChecks)
}

func TestTaskServiceRunTaskPrompt_TreatsAnInterruptedWorkingProviderAsIdle(t *testing.T) {
	svc, _ := promptRunFixture(t)
	setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking, RawEventName: HookEventPreToolUse})
	svc.providerRepo.interrupted = true
	svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderCodex, SessionID: "sess-1",
	}

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireLaunched(t, svc, result, "started fresh")
	require.Equal(t, []string{"sess-1"}, svc.providerRepo.interruptChecks)
	require.Len(t, svc.sessionClient.closedTasks, 1)
	require.Equal(t, []string{"ensure_task_session_environment", "close_task_window", "start_task_session"}, svc.events,
		"the idle session is closed before the run starts")
}

func TestTaskServiceRunTaskPrompt_ReturnsAFailedInterruptionCheckAsAnError(t *testing.T) {
	svc, _ := promptRunFixture(t)
	setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking})
	svc.providerRepo.interruptedErr = errors.New("transcript unreadable")
	svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderCodex, SessionID: "sess-1",
	}

	_, err := svc.service.RunTaskPrompt(t.Context(), RunTaskPromptInput{TaskID: "task-1", Prompt: "triage"})

	require.ErrorContains(t, err, "transcript unreadable")
	require.Nil(t, svc.sessionClient.startedTask)
	require.Empty(t, svc.sessionClient.closedTasks)
}

func TestTaskServiceRunTaskPrompt_SkipsAProviderWaitingOnAPermissionPrompt(t *testing.T) {
	for _, event := range []string{HookEventNotification, HookEventPermissionRequest} {
		t.Run(event, func(t *testing.T) {
			svc, _ := promptRunFixture(t)
			setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: event})

			result := runPrompt(t, svc, RunTaskPromptInput{})

			requireSkipped(t, svc, result, "skipped: waiting on a permission prompt")
		})
	}
}

func TestTaskServiceRunTaskPrompt_ClosesAnIdleSessionBeforeLaunching(t *testing.T) {
	tests := []struct {
		name   string
		status TaskStatusUpdate
	}{
		{"turn ended", TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop}},
		{"turn failed", TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStopFailure}},
		{"resumed", TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventSessionStart}},
		{"starting", TaskStatusUpdate{Phase: TaskStatusPhaseStarting, RawEventName: HookEventSessionStart}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, task := promptRunFixture(t)
			task.TmuxWindow = "s2"
			setWindowedProvider(svc, tt.status)

			result := runPrompt(t, svc, RunTaskPromptInput{})

			requireLaunched(t, svc, result, "started fresh")
			require.Len(t, svc.sessionClient.closedTasks, 1)
			require.Equal(t, "s2", svc.sessionClient.closedTasks[0].TmuxWindow, "the task's own window")
			require.Equal(
				t,
				[]string{"ensure_task_session_environment", "close_task_window", "start_task_session"},
				svc.events,
			)
		})
	}
}

func TestTaskServiceRunTaskPrompt_SkipsAStatusItDoesNotKnow(t *testing.T) {
	t.Run("stopped", func(t *testing.T) {
		svc, _ := promptRunFixture(t)
		setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseStopped, RawEventName: HookEventSessionEnd})

		result := runPrompt(t, svc, RunTaskPromptInput{})

		requireSkipped(t, svc, result, "skipped: status stopped unknown")
	})
	t.Run("none recorded", func(t *testing.T) {
		svc, _ := promptRunFixture(t)
		svc.sessionClient.inspectState = TaskSessionRuntimeState{Exists: true, ActiveCommands: []string{"zsh", "codex"}}

		result := runPrompt(t, svc, RunTaskPromptInput{})

		requireSkipped(t, svc, result, "skipped: status unknown")
	})
}

func TestTaskServiceRunTaskPrompt_DoesNotCloseTheWindowWhenTheRunCannotBeBuilt(t *testing.T) {
	svc, _ := promptRunFixture(t)
	setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop})
	svc.providerRepo.promptRunErr = errors.New("not supported by codex")

	_, err := svc.service.RunTaskPrompt(t.Context(), RunTaskPromptInput{TaskID: "task-1", Prompt: "triage"})

	require.ErrorContains(t, err, "not supported by codex")
	require.Empty(t, svc.sessionClient.closedTasks)
	require.Nil(t, svc.sessionClient.startedTask)
}

func TestTaskServiceRunTaskPrompt_DoesNotLaunchWhenTheWindowCannotBeClosed(t *testing.T) {
	svc, _ := promptRunFixture(t)
	setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop})
	svc.sessionClient.closeErr = errors.New("processes did not exit")

	_, err := svc.service.RunTaskPrompt(t.Context(), RunTaskPromptInput{TaskID: "task-1", Prompt: "triage"})

	require.ErrorContains(t, err, "processes did not exit")
	require.Nil(t, svc.sessionClient.startedTask)
}

func TestTaskServiceRunTaskPrompt_ReturnsALaunchFailureAsAnError(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.sessionClient.startErr = errors.New("tmux refused")

	_, err := svc.service.RunTaskPrompt(t.Context(), RunTaskPromptInput{TaskID: "task-1", Prompt: "triage"})

	require.ErrorContains(t, err, "tmux refused")
}

func TestTaskServiceRunTaskPrompt_SameResumesTheLatestSession(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderCodex, SessionID: "3f2a9c1e-77aa-4c1b-9d00-0123456789ab",
	}

	result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

	requireLaunched(t, svc, result, "resumed 3f2a…")
	require.Equal(t, "3f2a9c1e-77aa-4c1b-9d00-0123456789ab", svc.providerRepo.promptRunLaunches[0].ResumeSessionID)
	require.Contains(t, svc.sessionClient.startedLaunch.Command, "3f2a9c1e-77aa-4c1b-9d00-0123456789ab",
		"the resume id reaches the launched argv")
}

func TestTaskServiceRunTaskPrompt_SameWithoutAPreviousSessionStartsFresh(t *testing.T) {
	svc, _ := promptRunFixture(t)

	result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

	requireLaunched(t, svc, result, "no previous session, started fresh")
	require.Empty(t, svc.providerRepo.promptRunLaunches[0].ResumeSessionID)
}

func TestTaskServiceRunTaskPrompt_SameIgnoresAnotherProvidersSession(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderClaude, SessionID: "claude-session",
	}

	result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: PromptRunSessionSame})

	requireLaunched(t, svc, result, "no previous session, started fresh")
	require.Empty(t, svc.providerRepo.promptRunLaunches[0].ResumeSessionID)
	require.NotContains(t, svc.sessionClient.startedLaunch.Command, "claude-session")
}

func TestTaskServiceRunTaskPrompt_NewNeverResumes(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderCodex, SessionID: "sess-1",
	}

	for _, mode := range []PromptRunSessionMode{PromptRunSessionNew, ""} {
		svc.sessionClient.startedTask = nil
		result := runPrompt(t, svc, RunTaskPromptInput{SessionMode: mode})

		requireLaunched(t, svc, result, "started fresh")
	}
	for _, launch := range svc.providerRepo.promptRunLaunches {
		require.Empty(t, launch.ResumeSessionID)
	}
}

func TestTaskServiceRunTaskPrompt_LaunchesTheProvidersArgvThroughThePromptWrapper(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.providerRepo.promptRunSpec = TaskSessionLaunchSpec{
		Command:     []string{"env", "CODEX_HOME=/h", "codex", "--full-auto"},
		ReadyMarker: "›",
	}

	result := runPrompt(t, svc, RunTaskPromptInput{Prompt: "- fix everything"})

	requireLaunched(t, svc, result, "started fresh")
	require.Equal(t, []string{
		"sh", "-c", `p=$(cat "$0" && printf x) || exit; exec "$@" -- "${p%x}"`,
		"/data/prompts/task-1.txt",
		"env", "CODEX_HOME=/h", "codex", "--full-auto",
	}, svc.sessionClient.startedLaunch.Command)
	require.Equal(t, []promptFileWrite{{taskID: "task-1", text: "- fix everything"}}, svc.promptFiles.writes)
	require.Equal(t, "task-1", svc.sessionClient.startedTask.ID)
}

func TestTaskServiceRunTaskPrompt_TypesNothingIntoTheProvider(t *testing.T) {
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
			require.Nil(t, svc.sessionClient.startedTask)
		})
	}
}

func TestTaskServiceRunTaskPrompt_FailsWhenThePromptCannotBeWritten(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.promptFiles.err = errors.New("disk full")

	_, err := svc.service.RunTaskPrompt(t.Context(), RunTaskPromptInput{TaskID: "task-1", Prompt: "triage"})

	require.ErrorContains(t, err, "disk full")
	require.Nil(t, svc.sessionClient.startedTask)
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

func TestTaskServiceRunTaskPrompt_ClosesNothingForAPromptThatIsTooLong(t *testing.T) {
	svc, _ := promptRunFixture(t)
	setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop})

	_, err := svc.service.RunTaskPrompt(t.Context(), RunTaskPromptInput{
		TaskID: "task-1",
		Prompt: strings.Repeat("x", maxPromptRunPromptBytes+1),
	})

	require.ErrorContains(t, err, "prompt is too long")
	require.Empty(t, svc.sessionClient.closedTasks, "the idle session is left as it was")
	require.Empty(t, svc.promptFiles.writes)
}

func TestTaskServiceRunTaskPrompt_TakesAPromptOfTheLargestSize(t *testing.T) {
	svc, _ := promptRunFixture(t)

	result := runPrompt(t, svc, RunTaskPromptInput{Prompt: strings.Repeat("x", maxPromptRunPromptBytes)})

	requireLaunched(t, svc, result, "started fresh")
}

func TestTaskServiceRunTaskPrompt_SkipsAPaneWhoseShellStartedSomething(t *testing.T) {
	// A shell waiting at its prompt has no child process; whatever it started
	// is running, shell or not, and would read what rig types.
	tests := []struct {
		name     string
		commands []string
		children []string
		detail   string
	}{
		{"a script run by sh", []string{"sh", "sh"}, []string{"sh"}, "skipped: pane is running sh"},
		{"a script run by bash", []string{"bash", "bash"}, []string{"bash"}, "skipped: pane is running bash"},
		{"a job in the background", []string{"zsh", "sh"}, []string{"sh"}, "skipped: pane is running sh"},
		{"a nested shell", []string{"zsh", "-bash"}, []string{"-bash"}, "skipped: pane is running bash"},
		{"another run's wrapper still starting", []string{"zsh", "sh"}, []string{"sh"}, "skipped: pane is running sh"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := promptRunFixture(t)
			svc.sessionClient.inspectState = TaskSessionRuntimeState{
				Exists:         true,
				ActiveCommands: tt.commands,
				ChildCommands:  tt.children,
			}

			result := runPrompt(t, svc, RunTaskPromptInput{})

			requireSkipped(t, svc, result, tt.detail)
		})
	}
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
			skip:   "skipped: task is shelved",
		},
		{
			name:   "deleted",
			change: func(svc *testTaskServiceHarness, _ *Task) { svc.taskRepo.listTasks = nil },
			skip:   "skipped: task not found",
		},
		{
			name:   "no longer ready",
			change: func(_ *testTaskServiceHarness, task *Task) { task.CreationStatus = TaskCreationStatusFailed },
			skip:   "skipped: task is not ready (failed)",
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
			requireLaunched(t, svc, result, "started fresh")
			tt.launch(t, svc)
		})
	}
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
	// session that has just started reads as idle, and would be closed.
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
	require.Equal(t, RunTaskPromptResult{Outcome: PromptRunLaunched, Detail: "started fresh"}, done.result)
	require.NoError(t, svc.service.operations.Run(
		t.Context(), "task-1", taskOperationRename, false, func(context.Context) error { return nil },
	))
}

func TestTaskServiceRunTaskPrompt_LetsGoOfTheTaskWhenTheProviderStaysSilent(t *testing.T) {
	svc, _ := promptRunFixture(t)
	svc.launcher.providerSessionWait = 20 * time.Millisecond
	svc.launcher.sessionPollInterval = time.Millisecond

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireLaunched(t, svc, result, "started fresh")
	require.NoError(t, svc.service.operations.Run(
		t.Context(), "task-1", taskOperationRename, false, func(context.Context) error { return nil },
	), "a provider held up by a startup dialog does not hold the task for good")
}

func TestTaskServiceRunTaskPrompt_ReadsTheStatusAfresh(t *testing.T) {
	svc, _ := promptRunFixture(t)
	setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop})
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

	requireSkipped(t, svc, result, "skipped: waiting on a permission prompt")
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
	setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop})
	result := runPrompt(t, svc, RunTaskPromptInput{})
	requireLaunched(t, svc, result, "started fresh")

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
	setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking})
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

func TestTaskServiceRunTaskPrompt_AsksTheProviderAboutTheTranscriptRigRecorded(t *testing.T) {
	svc, _ := promptRunFixture(t)
	setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking})
	svc.providerRepo.interrupted = true
	svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderCodex, SessionID: "sess-2",
	}
	svc.taskRepo.providerSessionsByTask["task-1"] = []TaskProviderSession{
		{
			TaskID: "task-1", Provider: ProviderCodex, ProviderSessionID: "sess-1",
			TranscriptPath: "/home/me/.codex/sessions/sess-1.jsonl", LastObservedAt: time.Unix(100, 0),
		},
		{
			TaskID: "task-1", Provider: ProviderCodex, ProviderSessionID: "sess-2",
			TranscriptPath: "/home/me/.codex/sessions/old.jsonl", LastObservedAt: time.Unix(200, 0),
		},
		{
			TaskID: "task-1", Provider: ProviderCodex, ProviderSessionID: "sess-2",
			TranscriptPath: "/home/me/.codex/sessions/sess-2.jsonl", LastObservedAt: time.Unix(300, 0),
		},
		{
			TaskID: "task-1", Provider: ProviderClaude, ProviderSessionID: "sess-2",
			TranscriptPath: "/home/me/.claude/projects/p/sess-2.jsonl", LastObservedAt: time.Unix(50, 0),
		},
	}

	result := runPrompt(t, svc, RunTaskPromptInput{})

	requireLaunched(t, svc, result, "started fresh")
	require.Len(t, svc.providerRepo.interruptedSessions, 1)
	require.Equal(t, TaskProviderSession{
		TaskID: "task-1", Provider: ProviderCodex, ProviderSessionID: "sess-2",
		TranscriptPath: "/home/me/.codex/sessions/sess-2.jsonl", LastObservedAt: time.Unix(300, 0),
	}, svc.providerRepo.interruptedSessions[0], "the latest record of the session the status belongs to")
}

func TestTaskServiceRunTaskPrompt_AsksAboutASessionByItsIDWhenNoTranscriptWasRecorded(t *testing.T) {
	svc, _ := promptRunFixture(t)
	setWindowedProvider(svc, TaskStatusUpdate{Phase: TaskStatusPhaseWorking})
	svc.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderCodex, SessionID: "sess-1",
	}
	svc.taskRepo.providerSessionsByTask["task-1"] = []TaskProviderSession{
		{TaskID: "task-1", Provider: ProviderCodex, ProviderSessionID: "sess-1"},
	}

	runPrompt(t, svc, RunTaskPromptInput{})

	require.Equal(t, []TaskProviderSession{
		{TaskID: "task-1", Provider: ProviderCodex, ProviderSessionID: "sess-1"},
	}, svc.providerRepo.interruptedSessions)
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
