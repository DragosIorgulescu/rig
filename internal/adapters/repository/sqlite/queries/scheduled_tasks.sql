-- name: CreateScheduledTask :exec
insert into scheduled_tasks (
  task_id, cron, prompt, session_mode, permission_mode, enabled, next_run_at,
  run_state, run_started_at, last_run_at, last_outcome, last_detail,
  run_skipped_at, run_skipped_detail, created_at, updated_at
) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetScheduledTask :one
select
  task_id, cron, prompt, session_mode, permission_mode, enabled, next_run_at,
  run_state, run_started_at, last_run_at, last_outcome, last_detail,
  run_skipped_at, run_skipped_detail, created_at, updated_at
from scheduled_tasks
where task_id = ?;

-- name: ListScheduledTasks :many
select
  task_id, cron, prompt, session_mode, permission_mode, enabled, next_run_at,
  run_state, run_started_at, last_run_at, last_outcome, last_detail,
  run_skipped_at, run_skipped_detail, created_at, updated_at
from scheduled_tasks
order by created_at asc, task_id asc;

-- name: UpdateScheduledTask :execrows
update scheduled_tasks set
  cron = ?,
  prompt = ?,
  session_mode = ?,
  permission_mode = ?,
  enabled = ?,
  next_run_at = ?,
  updated_at = ?
where task_id = ? and updated_at = ?;

-- name: ClaimScheduledRun :execrows
update scheduled_tasks set
  next_run_at = ?
where task_id = ? and next_run_at = ? and enabled = 1;

-- name: StartScheduledRun :execrows
update scheduled_tasks set
  run_state = 'running',
  run_started_at = ?1,
  last_run_at = ?1,
  last_outcome = 'running',
  last_detail = ?2,
  run_skipped_at = '',
  run_skipped_detail = ''
where task_id = ?3;

-- name: SkipScheduledRun :execrows
-- A skipped run is the latest run when none is in flight. When one is, its
-- record stays as it is and the skip is kept beside it.
update scheduled_tasks set
  last_run_at = case when run_state = '' then ?1 else last_run_at end,
  last_outcome = case when run_state = '' then 'skipped' else last_outcome end,
  last_detail = case when run_state = '' then ?2 else last_detail end,
  run_skipped_at = case when run_state = '' then '' else ?1 end,
  run_skipped_detail = case when run_state = '' then '' else ?2 end
where task_id = ?3;

-- name: FailScheduledRun :execrows
update scheduled_tasks set
  run_state = '',
  last_run_at = ?1,
  last_outcome = 'failed',
  last_detail = ?2,
  run_skipped_at = '',
  run_skipped_detail = ''
where task_id = ?3;

-- name: ResolveScheduledRun :execrows
update scheduled_tasks set
  run_state = '',
  last_outcome = ?1,
  last_detail = case when cast(?2 as text) = '' then last_detail else cast(?2 as text) end
where task_id = ?3 and run_state = 'running';
