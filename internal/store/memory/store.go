package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/store"
)

type Store struct {
	mu              sync.RWMutex
	apiKeys         map[string]domain.APIKey
	projects        map[string]domain.Project
	idempotency     map[string]domain.IdempotencyRecord
	artifacts       map[string]domain.Artifact
	functions       map[string]domain.FunctionVersion
	buildJobs       map[string]domain.BuildJob
	executionJobs   map[string]domain.ExecutionJob
	attempts        map[string]domain.Attempt
	costRecords     map[string]domain.CostRecord
	hosts           map[string]domain.Host
	warmPools       map[string]domain.WarmPool
	webhookTriggers map[string]domain.WebhookTrigger
	regions         map[string]domain.Region
}

var _ store.Store = (*Store)(nil)

func New() *Store {
	return &Store{
		apiKeys:         make(map[string]domain.APIKey),
		projects:        make(map[string]domain.Project),
		idempotency:     make(map[string]domain.IdempotencyRecord),
		artifacts:       make(map[string]domain.Artifact),
		functions:       make(map[string]domain.FunctionVersion),
		buildJobs:       make(map[string]domain.BuildJob),
		executionJobs:   make(map[string]domain.ExecutionJob),
		attempts:        make(map[string]domain.Attempt),
		costRecords:     make(map[string]domain.CostRecord),
		hosts:           make(map[string]domain.Host),
		warmPools:       make(map[string]domain.WarmPool),
		webhookTriggers: make(map[string]domain.WebhookTrigger),
		regions:         make(map[string]domain.Region),
	}
}

func (s *Store) EnsureProject(_ context.Context, projectID, tenantID, name string) (*domain.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	project, ok := s.projects[projectID]
	if !ok {
		project = domain.Project{ID: projectID, TenantID: tenantID, Name: name}
		s.projects[projectID] = project
	} else if project.TenantID != tenantID {
		return nil, store.ErrAccessDenied
	}
	cp := project
	return &cp, nil
}

func (s *Store) GetProject(_ context.Context, projectID string) (*domain.Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	project, ok := s.projects[projectID]
	if !ok {
		return nil, fmt.Errorf("%w: project %q", store.ErrNotFound, projectID)
	}
	cp := project
	return &cp, nil
}

func (s *Store) PutAPIKey(_ context.Context, key *domain.APIKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apiKeys[key.KeyHash] = *key
	return nil
}

func (s *Store) GetAPIKeyByHash(_ context.Context, keyHash string) (*domain.APIKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.apiKeys[keyHash]
	if !ok {
		return nil, fmt.Errorf("%w: api key %q", store.ErrNotFound, keyHash)
	}
	cp := key
	return &cp, nil
}

func (s *Store) TouchAPIKeyLastUsed(_ context.Context, keyHash string, usedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.apiKeys[keyHash]
	if !ok {
		return fmt.Errorf("%w: api key %q", store.ErrNotFound, keyHash)
	}
	key.LastUsedAt = usedAt
	s.apiKeys[keyHash] = key
	return nil
}

func (s *Store) CreateIdempotencyRecord(_ context.Context, record *domain.IdempotencyRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := idempotencyKey(record.Scope, record.ProjectID, record.Key)
	if _, exists := s.idempotency[key]; exists {
		return fmt.Errorf("%w: idempotency record %s", store.ErrAlreadyExists, key)
	}
	s.idempotency[key] = cloneIdempotencyRecord(*record)
	return nil
}

func (s *Store) UpdateIdempotencyRecord(_ context.Context, record *domain.IdempotencyRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := idempotencyKey(record.Scope, record.ProjectID, record.Key)
	s.idempotency[key] = cloneIdempotencyRecord(*record)
	return nil
}

func (s *Store) DeleteIdempotencyRecord(_ context.Context, scope, projectID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.idempotency, idempotencyKey(scope, projectID, key))
	return nil
}

func (s *Store) GetIdempotencyRecord(_ context.Context, scope, projectID, key string) (*domain.IdempotencyRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.idempotency[idempotencyKey(scope, projectID, key)]
	if !ok {
		return nil, fmt.Errorf("idempotency record %q not found", idempotencyKey(scope, projectID, key))
	}
	cp := cloneIdempotencyRecord(record)
	return &cp, nil
}

func (s *Store) PutArtifact(_ context.Context, artifact *domain.Artifact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := cloneArtifact(*artifact)
	s.artifacts[artifact.Digest] = cp
	return nil
}

func (s *Store) GetArtifact(_ context.Context, digest string) (*domain.Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	artifact, ok := s.artifacts[digest]
	if !ok {
		return nil, fmt.Errorf("artifact %q not found", digest)
	}
	cp := cloneArtifact(artifact)
	return &cp, nil
}

func (s *Store) PutFunctionVersion(_ context.Context, version *domain.FunctionVersion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := cloneFunctionVersion(*version)
	s.functions[version.ID] = cp
	return nil
}

func (s *Store) GetFunctionVersion(_ context.Context, versionID string) (*domain.FunctionVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	version, ok := s.functions[versionID]
	if !ok {
		return nil, fmt.Errorf("function version %q not found", versionID)
	}
	cp := cloneFunctionVersion(version)
	return &cp, nil
}

func (s *Store) PutBuildJob(_ context.Context, job *domain.BuildJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buildJobs[job.ID] = cloneBuildJob(*job)
	return nil
}

func (s *Store) GetBuildJob(_ context.Context, jobID string) (*domain.BuildJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.buildJobs[jobID]
	if !ok {
		return nil, fmt.Errorf("%w: build job %q", store.ErrNotFound, jobID)
	}
	cp := cloneBuildJob(job)
	return &cp, nil
}

func (s *Store) PutExecutionJob(_ context.Context, job *domain.ExecutionJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.executionJobs[job.ID] = cloneExecutionJob(*job)
	return nil
}

func (s *Store) UpdateExecutionJob(_ context.Context, job *domain.ExecutionJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.executionJobs[job.ID] = cloneExecutionJob(*job)
	return nil
}

func (s *Store) GetExecutionJob(_ context.Context, jobID string) (*domain.ExecutionJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.executionJobs[jobID]
	if !ok {
		return nil, fmt.Errorf("job %q not found", jobID)
	}
	cp := cloneExecutionJob(job)
	return &cp, nil
}

func (s *Store) ClaimNextExecutionJob(_ context.Context, fromStates []domain.JobState, toState domain.JobState, now time.Time) (*domain.ExecutionJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	allowed := make(map[domain.JobState]struct{}, len(fromStates))
	for _, state := range fromStates {
		allowed[state] = struct{}{}
	}

	var chosen *domain.ExecutionJob
	for _, job := range s.executionJobs {
		if _, ok := allowed[job.State]; !ok {
			continue
		}
		candidate := cloneExecutionJob(job)
		if chosen == nil ||
			candidate.UpdatedAt.Before(chosen.UpdatedAt) ||
			(candidate.UpdatedAt.Equal(chosen.UpdatedAt) && candidate.CreatedAt.Before(chosen.CreatedAt)) ||
			(candidate.UpdatedAt.Equal(chosen.UpdatedAt) && candidate.CreatedAt.Equal(chosen.CreatedAt) && candidate.ID < chosen.ID) {
			chosen = &candidate
		}
	}
	if chosen == nil {
		return nil, store.ErrNotFound
	}

	chosen.State = toState
	chosen.UpdatedAt = now
	s.executionJobs[chosen.ID] = cloneExecutionJob(*chosen)
	return chosen, nil
}

func (s *Store) PutAttempt(_ context.Context, attempt *domain.Attempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[attempt.ID] = *attempt
	return nil
}

func (s *Store) UpdateAttempt(_ context.Context, attempt *domain.Attempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[attempt.ID] = *attempt
	return nil
}

func (s *Store) GetAttempt(_ context.Context, attemptID string) (*domain.Attempt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	attempt, ok := s.attempts[attemptID]
	if !ok {
		return nil, fmt.Errorf("attempt %q not found", attemptID)
	}
	cp := attempt
	return &cp, nil
}

func (s *Store) ListAttemptsByJob(_ context.Context, jobID string) ([]domain.Attempt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	attempts := make([]domain.Attempt, 0)
	for _, attempt := range s.attempts {
		if attempt.JobID == jobID {
			attempts = append(attempts, attempt)
		}
	}
	sort.SliceStable(attempts, func(i, j int) bool {
		if attempts[i].CreatedAt.Equal(attempts[j].CreatedAt) {
			return attempts[i].ID < attempts[j].ID
		}
		return attempts[i].CreatedAt.Before(attempts[j].CreatedAt)
	})
	return attempts, nil
}

func (s *Store) PutCostRecord(_ context.Context, record *domain.CostRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.costRecords[record.ID] = *record
	return nil
}

func (s *Store) ListCostRecordsByJob(_ context.Context, jobID string) ([]domain.CostRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := make([]domain.CostRecord, 0)
	for _, record := range s.costRecords {
		if record.JobID == jobID {
			records = append(records, record)
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].ID < records[j].ID
		}
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})
	return records, nil
}

func (s *Store) ListExpiredAttempts(_ context.Context, before time.Time) ([]domain.Attempt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	expired := make([]domain.Attempt, 0)
	for _, attempt := range s.attempts {
		if attempt.State.Terminal() {
			continue
		}
		if !attempt.LeaseExpiresAt.IsZero() && !attempt.LeaseExpiresAt.After(before) {
			expired = append(expired, attempt)
		}
	}
	return expired, nil
}

func (s *Store) PutHost(_ context.Context, host *domain.Host) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts[host.ID] = cloneHost(*host)
	return nil
}

func (s *Store) UpdateHost(_ context.Context, host *domain.Host) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts[host.ID] = cloneHost(*host)
	return nil
}

func (s *Store) GetHost(_ context.Context, hostID string) (*domain.Host, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	host, ok := s.hosts[hostID]
	if !ok {
		return nil, fmt.Errorf("host %q not found", hostID)
	}
	cp := cloneHost(host)
	return &cp, nil
}

func (s *Store) ListHostsByRegion(_ context.Context, region string) ([]domain.Host, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	hosts := make([]domain.Host, 0)
	for _, host := range s.hosts {
		if host.Region == region {
			hosts = append(hosts, cloneHost(host))
		}
	}
	return hosts, nil
}

func (s *Store) ReplaceWarmPoolsForHost(_ context.Context, region, hostID string, pools []domain.WarmPool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := warmPoolKey(region, hostID, "")
	for key := range s.warmPools {
		if strings.HasPrefix(key, prefix) {
			delete(s.warmPools, key)
		}
	}
	for _, pool := range pools {
		s.warmPools[warmPoolKey(pool.Region, pool.HostID, pool.FunctionVersionID)] = pool
	}
	return nil
}

func (s *Store) ListWarmPoolsByRegion(_ context.Context, region string) ([]domain.WarmPool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pools := make([]domain.WarmPool, 0)
	for _, pool := range s.warmPools {
		if pool.Region == region {
			pools = append(pools, pool)
		}
	}
	sort.SliceStable(pools, func(i, j int) bool {
		if pools[i].HostID == pools[j].HostID {
			return pools[i].FunctionVersionID < pools[j].FunctionVersionID
		}
		return pools[i].HostID < pools[j].HostID
	})
	return pools, nil
}

func (s *Store) PutWebhookTrigger(_ context.Context, trigger *domain.WebhookTrigger) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.webhookTriggers[trigger.Token]; exists {
		return fmt.Errorf("%w: webhook trigger %s", store.ErrAlreadyExists, trigger.Token)
	}
	s.webhookTriggers[trigger.Token] = *trigger
	return nil
}

func (s *Store) GetWebhookTrigger(_ context.Context, token string) (*domain.WebhookTrigger, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	trigger, ok := s.webhookTriggers[token]
	if !ok {
		return nil, fmt.Errorf("webhook trigger %q not found", token)
	}
	cp := trigger
	return &cp, nil
}

func (s *Store) ListWebhookTriggersByFunctionVersion(_ context.Context, versionID string) ([]domain.WebhookTrigger, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	triggers := make([]domain.WebhookTrigger, 0)
	for _, trigger := range s.webhookTriggers {
		if trigger.FunctionVersionID == versionID {
			triggers = append(triggers, trigger)
		}
	}
	return triggers, nil
}

func (s *Store) PutRegion(_ context.Context, region *domain.Region) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.regions[region.Name] = *region
	return nil
}

func (s *Store) GetRegion(_ context.Context, region string) (*domain.Region, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.regions[region]
	if !ok {
		return nil, fmt.Errorf("region %q not found", region)
	}
	cp := value
	return &cp, nil
}

func (s *Store) ListRegions(_ context.Context) ([]domain.Region, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	regions := make([]domain.Region, 0, len(s.regions))
	for _, region := range s.regions {
		regions = append(regions, region)
	}
	return regions, nil
}

func cloneArtifact(a domain.Artifact) domain.Artifact {
	cp := a
	cp.Regions = make(map[string]time.Time, len(a.Regions))
	for k, v := range a.Regions {
		cp.Regions[k] = v
	}
	return cp
}

func cloneIdempotencyRecord(record domain.IdempotencyRecord) domain.IdempotencyRecord {
	return record
}

func cloneFunctionVersion(v domain.FunctionVersion) domain.FunctionVersion {
	cp := v
	cp.Regions = append([]string(nil), v.Regions...)
	cp.EnvRefs = append([]string(nil), v.EnvRefs...)
	return cp
}

func cloneBuildJob(job domain.BuildJob) domain.BuildJob {
	cp := job
	cp.Request = append([]byte(nil), job.Request...)
	return cp
}

func cloneExecutionJob(j domain.ExecutionJob) domain.ExecutionJob {
	cp := j
	cp.Payload = append([]byte(nil), j.Payload...)
	if j.Result != nil {
		result := *j.Result
		result.Output = append([]byte(nil), j.Result.Output...)
		cp.Result = &result
	}
	return cp
}

func cloneHost(h domain.Host) domain.Host {
	cp := h
	cp.FunctionWarm = make(map[string]int, len(h.FunctionWarm))
	for k, v := range h.FunctionWarm {
		cp.FunctionWarm[k] = v
	}
	return cp
}

func idempotencyKey(scope, projectID, key string) string {
	return scope + ":" + projectID + ":" + key
}

func warmPoolKey(region, hostID, functionVersionID string) string {
	return region + ":" + hostID + ":" + functionVersionID
}
