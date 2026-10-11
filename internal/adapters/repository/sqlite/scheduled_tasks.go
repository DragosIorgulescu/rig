package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BaronBonet/rig/internal/adapters/repository/sqlite/generated"
	"github.com/BaronBonet/rig/internal/core"
)

func (r *repository) CreateScheduledTask(ctx context.Context, schedule core.ScheduledTask) error {
	schedule.TaskID = strings.TrimSpace(schedule.TaskID)
	if schedule.TaskID == "" {
		return fmt.Errorf("scheduled task task ID is required")
	}
	return r.queries.CreateScheduledTask(ctx, createScheduledTaskParams(schedule))
}

func (r *repository) GetScheduledTask(ctx context.Context, taskID string) (*core.ScheduledTask, error) {
	row, err := r.queries.GetScheduledTask(ctx, strings.TrimSpace(taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	schedule := scheduledTaskFromRow(row)
	return &schedule, nil
}

func (r *repository) ListScheduledTasks(ctx context.Context) ([]core.ScheduledTask, error) {
	rows, err := r.queries.ListScheduledTasks(ctx)
	if err != nil {
		return nil, err
	}

	schedules := make([]core.ScheduledTask, 0, len(rows))
	for _, row := range rows {
		schedules = append(schedules, scheduledTaskFromRow(row))
	}
	return schedules, nil
}

func (r *repository) UpdateScheduledTask(
	ctx context.Context,
	schedule core.ScheduledTask,
	expectedUpdatedAt time.Time,
) error {
	schedule.TaskID = strings.TrimSpace(schedule.TaskID)
	changed, err := r.queries.UpdateScheduledTask(ctx, generated.UpdateScheduledTaskParams{
		Cron:           schedule.Cron,
		Prompt:         schedule.Prompt,
		SessionMode:    string(schedule.SessionMode),
		PermissionMode: string(schedule.PermissionMode),
		Enabled:        boolToInt(schedule.Enabled),
		NextRunAt:      formatTime(schedule.NextRunAt),
		UpdatedAt:      formatTime(schedule.UpdatedAt),
		TaskID:         schedule.TaskID,
		UpdatedAt_2:    formatTime(expectedUpdatedAt),
	})
	if err != nil {
		return err
	}
	if changed > 0 {
		return nil
	}

	// Nothing matched: either the schedule is gone or it was edited since.
	existing, err := r.GetScheduledTask(ctx, schedule.TaskID)
	if err != nil {
		return err
	}
	if existing == nil {
		return core.ErrScheduleNotFound
	}
	return core.ErrScheduleConflict
}

func (r *repository) ClaimScheduledRun(
	ctx context.Context,
	taskID string,
	dueAt time.Time,
	nextRunAt time.Time,
) (bool, error) {
	claimed, err := r.queries.ClaimScheduledRun(ctx, generated.ClaimScheduledRunParams{
		NextRunAt:   formatTime(nextRunAt),
		TaskID:      strings.TrimSpace(taskID),
		NextRunAt_2: formatTime(dueAt),
	})
	return claimed > 0, err
}

func (r *repository) StartScheduledRun(ctx context.Context, taskID string, at time.Time, detail string) error {
	_, err := r.queries.StartScheduledRun(ctx, generated.StartScheduledRunParams{
		RunStartedAt: formatTime(at),
		LastDetail:   detail,
		TaskID:       strings.TrimSpace(taskID),
	})
	return err
}

func (r *repository) SkipScheduledRun(ctx context.Context, taskID string, at time.Time, detail string) error {
	_, err := r.queries.SkipScheduledRun(ctx, generated.SkipScheduledRunParams{
		LastRunAt:  formatTime(at),
		LastDetail: detail,
		TaskID:     strings.TrimSpace(taskID),
	})
	return err
}

func (r *repository) FailScheduledRun(ctx context.Context, taskID string, at time.Time, detail string) error {
	_, err := r.queries.FailScheduledRun(ctx, generated.FailScheduledRunParams{
		LastRunAt:  formatTime(at),
		LastDetail: detail,
		TaskID:     strings.TrimSpace(taskID),
	})
	return err
}

func (r *repository) ResolveScheduledRun(
	ctx context.Context,
	taskID string,
	outcome core.ScheduledRunOutcome,
	detail string,
) (bool, error) {
	resolved, err := r.queries.ResolveScheduledRun(ctx, generated.ResolveScheduledRunParams{
		LastOutcome: string(outcome),
		Column2:     detail,
		TaskID:      strings.TrimSpace(taskID),
	})
	return resolved > 0, err
}
