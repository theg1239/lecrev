alter table function_versions
  add column if not exists env_vars jsonb not null default '{}'::jsonb;
