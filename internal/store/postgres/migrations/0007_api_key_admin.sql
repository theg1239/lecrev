alter table api_keys
  add column if not exists is_admin boolean not null default false;
