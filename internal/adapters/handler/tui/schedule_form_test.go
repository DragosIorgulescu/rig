package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/BaronBonet/rig/internal/core"
)

// --- creating --------------------------------------------------------------------------------

func createdScheduledTask(frontend *frontendHarness) *core.Task {
	task := scheduledTask("sched-2", "weekly report")
	frontend.createTaskEvents = []core.TaskCreateEvent{{Task: task}}
	frontend.listTasks = append(frontend.listTasks, task)
	frontend.schedules = append(frontend.schedules, digestSchedule("sched-2", nil))
	return task
}

func TestScheduledList_NComposesThePromptThenChoosesTheScheduleAndCreatesAScheduledTask(t *testing.T) {
	frontend := scheduledFrontend(nil)
	createdScheduledTask(frontend)
	m := scheduledList(t, frontend)

	m = send(t, m, key("n"))
	require.Equal(t, modePromptInput, m.mode)
	view := viewOf(m)
	require.Contains(t, view, "new scheduled task")
	require.Contains(t, view, "Enter the prompt every run gives the provider. The schedule comes next.")
	require.Contains(t, view, "What should every run do?")
	require.Contains(t, view, "enter next: schedule")
	require.NotContains(t, view, "pull requests")

	m = sendText(t, m, "write the weekly report")
	m = send(t, m, ctrl('p'))
	require.Equal(t, modePromptInput, m.mode, "a Scheduled task cannot start from a pull request")
	require.NoError(t, m.draft.err)

	m = send(t, m, keyEnter)
	require.Equal(t, modeScheduleStep, m.mode)
	require.Zero(t, frontend.createTaskStreamCalls, "nothing is created before the schedule is chosen")
	view = viewOf(m)
	require.Contains(t, view, "new scheduled task · schedule")
	require.Contains(t, view, "write the weekly report")
	require.Contains(t, view, "workspace    this folder · not a git repository")
	require.Contains(t, view, "0 9 * * 1-5", "the cron field shows an example")
	require.Contains(t, view, "new: a fresh session for every run")
	require.Contains(t, view, "auto: the provider decides what needs you")

	m = sendText(t, m, "30 6 * * 1")
	m = send(t, m, ctrl('t'))
	m = send(t, m, ctrl('p'))
	view = viewOf(m)
	require.Contains(t, view, "same: one session, each run sends it the prompt")
	require.Contains(t, view, "default: asks the way a session you start by hand does")
	require.Contains(t, view, "30 6 * * 1")
	m = send(t, m, keyEnter)

	require.Equal(t, 1, frontend.createTaskStreamCalls)
	require.Equal(t, "write the weekly report", frontend.createInput.Prompt)
	require.Equal(t, "/src/code", frontend.createInput.Cwd)
	require.Equal(t, core.ProviderCodex, frontend.createInput.Provider)
	require.Equal(t, &core.ScheduleInput{
		Cron: "30 6 * * 1", Prompt: "write the weekly report",
		SessionMode: core.PromptRunSessionSame, PermissionMode: core.TaskPermissionModeDefault,
	}, frontend.createInput.Schedule)
	require.Equal(t, modeBrowse, m.mode)
	require.Equal(t, opNone, m.pending)
	require.ElementsMatch(t, []string{"sched-1", "sched-2"}, rowIDs(m), "the new task is on the scheduled list")
	require.Equal(t, "sched-2", taskID(m.selectedRow().task))
	require.NotNil(t, m.selectedRow().schedule, "with its schedule")
}

func TestScheduledList_ANewScheduleStartsAsNewSessionsWithAutoPermissions(t *testing.T) {
	frontend := scheduledFrontend(nil)
	createdScheduledTask(frontend)
	m := send(t, scheduledList(t, frontend), key("n"))
	m = sendText(t, m, "write the weekly report")
	m = send(t, m, keyEnter)
	m = sendText(t, m, "@daily")
	send(t, m, keyEnter)

	require.Equal(t, core.PromptRunSessionNew, frontend.createInput.Schedule.SessionMode)
	require.Equal(t, core.TaskPermissionModeAuto, frontend.createInput.Schedule.PermissionMode)
}

func TestScheduledList_ThePermissionModeCyclesThroughAllFourAndBack(t *testing.T) {
	m := send(t, scheduledList(t, scheduledFrontend(nil)), key("n"))
	m = send(t, sendText(t, m, "report"), keyEnter)

	var seen []core.TaskPermissionMode
	for range 5 {
		seen = append(seen, m.scheduleForm.permissionMode)
		m = send(t, m, ctrl('p'))
	}
	require.Equal(t, []core.TaskPermissionMode{
		core.TaskPermissionModeAuto, core.TaskPermissionModeDefault, core.TaskPermissionModeAcceptEdits,
		core.TaskPermissionModeBypass, core.TaskPermissionModeAuto,
	}, seen)
	require.Contains(t, viewOf(m), "default: asks the way a session you start by hand does")
	for range 2 {
		m = send(t, m, ctrl('p'))
	}
	require.Contains(t, viewOf(m), "bypass: does anything without asking")
	m = send(t, m, ctrl('t'))
	require.Equal(t, core.PromptRunSessionSame, m.scheduleForm.sessionMode)
	m = send(t, m, ctrl('t'))
	require.Equal(t, core.PromptRunSessionNew, m.scheduleForm.sessionMode)
}

func TestScheduledList_TheComposerNeedsAPromptBeforeTheScheduleStep(t *testing.T) {
	m := send(t, scheduledList(t, scheduledFrontend(nil)), key("n"))

	m = send(t, m, keyEnter)
	require.Equal(t, modePromptInput, m.mode)
	m = send(t, sendText(t, m, "   "), keyEnter)
	require.Equal(t, modePromptInput, m.mode)
}

func TestScheduledList_ACronIsRequiredAndAMissingOneIsReportedOnTheStep(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), key("n"))
	m = send(t, sendText(t, m, "report"), keyEnter)

	m = send(t, m, keyEnter)
	require.Equal(t, modeScheduleStep, m.mode)
	require.Zero(t, frontend.createTaskStreamCalls)
	require.Contains(t, viewOf(m), "Error: a schedule is required: for example 0 9 * * 1-5, or @daily")

	m = sendText(t, m, "@daily")
	require.NotContains(t, viewOf(m), "a schedule is required", "typing clears the error")

	m = sendText(t, m, "")
	for range 6 {
		m = send(t, m, keyBackspace)
	}
	m = send(t, sendText(t, m, "   "), keyEnter)
	require.Zero(t, frontend.createTaskStreamCalls, "blanks are no schedule")
}

func TestScheduledList_EscOnTheScheduleStepGoesBackToThePromptWithEverythingTyped(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), key("n"))
	m = send(t, sendText(t, m, "report"), keyEnter)
	m = send(t, sendText(t, m, "@weekly"), ctrl('t'))

	m = send(t, m, keyEsc)
	require.Equal(t, modePromptInput, m.mode)
	require.Equal(t, "report", m.draft.prompt)
	require.True(t, m.draft.scheduled)
	m = sendText(t, m, " weekly")
	m = send(t, m, keyEnter)
	require.Equal(t, modeScheduleStep, m.mode)
	require.Equal(t, "@weekly", m.scheduleForm.cron.Value())
	require.Equal(t, core.PromptRunSessionSame, m.scheduleForm.sessionMode)
	require.Contains(t, viewOf(m), "report weekly")

	m = send(t, send(t, m, keyEsc), keyEsc)
	require.Equal(t, modeBrowse, m.mode)
	require.False(t, m.draft.scheduled, "cancelling discards the draft")
	require.Equal(t, scheduleFormState{}.sessionMode, m.scheduleForm.sessionMode, "and the schedule typed")
	require.Zero(t, frontend.createTaskStreamCalls)
}

func TestScheduledList_ARefusedScheduleReopensTheStepWithTheErrorAndWhatWasTyped(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.createTaskEvents = []core.TaskCreateEvent{{
		Err: errors.New(`schedule "nonsense": expected exactly 5 fields, found 1: [nonsense]`),
	}}
	m := send(t, scheduledList(t, frontend), key("n"))
	m = send(t, sendText(t, m, "write the weekly report"), keyEnter)
	m = send(t, m, ctrl('t'))
	m = send(t, sendText(t, m, "nonsense"), keyEnter)

	require.Equal(t, 1, frontend.createTaskStreamCalls)
	require.Equal(t, modeScheduleStep, m.mode, "a bad cron is reported on the step, not in the list")
	require.Equal(t, opNone, m.pending)
	view := viewOf(m)
	require.Contains(t, view, `Error: schedule "nonsense": expected exactly 5 fields, found 1: [nonsense]`)
	require.Equal(t, "nonsense", m.scheduleForm.cron.Value())
	require.Equal(t, core.PromptRunSessionSame, m.scheduleForm.sessionMode)
	require.Equal(t, "write the weekly report", m.promptValue())
	require.Nil(t, m.create.schedule)
	require.NoError(t, m.create.err)

	createdScheduledTask(frontend)
	for range len("nonsense") {
		m = send(t, m, keyBackspace)
	}
	m = send(t, sendText(t, m, "0 7 * * *"), keyEnter)
	require.Equal(t, 2, frontend.createTaskStreamCalls)
	require.Equal(t, "0 7 * * *", frontend.createInput.Schedule.Cron)
	require.Equal(t, modeBrowse, m.mode)
	require.Equal(t, "sched-2", taskID(m.selectedRow().task))
}

func TestScheduledList_AStreamThatCannotBeStartedReopensTheStep(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.createTaskStreamErr = errors.New("daemon not reachable")
	m := send(t, scheduledList(t, frontend), key("n"))
	m = send(t, sendText(t, m, "report"), keyEnter)
	m = send(t, sendText(t, m, "@daily"), keyEnter)

	require.Equal(t, modeScheduleStep, m.mode)
	require.Contains(t, viewOf(m), "Error: daemon not reachable")
	require.Equal(t, "@daily", m.scheduleForm.cron.Value())
}

func TestScheduledList_ATaskThatWasMadeButNotSetUpIsRetriedFromItsRowNotMadeAgain(t *testing.T) {
	frontend := scheduledFrontend(nil)
	failed := scheduledTask("sched-2", "weekly report")
	failed.CreationStatus, failed.CreationStep = core.TaskCreationStatusFailed, core.TaskCreateProgressStartingSession
	frontend.listTasks = append(frontend.listTasks, failed)
	frontend.schedules = append(frontend.schedules, digestSchedule("sched-2", nil))
	frontend.createTaskEvents = []core.TaskCreateEvent{{Task: failed, Err: errors.New("tmux is not running")}}
	m := send(t, scheduledList(t, frontend), key("n"))
	m = send(t, sendText(t, m, "report"), keyEnter)
	m = send(t, sendText(t, m, "@daily"), keyEnter)

	require.Equal(t, modeBrowse, m.mode, "the form is not reopened: sending it again would make another task")
	require.Nil(t, m.create.schedule)
	require.Nil(t, m.create.retry)
	view := viewOf(m)
	require.Contains(t, view, "Error: tmux is not running")
	require.Contains(t, view, "weekly report")
	require.Contains(t, view, "× session failed")
	require.Contains(t, view, "R retry")
}

func TestScheduledList_AStreamThatClosesWithoutAnAnswerDoesNotReopenTheStep(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.createTaskEvents = nil
	m := send(t, scheduledList(t, frontend), key("n"))
	m = send(t, sendText(t, m, "report"), keyEnter)
	m = send(t, sendText(t, m, "@daily"), keyEnter)

	require.Equal(t, modeBrowse, m.mode, "whether the task was made is unknown, so it is not made again")
	require.Contains(t, viewOf(m), "create task stream closed unexpectedly")
}

func TestScheduledList_ANewTaskOnTheCurrentListIsNotScheduled(t *testing.T) {
	m := send(t, scheduledList(t, scheduledFrontend(nil)), key("n"))
	require.True(t, m.draft.scheduled)
	m = send(t, m, keyEsc)
	m = send(t, m, key("s"))
	require.False(t, m.showScheduled)

	m = send(t, m, key("n"))
	require.Equal(t, modePromptInput, m.mode)
	require.False(t, m.draft.scheduled)
	require.Contains(t, viewOf(m), "Enter task prompt.")
	require.NotContains(t, viewOf(m), "new scheduled task")
}

func TestScheduledList_NAsksForProviderSetupFirstLikeAnyNewTask(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := scheduledList(t, frontend)
	m.providerSetup = nil

	m = send(t, m, key("n"))
	require.Equal(t, modeProviderSetup, m.mode)
}

func TestScheduledList_NDoesNotStartWhileSomethingElseIsRunning(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(nil))
	m.pending = opCreating

	next, cmd := m.Update(key("n"))
	require.Nil(t, cmd)
	require.Equal(t, modeBrowse, asModel(t, next).mode)
}

// --- editing ---------------------------------------------------------------------------------

func TestScheduledList_EOpensTheEditorWithTheStoredPromptScheduleAndModes(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) {
		s.SessionMode, s.PermissionMode = core.PromptRunSessionSame, core.TaskPermissionModeBypass
	}))

	m = send(t, m, key("e"))
	require.Equal(t, modeScheduleEdit, m.mode)
	require.Equal(t, digestPrompt, m.scheduleForm.prompt.Value())
	require.Equal(t, "0 9 * * 1-5", m.scheduleForm.cron.Value())
	require.Equal(t, core.PromptRunSessionSame, m.scheduleForm.sessionMode)
	require.Equal(t, core.TaskPermissionModeBypass, m.scheduleForm.permissionMode)
	require.Equal(t, fromDaemon(scheduleClock.Add(-48*time.Hour)), m.scheduleForm.version)
	view := viewOf(m)
	for _, want := range []string{
		"edit schedule", "morning digest", "Summarise yesterday's merged pull requests",
		"and post the list in the team channel.", "0 9 * * 1-5", "same", "bypass",
		"enter save", "tab next field", "esc cancel",
	} {
		require.Contains(t, view, want)
	}

	m = send(t, m, keyEsc)
	require.Equal(t, modeBrowse, m.mode)
	require.True(t, m.showScheduled)
}

func TestScheduledList_EOnTheCurrentListStillRenames(t *testing.T) {
	m := dashboardIn(t, scheduledFrontend(nil), "/src/code")
	m = send(t, m, key("e"))
	require.Equal(t, modeRenameTask, m.mode)
}

func TestScheduledList_EditingThePromptTheCronAndTheModesSavesThemAgainstTheVersionRead(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), key("e"))

	m = sendText(t, m, " Then archive them.")
	m = send(t, m, keyTab)
	require.Equal(t, scheduleFieldCron, m.scheduleForm.focus)
	for range len("0 9 * * 1-5") {
		m = send(t, m, keyBackspace)
	}
	m = sendText(t, m, "@every 2h")
	m = send(t, m, ctrl('t'))
	m = send(t, m, ctrl('p'))
	m = send(t, m, ctrl('p'))
	m = send(t, m, keyEnter)

	require.Equal(t, []string{"sched-1"}, frontend.updateScheduleTaskIDs)
	require.Equal(t, []core.ScheduleInput{{
		Cron: "@every 2h", Prompt: digestPrompt + " Then archive them.",
		SessionMode: core.PromptRunSessionSame, PermissionMode: core.TaskPermissionModeAcceptEdits,
	}}, frontend.updateScheduleInputs)
	require.Equal(t, []time.Time{fromDaemon(scheduleClock.Add(-48 * time.Hour))}, frontend.updateScheduleVersions)
	require.Equal(t, modeBrowse, m.mode)
	require.Equal(t, opNone, m.pending)
	require.Equal(t, "@every 2h", m.rows[0].schedule.Cron, "the row shows what was saved")
	require.Contains(t, rowText(t, m, "morning digest"), "@every 2h")
}

func TestScheduledList_TabAndShiftTabMoveBetweenThePromptAndTheCron(t *testing.T) {
	m := send(t, scheduledList(t, scheduledFrontend(nil)), key("e"))
	require.Equal(t, scheduleFieldPrompt, m.scheduleForm.focus)

	m = send(t, m, keyTab)
	require.Equal(t, scheduleFieldCron, m.scheduleForm.focus)
	m = sendText(t, m, "x")
	require.Equal(t, "0 9 * * 1-5x", m.scheduleForm.cron.Value())
	require.Equal(t, digestPrompt, m.scheduleForm.prompt.Value(), "typing goes to the field with the cursor")

	m = send(t, m, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.Equal(t, scheduleFieldPrompt, m.scheduleForm.focus)
	m = sendText(t, m, "y")
	require.Equal(t, digestPrompt+"y", m.scheduleForm.prompt.Value())
}

func TestScheduledList_AModifiedEnterAddsALineToThePromptAndAPasteGoesToTheFieldWithTheCursor(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), key("e"))

	m = send(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})
	require.Equal(t, digestPrompt+"\n", m.scheduleForm.prompt.Value())
	require.Empty(t, frontend.updateScheduleInputs)

	next, _ := m.Update(tea.PasteMsg{Content: "Also list the open ones."})
	m = asModel(t, next)
	require.Equal(t, digestPrompt+"\nAlso list the open ones.", m.scheduleForm.prompt.Value())
	require.Equal(t, "0 9 * * 1-5", m.scheduleForm.cron.Value())
}

func TestScheduledList_SavingWithNothingChangedAsksTheDaemonForNothing(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), key("e"))

	m = send(t, m, keyEnter)
	require.Empty(t, frontend.updateScheduleInputs)
	require.Equal(t, modeBrowse, m.mode)

	m = send(t, m, key("e"))
	m = send(t, m, ctrl('t'))
	m = send(t, m, ctrl('t'))
	m = send(t, m, keyEnter)
	require.Empty(t, frontend.updateScheduleInputs, "a choice put back is no change")
}

func TestScheduledList_ASchedulesDefaultsAreNotAChangeEither(t *testing.T) {
	frontend := scheduledFrontend(func(s *core.ScheduledTask) { s.SessionMode, s.PermissionMode = "", "" })
	m := send(t, scheduledList(t, frontend), key("e"))
	require.Equal(t, core.PromptRunSessionNew, m.scheduleForm.sessionMode)
	require.Equal(t, core.TaskPermissionModeAuto, m.scheduleForm.permissionMode)

	send(t, m, keyEnter)
	require.Empty(t, frontend.updateScheduleInputs)
}

func TestScheduledList_ACronOrPromptLeftEmptyIsReportedOnTheEditor(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), key("e"))

	m.scheduleForm.prompt.SetValue("  ")
	m = send(t, m, keyEnter)
	require.Equal(t, modeScheduleEdit, m.mode)
	require.Contains(t, viewOf(m), "Error: a scheduled task needs a prompt")

	m.scheduleForm.prompt.SetValue("do it")
	m.scheduleForm.cron.SetValue("")
	m = send(t, m, keyEnter)
	require.Contains(t, viewOf(m), "Error: a schedule is required")
	require.Empty(t, frontend.updateScheduleInputs)
}

func TestScheduledList_ARefusedEditStaysOnTheEditorWithTheErrorAndWhatWasTyped(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.updateScheduleErr = errors.New(
		`schedule "* * * * *" runs again after 1m0s: runs must be at least 5m0s apart`,
	)
	m := send(t, scheduledList(t, frontend), key("e"))
	m = send(t, m, keyTab)
	m.scheduleForm.cron.SetValue("* * * * *")
	m = send(t, m, keyEnter)

	require.Equal(t, modeScheduleEdit, m.mode)
	require.Equal(t, opNone, m.pending)
	require.Contains(t, viewOf(m), "Error: schedule \"* * * * *\" runs again after 1m0s")
	require.Equal(t, "* * * * *", m.scheduleForm.cron.Value())
	require.Equal(t, "0 9 * * 1-5", m.rows[0].schedule.Cron, "the row keeps what is saved")

	m = send(t, m, keyEsc)
	require.Equal(t, modeBrowse, m.mode)
}

func TestScheduledList_ASchedulesEditedElsewhereReloadsTheEditorAndTheEditIsMadeAgain(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), key("e"))
	m = sendText(t, m, " Then archive them.")

	// Another rig window edits and saves meanwhile.
	frontend.schedules[0].Cron = "30 6 * * *"
	frontend.schedules[0].Prompt = "Check the build."
	frontend.schedules[0].UpdatedAt = scheduleClock
	m = send(t, m, keyEnter)

	require.Equal(t, modeScheduleEdit, m.mode)
	require.Equal(t, opNone, m.pending)
	require.Equal(t, "Check the build.", m.scheduleForm.prompt.Value(), "the editor shows the schedule as it is now")
	require.Equal(t, "30 6 * * *", m.scheduleForm.cron.Value())
	require.Equal(t, scheduleClock, m.scheduleForm.version)
	require.Contains(t, viewOf(m), "Error: the schedule was changed elsewhere since you opened it: "+
		"the latest version is shown, so make your edit again")
	require.Equal(t, "30 6 * * *", m.rows[0].schedule.Cron, "and so does the row")

	m = sendText(t, m, " Then archive them.")
	m = send(t, m, keyEnter)
	require.Equal(
		t,
		[]time.Time{fromDaemon(scheduleClock.Add(-48 * time.Hour)), scheduleClock},
		frontend.updateScheduleVersions,
		"the second edit is made against the version that was loaded",
	)
	require.Equal(t, modeBrowse, m.mode)
	require.Equal(t, "Check the build. Then archive them.", frontend.schedules[0].Prompt)
}

func TestScheduledList_AConflictWithNoLatestVersionIsReportedOnTheEditor(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), key("e"))
	m = sendText(t, m, "!")
	frontend.schedules[0].UpdatedAt = scheduleClock
	frontend.listSchedulesErr = errors.New("daemon busy")
	m = send(t, m, keyEnter)

	require.Equal(t, modeScheduleEdit, m.mode)
	require.Contains(t, viewOf(m), "Error: the schedule was changed since it was read")
	require.True(t, strings.HasSuffix(m.scheduleForm.prompt.Value(), "!"), "what was typed is kept")
}

func TestScheduledList_ASavedEditAfterTheEditorWasLeftStillUpdatesTheRow(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), key("e"))
	saved := digestSchedule("sched-1", func(s *core.ScheduledTask) { s.Cron = "@hourly" })

	next, _ := m.Update(keyEsc)
	m = asModel(t, next)
	next, _ = m.Update(scheduleSavedMsg{taskID: "sched-1", schedule: &saved})
	m = asModel(t, next)
	require.Equal(t, "@hourly", m.rows[0].schedule.Cron)
	require.Equal(t, modeBrowse, m.mode)

	next, _ = m.Update(scheduleSavedMsg{taskID: "sched-1", err: errors.New("daemon down")})
	m = asModel(t, next)
	require.Contains(t, viewOf(m), "daemon down", "outside the editor the error is shown on the list")
}

func TestScheduledList_TheEditorShowsSavingWhileTheDaemonWorks(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), key("e"))
	m = sendText(t, m, "!")

	next, cmd := m.Update(keyEnter)
	saving := asModel(t, next)
	require.NotNil(t, cmd)
	require.Equal(t, opEditingSchedule, saving.pending)
	require.Contains(t, viewOf(saving), "Saving the schedule...")

	next, _ = saving.Update(keyEsc)
	require.Equal(t, modeScheduleEdit, asModel(t, next).mode, "input waits for the daemon")
}

func TestScheduledList_PausingShowsWhatItIsDoingWhileTheDaemonWorks(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(nil))

	next, _ := m.Update(key("z"))
	require.Contains(t, viewOf(asModel(t, next)), "Pausing the schedule...")

	m = send(t, m, key("z"))
	next, _ = m.Update(key("z"))
	require.Contains(t, viewOf(asModel(t, next)), "Resuming the schedule...")

	next, _ = m.Update(key("r"))
	require.Contains(t, viewOf(asModel(t, next)), "Asking for a run...")
}

// Whatever is in flight, n opens no composer: a schedule step opened under a
// pending operation would ignore every key, esc included, until it ended.
func TestScheduledList_NAndAOpenNoComposerWhileAnyOperationIsInFlight(t *testing.T) {
	for _, op := range []pendingOp{
		opCreating, opDeleting, opSwitching, opImporting, opShelving, opRenaming, opEditingSchedule,
		opTogglingSchedule, opRunningSchedule,
	} {
		for _, pressed := range []string{"n", "a"} {
			m := scheduledList(t, scheduledFrontend(nil))
			m.pending = op
			draft := m.draft

			next, cmd := m.Update(key(pressed))

			require.Nil(t, cmd, "%v %s", op, pressed)
			got := asModel(t, next)
			require.Equal(t, modeBrowse, got.mode, "%v %s", op, pressed)
			require.False(t, got.draft.scheduled, "%v %s", op, pressed)
			require.Equal(t, draft, got.draft, "%v %s: the draft of the user is left alone", op, pressed)
		}
	}
}

func TestScheduledList_OneThingAtATime(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(nil))
	m.pending = opShelving

	for _, pressed := range []string{"z", "r", "e", "n"} {
		next, cmd := m.Update(key(pressed))
		require.Nil(t, cmd, pressed)
		require.Equal(t, modeBrowse, asModel(t, next).mode, pressed)
	}
}

// --- modes -----------------------------------------------------------------------------------

func TestScheduleModes_BackLeadsToTheComposerFromTheStepAndToTheListFromTheEditor(t *testing.T) {
	require.Equal(t, backDest{to: modePromptInput}, backTarget(modeScheduleStep, true, false))
	require.Equal(t, backDest{to: modeBrowse}, backTarget(modeScheduleEdit, true, false))

	require.Equal(t, familyOf(modePromptInput), familyOf(modeScheduleStep), "the step continues the draft")
	require.NotEqual(t, familyOf(modeBrowse), familyOf(modeScheduleEdit))
	require.NotEqual(t, familyOf(modePromptInput), familyOf(modeScheduleEdit))
}

func TestScheduleModes_NeedAProviderSetupAndNoOperationInFlight(t *testing.T) {
	for _, mode := range []modelMode{modeScheduleStep, modeScheduleEdit} {
		require.True(t, canEnter(mode, opNone, false, true))
		require.False(t, canEnter(mode, opNone, false, false), "no provider setup")
		require.False(t, canEnter(mode, opEditingSchedule, false, true), "an operation is in flight")
		require.False(t, canEnter(mode, opNone, true, true), "setup only")
	}
}

func TestScheduleModes_LeavingAFamilyDiscardsItsFormState(t *testing.T) {
	m := send(t, scheduledList(t, scheduledFrontend(nil)), key("e"))
	require.True(t, m.scheduleForm.editing())
	m = send(t, m, keyEsc)
	require.False(t, m.scheduleForm.editing())
	require.Equal(t, core.PromptRunSessionMode(""), m.scheduleForm.sessionMode)
}

func TestScheduleForms_TheCursorKeepsBlinkingInTheFieldThatHasIt(t *testing.T) {
	m := send(t, scheduledList(t, scheduledFrontend(nil)), key("n"))
	next, cmd := sendText(t, m, "report").Update(keyEnter)
	step := asModel(t, next)
	blinks := flatten(cmd)
	require.NotEmpty(t, blinks)
	for _, blink := range blinks {
		_, follow := step.Update(blink)
		require.NotNil(t, follow, "the next blink is asked for")
	}

	next, cmd = scheduledList(t, scheduledFrontend(nil)).Update(key("e"))
	edit := asModel(t, next)
	blinks = flatten(cmd)
	require.NotEmpty(t, blinks)
	for _, blink := range blinks {
		_, follow := edit.Update(blink)
		require.NotNil(t, follow, "the next blink is asked for")
	}
}

func TestScheduleStep_SaysWhereTheTaskWillRun(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repo, ".git"), 0o755))
	m := dashboardIn(t, newFrontendHarness(), repo)
	m = send(t, send(t, m, key("s")), key("n"))
	m = send(t, sendText(t, m, "report"), keyEnter)
	require.Contains(t, viewOf(m), "workspace    new worktree")

	m = send(t, m, keyEsc)
	m = send(t, m, ctrl('o'))
	m = send(t, m, keyEnter)
	require.Contains(t, viewOf(m), "workspace    this folder")
	require.NotContains(t, viewOf(m), "not a git repository")

}

func TestScheduleStep_TheWorkspaceChosenInTheComposerIsWhatIsCreated(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repo, ".git"), 0o755))
	for name, folder := range map[string]bool{"a new worktree": false, "this folder": true} {
		t.Run(name, func(t *testing.T) {
			frontend := newFrontendHarness()
			m := send(t, send(t, dashboardIn(t, frontend, repo), key("s")), key("n"))
			m = sendText(t, m, "report")
			if folder {
				m = send(t, m, ctrl('o'))
			}
			m = send(t, m, keyEnter)
			send(t, sendText(t, m, "@daily"), keyEnter)

			want := core.WorkspaceKind("")
			if folder {
				want = core.WorkspaceKindFolder
			}
			require.Equal(t, want, frontend.createInput.Workspace)
			require.Equal(t, repo, frontend.createInput.Cwd)
		})
	}
}

func TestPermissionModes_AnUnknownModeLeadsToTheDefaultAndTheSessionModesAreEachOthersOther(t *testing.T) {
	require.Equal(t, core.TaskPermissionModeAuto, nextPermissionMode(""))
	require.Equal(t, core.TaskPermissionModeAuto, nextPermissionMode("yolo"))
	require.Equal(t, core.PromptRunSessionSame, otherSessionMode(core.PromptRunSessionNew))
	require.Equal(t, core.PromptRunSessionNew, otherSessionMode(core.PromptRunSessionSame))
	require.Contains(t, schedulePermissionText(core.TaskPermissionModeAcceptEdits), "edits files without asking")
}

func TestScheduleStep_ShowsWhatTheComposerChoseAndOnlyTheFirstLineOfThePrompt(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.providerSetup = &core.ProviderSetup{
		Configured: []core.Provider{core.ProviderCodex, core.ProviderClaude}, Default: core.ProviderCodex,
	}
	m := scheduledList(t, frontend)
	m.providerSetup = frontend.providerSetup
	m.launchSettings = launchSettingsFixture()
	m = send(t, m, key("n"))
	m = send(t, m, keyTab)
	m = send(t, m, ctrl('t'))
	m = send(t, m, ctrl('r'))
	m = sendText(t, m, "first line")
	m = send(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})
	m = sendText(t, m, "second line")
	m = send(t, m, keyEnter)

	view := viewOf(m)
	require.Contains(t, view, "prompt       first line\n")
	require.NotContains(t, view, "second line")
	require.Contains(t, view, "provider     claude  ·  opus · high effort")

	m = send(t, sendText(t, m, "@daily"), keyEnter)
	require.Equal(t, "first line\nsecond line", frontend.createInput.Prompt, "the whole prompt is what is sent")
	require.Equal(t, core.ProviderClaude, frontend.createInput.Provider)
	require.Equal(t, "opus", frontend.createInput.Model)
	require.Equal(t, "high", frontend.createInput.Effort)
	require.Equal(t, core.LaunchOptions{Model: "opus", Effort: "high"},
		m.launchSettings.Defaults[core.ProviderClaude], "the next draft preselects what was just launched")
}

func TestScheduleStep_TabAndAModifiedEnterDoNothing(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), key("n"))
	m = send(t, sendText(t, m, "report"), keyEnter)
	m = sendText(t, m, "@daily")

	next, cmd := m.Update(keyTab)
	require.Nil(t, cmd)
	m = asModel(t, next)
	require.Equal(t, scheduleFieldCron, m.scheduleForm.focus)
	next, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.Nil(t, cmd)
	m = asModel(t, next)

	m = send(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})
	require.Zero(t, frontend.createTaskStreamCalls, "only a plain enter creates the task")
	require.Equal(t, "@daily", m.scheduleForm.cron.Value())
}

func TestScheduleStep_AnErrorFromEarlierIsGoneWhenTheStepIsEnteredAgain(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.createTaskEvents = []core.TaskCreateEvent{{Err: errors.New("schedule refused")}}
	m := send(t, scheduledList(t, frontend), key("n"))
	m = send(t, sendText(t, m, "report"), keyEnter)
	m = send(t, sendText(t, m, "nonsense"), keyEnter)
	require.Contains(t, viewOf(m), "schedule refused")

	m = send(t, m, keyEsc)
	require.Equal(t, modePromptInput, m.mode)
	m = send(t, m, keyEnter)
	require.Equal(t, modeScheduleStep, m.mode)
	require.NotContains(t, viewOf(m), "schedule refused")
}

func TestScheduleStep_EscGivesTheComposerItsCursorBack(t *testing.T) {
	m := send(t, scheduledList(t, scheduledFrontend(nil)), key("n"))
	m = send(t, sendText(t, m, "report"), keyEnter)

	next, cmd := m.Update(keyEsc)
	back := asModel(t, next)
	require.NotNil(t, cmd, "the cursor of the prompt blinks again")
	require.True(t, back.draft.input.Focused())
	require.False(t, back.scheduleForm.cron.Focused())
}

func TestScheduleEdit_ChangingOneThingAloneIsAChange(t *testing.T) {
	cases := map[string]func(t *testing.T, m model) model{
		"the prompt": func(t *testing.T, m model) model { return sendText(t, m, "!") },
		"the cron": func(t *testing.T, m model) model {
			return sendText(t, send(t, m, keyTab), "0")
		},
		"the session mode":    func(t *testing.T, m model) model { return send(t, m, ctrl('t')) },
		"the permission mode": func(t *testing.T, m model) model { return send(t, m, ctrl('p')) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			frontend := scheduledFrontend(nil)
			m := send(t, scheduledList(t, frontend), key("e"))
			m = change(t, m)
			send(t, m, keyEnter)

			require.Len(t, frontend.updateScheduleInputs, 1)
			saved := frontend.updateScheduleInputs[0]
			original := digestSchedule("sched-1", nil).ScheduleInput
			changed := 0
			for _, differs := range []bool{
				saved.Prompt != original.Prompt, saved.Cron != original.Cron,
				saved.SessionMode != original.SessionMode, saved.PermissionMode != original.PermissionMode,
			} {
				if differs {
					changed++
				}
			}
			require.Equal(t, 1, changed, "only %s changed: %+v", name, saved)
		})
	}
}

func TestScheduleEdit_AWhitespaceOnlyChangeToThePromptOrCronIsNoChange(t *testing.T) {
	frontend := scheduledFrontend(func(s *core.ScheduledTask) { s.Prompt, s.Cron = "  do it  ", " @daily " })
	m := send(t, scheduledList(t, frontend), key("e"))
	m = send(t, m, keyEnter)

	require.Empty(t, frontend.updateScheduleInputs)
	require.Equal(t, modeBrowse, m.mode)
}

func TestScheduleEdit_AConflictWhileTheEditorIsClosedShowsTheLatestVersionOnTheRow(t *testing.T) {
	frontend := scheduledFrontend(nil)
	other := scheduledTask("sched-2", "nightly cleanup")
	frontend.listTasks = append(frontend.listTasks, other)
	frontend.schedules = append(frontend.schedules, digestSchedule("sched-2", nil))
	m := scheduledList(t, frontend)
	latest := digestSchedule("sched-1", func(s *core.ScheduledTask) { s.Cron = "@hourly" })

	next, _ := m.Update(scheduleSavedMsg{taskID: "sched-1", err: core.ErrScheduleConflict, latest: &latest})
	m = asModel(t, next)

	require.Equal(t, modeBrowse, m.mode)
	require.Equal(t, "@hourly", m.taskRowByID("sched-1").schedule.Cron)
	require.Equal(t, "0 9 * * 1-5", m.taskRowByID("sched-2").schedule.Cron, "only its own row")
}

func TestScheduleEdit_AConflictTakesTheLatestVersionOfTheTasksOwnSchedule(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.listTasks = append(frontend.listTasks, scheduledTask("sched-2", "nightly cleanup"))
	weekly := digestSchedule("sched-2", func(s *core.ScheduledTask) { s.Cron = "@weekly" })
	frontend.schedules = append([]core.ScheduledTask{weekly}, frontend.schedules...)
	m := send(t, scheduledList(t, frontend), key("e"))
	require.Equal(t, "sched-1", taskID(m.scheduleForm.task))
	m = sendText(t, m, "!")

	frontend.schedules[1].UpdatedAt = scheduleClock
	frontend.schedules[1].Cron = "30 6 * * *"
	m = send(t, m, keyEnter)

	require.Equal(t, "30 6 * * *", m.scheduleForm.cron.Value(), "not the cron of the schedule listed first")
	require.Equal(t, scheduleClock, m.scheduleForm.version)
}

func TestScheduleEdit_AnErrorOnTheEditorGoesWhenSomethingIsTyped(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), key("e"))
	m.scheduleForm.prompt.SetValue("")
	m = send(t, m, keyEnter)
	require.Contains(t, viewOf(m), "a scheduled task needs a prompt")

	m = sendText(t, m, "x")
	require.NotContains(t, viewOf(m), "a scheduled task needs a prompt")
}

func TestScheduleEdit_TheTaskCanBeEditedWhileTheTickRefreshesTheRow(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), key("e"))
	m = sendText(t, m, "!")
	frontend.schedules[0].LastOutcome = core.ScheduledRunFailed

	next, cmd := m.Update(activityRefreshTickMsg{})
	m = drive(t, asModel(t, next), cmd)

	require.Equal(t, modeScheduleEdit, m.mode, "the editor stays")
	require.True(t, strings.HasSuffix(m.scheduleForm.prompt.Value(), "!"), "with what was typed")
	require.Equal(t, core.ScheduledRunFailed, m.rows[0].schedule.LastOutcome)
}

func TestScheduleModes_EscOnTheStepAndTheEditorNeverQuit(t *testing.T) {
	m := send(t, scheduledList(t, scheduledFrontend(nil)), key("e"))
	next, cmd := m.Update(keyEsc)
	require.Nil(t, cmd)
	require.Equal(t, modeBrowse, asModel(t, next).mode)

	m = send(t, scheduledList(t, scheduledFrontend(nil)), key("n"))
	m = send(t, sendText(t, m, "q report"), keyEnter)
	m = sendText(t, m, "q")
	require.Equal(t, modeScheduleStep, m.mode, "q is text on the forms")
	require.Equal(t, "q", m.scheduleForm.cron.Value())
}

func TestScheduledList_ARefusedScheduleIsShownInTheListWhenTheStepCannotReopen(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.createTaskEvents = []core.TaskCreateEvent{{Err: errors.New("schedule refused")}}
	m := send(t, scheduledList(t, frontend), key("n"))
	m = send(t, sendText(t, m, "report"), keyEnter)
	m = sendText(t, m, "nonsense")
	m.providerSetup = nil

	m = send(t, m, keyEnter)
	require.Equal(t, modeBrowse, m.mode)
	require.Contains(t, viewOf(m), "Error: schedule refused")
}

func TestScheduledList_AFailureWithNothingToReopenIsShownInTheList(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(nil))
	m.create = createFlowState{schedule: &scheduleFormState{}}

	m, _ = m.failCreate(errors.New("schedule refused"))
	require.Equal(t, modeBrowse, m.mode)
	require.Contains(t, viewOf(m), "Error: schedule refused")
}

func TestScheduleStep_SubmittingLeavesTheFormAtOnceAndKeepsItForARefusal(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.providerSetup = &core.ProviderSetup{
		Configured: []core.Provider{core.ProviderCodex},
		Default:    core.ProviderCodex,
	}
	m := send(t, scheduledList(t, frontend), key("n"))
	m.providerEnv = core.ProviderEnv{"CODEX_HOME": "/work/codex"}
	m = send(t, sendText(t, m, "report"), keyEnter)
	m = sendText(t, m, "@daily")

	next, cmd := m.Update(keyEnter)
	submitted := asModel(t, next)
	require.NotNil(t, cmd)
	require.Equal(t, modeBrowse, submitted.mode, "the progress shows in the list")
	require.Equal(t, opCreating, submitted.pending)
	require.NotNil(t, submitted.create.retry, "the composer is kept")
	require.NotNil(t, submitted.create.schedule, "and so is the schedule step")
	require.Equal(t, "@daily", submitted.create.schedule.cron.Value())
	_ = flatten(cmd) // the daemon is asked to create it
	require.Equal(t, core.ProviderEnv{"CODEX_HOME": "/work/codex"}, frontend.createInput.ProviderEnv)

	submitted.endOp()
	reopened, follow := submitted.failCreate(errors.New("schedule refused"))
	require.Equal(t, modeScheduleStep, reopened.mode)
	require.NotNil(t, follow, "the cursor of the cron field blinks again")
	require.True(t, reopened.scheduleForm.cron.Focused())
	require.False(t, reopened.draft.input.Focused())
}

func TestScheduleStep_TheComposerLosesItsCursorToTheStep(t *testing.T) {
	m := send(t, scheduledList(t, scheduledFrontend(nil)), key("n"))
	m = sendText(t, m, "report")
	require.True(t, m.draft.input.Focused())

	m = send(t, m, keyEnter)
	require.False(t, m.draft.input.Focused())
	require.True(t, m.scheduleForm.cron.Focused())
}

func TestScheduleStep_CannotBeEnteredWhileSomethingElseIsRunning(t *testing.T) {
	m := send(t, scheduledList(t, scheduledFrontend(nil)), key("n"))
	m = sendText(t, m, "report")
	m.pending = opCreating

	got, cmd := m.enterScheduleStep()
	require.Nil(t, cmd)
	require.Equal(t, modePromptInput, got.mode)
}

func TestScheduledList_NDoesNothingInASetupOnlyWindow(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(nil))
	m.setupOnly = true

	m = send(t, m, key("n"))
	require.Equal(t, modeBrowse, m.mode)
	require.False(t, m.draft.scheduled)
}

func TestScheduledList_EDoesNothingWithoutAProviderSetup(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(nil))
	m.providerSetup = nil

	m = send(t, m, key("e"))
	require.Equal(t, modeBrowse, m.mode)
}

func TestScheduleEdit_EDoesNothingWhileSomethingElseIsRunningAndDoesNotComplainAboutIt(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.schedules = nil
	m := scheduledList(t, frontend)
	m.pending = opShelving

	next, cmd := m.Update(key("e"))
	require.Nil(t, cmd)
	got := asModel(t, next)
	require.Equal(t, modeBrowse, got.mode)
	require.NoError(t, got.err, "it is not a schedule problem")
}

func TestScheduleEdit_AnAnswerAboutAnotherTaskLeavesTheEditorAlone(t *testing.T) {
	m := send(t, scheduledList(t, scheduledFrontend(nil)), key("e"))
	m = sendText(t, m, "!")
	other := digestSchedule("sched-2", nil)

	next, _ := m.Update(scheduleSavedMsg{taskID: "sched-2", schedule: &other})
	m = asModel(t, next)
	require.Equal(t, modeScheduleEdit, m.mode, "the editor of sched-1 stays open")

	next, _ = m.Update(scheduleSavedMsg{taskID: "sched-2", err: core.ErrScheduleConflict, latest: &other})
	m = asModel(t, next)
	require.Equal(t, modeScheduleEdit, m.mode)
	require.True(t, strings.HasSuffix(m.scheduleForm.prompt.Value(), "!"))

	next, _ = m.Update(scheduleSavedMsg{taskID: "sched-2", err: errors.New("daemon down")})
	m = asModel(t, next)
	require.NoError(t, m.scheduleForm.err, "an error about another task is not the editor's")
}

func TestScheduleEdit_ATabMovesTheCursorOutOfTheFieldItLeaves(t *testing.T) {
	m := send(t, scheduledList(t, scheduledFrontend(nil)), key("e"))
	require.True(t, m.scheduleForm.prompt.Focused())
	require.False(t, m.scheduleForm.cron.Focused())

	m = send(t, m, keyTab)
	require.False(t, m.scheduleForm.prompt.Focused())
	require.True(t, m.scheduleForm.cron.Focused())
	m = send(t, m, keyTab)
	require.True(t, m.scheduleForm.prompt.Focused())
	require.False(t, m.scheduleForm.cron.Focused())
}

func TestNewSession_AFailureThatReturnsItsTaskStillReopensTheComposer(t *testing.T) {
	frontend := newFrontendHarness()
	frontend.listTasks = []*core.Task{{
		ID: "task-1", DisplayName: "billing retry", Provider: core.ProviderCodex,
		CreationStatus: core.TaskCreationStatusReady,
	}}
	frontend.newSessionEvents = []core.TaskCreateEvent{{Task: frontend.listTasks[0], Err: errors.New("window failed")}}
	m := newLoadedModel(frontend)
	m.launchSettings = launchSettingsFixture()

	m = pressKeys(t, m, tea.KeyPressMsg{Text: "N"}, ctrl('w'))
	m = pressKeys(t, m, typed("align the rows")...)
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	next, _ = asModel(t, next).Update(requireMsgType[taskCreateEventMsg](t, runBatchCmd(t, cmd)))
	failed := asModel(t, next)

	require.Equal(t, modePromptInput, failed.mode, "the task being listed does not keep the composer from coming back")
	require.Equal(t, "align the rows", failed.promptValue())
}

func TestScheduledList_TheComposerOfAScheduledTaskInARepositoryOffersNoPullRequests(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repo, ".git"), 0o755))
	frontend := newFrontendHarness()
	m := send(t, send(t, dashboardIn(t, frontend, repo), key("s")), key("n"))
	m = sendText(t, m, "report")

	m = send(t, m, ctrl('p'))
	require.Equal(t, modePromptInput, m.mode, "ctrl+p does not leave for the pull requests")
	require.Empty(t, frontend.listRepoPullRequestsCwd)

	m = send(t, send(t, dashboardIn(t, frontend, repo), key("n")), ctrl('p'))
	require.Equal(t, modePRPicker, m.mode, "while a task on the current list can")
}

func TestScheduleForms_AnErrorStaysWhileTheCursorBlinks(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.createTaskEvents = []core.TaskCreateEvent{{Err: errors.New("schedule refused")}}
	m := send(t, scheduledList(t, frontend), key("n"))
	m = send(t, sendText(t, m, "report"), keyEnter)
	m = send(t, sendText(t, m, "nonsense"), keyEnter)
	require.Contains(t, viewOf(m), "schedule refused")

	for _, blink := range flatten(m.scheduleForm.cron.Focus()) {
		next, _ := m.Update(blink)
		m = asModel(t, next)
	}
	require.Contains(t, viewOf(m), "schedule refused", "only typing takes the error away")

	m = send(t, m, ctrl('t'))
	require.Contains(t, viewOf(m), "schedule refused", "and choosing does not")
	next, _ := m.Update(tea.PasteMsg{Content: "x"})
	require.NotContains(t, viewOf(asModel(t, next)), "schedule refused", "a paste is typing")
}

func TestScheduleEdit_AnErrorStaysWhileTheCursorBlinks(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.updateScheduleErr = errors.New("daemon down")
	m := send(t, scheduledList(t, frontend), key("e"))
	m = sendText(t, m, "!")
	m = send(t, m, keyEnter)
	require.Contains(t, viewOf(m), "daemon down")

	for _, blink := range flatten(m.scheduleForm.prompt.Focus()) {
		next, _ := m.Update(blink)
		m = asModel(t, next)
	}
	require.Contains(t, viewOf(m), "daemon down")
}
