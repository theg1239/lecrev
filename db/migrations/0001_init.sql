create table if not exists tenants (
  id text primary key,
  name text not null,
  created_at timestamptz not null default now()
);

create table if not exists projects (
  id text primary key,
  tenant_id text not null references tenants(id),
  name text not null,
  created_at timestamptz not null default now()
);

create table if not exists function_versions (
  id text primary key,
  project_id text not null references projects(id),
  name text not null,
  runtime text not null,
  entrypoint text not null,
  memory_mb integer not null,
  timeout_sec integer not null,
  network_policy text not null,
  regions jsonb not null,
  env_refs jsonb not null,
  max_retries integer not null,
  artifact_digest text not null,
  source_type text not null,
  state text not null,
  created_at timestamptz not null default now()
);

create table if not exists artifacts (
  digest text primary key,
  size_bytes bigint not null,
  bundle_key text not null,
  startup_key text not null,
  regions jsonb not null,
  created_at timestamptz not null default now()
);

create table if not exists build_jobs (
  id text primary key,
  function_version_id text not null references function_versions(id),
  state text not null,
  error text,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table if not exists execution_jobs (
  id text primary key,
  function_version_id text not null references function_versions(id),
  project_id text not null references projects(id),
  target_region text,
  state text not null,
  payload jsonb not null,
  max_retries integer not null,
  attempt_count integer not null default 0,
  last_attempt_id text,
  error text,
  result jsonb,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table if not exists attempts (
  id text primary key,
  job_id text not null references execution_jobs(id),
  function_version_id text not null references function_versions(id),
  host_id text,
  region text not null,
  state text not null,
  lease_expires_at timestamptz not null,
  error text,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table if not exists hosts (
  id text primary key,
  region text not null,
  driver text not null,
  state text not null,
  available_slots integer not null,
  blank_warm integer not null,
  function_warm jsonb not null,
  last_heartbeat timestamptz not null default now()
);
