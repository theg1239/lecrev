package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type Service struct {
	store       store.Store
	dispatchers map[string]RegionDispatcher
	now         func() time.Time
}

type candidate struct {
	region     string
	dispatcher RegionDispatcher
	stats      domain.RegionStats
}

func New(store store.Store, dispatchers []RegionDispatcher) *Service {
	index := make(map[string]RegionDispatcher, len(dispatchers))
	for _, dispatcher := range dispatchers {
		index[dispatcher.Region()] = dispatcher
	}
	return &Service{
		store:       store,
		dispatchers: index,
		now:         func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) DispatchExecution(ctx context.Context, versionID string, payload []byte) (*domain.ExecutionJob, error) {
	return s.DispatchExecutionIdempotent(ctx, versionID, payload, "")
}

func (s *Service) DispatchExecutionIdempotent(ctx context.Context, versionID string, payload []byte, idempotencyKey string) (*domain.ExecutionJob, error) {
	version, err := s.store.GetFunctionVersion(ctx, versionID)
	if err != nil {
		return nil, err
	}
	if idempotencyKey != "" {
		requestHash, err := invokeRequestHash(versionID, payload)
		if err != nil {
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
				return s.replayExecution(ctx, version.ProjectID, idempotencyKey, requestHash)
			}
			return nil, err
		}
		job, err := s.dispatchExecution(ctx, version, payload)
		if err != nil {
			_ = s.store.DeleteIdempotencyRecord(ctx, record.Scope, record.ProjectID, record.Key)
			return nil, err
		}
		record.ResourceID = job.ID
		record.Status = domain.IdempotencyStatusCompleted
		record.UpdatedAt = s.now()
		if err := s.store.UpdateIdempotencyRecord(ctx, record); err != nil {
			return nil, err
		}
		return job, nil
	}
	return s.dispatchExecution(ctx, version, payload)
}

func (s *Service) dispatchExecution(ctx context.Context, version *domain.FunctionVersion, payload []byte) (*domain.ExecutionJob, error) {
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
	if err := s.dispatchJobAttempt(ctx, version, job); err != nil {
		return nil, err
	}
	return job, nil
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
	version, err := s.store.GetFunctionVersion(ctx, job.FunctionVersionID)
	if err != nil {
		return nil, err
	}
	if job.AttemptCount > job.MaxRetries {
		return nil, fmt.Errorf("job %s exceeded retry budget", job.ID)
	}
	if err := s.dispatchJobAttempt(ctx, version, job); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Service) dispatchJobAttempt(ctx context.Context, version *domain.FunctionVersion, job *domain.ExecutionJob) error {
	now := s.now()
	candidates, err := s.rankDispatchers(version)
	if err != nil {
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
		job.State = domain.JobStateAssigned
		job.Error = ""
		job.UpdatedAt = now

		if err := s.store.PutAttempt(ctx, attempt); err != nil {
			return err
		}
		if err := s.store.UpdateExecutionJob(ctx, job); err != nil {
			return err
		}

		assignment := domain.Assignment{
			AttemptID:         attempt.ID,
			JobID:             job.ID,
			FunctionVersionID: version.ID,
			ArtifactDigest:    version.ArtifactDigest,
			Entrypoint:        version.Entrypoint,
			EnvRefs:           append([]string(nil), version.EnvRefs...),
			Payload:           append([]byte(nil), job.Payload...),
			NetworkPolicy:     version.NetworkPolicy,
			TimeoutSec:        version.TimeoutSec,
		}
		if err := candidate.dispatcher.EnqueueExecution(ctx, assignment); err == nil {
			job.AttemptCount++
			job.UpdatedAt = s.now()
			return s.store.UpdateExecutionJob(ctx, job)
		} else {
			attempt.State = domain.AttemptStateFailed
			attempt.Error = err.Error()
			attempt.LeaseExpiresAt = s.now()
			attempt.UpdatedAt = s.now()
			_ = s.store.UpdateAttempt(ctx, attempt)
			dispatchErrors = append(dispatchErrors, fmt.Sprintf("%s: %v", candidate.region, err))
		}
	}

	job.State = domain.JobStateQueued
	job.Error = strings.Join(dispatchErrors, "; ")
	job.UpdatedAt = s.now()
	_ = s.store.UpdateExecutionJob(ctx, job)
	return errors.New(job.Error)
}

func (s *Service) pickRegion(version *domain.FunctionVersion) (string, RegionDispatcher, error) {
	candidates, err := s.rankDispatchers(version)
	if err != nil {
		return "", nil, err
	}
	pick := candidates[0]
	return pick.region, pick.dispatcher, nil
}

func (s *Service) rankDispatchers(version *domain.FunctionVersion) ([]candidate, error) {
	candidates := make([]candidate, 0, len(version.Regions))
	for _, region := range version.Regions {
		dispatcher, ok := s.dispatchers[region]
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{
			region:     region,
			dispatcher: dispatcher,
			stats:      dispatcher.Stats(),
		})
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no dispatcher available for %v", version.Regions)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].stats.FunctionWarm != candidates[j].stats.FunctionWarm {
			return candidates[i].stats.FunctionWarm > candidates[j].stats.FunctionWarm
		}
		if candidates[i].stats.BlankWarm != candidates[j].stats.BlankWarm {
			return candidates[i].stats.BlankWarm > candidates[j].stats.BlankWarm
		}
		return candidates[i].stats.AvailableHosts > candidates[j].stats.AvailableHosts
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
