alter table attempts
  add column if not exists started_at timestamptz;
