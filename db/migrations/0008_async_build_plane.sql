alter table function_versions
  add column if not exists build_job_id text;

alter table build_jobs
  add column if not exists target_region text,
  add column if not exists request jsonb not null default '{}'::jsonb;

create index if not exists idx_build_jobs_function_version_id on build_jobs (function_version_id);
