package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeCalculator is a ScheduleCalculator for tests, over four kinds of
// expression in UTC:
//
//	"every 10m"        fires on every multiple of the duration
//	"daily 09:00"      fires once a day at that time
//	"minutes 0,10,12"  fires at those minutes of every hour
//	"never"            parses, but does not fire
//	"zoned"            fires like "every 10m", but Validate refuses it
//	"late"             passes Validate, but has no next time
//
// Anything else is not a schedule, and "panic" panics.
type fakeCalculator struct{}

func (fakeCalculator) next(expr string) (func(time.Time) time.Time, error) {
	fields := strings.Fields(expr)
	switch {
	case expr == "panic":
		panic("the calculator is broken")
	case expr == "never":
		return func(time.Time) time.Time { return time.Time{} }, nil
	case expr == "late":
		return func(time.Time) time.Time { return time.Time{} }, nil
	case expr == "zoned":
		return func(after time.Time) time.Time { return after.Truncate(10 * time.Minute).Add(10 * time.Minute) }, nil
	case len(fields) == 2 && fields[0] == "every":
		interval, err := time.ParseDuration(fields[1])
		if err != nil || interval <= 0 {
			return nil, errors.New("not a cron schedule")
		}
		return func(after time.Time) time.Time { return after.Truncate(interval).Add(interval) }, nil
	case len(fields) == 2 && fields[0] == "daily":
		at, err := time.Parse("15:04", fields[1])
		if err != nil {
			return nil, errors.New("not a cron schedule")
		}
		return func(after time.Time) time.Time {
			day := time.Date(after.Year(), after.Month(), after.Day(), at.Hour(), at.Minute(), 0, 0, time.UTC)
			if !day.After(after) {
				day = day.AddDate(0, 0, 1)
			}
			return day
		}, nil
	case len(fields) == 2 && fields[0] == "minutes":
		var minutes []int
		for _, field := range strings.Split(fields[1], ",") {
			var minute int
			if _, err := fmt.Sscanf(field, "%d", &minute); err != nil {
				return nil, errors.New("not a cron schedule")
			}
			minutes = append(minutes, minute)
		}
		return func(after time.Time) time.Time {
			hour := after.Truncate(time.Hour)
			for ; ; hour = hour.Add(time.Hour) {
				for _, minute := range minutes {
					if at := hour.Add(time.Duration(minute) * time.Minute); at.After(after) {
						return at
					}
				}
			}
		}, nil
	default:
		return nil, errors.New("not a cron schedule")
	}
}

func (c fakeCalculator) Validate(expr string) error {
	if expr == "zoned" {
		return errors.New("time zones are not supported")
	}
	if expr == "late" {
		return nil
	}
	next, err := c.next(expr)
	if err != nil {
		return err
	}
	if next(time.Now()).IsZero() {
		return errors.New("it never fires")
	}
	return nil
}

func (c fakeCalculator) Next(expr string, after time.Time) (time.Time, error) {
	next, err := c.next(expr)
	if err != nil {
		return time.Time{}, err
	}
	at := next(after.UTC())
	if at.IsZero() {
		return time.Time{}, errors.New("it never fires")
	}
	return at, nil
}

// fakeClock is a clock that moves when a test says so.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// stubRunner stands in for RunTaskPrompt: it records the runs it is asked
// for, answers each with result and err, and holds each until release is
// closed, when it is set.
type stubRunner struct {
	mu       sync.Mutex
	inputs   []RunTaskPromptInput
	contexts []context.Context
	result   RunTaskPromptResult
	err      error
	release  chan struct{}
	started  chan struct{}
	panics   bool
	// ignoresContext holds a run until release even when its context ends.
	ignoresContext bool
}

func (r *stubRunner) run(ctx context.Context, input RunTaskPromptInput) (RunTaskPromptResult, error) {
	r.mu.Lock()
	r.inputs = append(r.inputs, input)
	r.contexts = append(r.contexts, ctx)
	result, err, release, started, panics := r.result, r.err, r.release, r.started, r.panics
	ignoresContext := r.ignoresContext
	r.mu.Unlock()

	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if panics {
		panic("the run is broken")
	}
	if release != nil {
		if ignoresContext {
			<-release
		} else {
			select {
			case <-release:
			case <-ctx.Done():
				return RunTaskPromptResult{}, ctx.Err()
			}
		}
	}
	return result, err
}

// contextOfRun is the context the n-th run was made with.
func (r *stubRunner) contextOfRun(n int) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.contexts[n]
}

func (r *stubRunner) runs() []RunTaskPromptInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RunTaskPromptInput(nil), r.inputs...)
}

// scheduleFixture is a ready Scheduled task, "task-1", whose daily schedule is
// due at 09:00 on a day whose clock reads 08:00.
type scheduleFixture struct {
	*testTaskServiceHarness
	// defaultRun is how the service makes a run before the fixture's stub
	// stands in for it.
	defaultRun func(context.Context, RunTaskPromptInput) (RunTaskPromptResult, error)
	task       *Task
	clock      *fakeClock
	runner     *stubRunner
	reports    []error
	mu         sync.Mutex
}

var (
	scheduleDay   = time.Date(2026, time.October, 12, 0, 0, 0, 0, time.UTC)
	scheduleNine  = scheduleDay.Add(9 * time.Hour)
	scheduleStart = scheduleDay.Add(8 * time.Hour)
)

func newScheduleFixture(t *testing.T) *scheduleFixture {
	t.Helper()

	svc, task := promptRunFixture(t)
	task.Kind = TaskKindScheduled
	task.Prompt = ""
	defaultRun := svc.service.schedules.run
	f := &scheduleFixture{
		testTaskServiceHarness: svc,
		defaultRun:             defaultRun,
		task:                   task,
		clock:                  &fakeClock{now: scheduleStart},
		runner: &stubRunner{result: RunTaskPromptResult{
			Outcome: PromptRunStarted, Detail: "started fresh", StartedAt: time.Now(),
		}},
	}
	m := svc.service.schedules
	m.now = f.clock.Now
	m.run = f.runner.run
	m.report = func(err error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reports = append(f.reports, err)
	}
	f.putSchedule(ScheduledTask{
		TaskID: "task-1",
		ScheduleInput: ScheduleInput{
			Cron:           "daily 09:00",
			Prompt:         "triage the open issues",
			SessionMode:    PromptRunSessionSame,
			PermissionMode: TaskPermissionModeAcceptEdits,
		},
		Enabled:   true,
		NextRunAt: scheduleNine,
		CreatedAt: scheduleStart.Add(-time.Hour),
		UpdatedAt: scheduleStart.Add(-time.Hour),
	})
	return f
}

// putSchedule stores a schedule as if it had been created with its Task.
func (f *scheduleFixture) putSchedule(schedule ScheduledTask) {
	f.taskRepo.mu.Lock()
	defer f.taskRepo.mu.Unlock()
	if _, exists := f.taskRepo.schedules[schedule.TaskID]; !exists {
		f.taskRepo.scheduleOrder = append(f.taskRepo.scheduleOrder, schedule.TaskID)
	}
	f.taskRepo.schedules[schedule.TaskID] = schedule
}

// schedule is the stored schedule of the fixture's Task.
func (f *scheduleFixture) schedule() ScheduledTask {
	f.taskRepo.mu.Lock()
	defer f.taskRepo.mu.Unlock()
	return f.taskRepo.schedules["task-1"]
}

// edit changes the stored schedule, as another caller would.
func (f *scheduleFixture) edit(change func(*ScheduledTask)) {
	schedule := f.schedule()
	change(&schedule)
	f.putSchedule(schedule)
}

// tick looks for due schedules at the given time and waits for the runs it
// starts to be made.
func (f *scheduleFixture) tick(now time.Time) {
	f.clock.Set(now)
	f.service.schedules.tickSafely(context.Background())
	f.service.schedules.runs.Wait()
}

func (f *scheduleFixture) reported() []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]error(nil), f.reports...)
}

func (f *scheduleFixture) runInput() RunTaskPromptInput {
	return RunTaskPromptInput{
		TaskID:         "task-1",
		Prompt:         "triage the open issues",
		SessionMode:    PromptRunSessionSame,
		PermissionMode: TaskPermissionModeAcceptEdits,
	}
}

// --- when a schedule fires

func TestScheduler_MakesARunWhenTheScheduleIsDueAndNotBefore(t *testing.T) {
	f := newScheduleFixture(t)

	f.tick(scheduleNine.Add(-time.Second))
	require.Empty(t, f.runner.runs(), "a schedule is not made early")
	require.Equal(t, scheduleNine, f.schedule().NextRunAt)

	f.tick(scheduleNine)

	require.Equal(t, []RunTaskPromptInput{f.runInput()}, f.runner.runs(), "the stored prompt and modes are run")
	require.Equal(t, scheduleNine.AddDate(0, 0, 1), f.schedule().NextRunAt, "the schedule goes on from now")

	f.tick(scheduleNine.Add(30 * time.Second))
	require.Len(t, f.runner.runs(), 1, "the run is made once")
}

func TestScheduler_MakesAMissedRunOnceAndDoesNotMakeUpTheOnesBetween(t *testing.T) {
	f := newScheduleFixture(t)
	f.edit(func(s *ScheduledTask) {
		s.Cron = "every 10m"
		s.NextRunAt = scheduleStart.Add(10 * time.Minute)
	})

	// The daemon was down for hours: six runs came and went.
	late := scheduleStart.Add(3*time.Hour + 4*time.Minute)
	f.tick(late)

	require.Len(t, f.runner.runs(), 1, "one run for everything that was missed")
	require.Equal(t, late.Truncate(10*time.Minute).Add(10*time.Minute), f.schedule().NextRunAt,
		"the schedule continues from its next time after now")

	f.tick(late.Add(time.Minute))
	require.Len(t, f.runner.runs(), 1, "nothing is backfilled afterwards")
}

func TestScheduler_SetsANextRunThatTheClockGoingBackLeftTooLate(t *testing.T) {
	f := newScheduleFixture(t)
	// The next run was worked out a day ahead of a clock that was then set back.
	f.edit(func(s *ScheduledTask) { s.NextRunAt = scheduleNine.AddDate(0, 0, 1) })

	f.tick(scheduleStart)

	require.Empty(t, f.runner.runs(), "setting the run right is not a run")
	require.Equal(t, scheduleNine, f.schedule().NextRunAt, "the run is where the schedule says from now")

	f.tick(scheduleNine)
	require.Len(t, f.runner.runs(), 1, "and it is made then")
}

func TestScheduler_LeavesANextRunThatIsRightAlone(t *testing.T) {
	f := newScheduleFixture(t)
	var claims int
	f.taskRepo.beforeClaim = func(string) { claims++ }

	f.tick(scheduleStart)
	f.tick(scheduleStart.Add(30 * time.Minute))

	require.Zero(t, claims, "a run that is not due and not too late is not touched")
	require.Equal(t, scheduleNine, f.schedule().NextRunAt)
}

func TestScheduler_DoesNothingForAPausedSchedule(t *testing.T) {
	f := newScheduleFixture(t)
	f.edit(func(s *ScheduledTask) { s.Enabled = false })
	var claims int
	f.taskRepo.beforeClaim = func(string) { claims++ }

	f.tick(scheduleNine.Add(time.Hour))

	require.Empty(t, f.runner.runs())
	require.Zero(t, claims)
	require.Equal(t, scheduleNine, f.schedule().NextRunAt, "a paused schedule keeps its record")
}

func TestScheduler_LeavesARunToWhoeverClaimedItFirst(t *testing.T) {
	f := newScheduleFixture(t)
	claimed := scheduleNine.AddDate(0, 0, 1)
	f.taskRepo.beforeClaim = func(string) {
		// Another daemon looked at the same schedule an instant earlier.
		f.edit(func(s *ScheduledTask) { s.NextRunAt = claimed })
	}

	f.tick(scheduleNine)

	require.Empty(t, f.runner.runs(), "a lost claim makes no run")
	require.Equal(t, claimed, f.schedule().NextRunAt)
}

func TestScheduler_WaitsForATaskStillBeingCreated(t *testing.T) {
	f := newScheduleFixture(t)
	f.task.CreationStatus = TaskCreationStatusCreating
	var claims int
	f.taskRepo.beforeClaim = func(string) { claims++ }

	f.tick(scheduleNine.Add(time.Minute))
	require.Empty(t, f.runner.runs())
	require.Zero(t, claims, "the run is not spent on a task that cannot take it")
	require.Equal(t, scheduleNine, f.schedule().NextRunAt)

	f.task.CreationStatus = TaskCreationStatusReady
	f.tick(scheduleNine.Add(2 * time.Minute))
	require.Len(t, f.runner.runs(), 1, "it is made once the task is ready")
}

func TestScheduler_NeverStartsASecondRunWhileTheFirstIsBeingMade(t *testing.T) {
	f := newScheduleFixture(t)
	f.runner.release = make(chan struct{})
	f.runner.started = make(chan struct{}, 1)
	f.edit(func(s *ScheduledTask) { s.Cron = "every 10m" })

	f.clock.Set(scheduleNine)
	f.service.schedules.tickSafely(t.Context())
	<-f.runner.started
	// The next run comes due while the first is still waiting for its provider.
	f.clock.Set(scheduleNine.Add(10 * time.Minute))
	f.service.schedules.tickSafely(t.Context())
	close(f.runner.release)
	f.service.schedules.runs.Wait()

	require.Len(t, f.runner.runs(), 1)
}

func TestScheduler_SkipsAStoredScheduleThatNoLongerFiresAndSaysWhyOnce(t *testing.T) {
	f := newScheduleFixture(t)
	f.edit(func(s *ScheduledTask) { s.Cron = "bad" })

	f.tick(scheduleNine)

	require.Empty(t, f.runner.runs())
	stored := f.schedule()
	require.Equal(t, ScheduledRunFailed, stored.LastOutcome)
	require.Equal(t, `schedule "bad": not a cron schedule`, stored.LastDetail)
	require.Equal(t, scheduleNine, stored.LastRunAt)

	f.tick(scheduleNine.Add(time.Hour))
	require.Equal(t, scheduleNine, f.schedule().LastRunAt, "the same reason is not written again")
}

// --- one bad schedule or run does not stop the others

func TestScheduler_ReportsARunThatPanicsAndGoesOn(t *testing.T) {
	f := newScheduleFixture(t)
	f.runner.panics = true

	f.tick(scheduleNine)

	reported := f.reported()
	require.Len(t, reported, 1)
	require.ErrorContains(t, reported[0], "panicked")
	require.ErrorContains(t, reported[0], "the run is broken")

	f.runner.panics = false
	f.edit(func(s *ScheduledTask) { s.NextRunAt = scheduleNine })
	f.tick(scheduleNine.Add(time.Minute))
	require.Len(t, f.runner.runs(), 2, "the schedule still runs")
	require.Equal(t, ScheduledRunRunning, f.schedule().LastOutcome)
}

func TestScheduler_LetsARunAfterAPanickedRunForTheSameTaskStart(t *testing.T) {
	f := newScheduleFixture(t)
	f.runner.panics = true
	f.tick(scheduleNine)

	f.runner.panics = false
	require.NoError(t, f.service.RunTaskScheduleNow(t.Context(), "task-1"),
		"a panicked run does not leave the task marked as being run")
	f.service.schedules.runs.Wait()
}

func TestScheduler_OneBrokenScheduleDoesNotStopTheOthers(t *testing.T) {
	f := newScheduleFixture(t)
	f.taskRepo.listTasks = []*Task{
		{ID: "task-0", CreationStatus: TaskCreationStatusReady, Kind: TaskKindScheduled},
		f.task,
	}
	f.putSchedule(ScheduledTask{
		TaskID:        "task-0",
		ScheduleInput: ScheduleInput{Cron: "panic", Prompt: "p"},
		Enabled:       true,
		NextRunAt:     scheduleNine,
	})
	// Listed in creation order, the broken one comes first.
	f.taskRepo.scheduleOrder = []string{"task-0", "task-1"}

	f.tick(scheduleNine)

	require.Len(t, f.runner.runs(), 1, "the schedule after the broken one still ran")
	require.Equal(t, "task-1", f.runner.runs()[0].TaskID)
	require.NotEmpty(t, f.reported())
	require.ErrorContains(t, f.reported()[0], "the calculator is broken")
}

func TestScheduler_ReportsATickThatPanicsAndLooksAgainNextTime(t *testing.T) {
	f := newScheduleFixture(t)
	f.taskRepo.onListTasks = func(call int) {
		if call == 1 {
			panic("the repository is broken")
		}
	}

	f.tick(scheduleNine)
	require.Empty(t, f.runner.runs())
	require.Len(t, f.reported(), 1)
	require.ErrorContains(t, f.reported()[0], "scheduler tick panicked")

	f.tick(scheduleNine.Add(time.Second))
	require.Len(t, f.runner.runs(), 1, "the next tick works")
}

func TestScheduler_ReportsWhatItCannotRead(t *testing.T) {
	f := newScheduleFixture(t)
	f.taskRepo.listErr = errors.New("database is locked")
	var claims int
	f.taskRepo.beforeClaim = func(string) { claims++ }
	// A next run the clock going back left too late would be set right, but
	// not without knowing the tasks.
	f.edit(func(s *ScheduledTask) { s.NextRunAt = scheduleNine.AddDate(0, 0, 3) })

	f.tick(scheduleNine)

	require.Zero(t, claims, "the tick ends where it could not read")
	require.Empty(t, f.runner.runs())
	require.Len(t, f.reported(), 1)
	require.ErrorContains(t, f.reported()[0], "database is locked")
}

// --- how a run is recorded

func TestScheduler_RecordsARunThatStartedAsRunning(t *testing.T) {
	f := newScheduleFixture(t)
	f.runner.result = RunTaskPromptResult{
		Outcome: PromptRunStarted, Detail: "resumed 3f2a…", StartedAt: time.Now(),
	}

	f.tick(scheduleNine)

	stored := f.schedule()
	require.Equal(t, ScheduledRunStateRunning, stored.RunState)
	require.Equal(t, ScheduledRunRunning, stored.LastOutcome)
	require.Equal(t, "resumed 3f2a…", stored.LastDetail)
	require.Equal(t, scheduleNine, stored.LastRunAt)
	require.Equal(t, scheduleNine, stored.RunStartedAt)
}

func TestScheduler_RecordsASkippedRunWithItsReason(t *testing.T) {
	f := newScheduleFixture(t)
	f.runner.result = RunTaskPromptResult{Outcome: PromptRunSkipped, Detail: "session busy"}

	f.tick(scheduleNine)

	stored := f.schedule()
	require.Equal(t, ScheduledRunSkipped, stored.LastOutcome)
	require.Equal(t, "session busy", stored.LastDetail)
	require.Equal(t, scheduleNine, stored.LastRunAt)
	require.Equal(t, ScheduledRunStateIdle, stored.RunState)
}

func TestScheduler_RecordsAFailedRunWithItsReasonAndEndsTheRunBefore(t *testing.T) {
	f := newScheduleFixture(t)
	f.edit(func(s *ScheduledTask) {
		s.RunState = ScheduledRunStateRunning
		s.LastOutcome = ScheduledRunRunning
	})
	f.runner.result = RunTaskPromptResult{Outcome: PromptRunFailed, Detail: "the session didn't take the prompt"}

	f.tick(scheduleNine)

	stored := f.schedule()
	require.Equal(t, ScheduledRunFailed, stored.LastOutcome)
	require.Equal(t, "the session didn't take the prompt", stored.LastDetail)
	require.Equal(t, scheduleNine, stored.LastRunAt)
	require.Equal(t, ScheduledRunStateIdle, stored.RunState, "a failed run is not in flight")
}

func TestScheduler_RecordsARunThatCouldNotBeMadeAsFailed(t *testing.T) {
	f := newScheduleFixture(t)
	f.runner.err = errors.New("read task status: database is locked")

	f.tick(scheduleNine)

	stored := f.schedule()
	require.Equal(t, ScheduledRunFailed, stored.LastOutcome)
	require.Equal(t, "read task status: database is locked", stored.LastDetail)
}

func TestScheduler_RecordsNothingForARunTheDaemonStoppingCutShort(t *testing.T) {
	f := newScheduleFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	f.runner.err = context.Canceled
	cancel()
	before := f.schedule()

	f.service.schedules.makeRun(ctx, before)

	require.Equal(t, before, f.schedule())
}

func TestScheduler_LeavesTheRecordOfARunInFlightWhenALaterRunIsSkipped(t *testing.T) {
	f := newScheduleFixture(t)
	f.edit(func(s *ScheduledTask) {
		s.RunState = ScheduledRunStateRunning
		s.RunStartedAt = scheduleStart
		s.LastRunAt = scheduleStart
		s.LastOutcome = ScheduledRunRunning
		s.LastDetail = "sent to the running session"
	})
	before := f.schedule()
	f.runner.result = RunTaskPromptResult{Outcome: PromptRunSkipped, Detail: "session busy"}

	f.tick(scheduleNine)

	require.Equal(t, before.RunState, f.schedule().RunState)
	require.Equal(t, before.LastOutcome, f.schedule().LastOutcome, "it still says the run is running")
	require.Equal(t, before.LastDetail, f.schedule().LastDetail)
	require.Equal(t, before.LastRunAt, f.schedule().LastRunAt)
}

func TestScheduler_ReplacesTheRecordOfARunLeftInFlightWhenTheNextRunStarts(t *testing.T) {
	f := newScheduleFixture(t)
	// The daemon restarted during the last run, which nobody saw end.
	f.edit(func(s *ScheduledTask) {
		s.RunState = ScheduledRunStateRunning
		s.RunStartedAt = scheduleStart.Add(-24 * time.Hour)
		s.LastRunAt = scheduleStart.Add(-24 * time.Hour)
		s.LastOutcome = ScheduledRunRunning
	})

	f.tick(scheduleNine)

	stored := f.schedule()
	require.Equal(t, ScheduledRunStateRunning, stored.RunState)
	require.Equal(t, scheduleNine, stored.RunStartedAt, "the new run is the one in flight")
	require.Equal(t, scheduleNine, stored.LastRunAt)
}

// --- a run that is already over when it is recorded

func TestScheduler_EndsARunWhoseTurnWasOverBeforeItWasRecorded(t *testing.T) {
	// The run began a moment before the scheduler's clock, as it does: the clock
	// is read when the run is begun and the prompt is sent after.
	startedAt := scheduleNine.Add(-5 * time.Second)
	tests := []struct {
		name    string
		status  TaskStatusUpdate
		want    ScheduledRunOutcome
		detail  string
		running bool
	}{
		{
			name: "stop",
			status: TaskStatusUpdate{
				Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop, ObservedAt: startedAt.Add(time.Second),
			},
			want:   ScheduledRunCompleted,
			detail: "started fresh",
		},
		{
			name: "stop failure",
			status: TaskStatusUpdate{
				Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStopFailure,
				ObservedAt: startedAt.Add(time.Second),
			},
			want:   ScheduledRunFailed,
			detail: "the agent stopped on an error",
		},
		{
			name: "session end",
			status: TaskStatusUpdate{
				Phase: TaskStatusPhaseStopped, RawEventName: HookEventSessionEnd, ObservedAt: startedAt.Add(time.Second),
			},
			want:   ScheduledRunFailed,
			detail: "the session ended",
		},
		{
			name: "a turn that is still going",
			status: TaskStatusUpdate{
				Phase: TaskStatusPhaseWorking, RawEventName: HookEventPreToolUse, ObservedAt: startedAt.Add(time.Second),
			},
			want:    ScheduledRunRunning,
			detail:  "started fresh",
			running: true,
		},
		{
			name: "the stop of the turn before",
			status: TaskStatusUpdate{
				Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop, ObservedAt: startedAt.Add(-time.Second),
			},
			want:    ScheduledRunRunning,
			detail:  "started fresh",
			running: true,
		},
		{
			name: "a stop at the very time the run began",
			status: TaskStatusUpdate{
				Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop, ObservedAt: startedAt,
			},
			want:    ScheduledRunRunning,
			detail:  "started fresh",
			running: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newScheduleFixture(t)
			f.runner.result = RunTaskPromptResult{
				Outcome:   PromptRunStarted,
				Detail:    "started fresh",
				StartedAt: startedAt,
			}
			status := tt.status
			status.TaskID, status.Provider = "task-1", ProviderCodex
			f.taskRepo.mu.Lock()
			f.taskRepo.latestByTask["task-1"] = status
			f.taskRepo.mu.Unlock()

			f.tick(scheduleNine)

			stored := f.schedule()
			require.Equal(t, tt.want, stored.LastOutcome)
			require.Equal(t, tt.detail, stored.LastDetail)
			require.Equal(t, tt.running, stored.IsRunning())
		})
	}
}

func TestScheduler_IgnoresEveryStatusForARunThatSaysNoStartTime(t *testing.T) {
	f := newScheduleFixture(t)
	f.runner.result = RunTaskPromptResult{Outcome: PromptRunStarted, Detail: "started fresh"}
	f.taskRepo.mu.Lock()
	f.taskRepo.latestByTask["task-1"] = TaskStatusUpdate{
		TaskID: "task-1", Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop,
		ObservedAt: time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	f.taskRepo.mu.Unlock()

	f.tick(scheduleNine)

	require.True(t, f.schedule().IsRunning(), "any status is newer than no start time at all")
}

// --- the loop

func TestRunScheduler_MakesDueRunsAtStartAndWhenPoked(t *testing.T) {
	f := newScheduleFixture(t)
	m := f.service.schedules
	m.tick = time.Hour
	f.clock.Set(scheduleNine.Add(time.Minute))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.service.RunScheduler(ctx)
	}()

	require.Eventually(t, func() bool { return len(f.runner.runs()) == 1 }, 5*time.Second, time.Millisecond,
		"a run missed while the daemon was down is made at start")

	f.clock.Set(scheduleNine.AddDate(0, 0, 1).Add(time.Minute))
	require.Never(t, func() bool { return len(f.runner.runs()) > 1 }, 50*time.Millisecond, 5*time.Millisecond,
		"nothing looks again before the tick or a poke")
	m.poke()
	require.Eventually(t, func() bool { return len(f.runner.runs()) == 2 }, 5*time.Second, time.Millisecond,
		"a poke makes the scheduler look at once")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduler did not stop with its context")
	}
}

func TestRunScheduler_LooksAgainEveryTick(t *testing.T) {
	f := newScheduleFixture(t)
	m := f.service.schedules
	m.tick = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go f.service.RunScheduler(ctx)

	require.Never(t, func() bool { return len(f.runner.runs()) > 0 }, 30*time.Millisecond, 5*time.Millisecond)
	f.clock.Set(scheduleNine)
	require.Eventually(t, func() bool { return len(f.runner.runs()) == 1 }, 5*time.Second, time.Millisecond,
		"a schedule that comes due is made by the ticker, without a poke")
}

func TestRunScheduler_WaitsForRunsInFlightBeforeItReturns(t *testing.T) {
	f := newScheduleFixture(t)
	f.service.schedules.tick = time.Hour
	f.runner.release = make(chan struct{})
	f.runner.started = make(chan struct{}, 1)
	f.runner.ignoresContext = true
	f.clock.Set(scheduleNine)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.service.RunScheduler(ctx)
	}()
	<-f.runner.started

	cancel()
	require.Never(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 50*time.Millisecond, 5*time.Millisecond, "the scheduler holds on for the run it is making")

	close(f.runner.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduler did not stop once its run was over")
	}
	require.Equal(t, ScheduledRunRunning, f.schedule().LastOutcome, "and the run it finished is recorded")
}

func TestRunScheduler_LeavesNoRecordForARunItsContextCutShort(t *testing.T) {
	f := newScheduleFixture(t)
	f.service.schedules.tick = time.Hour
	f.runner.release = make(chan struct{})
	f.runner.started = make(chan struct{}, 1)
	f.clock.Set(scheduleNine)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.service.RunScheduler(ctx)
	}()
	<-f.runner.started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduler did not stop with its context")
	}

	require.Equal(
		t,
		ScheduledRunNone,
		f.schedule().LastOutcome,
		"a run the daemon cut short says nothing of how it went",
	)
}

func TestRunScheduler_DoesNothingWithoutACalculator(t *testing.T) {
	f := newScheduleFixture(t)
	f.service.schedules.calculator = nil
	f.clock.Set(scheduleNine)

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.service.RunScheduler(t.Context())
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduler cannot run without a calculator and must return")
	}
	require.Empty(t, f.runner.runs())
}

// --- run now

func TestRunTaskScheduleNow_StartsARunWithoutWaitingForItAndLeavesTheNextRunAlone(t *testing.T) {
	f := newScheduleFixture(t)
	f.runner.release = make(chan struct{})
	f.runner.started = make(chan struct{}, 1)

	require.NoError(t, f.service.RunTaskScheduleNow(t.Context(), "task-1"))
	<-f.runner.started
	// The call has returned while the run still waits for its provider.
	require.Equal(t, ScheduledRunNone, f.schedule().LastOutcome)

	err := f.service.RunTaskScheduleNow(t.Context(), "task-1")
	require.ErrorContains(t, err, "already being run", "a second run is not started beside the first")

	close(f.runner.release)
	f.service.schedules.runs.Wait()

	require.Equal(t, []RunTaskPromptInput{f.runInput()}, f.runner.runs())
	stored := f.schedule()
	require.Equal(t, ScheduledRunRunning, stored.LastOutcome)
	require.Equal(t, scheduleNine, stored.NextRunAt, "a manual run leaves the schedule's next run as it was")
	require.NoError(t, f.service.RunTaskScheduleNow(t.Context(), "task-1"), "the task is free again")
	f.service.schedules.runs.Wait()
}

func TestRunTaskScheduleNow_RunsAPausedScheduleToo(t *testing.T) {
	f := newScheduleFixture(t)
	f.edit(func(s *ScheduledTask) { s.Enabled = false })

	require.NoError(t, f.service.RunTaskScheduleNow(t.Context(), "task-1"))
	f.service.schedules.runs.Wait()

	require.Len(t, f.runner.runs(), 1)
	require.False(t, f.schedule().Enabled, "and leaves it paused")
}

func TestRunTaskScheduleNow_RunsUnderTheSchedulersContextNotTheRequests(t *testing.T) {
	f := newScheduleFixture(t)
	f.service.schedules.tick = time.Hour
	f.runner.release = make(chan struct{})
	f.runner.started = make(chan struct{}, 1)
	f.clock.Set(scheduleStart)
	daemonCtx, stopDaemon := context.WithCancel(t.Context())
	go f.service.RunScheduler(daemonCtx)

	// The scheduler publishes its context as it starts.
	require.Eventually(
		t,
		func() bool { return f.service.schedules.context() == daemonCtx },
		5*time.Second,
		time.Millisecond,
	)

	requestCtx, endRequest := context.WithCancel(t.Context())
	require.NoError(t, f.service.RunTaskScheduleNow(requestCtx, "task-1"))
	<-f.runner.started
	endRequest()

	runCtx := f.runner.contextOfRun(0)
	require.NoError(t, runCtx.Err(), "the run outlives the request that asked for it")
	stopDaemon()
	require.Error(t, runCtx.Err(), "and ends with the daemon")
	close(f.runner.release)
	f.service.schedules.runs.Wait()
}

func TestRunTaskScheduleNow_RefusesATaskThatIsNotScheduled(t *testing.T) {
	f := newScheduleFixture(t)
	f.taskRepo.mu.Lock()
	delete(f.taskRepo.schedules, "task-1")
	f.taskRepo.mu.Unlock()

	err := f.service.RunTaskScheduleNow(t.Context(), "task-1")
	require.ErrorIs(t, err, ErrScheduleNotFound)

	err = f.service.RunTaskScheduleNow(t.Context(), "task-404")
	require.ErrorIs(t, err, ErrTaskNotFound)
	require.Empty(t, f.runner.runs())
}

// --- the scheduler with the real prompt run behind it

// useRealRuns makes the fixture's runs real prompt runs instead of the stub's.
func (f *scheduleFixture) useRealRuns() {
	f.service.schedules.run = f.service.RunTaskPrompt
}

func TestScheduler_SendsThePromptToAnIdleRunningSessionAndEndsTheRunWhenItsTurnDoes(t *testing.T) {
	f := newScheduleFixture(t)
	f.useRealRuns()
	idleProvider(f.testTaskServiceHarness)
	providerTakesPrompts(f.testTaskServiceHarness)

	f.tick(scheduleNine)

	require.Equal(t, []string{"triage the open issues"}, f.sessionClient.submittedPrompts)
	stored := f.schedule()
	require.Equal(t, ScheduledRunRunning, stored.LastOutcome)
	require.Equal(t, "sent to the running session", stored.LastDetail)
	require.True(t, stored.IsRunning())
	require.Equal(t, scheduleNine.AddDate(0, 0, 1), stored.NextRunAt)

	err := f.deliver(
		&TaskStatusUpdate{Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventStop},
		HookEventInput{EventName: HookEventStop},
	)

	require.NoError(t, err)
	require.Equal(t, ScheduledRunCompleted, f.schedule().LastOutcome)
	require.False(t, f.schedule().IsRunning())
}

func TestScheduler_RecordsABusySessionAsSkippedWithoutSendingAnything(t *testing.T) {
	f := newScheduleFixture(t)
	f.useRealRuns()
	idleProvider(f.testTaskServiceHarness)
	f.sessionClient.paneShows = []paneShow{{shows: true}}

	f.tick(scheduleNine)

	require.Empty(t, f.sessionClient.submittedPrompts)
	stored := f.schedule()
	require.Equal(t, ScheduledRunSkipped, stored.LastOutcome)
	require.Equal(t, "session busy", stored.LastDetail)
	require.Equal(t, scheduleNine, stored.LastRunAt)
	require.Equal(t, scheduleNine.AddDate(0, 0, 1), stored.NextRunAt, "a skipped run still uses up its time")
}

func TestScheduler_RecordsASessionWaitingOnAPermissionPromptAsSkipped(t *testing.T) {
	f := newScheduleFixture(t)
	f.useRealRuns()
	runningProvider(f.testTaskServiceHarness, TaskStatusUpdate{
		Phase: TaskStatusPhaseWaitingForInput, RawEventName: HookEventNotification,
	})

	f.tick(scheduleNine)

	require.Empty(t, f.sessionClient.submittedPrompts)
	require.Equal(t, ScheduledRunSkipped, f.schedule().LastOutcome)
	require.Equal(t, "waiting on a permission prompt", f.schedule().LastDetail)
}

func TestScheduler_RecordsAPromptTheSessionDidNotTakeAsFailed(t *testing.T) {
	f := newScheduleFixture(t)
	f.useRealRuns()
	idleProvider(f.testTaskServiceHarness)

	f.tick(scheduleNine)

	stored := f.schedule()
	require.Equal(t, ScheduledRunFailed, stored.LastOutcome)
	require.Equal(t, "the session didn't take the prompt", stored.LastDetail)
	require.False(t, stored.IsRunning())
}

func TestScheduler_StartsTheProviderWhenNoneIsRunningAndResumesTheLastConversation(t *testing.T) {
	f := newScheduleFixture(t)
	f.useRealRuns()
	f.sessionClient.inspectState = TaskSessionRuntimeState{Exists: false}

	f.tick(scheduleNine)

	require.NotNil(t, f.sessionClient.startedTask, "the provider is started in the window")
	require.Equal(t, "no previous session, started fresh", f.schedule().LastDetail)
	require.Equal(t, ScheduledRunRunning, f.schedule().LastOutcome)

	f.sessionClient.startedTask = nil
	f.taskRepo.mu.Lock()
	f.taskRepo.latestResumeByTask["task-1"] = TaskResumeMetadata{
		TaskID: "task-1", Provider: ProviderCodex, SessionID: "3f2a9c",
	}
	f.taskRepo.mu.Unlock()
	f.tick(scheduleNine.AddDate(0, 0, 1))

	require.NotNil(t, f.sessionClient.startedTask)
	require.Equal(t, "resumed 3f2a…", f.schedule().LastDetail)
}

func TestScheduler_EndsAnIdleSessionBeforeStartingAFreshOneInNewMode(t *testing.T) {
	f := newScheduleFixture(t)
	f.useRealRuns()
	f.edit(func(s *ScheduledTask) { s.SessionMode = PromptRunSessionNew })
	idleProvider(f.testTaskServiceHarness)

	f.tick(scheduleNine)

	require.Len(t, f.sessionClient.respawnedTasks, 1, "the idle session in the window is ended")
	require.NotNil(t, f.sessionClient.startedTask)
	require.Equal(t, "started fresh", f.schedule().LastDetail)
	require.Equal(t, []string{"respawn_task_pane", "start_task_session"}, f.events[len(f.events)-2:])
}

func TestScheduler_ReportsAClaimItCouldNotMakeAndMakesNoRun(t *testing.T) {
	f := newScheduleFixture(t)
	f.taskRepo.claimErr = errors.New("database is locked")

	f.tick(scheduleNine)

	require.Empty(t, f.runner.runs())
	require.Len(t, f.reported(), 1)
	require.ErrorContains(t, f.reported()[0], "claim the run of task task-1: database is locked")
}

func TestScheduler_StopsLookingOnceItsContextEnds(t *testing.T) {
	f := newScheduleFixture(t)
	f.putSchedule(ScheduledTask{
		TaskID: "task-2", ScheduleInput: ScheduleInput{Cron: "daily 09:00", Prompt: "p"}, Enabled: true,
		NextRunAt: scheduleNine,
	})
	f.taskRepo.listTasks = append(f.taskRepo.listTasks, &Task{
		ID: "task-2", CreationStatus: TaskCreationStatusReady, Kind: TaskKindScheduled,
	})
	var claims int
	f.taskRepo.beforeClaim = func(string) { claims++ }
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f.clock.Set(scheduleNine)

	f.service.schedules.tickOnce(ctx)
	f.service.schedules.runs.Wait()

	require.Zero(t, claims, "nothing is claimed for a daemon that is stopping")
	require.Empty(t, f.runner.runs())
}

func TestScheduler_ReportsARunItCouldNotRecord(t *testing.T) {
	f := newScheduleFixture(t)
	f.service.schedules.tasks = failingStartRepository{TaskRepository: f.service.schedules.tasks}

	f.tick(scheduleNine)

	require.Len(t, f.runner.runs(), 1)
	require.Len(t, f.reported(), 1)
	require.ErrorContains(t, f.reported()[0], "record the run of task task-1: database is locked")
}

// failingStartRepository is a task repository that cannot record a run.
type failingStartRepository struct{ TaskRepository }

func (failingStartRepository) StartScheduledRun(context.Context, string, time.Time, string) error {
	return errors.New("database is locked")
}

func TestScheduler_ReportsAnUnrunnableScheduleItCouldNotRecord(t *testing.T) {
	f := newScheduleFixture(t)
	f.edit(func(s *ScheduledTask) { s.Cron = "bad" })
	f.service.schedules.tasks = failingFailRepository{TaskRepository: f.service.schedules.tasks}

	f.tick(scheduleNine)

	require.Len(t, f.reported(), 1)
	require.ErrorContains(t, f.reported()[0], "record the schedule of task task-1: database is locked")
}

// failingFailRepository is a task repository that cannot record a failure.
type failingFailRepository struct{ TaskRepository }

func (failingFailRepository) FailScheduledRun(context.Context, string, time.Time, string) error {
	return errors.New("database is locked")
}

func TestRunTaskScheduleNow_MakesAPromptRunAsTheServiceWiresIt(t *testing.T) {
	f := newScheduleFixture(t)
	f.service.schedules.run = f.defaultRun
	f.sessionClient.inspectState = TaskSessionRuntimeState{Exists: false}

	require.NoError(t, f.service.RunTaskScheduleNow(t.Context(), "task-1"))
	f.service.schedules.runs.Wait()

	require.NotNil(t, f.sessionClient.startedTask, "the run starts the provider in the task's window")
	require.Equal(t, ScheduledRunRunning, f.schedule().LastOutcome)
}
