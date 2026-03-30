alter table build_jobs
  add column if not exists logs_key text;
