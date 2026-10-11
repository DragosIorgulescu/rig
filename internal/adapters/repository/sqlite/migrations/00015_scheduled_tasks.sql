-- +goose Up

alter table tasks add column kind text not null default '';

-- The schedule of a scheduled task. Times are RFC 3339 UTC, or '' when unset.
-- run_state is 'running' from the moment a run starts until a hook says its
-- turn ended; last_outcome keeps how the latest run went.
create table if not exists scheduled_tasks (
  task_id text primary key,
  cron text not null,
  prompt text not null,
  session_mode text not null default 'new',
  permission_mode text not null default 'auto',
  enabled integer not null default 1,
  next_run_at text not null default '',
  run_state text not null default '',
  run_started_at text not null default '',
  last_run_at text not null default '',
  last_outcome text not null default '',
  last_detail text not null default '',
  created_at text not null,
  updated_at text not null,
  foreign key(task_id) references tasks(id) on delete cascade
);

-- +goose Down

drop table if exists scheduled_tasks;
alter table tasks drop column kind;
