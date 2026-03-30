create table if not exists http_triggers (
    token text primary key,
    project_id text not null,
    function_version_id text not null,
    description text,
    auth_mode text not null default 'none',
    enabled boolean not null default true,
    created_at timestamptz not null
);

create index if not exists idx_http_triggers_version on http_triggers (function_version_id);
