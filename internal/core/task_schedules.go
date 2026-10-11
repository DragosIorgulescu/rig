package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// taskSchedules is the Scheduled task module: the schedules of Scheduled
// tasks, the scheduler that fires them, and how a run that started is later
// resolved from the hook events of its session.
//
// A schedule is kept beside its Task record. The scheduler claims each run
// with a compare-and-swap on the schedule's next run, so a run is made once
// however many callers look at the same schedule, and a run that starts is
// recorded as running until a hook says its turn is over.
type taskSchedules struct {
	tasks      TaskRepository
	launcher   *sessionLauncher
	calculator ScheduleCalculator
	// run makes one prompt run; it is the service's RunTaskPrompt.
	run func(context.Context, RunTaskPromptInput) (RunTaskPromptResult, error)
	// now is the scheduler's clock. It decides when a schedule is due and
	// stamps the schedule records; tests set it.
	now func() time.Time
	// report is told of failures nobody is waiting on, such as a scheduler
	// tick that panicked; it must not be nil.
	report func(error)
	// minInterval is the shortest time allowed between two runs of a schedule.
	minInterval time.Duration
	// tick is how often the scheduler looks for runs that are due.
	tick time.Duration
	// pokes wake the scheduler, so an edit takes effect before the next tick.
	pokes chan struct{}

	mu sync.Mutex
	// inFlight holds the Tasks with a run being made by this process, which
	// start and wait for the provider to take the prompt.
	inFlight map[string]bool
	// runCtx is the context scheduled runs are made under: the one the
	// scheduler loop runs with, or Background before it.
	runCtx context.Context
	runs   sync.WaitGroup
}

func newTaskSchedules(
	tasks TaskRepository,
	launcher *sessionLauncher,
	calculator ScheduleCalculator,
	minInterval time.Duration,
	tick time.Duration,
	report func(error),
) *taskSchedules {
	if report == nil {
		report = func(error) {}
	}
	return &taskSchedules{
		tasks:       tasks,
		launcher:    launcher,
		calculator:  calculator,
		now:         time.Now,
		report:      report,
		minInterval: cmp.Or(minInterval, DefaultScheduleMinInterval),
		tick:        cmp.Or(tick, DefaultSchedulerTick),
		pokes:       make(chan struct{}, 1),
		inFlight:    make(map[string]bool),
		runCtx:      context.Background(),
	}
}

var errSchedulingNotConfigured = errors.New("scheduled tasks are not configured")

// prepare checks the schedule a new Scheduled task is created with and returns
// its row, due for the first time at the schedule's next time. A prompt the
// schedule leaves empty is fallbackPrompt, the one the task is created with.
// The provider is the Task's own, which has to accept the permission mode.
func (m *taskSchedules) prepare(
	input ScheduleInput,
	fallbackPrompt string,
	task *Task,
	providerClient ProviderClient,
) (*ScheduledTask, error) {
	if strings.TrimSpace(input.Prompt) == "" {
		input.Prompt = fallbackPrompt
	}
	normalized, next, err := m.normalize(input)
	if err != nil {
		return nil, err
	}
	if err := checkPermissionMode(providerClient, task, normalized.PermissionMode); err != nil {
		return nil, err
	}

	now := m.now().UTC()
	return &ScheduledTask{
		ScheduleInput: normalized,
		Enabled:       true,
		NextRunAt:     next,
		CreatedAt:     now,
		UpdatedAt:     now,
	}, nil
}

// normalize checks a schedule and returns it with its defaults filled in, and
// when it fires next. The user is told what is wrong with it in these errors.
func (m *taskSchedules) normalize(input ScheduleInput) (ScheduleInput, time.Time, error) {
	if m.calculator == nil {
		return ScheduleInput{}, time.Time{}, errSchedulingNotConfigured
	}
	input.Cron = strings.TrimSpace(input.Cron)
	if input.Cron == "" {
		return ScheduleInput{}, time.Time{}, fmt.Errorf("schedule is required")
	}
	if err := checkPromptRunPrompt(input.Prompt); err != nil {
		return ScheduleInput{}, time.Time{}, err
	}
	sessionMode, err := parsePromptRunSessionMode(input.SessionMode)
	if err != nil {
		return ScheduleInput{}, time.Time{}, err
	}
	permissionMode, err := ParseTaskPermissionMode(string(input.PermissionMode))
	if err != nil {
		return ScheduleInput{}, time.Time{}, err
	}
	input.SessionMode, input.PermissionMode = sessionMode, permissionMode

	if err := m.calculator.Validate(input.Cron); err != nil {
		return ScheduleInput{}, time.Time{}, fmt.Errorf("schedule %q: %w", input.Cron, err)
	}
	next, err := m.calculator.Next(input.Cron, m.now())
	if err != nil {
		return ScheduleInput{}, time.Time{}, fmt.Errorf("schedule %q: %w", input.Cron, err)
	}
	if err := m.checkInterval(input.Cron, next); err != nil {
		return ScheduleInput{}, time.Time{}, err
	}
	return input, next.UTC(), nil
}

// checkInterval rejects a schedule that runs again within the minimum
// interval, looking at the next few runs after first. A schedule that stops
// firing within them is checked as far as it goes.
func (m *taskSchedules) checkInterval(expr string, first time.Time) error {
	runs := m.upcomingRuns(expr, first)
	for i := 1; i < len(runs); i++ {
		if gap := runs[i].Sub(runs[i-1]); gap < m.minInterval {
			return fmt.Errorf(
				"schedule %q runs again after %s: runs must be at least %s apart",
				expr, gap.Round(time.Second), m.minInterval,
			)
		}
	}
	return nil
}

// upcomingRuns is first and the runs of the schedule after it, as many as are
// checked against the minimum interval; fewer when the schedule stops firing.
func (m *taskSchedules) upcomingRuns(expr string, first time.Time) []time.Time {
	runs := []time.Time{first}
	for len(runs) <= maxCheckedScheduleRuns {
		next, err := m.calculator.Next(expr, runs[len(runs)-1])
		if err != nil {
			break
		}
		runs = append(runs, next)
	}
	return runs
}

// checkPermissionMode asks the Task's provider whether it can run with the
// permission mode, so a mode it refuses is reported when the schedule is made
// rather than when the first run fails.
func checkPermissionMode(providerClient ProviderClient, task *Task, mode TaskPermissionMode) error {
	if _, err := providerClient.BuildPromptRunLaunchSpec(task, PromptRunLaunch{PermissionMode: mode}); err != nil {
		return fmt.Errorf("permission mode %q: %w", mode, err)
	}
	return nil
}

// insert stores the schedule of a Task just created. When it cannot be stored
// the Task is removed again, so no Scheduled task is left without a schedule.
func (m *taskSchedules) insert(ctx context.Context, task *Task, schedule *ScheduledTask) error {
	schedule.TaskID = task.ID
	if err := m.tasks.CreateScheduledTask(ctx, *schedule); err != nil {
		cleanupErr := m.tasks.DeleteTask(context.WithoutCancel(ctx), task.ID)
		return errors.Join(fmt.Errorf("store the schedule: %w", err), cleanupErr)
	}
	return nil
}

func (s *service) ListTaskSchedules(ctx context.Context) ([]ScheduledTask, error) {
	return s.tasks.ListScheduledTasks(ctx)
}

// UpdateTaskSchedule replaces the editable parts of a schedule and works out
// the next run again from now, so an edit never leaves a run due in the past.
func (s *service) UpdateTaskSchedule(
	ctx context.Context,
	taskID string,
	update ScheduleInput,
	expectedUpdatedAt time.Time,
) (*ScheduledTask, error) {
	m := s.schedules
	task, schedule, err := m.load(ctx, taskID)
	if err != nil {
		return nil, err
	}
	normalized, next, err := m.normalize(update)
	if err != nil {
		return nil, err
	}
	_, providerClient, err := s.launcher.resolveProvider(ctx, task.Provider)
	if err != nil {
		return nil, err
	}
	if err := checkPermissionMode(providerClient, task, normalized.PermissionMode); err != nil {
		return nil, err
	}

	schedule.ScheduleInput = normalized
	schedule.NextRunAt = next
	schedule.UpdatedAt = m.stamp(expectedUpdatedAt)
	if err := s.tasks.UpdateScheduledTask(ctx, *schedule, expectedUpdatedAt); err != nil {
		return nil, err
	}
	m.poke()
	return s.tasks.GetScheduledTask(ctx, task.ID)
}

// SetTaskScheduleEnabled pauses a schedule or resumes it. A resumed schedule
// runs next at its next time from now: the runs it missed while paused are not
// made up.
func (s *service) SetTaskScheduleEnabled(ctx context.Context, taskID string, enabled bool) (*ScheduledTask, error) {
	m := s.schedules
	_, schedule, err := m.load(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if schedule.Enabled == enabled {
		return schedule, nil
	}

	expectedUpdatedAt := schedule.UpdatedAt
	schedule.Enabled = enabled
	if enabled {
		_, next, err := m.normalize(schedule.ScheduleInput)
		if err != nil {
			return nil, err
		}
		schedule.NextRunAt = next
	}
	schedule.UpdatedAt = m.stamp(expectedUpdatedAt)
	if err := s.tasks.UpdateScheduledTask(ctx, *schedule, expectedUpdatedAt); err != nil {
		return nil, err
	}
	m.poke()
	return s.tasks.GetScheduledTask(ctx, schedule.TaskID)
}

// RunTaskScheduleNow starts a run of the schedule in the daemon and returns
// without waiting for it. The run is made as a scheduled one is, whether or
// not the schedule is paused, and leaves the next scheduled run as it was.
func (s *service) RunTaskScheduleNow(ctx context.Context, taskID string) error {
	m := s.schedules
	_, schedule, err := m.load(ctx, taskID)
	if err != nil {
		return err
	}
	if !m.start(m.context(), *schedule) {
		return fmt.Errorf("the schedule is already being run")
	}
	return nil
}

// load reads a Scheduled task and its schedule.
func (m *taskSchedules) load(ctx context.Context, taskID string) (*Task, *ScheduledTask, error) {
	task, err := taskByID(ctx, m.tasks, taskID)
	if err != nil {
		return nil, nil, err
	}
	schedule, err := m.tasks.GetScheduledTask(ctx, task.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("read the schedule: %w", err)
	}
	if schedule == nil {
		return nil, nil, fmt.Errorf("%q has no schedule: %w", task.DisplayName, ErrScheduleNotFound)
	}
	return task, schedule, nil
}

// stamp is the UpdatedAt of an edit of a schedule last updated at previous: the
// clock's time, and always later than previous so that an edit is never taken
// for the version before it.
func (m *taskSchedules) stamp(previous time.Time) time.Time {
	stamped := m.now().UTC()
	if !stamped.After(previous) {
		stamped = previous.Add(time.Nanosecond)
	}
	return stamped
}

// poke wakes the scheduler.
func (m *taskSchedules) poke() {
	select {
	case m.pokes <- struct{}{}:
	default:
	}
}

// context is what a run made on request, rather than by the loop, runs under.
func (m *taskSchedules) context() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runCtx
}

// resolveRun ends the run in flight of the Task a status update is for, when
// the update says its turn is over. It is called for every status a hook
// drives: Stop completes the run, StopFailure fails it with the error the
// event gave, and SessionEnd fails it as the session ended. A Task with no run
// in flight, which is every ordinary Task, is left alone.
func (m *taskSchedules) resolveRun(ctx context.Context, update TaskStatusUpdate, errorText string) {
	outcome, detail, ends := scheduledRunEnd(update, errorText)
	if !ends {
		return
	}
	if _, err := m.tasks.ResolveScheduledRun(ctx, update.TaskID, outcome, detail); err != nil {
		m.report(fmt.Errorf("resolve the scheduled run of task %s: %w", update.TaskID, err))
	}
}

// settleRun ends a run that started if the session's turn is already over by
// the time the run is recorded: a short turn can end before the prompt run
// that sent the prompt returns, and its Stop hook then found no run in flight.
func (m *taskSchedules) settleRun(ctx context.Context, taskID string, startedAt time.Time) {
	if startedAt.IsZero() {
		return
	}
	status, err := m.tasks.LatestTaskStatus(ctx, taskID)
	if err != nil || status == nil || !status.ObservedAt.After(startedAt) {
		return
	}
	m.resolveRun(ctx, *status, "")
}
