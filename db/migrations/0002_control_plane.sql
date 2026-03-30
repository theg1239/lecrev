create table if not exists api_keys (
  key_hash text primary key,
  tenant_id text not null references tenants(id),
  description text,
  created_at timestamptz not null default now()
);

create table if not exists secret_refs (
  id text primary key,
  tenant_id text not null references tenants(id),
  provider text not null,
  external_ref text not null,
  created_at timestamptz not null default now()
);

create table if not exists functions (
  id text primary key,
  project_id text not null references projects(id),
  name text not null,
  latest_version_id text references function_versions(id),
  created_at timestamptz not null default now()
);

create table if not exists leases (
  attempt_id text primary key references attempts(id) on delete cascade,
  host_id text,
  region text not null,
  expires_at timestamptz not null,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table if not exists regions (
  name text primary key,
  state text not null,
  available_hosts integer not null default 0,
  blank_warm integer not null default 0,
  function_warm integer not null default 0,
  last_heartbeat_at timestamptz not null default now(),
  last_error text
);

create table if not exists warm_pools (
  region text not null,
  host_id text not null references hosts(id) on delete cascade,
  function_version_id text not null,
  blank_warm integer not null default 0,
  function_warm integer not null default 0,
  updated_at timestamptz not null default now(),
  primary key (region, host_id, function_version_id)
);

create table if not exists cost_records (
  id text primary key,
  tenant_id text not null references tenants(id),
  project_id text not null references projects(id),
  job_id text references execution_jobs(id),
  cpu_ms bigint not null default 0,
  memory_mb_ms bigint not null default 0,
  warm_instance_ms bigint not null default 0,
  data_egress_bytes bigint not null default 0,
  created_at timestamptz not null default now()
);

create index if not exists idx_attempts_lease_expires_at on attempts (lease_expires_at);
create index if not exists idx_execution_jobs_state on execution_jobs (state);
create index if not exists idx_hosts_region on hosts (region);
create index if not exists idx_leases_expires_at on leases (expires_at);
