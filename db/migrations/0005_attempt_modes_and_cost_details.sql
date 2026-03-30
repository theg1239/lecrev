alter table attempts
  add column if not exists start_mode text;

alter table cost_records
  add column if not exists attempt_id text references attempts(id),
  add column if not exists host_id text,
  add column if not exists region text,
  add column if not exists start_mode text;

create index if not exists idx_attempts_job_id on attempts (job_id);
create index if not exists idx_cost_records_job_id on cost_records (job_id);
create index if not exists idx_cost_records_attempt_id on cost_records (attempt_id);
