package tui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/BaronBonet/rig/internal/core"
)

// The scheduled list is the dashboard's second list, beside the current tasks
// and the shelf: only Scheduled tasks are on it, each with the schedule it
// runs by. The current list leaves them out.

// now is the time the dashboard shows schedules against.
func (m model) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

// splitScheduled separates the Scheduled tasks from the ordinary ones.
func splitScheduled(tasks []*core.Task) ([]*core.Task, []*core.Task) {
	var ordinary, scheduled []*core.Task
	for _, task := range tasks {
		if task.IsScheduled() {
			scheduled = append(scheduled, task)
		} else {
			ordinary = append(ordinary, task)
		}
	}
	return ordinary, scheduled
}

// scheduledKey reports whether a key does something on the scheduled list.
// Scheduled tasks have one window and no sessions of their own to start, so
// the shelf, new sessions, importing and switching provider are left out.
func scheduledKey(key string) bool {
	switch key {
	case "tab", "N", "p", "i":
		return false
	default:
		return true
	}
}

// toggleScheduled switches the list between the current tasks and the
// scheduled tasks.
func (m model) toggleScheduled() (tea.Model, tea.Cmd) {
	m.showScheduled = !m.showScheduled
	m.selected = 0
	return m, m.loadTasks(true)
}

// updateScheduledList handles the keys that mean something else on the
// scheduled list than on the current one. handled is false for the rest, which
// do what they do everywhere.
func (m model) updateScheduledList(key string) (next tea.Model, cmd tea.Cmd, handled bool) {
	switch key {
	case "esc":
		next, cmd = m.toggleScheduled()
	case "n", "a":
		next, cmd = m.enterScheduledPromptMode()
	case "e":
		next, cmd = m.enterScheduleEdit()
	case "r":
		next, cmd = m.runSelectedScheduleNow()
	case "z":
		next, cmd = m.toggleSelectedSchedule()
	case "d":
		next, cmd = m.confirmCleanup()
	default:
		return m, nil, false
	}
	return next, cmd, true
}

// confirmCleanup asks to remove the selected task.
func (m model) confirmCleanup() (tea.Model, tea.Cmd) {
	if len(m.rows) == 0 {
		return m, nil
	}
	m.transition(modeCleanupConfirm)
	return m, nil
}

// selectedSchedule is the schedule of the selected task, or nil when there is
// none or it has not loaded.
func (m model) selectedSchedule() (*taskRow, *core.ScheduledTask) {
	row := m.selectedRow()
	if row == nil || row.task == nil {
		return nil, nil
	}
	return row, row.schedule
}

// runSelectedScheduleNow asks the daemon for a run of the selected Scheduled
// task now, whether or not its schedule is paused.
func (m model) runSelectedScheduleNow() (tea.Model, tea.Cmd) {
	row := m.selectedRow()
	if m.pending != opNone || m.opening || row == nil || row.task == nil {
		return m, nil
	}
	m.beginOp(opRunningSchedule)
	return m, tea.Batch(
		runScheduleNowCmd(m.statusContext, m.frontend, row.task),
		shimmerTickCmd(),
	)
}

// toggleSelectedSchedule pauses the selected schedule, or resumes it.
func (m model) toggleSelectedSchedule() (tea.Model, tea.Cmd) {
	row, schedule := m.selectedSchedule()
	if m.pending != opNone || row == nil {
		return m, nil
	}
	if schedule == nil {
		m.err = errors.New("the schedule has not loaded yet: try again in a moment")
		return m, nil
	}
	enabled := !schedule.Enabled
	m.beginOp(opTogglingSchedule)
	m.scheduleResuming = enabled
	return m, tea.Batch(
		setScheduleEnabledCmd(m.statusContext, m.frontend, taskID(row.task), enabled),
		shimmerTickCmd(),
	)
}

// applySchedule gives the row of the schedule's task the schedule.
func (m *model) applySchedule(schedule core.ScheduledTask) {
	if row := m.taskRowByID(schedule.TaskID); row != nil {
		copied := schedule
		row.schedule = &copied
	}
}

// applySchedules gives every listed row the schedule of its task, and none to
// a row whose task has no schedule.
func (m *model) applySchedules(schedules []core.ScheduledTask) {
	attachSchedules(m.rows, schedules)
}

// attachSchedules gives the rows being listed the schedules just loaded.
func attachSchedules(rows []taskRow, schedules []core.ScheduledTask) {
	byTask := make(map[string]core.ScheduledTask, len(schedules))
	for _, schedule := range schedules {
		byTask[schedule.TaskID] = schedule
	}
	for index := range rows {
		rows[index].schedule = nil
		if schedule, ok := byTask[taskID(rows[index].task)]; ok {
			copied := schedule
			rows[index].schedule = &copied
		}
	}
}

// scheduleMoment names a moment for the dashboard, in the time zone of now: the
// time of day for today, with "tomorrow" or "yesterday" for the day either
// side, the weekday within the week around now, and the date beyond it.
func scheduleMoment(at time.Time, now time.Time) string {
	at = at.In(now.Location())
	clock := at.Format("15:04")
	days := calendarDaysBetween(now, at)
	switch {
	case days == 0:
		return clock
	case days == 1:
		return clock + " tomorrow"
	case days == -1:
		return clock + " yesterday"
	case days > 1 && days < 7, days < -1 && days > -7:
		return at.Format("Mon") + " " + clock
	case at.Year() == now.Year():
		return at.Format("Jan 2") + " " + clock
	default:
		return at.Format("Jan 2 2006") + " " + clock
	}
}

// scheduleDateTime is a moment with its weekday and date, for the details
// panel, which has room for it.
func scheduleDateTime(at time.Time, now time.Time) string {
	at = at.In(now.Location())
	if at.Year() == now.Year() {
		return at.Format("Mon Jan 2 15:04")
	}
	return at.Format("Mon Jan 2 2006 15:04")
}

// calendarDaysBetween is how many calendar days later than from the day of to
// is; negative when to is earlier.
func calendarDaysBetween(from time.Time, to time.Time) int {
	fromDay := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	toDay := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	return int(math.Round(toDay.Sub(fromDay).Hours() / 24))
}

// untilText says how long until a moment, to the minute.
func untilText(until time.Duration) string {
	if until < time.Minute {
		return "<1m"
	}
	return formatElapsed(until)
}

// nextRunText says when an active schedule fires next: how long until it
// when that is under a day, and when it is otherwise.
func nextRunText(schedule core.ScheduledTask, now time.Time) string {
	if schedule.NextRunAt.IsZero() {
		return ""
	}
	until := schedule.NextRunAt.Sub(now)
	switch {
	case until <= 0:
		return "due now"
	case until < 24*time.Hour:
		return "in " + untilText(until)
	default:
		return scheduleMoment(schedule.NextRunAt, now)
	}
}

// lastRunText says how the latest run went, for a row, with the style that
// suits the outcome.
func lastRunText(schedule core.ScheduledTask, now time.Time) (string, lipgloss.Style) {
	moment := ""
	if !schedule.LastRunAt.IsZero() {
		moment = scheduleMoment(schedule.LastRunAt, now)
	}
	withDetail := func(text string) string {
		if detail := strings.TrimSpace(schedule.LastDetail); detail != "" {
			return text + ": " + detail
		}
		return text
	}

	switch schedule.LastOutcome {
	case core.ScheduledRunRunning:
		since := moment
		if !schedule.RunStartedAt.IsZero() {
			since = scheduleMoment(schedule.RunStartedAt, now)
		}
		return strings.TrimSpace("running since " + since), healthyStyle
	case core.ScheduledRunCompleted:
		return strings.TrimSpace("ran " + moment), dimStyle
	case core.ScheduledRunSkipped:
		return withDetail(strings.TrimSpace("skipped " + moment)), warningStyle
	case core.ScheduledRunFailed:
		return withDetail(strings.TrimSpace("failed " + moment)), errorStyle
	default:
		return "not run yet", dimStyle
	}
}

// textPart is a piece of a row's line with its style.
type textPart struct {
	text  string
	style lipgloss.Style
}

const partSeparator = " · "

// joinParts renders the parts one after another, apart by a separator, and no
// wider than width: the first part that does not fit is cut, and the parts
// after it are left out.
func joinParts(parts []textPart, width int) string {
	var builder strings.Builder
	used := 0
	for _, part := range parts {
		if part.text == "" {
			continue
		}
		if used > 0 {
			if used+lipgloss.Width(partSeparator) >= width {
				break
			}
			builder.WriteString(mutedStyle.Render(partSeparator))
			used += lipgloss.Width(partSeparator)
		}
		text := truncateStr(part.text, width-used)
		builder.WriteString(part.style.Render(text))
		used += lipgloss.Width(text)
	}
	return builder.String()
}

// scheduleRowText is the second line of a Scheduled task's row after its
// provider: the schedule, when it fires next, and how the latest run went.
func (m model) scheduleRowText(schedule *core.ScheduledTask, width int) string {
	if schedule == nil {
		return dimStyle.Render(truncateStr("no schedule yet", width))
	}
	now := m.now()
	parts := []textPart{{text: schedule.Cron, style: primaryStyle}}
	if schedule.Enabled {
		if next := nextRunText(*schedule, now); next != "" {
			parts = append(parts, textPart{text: "next " + next, style: dimStyle})
		}
	} else {
		parts = append(parts, textPart{text: "paused", style: warningStyle})
	}
	last, lastStyle := lastRunText(*schedule, now)
	parts = append(parts, textPart{text: last, style: lastStyle})
	if skipped := skippedBesideRunText(*schedule, now); skipped != "" {
		parts = append(parts, textPart{text: skipped, style: warningStyle})
	}
	return joinParts(parts, width)
}

// skippedBesideRunText says that a run due while another was in flight was
// skipped, and why, for a row whose run is still in flight: the run's own
// record stays as it was, so the skip is told of beside it. It is empty with
// none, and once the run has ended, when the row is about the run that ended.
func skippedBesideRunText(schedule core.ScheduledTask, now time.Time) string {
	if !schedule.IsRunning() || schedule.RunSkippedAt.IsZero() {
		return ""
	}
	text := "skipped " + scheduleMoment(schedule.RunSkippedAt, now)
	if detail := strings.TrimSpace(schedule.RunSkippedDetail); detail != "" {
		text += ": " + detail
	}
	return text
}

// scheduleModeText says what a session mode means for a run.
func scheduleModeText(mode core.PromptRunSessionMode) string {
	if mode == core.PromptRunSessionSame {
		return "same: one session, each run sends it the prompt"
	}
	return "new: a fresh session for every run"
}

// schedulePermissionText says what a permission mode lets the provider do.
func schedulePermissionText(mode core.TaskPermissionMode) string {
	switch mode {
	case core.TaskPermissionModeDefault:
		return "default: asks the way a session you start by hand does"
	case core.TaskPermissionModeAcceptEdits:
		return "accept_edits: edits files without asking"
	case core.TaskPermissionModeBypass:
		return "bypass: does anything without asking"
	default:
		return "auto: the provider decides what needs you"
	}
}

// wrapFull wraps text to width without leaving any of it out: line breaks are
// kept, and a word wider than the width is cut into lines.
func wrapFull(text string, width int) []string {
	if width <= 0 {
		return nil
	}
	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}
		current := ""
		for _, word := range words {
			for lipgloss.Width(word) > width {
				head := clipVisible(word, width)
				if head == "" {
					// A character wider than the whole width: show it as it is.
					head = string([]rune(word)[:1])
				}
				if current != "" {
					lines = append(lines, current)
					current = ""
				}
				lines = append(lines, head)
				word = strings.TrimPrefix(word, head)
			}
			switch {
			case current == "":
				current = word
			case lipgloss.Width(current)+1+lipgloss.Width(word) <= width:
				current += " " + word
			default:
				lines = append(lines, current)
				current = word
			}
		}
		if current != "" {
			lines = append(lines, current)
		}
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// scheduledTaskDetailView is the details panel of a Scheduled task: the prompt
// in full, when it runs and how, and how the latest run went.
func (m model) scheduledTaskDetailView(row taskRow) string {
	task, schedule := row.task, row.schedule
	totalWidth := m.totalWidth()
	leftWidth := max(totalWidth*40/100, 38)
	rightWidth := max(totalWidth-leftWidth-6, 20)
	now := m.now()

	var builder strings.Builder
	builder.WriteString("   " + headerLabelStyle.Render("PROMPT") + "\n")
	if schedule == nil {
		builder.WriteString("   " + dimStyle.Render("The schedule has not loaded yet.") + "\n")
	} else {
		for _, line := range wrapFull(schedule.Prompt, totalWidth-6) {
			builder.WriteString("   " + primaryStyle.Render(line) + "\n")
		}
	}
	builder.WriteString("\n")

	scheduleLines := []string{headerLabelStyle.Render("SCHEDULE")}
	sessionLines := []string{headerLabelStyle.Render("SESSION")}
	if schedule != nil {
		scheduleLines = append(scheduleLines, mutedStyle.Render("cron")+"   "+primaryStyle.Render(schedule.Cron))
		scheduleLines = append(scheduleLines, mutedStyle.Render("next")+"   "+nextRunDetail(*schedule, now))
		sessionLines = append(sessionLines,
			mutedStyle.Render("mode")+"         "+primaryStyle.Render(truncateStr(
				emptyFallback(string(schedule.SessionMode), string(core.PromptRunSessionNew)), rightWidth-13)),
			mutedStyle.Render("permissions")+"  "+primaryStyle.Render(truncateStr(
				emptyFallback(string(schedule.PermissionMode), string(core.TaskPermissionModeAuto)), rightWidth-13)),
		)
	}
	statusText, statusStyle := taskStatusDetailText(row.status)
	if row.status == nil {
		// Its session is an idle shell until the first run.
		statusText, statusStyle = iconStatusIdle+" idle", dimStyle
	}
	sessionLines = append(sessionLines, mutedStyle.Render("state")+"        "+statusStyle.Render(statusText))
	if provider := strings.TrimSpace(string(task.Provider)); provider != "" {
		sessionLines = append(sessionLines,
			mutedStyle.Render("provider")+"     "+providerStyle(provider).Render(provider))
	}
	if launch := launchOptionsText(task.Launch()); launch != "" {
		sessionLines = append(sessionLines, mutedStyle.Render("launch")+"       "+primaryStyle.Render(launch))
	}
	for _, line := range zipColumns(scheduleLines, sessionLines, leftWidth) {
		builder.WriteString(line + "\n")
	}
	builder.WriteString("\n")

	lastRunLines := []string{headerLabelStyle.Render("LAST RUN")}
	if schedule != nil {
		lastRunLines = append(lastRunLines, lastRunDetailLines(*schedule, now, leftWidth)...)
	}
	workspaceLines := []string{headerLabelStyle.Render("WORKSPACE")}
	if strings.TrimSpace(task.BranchName) != "" {
		workspaceLines = append(workspaceLines, mutedStyle.Render("branch")+" "+primaryStyle.Render(
			truncateStr(task.BranchName, rightWidth-7)))
	}
	if strings.TrimSpace(task.RepoName) != "" {
		workspaceLines = append(workspaceLines, mutedStyle.Render("repo")+"   "+primaryStyle.Render(
			truncateStr(task.RepoName, rightWidth-7)))
	}
	if strings.TrimSpace(task.WorktreePath) != "" {
		workspaceLines = append(workspaceLines, mutedStyle.Render("path")+"   "+dimStyle.Render(
			truncateStr(task.WorktreePath, rightWidth-7)))
	}
	if window := strings.TrimSpace(task.TmuxWindow); window != "" {
		workspaceLines = append(workspaceLines, mutedStyle.Render("window")+" "+primaryStyle.Render(
			truncateStr(task.TmuxSession+":"+window, rightWidth-7)))
	}
	for _, line := range zipColumns(lastRunLines, workspaceLines, leftWidth) {
		builder.WriteString(line + "\n")
	}

	m.writeDetailExtras(&builder, row, false)
	return strings.TrimRight(builder.String(), "\n")
}

// nextRunDetail is when a schedule fires next, as a date and time, with how
// long that is when under a day, or that it is paused.
func nextRunDetail(schedule core.ScheduledTask, now time.Time) string {
	if !schedule.Enabled {
		return warningStyle.Render("paused") + mutedStyle.Render(" · press z to resume")
	}
	if schedule.NextRunAt.IsZero() {
		return dimStyle.Render("not scheduled")
	}
	text := primaryStyle.Render(scheduleDateTime(schedule.NextRunAt, now))
	if until := schedule.NextRunAt.Sub(now); until > 0 && until < 24*time.Hour {
		text += mutedStyle.Render(" · in " + untilText(until))
	}
	return text
}

// lastRunDetailLines is how the latest run went in full: when, the outcome,
// and the reason or note the run left, wrapped to width.
func lastRunDetailLines(schedule core.ScheduledTask, now time.Time, width int) []string {
	if schedule.LastOutcome == core.ScheduledRunNone {
		return []string{dimStyle.Render("not run yet")}
	}

	at := schedule.LastRunAt
	outcomeStyle := dimStyle
	switch schedule.LastOutcome {
	case core.ScheduledRunRunning:
		outcomeStyle = healthyStyle
		if !schedule.RunStartedAt.IsZero() {
			at = schedule.RunStartedAt
		}
	case core.ScheduledRunSkipped:
		outcomeStyle = warningStyle
	case core.ScheduledRunFailed:
		outcomeStyle = errorStyle
	case core.ScheduledRunCompleted, core.ScheduledRunNone:
	}

	lines := []string{mutedStyle.Render("result") + "  " + outcomeStyle.Render(string(schedule.LastOutcome))}
	if !at.IsZero() {
		when := scheduleDateTime(at, now)
		if ago := now.Sub(at); ago >= 0 {
			when += " · " + untilText(ago) + " ago"
		}
		lines = append(lines, mutedStyle.Render("when")+"    "+primaryStyle.Render(when))
	}
	const detailLines = 6
	wrapped := wrapFull(strings.TrimSpace(schedule.LastDetail), width-8)
	if len(wrapped) > detailLines {
		wrapped = append(wrapped[:detailLines-1], truncateVisible(wrapped[detailLines-1]+"...", width-8))
	}
	for index, line := range wrapped {
		label := "        "
		if index == 0 {
			label = mutedStyle.Render("why") + "     "
		}
		lines = append(lines, label+outcomeStyle.Render(line))
	}
	return append(lines, skippedBesideRunDetailLines(schedule, now, width)...)
}

// skippedBesideRunDetailLines is the skip of a run due while another was in
// flight, in full: when, and the reason, wrapped to width. It stays on the
// panel after the run it was due beside has ended, until the next run starts.
func skippedBesideRunDetailLines(schedule core.ScheduledTask, now time.Time, width int) []string {
	if schedule.RunSkippedAt.IsZero() {
		return nil
	}
	lines := []string{mutedStyle.Render("skipped") + " " + warningStyle.Render(
		scheduleDateTime(schedule.RunSkippedAt, now)+" · while a run was in progress")}
	wrapped := wrapFull(strings.TrimSpace(schedule.RunSkippedDetail), width-8)
	for index, line := range wrapped {
		label := "        "
		if index == 0 {
			label = mutedStyle.Render("why") + "     "
		}
		lines = append(lines, label+warningStyle.Render(line))
	}
	return lines
}

// scheduledText says that the scheduled list is shown, or how many Scheduled
// tasks there are.
func (m model) scheduledText() string {
	switch {
	case m.showScheduled:
		return "Scheduled tasks run their prompts on a schedule. Press s to return."
	case m.scheduledTasks == 1:
		return "1 scheduled task. Press s to see it."
	case m.scheduledTasks > 1:
		return fmt.Sprintf("%d scheduled tasks. Press s to see them.", m.scheduledTasks)
	default:
		return ""
	}
}

// scheduledKeybinds is the scheduled list's key bar, at most width wide once
// the window size is known.
func (m model) scheduledKeybinds(width int) string {
	pause := "pause"
	if _, schedule := m.selectedSchedule(); schedule != nil && !schedule.Enabled {
		pause = "resume"
	}
	binds := [][2]string{
		{"n", "new"}, {"e", "edit"}, {"r", "run now"}, {"z", pause}, {"d", "delete"}, {"s", "back"},
	}
	if row := m.selectedRow(); row != nil && row.task != nil &&
		row.task.CreationStatus == core.TaskCreationStatusFailed {
		binds = append(binds, [2]string{"R", "retry"})
	}
	binds = append(binds, [2]string{"space", "details"}, [2]string{"q", "quit"})
	return m.fitKeybindBar(width, binds, "space", "q", "d")
}

// scheduleCommandFailed is the message of a schedule command that went wrong.
func scheduleCommandFailed(action string, err error) error {
	return fmt.Errorf("%s: %w", action, err)
}

// loadSchedulesCmd loads the schedules of every Scheduled task.
func loadSchedulesCmd(ctx context.Context, frontend core.TaskFrontend) tea.Cmd {
	return func() tea.Msg {
		schedules, err := frontend.ListTaskSchedules(ctx)
		return schedulesLoadedMsg{schedules: schedules, err: err}
	}
}

// updateScheduleCmd saves an edited schedule. When the schedule was changed
// since it was read, the latest one comes back with the conflict.
func updateScheduleCmd(
	ctx context.Context,
	frontend core.TaskFrontend,
	taskID string,
	input core.ScheduleInput,
	expectedUpdatedAt time.Time,
) tea.Cmd {
	return func() tea.Msg {
		schedule, err := frontend.UpdateTaskSchedule(ctx, taskID, input, expectedUpdatedAt)
		msg := scheduleSavedMsg{taskID: taskID, schedule: schedule, err: err}
		if errors.Is(err, core.ErrScheduleConflict) {
			if schedules, listErr := frontend.ListTaskSchedules(ctx); listErr == nil {
				for _, latest := range schedules {
					if latest.TaskID == taskID {
						msg.latest = &latest
						break
					}
				}
			}
		}
		return msg
	}
}

// setScheduleEnabledCmd pauses a schedule or resumes it.
func setScheduleEnabledCmd(ctx context.Context, frontend core.TaskFrontend, taskID string, enabled bool) tea.Cmd {
	return func() tea.Msg {
		schedule, err := frontend.SetTaskScheduleEnabled(ctx, taskID, enabled)
		return scheduleEnabledMsg{taskID: taskID, schedule: schedule, enabled: enabled, err: err}
	}
}

// runScheduleNowCmd asks the daemon for a run now. The daemon makes the run on
// its own; how it went shows on the schedule.
func runScheduleNowCmd(ctx context.Context, frontend core.TaskFrontend, task *core.Task) tea.Cmd {
	return func() tea.Msg {
		return scheduleRunRequestedMsg{task: task, err: frontend.RunTaskScheduleNow(ctx, task.ID)}
	}
}

// schedulesLoadedMsg carries the schedules of every Scheduled task.
type schedulesLoadedMsg struct {
	err       error
	schedules []core.ScheduledTask
}

// scheduleSavedMsg reports an edit the daemon saved or refused. After a
// conflict, latest is the schedule as it is now, when it could be read.
type scheduleSavedMsg struct {
	err      error
	schedule *core.ScheduledTask
	latest   *core.ScheduledTask
	taskID   string
}

// scheduleEnabledMsg reports a schedule paused or resumed, or the refusal.
type scheduleEnabledMsg struct {
	err      error
	schedule *core.ScheduledTask
	taskID   string
	enabled  bool
}

// scheduleRunRequestedMsg reports a run the daemon took on, or refused.
type scheduleRunRequestedMsg struct {
	err  error
	task *core.Task
}

// updateScheduleMessage handles what the schedule commands return.
func (m model) updateScheduleMessage(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case schedulesLoadedMsg:
		// A schedule that cannot be read keeps the last one shown.
		if msg.err == nil {
			m.applySchedules(msg.schedules)
		}
		return m, nil, true
	case scheduleSavedMsg:
		next, cmd := m.scheduleSaved(msg)
		return next, cmd, true
	case scheduleEnabledMsg:
		m.endOp()
		if msg.err != nil {
			action := "pause"
			if msg.enabled {
				action = "resume"
			}
			// The schedule may have changed meanwhile: show it as it is now.
			m.err = scheduleCommandFailed(action, msg.err)
			return m, loadSchedulesCmd(m.statusContext, m.frontend), true
		}
		if msg.schedule != nil {
			m.applySchedule(*msg.schedule)
		}
		return m, nil, true
	case scheduleRunRequestedMsg:
		m.endOp()
		if msg.err != nil {
			m.err = scheduleCommandFailed("run now", msg.err)
			return m, nil, true
		}
		m.notice = fmt.Sprintf(
			"Run requested for %s. How it goes shows on its row.", emptyFallback(msg.task.DisplayName, msg.task.ID),
		)
		m.noticeAt = m.now()
		return m, nil, true
	default:
		return m, nil, false
	}
}

// scheduleNoticeLifetime is how long a note on a run that was requested stays
// on screen without a key press: the run is made in the daemon, and its outcome
// shows on its row, which the note must not stand in for once it is there.
const scheduleNoticeLifetime = 5 * time.Second

// expireNotice takes the note away once it has been shown for its lifetime.
func (m *model) expireNotice() {
	if m.notice != "" && !m.noticeAt.IsZero() && m.now().Sub(m.noticeAt) >= scheduleNoticeLifetime {
		m.notice, m.noticeAt = "", time.Time{}
	}
}

// scheduleStatusLine is the line shown under the list while a schedule command
// is in flight.
func (m model) scheduleStatusLine() string {
	switch m.pending {
	case opTogglingSchedule:
		if m.scheduleResuming {
			return stepActiveLine("Resuming the schedule...", m.shimmerTick)
		}
		return stepActiveLine("Pausing the schedule...", m.shimmerTick)
	case opRunningSchedule:
		return stepActiveLine("Asking for a run...", m.shimmerTick)
	default:
		return ""
	}
}
