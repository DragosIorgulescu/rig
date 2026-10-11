package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// maxPromptRunPromptBytes caps a prompt. A launch hands it to the provider as
// one argument of its command, which the system refuses to start when too
// long: Linux allows 128 KiB for one argument, macOS about 1 MiB for the whole
// command and environment. The cap leaves room under both, and holds for a
// prompt sent to a running session too, so a run does not depend on which of
// the two it turns out to be.
const maxPromptRunPromptBytes = 96 * 1024

const (
	// defaultPromptRunRecheckInterval is how long a run waits between its two
	// looks at the window.
	defaultPromptRunRecheckInterval = time.Second
	// defaultPromptRunTakenWait is how long a run waits for a running session
	// to take the prompt it was sent.
	defaultPromptRunTakenWait = 15 * time.Second
	// defaultPromptRunTakenPoll paces that wait.
	defaultPromptRunTakenPoll = 250 * time.Millisecond
)

// promptRunTiming is the pacing of a prompt run. A zero value keeps the
// default; tests and the sandbox shorten them.
type promptRunTiming struct {
	// recheckInterval separates the two looks at the window that decide
	// whether the session is busy: a turn's status line can be off screen
	// for a moment between two of its steps.
	recheckInterval time.Duration
	// takenWait bounds the wait for a running session to take a prompt sent
	// to it, and takenPoll paces it.
	takenWait time.Duration
	takenPoll time.Duration
}

func (t promptRunTiming) recheck() time.Duration {
	return cmp.Or(t.recheckInterval, defaultPromptRunRecheckInterval)
}

func (t promptRunTiming) takeWait() time.Duration {
	return cmp.Or(t.takenWait, defaultPromptRunTakenWait)
}

func (t promptRunTiming) takePoll() time.Duration {
	return cmp.Or(t.takenPoll, defaultPromptRunTakenPoll)
}

// RunTaskPrompt gives the Task's provider a prompt, or reports why it did not.
// It is what a scheduled run does.
//
// Only a session that is busy is left alone: the Task must be ready and on the
// current list, no other Task operation may be running, and the session must
// not be working on a turn or blocked on a permission prompt. Whether the
// user is looking at the window does not matter. A skip is a normal result.
//
// With PromptRunSessionSame, a provider that is running in the Task's window
// is sent the prompt, and otherwise it is started with the prompt, resuming
// the Task's last session when there is one. With PromptRunSessionNew, whatever
// session is in the window is ended and a fresh one is started with the prompt.
//
// Errors are for input that cannot be a run, a Task that cannot be read, and a
// cancelled context. A run that goes wrong with a cause the user can act on, a
// launch that fails or a session that does not take the prompt, is a result
// with the PromptRunFailed outcome.
//
// The Task's operation lock is held until the provider has taken the prompt,
// not only until it was sent or its command typed: a second run, or any other
// operation, that came in while the provider was still starting would see a
// bare shell, or a status left by the session before, and type over the run.
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
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			result = promptRunFailed(err.Error())
		}
		return nil
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
	if err := checkPromptRunPrompt(run.prompt); err != nil {
		return promptRun{}, err
	}
	sessionMode, err := parsePromptRunSessionMode(run.sessionMode)
	if err != nil {
		return promptRun{}, err
	}
	run.sessionMode = sessionMode
	mode, err := ParseTaskPermissionMode(string(input.PermissionMode))
	if err != nil {
		return promptRun{}, err
	}
	run.permissionMode = mode
	return run, nil
}

// checkPromptRunPrompt reports why a prompt cannot be a prompt run's.
func checkPromptRunPrompt(prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("prompt is required")
	}
	if len(prompt) > maxPromptRunPromptBytes {
		return fmt.Errorf("prompt is too long: %d bytes, and a prompt run takes at most %d",
			len(prompt), maxPromptRunPromptBytes)
	}
	return nil
}

// parsePromptRunSessionMode returns the session mode a name stands for; an
// empty name is PromptRunSessionNew.
func parsePromptRunSessionMode(mode PromptRunSessionMode) (PromptRunSessionMode, error) {
	switch mode {
	case "":
		return PromptRunSessionNew, nil
	case PromptRunSessionNew, PromptRunSessionSame:
		return mode, nil
	default:
		return "", fmt.Errorf("unknown session mode %q: use %s or %s", mode, PromptRunSessionNew, PromptRunSessionSame)
	}
}

func promptRunSkipped(reason string) RunTaskPromptResult {
	return RunTaskPromptResult{Outcome: PromptRunSkipped, Detail: reason}
}

func promptRunFailed(detail string) RunTaskPromptResult {
	return RunTaskPromptResult{Outcome: PromptRunFailed, Detail: detail}
}

func promptRunStarted(detail string, startedAt time.Time) RunTaskPromptResult {
	return RunTaskPromptResult{Outcome: PromptRunStarted, Detail: detail, StartedAt: startedAt}
}

// runTaskPrompt is RunTaskPrompt once the Task's operation lock is held. An
// error it returns is a run that failed.
func (s *service) runTaskPrompt(ctx context.Context, task *Task, run promptRun) (RunTaskPromptResult, error) {
	_, providerClient, err := s.launcher.resolveProvider(ctx, task.Provider)
	if err != nil {
		return RunTaskPromptResult{}, err
	}
	if s.promptFiles == nil {
		return RunTaskPromptResult{}, fmt.Errorf("prompt file store not configured")
	}

	runtime, err := s.tmuxSession.InspectTaskSession(ctx, task)
	if err != nil {
		return RunTaskPromptResult{}, fmt.Errorf("inspect task session: %w", err)
	}
	running := taskSessionRunningProvider(runtime, providerClient.TaskSessionCommandName())
	active, reason, err := s.sessionActive(ctx, task, providerClient, running)
	if err != nil {
		return RunTaskPromptResult{}, err
	}
	if active {
		return promptRunSkipped(reason), nil
	}

	if running && run.sessionMode == PromptRunSessionSame {
		return s.sendPromptToSession(ctx, task, providerClient, run)
	}
	// Whatever is running is idle, and a fresh session replaces it. So does
	// anything else in the window that is not a shell: the provider's command
	// is typed into the window, and would be input to an editor or a server.
	return s.startPromptRunSession(ctx, task, providerClient, run, running || runsMoreThanAShell(runtime))
}

// shellCommands are the programs a window runs when nothing else does.
var shellCommands = []string{"sh", "bash", "zsh", "fish", "dash", "ksh", "tcsh", "csh", "nu", "elvish", "xonsh"}

// runsMoreThanAShell reports whether the window runs a process that is not a
// shell, something the window's user left there, or a session that was not
// made by this Task's provider. A window that does not exist, or whose
// processes cannot be told, runs nothing known.
func runsMoreThanAShell(runtime TaskSessionRuntimeState) bool {
	if !runtime.Exists {
		return false
	}
	for _, command := range runtime.ActiveCommands {
		command = strings.TrimSpace(command)
		if command == "" {
			continue
		}
		// A login shell's name starts with "-".
		if !slices.Contains(shellCommands, strings.TrimPrefix(filepath.Base(command), "-")) {
			return true
		}
	}
	return false
}

// Why a session is active, as a skipped run records it.
const (
	sessionBusyReason       = "session busy"
	sessionPermissionReason = "waiting on a permission prompt"
)

// sessionActive reports whether the Task's session is busy, and why. A session
// is busy when it is working on a turn or blocked on a permission prompt, and
// in no other case: that the user is looking at the window, or that a hook
// last said the provider was working or asking, is not enough.
//
// The provider's hook status can be wrong in both directions. It stays at
// working after the user interrupts a turn, and at a permission prompt after
// the user dismisses it, since no hook reports either, so the window is read: a
// turn that runs shows the provider's busy markers, and a prompt that waits
// shows its permission markers. A hook status that asks for a permission is
// the only reason to look for those. The window is read twice, an interval
// apart, since a marker can be off screen for a moment. The hook status
// decides only when the window cannot be read.
//
// A provider that is not running has no session to be busy.
func (s *service) sessionActive(
	ctx context.Context,
	task *Task,
	providerClient ProviderClient,
	running bool,
) (bool, string, error) {
	if !running {
		return false, "", nil
	}

	// The status is read afresh: the cached view can be older than the hook
	// event that made the provider ask for a permission.
	status, err := s.observation.FreshTaskStatus(ctx, task.ID)
	if err != nil {
		return false, "", fmt.Errorf("read task status: %w", err)
	}
	asksPermission := statusAsksPermission(status)

	reason, err := s.windowActivity(ctx, task, providerClient, asksPermission)
	if err == nil && reason == "" {
		if err = sleepContext(ctx, s.promptRunTiming.recheck()); err != nil {
			return false, "", err
		}
		reason, err = s.windowActivity(ctx, task, providerClient, asksPermission)
	}
	switch {
	case err != nil:
		// The window cannot be read, so the hook status is all there is.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, "", ctxErr
		}
		switch {
		case asksPermission:
			return true, sessionPermissionReason, nil
		case status != nil &&
			(status.Phase == TaskStatusPhaseWorking || status.Phase == TaskStatusPhaseWorkingInBackground):
			return true, sessionBusyReason, nil
		default:
			return false, "", nil
		}
	case reason != "":
		return true, reason, nil
	default:
		return false, "", nil
	}
}

// statusAsksPermission reports whether a status says the provider asked the
// user for a permission and has not been answered.
func statusAsksPermission(status *TaskStatusUpdate) bool {
	return status != nil && status.Phase == TaskStatusPhaseWaitingForInput &&
		(status.RawEventName == HookEventNotification || status.RawEventName == HookEventPermissionRequest)
}

// windowActivity reads the Task's window once and says why it shows an active
// session, or "" when it does not. A permission prompt is only looked for when
// the hook status asks for one, and comes before a busy turn since it is what
// the session is blocked on.
func (s *service) windowActivity(
	ctx context.Context,
	task *Task,
	providerClient ProviderClient,
	asksPermission bool,
) (string, error) {
	if asksPermission {
		shows, err := s.tmuxSession.TaskPaneShows(ctx, task, providerClient.PermissionMarkers())
		if err != nil {
			return "", err
		}
		if shows {
			return sessionPermissionReason, nil
		}
	}
	shows, err := s.tmuxSession.TaskPaneShows(ctx, task, providerClient.BusyMarkers())
	if err != nil {
		return "", err
	}
	if shows {
		return sessionBusyReason, nil
	}
	return "", nil
}

// sendPromptToSession sends the prompt to the provider already running in the
// Task's window and waits for it to take it. A prompt the session ignores,
// because it was not ready for input or the paste went elsewhere, is a failed
// run rather than a successful one nobody can see failing.
func (s *service) sendPromptToSession(
	ctx context.Context,
	task *Task,
	providerClient ProviderClient,
	run promptRun,
) (RunTaskPromptResult, error) {
	sentAt := time.Now()
	if err := s.tmuxSession.SubmitTaskPrompt(ctx, task, run.prompt); err != nil {
		return RunTaskPromptResult{}, fmt.Errorf("send the prompt to the session: %w", err)
	}

	taken, err := s.awaitPromptSent(ctx, task, providerClient, sentAt)
	if err != nil {
		return RunTaskPromptResult{}, err
	}
	if !taken {
		return promptRunFailed("the session didn't take the prompt"), nil
	}
	return promptRunStarted("sent to the running session", sentAt), nil
}

// awaitPromptSent waits until the session shows it took the prompt sent at
// sentAt, within the run's bound, and reports whether it did. It did when a
// status newer than the send says the provider is working on a turn, or has
// finished one, or when the window shows a busy marker.
func (s *service) awaitPromptSent(
	ctx context.Context,
	task *Task,
	providerClient ProviderClient,
	sentAt time.Time,
) (bool, error) {
	timing := s.promptRunTiming
	deadline := time.Now().Add(timing.takeWait())
	for {
		status, err := s.tasks.LatestTaskStatus(ctx, task.ID)
		if err == nil && promptSent(status, sentAt) {
			return true, nil
		}
		// A window that cannot be read says nothing; the status may still.
		if shows, err := s.tmuxSession.TaskPaneShows(ctx, task, providerClient.BusyMarkers()); err == nil && shows {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, ctx.Err()
		}
		if err := sleepContext(ctx, timing.takePoll()); err != nil {
			return false, err
		}
	}
}

// promptSent reports whether a status says the session took a prompt sent at
// sentAt: it was reported after the send, and the provider is working on a
// turn or finished one. A turn that ends before the status is looked at again
// leaves only the Stop that ended it, which a session that was idle before the
// send cannot have reported for anything else.
func promptSent(status *TaskStatusUpdate, sentAt time.Time) bool {
	if status == nil || !status.ObservedAt.After(sentAt) {
		return false
	}
	switch status.Phase {
	case TaskStatusPhaseWorking, TaskStatusPhaseWorkingInBackground:
		return true
	case TaskStatusPhaseWaitingForInput:
		return status.RawEventName == HookEventStop || status.RawEventName == HookEventStopFailure
	default:
		return false
	}
}

// startPromptRunSession starts the Task's provider on the prompt in its window.
// replace says something is running there that is not a shell, an idle session
// or anything else, which is ended first.
func (s *service) startPromptRunSession(
	ctx context.Context,
	task *Task,
	providerClient ProviderClient,
	run promptRun,
	replace bool,
) (RunTaskPromptResult, error) {
	resumeSessionID := ""
	if run.sessionMode == PromptRunSessionSame {
		var err error
		resumeSessionID, err = s.latestProviderSessionID(ctx, task)
		if err != nil {
			return RunTaskPromptResult{}, err
		}
	}

	// Everything that can fail without touching the window comes first, so a
	// failed run does not leave an idle session ended for nothing.
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

	// An idle session is ended rather than typed into, so the run starts
	// clean, with its own model, effort and permission mode. Anything else in
	// the way goes too: the command is typed into a shell or into nothing.
	if replace {
		if err := s.tmuxSession.RespawnTaskPane(ctx, task); err != nil {
			return RunTaskPromptResult{}, fmt.Errorf("end the idle session: %w", err)
		}
	}

	// The Task record stays as it was: a run must not change the provider,
	// model or effort the user's own sessions start with.
	launchedAt := time.Now()
	if err := s.tmuxSession.StartTaskSession(ctx, task, launch); err != nil {
		return RunTaskPromptResult{}, fmt.Errorf("start task session: %w", err)
	}
	if !s.awaitPromptRunStarted(ctx, task, launchedAt) {
		if exited := s.providerExitedAtLaunch(ctx, task, providerClient); exited != "" {
			return promptRunFailed(exited), nil
		}
	}

	switch {
	case resumeSessionID != "":
		return promptRunStarted("resumed "+shortSessionID(resumeSessionID), launchedAt), nil
	case run.sessionMode == PromptRunSessionSame:
		return promptRunStarted("no previous session, started fresh", launchedAt), nil
	default:
		return promptRunStarted("started fresh", launchedAt), nil
	}
}

// awaitPromptRunStarted waits, within the launcher's bound for a provider to
// report its session, until the provider launched at launchedAt has taken the
// prompt, and reports whether it did. A provider that never reports, such as
// one held up by a startup dialog, is left to start in its own time once the
// bound has run out.
func (s *service) awaitPromptRunStarted(ctx context.Context, task *Task, launchedAt time.Time) bool {
	wait := s.launcher.providerSessionWait
	if wait <= 0 {
		return false
	}
	interval := cmp.Or(s.launcher.sessionPollInterval, 250*time.Millisecond)
	deadline := time.Now().Add(wait)
	for {
		status, err := s.tasks.LatestTaskStatus(ctx, task.ID)
		if err == nil && promptRunTaken(status, launchedAt) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		if sleepContext(ctx, interval) != nil {
			return false
		}
	}
}

// launchExitMessageLines is how many of the last lines of the window, above the
// shell's prompt, say why a provider that exited at launch did.
const launchExitMessageLines = 2

// shellPromptEndings are what the last character of a shell's prompt line
// usually is.
const shellPromptEndings = "%$#>❯"

// providerExitedAtLaunch says why the run failed when the provider it started
// is gone from the window by the time the launcher's bound for it to report
// has run out, and returns "" when it is there, or cannot be told to be gone.
// Such a provider, held up by an expired login, a flag it refuses or a binary
// that is missing, says its reason on the window and sends no hook, so
// nothing else would ever end the run.
//
// It is only asked once the whole bound has passed: a provider that was only
// just typed into the window is not yet a process, and would read as gone.
func (s *service) providerExitedAtLaunch(ctx context.Context, task *Task, providerClient ProviderClient) string {
	if ctx.Err() != nil || s.launcher.providerSessionWait <= 0 {
		return ""
	}
	runtime, err := s.tmuxSession.InspectTaskSession(ctx, task)
	if err != nil || (runtime.Exists && runtime.ChildProcessEvidenceUnavailable) {
		return ""
	}
	if taskSessionRunningProvider(runtime, providerClient.TaskSessionCommandName()) {
		return ""
	}

	detail := "the provider exited at launch"
	screen, err := s.tmuxSession.CaptureTaskSession(ctx, task)
	if err != nil {
		return detail
	}
	var lastLines []string
	lines := strings.Split(screen, "\n")
	atBottom := true
	for i := len(lines) - 1; i >= 0 && len(lastLines) < launchExitMessageLines; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		// The shell's prompt, drawn again once the provider has gone, says
		// nothing of why it went.
		last, _ := utf8.DecodeLastRuneInString(line)
		if atBottom && strings.ContainsRune(shellPromptEndings, last) {
			atBottom = false
			continue
		}
		atBottom = false
		lastLines = append([]string{line}, lastLines...)
	}
	if len(lastLines) == 0 {
		return detail
	}
	return compactDetail(detail+": "+strings.Join(lastLines, " / "), maxRunDetailRunes)
}

// promptRunTaken reports whether a status says the provider launched at
// launchedAt has taken the prompt: it is working, or has finished its turn,
// after the launch. A session start does not say so, since a provider reports
// it before it submits the prompt, and a resumed session reads as idle in
// between; a run that looked then would end the provider it just started.
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

// sleepContext waits for d, or returns the context's error when it ends first.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
