package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/idempotency"
	"github.com/theg1239/lecrev/internal/store"
)

type RegionDispatcher interface {
	Region() string
	Stats() domain.RegionStats
	EnqueueExecution(ctx context.Context, assignment domain.Assignment) error
}

type DirectDispatcher interface {
	ExecuteDirect(ctx context.Context, assignment domain.Assignment) (*domain.DirectExecutionResult, error)
}

type WarmPreparer interface {
	PrepareFunctionWarm(ctx context.Context, version *domain.FunctionVersion) error
}

type Service struct {
	store                            store.Store
	dispatchers                      map[string]RegionDispatcher
	now                              func() time.Time
	pollInterval                     time.Duration
	schedulingTimeout                time.Duration
	healthyWithin                    time.Duration
	wakeCh                           chan struct{}
	maxActiveExecutionJobsPerProject int
}

type candidate struct {
	region        string
	dispatcher    RegionDispatcher
	stats         domain.RegionStats
	backlog       int
	healthy       bool
	artifactLocal bool
	lastHeartbeat time.Time
}

func New(store store.Store, dispatchers []RegionDispatcher) *Service {
	index := make(map[string]RegionDispatcher, len(dispatchers))
	for _, dispatcher := range dispatchers {
		index[dispatcher.Region()] = dispatcher
	}
	return &Service{
		store:                            store,
		dispatchers:                      index,
		now:                              func() time.Time { return time.Now().UTC() },
		pollInterval:                     50 * time.Millisecond,
		schedulingTimeout:                10 * time.Second,
		healthyWithin:                    15 * time.Second,
		wakeCh:                           make(chan struct{}, 1),
		maxActiveExecutionJobsPerProject: 200,
	}
}

func (s *Service) DispatchExecution(ctx context.Context, versionID string, payload []byte) (*domain.ExecutionJob, error) {
	return s.DispatchExecutionIdempotent(ctx, versionID, payload, "")
}

func (s *Service) DispatchExecutionIdempotent(ctx context.Context, versionID string, payload []byte, idempotencyKey string) (*domain.ExecutionJob, error) {
	started := time.Now()
	var (
		versionLookupMs int64
		idempotencyMs   int64
		enqueueMs       int64
		jobID           string
		state           = "started"
		errMsg          string
	)
	defer func() {
		slog.Info("scheduler dispatch request timing",
			"functionVersionID", versionID,
			"jobID", jobID,
			"state", state,
			"error", errMsg,
			"versionLookupMs", versionLookupMs,
			"idempotencyMs", idempotencyMs,
			"enqueueMs", enqueueMs,
			"totalMs", time.Since(started).Milliseconds(),
		)
	}()
	versionLookupStarted := time.Now()
	version, err := s.store.GetFunctionVersion(ctx, versionID)
	versionLookupMs = time.Since(versionLookupStarted).Milliseconds()
	if err != nil {
		state = "version_lookup_failed"
		errMsg = err.Error()
		return nil, err
	}
	if idempotencyKey != "" {
		idempotencyStarted := time.Now()
		requestHash, err := invokeRequestHash(versionID, payload)
		if err != nil {
			state = "request_hash_failed"
			errMsg = err.Error()
			return nil, err
		}
		now := s.now()
		record := &domain.IdempotencyRecord{
			Scope:       "invoke",
			ProjectID:   version.ProjectID,
			Key:         idempotencyKey,
			RequestHash: requestHash,
			Resource:    "execution_job",
			Status:      domain.IdempotencyStatusPending,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := s.store.CreateIdempotencyRecord(ctx, record); err != nil {
			if errors.Is(err, store.ErrAlreadyExists) {
				idempotencyMs = time.Since(idempotencyStarted).Milliseconds()
				job, replayErr := s.replayExecution(ctx, version.ProjectID, idempotencyKey, requestHash)
				if replayErr != nil {
					state = "idempotency_replay_failed"
					errMsg = replayErr.Error()
					return nil, replayErr
				}
				jobID = job.ID
				state = "replayed"
				return job, nil
			}
			idempotencyMs = time.Since(idempotencyStarted).Milliseconds()
			state = "idempotency_create_failed"
			errMsg = err.Error()
			return nil, err
		}
		enqueueStarted := time.Now()
		job, err := s.enqueueExecution(ctx, version, payload)
		enqueueMs = time.Since(enqueueStarted).Milliseconds()
		idempotencyMs = time.Since(idempotencyStarted).Milliseconds()
		if err != nil {
			_ = s.store.DeleteIdempotencyRecord(ctx, record.Scope, record.ProjectID, record.Key)
			state = "enqueue_failed"
			errMsg = err.Error()
			return nil, err
		}
		jobID = job.ID
		record.ResourceID = job.ID
		record.Status = domain.IdempotencyStatusCompleted
		record.UpdatedAt = s.now()
		if err := s.store.UpdateIdempotencyRecord(ctx, record); err != nil {
			state = "idempotency_update_failed"
			errMsg = err.Error()
			return nil, err
		}
		state = "enqueued"
		return job, nil
	}
	enqueueStarted := time.Now()
	job, err := s.enqueueExecution(ctx, version, payload)
	enqueueMs = time.Since(enqueueStarted).Milliseconds()
	if err != nil {
		state = "enqueue_failed"
		errMsg = err.Error()
		return nil, err
	}
	jobID = job.ID
	state = "enqueued"
	return job, nil
}

func (s *Service) enqueueExecution(ctx context.Context, version *domain.FunctionVersion, payload []byte) (*domain.ExecutionJob, error) {
	if err := s.enforceExecutionQuota(ctx, version.ProjectID); err != nil {
		return nil, err
	}
	now := s.now()
	job := &domain.ExecutionJob{
		ID:                uuid.NewString(),
		FunctionVersionID: version.ID,
		ProjectID:         version.ProjectID,
		State:             domain.JobStateQueued,
		Payload:           append([]byte(nil), payload...),
		MaxRetries:        version.MaxRetries,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := s.store.PutExecutionJob(ctx, job); err != nil {
		return nil, err
	}
	s.wake()
	return job, nil
}

func (s *Service) enforceExecutionQuota(ctx context.Context, projectID string) error {
	if s.maxActiveExecutionJobsPerProject <= 0 {
		return nil
	}
	count, err := s.store.CountExecutionJobsByProjectStates(ctx, projectID, []domain.JobState{
		domain.JobStateQueued,
		domain.JobStateScheduling,
		domain.JobStateAssigned,
		domain.JobStateRunning,
		domain.JobStateRetrying,
	})
	if err != nil {
		return err
	}
	if count >= s.maxActiveExecutionJobsPerProject {
		return fmt.Errorf("%w: project %s already has %d active execution jobs (limit %d)", domain.ErrProjectExecutionQuota, projectID, count, s.maxActiveExecutionJobsPerProject)
	}
	return nil
}

func (s *Service) replayExecution(ctx context.Context, projectID, key, requestHash string) (*domain.ExecutionJob, error) {
	record, err := s.store.GetIdempotencyRecord(ctx, "invoke", projectID, key)
	if err != nil {
		return nil, err
	}
	if record.RequestHash != requestHash {
		return nil, domain.ErrIdempotencyConflict
	}
	if record.Status != domain.IdempotencyStatusCompleted || record.ResourceID == "" {
		return nil, domain.ErrIdempotencyInProgress
	}
	return s.store.GetExecutionJob(ctx, record.ResourceID)
}

func (s *Service) RetryExecution(ctx context.Context, jobID string) (*domain.ExecutionJob, error) {
	job, err := s.store.GetExecutionJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.AttemptCount > job.MaxRetries {
		return nil, fmt.Errorf("job %s exceeded retry budget", job.ID)
	}
	job.State = domain.JobStateRetrying
	job.Error = ""
	job.TargetRegion = ""
	job.UpdatedAt = s.now()
	if err := s.store.UpdateExecutionJob(ctx, job); err != nil {
		return nil, err
	}
	s.wake()
	return job, nil
}

func (s *Service) PrepareFunctionVersion(ctx context.Context, version *domain.FunctionVersion) error {
	if version == nil {
		return fmt.Errorf("function version is required")
	}
	var (
		errs      []string
		successes int
		attempted int
	)
	for _, region := range version.Regions {
		dispatcher, ok := s.dispatchers[region]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: missing dispatcher", region))
			continue
		}
		preparer, ok := dispatcher.(WarmPreparer)
		if !ok {
			continue
		}
		attempted++
		if err := preparer.PrepareFunctionWarm(ctx, version); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", region, err))
			continue
		}
		successes++
	}
	if successes > 0 {
		if len(errs) > 0 {
			slog.Warn("warm preparation partially deferred",
				"functionVersionID", version.ID,
				"successfulRegions", successes,
				"attemptedRegions", attempted,
				"errors", strings.Join(errs, "; "),
			)
		}
		return nil
	}
	if len(errs) > 0 {
		return fmt.Errorf("prepare function version %s: %s", version.ID, strings.Join(errs, "; "))
	}
	return nil
}

func (s *Service) ExecuteDirect(ctx context.Context, versionID string, payload []byte) (*domain.DirectExecutionResult, error) {
	version, err := s.store.GetFunctionVersion(ctx, versionID)
	if err != nil {
		return nil, err
	}
	if version.State != domain.FunctionStateReady {
		return nil, domain.ErrFunctionVersionNotReady
	}
	if !s.hasDirectDispatcher(version) {
		return nil, domain.ErrDirectInvokeUnavailable
	}

	artifactMeta, err := s.store.GetArtifact(ctx, version.ArtifactDigest)
	if err != nil {
		return nil, err
	}
	candidates, err := s.rankDispatchers(ctx, version)
	if err != nil {
		return nil, err
	}

	var (
		sawDirect      bool
		capacityErrors []string
		dispatchErrors []string
	)
	for _, candidate := range candidates {
		dispatcher, ok := candidate.dispatcher.(DirectDispatcher)
		if !ok {
			continue
		}
		sawDirect = true
		assignment := domain.Assignment{
			AttemptID:         "direct-attempt-" + uuid.NewString(),
			JobID:             "direct-job-" + uuid.NewString(),
			FunctionVersionID: version.ID,
			ArtifactDigest:    version.ArtifactDigest,
			ArtifactBundleKey: artifactMeta.BundleKey,
			Entrypoint:        version.Entrypoint,
			EnvRefs:           append([]string(nil), version.EnvRefs...),
			Payload:           append([]byte(nil), payload...),
			NetworkPolicy:     version.NetworkPolicy,
			TimeoutSec:        version.TimeoutSec,
			MemoryMB:          version.MemoryMB,
		}
		result, err := dispatcher.ExecuteDirect(ctx, assignment)
		if err == nil {
			return result, nil
		}
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil, err
		case errors.Is(err, domain.ErrNoExecutionCapacity):
			capacityErrors = append(capacityErrors, fmt.Sprintf("%s: %v", candidate.region, err))
		default:
			dispatchErrors = append(dispatchErrors, fmt.Sprintf("%s: %v", candidate.region, err))
		}
	}
	if !sawDirect {
		return nil, domain.ErrDirectInvokeUnavailable
	}
	if len(capacityErrors) > 0 && len(dispatchErrors) == 0 {
		return nil, fmt.Errorf("%w: %s", domain.ErrNoExecutionCapacity, strings.Join(capacityErrors, "; "))
	}
	if len(dispatchErrors) > 0 {
		return nil, fmt.Errorf("direct invoke failed: %s", strings.Join(dispatchErrors, "; "))
	}
	return nil, domain.ErrDirectInvokeUnavailable
}

func (s *Service) hasDirectDispatcher(version *domain.FunctionVersion) bool {
	if version == nil {
		return false
	}
	for _, region := range version.Regions {
		dispatcher, ok := s.dispatchers[region]
		if !ok {
			continue
		}
		if _, ok := dispatcher.(DirectDispatcher); ok {
			return true
		}
	}
	return false
}

func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		if _, err := s.recoverStaleScheduling(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("scheduler recovery failed", "err", err)
		}
		if err := s.schedulePending(ctx); err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, context.Canceled) {
			slog.Error("scheduler loop failed", "err", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-s.wakeCh:
		}
	}
}

func (s *Service) schedulePending(ctx context.Context) error {
	for {
		dispatched, err := s.scheduleNext(ctx)
		if err != nil {
			return err
		}
		if !dispatched {
			return nil
		}
	}
}

func (s *Service) scheduleNext(ctx context.Context) (bool, error) {
	job, err := s.store.ClaimNextExecutionJob(ctx,
		[]domain.JobState{domain.JobStateQueued, domain.JobStateRetrying},
		domain.JobStateScheduling,
		s.now(),
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}

	if job.AttemptCount > job.MaxRetries {
		job.State = domain.JobStateFailed
		job.Error = fmt.Sprintf("job %s exceeded retry budget", job.ID)
		job.UpdatedAt = s.now()
		if err := s.store.UpdateExecutionJob(ctx, job); err != nil {
			return false, err
		}
		return true, nil
	}

	version, err := s.store.GetFunctionVersion(ctx, job.FunctionVersionID)
	if err != nil {
		job.State = domain.JobStateFailed
		job.Error = err.Error()
		job.UpdatedAt = s.now()
		if updateErr := s.store.UpdateExecutionJob(ctx, job); updateErr != nil {
			return false, updateErr
		}
		return true, nil
	}

	if err := s.dispatchJobAttempt(ctx, version, job); err != nil {
		if errors.Is(err, domain.ErrNoExecutionCapacity) {
			return false, nil
		}
		return true, nil
	}
	return true, nil
}

func (s *Service) dispatchJobAttempt(ctx context.Context, version *domain.FunctionVersion, job *domain.ExecutionJob) error {
	now := s.now()
	started := time.Now()
	var (
		artifactLookupMs int64
		rankMs           int64
		persistAttemptMs int64
		enqueueMs        int64
		targetRegion     string
		state            = "started"
		errMsg           string
	)
	defer func() {
		slog.Info("scheduler attempt timing",
			"jobID", job.ID,
			"functionVersionID", version.ID,
			"targetRegion", targetRegion,
			"state", state,
			"error", errMsg,
			"artifactLookupMs", artifactLookupMs,
			"rankMs", rankMs,
			"persistAttemptMs", persistAttemptMs,
			"enqueueMs", enqueueMs,
			"totalMs", time.Since(started).Milliseconds(),
		)
	}()
	artifactLookupStarted := time.Now()
	artifactMeta, err := s.store.GetArtifact(ctx, version.ArtifactDigest)
	artifactLookupMs = time.Since(artifactLookupStarted).Milliseconds()
	if err != nil {
		job.State = domain.JobStateQueued
		job.TargetRegion = ""
		job.Error = err.Error()
		job.UpdatedAt = now
		_ = s.store.UpdateExecutionJob(ctx, job)
		state = "artifact_lookup_failed"
		errMsg = err.Error()
		return err
	}
	rankStarted := time.Now()
	candidates, err := s.rankDispatchers(ctx, version)
	rankMs = time.Since(rankStarted).Milliseconds()
	if err != nil {
		job.State = domain.JobStateQueued
		job.TargetRegion = ""
		job.Error = err.Error()
		job.UpdatedAt = now
		_ = s.store.UpdateExecutionJob(ctx, job)
		state = "rank_failed"
		errMsg = err.Error()
		return err
	}

	var dispatchErrors []string
	for _, candidate := range candidates {
		attempt := &domain.Attempt{
			ID:                uuid.NewString(),
			JobID:             job.ID,
			FunctionVersionID: version.ID,
			Region:            candidate.region,
			State:             domain.AttemptStateAssigned,
			LeaseExpiresAt:    now.Add(30 * time.Second),
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		job.LastAttemptID = attempt.ID
		job.TargetRegion = candidate.region
		targetRegion = candidate.region
		job.State = domain.JobStateAssigned
		job.Error = ""
		job.UpdatedAt = now

		persistStarted := time.Now()
		if err := s.store.PutAttempt(ctx, attempt); err != nil {
			persistAttemptMs = time.Since(persistStarted).Milliseconds()
			state = "put_attempt_failed"
			errMsg = err.Error()
			return err
		}
		if err := s.store.UpdateExecutionJob(ctx, job); err != nil {
			persistAttemptMs = time.Since(persistStarted).Milliseconds()
			state = "update_job_failed"
			errMsg = err.Error()
			return err
		}
		persistAttemptMs = time.Since(persistStarted).Milliseconds()

		assignment := domain.Assignment{
			AttemptID:         attempt.ID,
			JobID:             job.ID,
			FunctionVersionID: version.ID,
			ArtifactDigest:    version.ArtifactDigest,
			ArtifactBundleKey: artifactMeta.BundleKey,
			Entrypoint:        version.Entrypoint,
			EnvRefs:           append([]string(nil), version.EnvRefs...),
			Payload:           append([]byte(nil), job.Payload...),
			NetworkPolicy:     version.NetworkPolicy,
			TimeoutSec:        version.TimeoutSec,
			MemoryMB:          version.MemoryMB,
		}
		enqueueStarted := time.Now()
		if err := candidate.dispatcher.EnqueueExecution(ctx, assignment); err == nil {
			enqueueMs = time.Since(enqueueStarted).Milliseconds()
			job.AttemptCount++
			job.UpdatedAt = s.now()
			state = "assigned"
			return s.store.UpdateExecutionJob(ctx, job)
		} else {
			enqueueMs = time.Since(enqueueStarted).Milliseconds()
			if errors.Is(err, domain.ErrNoExecutionCapacity) {
				attempt.State = domain.AttemptStateFailed
				attempt.Error = err.Error()
				attempt.LeaseExpiresAt = s.now()
				attempt.UpdatedAt = s.now()
				_ = s.store.UpdateAttempt(ctx, attempt)
				job.State = domain.JobStateQueued
				job.TargetRegion = ""
				job.Error = ""
				job.UpdatedAt = s.now()
				_ = s.store.UpdateExecutionJob(ctx, job)
				state = "capacity_unavailable"
				errMsg = err.Error()
				return err
			}
			attempt.State = domain.AttemptStateFailed
			attempt.Error = err.Error()
			attempt.LeaseExpiresAt = s.now()
			attempt.UpdatedAt = s.now()
			_ = s.store.UpdateAttempt(ctx, attempt)
			dispatchErrors = append(dispatchErrors, fmt.Sprintf("%s: %v", candidate.region, err))
		}
	}

	job.State = domain.JobStateQueued
	job.TargetRegion = ""
	job.Error = strings.Join(dispatchErrors, "; ")
	job.UpdatedAt = s.now()
	_ = s.store.UpdateExecutionJob(ctx, job)
	state = "enqueue_failed"
	errMsg = job.Error
	return errors.New(job.Error)
}

func (s *Service) pickRegion(version *domain.FunctionVersion) (string, RegionDispatcher, error) {
	candidates, err := s.rankDispatchers(context.Background(), version)
	if err != nil {
		return "", nil, err
	}
	pick := candidates[0]
	return pick.region, pick.dispatcher, nil
}

func (s *Service) rankDispatchers(ctx context.Context, version *domain.FunctionVersion) ([]candidate, error) {
	artifactRegions, err := s.artifactRegionIndex(ctx, version.ArtifactDigest)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	candidates := make([]candidate, 0, len(version.Regions))
	for _, region := range version.Regions {
		dispatcher, ok := s.dispatchers[region]
		if !ok {
			continue
		}
		stats := dispatcher.Stats()
		backlog := 0
		healthy := true
		lastHeartbeat := time.Time{}
		if queued, err := s.store.CountActiveExecutionJobsByRegion(ctx, region); err == nil {
			backlog = queued
		} else {
			return nil, err
		}
		if snapshot, err := s.store.GetRegion(ctx, region); err == nil {
			stats = domain.RegionStats{
				AvailableHosts: snapshot.AvailableHosts,
				BlankWarm:      snapshot.BlankWarm,
				FunctionWarm:   snapshot.FunctionWarm,
			}
			lastHeartbeat = snapshot.LastHeartbeatAt
			healthy = regionHealthy(snapshot, s.now(), s.healthyWithin)
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		candidates = append(candidates, candidate{
			region:        region,
			dispatcher:    dispatcher,
			stats:         stats,
			backlog:       backlog,
			healthy:       healthy,
			artifactLocal: artifactRegions[region],
			lastHeartbeat: lastHeartbeat,
		})
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no dispatcher available for %v", version.Regions)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].healthy != candidates[j].healthy {
			return candidates[i].healthy
		}
		if candidates[i].artifactLocal != candidates[j].artifactLocal {
			return candidates[i].artifactLocal
		}
		if candidates[i].backlog != candidates[j].backlog {
			return candidates[i].backlog < candidates[j].backlog
		}
		if candidates[i].stats.FunctionWarm != candidates[j].stats.FunctionWarm {
			return candidates[i].stats.FunctionWarm > candidates[j].stats.FunctionWarm
		}
		if candidates[i].stats.BlankWarm != candidates[j].stats.BlankWarm {
			return candidates[i].stats.BlankWarm > candidates[j].stats.BlankWarm
		}
		if candidates[i].stats.AvailableHosts != candidates[j].stats.AvailableHosts {
			return candidates[i].stats.AvailableHosts > candidates[j].stats.AvailableHosts
		}
		if !candidates[i].lastHeartbeat.Equal(candidates[j].lastHeartbeat) {
			return candidates[i].lastHeartbeat.After(candidates[j].lastHeartbeat)
		}
		return candidates[i].region < candidates[j].region
	})
	return candidates, nil
}

func invokeRequestHash(versionID string, payload []byte) (string, error) {
	return idempotency.Hash(struct {
		VersionID string          `json:"versionId"`
		Payload   json.RawMessage `json:"payload"`
	}{
		VersionID: versionID,
		Payload:   idempotency.NormalizeJSON(payload),
	})
}

func (s *Service) wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

func (s *Service) recoverStaleScheduling(ctx context.Context) (int, error) {
	if s.schedulingTimeout <= 0 {
		return 0, nil
	}
	now := s.now()
	recovered, err := s.store.RequeueStaleExecutionJobs(
		ctx,
		domain.JobStateScheduling,
		domain.JobStateQueued,
		now.Add(-s.schedulingTimeout),
		now,
		"scheduling claim expired before dispatch",
	)
	if err != nil {
		return 0, err
	}
	if recovered > 0 {
		s.wake()
	}
	return recovered, nil
}

func (s *Service) artifactRegionIndex(ctx context.Context, digest string) (map[string]bool, error) {
	if digest == "" {
		return nil, nil
	}
	artifact, err := s.store.GetArtifact(ctx, digest)
	if err != nil {
		return nil, err
	}
	regions := make(map[string]bool, len(artifact.Regions))
	for region := range artifact.Regions {
		regions[region] = true
	}
	return regions, nil
}

func regionHealthy(region *domain.Region, now time.Time, maxHeartbeatAge time.Duration) bool {
	if region == nil {
		return true
	}
	if region.State != "active" {
		return false
	}
	if maxHeartbeatAge <= 0 || region.LastHeartbeatAt.IsZero() {
		return true
	}
	return now.Sub(region.LastHeartbeatAt) <= maxHeartbeatAge
}
