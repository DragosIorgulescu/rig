package core

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"
)

// maxCheckedScheduleRuns is how many consecutive runs of a schedule are
// checked against the minimum interval.
const maxCheckedScheduleRuns = 50

// RunScheduler runs the scheduler until ctx ends: it looks for schedules that
// are due every tick, and again whenever a schedule is edited, and makes each
// due run in a goroutine of its own. It is not part of the TaskService port;
// the daemon starts it.
//
// A tick that panics is reported and the scheduler goes on, as does a run
// that panics: one bad schedule must not stop the others.
func (s *service) RunScheduler(ctx context.Context) {
	m := s.schedules
	if m.calculator == nil {
		return
	}

	m.mu.Lock()
	m.runCtx = ctx
	m.mu.Unlock()

	ticker := time.NewTicker(m.tick)
	defer ticker.Stop()
	for {
		// The first look is at start, so the runs missed while the daemon was
		// down are made at once.
		m.tickSafely(ctx)
		select {
		case <-ctx.Done():
			m.runs.Wait()
			return
		case <-ticker.C:
		case <-m.pokes:
		}
	}
}

func (m *taskSchedules) tickSafely(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			m.report(fmt.Errorf("scheduler tick panicked: %v\n%s", recovered, debug.Stack()))
		}
	}()
	m.tickOnce(ctx)
}

// tickOnce makes the runs that are due now. A run that is missed, because the
// daemon was down or the Task was not ready, is made once when it can be, and
// the schedule goes on from its next time after now; the runs between are not
// made up.
func (m *taskSchedules) tickOnce(ctx context.Context) {
	now := m.now()
	schedules, err := m.tasks.ListScheduledTasks(ctx)
	if err != nil {
		m.report(fmt.Errorf("list schedules: %w", err))
		return
	}
	if len(schedules) == 0 {
		return
	}
	tasks, err := m.tasks.ListTasks(ctx)
	if err != nil {
		m.report(fmt.Errorf("list tasks for the scheduler: %w", err))
		return
	}
	byID := make(map[string]*Task, len(tasks))
	for _, task := range tasks {
		if task != nil {
			byID[task.ID] = task
		}
	}

	for _, schedule := range schedules {
		if ctx.Err() != nil {
			return
		}
		task := byID[schedule.TaskID]
		ready := task != nil && task.CreationStatus == TaskCreationStatusReady
		// A run whose session is gone ends before the schedule is looked at, so
		// that the run due now is recorded over a run that is over.
		if schedule.IsRunning() && ready {
			m.endRunIfSessionGoneSafely(ctx, schedule, task, now)
		}
		if schedule.Enabled {
			m.considerSafely(ctx, schedule, now, ready)
		}
	}
}

// goneSessionGrace is how long a run is left alone before its provider is
// looked for in the window, so a provider that was only just started is not
// taken for one that is gone.
const goneSessionGrace = 10 * time.Second

// endRunIfSessionGoneSafely is endRunIfSessionGone for one schedule among
// several: a schedule that breaks the scheduler is reported and the others are
// still looked at.
func (m *taskSchedules) endRunIfSessionGoneSafely(
	ctx context.Context,
	schedule ScheduledTask,
	task *Task,
	now time.Time,
) {
	defer func() {
		if recovered := recover(); recovered != nil {
			m.report(fmt.Errorf("scheduler panicked looking for the session of task %s: %v\n%s",
				schedule.TaskID, recovered, debug.Stack()))
		}
	}()
	m.endRunIfSessionGone(ctx, schedule, task, now)
}

// endRunIfSessionGone fails a run in flight whose provider is no longer in the
// Task's window. A provider that crashes, is killed by a reboot or exits
// without saying so sends no Stop or SessionEnd, and nothing else would end
// the run before the next one. What cannot be told, a window that cannot be
// inspected or processes that cannot be seen, leaves the run as it is, and so
// does a run this process is still making.
func (m *taskSchedules) endRunIfSessionGone(ctx context.Context, schedule ScheduledTask, task *Task, now time.Time) {
	if now.Sub(schedule.RunStartedAt) < goneSessionGrace || m.isInFlight(schedule.TaskID) {
		return
	}
	_, providerClient, err := m.launcher.resolveProvider(ctx, task.Provider)
	if err != nil {
		return
	}
	runtime, err := m.launcher.tmuxSession.InspectTaskSession(ctx, task)
	if err != nil || (runtime.Exists && runtime.ChildProcessEvidenceUnavailable) {
		return
	}
	if taskSessionRunningProvider(runtime, providerClient.TaskSessionCommandName()) {
		return
	}
	_, err = m.tasks.ResolveScheduledRun(ctx, schedule.TaskID, ScheduledRunFailed, "the session ended")
	if err != nil {
		m.report(fmt.Errorf("end the run of task %s: %w", schedule.TaskID, err))
	}
}

// isInFlight reports whether this process is making a run for the Task.
func (m *taskSchedules) isInFlight(taskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inFlight[taskID]
}

// considerSafely is consider for one schedule among several: a schedule that
// breaks the scheduler is reported and the others are still looked at.
func (m *taskSchedules) considerSafely(ctx context.Context, schedule ScheduledTask, now time.Time, ready bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			m.report(fmt.Errorf("scheduler panicked on the schedule of task %s: %v\n%s",
				schedule.TaskID, recovered, debug.Stack()))
		}
	}()
	m.consider(ctx, schedule, now, ready)
}

// consider fires one schedule if it is due.
func (m *taskSchedules) consider(ctx context.Context, schedule ScheduledTask, now time.Time, ready bool) {
	next, err := m.calculator.Next(schedule.Cron, now)
	if err != nil {
		m.recordUnrunnable(ctx, schedule, now, err)
		return
	}

	if schedule.NextRunAt.After(now) {
		// The schedule is not due. A next run later than the schedule gives
		// from now means the clock went back since it was set, so it is set
		// again, rather than waiting out the difference.
		if schedule.NextRunAt.After(next) {
			m.claim(ctx, schedule, next)
		}
		return
	}
	// A Task still being created cannot take a run; the schedule stays due and
	// fires once it is ready.
	if !ready {
		return
	}
	if m.claim(ctx, schedule, next) {
		m.start(ctx, schedule)
	}
}

// claim moves the schedule's next run on to next, and reports whether this
// caller was the one to do it.
func (m *taskSchedules) claim(ctx context.Context, schedule ScheduledTask, next time.Time) bool {
	claimed, err := m.tasks.ClaimScheduledRun(ctx, schedule.TaskID, schedule.NextRunAt, next.UTC())
	if err != nil {
		m.report(fmt.Errorf("claim the run of task %s: %w", schedule.TaskID, err))
		return false
	}
	return claimed
}

// recordUnrunnable records a stored schedule that no longer yields a next run,
// once, so the dashboard can say why it does not run.
func (m *taskSchedules) recordUnrunnable(ctx context.Context, schedule ScheduledTask, now time.Time, cause error) {
	detail := fmt.Sprintf("schedule %q: %v", schedule.Cron, cause)
	if schedule.LastOutcome == ScheduledRunFailed && schedule.LastDetail == detail {
		return
	}
	if err := m.tasks.FailScheduledRun(ctx, schedule.TaskID, now.UTC(), detail); err != nil {
		m.report(fmt.Errorf("record the schedule of task %s: %w", schedule.TaskID, err))
	}
}

// start makes a run of the schedule in a goroutine, and reports whether it
// did: a run already being made by this process for the Task is left to finish.
// Every run ends its own panics.
func (m *taskSchedules) start(ctx context.Context, schedule ScheduledTask) bool {
	m.mu.Lock()
	if m.inFlight[schedule.TaskID] {
		m.mu.Unlock()
		return false
	}
	m.inFlight[schedule.TaskID] = true
	m.runs.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.runs.Done()
		defer func() {
			m.mu.Lock()
			delete(m.inFlight, schedule.TaskID)
			m.mu.Unlock()
		}()
		defer func() {
			if recovered := recover(); recovered != nil {
				m.report(fmt.Errorf("scheduled run of task %s panicked: %v\n%s",
					schedule.TaskID, recovered, debug.Stack()))
			}
		}()
		m.makeRun(ctx, schedule)
	}()
	return true
}

// makeRun makes one run of the schedule and records how it went.
//
// A run that was skipped, because the session is busy, leaves the record of a
// run still in flight as it is: the dashboard keeps saying it runs. A run that
// starts replaces the record, which also ends one left in flight by a daemon
// that stopped meanwhile, since the run found its session idle.
func (m *taskSchedules) makeRun(ctx context.Context, schedule ScheduledTask) {
	at := m.now().UTC()
	result, err := m.run(ctx, RunTaskPromptInput{
		TaskID:         schedule.TaskID,
		Prompt:         schedule.Prompt,
		SessionMode:    schedule.SessionMode,
		PermissionMode: schedule.PermissionMode,
	})
	if err != nil {
		if ctx.Err() != nil {
			// The daemon is stopping; nothing is known of how the run went.
			return
		}
		result = RunTaskPromptResult{Outcome: PromptRunFailed, Detail: err.Error()}
	}

	// The record is written even when the daemon is stopping: the run did
	// happen, and its record should say so.
	ctx = context.WithoutCancel(ctx)
	switch result.Outcome {
	case PromptRunStarted:
		err = m.tasks.StartScheduledRun(ctx, schedule.TaskID, at, result.Detail)
		if err == nil {
			m.settleRun(ctx, schedule.TaskID, result.StartedAt)
		}
	case PromptRunSkipped:
		err = m.tasks.SkipScheduledRun(ctx, schedule.TaskID, at, result.Detail)
	default:
		err = m.tasks.FailScheduledRun(ctx, schedule.TaskID, at, result.Detail)
	}
	if err != nil {
		m.report(fmt.Errorf("record the run of task %s: %w", schedule.TaskID, err))
	}
}
