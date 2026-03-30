create table if not exists idempotency_records (
  scope text not null,
  project_id text not null references projects(id),
  key text not null,
  request_hash text not null,
  resource_id text,
  resource text,
  status text not null,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  primary key (scope, project_id, key)
);

create table if not exists webhook_triggers (
  token text primary key,
  project_id text not null references projects(id),
  function_version_id text not null references function_versions(id),
  description text,
  enabled boolean not null default true,
  created_at timestamptz not null default now()
);

create index if not exists idx_idempotency_project on idempotency_records (project_id);
create index if not exists idx_webhook_triggers_version on webhook_triggers (function_version_id);
