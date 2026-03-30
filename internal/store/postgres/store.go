package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/store"
)

type Store struct {
	pool *pgxpool.Pool
}

var _ store.Store = (*Store)(nil)

func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) EnsureProject(ctx context.Context, projectID, tenantID, name string) (*domain.Project, error) {
	now := time.Now().UTC()
	if _, err := s.pool.Exec(ctx, `
		insert into tenants (id, name, created_at)
		values ($1, $2, $3)
		on conflict (id) do nothing
	`, tenantID, tenantID, now); err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `
		insert into projects (id, tenant_id, name, created_at)
		values ($1, $2, $3, $4)
		on conflict (id) do nothing
	`, projectID, tenantID, name, now); err != nil {
		return nil, err
	}

	project := &domain.Project{}
	if err := s.pool.QueryRow(ctx, `
		select id, tenant_id, name, created_at
	from projects
	where id = $1
	`, projectID).Scan(&project.ID, &project.TenantID, &project.Name, &project.CreatedAt); err != nil {
		return nil, err
	}
	if project.TenantID != tenantID {
		return nil, store.ErrAccessDenied
	}
	return project, nil
}

func (s *Store) GetProject(ctx context.Context, projectID string) (*domain.Project, error) {
	project := &domain.Project{}
	if err := s.pool.QueryRow(ctx, `
		select id, tenant_id, name, created_at
		from projects
		where id = $1
	`, projectID).Scan(&project.ID, &project.TenantID, &project.Name, &project.CreatedAt); err != nil {
		return nil, mapNotFound(err)
	}
	return project, nil
}

func (s *Store) PutAPIKey(ctx context.Context, key *domain.APIKey) error {
	if _, err := s.pool.Exec(ctx, `
		insert into tenants (id, name, created_at)
		values ($1, $2, $3)
		on conflict (id) do nothing
	`, key.TenantID, key.TenantID, key.CreatedAt); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		insert into api_keys (key_hash, tenant_id, description, is_admin, disabled, created_at, last_used_at)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (key_hash) do update set
			tenant_id = excluded.tenant_id,
			description = excluded.description,
			is_admin = excluded.is_admin,
			disabled = excluded.disabled,
			created_at = excluded.created_at,
			last_used_at = excluded.last_used_at
	`, key.KeyHash, key.TenantID, nullableText(key.Description), key.IsAdmin, key.Disabled, key.CreatedAt, nullableTime(key.LastUsedAt))
	return err
}

func (s *Store) GetAPIKeyByHash(ctx context.Context, keyHash string) (*domain.APIKey, error) {
	var key domain.APIKey
	var lastUsedAt *time.Time
	if err := s.pool.QueryRow(ctx, `
		select key_hash, tenant_id, coalesce(description, ''), is_admin, disabled, created_at, last_used_at
		from api_keys
		where key_hash = $1
	`, keyHash).Scan(&key.KeyHash, &key.TenantID, &key.Description, &key.IsAdmin, &key.Disabled, &key.CreatedAt, &lastUsedAt); err != nil {
		return nil, mapNotFound(err)
	}
	if lastUsedAt != nil {
		key.LastUsedAt = *lastUsedAt
	}
	return &key, nil
}

func (s *Store) TouchAPIKeyLastUsed(ctx context.Context, keyHash string, usedAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		update api_keys
		set last_used_at = $2
		where key_hash = $1
	`, keyHash, usedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) CreateIdempotencyRecord(ctx context.Context, record *domain.IdempotencyRecord) error {
	_, err := s.pool.Exec(ctx, `
		insert into idempotency_records (
			scope, project_id, key, request_hash, resource_id, resource, status, created_at, updated_at
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, record.Scope, record.ProjectID, record.Key, record.RequestHash, nullableText(record.ResourceID),
		nullableText(record.Resource), string(record.Status), record.CreatedAt, record.UpdatedAt)
	return mapExecError(err)
}

func (s *Store) UpdateIdempotencyRecord(ctx context.Context, record *domain.IdempotencyRecord) error {
	_, err := s.pool.Exec(ctx, `
		update idempotency_records
		set request_hash = $4,
		    resource_id = $5,
		    resource = $6,
		    status = $7,
		    updated_at = $8
		where scope = $1 and project_id = $2 and key = $3
	`, record.Scope, record.ProjectID, record.Key, record.RequestHash, nullableText(record.ResourceID),
		nullableText(record.Resource), string(record.Status), record.UpdatedAt)
	return err
}

func (s *Store) DeleteIdempotencyRecord(ctx context.Context, scope, projectID, key string) error {
	_, err := s.pool.Exec(ctx, `
		delete from idempotency_records
		where scope = $1 and project_id = $2 and key = $3
	`, scope, projectID, key)
	return err
}

func (s *Store) GetIdempotencyRecord(ctx context.Context, scope, projectID, key string) (*domain.IdempotencyRecord, error) {
	var record domain.IdempotencyRecord
	var status string
	if err := s.pool.QueryRow(ctx, `
		select scope, project_id, key, request_hash, coalesce(resource_id, ''), coalesce(resource, ''),
		       status, created_at, updated_at
		from idempotency_records
		where scope = $1 and project_id = $2 and key = $3
	`, scope, projectID, key).Scan(
		&record.Scope, &record.ProjectID, &record.Key, &record.RequestHash,
		&record.ResourceID, &record.Resource, &status, &record.CreatedAt, &record.UpdatedAt,
	); err != nil {
		return nil, err
	}
	record.Status = domain.IdempotencyStatus(status)
	return &record, nil
}

func (s *Store) PutArtifact(ctx context.Context, artifact *domain.Artifact) error {
	regions, err := json.Marshal(artifact.Regions)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		insert into artifacts (digest, size_bytes, bundle_key, startup_key, regions, created_at)
		values ($1, $2, $3, $4, $5::jsonb, $6)
		on conflict (digest) do update set
			size_bytes = excluded.size_bytes,
			bundle_key = excluded.bundle_key,
			startup_key = excluded.startup_key,
			regions = excluded.regions
	`, artifact.Digest, artifact.SizeBytes, artifact.BundleKey, artifact.StartupKey, regions, artifact.CreatedAt)
	return err
}

func (s *Store) GetArtifact(ctx context.Context, digest string) (*domain.Artifact, error) {
	var artifact domain.Artifact
	var rawRegions []byte
	if err := s.pool.QueryRow(ctx, `
		select digest, size_bytes, bundle_key, startup_key, regions, created_at
		from artifacts
		where digest = $1
	`, digest).Scan(&artifact.Digest, &artifact.SizeBytes, &artifact.BundleKey, &artifact.StartupKey, &rawRegions, &artifact.CreatedAt); err != nil {
		return nil, mapNotFound(err)
	}
	if err := json.Unmarshal(rawRegions, &artifact.Regions); err != nil {
		return nil, err
	}
	return &artifact, nil
}

func (s *Store) PutFunctionVersion(ctx context.Context, version *domain.FunctionVersion) error {
	regions, err := json.Marshal(version.Regions)
	if err != nil {
		return err
	}
	envRefs, err := json.Marshal(version.EnvRefs)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		insert into function_versions (
			id, project_id, name, runtime, entrypoint, memory_mb, timeout_sec, network_policy,
			regions, env_refs, max_retries, build_job_id, artifact_digest, source_type, state, created_at
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10::jsonb, $11, $12, $13, $14, $15, $16)
		on conflict (id) do update set
			project_id = excluded.project_id,
			name = excluded.name,
			runtime = excluded.runtime,
			entrypoint = excluded.entrypoint,
			memory_mb = excluded.memory_mb,
			timeout_sec = excluded.timeout_sec,
			network_policy = excluded.network_policy,
			regions = excluded.regions,
			env_refs = excluded.env_refs,
			max_retries = excluded.max_retries,
			build_job_id = excluded.build_job_id,
			artifact_digest = excluded.artifact_digest,
			source_type = excluded.source_type,
			state = excluded.state
	`, version.ID, version.ProjectID, version.Name, version.Runtime, version.Entrypoint, version.MemoryMB,
		version.TimeoutSec, string(version.NetworkPolicy), regions, envRefs, version.MaxRetries,
		nullableText(version.BuildJobID), version.ArtifactDigest, string(version.SourceType), string(version.State), version.CreatedAt)
	if err != nil {
		return err
	}
	functionID := fmt.Sprintf("%s:%s", version.ProjectID, version.Name)
	_, err = s.pool.Exec(ctx, `
		insert into functions (id, project_id, name, latest_version_id, created_at)
		values ($1, $2, $3, $4, $5)
		on conflict (id) do update set
			project_id = excluded.project_id,
			name = excluded.name,
			latest_version_id = excluded.latest_version_id
	`, functionID, version.ProjectID, version.Name, version.ID, version.CreatedAt)
	return err
}

func (s *Store) GetFunctionVersion(ctx context.Context, versionID string) (*domain.FunctionVersion, error) {
	var version domain.FunctionVersion
	var rawRegions []byte
	var rawEnvRefs []byte
	var networkPolicy string
	var sourceType string
	var state string
	if err := s.pool.QueryRow(ctx, `
		select id, project_id, name, runtime, entrypoint, memory_mb, timeout_sec, network_policy,
		       regions, env_refs, max_retries, coalesce(build_job_id, ''), artifact_digest, source_type, state, created_at
		from function_versions
		where id = $1
	`, versionID).Scan(
		&version.ID, &version.ProjectID, &version.Name, &version.Runtime, &version.Entrypoint,
		&version.MemoryMB, &version.TimeoutSec, &networkPolicy, &rawRegions, &rawEnvRefs,
		&version.MaxRetries, &version.BuildJobID, &version.ArtifactDigest, &sourceType, &state, &version.CreatedAt,
	); err != nil {
		return nil, mapNotFound(err)
	}
	version.NetworkPolicy = domain.NetworkPolicy(networkPolicy)
	version.SourceType = domain.SourceType(sourceType)
	version.State = domain.FunctionState(state)
	if err := json.Unmarshal(rawRegions, &version.Regions); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rawEnvRefs, &version.EnvRefs); err != nil {
		return nil, err
	}
	return &version, nil
}

func (s *Store) PutBuildJob(ctx context.Context, job *domain.BuildJob) error {
	_, err := s.pool.Exec(ctx, `
		insert into build_jobs (id, function_version_id, target_region, state, error, request, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)
		on conflict (id) do update set
			function_version_id = excluded.function_version_id,
			target_region = excluded.target_region,
			state = excluded.state,
			error = excluded.error,
			request = excluded.request,
			updated_at = excluded.updated_at
	`, job.ID, job.FunctionVersionID, nullableText(job.TargetRegion), job.State, nullableText(job.Error), normalizeRawJSON(job.Request), job.CreatedAt, job.UpdatedAt)
	return err
}

func (s *Store) GetBuildJob(ctx context.Context, jobID string) (*domain.BuildJob, error) {
	var job domain.BuildJob
	var rawRequest []byte
	if err := s.pool.QueryRow(ctx, `
		select id, function_version_id, coalesce(target_region, ''), state, coalesce(error, ''), request, created_at, updated_at
		from build_jobs
		where id = $1
	`, jobID).Scan(
		&job.ID, &job.FunctionVersionID, &job.TargetRegion, &job.State, &job.Error, &rawRequest, &job.CreatedAt, &job.UpdatedAt,
	); err != nil {
		return nil, mapNotFound(err)
	}
	job.Request = append([]byte(nil), rawRequest...)
	return &job, nil
}

func (s *Store) PutExecutionJob(ctx context.Context, job *domain.ExecutionJob) error {
	return s.upsertExecutionJob(ctx, job)
}

func (s *Store) UpdateExecutionJob(ctx context.Context, job *domain.ExecutionJob) error {
	return s.upsertExecutionJob(ctx, job)
}

func (s *Store) upsertExecutionJob(ctx context.Context, job *domain.ExecutionJob) error {
	payload := normalizeRawJSON(job.Payload)
	result, err := json.Marshal(job.Result)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		insert into execution_jobs (
			id, function_version_id, project_id, target_region, state, payload, max_retries,
			attempt_count, last_attempt_id, error, result, created_at, updated_at
		)
		values ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11::jsonb, $12, $13)
		on conflict (id) do update set
			function_version_id = excluded.function_version_id,
			project_id = excluded.project_id,
			target_region = excluded.target_region,
			state = excluded.state,
			payload = excluded.payload,
			max_retries = excluded.max_retries,
			attempt_count = excluded.attempt_count,
			last_attempt_id = excluded.last_attempt_id,
			error = excluded.error,
			result = excluded.result,
			updated_at = excluded.updated_at
	`, job.ID, job.FunctionVersionID, job.ProjectID, nullableText(job.TargetRegion), string(job.State), payload,
		job.MaxRetries, job.AttemptCount, nullableText(job.LastAttemptID), nullableText(job.Error), result,
		job.CreatedAt, job.UpdatedAt)
	return err
}

func (s *Store) GetExecutionJob(ctx context.Context, jobID string) (*domain.ExecutionJob, error) {
	var job domain.ExecutionJob
	var rawPayload []byte
	var rawResult []byte
	var state string
	if err := s.pool.QueryRow(ctx, `
		select id, function_version_id, project_id, coalesce(target_region, ''), state, payload,
		       max_retries, attempt_count, coalesce(last_attempt_id, ''), coalesce(error, ''), result,
		       created_at, updated_at
		from execution_jobs
		where id = $1
	`, jobID).Scan(
		&job.ID, &job.FunctionVersionID, &job.ProjectID, &job.TargetRegion, &state, &rawPayload,
		&job.MaxRetries, &job.AttemptCount, &job.LastAttemptID, &job.Error, &rawResult,
		&job.CreatedAt, &job.UpdatedAt,
	); err != nil {
		return nil, mapNotFound(err)
	}
	job.State = domain.JobState(state)
	job.Payload = append([]byte(nil), rawPayload...)
	if len(rawResult) > 0 && string(rawResult) != "null" {
		var result domain.JobResult
		if err := json.Unmarshal(rawResult, &result); err != nil {
			return nil, err
		}
		job.Result = &result
	}
	return &job, nil
}

func (s *Store) ClaimNextExecutionJob(ctx context.Context, fromStates []domain.JobState, toState domain.JobState, now time.Time) (*domain.ExecutionJob, error) {
	states := make([]string, 0, len(fromStates))
	for _, state := range fromStates {
		states = append(states, string(state))
	}

	var job domain.ExecutionJob
	var rawPayload []byte
	var rawResult []byte
	var state string
	err := s.pool.QueryRow(ctx, `
		with next_job as (
			select id
			from execution_jobs
			where state = any($1)
			order by updated_at asc, created_at asc, id asc
			for update skip locked
			limit 1
		)
		update execution_jobs j
		set state = $2,
		    updated_at = $3
		from next_job
		where j.id = next_job.id
		returning id, function_version_id, project_id, coalesce(target_region, ''), state, payload,
		          max_retries, attempt_count, coalesce(last_attempt_id, ''), coalesce(error, ''), result,
		          created_at, updated_at
	`, states, string(toState), now).Scan(
		&job.ID, &job.FunctionVersionID, &job.ProjectID, &job.TargetRegion, &state, &rawPayload,
		&job.MaxRetries, &job.AttemptCount, &job.LastAttemptID, &job.Error, &rawResult,
		&job.CreatedAt, &job.UpdatedAt,
	)
	if err != nil {
		return nil, mapNotFound(err)
	}

	job.State = domain.JobState(state)
	job.Payload = append([]byte(nil), rawPayload...)
	if len(rawResult) > 0 && string(rawResult) != "null" {
		var result domain.JobResult
		if err := json.Unmarshal(rawResult, &result); err != nil {
			return nil, err
		}
		job.Result = &result
	}
	return &job, nil
}

func (s *Store) PutAttempt(ctx context.Context, attempt *domain.Attempt) error {
	return s.upsertAttempt(ctx, attempt)
}

func (s *Store) UpdateAttempt(ctx context.Context, attempt *domain.Attempt) error {
	return s.upsertAttempt(ctx, attempt)
}

func (s *Store) upsertAttempt(ctx context.Context, attempt *domain.Attempt) error {
	_, err := s.pool.Exec(ctx, `
		insert into attempts (
			id, job_id, function_version_id, host_id, region, state, start_mode, started_at, lease_expires_at, error, created_at, updated_at
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		on conflict (id) do update set
			job_id = excluded.job_id,
			function_version_id = excluded.function_version_id,
			host_id = excluded.host_id,
			region = excluded.region,
			state = excluded.state,
			start_mode = excluded.start_mode,
			started_at = excluded.started_at,
			lease_expires_at = excluded.lease_expires_at,
			error = excluded.error,
			updated_at = excluded.updated_at
	`, attempt.ID, attempt.JobID, attempt.FunctionVersionID, nullableText(attempt.HostID), attempt.Region, string(attempt.State),
		nullableText(string(attempt.StartMode)), nullableTime(attempt.StartedAt), attempt.LeaseExpiresAt, nullableText(attempt.Error), attempt.CreatedAt, attempt.UpdatedAt)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		insert into leases (attempt_id, host_id, region, expires_at, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (attempt_id) do update set
			host_id = excluded.host_id,
			region = excluded.region,
			expires_at = excluded.expires_at,
			updated_at = excluded.updated_at
	`, attempt.ID, nullableText(attempt.HostID), attempt.Region, attempt.LeaseExpiresAt, attempt.CreatedAt, attempt.UpdatedAt)
	return err
}

func (s *Store) GetAttempt(ctx context.Context, attemptID string) (*domain.Attempt, error) {
	var attempt domain.Attempt
	var state string
	var startMode string
	var startedAt *time.Time
	if err := s.pool.QueryRow(ctx, `
		select id, job_id, function_version_id, coalesce(host_id, ''), region, state, coalesce(start_mode, ''), started_at, lease_expires_at,
		       coalesce(error, ''), created_at, updated_at
		from attempts
		where id = $1
	`, attemptID).Scan(
		&attempt.ID, &attempt.JobID, &attempt.FunctionVersionID, &attempt.HostID, &attempt.Region,
		&state, &startMode, &startedAt, &attempt.LeaseExpiresAt, &attempt.Error, &attempt.CreatedAt, &attempt.UpdatedAt,
	); err != nil {
		return nil, mapNotFound(err)
	}
	attempt.State = domain.AttemptState(state)
	attempt.StartMode = domain.StartMode(startMode)
	if startedAt != nil {
		attempt.StartedAt = *startedAt
	}
	return &attempt, nil
}

func (s *Store) ListAttemptsByJob(ctx context.Context, jobID string) ([]domain.Attempt, error) {
	rows, err := s.pool.Query(ctx, `
		select id, job_id, function_version_id, coalesce(host_id, ''), region, state, coalesce(start_mode, ''),
		       started_at, lease_expires_at, coalesce(error, ''), created_at, updated_at
		from attempts
		where job_id = $1
		order by created_at asc, id asc
	`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	attempts := make([]domain.Attempt, 0)
	for rows.Next() {
		var attempt domain.Attempt
		var state string
		var startMode string
		var startedAt *time.Time
		if err := rows.Scan(
			&attempt.ID, &attempt.JobID, &attempt.FunctionVersionID, &attempt.HostID, &attempt.Region,
			&state, &startMode, &startedAt, &attempt.LeaseExpiresAt, &attempt.Error, &attempt.CreatedAt, &attempt.UpdatedAt,
		); err != nil {
			return nil, err
		}
		attempt.State = domain.AttemptState(state)
		attempt.StartMode = domain.StartMode(startMode)
		if startedAt != nil {
			attempt.StartedAt = *startedAt
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) PutCostRecord(ctx context.Context, record *domain.CostRecord) error {
	_, err := s.pool.Exec(ctx, `
		insert into cost_records (
			id, tenant_id, project_id, job_id, attempt_id, host_id, region, start_mode,
			cpu_ms, memory_mb_ms, warm_instance_ms, data_egress_bytes, created_at
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		on conflict (id) do update set
			tenant_id = excluded.tenant_id,
			project_id = excluded.project_id,
			job_id = excluded.job_id,
			attempt_id = excluded.attempt_id,
			host_id = excluded.host_id,
			region = excluded.region,
			start_mode = excluded.start_mode,
			cpu_ms = excluded.cpu_ms,
			memory_mb_ms = excluded.memory_mb_ms,
			warm_instance_ms = excluded.warm_instance_ms,
			data_egress_bytes = excluded.data_egress_bytes,
			created_at = excluded.created_at
	`, record.ID, record.TenantID, record.ProjectID, nullableText(record.JobID), nullableText(record.AttemptID),
		nullableText(record.HostID), nullableText(record.Region), nullableText(string(record.StartMode)),
		record.CPUMs, record.MemoryMBMs, record.WarmInstanceMs, record.DataEgressBytes, record.CreatedAt)
	return err
}

func (s *Store) ListCostRecordsByJob(ctx context.Context, jobID string) ([]domain.CostRecord, error) {
	rows, err := s.pool.Query(ctx, `
		select id, tenant_id, project_id, coalesce(job_id, ''), coalesce(attempt_id, ''), coalesce(host_id, ''),
		       coalesce(region, ''), coalesce(start_mode, ''), cpu_ms, memory_mb_ms, warm_instance_ms,
		       data_egress_bytes, created_at
		from cost_records
		where job_id = $1
		order by created_at asc, id asc
	`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := make([]domain.CostRecord, 0)
	for rows.Next() {
		var record domain.CostRecord
		var startMode string
		if err := rows.Scan(
			&record.ID, &record.TenantID, &record.ProjectID, &record.JobID, &record.AttemptID,
			&record.HostID, &record.Region, &startMode, &record.CPUMs, &record.MemoryMBMs,
			&record.WarmInstanceMs, &record.DataEgressBytes, &record.CreatedAt,
		); err != nil {
			return nil, err
		}
		record.StartMode = domain.StartMode(startMode)
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) ListExpiredAttempts(ctx context.Context, before time.Time) ([]domain.Attempt, error) {
	rows, err := s.pool.Query(ctx, `
		select id, job_id, function_version_id, coalesce(host_id, ''), region, state, coalesce(start_mode, ''),
		       started_at, lease_expires_at,
		       coalesce(error, ''), created_at, updated_at
		from attempts
		where state not in ('succeeded', 'failed') and lease_expires_at <= $1
		order by lease_expires_at asc
	`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	attempts := make([]domain.Attempt, 0)
	for rows.Next() {
		var attempt domain.Attempt
		var state string
		var startMode string
		var startedAt *time.Time
		if err := rows.Scan(
			&attempt.ID, &attempt.JobID, &attempt.FunctionVersionID, &attempt.HostID, &attempt.Region,
			&state, &startMode, &startedAt, &attempt.LeaseExpiresAt, &attempt.Error, &attempt.CreatedAt, &attempt.UpdatedAt,
		); err != nil {
			return nil, err
		}
		attempt.State = domain.AttemptState(state)
		attempt.StartMode = domain.StartMode(startMode)
		if startedAt != nil {
			attempt.StartedAt = *startedAt
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) PutHost(ctx context.Context, host *domain.Host) error {
	return s.upsertHost(ctx, host)
}

func (s *Store) UpdateHost(ctx context.Context, host *domain.Host) error {
	return s.upsertHost(ctx, host)
}

func (s *Store) upsertHost(ctx context.Context, host *domain.Host) error {
	functionWarm, err := json.Marshal(host.FunctionWarm)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		insert into hosts (id, region, driver, state, available_slots, blank_warm, function_warm, last_heartbeat)
		values ($1, $2, $3, $4, $5, $6, $7::jsonb, $8)
		on conflict (id) do update set
			region = excluded.region,
			driver = excluded.driver,
			state = excluded.state,
			available_slots = excluded.available_slots,
			blank_warm = excluded.blank_warm,
			function_warm = excluded.function_warm,
			last_heartbeat = excluded.last_heartbeat
	`, host.ID, host.Region, host.Driver, string(host.State), host.AvailableSlots, host.BlankWarm, functionWarm, host.LastHeartbeat)
	return err
}

func (s *Store) GetHost(ctx context.Context, hostID string) (*domain.Host, error) {
	var host domain.Host
	var rawFunctionWarm []byte
	var state string
	if err := s.pool.QueryRow(ctx, `
		select id, region, driver, state, available_slots, blank_warm, function_warm, last_heartbeat
		from hosts
		where id = $1
	`, hostID).Scan(
		&host.ID, &host.Region, &host.Driver, &state, &host.AvailableSlots,
		&host.BlankWarm, &rawFunctionWarm, &host.LastHeartbeat,
	); err != nil {
		return nil, mapNotFound(err)
	}
	host.State = domain.HostState(state)
	if err := json.Unmarshal(rawFunctionWarm, &host.FunctionWarm); err != nil {
		return nil, err
	}
	return &host, nil
}

func (s *Store) ListHostsByRegion(ctx context.Context, region string) ([]domain.Host, error) {
	rows, err := s.pool.Query(ctx, `
		select id, region, driver, state, available_slots, blank_warm, function_warm, last_heartbeat
		from hosts
		where region = $1
		order by id asc
	`, region)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hosts := make([]domain.Host, 0)
	for rows.Next() {
		var host domain.Host
		var rawFunctionWarm []byte
		var state string
		if err := rows.Scan(
			&host.ID, &host.Region, &host.Driver, &state, &host.AvailableSlots,
			&host.BlankWarm, &rawFunctionWarm, &host.LastHeartbeat,
		); err != nil {
			return nil, err
		}
		host.State = domain.HostState(state)
		if err := json.Unmarshal(rawFunctionWarm, &host.FunctionWarm); err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, rows.Err()
}

func (s *Store) ReplaceWarmPoolsForHost(ctx context.Context, region, hostID string, pools []domain.WarmPool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		delete from warm_pools
		where region = $1 and host_id = $2
	`, region, hostID); err != nil {
		return err
	}
	for _, pool := range pools {
		if _, err := tx.Exec(ctx, `
			insert into warm_pools (region, host_id, function_version_id, blank_warm, function_warm, updated_at)
			values ($1, $2, $3, $4, $5, $6)
		`, pool.Region, pool.HostID, pool.FunctionVersionID, pool.BlankWarm, pool.FunctionWarm, pool.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) ListWarmPoolsByRegion(ctx context.Context, region string) ([]domain.WarmPool, error) {
	rows, err := s.pool.Query(ctx, `
		select region, host_id, function_version_id, blank_warm, function_warm, updated_at
		from warm_pools
		where region = $1
		order by host_id asc, function_version_id asc
	`, region)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pools := make([]domain.WarmPool, 0)
	for rows.Next() {
		var pool domain.WarmPool
		if err := rows.Scan(
			&pool.Region, &pool.HostID, &pool.FunctionVersionID, &pool.BlankWarm, &pool.FunctionWarm, &pool.UpdatedAt,
		); err != nil {
			return nil, err
		}
		pools = append(pools, pool)
	}
	return pools, rows.Err()
}

func (s *Store) PutWebhookTrigger(ctx context.Context, trigger *domain.WebhookTrigger) error {
	_, err := s.pool.Exec(ctx, `
		insert into webhook_triggers (token, project_id, function_version_id, description, enabled, created_at)
		values ($1, $2, $3, $4, $5, $6)
	`, trigger.Token, trigger.ProjectID, trigger.FunctionVersionID, nullableText(trigger.Description), trigger.Enabled, trigger.CreatedAt)
	return mapExecError(err)
}

func (s *Store) GetWebhookTrigger(ctx context.Context, token string) (*domain.WebhookTrigger, error) {
	var trigger domain.WebhookTrigger
	if err := s.pool.QueryRow(ctx, `
		select token, project_id, function_version_id, coalesce(description, ''), enabled, created_at
		from webhook_triggers
		where token = $1
	`, token).Scan(
		&trigger.Token, &trigger.ProjectID, &trigger.FunctionVersionID, &trigger.Description, &trigger.Enabled, &trigger.CreatedAt,
	); err != nil {
		return nil, mapNotFound(err)
	}
	return &trigger, nil
}

func (s *Store) ListWebhookTriggersByFunctionVersion(ctx context.Context, versionID string) ([]domain.WebhookTrigger, error) {
	rows, err := s.pool.Query(ctx, `
		select token, project_id, function_version_id, coalesce(description, ''), enabled, created_at
		from webhook_triggers
		where function_version_id = $1
		order by created_at asc, token asc
	`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	triggers := make([]domain.WebhookTrigger, 0)
	for rows.Next() {
		var trigger domain.WebhookTrigger
		if err := rows.Scan(
			&trigger.Token, &trigger.ProjectID, &trigger.FunctionVersionID, &trigger.Description, &trigger.Enabled, &trigger.CreatedAt,
		); err != nil {
			return nil, err
		}
		triggers = append(triggers, trigger)
	}
	return triggers, rows.Err()
}

func (s *Store) PutRegion(ctx context.Context, region *domain.Region) error {
	_, err := s.pool.Exec(ctx, `
		insert into regions (name, state, available_hosts, blank_warm, function_warm, last_heartbeat_at, last_error)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (name) do update set
			state = excluded.state,
			available_hosts = excluded.available_hosts,
			blank_warm = excluded.blank_warm,
			function_warm = excluded.function_warm,
			last_heartbeat_at = excluded.last_heartbeat_at,
			last_error = excluded.last_error
	`, region.Name, region.State, region.AvailableHosts, region.BlankWarm, region.FunctionWarm, region.LastHeartbeatAt, nullableText(region.LastError))
	return err
}

func (s *Store) GetRegion(ctx context.Context, regionName string) (*domain.Region, error) {
	var region domain.Region
	if err := s.pool.QueryRow(ctx, `
		select name, state, available_hosts, blank_warm, function_warm, last_heartbeat_at, coalesce(last_error, '')
		from regions
		where name = $1
	`, regionName).Scan(
		&region.Name, &region.State, &region.AvailableHosts, &region.BlankWarm,
		&region.FunctionWarm, &region.LastHeartbeatAt, &region.LastError,
	); err != nil {
		return nil, mapNotFound(err)
	}
	return &region, nil
}

func (s *Store) ListRegions(ctx context.Context) ([]domain.Region, error) {
	rows, err := s.pool.Query(ctx, `
		select name, state, available_hosts, blank_warm, function_warm, last_heartbeat_at, coalesce(last_error, '')
		from regions
		order by name asc
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	regions := make([]domain.Region, 0)
	for rows.Next() {
		var region domain.Region
		if err := rows.Scan(
			&region.Name, &region.State, &region.AvailableHosts, &region.BlankWarm,
			&region.FunctionWarm, &region.LastHeartbeatAt, &region.LastError,
		); err != nil {
			return nil, err
		}
		regions = append(regions, region)
	}
	sort.SliceStable(regions, func(i, j int) bool { return regions[i].Name < regions[j].Name })
	return regions, rows.Err()
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func mapExecError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: %s", store.ErrAlreadyExists, pgErr.ConstraintName)
	}
	return err
}

func mapNotFound(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	return err
}

func normalizeRawJSON(value json.RawMessage) []byte {
	if len(value) == 0 {
		return []byte("null")
	}
	return append([]byte(nil), value...)
}
