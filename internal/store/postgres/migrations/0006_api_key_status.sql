alter table api_keys
  add column if not exists disabled boolean not null default false,
  add column if not exists last_used_at timestamptz;
