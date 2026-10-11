package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/BaronBonet/rig/internal/core"
)

// scheduleClock is "now" for the scheduled list tests: a Sunday morning, in a
// time zone two hours ahead of UTC, which no machine's zone is by accident. The
// daemon sends times in UTC, and the dashboard shows them in the zone of its
// clock.
var scheduleClock = time.Date(2026, time.October, 11, 8, 0, 0, 0, time.FixedZone("test", 2*60*60))

// fromDaemon is a time as the daemon sends it.
func fromDaemon(at time.Time) time.Time { return at.UTC() }

const digestPrompt = "Summarise yesterday's merged pull requests\nand post the list in the team channel."

func scheduledTask(id string, name string) *core.Task {
	return &core.Task{
		ID: id, DisplayName: name, Kind: core.TaskKindScheduled, Provider: core.ProviderCodex,
		Prompt:   "write the digest",
		RepoName: "code", RepoRoot: "/src/code", WorktreePath: "/src/code", WorkspaceKind: core.WorkspaceKindFolder,
		TmuxSession: "code_" + id, TmuxWindow: "task", CreationStatus: core.TaskCreationStatusReady,
	}
}

// digestSchedule is a schedule that has run this morning and fires again in a
// few hours.
func digestSchedule(taskID string, change func(*core.ScheduledTask)) core.ScheduledTask {
	schedule := core.ScheduledTask{
		TaskID: taskID,
		ScheduleInput: core.ScheduleInput{
			Cron: "0 9 * * 1-5", Prompt: digestPrompt,
			SessionMode: core.PromptRunSessionNew, PermissionMode: core.TaskPermissionModeAuto,
		},
		Enabled:     true,
		NextRunAt:   fromDaemon(scheduleClock.Add(3*time.Hour + 12*time.Minute)),
		LastRunAt:   fromDaemon(scheduleClock.Add(-time.Hour)),
		LastOutcome: core.ScheduledRunCompleted,
		LastDetail:  "started fresh",
		CreatedAt:   fromDaemon(scheduleClock.Add(-72 * time.Hour)),
		UpdatedAt:   fromDaemon(scheduleClock.Add(-48 * time.Hour)),
	}
	if change != nil {
		change(&schedule)
	}
	return schedule
}

// scheduledFrontend lists one ordinary task and one Scheduled task, both in
// /src/code.
func scheduledFrontend(change func(*core.ScheduledTask)) *frontendHarness {
	frontend := newFrontendHarness()
	frontend.listTasks = []*core.Task{
		{ID: "task-1", DisplayName: "search integration", RepoName: "code", RepoRoot: "/src/code",
			WorktreePath: "/src/code", WorkspaceKind: core.WorkspaceKindFolder, TmuxSession: "code_search"},
		scheduledTask("sched-1", "morning digest"),
	}
	frontend.schedules = []core.ScheduledTask{digestSchedule("sched-1", change)}
	return frontend
}

// flatten lists the messages a command produces, batches opened.
func flatten(cmd tea.Cmd) []tea.Msg {
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var msgs []tea.Msg
	for _, inner := range batch {
		if inner != nil {
			msgs = append(msgs, flatten(inner)...)
		}
	}
	return msgs
}

// drive runs a command and applies what the daemon answers, the way the
// program loop would, for the answers a scheduled list test waits for.
func drive(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	if cmd == nil {
		return m
	}
	for _, msg := range flatten(cmd) {
		switch msg.(type) {
		case tasksLoadedMsg, schedulesLoadedMsg, scheduleSavedMsg, scheduleEnabledMsg, scheduleRunRequestedMsg,
			taskDeletedMsg, taskOpenedMsg, taskCreateEventMsg, taskCreatedMsg, taskCreateStreamStartFailedMsg,
			taskCreateStreamClosedMsg:
			next, follow := m.Update(msg)
			m = asModel(t, next)
			m = drive(t, m, follow)
		}
	}
	return m
}

// send presses a key and applies the answers to the commands it started.
func send(t *testing.T, m model, key tea.KeyPressMsg) model {
	t.Helper()
	next, cmd := m.Update(key)
	return drive(t, asModel(t, next), cmd)
}

// sendText types text, a key press for each character.
func sendText(t *testing.T, m model, text string) model {
	t.Helper()
	for _, key := range typed(text) {
		next, _ := m.Update(key)
		m = asModel(t, next)
	}
	return m
}

func key(text string) tea.KeyPressMsg { return tea.KeyPressMsg{Code: rune(text[0]), Text: text} }

var (
	keyEnter     = tea.KeyPressMsg{Code: tea.KeyEnter}
	keyEsc       = tea.KeyPressMsg{Code: tea.KeyEscape}
	keyTab       = tea.KeyPressMsg{Code: tea.KeyTab}
	keySpace     = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	keyBackspace = tea.KeyPressMsg{Code: tea.KeyBackspace}
)

func viewOf(m model) string { return stripANSI(m.View().Content) }

// scheduledList opens the dashboard of /src/code on its scheduled list, with
// the clock at scheduleClock.
func scheduledList(t *testing.T, frontend *frontendHarness) model {
	t.Helper()
	m := dashboardIn(t, frontend, "/src/code")
	m.clock = func() time.Time { return scheduleClock }
	m.width = 120
	return send(t, m, key("s"))
}

func rowText(t *testing.T, m model, name string) string {
	t.Helper()
	lines := strings.Split(viewOf(m), "\n")
	for index, line := range lines {
		if strings.Contains(line, name) && index+1 < len(lines) {
			return line + "\n" + lines[index+1]
		}
	}
	t.Fatalf("no row for %q in:\n%s", name, viewOf(m))
	return ""
}

func TestScheduledList_SShowsOnlyScheduledTasksAndTheCurrentListLeavesThemOut(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := dashboardIn(t, frontend, "/src/code")
	require.Equal(t, []string{"task-1"}, rowIDs(m), "the current list leaves Scheduled tasks out")
	require.Contains(t, viewOf(m), "1 scheduled task. Press s to see it.")
	require.Contains(t, viewOf(m), "s scheduled")

	m = send(t, m, key("s"))
	require.True(t, m.showScheduled)
	require.Equal(t, []string{"sched-1"}, rowIDs(m), "the scheduled list shows only Scheduled tasks")
	view := viewOf(m)
	require.Contains(t, view, "Scheduled tasks run their prompts on a schedule. Press s to return.")
	require.NotContains(t, view, "search integration")

	m = send(t, m, key("s"))
	require.False(t, m.showScheduled)
	require.Equal(t, []string{"task-1"}, rowIDs(m))

	m = send(t, send(t, m, key("s")), keyEsc)
	require.False(t, m.showScheduled, "esc leaves the scheduled list instead of quitting")
	require.Equal(t, []string{"task-1"}, rowIDs(m))
}

func TestScheduledList_TheShelfAndTheScheduledListDoNotMix(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.listTasks = append(frontend.listTasks, &core.Task{
		ID: "task-2", DisplayName: "fix router links", RepoName: "code", RepoRoot: "/src/code",
		WorktreePath: "/src/code", WorkspaceKind: core.WorkspaceKindFolder, ShelvedAt: time.Now(),
	})
	m := dashboardIn(t, frontend, "/src/code")

	m = send(t, m, keyTab)
	require.True(t, m.showShelf)
	require.Equal(t, []string{"task-2"}, rowIDs(m))
	require.NotContains(t, viewOf(m), "scheduled task", "the shelf does not offer the scheduled list")
	m = send(t, m, key("s"))
	require.False(t, m.showScheduled, "s does nothing on the shelf")
	require.True(t, m.showShelf)
	m = send(t, m, keyEsc)

	m = send(t, m, key("s"))
	require.Equal(t, []string{"sched-1"}, rowIDs(m))
	require.NotContains(
		t,
		viewOf(m),
		"shelved",
		"tab is not available on the scheduled list, so it does not hint at the shelf",
	)
	m = send(t, m, keyTab)
	require.False(t, m.showShelf, "tab does nothing on the scheduled list")
	require.True(t, m.showScheduled)
}

func TestScheduledList_ScopesToTheLaunchFolderAndFShowsEveryFolder(t *testing.T) {
	frontend := scheduledFrontend(nil)
	elsewhere := scheduledTask("sched-2", "nightly cleanup")
	elsewhere.RepoRoot, elsewhere.WorktreePath = "/src/other", "/src/other"
	frontend.listTasks = append(frontend.listTasks, elsewhere)
	frontend.schedules = append(frontend.schedules, digestSchedule("sched-2", nil))

	m := scheduledList(t, frontend)
	require.Equal(t, []string{"sched-1"}, rowIDs(m))
	require.Contains(t, viewOf(m), "1 task in other folders hidden. Press f to show all folders.")

	m = send(t, m, key("f"))
	require.ElementsMatch(t, []string{"sched-1", "sched-2"}, rowIDs(m))
	require.True(t, m.showScheduled, "f keeps the list")
}

func TestScheduledList_EmptyListSaysHowToScheduleOne(t *testing.T) {
	frontend := newFrontendHarness()
	frontend.listTasks = shelfTasksFixture()
	m := send(t, dashboardIn(t, frontend, "/src/code"), key("s"))

	require.Empty(t, m.rows)
	view := viewOf(m)
	require.Contains(t, view, "No scheduled tasks.")
	require.Contains(t, view, "Press n to schedule one.")
	require.Zero(t, frontend.listSchedulesCalls, "no Scheduled task, so no schedule is asked for")
}

func TestScheduledList_SchedulesAreLoadedWithTheTasksOnlyWhenThereAreScheduledTasks(t *testing.T) {
	frontend := newFrontendHarness()
	frontend.listTasks = shelfTasksFixture()
	dashboardIn(t, frontend, "/src/code")
	require.Zero(t, frontend.listSchedulesCalls)

	frontend = scheduledFrontend(nil)
	dashboardIn(t, frontend, "/src/code")
	require.Equal(t, 1, frontend.listSchedulesCalls)
}

func TestScheduledList_ASchedulesThatCannotBeLoadedIsReportedAndTheTasksStillList(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.listSchedulesErr = errors.New("daemon too old")
	m := scheduledList(t, frontend)

	require.Equal(t, []string{"sched-1"}, rowIDs(m))
	view := viewOf(m)
	require.Contains(t, view, "could not load the schedules: daemon too old")
	require.Contains(t, view, "no schedule yet")
}

func TestScheduledList_KeysForOtherListsDoNothing(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.providerSetup = &core.ProviderSetup{
		Configured: []core.Provider{core.ProviderCodex, core.ProviderClaude}, Default: core.ProviderCodex,
	}
	frontend.importableSessions = []core.ProviderSessionSummary{{Provider: core.ProviderCodex, SessionID: "s1"}}
	m := scheduledList(t, frontend)
	m.providerSetup = frontend.providerSetup

	for _, pressed := range []tea.KeyPressMsg{keyTab, key("N"), key("p"), key("i")} {
		next, cmd := m.Update(pressed)
		require.Nil(t, cmd, pressed.String())
		got := asModel(t, next)
		require.Equal(t, modeBrowse, got.mode, pressed.String())
		require.True(t, got.showScheduled, pressed.String())
	}
}

func TestScheduledList_RowsSayWhenTheTaskRunsNextAndHowTheLastRunWent(t *testing.T) {
	cases := map[string]struct {
		change func(*core.ScheduledTask)
		want   []string
		not    []string
	}{
		"completed today": {
			func(s *core.ScheduledTask) {},
			[]string{"0 9 * * 1-5 · next in 3h 12m · ran 07:00"}, nil,
		},
		"completed yesterday": {
			func(s *core.ScheduledTask) { s.LastRunAt = scheduleClock.Add(-23 * time.Hour) },
			[]string{"ran 09:00 yesterday"}, nil,
		},
		"running": {
			func(s *core.ScheduledTask) {
				s.LastOutcome = core.ScheduledRunRunning
				s.RunState = core.ScheduledRunStateRunning
				s.RunStartedAt = scheduleClock.Add(-5 * time.Minute)
			},
			[]string{"next in 3h 12m · running since 07:55"}, []string{"ran 07"},
		},
		"skipped": {
			func(s *core.ScheduledTask) {
				s.LastOutcome, s.LastDetail = core.ScheduledRunSkipped, "session busy"
			},
			[]string{"skipped 07:00: session busy"}, nil,
		},
		"failed": {
			func(s *core.ScheduledTask) {
				s.LastOutcome, s.LastDetail = core.ScheduledRunFailed, "the session didn't take the prompt"
			},
			[]string{"failed 07:00: the session didn't take the prompt"}, nil,
		},
		"failed without a reason": {
			func(s *core.ScheduledTask) { s.LastOutcome, s.LastDetail = core.ScheduledRunFailed, "" },
			[]string{"failed 07:00"}, []string{"failed 07:00:"},
		},
		"never run": {
			func(s *core.ScheduledTask) {
				s.LastOutcome, s.LastRunAt, s.LastDetail = core.ScheduledRunNone, time.Time{}, ""
			},
			[]string{"next in 3h 12m · not run yet"}, nil,
		},
		"paused": {
			func(s *core.ScheduledTask) { s.Enabled = false },
			[]string{"0 9 * * 1-5 · paused · ran 07:00"}, []string{"next"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := scheduledList(t, scheduledFrontend(tc.change))
			text := rowText(t, m, "morning digest")
			for _, want := range tc.want {
				require.Contains(t, text, want)
			}
			for _, not := range tc.not {
				require.NotContains(t, text, not)
			}
			require.Contains(t, text, "codex", "the schedule is on the line of the provider")
		})
	}
}

func TestScheduledList_RowsShowTheNextRunRelativeUnderADayAndByTheClockAfter(t *testing.T) {
	cases := map[string]struct {
		next time.Time
		want string
	}{
		"overdue":          {scheduleClock.Add(-time.Minute), "next due now"},
		"now":              {scheduleClock, "next due now"},
		"seconds away":     {scheduleClock.Add(30 * time.Second), "next in <1m"},
		"a minute away":    {scheduleClock.Add(time.Minute), "next in 1m"},
		"the last minute":  {scheduleClock.Add(23*time.Hour + 59*time.Minute), "next in 23h 59m"},
		"a day away":       {scheduleClock.AddDate(0, 0, 1), "next 08:00 tomorrow"},
		"tomorrow":         {scheduleClock.Add(25 * time.Hour), "next 09:00 tomorrow"},
		"this week":        {scheduleClock.AddDate(0, 0, 3), "next Wed 08:00"},
		"a week and a day": {scheduleClock.AddDate(0, 0, 8), "next Oct 19 08:00"},
		"later this year":  {scheduleClock.AddDate(0, 0, 30), "next Nov 10 08:00"},
		"in another year":  {scheduleClock.AddDate(0, 0, 100), "next Jan 19 2027 08:00"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) { s.NextRunAt = tc.next }))
			require.Contains(t, rowText(t, m, "morning digest"), tc.want)
		})
	}
}

func TestScheduleMoment_NamesTheDayRelativeToNow(t *testing.T) {
	at := func(day int, hour int, minute int) time.Time {
		return time.Date(2026, time.October, day, hour, minute, 0, 0, scheduleClock.Location())
	}
	now := at(11, 23, 59)
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"today", at(11, 0, 5), "00:05"},
		{"just after midnight is tomorrow", at(12, 0, 1), "00:01 tomorrow"},
		{"last night is today", at(11, 23, 0), "23:00"},
		{"yesterday", at(10, 23, 59), "23:59 yesterday"},
		{"two days on is a weekday", at(13, 9, 0), "Tue 09:00"},
		{"the last day named by weekday", at(17, 9, 0), "Sat 09:00"},
		{"a week on is a date", at(18, 9, 0), "Oct 18 09:00"},
		{"two days back is a weekday", at(9, 9, 0), "Fri 09:00"},
		{"a week back is a date", at(4, 9, 0), "Oct 4 09:00"},
		{"another year", time.Date(2027, time.March, 2, 9, 0, 0, 0, scheduleClock.Location()), "Mar 2 2027 09:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, scheduleMoment(tc.at, now))
		})
	}
}

func TestScheduledList_ARowWithoutItsScheduleSaysSoAndAFreshTaskIsIdle(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.schedules = nil
	m := scheduledList(t, frontend)

	text := rowText(t, m, "morning digest")
	require.Contains(t, text, "no schedule yet")
	require.Contains(t, text, "○ idle", "a session that has not been started is idle, not unknown")
}

func TestScheduledList_ARowIsCutToTheWidthAndLosesTheLastRunReasonFirst(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) {
		s.LastOutcome = core.ScheduledRunFailed
		s.LastDetail = strings.Repeat("the provider said something very long ", 6)
	}))
	for _, width := range []int{48, 60, 72, 100} {
		m.width, m.height = width, 40
		text := rowText(t, m, "morning digest")
		for _, line := range strings.Split(text, "\n") {
			require.LessOrEqual(t, lipgloss.Width(line), width, line)
		}
		require.Contains(t, text, "0 9 * * 1-5", "the schedule stays when the line is cut")
	}
	m.width = 100
	require.Contains(t, rowText(t, m, "morning digest"), "failed 07:00: the provider said something")
}

func TestScheduledList_SpaceShowsThePromptInFullAndHowAndWhenItRuns(t *testing.T) {
	long := strings.Repeat("Check the dashboards and write down anything unusual. ", 12) + "LASTWORD"
	m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) {
		s.Prompt = digestPrompt + "\n\n" + long
		s.SessionMode, s.PermissionMode = core.PromptRunSessionSame, core.TaskPermissionModeAcceptEdits
		s.LastOutcome, s.LastDetail = core.ScheduledRunFailed, "the session didn't take the prompt"
	}))
	require.NotContains(t, viewOf(m), "PROMPT", "details are hidden until space")

	m = send(t, m, keySpace)
	view := viewOf(m)
	for _, want := range []string{
		"PROMPT", "Summarise yesterday's merged pull requests", "and post the list in the team channel.",
		"LASTWORD", "SCHEDULE", "0 9 * * 1-5", "Sun Oct 11 11:12", "in 3h 12m",
		"SESSION", "same", "accept_edits",
		"LAST RUN", "failed", "Sun Oct 11 07:00", "1h 0m ago", "the session didn't take the prompt",
		"WORKSPACE", "/src/code",
	} {
		require.Contains(t, view, want)
	}
	require.NotContains(t, view, "INITIAL PROMPT", "the stored prompt is the one that matters")
	require.Greater(t, strings.Count(view, "Check the dashboards"), 3, "no cap on the lines of the prompt")
}

func TestScheduledList_DetailsOfAPausedScheduleAndOfOneThatNeverRan(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) {
		s.Enabled = false
		s.LastOutcome, s.LastRunAt, s.LastDetail = core.ScheduledRunNone, time.Time{}, ""
	}))
	view := viewOf(send(t, m, keySpace))

	require.Contains(t, view, "paused")
	require.Contains(t, view, "press z to resume")
	require.NotContains(t, view, "ago")
	_, lastRun, _ := strings.Cut(view, "LAST RUN")
	require.Contains(t, lastRun, "not run yet")
	require.NotContains(t, lastRun, "result", "there is no outcome to give")
	require.Contains(t, view, "state        ○ idle", "a session that has not been started is idle")
}

func TestScheduledList_DetailsOfARunningScheduleAndASkippedOne(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) {
		s.LastOutcome, s.RunState = core.ScheduledRunRunning, core.ScheduledRunStateRunning
		s.RunStartedAt = scheduleClock.Add(-5 * time.Minute)
	}))
	view := viewOf(send(t, m, keySpace))
	require.Contains(t, view, "running")
	require.Contains(t, view, "Sun Oct 11 07:55 · 5m ago", "a run in flight shows when it began")

	m = scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) {
		s.LastOutcome, s.LastDetail = core.ScheduledRunSkipped, "session busy"
	}))
	view = viewOf(send(t, m, keySpace))
	require.Contains(t, view, "skipped")
	require.Contains(t, view, "session busy")
}

func TestScheduledList_ALongFailureReasonIsWrappedAndCutAfterSixLines(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) {
		s.LastOutcome = core.ScheduledRunFailed
		s.LastDetail = strings.Repeat("overloaded ", 80)
	}))
	view := viewOf(send(t, m, keySpace))

	lines := 0
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "overloaded") && !strings.Contains(line, "failed") {
			lines++
		}
	}
	require.Equal(t, 6, lines, "six lines of the reason, the last cut short")
	require.Contains(t, view, "...")
}

func TestWrapFull_KeepsLineBreaksAndCutsWordsWiderThanTheLine(t *testing.T) {
	require.Equal(t, []string{"one two", "three", "", "four"}, wrapFull("one two three\n\nfour", 8))
	require.Equal(t, []string{"abcd", "efgh", "ij"}, wrapFull("abcdefghij", 4))
	require.Equal(t, []string{"ab", "abcd", "efgh"}, wrapFull("ab abcdefgh", 4))
	require.Equal(t, []string{"a", "b"}, wrapFull("a\r\nb\n\n", 4), "trailing blank lines are dropped")
	require.Nil(t, wrapFull("text", 0))
}

func TestScheduledList_TheKeyBarOffersTheScheduleKeysAndFitsANarrowScreen(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(nil))
	require.Contains(t, viewOf(m), "n new  e edit  r run now  z pause  d delete  s back  space details  q quit")

	for _, width := range []int{60, 96} {
		m.width, m.height = width, 30
		header := stripANSI(strings.Split(m.View().Content, "\n")[0])
		require.LessOrEqual(t, lipgloss.Width(header), width, header)
		require.Contains(t, header, "z pause")
		require.Contains(t, header, "r run now")
	}

	paused := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) { s.Enabled = false }))
	require.Contains(t, viewOf(paused), "z resume")
}

func TestScheduledList_ZPausesAndResumesTheSelectedSchedule(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := scheduledList(t, frontend)

	m = send(t, m, key("z"))
	require.Equal(t, []string{"sched-1:false"}, frontend.setEnabledCalls)
	require.Equal(t, opNone, m.pending)
	require.False(t, m.rows[0].schedule.Enabled)
	text := rowText(t, m, "morning digest")
	require.Contains(t, text, "paused")
	require.NotContains(t, text, "next")
	require.Contains(t, viewOf(m), "z resume")

	m = send(t, m, key("z"))
	require.Equal(t, []string{"sched-1:false", "sched-1:true"}, frontend.setEnabledCalls)
	require.True(t, m.rows[0].schedule.Enabled)
	require.Contains(t, rowText(t, m, "morning digest"), "next in 3h 12m")
}

func TestScheduledList_ZShowsWhyAPauseWasRefusedAndShowsTheScheduleAsItIs(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.setEnabledErr = core.ErrScheduleConflict
	m := scheduledList(t, frontend)
	frontend.schedules[0].Enabled = false
	listCalls := frontend.listSchedulesCalls

	m = send(t, m, key("z"))
	require.Contains(t, viewOf(m), "pause: the schedule was changed since it was read")
	require.Greater(t, frontend.listSchedulesCalls, listCalls, "the schedules are read again")
	require.False(t, m.rows[0].schedule.Enabled, "and the row shows what they are")

	frontend.setEnabledErr = errors.New("daemon down")
	m = send(t, m, key("z"))
	require.Contains(t, viewOf(m), "resume: daemon down")
}

func TestScheduledList_ZNeedsALoadedSchedule(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.schedules = nil
	m := scheduledList(t, frontend)

	m = send(t, m, key("z"))
	require.Empty(t, frontend.setEnabledCalls)
	require.Contains(t, viewOf(m), "the schedule has not loaded yet")
	m = send(t, m, key("e"))
	require.Equal(t, modeBrowse, m.mode)
	require.Contains(t, viewOf(m), "the schedule has not loaded yet")
}

func TestScheduledList_RRunsTheSelectedTaskNowEvenWhenPaused(t *testing.T) {
	frontend := scheduledFrontend(func(s *core.ScheduledTask) { s.Enabled = false })
	m := scheduledList(t, frontend)
	listCalls := frontend.listTasksCalls

	m = send(t, m, key("r"))
	require.Equal(t, []string{"sched-1"}, frontend.runNowTaskIDs)
	require.Equal(t, listCalls, frontend.listTasksCalls, "r runs the task here; it does not refresh the list")
	require.Equal(t, opNone, m.pending)
	require.Contains(t, viewOf(m), "Run requested for morning digest. How it goes shows on its row.")

	m = send(t, m, key("j"))
	require.NotContains(t, viewOf(m), "Run requested", "the note goes with the next key")
}

func TestScheduledList_RShowsWhyARunWasRefused(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.runNowErr = errors.New(`"morning digest" is already being run`)
	m := send(t, scheduledList(t, frontend), key("r"))

	view := viewOf(m)
	require.Contains(t, view, `run now: "morning digest" is already being run`)
	require.NotContains(t, view, "Run requested")
}

func TestScheduledList_RStillRefreshesTheCurrentList(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := dashboardIn(t, frontend, "/src/code")
	calls := frontend.listTasksCalls

	next, cmd := m.Update(key("r"))
	require.True(t, asModel(t, next).loading)
	drive(t, asModel(t, next), cmd)
	require.Equal(t, calls+1, frontend.listTasksCalls)
	require.Empty(t, frontend.runNowTaskIDs)
}

func TestScheduledList_DDeletesAfterAConfirmationAndXDoesToo(t *testing.T) {
	for _, pressed := range []string{"d", "x"} {
		t.Run(pressed, func(t *testing.T) {
			frontend := scheduledFrontend(nil)
			m := send(t, scheduledList(t, frontend), key(pressed))
			require.Equal(t, modeCleanupConfirm, m.mode)
			require.Empty(t, frontend.shelveTaskIDs, "d deletes here; it does not shelve")
			view := viewOf(m)
			require.Contains(t, view, "morning digest")
			require.Contains(
				t,
				view,
				"The schedule stops and is deleted, and the tmux session is closed. The folder is kept.",
			)

			m = send(t, m, key("y"))
			require.Equal(t, []string{"sched-1"}, frontend.deleteTaskIDs)
			require.Empty(t, m.rows)
			require.Equal(t, modeBrowse, m.mode)
		})
	}

	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), key("d"))
	m = send(t, m, key("n"))
	require.Empty(t, frontend.deleteTaskIDs)
	require.Equal(t, []string{"sched-1"}, rowIDs(m))
}

func TestScheduledList_DeletingAWorktreeTaskSaysTheWorktreeGoes(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.listTasks[1].WorkspaceKind = core.WorkspaceKindWorktree
	m := send(t, scheduledList(t, frontend), key("d"))

	require.Contains(t, viewOf(m), "The schedule stops and is deleted, and the tmux session and worktree are removed.")
}

func TestScheduledList_DOnAnEmptyListDoesNothing(t *testing.T) {
	m := send(t, dashboardIn(t, newFrontendHarness(), "/src/code"), key("s"))
	m = send(t, m, key("d"))
	require.Equal(t, modeBrowse, m.mode)
}

func TestScheduledList_EnterOpensTheTasksWindow(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := send(t, scheduledList(t, frontend), keyEnter)

	require.Equal(t, "sched-1", frontend.attachedTask.ID)
	require.False(t, m.opening)
}

func TestScheduledList_TheRefreshTickReadsTheSchedulesAgainWhileTheListIsShown(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := scheduledList(t, frontend)
	calls := frontend.listSchedulesCalls

	frontend.schedules[0].LastOutcome = core.ScheduledRunFailed
	frontend.schedules[0].LastDetail = "the session ended"
	next, cmd := m.Update(activityRefreshTickMsg{})
	m = drive(t, asModel(t, next), cmd)

	require.Equal(t, calls+1, frontend.listSchedulesCalls)
	require.Contains(t, rowText(t, m, "morning digest"), "failed 07:00: the session ended")

	m = send(t, m, key("s"))
	calls = frontend.listSchedulesCalls
	next, cmd = m.Update(activityRefreshTickMsg{})
	drive(t, asModel(t, next), cmd)
	require.Equal(t, calls, frontend.listSchedulesCalls, "the current list has no schedules to read")
}

func TestScheduledList_ASchedulesThatCannotBeReadKeepsTheLastOneShown(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := scheduledList(t, frontend)

	frontend.listSchedulesErr = errors.New("daemon busy")
	m = drive(t, m, loadSchedulesCmd(m.statusContext, m.frontend))

	require.Contains(t, rowText(t, m, "morning digest"), "ran 07:00")
	require.NotContains(t, viewOf(m), "daemon busy")
}

func TestScheduledList_AReloadKeepsTheSchedulesWhenTheyCannotBeLoaded(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := scheduledList(t, frontend)

	frontend.listSchedulesErr = errors.New("daemon busy")
	m = send(t, m, key("f"))

	require.Contains(t, rowText(t, m, "morning digest"), "ran 07:00", "a reload carries the schedule over")
	require.Contains(t, viewOf(m), "could not load the schedules: daemon busy")
}

func TestScheduledList_TheCurrentListCountsOnlyTheScheduledTasksOfItsFolder(t *testing.T) {
	frontend := scheduledFrontend(nil)
	elsewhere := scheduledTask("sched-2", "nightly cleanup")
	elsewhere.RepoRoot, elsewhere.WorktreePath = "/src/other", "/src/other"
	frontend.listTasks = append(frontend.listTasks, elsewhere)
	frontend.schedules = append(frontend.schedules, digestSchedule("sched-2", nil))

	view := viewOf(dashboardIn(t, frontend, "/src/code"))
	require.Contains(t, view, "1 scheduled task. Press s to see it.")

	frontend.listTasks = append(frontend.listTasks, scheduledTask("sched-3", "weekly report"))
	frontend.schedules = append(frontend.schedules, digestSchedule("sched-3", nil))
	view = viewOf(dashboardIn(t, frontend, "/src/code"))
	require.Contains(t, view, "2 scheduled tasks. Press s to see them.")

	frontend.listTasks = shelfTasksFixture()
	require.NotContains(t, viewOf(dashboardIn(t, frontend, "/src/code")), "scheduled task")
}

func TestScheduledList_ASchedulesFailureIsShownOnlyOnTheScheduledList(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.listSchedulesErr = errors.New("daemon too old")

	m := dashboardIn(t, frontend, "/src/code")
	require.Equal(t, 1, frontend.listSchedulesCalls)
	require.NotContains(t, viewOf(m), "could not load the schedules", "the current list has no schedules to show")
	require.NoError(t, m.err)
}

func TestScheduledList_SwitchingListsStartsAtTheTopAndArrangesTheListAfresh(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.listTasks = append(frontend.listTasks, scheduledTask("sched-2", "nightly cleanup"))
	frontend.schedules = append(frontend.schedules, digestSchedule("sched-2", nil))
	m := scheduledList(t, frontend)
	m = send(t, m, key("j"))
	require.Equal(t, 1, m.selected)

	next, cmd := m.Update(key("s"))
	require.Zero(t, asModel(t, next).selected)
	var loads []tasksLoadedMsg
	for _, msg := range flatten(cmd) {
		if loaded, ok := msg.(tasksLoadedMsg); ok {
			loads = append(loads, loaded)
		}
	}
	require.Len(t, loads, 1)
	require.True(t, loads[0].reorder, "a new list is arranged afresh")

	m = send(t, m, key("s"))
	m = send(t, m, key("s"))
	require.Zero(t, m.selected)
	require.Len(t, m.rows, 2)
}

func TestScheduledList_AIsAnotherWayToCreateOne(t *testing.T) {
	m := send(t, scheduledList(t, scheduledFrontend(nil)), key("a"))
	require.Equal(t, modePromptInput, m.mode)
	require.True(t, m.draft.scheduled)
}

func TestScheduledList_TheKeysThatActOnATaskDoNothingOnAnEmptyList(t *testing.T) {
	frontend := newFrontendHarness()
	frontend.listTasks = shelfTasksFixture()
	m := send(t, dashboardIn(t, frontend, "/src/code"), key("s"))

	for _, pressed := range []tea.KeyPressMsg{key("z"), key("e"), key("r"), key("d"), key("x"), keyEnter} {
		next, cmd := m.Update(pressed)
		require.Nil(t, cmd, pressed.String())
		got := asModel(t, next)
		require.Equal(t, modeBrowse, got.mode, pressed.String())
		require.NoError(t, got.err, "there is no task to complain about: %s", pressed.String())
	}
	require.Empty(t, frontend.setEnabledCalls)
	require.Empty(t, frontend.runNowTaskIDs)
}

func TestScheduledList_RDoesNotRunWhileAWindowIsOpening(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(nil))
	m.opening = true

	next, cmd := m.Update(key("r"))
	require.Nil(t, cmd)
	require.Equal(t, opNone, asModel(t, next).pending)
}

func TestScheduledList_AScheduleThatIsNoLongerThereLeavesItsRowWithoutOne(t *testing.T) {
	frontend := scheduledFrontend(nil)
	m := scheduledList(t, frontend)
	require.NotNil(t, m.rows[0].schedule)

	frontend.schedules = nil
	m = drive(t, m, loadSchedulesCmd(m.statusContext, m.frontend))
	require.Nil(t, m.rows[0].schedule, "a refresh shows what the daemon has")
	require.Contains(t, rowText(t, m, "morning digest"), "no schedule yet")

	frontend.schedules = []core.ScheduledTask{digestSchedule("sched-1", nil)}
	m = send(t, m, key("f"))
	require.NotNil(t, m.rows[0].schedule)
	frontend.schedules = nil
	m = send(t, m, key("f"))
	require.Nil(t, m.rows[0].schedule, "and so does a reload")
}

func TestScheduledList_ARowWithoutANextRunDoesNotSayWhenItRunsNext(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) { s.NextRunAt = time.Time{} }))

	text := rowText(t, m, "morning digest")
	require.NotContains(t, text, "next")
	require.Contains(t, text, "0 9 * * 1-5 · ran 07:00")
}

func TestScheduledList_ALiveStatusShowsInsteadOfIdle(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(nil))
	m.rows[0].status = &core.TaskStatusUpdate{Phase: core.TaskStatusPhaseWorking}

	require.Contains(t, rowText(t, m, "morning digest"), "● working")
	view := viewOf(send(t, m, keySpace))
	require.Contains(t, view, "state        ● working")
	require.NotContains(t, view, "idle")
}

func TestScheduledList_DetailsOfARowWhoseScheduleHasNotLoaded(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.schedules = nil
	view := viewOf(send(t, scheduledList(t, frontend), keySpace))

	require.Contains(t, view, "PROMPT")
	require.Contains(t, view, "The schedule has not loaded yet.")
	require.Contains(t, view, "SCHEDULE")
	require.Contains(t, view, "LAST RUN")
	require.NotContains(t, view, "cron")
}

func TestScheduledList_DetailsNameTheDefaultModesOfAScheduleThatLeavesThemOut(t *testing.T) {
	frontend := scheduledFrontend(func(s *core.ScheduledTask) { s.SessionMode, s.PermissionMode = "", "" })
	frontend.listTasks[1].Model, frontend.listTasks[1].Effort = "gpt-5", "high"
	view := viewOf(send(t, scheduledList(t, frontend), keySpace))

	require.Contains(t, view, "mode         new")
	require.Contains(t, view, "permissions  auto")
	require.Contains(t, view, "provider     codex")
	require.Contains(t, view, "launch       gpt-5 · high effort")
}

func TestScheduledList_DetailsSayHowLongUntilTheNextRunOnlyWhenItIsUnderADay(t *testing.T) {
	cases := map[string]struct {
		next    time.Time
		want    string
		notWant string
	}{
		"under a day":  {scheduleClock.Add(90 * time.Minute), "Sun Oct 11 09:30 · in 1h 30m", ""},
		"a day away":   {scheduleClock.AddDate(0, 0, 1), "Mon Oct 12 08:00", "in 24h"},
		"another year": {scheduleClock.AddDate(1, 0, 0), "Mon Oct 11 2027 08:00", "· in"},
		"due":          {scheduleClock, "Sun Oct 11 08:00", "· in"},
		"unknown":      {time.Time{}, "not scheduled", "· in"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) { s.NextRunAt = tc.next }))
			view := viewOf(send(t, m, keySpace))
			require.Contains(t, view, tc.want)
			if tc.notWant != "" {
				require.NotContains(t, view, tc.notWant)
			}
		})
	}
}

func TestScheduledList_DetailsOfARunInTheFutureDoNotSayItWasAgo(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) { s.LastRunAt = scheduleClock.Add(time.Hour) }))
	view := viewOf(send(t, m, keySpace))

	require.Contains(t, view, "Sun Oct 11 09:00")
	require.NotContains(t, view, "ago")
}

func TestScheduledList_DetailsOfACompletedRunSayWhatTheRunDid(t *testing.T) {
	view := viewOf(send(t, scheduledList(t, scheduledFrontend(nil)), keySpace))

	require.Contains(t, view, "result  completed")
	require.Contains(t, view, "why     started fresh")
}

func TestLastRunText_IsExactlyWhatARowSays(t *testing.T) {
	at := scheduleClock.Add(-time.Hour)
	cases := []struct {
		name     string
		schedule core.ScheduledTask
		want     string
	}{
		{"never", core.ScheduledTask{}, "not run yet"},
		{"completed", core.ScheduledTask{LastOutcome: core.ScheduledRunCompleted, LastRunAt: at}, "ran 07:00"},
		{"completed with no time", core.ScheduledTask{LastOutcome: core.ScheduledRunCompleted}, "ran"},
		{"running", core.ScheduledTask{LastOutcome: core.ScheduledRunRunning, LastRunAt: at}, "running since 07:00"},
		{"running since its start", core.ScheduledTask{
			LastOutcome: core.ScheduledRunRunning, LastRunAt: at, RunStartedAt: scheduleClock.Add(-5 * time.Minute),
		}, "running since 07:55"},
		{"running with no time", core.ScheduledTask{LastOutcome: core.ScheduledRunRunning}, "running since"},
		{"skipped", core.ScheduledTask{
			LastOutcome: core.ScheduledRunSkipped, LastRunAt: at, LastDetail: "session busy",
		}, "skipped 07:00: session busy"},
		{"failed", core.ScheduledTask{
			LastOutcome: core.ScheduledRunFailed, LastRunAt: at, LastDetail: " the session ended ",
		}, "failed 07:00: the session ended"},
		{"failed with no time", core.ScheduledTask{
			LastOutcome: core.ScheduledRunFailed, LastDetail: "the session ended",
		}, "failed: the session ended"},
		{
			"failed with no reason",
			core.ScheduledTask{LastOutcome: core.ScheduledRunFailed, LastRunAt: at},
			"failed 07:00",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, _ := lastRunText(tc.schedule, scheduleClock)
			require.Equal(t, tc.want, text)
		})
	}

	_, style := lastRunText(core.ScheduledTask{LastOutcome: core.ScheduledRunFailed}, scheduleClock)
	require.Equal(t, errorStyle.Render("x"), style.Render("x"))
	_, style = lastRunText(core.ScheduledTask{LastOutcome: core.ScheduledRunSkipped}, scheduleClock)
	require.Equal(t, warningStyle.Render("x"), style.Render("x"))
	_, style = lastRunText(core.ScheduledTask{LastOutcome: core.ScheduledRunRunning}, scheduleClock)
	require.Equal(t, healthyStyle.Render("x"), style.Render("x"))
	_, style = lastRunText(core.ScheduledTask{LastOutcome: core.ScheduledRunCompleted}, scheduleClock)
	require.Equal(t, dimStyle.Render("x"), style.Render("x"))
}

func TestJoinParts_FitsTheWidthAndCutsTheFirstPartThatDoesNot(t *testing.T) {
	parts := []textPart{{"abc", primaryStyle}, {"", primaryStyle}, {"defg", dimStyle}, {"hijkl", dimStyle}}
	plain := func(width int) string { return stripANSI(joinParts(parts, width)) }

	require.Equal(t, "abc · defg · hijkl", plain(18))
	require.Equal(t, "abc · defg · hij…", plain(17), "the part that does not fit is cut")
	require.Equal(t, "abc · defg", plain(11), "a part with no room is left out")
	require.Equal(t, "abc · defg", plain(10))
	require.Equal(t, "abc · de…", plain(9))
	require.Equal(t, "abc", plain(6), "a separator with nothing after it is left out")
	require.Equal(t, "abc", plain(3))
	require.Equal(t, "a…", plain(2))
	require.Empty(t, plain(0))
	require.Equal(t, "abc · defg", stripANSI(joinParts(parts[:3], 40)), "blank parts take no separator")
}

func TestScheduledList_TheNormalKeyBarKeepsTheScheduledKeyOnANarrowScreen(t *testing.T) {
	m := dashboardIn(t, scheduledFrontend(nil), "/src/code")
	m.width, m.height = 96, 30

	header := stripANSI(strings.Split(m.View().Content, "\n")[0])
	require.LessOrEqual(t, lipgloss.Width(header), 96)
	require.Contains(t, header, "s scheduled")
	require.Contains(t, header, "tab shelf")
}

func TestScheduledList_ARunRequestedNoticeTakesTwoLinesFromTheRows(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(nil))
	m.width, m.height = 100, 30
	without := m.taskListRowBudget(100, 30)

	m.notice = "Run requested."
	require.Equal(t, without-2, m.taskListRowBudget(100, 30))
	m.notice = ""
	m.err = errors.New("failed")
	require.Equal(t, without-2, m.taskListRowBudget(100, 30), "as an error does")
}

func TestScheduledList_ARunRequestedNoticeIsCutToTheWidth(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(nil))
	m.width = 40
	m.notice = strings.Repeat("a long note ", 10)

	found := false
	for _, line := range strings.Split(viewOf(m), "\n") {
		if strings.Contains(line, "a long note") {
			found = true
			require.LessOrEqual(t, lipgloss.Width(line), 40, line)
		}
	}
	require.True(t, found)
}

func TestScheduledList_DetailsNameTheWorkspaceOnlyWhereTheTaskHasOne(t *testing.T) {
	view := viewOf(send(t, scheduledList(t, scheduledFrontend(nil)), keySpace))
	require.Contains(t, view, "repo   code")
	require.Contains(t, view, "path   /src/code")
	require.Contains(t, view, "window code_sched-1:task")
	require.NotContains(t, view, "branch")

	frontend := scheduledFrontend(nil)
	frontend.listTasks[1].BranchName = "feat/morning-digest"
	view = viewOf(send(t, scheduledList(t, frontend), keySpace))
	require.Contains(t, view, "branch feat/morning-digest")

	frontend = scheduledFrontend(nil)
	frontend.listTasks[1].RepoName, frontend.listTasks[1].WorktreePath, frontend.listTasks[1].TmuxWindow = "", "", ""
	view = viewOf(send(t, scheduledList(t, frontend), keySpace))
	require.NotContains(t, view, "repo   ")
	require.NotContains(t, view, "path   ")
	require.NotContains(t, view, "window ")
}

func TestScheduledList_DetailsColourTheLastRunByHowItWent(t *testing.T) {
	cases := map[core.ScheduledRunOutcome]lipgloss.Style{
		core.ScheduledRunCompleted: dimStyle,
		core.ScheduledRunSkipped:   warningStyle,
		core.ScheduledRunFailed:    errorStyle,
		core.ScheduledRunRunning:   healthyStyle,
	}
	for outcome, style := range cases {
		t.Run(string(outcome), func(t *testing.T) {
			m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) { s.LastOutcome = outcome }))
			m = send(t, m, keySpace)
			require.Contains(t, m.View().Content, style.Render(string(outcome)))
		})
	}
}

func TestScheduledList_ARunInFlightWithNoRecordedStartShowsWhenItWasMade(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) { s.LastOutcome = core.ScheduledRunRunning }))
	view := viewOf(send(t, m, keySpace))
	require.Contains(t, view, "Sun Oct 11 07:00 · 1h 0m ago")
}

func TestScheduledList_AnAnswerForAnotherRowChangesThatRowOnly(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.listTasks = append(frontend.listTasks, scheduledTask("sched-2", "nightly cleanup"))
	frontend.schedules = append(frontend.schedules, digestSchedule("sched-2", nil))
	m := scheduledList(t, frontend)
	require.Equal(t, "sched-1", taskID(m.selectedRow().task))

	paused := digestSchedule("sched-2", func(s *core.ScheduledTask) { s.Enabled = false })
	next, _ := m.Update(scheduleEnabledMsg{taskID: "sched-2", schedule: &paused})
	m = asModel(t, next)

	require.True(t, m.taskRowByID("sched-1").schedule.Enabled)
	require.False(t, m.taskRowByID("sched-2").schedule.Enabled)
}

func TestScheduledList_DetailsOfACompletedRunWithNoTimeHaveNoWhenLine(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) { s.LastRunAt = time.Time{} }))
	view := viewOf(send(t, m, keySpace))

	require.Contains(t, view, "result  completed")
	require.NotContains(t, view, "when ")
}

func TestScheduledList_DetailsOfARunMadeJustNowSayItWasLessThanAMinuteAgo(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) { s.LastRunAt = fromDaemon(scheduleClock) }))
	require.Contains(t, viewOf(send(t, m, keySpace)), "Sun Oct 11 08:00 · <1m ago")
}

func TestScheduledList_DetailsCutAReasonOfMoreThanSixLinesAndKeepOneOfSix(t *testing.T) {
	word := func(index int) string { return strings.Repeat(string(rune('a'+index)), 38) }
	words := func(count int) string {
		var all []string
		for index := range count {
			all = append(all, word(index))
		}
		return strings.Join(all, " ")
	}

	m := scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) {
		s.LastOutcome, s.LastDetail = core.ScheduledRunFailed, words(6)
	}))
	view := viewOf(send(t, m, keySpace))
	require.Contains(t, view, word(5), "six lines are all shown")
	require.NotContains(t, view, "...")

	m = scheduledList(t, scheduledFrontend(func(s *core.ScheduledTask) {
		s.LastOutcome, s.LastDetail = core.ScheduledRunFailed, words(7)
	}))
	view = viewOf(send(t, m, keySpace))
	require.Contains(t, view, word(4))
	require.NotContains(t, view, word(6), "a seventh line is not")
	require.Contains(t, view, "...")
	last := 0
	_, details, _ := strings.Cut(view, "LAST RUN")
	for _, line := range strings.Split(details, "\n") {
		for index := range 7 {
			if strings.Contains(line, strings.Repeat(string(rune('a'+index)), 8)) {
				last++
				break
			}
		}
	}
	require.Equal(t, 6, last, "the reason takes six lines")
}

func TestScheduledList_TheKeyBarDropsDetailsAndQuitBeforeDelete(t *testing.T) {
	m := scheduledList(t, scheduledFrontend(nil))
	m.width, m.height = 72, 30

	header := stripANSI(strings.Split(m.View().Content, "\n")[0])
	require.LessOrEqual(t, lipgloss.Width(header), 72, header)
	require.Contains(t, header, "d delete")
	require.Contains(t, header, "q quit")
	require.Contains(t, header, "s back")
	require.NotContains(t, header, "space details")
}

func TestScheduledList_FailedCreationOffersARetryOnTheKeyBar(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.listTasks[1].CreationStatus = core.TaskCreationStatusFailed
	frontend.listTasks[1].CreationStep = core.TaskCreateProgressStartingSession
	m := scheduledList(t, frontend)
	require.Contains(t, viewOf(m), "s back  R retry  space details")

	ready := scheduledList(t, scheduledFrontend(nil))
	require.NotContains(t, viewOf(ready), "R retry")
}

func TestScheduledList_AGenericErrorFromAnEditDoesNotReadTheSchedulesAgain(t *testing.T) {
	frontend := scheduledFrontend(nil)
	frontend.updateScheduleErr = errors.New("daemon down")
	m := send(t, scheduledList(t, frontend), key("e"))
	m = sendText(t, m, "!")
	calls := frontend.listSchedulesCalls

	send(t, m, keyEnter)
	require.Equal(t, calls, frontend.listSchedulesCalls)
}

func TestWrapFull_FillsALineExactlyAndCutsWideCharacters(t *testing.T) {
	require.Equal(t, []string{"ab cd"}, wrapFull("ab cd", 5), "a line may be exactly as wide as the width")
	require.Equal(t, []string{"ab", "cd"}, wrapFull("ab cd", 4))
	wide := []string{string(rune(0x65e5)), string(rune(0x672c))} // characters two columns wide
	require.Equal(t, wide, wrapFull(wide[0]+wide[1], 1), "a character wider than the line is still shown")
	require.Equal(t, append(append([]string(nil), wide...), "a", "b"), wrapFull(wide[0]+wide[1]+"\nab", 1),
		"and takes no blank line of its own")
}

func TestScheduleDateTime_NamesTheYearOnlyWhenItIsNotThisOne(t *testing.T) {
	now := scheduleClock
	require.Equal(t, "Sun Oct 11 11:12", scheduleDateTime(fromDaemon(now.Add(3*time.Hour+12*time.Minute)), now))
	require.Equal(t, "Mon Oct 11 2027 08:00", scheduleDateTime(fromDaemon(now.AddDate(1, 0, 0)), now))
	require.Equal(t, "Fri Dec 25 10:00", scheduleDateTime(
		time.Date(2026, time.December, 25, 8, 0, 0, 0, time.UTC), now), "in the zone of the clock, not UTC")
}

func TestScheduleMoment_ShowsTimesInTheZoneOfTheClock(t *testing.T) {
	now := scheduleClock
	utc := time.Date(2026, time.October, 11, 6, 30, 0, 0, time.UTC)
	require.Equal(t, "08:30", scheduleMoment(utc, now))
	late := time.Date(2026, time.October, 11, 22, 30, 0, 0, time.UTC)
	require.Equal(t, "00:30 tomorrow", scheduleMoment(late, now), "the day is the day in the clock's zone")
}

func TestScheduleMoment_NamesTheLastDayBackByWeekday(t *testing.T) {
	now := scheduleClock
	require.Equal(t, "Mon 09:00", scheduleMoment(time.Date(2026, time.October, 5, 9, 0, 0, 0, now.Location()), now))
	require.Equal(t, "Oct 4 09:00", scheduleMoment(time.Date(2026, time.October, 4, 9, 0, 0, 0, now.Location()), now))
}

func TestFirstLine_IsTheFirstLineWithAnythingOnIt(t *testing.T) {
	require.Equal(t, "x y", firstLine("\n  \n  x y \nz"))
	require.Empty(t, firstLine(" \n "))
}
