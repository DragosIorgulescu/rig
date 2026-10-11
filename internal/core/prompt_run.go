package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// maxPromptRunPromptBytes caps a prompt. It reaches the provider as one
// argument of its command, which the system refuses to start when too long:
// Linux allows 128 KiB for one argument, macOS about 1 MiB for the whole
// command and environment. The cap leaves room under both.
const maxPromptRunPromptBytes = 96 * 1024

// RunTaskPrompt starts the Task's provider on the prompt, or reports why it did
// not. The checks run in order and the first one that fails decides the
// outcome: the Task must be ready and on the current list, no other Task
// operation may be running, nobody may be viewing its window, and whatever is
// in the window must be idle or gone. A busy Task is skipped, never
// interrupted: a skip is a normal result, and errors are for real failures.
//
// The Task's operation lock is held until the provider has taken the prompt,
// not only until its command is typed: a second run, or any other operation,
// that came in while the provider was still starting would see a bare shell,
// or a status left by the session before, and type over or close the run.
func (s *service) RunTaskPrompt(ctx context.Context, input RunTaskPromptInput) (RunTaskPromptResult, error) {
	run, err := newPromptRun(input)
	if err != nil {
		return RunTaskPromptResult{}, err
	}

	// Looked at before the lock, so a Task that cannot run is reported as
	// such even while another operation holds it.
	_, skip, err := s.promptRunTask(ctx, run.taskID)
	if err != nil {
		return RunTaskPromptResult{}, err
	}
	if skip != "" {
		return promptRunSkipped(skip), nil
	}

	var result RunTaskPromptResult
	err = s.operations.Run(ctx, run.taskID, taskOperationPromptRun, false, func(ctx context.Context) error {
		// The Task may have been shelved, renamed or deleted since it was
		// read above, so the run works from the record as the lock finds it.
		task, skip, err := s.promptRunTask(ctx, run.taskID)
		if err != nil {
			return err
		}
		if skip != "" {
			result = promptRunSkipped(skip)
			return nil
		}
		result, err = s.runTaskPrompt(ctx, task, run)
		return err
	})
	if errors.Is(err, ErrTaskOperationInProgress) {
		return promptRunSkipped("another operation is running"), nil
	}
	return result, err
}

// promptRunTask reads the Task a prompt run is for. It returns why the Task
// cannot take one, or the Task when it can.
func (s *service) promptRunTask(ctx context.Context, taskID string) (*Task, string, error) {
	task, err := taskByID(ctx, s.tasks, taskID)
	switch {
	case errors.Is(err, ErrTaskNotFound):
		return nil, "task not found", nil
	case err != nil:
		return nil, "", err
	case task.CreationStatus != TaskCreationStatusReady:
		return nil, fmt.Sprintf("task is not ready (%s)", task.CreationStatus), nil
	case task.IsShelved():
		return nil, "task is shelved", nil
	}
	return task, "", nil
}

// promptRun is a validated RunTaskPromptInput.
type promptRun struct {
	taskID         string
	prompt         string
	sessionMode    PromptRunSessionMode
	permissionMode TaskPermissionMode
}

func newPromptRun(input RunTaskPromptInput) (promptRun, error) {
	run := promptRun{
		taskID:      strings.TrimSpace(input.TaskID),
		prompt:      input.Prompt,
		sessionMode: input.SessionMode,
	}
	if run.taskID == "" {
		return promptRun{}, fmt.Errorf("task ID is required")
	}
	if strings.TrimSpace(run.prompt) == "" {
		return promptRun{}, fmt.Errorf("prompt is required")
	}
	if len(run.prompt) > maxPromptRunPromptBytes {
		return promptRun{}, fmt.Errorf("prompt is too long: %d bytes, and a prompt run takes at most %d",
			len(run.prompt), maxPromptRunPromptBytes)
	}
	switch run.sessionMode {
	case "":
		run.sessionMode = PromptRunSessionNew
	case PromptRunSessionNew, PromptRunSessionSame:
	default:
		return promptRun{}, fmt.Errorf("unknown session mode %q: use %s or %s",
			run.sessionMode, PromptRunSessionNew, PromptRunSessionSame)
	}
	mode, err := ParseTaskPermissionMode(string(input.PermissionMode))
	if err != nil {
		return promptRun{}, err
	}
	run.permissionMode = mode
	return run, nil
}

func promptRunSkipped(reason string) RunTaskPromptResult {
	return RunTaskPromptResult{Outcome: PromptRunSkipped, Detail: "skipped: " + reason}
}

// runTaskPrompt is RunTaskPrompt once the Task's operation lock is held.
func (s *service) runTaskPrompt(ctx context.Context, task *Task, run promptRun) (RunTaskPromptResult, error) {
	_, providerClient, err := s.launcher.resolveProvider(ctx, task.Provider)
	if err != nil {
		return RunTaskPromptResult{}, err
	}
	if s.promptFiles == nil {
		return RunTaskPromptResult{}, fmt.Errorf("prompt file store not configured")
	}

	viewed, err := s.tmuxSession.TaskWindowViewed(ctx, task)
	if err != nil {
		return RunTaskPromptResult{}, fmt.Errorf("check whether the task window is being viewed: %w", err)
	}
	if viewed {
		return promptRunSkipped("you're viewing it"), nil
	}

	window, err := s.promptRunWindow(ctx, task, providerClient)
	if err != nil {
		return RunTaskPromptResult{}, err
	}
	if window.busy != "" {
		return promptRunSkipped(window.busy), nil
	}

	resumeSessionID := ""
	if run.sessionMode == PromptRunSessionSame {
		resumeSessionID, err = s.latestProviderSessionID(ctx, task)
		if err != nil {
			return RunTaskPromptResult{}, err
		}
	}

	// Everything that can fail without touching the window comes first, so a
	// failed run does not leave an idle session closed for nothing.
	if err := providerClient.EnsureTaskSessionEnvironment(ctx, task.ProviderEnv); err != nil {
		return RunTaskPromptResult{}, fmt.Errorf("ensure task session environment: %w", err)
	}
	promptFile, err := s.promptFiles.WritePromptFile(task.ID, run.prompt)
	if err != nil {
		return RunTaskPromptResult{}, fmt.Errorf("write prompt file: %w", err)
	}
	launch, err := providerClient.BuildPromptRunLaunchSpec(task, PromptRunLaunch{
		ResumeSessionID: resumeSessionID,
		PermissionMode:  run.permissionMode,
	})
	if err != nil {
		return RunTaskPromptResult{}, fmt.Errorf("build prompt run launch spec: %w", err)
	}
	if len(launch.Command) == 0 {
		return RunTaskPromptResult{}, fmt.Errorf("build prompt run launch spec: provider returned no command")
	}
	// The prompt is an argument of the command, so nothing is typed into the
	// provider once it is up.
	launch.PrefillInput = nil
	launch.Command = promptRunCommand(promptFile, launch.Command)

	// A prompt a user's own launch left waiting for its provider's session
	// start belongs to that launch, not to the provider this run starts.
	s.launcher.withdrawLatePrefill(task.ID)

	// An idle provider's window is closed rather than typed into, so the run
	// starts clean, with its own model, effort and permission mode.
	if window.closeFirst {
		if err := s.tmuxSession.CloseTaskWindow(ctx, task); err != nil {
			return RunTaskPromptResult{}, fmt.Errorf("close the idle session: %w", err)
		}
	}

	// The Task record stays as it was: a run must not change the provider,
	// model or effort the user's own sessions start with.
	launchedAt := time.Now()
	if err := s.tmuxSession.StartTaskSession(ctx, task, launch); err != nil {
		return RunTaskPromptResult{}, fmt.Errorf("start task session: %w", err)
	}
	s.awaitPromptRunStarted(ctx, task, launchedAt)

	switch {
	case resumeSessionID != "":
		return RunTaskPromptResult{
			Outcome: PromptRunLaunched,
			Detail:  "resumed " + shortSessionID(resumeSessionID),
		}, nil
	case run.sessionMode == PromptRunSessionSame:
		return RunTaskPromptResult{Outcome: PromptRunLaunched, Detail: "no previous session, started fresh"}, nil
	default:
		return RunTaskPromptResult{Outcome: PromptRunLaunched, Detail: "started fresh"}, nil
	}
}

// awaitPromptRunStarted waits, within the launcher's bound for a provider to
// report its session, until the provider launched at launchedAt has taken the
// prompt. A provider that never reports, such as one held up by a startup
// dialog, is left to start in its own time once the bound has run out.
func (s *service) awaitPromptRunStarted(ctx context.Context, task *Task, launchedAt time.Time) {
	wait := s.launcher.providerSessionWait
	if wait <= 0 {
		return
	}
	interval := cmp.Or(s.launcher.sessionPollInterval, 250*time.Millisecond)
	deadline := time.Now().Add(wait)
	for {
		status, err := s.tasks.LatestTaskStatus(ctx, task.ID)
		if err == nil && promptRunTaken(status, launchedAt) {
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// promptRunTaken reports whether a status says the provider launched at
// launchedAt has taken the prompt: it is working, or has finished its turn,
// after the launch. A session start does not say so, since a provider reports
// it before it submits the prompt, and a resumed session reads as idle in
// between; a run that looked then would close the provider it just started.
// Nor does a provider that stopped, which can be the session before.
func promptRunTaken(status *TaskStatusUpdate, launchedAt time.Time) bool {
	if status == nil || !status.ObservedAt.After(launchedAt) || status.RawEventName == HookEventSessionStart {
		return false
	}
	switch status.Phase {
	case TaskStatusPhaseWorking, TaskStatusPhaseWorkingInBackground, TaskStatusPhaseWaitingForInput:
		return true
	default:
		return false
	}
}

// promptRunWindowState is what the Task's window allows a prompt run to do.
type promptRunWindowState struct {
	// busy, when not empty, is why the run must be skipped.
	busy string
	// closeFirst means an idle provider is running in the window and the
	// window must be closed before the run is launched.
	closeFirst bool
}

// promptRunWindow reads the Task's window and decides whether, and how, the
// provider can be launched in it. It changes nothing.
func (s *service) promptRunWindow(
	ctx context.Context,
	task *Task,
	providerClient ProviderClient,
) (promptRunWindowState, error) {
	runtime, err := s.tmuxSession.InspectTaskSession(ctx, task)
	if err != nil {
		return promptRunWindowState{}, fmt.Errorf("inspect task session: %w", err)
	}
	if !runtime.Exists {
		return promptRunWindowState{}, nil
	}

	if !taskSessionRunningProvider(runtime, providerClient.TaskSessionCommandName()) {
		return promptRunWindowState{busy: busyPaneReason(runtime)}, nil
	}

	// The status decides whether a window is closed, so it is read afresh: the
	// cached view can be older than the hook event that made the provider ask
	// for a permission.
	status, err := s.observation.FreshTaskStatus(ctx, task.ID)
	if err != nil {
		return promptRunWindowState{}, fmt.Errorf("read task status: %w", err)
	}
	if status == nil {
		return promptRunWindowState{busy: "status unknown"}, nil
	}
	switch status.Phase {
	case TaskStatusPhaseWorking, TaskStatusPhaseWorkingInBackground:
		interrupted, err := s.lastTurnInterrupted(ctx, task, providerClient)
		if err != nil {
			return promptRunWindowState{}, err
		}
		if !interrupted {
			return promptRunWindowState{busy: "still working"}, nil
		}
	case TaskStatusPhaseWaitingForInput:
		if status.RawEventName == HookEventNotification || status.RawEventName == HookEventPermissionRequest {
			return promptRunWindowState{busy: "waiting on a permission prompt"}, nil
		}
	case TaskStatusPhaseStarting:
	default:
		return promptRunWindowState{busy: fmt.Sprintf("status %s unknown", status.Phase)}, nil
	}
	return promptRunWindowState{closeFirst: true}, nil
}

// lastTurnInterrupted asks the provider whether the user interrupted the last
// turn of the Task's latest session, which no hook reports. Without a session
// to ask about, the turn counts as running.
func (s *service) lastTurnInterrupted(ctx context.Context, task *Task, providerClient ProviderClient) (bool, error) {
	sessionID, err := s.latestProviderSessionID(ctx, task)
	if err != nil || sessionID == "" {
		return false, err
	}
	session, err := s.recordedProviderSession(ctx, task, sessionID)
	if err != nil {
		return false, err
	}
	interrupted, err := providerClient.LastTurnInterrupted(ctx, task, session)
	if err != nil {
		return false, fmt.Errorf("check whether the last turn was interrupted: %w", err)
	}
	return interrupted, nil
}

// recordedProviderSession is the Task's provider session as rig recorded it
// from the provider's hooks, which carry where the provider keeps its
// transcript, or just the session's identity when none was recorded.
func (s *service) recordedProviderSession(
	ctx context.Context,
	task *Task,
	sessionID string,
) (TaskProviderSession, error) {
	session := TaskProviderSession{TaskID: task.ID, Provider: task.Provider, ProviderSessionID: sessionID}
	recorded, err := s.tasks.ListTaskProviderSessions(ctx, task.ID)
	if err != nil {
		return TaskProviderSession{}, fmt.Errorf("list provider sessions: %w", err)
	}
	for _, candidate := range latestProviderSessionsByID(recorded) {
		if candidate.Provider == task.Provider && candidate.ProviderSessionID == sessionID {
			return candidate, nil
		}
	}
	return session, nil
}

// latestProviderSessionID is the Task's latest provider session, or "" when it
// has none or the latest one belongs to another provider than the Task's.
func (s *service) latestProviderSessionID(ctx context.Context, task *Task) (string, error) {
	metadata, err := s.tasks.LatestTaskResumeMetadata(ctx, task.ID)
	if err != nil {
		return "", fmt.Errorf("read latest session: %w", err)
	}
	if metadata == nil || metadata.Provider != task.Provider {
		return "", nil
	}
	return strings.TrimSpace(metadata.SessionID), nil
}

// busyPaneReason explains why a pane without the Task's provider cannot be
// launched into, or is empty when it can: only a login shell waiting at its
// prompt may be in it, and the process evidence must say so, or rig would type
// the provider's command into a dev server, an editor or a script that reads
// its input. A shell at its prompt has no child process, so anything it
// started counts as running, a script run by "sh" or a nested "bash" as much
// as "vim".
func busyPaneReason(runtime TaskSessionRuntimeState) string {
	shell := ""
	for _, command := range runtime.ActiveCommands {
		name := paneCommandName(command)
		if name == "" {
			continue
		}
		if !isLoginShell(name) {
			return "pane is running " + name
		}
		shell = name
	}
	for _, command := range runtime.ChildCommands {
		if name := paneCommandName(command); name != "" {
			return "pane is running " + name
		}
	}
	if runtime.ChildProcessEvidenceUnavailable {
		if shell == "" {
			return "pane state unknown"
		}
		return "pane is running " + shell + " and what it started cannot be seen"
	}
	return ""
}

// paneCommandName is the name of a command as the process list shows it: its base
// name, without the "-" that marks a login shell.
func paneCommandName(command string) string {
	name := strings.TrimPrefix(filepath.Base(strings.TrimSpace(command)), "-")
	if name == "." {
		return ""
	}
	return name
}

// isLoginShell reports whether a command name, without any leading "-", is an
// interactive shell the pane starts out with.
func isLoginShell(name string) bool {
	switch name {
	case "zsh", "bash", "fish", "sh":
		return true
	default:
		return false
	}
}

// promptRunScript is the script the prompt wrapper runs. promptFile is its $0
// and the provider's argv its "$@". "$(...)" would drop the newlines the
// prompt ends with, so an "x" is added after them and taken off again. A prompt
// file that cannot be read stops the script, rather than starting the provider
// on an empty prompt.
const promptRunScript = `p=$(cat "$0" && printf x) || exit; exec "$@" -- "${p%x}"`

// promptRunCommand wraps a provider's argv so that the prompt in promptFile is
// its last argument. The prompt is read by the shell that runs the command,
// not typed or quoted by rig, so it may hold anything; "--" keeps one that
// starts with "-" from being read as a flag.
func promptRunCommand(promptFile string, argv []string) []string {
	command := []string{"sh", "-c", promptRunScript, promptFile}
	return append(command, argv...)
}

// shortSessionID abbreviates a provider session ID for display.
func shortSessionID(sessionID string) string {
	const shown = 4
	if len([]rune(sessionID)) <= shown {
		return sessionID
	}
	return string([]rune(sessionID)[:shown]) + "…"
}
