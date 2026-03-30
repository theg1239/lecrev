package store

import (
	"context"
	"time"

	"github.com/theg1239/lecrev/internal/domain"
)

type Store interface {
	EnsureProject(ctx context.Context, projectID, tenantID, name string) (*domain.Project, error)
	GetProject(ctx context.Context, projectID string) (*domain.Project, error)
	PutAPIKey(ctx context.Context, key *domain.APIKey) error
	GetAPIKeyByHash(ctx context.Context, keyHash string) (*domain.APIKey, error)
	TouchAPIKeyLastUsed(ctx context.Context, keyHash string, usedAt time.Time) error
	CreateIdempotencyRecord(ctx context.Context, record *domain.IdempotencyRecord) error
	UpdateIdempotencyRecord(ctx context.Context, record *domain.IdempotencyRecord) error
	DeleteIdempotencyRecord(ctx context.Context, scope, projectID, key string) error
	GetIdempotencyRecord(ctx context.Context, scope, projectID, key string) (*domain.IdempotencyRecord, error)
	PutArtifact(ctx context.Context, artifact *domain.Artifact) error
	GetArtifact(ctx context.Context, digest string) (*domain.Artifact, error)
	PutFunctionVersion(ctx context.Context, version *domain.FunctionVersion) error
	GetFunctionVersion(ctx context.Context, versionID string) (*domain.FunctionVersion, error)
	PutBuildJob(ctx context.Context, job *domain.BuildJob) error
	GetBuildJob(ctx context.Context, jobID string) (*domain.BuildJob, error)
	CountBuildJobsByProjectStates(ctx context.Context, projectID string, states []string) (int, error)
	PutExecutionJob(ctx context.Context, job *domain.ExecutionJob) error
	UpdateExecutionJob(ctx context.Context, job *domain.ExecutionJob) error
	GetExecutionJob(ctx context.Context, jobID string) (*domain.ExecutionJob, error)
	CountExecutionJobsByProjectStates(ctx context.Context, projectID string, states []domain.JobState) (int, error)
	ClaimNextExecutionJob(ctx context.Context, fromStates []domain.JobState, toState domain.JobState, now time.Time) (*domain.ExecutionJob, error)
	RequeueStaleExecutionJobs(ctx context.Context, fromState, toState domain.JobState, staleBefore, now time.Time, errorMessage string) (int, error)
	CountActiveExecutionJobsByRegion(ctx context.Context, region string) (int, error)
	PutAttempt(ctx context.Context, attempt *domain.Attempt) error
	UpdateAttempt(ctx context.Context, attempt *domain.Attempt) error
	GetAttempt(ctx context.Context, attemptID string) (*domain.Attempt, error)
	ListAttemptsByJob(ctx context.Context, jobID string) ([]domain.Attempt, error)
	PutCostRecord(ctx context.Context, record *domain.CostRecord) error
	ListCostRecordsByJob(ctx context.Context, jobID string) ([]domain.CostRecord, error)
	ListExpiredAttempts(ctx context.Context, before time.Time) ([]domain.Attempt, error)
	PutHost(ctx context.Context, host *domain.Host) error
	UpdateHost(ctx context.Context, host *domain.Host) error
	GetHost(ctx context.Context, hostID string) (*domain.Host, error)
	ListHostsByRegion(ctx context.Context, region string) ([]domain.Host, error)
	ReplaceWarmPoolsForHost(ctx context.Context, region, hostID string, pools []domain.WarmPool) error
	ListWarmPoolsByRegion(ctx context.Context, region string) ([]domain.WarmPool, error)
	PutWebhookTrigger(ctx context.Context, trigger *domain.WebhookTrigger) error
	GetWebhookTrigger(ctx context.Context, token string) (*domain.WebhookTrigger, error)
	ListWebhookTriggersByFunctionVersion(ctx context.Context, versionID string) ([]domain.WebhookTrigger, error)
	PutRegion(ctx context.Context, region *domain.Region) error
	GetRegion(ctx context.Context, region string) (*domain.Region, error)
	ListRegions(ctx context.Context) ([]domain.Region, error)
}
