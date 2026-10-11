package core

import (
	"errors"
	"fmt"
	"time"
)

// TaskKind says what sort of Task a Task is. The zero value is an ordinary
// Task the user works in; the other kinds have a list of their own.
type TaskKind string

const (
	// TaskKindScheduled is a Task that runs a stored prompt on a schedule. It
	// has one tmux window and no child sessions, and is never shelved.
	TaskKindScheduled TaskKind = "scheduled"
)

// IsScheduled reports whether the Task is a Scheduled task.
func (t *Task) IsScheduled() bool {
	return t != nil && t.Kind == TaskKindScheduled
}

// DefaultScheduleMinInterval is the shortest time allowed between two runs of
// a schedule, so a typo cannot start a provider session every minute.
const DefaultScheduleMinInterval = 5 * time.Minute

// DefaultSchedulerTick is how often the scheduler looks for runs that are due.
const DefaultSchedulerTick = 30 * time.Second

// ScheduleInput is what the user chooses for a Scheduled task: when it runs,
// what it is asked, and how its sessions are used. It is the schedule of a
// new Scheduled task and what an edit replaces.
type ScheduleInput struct {
	// Cron is a standard five-field cron expression or a descriptor such as
	// "@daily" or "@every 2h", in local time.
	Cron   string `json:"cron"`
	Prompt string `json:"prompt"`
	// SessionMode defaults to PromptRunSessionNew.
	SessionMode PromptRunSessionMode `json:"session_mode,omitempty"`
	// PermissionMode defaults to TaskPermissionModeAuto.
	PermissionMode TaskPermissionMode `json:"permission_mode,omitempty"`
}

// ScheduledRunOutcome says how the latest run of a Scheduled task went.
type ScheduledRunOutcome string

const (
	// ScheduledRunNone is the outcome of a Scheduled task that has not run.
	ScheduledRunNone ScheduledRunOutcome = ""
	// ScheduledRunRunning means the provider took the prompt and has not yet
	// reported the turn over.
	ScheduledRunRunning ScheduledRunOutcome = "running"
	// ScheduledRunCompleted means the provider ended the turn the run began.
	ScheduledRunCompleted ScheduledRunOutcome = "completed"
	// ScheduledRunSkipped means the run was left alone, normally because the
	// session was busy.
	ScheduledRunSkipped ScheduledRunOutcome = "skipped"
	// ScheduledRunFailed means the prompt did not get to the provider or the
	// turn ended on an error; the detail says why.
	ScheduledRunFailed ScheduledRunOutcome = "failed"
)

// ScheduledRunState says whether a Scheduled task has a run in flight.
type ScheduledRunState string

const (
	// ScheduledRunStateIdle is a Scheduled task with no run in flight.
	ScheduledRunStateIdle ScheduledRunState = ""
	// ScheduledRunStateRunning is a Scheduled task whose latest run started
	// and has not ended yet.
	ScheduledRunStateRunning ScheduledRunState = "running"
)

// ScheduledTask is the schedule of a Scheduled task, kept beside its Task
// record and gone with it.
type ScheduledTask struct {
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt changes whenever the schedule is edited, enabled or paused. A
	// caller that edits a schedule names the UpdatedAt it read, so an edit
	// made meanwhile elsewhere is not overwritten.
	UpdatedAt time.Time `json:"updated_at"`
	// NextRunAt is when the schedule fires next; meaningless while paused.
	NextRunAt time.Time `json:"next_run_at,omitzero"`
	// RunStartedAt is when the run in flight began; zero with none.
	RunStartedAt time.Time `json:"run_started_at,omitzero"`
	// LastRunAt is when the latest run was made, whatever came of it.
	LastRunAt time.Time `json:"last_run_at,omitzero"`
	TaskID    string    `json:"task_id"`
	ScheduleInput
	// Enabled is false while the schedule is paused.
	Enabled  bool              `json:"enabled"`
	RunState ScheduledRunState `json:"run_state,omitempty"`
	// LastOutcome and LastDetail say how the latest run went, and why when it
	// was skipped or failed.
	LastOutcome ScheduledRunOutcome `json:"last_outcome,omitempty"`
	LastDetail  string              `json:"last_detail,omitempty"`
	// RunSkippedAt and RunSkippedDetail say when and why the latest run due
	// while another was in flight was skipped. That run's own record is left as
	// it is, so the skip is kept beside it, until the next run starts.
	RunSkippedAt     time.Time `json:"run_skipped_at,omitzero"`
	RunSkippedDetail string    `json:"run_skipped_detail,omitempty"`
}

// IsRunning reports whether a run of the schedule is in flight.
func (s ScheduledTask) IsRunning() bool {
	return s.RunState == ScheduledRunStateRunning
}

var (
	// ErrScheduleConflict reports that a schedule was edited since the caller
	// read it.
	ErrScheduleConflict = errors.New("the schedule was changed since it was read")
	// ErrScheduleNotFound reports that a Task has no schedule.
	ErrScheduleNotFound = errors.New("schedule not found")
	// ErrTaskScheduled reports an action refused because the Task is a
	// Scheduled task.
	ErrTaskScheduled = errors.New("not available for a scheduled task")
)

func refuseScheduledTask(task *Task, action string) error {
	if task.IsScheduled() {
		return fmt.Errorf("%q is a scheduled task: it cannot %s: %w", task.DisplayName, action, ErrTaskScheduled)
	}
	return nil
}

// maxRunDetailRunes caps the reason recorded for a run: it is shown on one line
// of the dashboard, and a provider's error can be a page long.
const maxRunDetailRunes = 240

// compactDetail puts text on one line and cuts it to at most limit characters.
func compactDetail(text string, limit int) string {
	text = compactActivityText(text)
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return text
}

// scheduledRunEnd says how a status update ends the run in flight, if it
// does. A turn that ends while subagents or workflows work in the background
// is not over: they bring the agent back for another turn, whose Stop ends it.
func scheduledRunEnd(update TaskStatusUpdate, errorText string) (ScheduledRunOutcome, string, bool) {
	switch {
	case update.Phase == TaskStatusPhaseStopped || update.RawEventName == HookEventSessionEnd:
		return ScheduledRunFailed, "the session ended", true
	case update.RawEventName == HookEventStopFailure:
		if errorText = compactDetail(errorText, maxRunDetailRunes); errorText != "" {
			return ScheduledRunFailed, errorText, true
		}
		return ScheduledRunFailed, "the agent stopped on an error", true
	case update.RawEventName == HookEventStop && update.Phase != TaskStatusPhaseWorkingInBackground:
		return ScheduledRunCompleted, "", true
	default:
		return "", "", false
	}
}
