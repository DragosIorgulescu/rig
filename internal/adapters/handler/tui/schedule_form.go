package tui

import (
	"errors"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/BaronBonet/rig/internal/core"
)

// scheduleField is the text field of the schedule form that has the cursor.
type scheduleField int

const (
	scheduleFieldPrompt scheduleField = iota
	scheduleFieldCron
)

// scheduleFormState is the working state of the two screens that choose a
// schedule: the step after the composer when a Scheduled task is created, and
// the editor of an existing schedule. Both choose a cron expression, a session
// mode and a permission mode; the editor also changes the prompt.
type scheduleFormState struct {
	err error
	// task is the Scheduled task being edited; nil while one is created.
	task *core.Task
	// version is the UpdatedAt of the schedule the edit started from; the
	// daemon refuses the edit if the schedule has changed since.
	version time.Time
	// prompt is the prompt being edited; unused while a task is created, whose
	// prompt is the composer's.
	prompt         textarea.Model
	cron           textinput.Model
	sessionMode    core.PromptRunSessionMode
	permissionMode core.TaskPermissionMode
	focus          scheduleField
}

// scheduleCronHints are the two lines under the cron field.
var scheduleCronHints = [2]string{
	"minute hour day month weekday, e.g. 0 9 * * 1-5 is weekdays at 09:00",
	"or @hourly, @daily, @weekly, @every 2h · local time",
}

// scheduleLabelWidth is the width of the labels down the left of the forms.
const scheduleLabelWidth = 13

// permissionModes are the permission modes in the order ctrl+p cycles them.
var permissionModes = []core.TaskPermissionMode{
	core.TaskPermissionModeAuto,
	core.TaskPermissionModeDefault,
	core.TaskPermissionModeAcceptEdits,
	core.TaskPermissionModeBypass,
}

// nextPermissionMode is the mode after current, wrapping around; an unknown
// current leads to the default mode.
func nextPermissionMode(current core.TaskPermissionMode) core.TaskPermissionMode {
	for index, mode := range permissionModes {
		if mode == current {
			return permissionModes[(index+1)%len(permissionModes)]
		}
	}
	return core.TaskPermissionModeAuto
}

// otherSessionMode is the session mode that is not mode.
func otherSessionMode(mode core.PromptRunSessionMode) core.PromptRunSessionMode {
	if mode == core.PromptRunSessionSame {
		return core.PromptRunSessionNew
	}
	return core.PromptRunSessionSame
}

func newScheduleCronInput(value string, width int) textinput.Model {
	input := newRenameInput(value, width)
	input.Placeholder = "0 9 * * 1-5"
	return input
}

// newCreateScheduleForm is the schedule step of a Scheduled task being created,
// with the defaults the daemon uses.
func newCreateScheduleForm(width int) scheduleFormState {
	return scheduleFormState{
		cron:           newScheduleCronInput("", width-8),
		sessionMode:    core.PromptRunSessionNew,
		permissionMode: core.TaskPermissionModeAuto,
		focus:          scheduleFieldCron,
	}
}

// newEditScheduleForm is the editor of a schedule, with what it has now.
func newEditScheduleForm(task *core.Task, schedule core.ScheduledTask, width int) scheduleFormState {
	prompt := newPromptInput()
	prompt.Placeholder = "What should every run do?"
	prompt.SetValue(schedule.Prompt)

	form := scheduleFormState{
		task:           task,
		version:        schedule.UpdatedAt,
		prompt:         prompt,
		cron:           newScheduleCronInput(schedule.Cron, width-8),
		sessionMode:    schedule.SessionMode,
		permissionMode: schedule.PermissionMode,
		focus:          scheduleFieldPrompt,
	}
	if form.sessionMode == "" {
		form.sessionMode = core.PromptRunSessionNew
	}
	if form.permissionMode == "" {
		form.permissionMode = core.TaskPermissionModeAuto
	}
	return form
}

// editing reports whether the form edits an existing schedule.
func (f *scheduleFormState) editing() bool {
	return f.task != nil
}

// focusField moves the cursor to a field.
func (f *scheduleFormState) focusField(field scheduleField) tea.Cmd {
	if field == scheduleFieldPrompt {
		f.focus = scheduleFieldPrompt
		f.cron.Blur()
		return f.prompt.Focus()
	}
	f.focus = scheduleFieldCron
	if f.editing() {
		f.prompt.Blur()
	}
	return f.cron.Focus()
}

// isScheduleFormMode reports whether the dashboard is on a schedule form.
func (m model) isScheduleFormMode() bool {
	return m.mode == modeScheduleStep || m.mode == modeScheduleEdit
}

// enterScheduledPromptMode opens the composer for a new Scheduled task: the
// prompt, provider, launch options and workspace are chosen as for any task,
// and the schedule step follows.
func (m model) enterScheduledPromptMode() (tea.Model, tea.Cmd) {
	if m.providerSetup == nil {
		return m.enterProviderSetupMode()
	}
	next, cmd := m.enterPromptInputMode("")
	entered, ok := next.(model)
	if !ok || entered.mode != modePromptInput {
		return next, cmd
	}
	entered.draft.scheduled = true
	entered.draft.input.Placeholder = "What should every run do?"
	entered.scheduleForm = newCreateScheduleForm(entered.totalWidth())
	return entered, cmd
}

// enterScheduleStep leaves the composer for the schedule step.
func (m model) enterScheduleStep() (model, tea.Cmd) {
	if !m.transition(modeScheduleStep) {
		return m, nil
	}
	m.draft.input.Blur()
	m.scheduleForm.err = nil
	return m, m.scheduleForm.focusField(scheduleFieldCron)
}

// enterScheduleEdit opens the editor for the selected schedule.
func (m model) enterScheduleEdit() (tea.Model, tea.Cmd) {
	row, schedule := m.selectedSchedule()
	if m.pending != opNone || row == nil {
		return m, nil
	}
	if schedule == nil {
		m.err = errors.New("the schedule has not loaded yet: try again in a moment")
		return m, nil
	}
	if !m.transition(modeScheduleEdit) {
		return m, nil
	}
	m.scheduleForm = newEditScheduleForm(row.task, *schedule, m.totalWidth())
	return m, m.scheduleForm.prompt.Focus()
}

// updateScheduleForm handles input on either schedule form.
func (m model) updateScheduleForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.pending != opNone {
		return m, nil
	}
	form := &m.scheduleForm

	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "esc":
			return m.handleBack()
		case "ctrl+t":
			form.sessionMode = otherSessionMode(form.sessionMode)
			return m, nil
		case "ctrl+p":
			form.permissionMode = nextPermissionMode(form.permissionMode)
			return m, nil
		case "tab", "shift+tab":
			if !form.editing() {
				return m, nil
			}
			if form.focus == scheduleFieldPrompt {
				return m, form.focusField(scheduleFieldCron)
			}
			return m, form.focusField(scheduleFieldPrompt)
		}
		if key.Key().Code == tea.KeyEnter && key.Mod == 0 {
			if form.editing() {
				return m.submitScheduleEdit()
			}
			return m.submitScheduledTask()
		}
	}

	switch msg.(type) {
	case tea.KeyPressMsg, tea.PasteMsg:
		// What was refused is stale once the user types; the cursor blinking
		// is not typing.
		form.err = nil
	}
	var cmd tea.Cmd
	if form.editing() && form.focus == scheduleFieldPrompt {
		form.prompt, cmd = form.prompt.Update(msg)
	} else {
		form.cron, cmd = form.cron.Update(msg)
	}
	return m, cmd
}

// scheduleInput is what the form has chosen, with the cron expression and the
// prompt trimmed. The prompt of a new task is the composer's.
func (m model) scheduleInput() core.ScheduleInput {
	form := m.scheduleForm
	prompt := m.promptValue()
	if form.editing() {
		prompt = form.prompt.Value()
	}
	return core.ScheduleInput{
		Cron:           strings.TrimSpace(form.cron.Value()),
		Prompt:         strings.TrimSpace(prompt),
		SessionMode:    form.sessionMode,
		PermissionMode: form.permissionMode,
	}
}

// checkScheduleInput reports what the form lacks. The daemon checks the rest,
// such as whether the expression means anything, and its error is shown on the
// form.
func checkScheduleInput(input core.ScheduleInput) error {
	switch {
	case input.Prompt == "":
		return errors.New("a scheduled task needs a prompt")
	case input.Cron == "":
		return errors.New("a schedule is required: for example 0 9 * * 1-5, or @daily")
	default:
		return nil
	}
}

// submitScheduledTask creates the Scheduled task the composer and the schedule
// step have described.
func (m model) submitScheduledTask() (tea.Model, tea.Cmd) {
	input := m.scheduleInput()
	if err := checkScheduleInput(input); err != nil {
		m.scheduleForm.err = err
		return m, nil
	}

	// Capture the draft's inputs before the transition discards it.
	cwd := m.currentCreateCwd()
	provider := m.effectiveCreateProvider()
	options := core.LaunchOptions{Model: m.draft.model, Effort: m.draft.effort}
	var workspace core.WorkspaceKind
	if m.draft.inFolder {
		workspace = core.WorkspaceKindFolder
	}
	submitted := m.draft
	form := m.scheduleForm
	m.transition(modeBrowse)
	m.beginOp(opCreating)
	// The draft and the form are kept, so a schedule the daemon refuses reopens
	// the step with what was typed.
	m.create = createFlowState{retry: &submitted, schedule: &form}
	m.rememberLaunchDefaults(provider, options)

	return m, tea.Batch(
		createTaskStreamCmd(m.statusContext, m.frontend, core.CreateTaskInput{
			Cwd:         cwd,
			Prompt:      input.Prompt,
			Provider:    provider,
			Workspace:   workspace,
			ProviderEnv: m.providerEnv,
			Model:       options.Model,
			Effort:      options.Effort,
			Schedule:    &input,
		}),
		shimmerTickCmd(),
	)
}

// reopenScheduleStep reopens the schedule step of a Scheduled task that was not
// made, with the error above what was typed.
func (m model) reopenScheduleStep(err error) (model, tea.Cmd) {
	retry, form := m.create.retry, m.create.schedule
	if !m.transition(modeScheduleStep) {
		m.create.err = err
		return m, nil
	}
	m.draft = *retry
	m.scheduleForm = *form
	m.scheduleForm.err = err
	m.create = createFlowState{}
	return m, m.scheduleForm.focusField(scheduleFieldCron)
}

// submitScheduleEdit saves the edited schedule.
func (m model) submitScheduleEdit() (tea.Model, tea.Cmd) {
	input := m.scheduleInput()
	if err := checkScheduleInput(input); err != nil {
		m.scheduleForm.err = err
		return m, nil
	}
	task := m.scheduleForm.task
	if current := m.taskRowByID(taskID(task)); current != nil && current.schedule != nil &&
		scheduleUnchanged(*current.schedule, input) {
		m.transition(modeBrowse)
		return m, nil
	}
	m.beginOp(opEditingSchedule)
	return m, tea.Batch(
		updateScheduleCmd(m.statusContext, m.frontend, taskID(task), input, m.scheduleForm.version),
		shimmerTickCmd(),
	)
}

// scheduleUnchanged reports whether an edit would leave the schedule as it is.
func scheduleUnchanged(schedule core.ScheduledTask, input core.ScheduleInput) bool {
	sessionMode, permissionMode := schedule.SessionMode, schedule.PermissionMode
	if sessionMode == "" {
		sessionMode = core.PromptRunSessionNew
	}
	if permissionMode == "" {
		permissionMode = core.TaskPermissionModeAuto
	}
	return strings.TrimSpace(schedule.Cron) == input.Cron &&
		strings.TrimSpace(schedule.Prompt) == input.Prompt &&
		sessionMode == input.SessionMode &&
		permissionMode == input.PermissionMode
}

// scheduleSaved applies the daemon's answer to an edit. A conflict, an edit
// made elsewhere since the editor opened, shows the schedule as it is now, so
// the edit is made again on top of it.
func (m model) scheduleSaved(msg scheduleSavedMsg) (tea.Model, tea.Cmd) {
	m.endOp()
	editing := taskID(m.scheduleForm.task) == msg.taskID
	switch {
	case msg.err == nil:
		if msg.schedule != nil {
			m.applySchedule(*msg.schedule)
		}
		if editing {
			m.transition(modeBrowse)
		}
		return m, nil
	case errors.Is(msg.err, core.ErrScheduleConflict) && msg.latest != nil:
		m.applySchedule(*msg.latest)
		if !editing {
			return m, nil
		}
		m.scheduleForm = newEditScheduleForm(m.scheduleForm.task, *msg.latest, m.totalWidth())
		m.scheduleForm.err = errors.New("the schedule was changed elsewhere since you opened it: " +
			"the latest version is shown, so make your edit again")
		return m, m.scheduleForm.prompt.Focus()
	default:
		if editing {
			m.scheduleForm.err = msg.err
		} else {
			m.err = msg.err
		}
		return m, nil
	}
}

// scheduleLine is a labelled line of the schedule forms.
func scheduleLine(label string, value string) string {
	return mutedStyle.Render(padRightVisible(label, scheduleLabelWidth)) + value
}

// scheduleChoiceLine shows one choice of the form and the key that changes it,
// followed by what the choice means.
func scheduleChoiceLine(label string, value string, key string, hint string) string {
	return scheduleLine(label, primaryStyle.Render(value)) +
		mutedStyle.Render("  ·  ") + keybindStyle.Render(key) + mutedStyle.Render(" "+hint)
}

// scheduleFormFields renders the cron field and the two choices.
func (m model) scheduleFormFields() string {
	form := m.scheduleForm
	var builder strings.Builder
	hintWidth := m.totalWidth() - scheduleLabelWidth - 2
	indent := strings.Repeat(" ", scheduleLabelWidth)
	builder.WriteString(scheduleLine("schedule", form.cron.View()) + "\n")
	for _, hint := range scheduleCronHints {
		builder.WriteString(indent + dimStyle.Render(truncateStr(hint, hintWidth)) + "\n")
	}
	builder.WriteString("\n")
	builder.WriteString(scheduleChoiceLine("session", string(form.sessionMode), "ctrl+t", "switch") + "\n")
	builder.WriteString(indent + dimStyle.Render(truncateStr(scheduleModeText(form.sessionMode), hintWidth)) + "\n")
	builder.WriteString(scheduleChoiceLine("permissions", string(form.permissionMode), "ctrl+p", "cycle") + "\n")
	builder.WriteString(indent + dimStyle.Render(
		truncateStr(schedulePermissionText(form.permissionMode), hintWidth)) + "\n")
	return builder.String()
}

// scheduleStepView is the schedule step of creating a Scheduled task.
func (m model) scheduleStepView() string {
	var builder strings.Builder
	builder.WriteString(m.screenHeader(mutedStyle.Render("new scheduled task · schedule")))
	builder.WriteString(errorBlock(m.scheduleForm.err))
	builder.WriteString(dimStyle.Render("When the task runs its prompt, and how.") + "\n\n")

	provider := string(m.effectiveCreateProvider())
	providerLine := providerStyle(provider).Render(provider)
	if launch := launchOptionsText(core.LaunchOptions{Model: m.draft.model, Effort: m.draft.effort}); launch != "" {
		providerLine += mutedStyle.Render("  ·  ") + primaryStyle.Render(launch)
	}
	builder.WriteString(scheduleLine("prompt", primaryStyle.Render(
		truncateStr(firstLine(m.promptValue()), m.totalWidth()-scheduleLabelWidth-2))) + "\n")
	builder.WriteString(scheduleLine("provider", providerLine) + "\n")
	builder.WriteString(scheduleLine("workspace", primaryStyle.Render(m.draftWorkspaceText())) + "\n\n")

	builder.WriteString(m.scheduleFormFields() + "\n")
	builder.WriteString(footerKeybinds(
		[2]string{"enter", "create"},
		[2]string{"ctrl+t", "session"},
		[2]string{"ctrl+p", "permissions"},
		[2]string{"esc", "back"},
	))
	return builder.String()
}

// scheduleEditView is the editor of an existing schedule.
func (m model) scheduleEditView() string {
	form := m.scheduleForm
	var builder strings.Builder
	builder.WriteString(m.screenHeader(mutedStyle.Render("edit schedule")))
	if task := form.task; task != nil {
		builder.WriteString(
			mutedStyle.Render("task  ") + primaryStyle.Render(emptyFallback(task.DisplayName, task.ID)) + "\n\n",
		)
	}
	if m.pending == opEditingSchedule {
		builder.WriteString(renderShimmer("Saving the schedule...", m.shimmerTick) + "\n")
		return builder.String()
	}
	builder.WriteString(errorBlock(form.err))

	promptBoxWidth := max(m.totalWidth()-4, 20)
	input := form.prompt
	input.SetWidth(max(promptBoxWidth-promptBoxStyle.GetHorizontalFrameSize(), 1))
	builder.WriteString(mutedStyle.Render("prompt") + "\n")
	builder.WriteString(promptBoxStyle.Width(promptBoxWidth).Render(input.View()) + "\n\n")

	builder.WriteString(m.scheduleFormFields() + "\n")
	builder.WriteString(footerKeybinds(
		[2]string{"enter", "save"},
		[2]string{"tab", "next field"},
		[2]string{"alt+enter", "newline"},
		[2]string{"ctrl+t", "session"},
		[2]string{"ctrl+p", "permissions"},
		[2]string{"esc", "cancel"},
	))
	return builder.String()
}

// firstLine is the first line of text that has anything on it.
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
