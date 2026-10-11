package tmux

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BaronBonet/rig/internal/core"
	"github.com/BaronBonet/rig/internal/pkg/subprocess"
)

const (
	promptSubmitDelay      = 500 * time.Millisecond
	promptInputSettleDelay = 500 * time.Millisecond
	taskWindowName         = "task"
	// returnKeyEnvVar names the tmux prefix key that returns to rig from a
	// task session; "none" leaves tmux's key bindings alone.
	returnKeyEnvVar  = "RIG_TMUX_RETURN_KEY"
	defaultReturnKey = "b"
	// rigSessionOption, set on a task's session, names the rig the return key
	// leads to, and on a rig's own session names that rig; rigLastSessionOption
	// is the fallback for a session without one. It is not rigSessionOption's
	// global value, which every session would read as its own.
	rigSessionOption     = "@rig_session"
	rigLastSessionOption = "@rig_last_session"
	// rigFolderOption, set on a rig's own session, is the folder it runs for.
	rigFolderOption = "@rig_folder"
	// promptPasteSettleDelay is the pause between pasting a prompt and the
	// Enter that submits it. A long paste takes the provider a moment to
	// take in, and an Enter sent before it has would be part of the paste.
	promptPasteSettleDelay = time.Second
	// paneTailLines is how many of the last non-empty lines of a pane
	// TaskPaneShows looks at: the provider's status line sits at the bottom of
	// its screen, and what is above it is the conversation, which may well
	// mention the markers.
	paneTailLines = 10
	// defaultRespawnTimeout bounds how long respawning a task pane waits for
	// the processes that ran in it to exit, and defaultRespawnSettle is the
	// pause after they have, so the old provider's SessionEnd hook cannot land
	// after the hooks of the session that replaces it.
	defaultRespawnTimeout = 15 * time.Second
	defaultRespawnSettle  = 2 * time.Second
	respawnPollInterval   = 100 * time.Millisecond
)

type repository struct {
	runner subprocess.Runner
	now    func() time.Time
	sleep  func(time.Duration)
	getenv func(string) string
	// respawnTimeout and respawnSettle override the defaults of
	// RespawnTaskPane's wait; zero keeps the default.
	respawnTimeout time.Duration
	respawnSettle  time.Duration
}

func New(runner subprocess.Runner) core.TmuxSessionClient {
	return &repository{
		runner: runner,
		now:    time.Now,
		sleep:  time.Sleep,
		getenv: os.Getenv,
	}
}

func (r *repository) HealthCheck(ctx context.Context) error {
	_, err := r.runner.Run(ctx, "", "tmux", "-V")
	return err
}

func (r *repository) StartTaskSession(ctx context.Context, task *core.Task, launch core.TaskSessionLaunchSpec) error {
	window := windowOf(task)
	// Provider switches launch into an existing idle session; only reconnects
	// and fresh tasks need a new session created. A child task's window is
	// added to its parent's session, which may already exist.
	alreadyExists := r.sessionExists(ctx, task.TmuxSession)
	session, cwd := task.TmuxSession, task.WorktreePath
	switch {
	case !alreadyExists:
		if err := r.createSession(ctx, session, window, cwd, task, !task.IsScheduled()); err != nil {
			return err
		}
	case !r.windowExists(ctx, session, window):
		if err := r.createWindow(ctx, session, window, cwd, task.ID, task.ProviderEnv); err != nil {
			return err
		}
	}

	if len(launch.Command) == 0 {
		return nil
	}

	if err := r.sendKeysToWindow(ctx, task.TmuxSession, window, launch.Command); err != nil {
		if alreadyExists {
			return err
		}
		return r.cleanupStartedSession(ctx, task.TmuxSession, err)
	}

	return nil
}

// windowOf is the window of the task's tmux session its provider runs in.
func windowOf(task *core.Task) string {
	if task == nil {
		return taskWindowName
	}
	if window := strings.TrimSpace(task.TmuxWindow); window != "" {
		return window
	}
	return taskWindowName
}

// PrefillTaskSession types the launch spec's PrefillInput into the task
// window once the provider shows its ready marker, without submitting it. A
// session that fails to take the prompt is left running: the provider is up
// and the prompt is still on the task record.
func (r *repository) PrefillTaskSession(
	ctx context.Context,
	task *core.Task,
	launch core.TaskSessionLaunchSpec,
) error {
	if len(launch.PrefillInput) == 0 {
		return nil
	}

	window := windowOf(task)
	if err := r.waitForPrompt(ctx, task.TmuxSession, window, launch.ReadyMarker, launch.Command); err != nil {
		return err
	}
	r.sleep(promptInputSettleDelay)

	return r.typeInWindow(ctx, task.TmuxSession, window, launch.PrefillInput)
}

// SubmitTaskInput types text literally into the task's window and presses
// Enter after a short settle, so a provider's command suggestion has appeared
// and Enter runs the command rather than only accepting the suggestion.
func (r *repository) SubmitTaskInput(ctx context.Context, task *core.Task, text string) error {
	if task == nil || strings.TrimSpace(task.TmuxSession) == "" {
		return fmt.Errorf("task tmux session is required")
	}
	target := exactWindowTarget(task.TmuxSession, windowOf(task))
	if _, err := r.runner.Run(ctx, "", "tmux", "send-keys", "-t", target, "-l", text); err != nil {
		return err
	}
	r.sleep(promptInputSettleDelay)
	_, err := r.runner.Run(ctx, "", "tmux", "send-keys", "-t", target, "Enter")
	return err
}

func (r *repository) sessionExists(ctx context.Context, sessionName string) bool {
	_, err := r.runner.Run(
		ctx,
		"",
		"tmux",
		"has-session",
		"-t",
		exactSessionTarget(sessionName),
	)
	return err == nil
}

func (r *repository) AttachTaskSession(ctx context.Context, task *core.Task) error {
	if task == nil || strings.TrimSpace(task.TmuxSession) == "" {
		return fmt.Errorf("task tmux session is required")
	}

	insideTmux := strings.TrimSpace(r.env("TMUX")) != ""
	command := "attach-session"
	if insideTmux {
		command = "switch-client"
	}
	// Attaching lands in the task's own window, not the one last shown in
	// its session, which its children's windows share. A window that is gone
	// reports the session missing, and the task is reconnected into it.
	target := exactWindowTarget(task.TmuxSession, windowOf(task))

	result, err := r.runner.Run(
		ctx,
		"",
		"tmux",
		command,
		"-t",
		target,
	)
	if isMissingSessionError(err, result) {
		return core.ErrTaskSessionNotFound
	}
	if err == nil && insideTmux {
		r.rememberReturnToRig(ctx, task.TmuxSession)
	}
	return err
}

// rememberReturnToRig lets one key bring the user back from a task to the
// rig that opened it: the session rig runs in is recorded on the task's
// session, and as the fallback for sessions no rig has claimed, and the key
// switches to whichever applies. Failures only lose the shortcut.
func (r *repository) rememberReturnToRig(ctx context.Context, taskSession string) {
	key, rigSession, ok := r.returnKeyAndRigSession(ctx)
	if !ok {
		return
	}
	r.setSessionOption(ctx, taskSession, rigSessionOption, rigSession)
	_, _ = r.runner.Run(ctx, "", "tmux", "set-option", "-g", rigLastSessionOption, rigSession)
	r.bindReturnKey(ctx, key, rigSession)
}

// ClaimTaskSessions points the return key of folder's tasks at this rig. A
// task session is taken over when it names no rig, a rig that is gone, or one
// whose folder does not hold the task or holds it less closely, so a task
// leads back to its own folder's rig whichever rig opened it last. The rig's
// folder is published on its own session for other rigs to compare.
func (r *repository) ClaimTaskSessions(ctx context.Context, folder string, tasks []*core.Task) error {
	folder = strings.TrimSpace(folder)
	if !filepath.IsAbs(folder) {
		return nil
	}
	folder = filepath.Clean(folder)
	key, rigSession, ok := r.returnKeyAndRigSession(ctx)
	if !ok {
		return nil
	}
	r.setSessionOption(ctx, rigSession, rigFolderOption, folder)
	r.bindReturnKey(ctx, key, rigSession)

	result, err := r.runner.Run(ctx, "", "tmux", "list-sessions", "-F",
		"#{session_name}\t#{"+rigSessionOption+"}\t#{"+rigFolderOption+"}")
	if err != nil {
		return err
	}
	recordedRig := make(map[string]string)
	rigFolder := make(map[string]string)
	for _, line := range strings.Split(result.Stdout, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[0] == "" {
			continue
		}
		recordedRig[fields[0]] = fields[1]
		rigFolder[fields[0]] = fields[2]
	}

	for _, task := range tasks {
		if task == nil || !core.TaskInFolder(task, folder) {
			continue
		}
		session := strings.TrimSpace(task.TmuxSession)
		recorded, live := recordedRig[session]
		if !live {
			continue
		}
		// This rig's own folder is listed too, so a session already pointing
		// here is kept as one an equally close rig holds.
		if other := rigFolder[recorded]; other != "" && core.TaskInFolder(task, other) && len(other) >= len(folder) {
			continue
		}
		r.setSessionOption(ctx, session, rigSessionOption, rigSession)
		recordedRig[session] = rigSession
	}
	return nil
}

// returnKeyAndRigSession reports the return key and the tmux session this
// rig runs in, or false when the key is turned off or rig is not in tmux.
func (r *repository) returnKeyAndRigSession(ctx context.Context) (string, string, bool) {
	key := strings.TrimSpace(r.env(returnKeyEnvVar))
	if key == "" {
		key = defaultReturnKey
	}
	if key == "none" || strings.TrimSpace(r.env("TMUX")) == "" {
		return "", "", false
	}
	args := []string{"display-message", "-p"}
	if pane := strings.TrimSpace(r.env("TMUX_PANE")); pane != "" {
		args = append(args, "-t", pane)
	}
	result, err := r.runner.Run(ctx, "", "tmux", append(args, "#{session_name}")...)
	rigSession := strings.TrimSpace(result.Stdout)
	if err != nil || rigSession == "" {
		return "", "", false
	}
	return key, rigSession, true
}

func (r *repository) setSessionOption(ctx context.Context, session string, option string, value string) {
	_, _ = r.runner.Run(ctx, "", "tmux", "set-option", "-t", exactSessionTarget(session)+":", option, value)
}

// bindReturnKey binds the key to the session's own rig, or the fallback. The
// rig's own session names itself, so the key stays put there instead of
// falling back to whichever rig opened a task last. The global
// rigSessionOption earlier builds set is cleared, or every session would read
// it as its own.
func (r *repository) bindReturnKey(ctx context.Context, key string, rigSession string) {
	r.setSessionOption(ctx, rigSession, rigSessionOption, rigSession)
	_, _ = r.runner.Run(ctx, "", "tmux", "set-option", "-gu", rigSessionOption)
	_, _ = r.runner.Run(ctx, "", "tmux", "bind-key", "-T", "prefix", key, "run-shell", "-C",
		"switch-client -t '=#{?"+rigSessionOption+",#{"+rigSessionOption+"},#{"+rigLastSessionOption+"}}'")
}

func (r *repository) env(key string) string {
	if r.getenv == nil {
		return ""
	}
	return r.getenv(key)
}

func (r *repository) InspectTaskSession(ctx context.Context, task *core.Task) (core.TaskSessionRuntimeState, error) {
	if task == nil || strings.TrimSpace(task.TmuxSession) == "" {
		return core.TaskSessionRuntimeState{}, nil
	}

	result, err := r.runner.Run(
		ctx,
		"",
		"tmux",
		"list-panes",
		"-t",
		exactWindowTarget(task.TmuxSession, windowOf(task)),
		"-F",
		"#{pane_current_command}\t#{pane_pid}",
	)
	// With no tmux server there is no session either, as after a reboot.
	if isMissingSessionError(err, result) || isNoTmuxServerError(err, result) {
		return core.TaskSessionRuntimeState{}, nil
	}
	if err != nil {
		return core.TaskSessionRuntimeState{}, err
	}

	commands, panePIDs := panesFromTmuxOutput(result.Stdout)
	// pane_current_command reports the foreground process title, which some
	// provider CLIs rewrite (Claude Code sets it to its version string), so the
	// comm names of each pane's child processes are reported as well.
	children, childEvidenceAvailable := r.paneChildProcesses(ctx, panePIDs)

	return core.TaskSessionRuntimeState{
		Exists:                          true,
		ActiveCommands:                  append(commands, processCommands(children)...),
		CommandStartedAt:                processCommandStartTimes(children),
		ChildProcessEvidenceUnavailable: !childEvidenceAvailable,
	}, nil
}

func (r *repository) CaptureTaskSession(ctx context.Context, task *core.Task) (string, error) {
	if task == nil || strings.TrimSpace(task.TmuxSession) == "" {
		return "", fmt.Errorf("task tmux session is required")
	}
	return r.capturePaneContent(ctx, task.TmuxSession, windowOf(task))
}

func (r *repository) InspectTaskSessions(
	ctx context.Context,
	tasks []*core.Task,
) (map[string]core.TaskSessionRuntimeState, error) {
	states := make(map[string]core.TaskSessionRuntimeState, len(tasks))
	// Tasks are keyed by session and window: a child task shares its
	// parent's session and has a window of its own there.
	taskIDsBySession := make(map[string][]string, len(tasks))
	for _, task := range tasks {
		if task == nil {
			continue
		}
		taskID := strings.TrimSpace(task.ID)
		if taskID == "" {
			continue
		}
		states[taskID] = core.TaskSessionRuntimeState{}
		session := normalizedSessionName(strings.TrimSpace(task.TmuxSession))
		if session != "" {
			key := session + ":" + windowOf(task)
			taskIDsBySession[key] = append(taskIDsBySession[key], taskID)
		}
	}

	result, err := r.runner.Run(
		ctx,
		"",
		"tmux",
		"list-panes",
		"-a",
		"-F",
		"#{session_name}\t#{window_name}\t#{pane_current_command}\t#{pane_pid}",
	)
	if isMissingSessionError(err, result) || isNoTmuxServerError(err, result) {
		return states, nil
	}
	if err != nil {
		return nil, err
	}

	inventory, complete := parsePaneInventory(result.Stdout)
	if !complete {
		return nil, fmt.Errorf("parse tmux pane inventory: incomplete output")
	}
	commandsBySession := make(map[string][]string)
	panePIDsBySession := make(map[string][]string)
	seenSessions := make(map[string]bool)
	allPanePIDsPresent := true
	for _, pane := range inventory {
		key := pane.session + ":" + pane.window
		if _, tracked := taskIDsBySession[key]; !tracked {
			continue
		}
		seenSessions[key] = true
		if pane.command != "" {
			commandsBySession[key] = append(commandsBySession[key], pane.command)
		}
		if pane.pid != "" {
			panePIDsBySession[key] = append(panePIDsBySession[key], pane.pid)
		} else {
			allPanePIDsPresent = false
		}
	}

	allPanePIDs := make([]string, 0)
	for _, panePIDs := range panePIDsBySession {
		allPanePIDs = append(allPanePIDs, panePIDs...)
	}
	childProcessesByPID, processInventoryAvailable := r.childProcessesByParentPID(ctx, allPanePIDs)
	childEvidenceAvailable := allPanePIDsPresent && processInventoryAvailable

	for session, taskIDs := range taskIDsBySession {
		commands := commandsBySession[session]
		exists := seenSessions[session]
		if !exists {
			continue
		}

		var children []paneChildProcess
		for _, panePID := range panePIDsBySession[session] {
			children = append(children, childProcessesByPID[panePID]...)
		}
		activeCommands := append(append([]string(nil), commands...), processCommands(children)...)
		for _, taskID := range taskIDs {
			states[taskID] = core.TaskSessionRuntimeState{
				Exists:                          true,
				ActiveCommands:                  append([]string(nil), activeCommands...),
				CommandStartedAt:                processCommandStartTimes(children),
				ChildProcessEvidenceUnavailable: !childEvidenceAvailable,
			}
		}
	}

	return states, nil
}

// paneChildProcess is one direct child of a pane's root process, typically the
// CLI the pane shell is running. startedAt is zero when ps reported no
// parseable elapsed time.
type paneChildProcess struct {
	command   string
	startedAt time.Time
}

// paneChildProcesses returns the direct children of each pane's root process.
func (r *repository) paneChildProcesses(ctx context.Context, panePIDs []string) ([]paneChildProcess, bool) {
	processesByPID, available := r.childProcessesByParentPID(ctx, panePIDs)
	var processes []paneChildProcess
	for _, panePID := range panePIDs {
		processes = append(processes, processesByPID[panePID]...)
	}
	return processes, available
}

func (r *repository) childProcessesByParentPID(
	ctx context.Context,
	panePIDs []string,
) (map[string][]paneChildProcess, bool) {
	processesByPID := make(map[string][]paneChildProcess)
	if len(panePIDs) == 0 {
		return processesByPID, true
	}

	result, err := r.runner.Run(ctx, "", "ps", "-axo", "ppid=,etime=,comm=")
	if err != nil {
		return processesByPID, false
	}

	wanted := make(map[string]bool, len(panePIDs))
	for _, pid := range panePIDs {
		wanted[pid] = true
	}

	now := r.now()
	for _, line := range strings.Split(result.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !wanted[fields[0]] {
			continue
		}
		process := paneChildProcess{command: strings.Join(fields[2:], " ")}
		if elapsed, ok := parseProcessElapsedTime(fields[1]); ok {
			process.startedAt = now.Add(-elapsed)
		}
		processesByPID[fields[0]] = append(processesByPID[fields[0]], process)
	}
	return processesByPID, true
}

// parseProcessElapsedTime parses ps's etime format, [[dd-]hh:]mm:ss.
func parseProcessElapsedTime(raw string) (time.Duration, bool) {
	var days int
	if dayPart, clock, ok := strings.Cut(raw, "-"); ok {
		parsed, err := strconv.Atoi(dayPart)
		if err != nil || parsed < 0 {
			return 0, false
		}
		days, raw = parsed, clock
	}

	parts := strings.Split(raw, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	seconds := 0
	for _, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return 0, false
		}
		seconds = seconds*60 + value
	}
	return time.Duration(days)*24*time.Hour + time.Duration(seconds)*time.Second, true
}

// processTree returns rootPIDs and every process descended from them, as ps
// reports them now. It falls back to rootPIDs when the process list is
// unavailable.
func (r *repository) processTree(ctx context.Context, rootPIDs []string) []string {
	tree := slices.Clone(rootPIDs)
	if len(rootPIDs) == 0 {
		return tree
	}
	result, err := r.runner.Run(ctx, "", "ps", "-axo", "pid=,ppid=")
	if err != nil {
		return tree
	}

	childrenOf := make(map[string][]string)
	for _, line := range strings.Split(result.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			childrenOf[fields[1]] = append(childrenOf[fields[1]], fields[0])
		}
	}
	seen := make(map[string]bool, len(tree))
	for _, pid := range tree {
		seen[pid] = true
	}
	for i := 0; i < len(tree); i++ {
		for _, child := range childrenOf[tree[i]] {
			if !seen[child] {
				seen[child] = true
				tree = append(tree, child)
			}
		}
	}
	return tree
}

// runningProcesses returns those of pids that still exist.
func (r *repository) runningProcesses(ctx context.Context, pids []string) []string {
	if len(pids) == 0 {
		return nil
	}
	// ps exits non-zero when none of the processes exist.
	result, _ := r.runner.Run(ctx, "", "ps", "-o", "pid=", "-p", strings.Join(pids, ","))
	return strings.Fields(result.Stdout)
}

func processCommands(processes []paneChildProcess) []string {
	commands := make([]string, 0, len(processes))
	for _, process := range processes {
		commands = append(commands, process.command)
	}
	return commands
}

// processCommandStartTimes maps each command to when its newest process
// started, or returns nil when no process has a known start time.
func processCommandStartTimes(processes []paneChildProcess) map[string]time.Time {
	var startedAt map[string]time.Time
	for _, process := range processes {
		if process.startedAt.IsZero() {
			continue
		}
		if startedAt == nil {
			startedAt = make(map[string]time.Time)
		}
		if process.startedAt.After(startedAt[process.command]) {
			startedAt[process.command] = process.startedAt
		}
	}
	return startedAt
}

func (r *repository) DeleteTaskSession(ctx context.Context, task *core.Task) error {
	if task == nil || strings.TrimSpace(task.TmuxSession) == "" {
		return nil
	}

	// A child task owns only its window; the session is its parent's.
	args := []string{"kill-session", "-t", exactSessionTarget(task.TmuxSession)}
	if window := strings.TrimSpace(task.TmuxWindow); window != "" {
		args = []string{"kill-window", "-t", exactWindowTarget(task.TmuxSession, window)}
	}
	result, err := r.runner.Run(ctx, "", "tmux", args...)
	if isMissingSessionError(err, result) {
		return nil
	}

	return err
}

// TaskPaneShows reports whether any of markers is among the last lines of what
// the task's window shows now. It looks at the window's visible text only, not
// its history, and at its last non-empty lines only, where a provider's status
// line is. A pane that cannot be captured is an error; the caller decides what
// that means.
func (r *repository) TaskPaneShows(ctx context.Context, task *core.Task, markers []string) (bool, error) {
	if task == nil || strings.TrimSpace(task.TmuxSession) == "" {
		return false, fmt.Errorf("task tmux session is required")
	}
	content, err := r.capturePaneContent(ctx, task.TmuxSession, windowOf(task))
	if err != nil {
		return false, err
	}

	var tail []string
	lines := strings.Split(content, "\n")
	for i := len(lines) - 1; i >= 0 && len(tail) < paneTailLines; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			tail = append(tail, strings.ToLower(line))
		}
	}
	for _, marker := range markers {
		marker = strings.ToLower(strings.TrimSpace(marker))
		if marker == "" {
			continue
		}
		for _, line := range tail {
			if strings.Contains(line, marker) {
				return true, nil
			}
		}
	}
	return false, nil
}

// SubmitTaskPrompt pastes text into the task's window as one bracketed paste,
// so the provider takes its newlines as part of the message rather than as
// Enter, then presses Enter once the paste has settled.
func (r *repository) SubmitTaskPrompt(ctx context.Context, task *core.Task, text string) error {
	if task == nil || strings.TrimSpace(task.TmuxSession) == "" {
		return fmt.Errorf("task tmux session is required")
	}
	if err := r.typeInWindow(ctx, task.TmuxSession, windowOf(task), []string{text}); err != nil {
		return err
	}
	r.sleep(promptPasteSettleDelay)
	_, err := r.runner.Run(
		ctx, "", "tmux", "send-keys", "-t", exactWindowTarget(task.TmuxSession, windowOf(task)), "Enter",
	)
	return err
}

// RespawnTaskPane replaces what runs in the task's window with a fresh shell:
// `respawn-pane -k` kills the pane's process and starts the pane's shell again,
// and the window and its session stay. It then waits for the processes that
// ran in the pane to exit. They are noted before the respawn: once the pane's
// shell is gone, what it started is reparented and can no longer be found as
// its children.
func (r *repository) RespawnTaskPane(ctx context.Context, task *core.Task) error {
	if task == nil || strings.TrimSpace(task.TmuxSession) == "" {
		return nil
	}
	target := exactWindowTarget(task.TmuxSession, windowOf(task))

	result, err := r.runner.Run(ctx, "", "tmux", "list-panes", "-t", target, "-F", "#{pane_pid}")
	if isMissingSessionError(err, result) || isNoTmuxServerError(err, result) {
		return nil
	}
	if err != nil {
		return err
	}
	panePIDs := strings.Fields(result.Stdout)
	processes := r.processTree(ctx, panePIDs)

	result, err = r.runner.Run(ctx, "", "tmux", "respawn-pane", "-k", "-t", target)
	if isMissingSessionError(err, result) {
		return nil
	}
	if err != nil {
		return err
	}

	timeout := cmp.Or(r.respawnTimeout, defaultRespawnTimeout)
	deadline := r.now().Add(timeout)
	for {
		alive := r.runningProcesses(ctx, processes)
		// A cancelled context also makes ps fail, which reads as no process.
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(alive) == 0 {
			break
		}
		if !r.now().Before(deadline) {
			return fmt.Errorf("processes %s of the respawned task pane did not exit within %s",
				strings.Join(alive, ", "), timeout)
		}
		r.sleep(respawnPollInterval)
	}

	r.sleep(cmp.Or(r.respawnSettle, defaultRespawnSettle))
	return nil
}

// createSession starts the task's tmux session with its task window and, when
// withEditor is set, an "editor" window beside it. A Scheduled task's session
// has the task window alone.
func (r *repository) createSession(
	ctx context.Context,
	sessionName, window, workingDir string,
	task *core.Task,
	withEditor bool,
) error {
	sessionName = normalizedSessionName(sessionName)

	args := []string{"new-session", "-d", "-s", sessionName, "-n", window, "-c", workingDir}
	args = append(args, taskEnvArgs(task.ID, task.ProviderEnv)...)

	_, err := r.runner.Run(ctx, "", "tmux", args...)
	if err != nil {
		return err
	}

	r.sleep(promptSubmitDelay)
	if !withEditor {
		return nil
	}

	_, err = r.runner.Run(
		ctx,
		"",
		"tmux",
		"new-window",
		"-d",
		"-t",
		exactSessionTarget(sessionName),
		"-n",
		"editor",
		"-c",
		workingDir,
	)
	if err == nil {
		return nil
	}

	_, cleanupErr := r.runner.Run(ctx, "", "tmux", "kill-session", "-t", exactSessionTarget(sessionName))
	if cleanupErr != nil {
		return errors.Join(err, cleanupErr)
	}

	return err
}

// taskEnvArgs are the tmux -e flags that give a session or window a task's
// identity and provider configuration. The environment reaches every shell and
// provider started there, and through them the provider hooks that report back
// to Rig; a window's own RIG_TASK_ID is what tells a child task's hooks from
// its parent's. tmux can only set variables, so one the task leaves unset is
// removed by the provider launch command instead.
func taskEnvArgs(taskID string, providerEnv core.ProviderEnv) []string {
	var args []string
	if taskID = strings.TrimSpace(taskID); taskID != "" {
		args = append(args, "-e", core.TaskIDEnvVar+"="+taskID)
	}
	for _, name := range slices.Sorted(maps.Keys(providerEnv)) {
		if value, _ := providerEnv.Lookup(name); value != "" {
			args = append(args, "-e", name+"="+value)
		}
	}
	return args
}

// createWindow adds a task's window to an existing session.
func (r *repository) createWindow(
	ctx context.Context,
	sessionName, window, workingDir, taskID string,
	providerEnv core.ProviderEnv,
) error {
	args := []string{"new-window", "-d", "-t", exactSessionTarget(sessionName), "-n", window, "-c", workingDir}
	args = append(args, taskEnvArgs(taskID, providerEnv)...)
	_, err := r.runner.Run(ctx, "", "tmux", args...)
	if err != nil {
		return err
	}
	r.sleep(promptSubmitDelay)
	return nil
}

// windowExists reports whether the session has a window of that name.
func (r *repository) windowExists(ctx context.Context, sessionName, window string) bool {
	result, err := r.runner.Run(ctx, "", "tmux", "list-windows", "-t", exactSessionTarget(sessionName),
		"-F", "#{window_name}")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.TrimSpace(line) == window {
			return true
		}
	}
	return false
}

func (r *repository) cleanupStartedSession(ctx context.Context, sessionName string, cause error) error {
	_, cleanupErr := r.runner.Run(ctx, "", "tmux", "kill-session", "-t", exactSessionTarget(sessionName))
	if cleanupErr != nil {
		return errors.Join(cause, cleanupErr)
	}

	return cause
}

func (r *repository) sendKeysToWindow(ctx context.Context, session, window string, command []string) error {
	_, err := r.runner.Run(
		ctx,
		"",
		"tmux",
		"send-keys",
		"-t",
		exactWindowTarget(session, window),
		strings.Join(quoteShellCommand(command), " "),
		"C-m",
	)
	return err
}

// quoteShellCommand quotes the command words that need it for the shell: any
// word with a character the shell treats specially, such as a space, a quote,
// a "$" or a parenthesis, and a word that starts with "=", which zsh expands
// to the path of a command.
func quoteShellCommand(command []string) []string {
	quoted := make([]string, 0, len(command))
	for _, part := range command {
		if needsShellQuoting(part) {
			quoted = append(quoted, "'"+strings.ReplaceAll(part, "'", "'\\''")+"'")
			continue
		}
		quoted = append(quoted, part)
	}
	return quoted
}

// needsShellQuoting reports whether a word is empty or holds a character
// outside [A-Za-z0-9_./:=@%+,-], the characters no shell treats specially in
// the middle of a word.
func needsShellQuoting(word string) bool {
	if word == "" || strings.HasPrefix(word, "=") {
		return true
	}
	return strings.ContainsFunc(word, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		default:
			return !strings.ContainsRune("_./:=@%+,-", r)
		}
	})
}

func (r *repository) capturePaneContent(ctx context.Context, session, window string) (string, error) {
	result, err := r.runner.Run(
		ctx,
		"",
		"tmux",
		"capture-pane",
		"-t",
		exactWindowTarget(session, window),
		"-p",
	)
	if err != nil {
		return "", err
	}

	return result.Stdout, nil
}

func (r *repository) typeInWindow(ctx context.Context, session, window string, command []string) error {
	text := strings.Join(command, " ")
	bufferName := "rig-prefill-" + normalizedSessionName(session) + "-" + window

	_, err := r.runner.RunWithStdin(ctx, subprocess.RunWithStdinOptions{
		Cwd:   "",
		Name:  "tmux",
		Args:  []string{"load-buffer", "-b", bufferName, "-"},
		Stdin: text,
	})
	if err != nil {
		return fmt.Errorf("load task input into tmux buffer: %w", err)
	}

	_, pasteErr := r.runner.Run(
		ctx,
		"",
		"tmux",
		"paste-buffer",
		"-p",
		"-t",
		exactWindowTarget(session, window),
		"-b",
		bufferName,
	)

	_, deleteErr := r.runner.Run(ctx, "", "tmux", "delete-buffer", "-b", bufferName)
	if pasteErr != nil {
		if deleteErr != nil {
			return errors.Join(pasteErr, deleteErr)
		}
		return pasteErr
	}

	return deleteErr
}

func (r *repository) waitForPrompt(ctx context.Context, session, window, marker string, command []string) error {
	const (
		pollInterval = 500 * time.Millisecond
		timeout      = 30 * time.Second
	)

	deadline := r.now().Add(timeout)
	for r.now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		content, err := r.capturePaneContent(ctx, session, window)
		if err == nil && promptReady(content, marker, shellCommandText(command)) {
			return nil
		}

		r.sleep(pollInterval)
	}

	return errors.New("timed out waiting for " + marker + " prompt")
}

// promptReady reports whether the pane shows the provider's ready marker on
// a line of its own. The shell's echo of the launch command is not it: a
// shell prompt may use the same character, and `❯ claude` is on screen before
// the provider has started, so a marker followed by the command, or by the
// start of a command too long for one line, is skipped.
func promptReady(content string, marker string, commandText string) bool {
	if marker == "" {
		return true
	}
	for _, line := range strings.Split(content, "\n") {
		_, rest, found := strings.Cut(line, marker)
		if !found {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "" || commandText == "" {
			return true
		}
		if strings.HasPrefix(commandText, rest) || strings.HasPrefix(rest, commandText) {
			continue
		}
		return true
	}
	return false
}

// shellCommandText is the launch command as the shell echoes it.
func shellCommandText(command []string) string {
	return strings.TrimSpace(strings.Join(quoteShellCommand(command), " "))
}

func exactSessionTarget(session string) string {
	return "=" + normalizedSessionName(session)
}

func exactWindowTarget(session, window string) string {
	return "=" + normalizedSessionName(session) + ":" + window
}

func normalizedSessionName(session string) string {
	return strings.ReplaceAll(session, ":", "-")
}

func panesFromTmuxOutput(output string) ([]string, []string) {
	lines := strings.Split(output, "\n")
	commands := make([]string, 0, len(lines))
	pids := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		command, pid, hasPID := strings.Cut(line, "\t")
		command = strings.TrimSpace(command)
		if command != "" {
			commands = append(commands, command)
		}
		if hasPID {
			if pid = strings.TrimSpace(pid); pid != "" {
				pids = append(pids, pid)
			}
		}
	}
	return commands, pids
}

type paneInventoryEntry struct {
	session string
	window  string
	command string
	pid     string
}

func parsePaneInventory(output string) ([]paneInventoryEntry, bool) {
	var inventory []paneInventoryEntry
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			return nil, false
		}
		session := normalizedSessionName(strings.TrimSpace(fields[0]))
		window := strings.TrimSpace(fields[1])
		if session == "" || window == "" {
			return nil, false
		}
		inventory = append(inventory, paneInventoryEntry{
			session: session,
			window:  window,
			command: strings.TrimSpace(fields[2]),
			pid:     strings.TrimSpace(fields[3]),
		})
	}
	return inventory, true
}

func isMissingSessionError(err error, result subprocess.Result) bool {
	if err == nil {
		return false
	}

	stderr := result.Stderr
	if strings.TrimSpace(stderr) == "" {
		var commandErr subprocess.CommandError
		if errors.As(err, &commandErr) {
			stderr = commandErr.Stderr
		}
	}

	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "can't find session") ||
		strings.Contains(lower, "can't find window") ||
		strings.Contains(lower, "can't find pane")
}

// isNoTmuxServerError reports whether a tmux failure means there is no tmux
// server to answer: none is running, its socket does not exist (the state
// after a reboot or logout), or nothing listens on the socket any more.
func isNoTmuxServerError(err error, result subprocess.Result) bool {
	if err == nil {
		return false
	}
	stderr := strings.ToLower(strings.TrimSpace(result.Stderr))
	if stderr == "" {
		var commandErr subprocess.CommandError
		if errors.As(err, &commandErr) {
			stderr = strings.ToLower(strings.TrimSpace(commandErr.Stderr))
		}
	}
	if strings.Contains(stderr, "no server running") {
		return true
	}
	return strings.Contains(stderr, "error connecting to") &&
		(strings.Contains(stderr, "no such file or directory") || strings.Contains(stderr, "connection refused"))
}
