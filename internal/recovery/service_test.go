package recovery

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ishaan/eeeverc/internal/domain"
	memstore "github.com/ishaan/eeeverc/internal/store/memory"
)

type fakeRetryer struct {
	calls []string
	job   *domain.ExecutionJob
}

func (f *fakeRetryer) RetryExecution(_ context.Context, jobID string) (*domain.ExecutionJob, error) {
	f.calls = append(f.calls, jobID)
	return f.job, nil
}

func TestRecoveryRetriesExpiredAttempt(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	now := time.Now().UTC().Add(-time.Minute)
	job := &domain.ExecutionJob{
		ID:                uuid.NewString(),
		FunctionVersionID: "fn-1",
		ProjectID:         "demo",
		State:             domain.JobStateRunning,
		Payload:           json.RawMessage(`{"hello":"world"}`),
		MaxRetries:        2,
		AttemptCount:      1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	attempt := &domain.Attempt{
		ID:                uuid.NewString(),
		JobID:             job.ID,
		FunctionVersionID: "fn-1",
		Region:            "ap-south-1",
		State:             domain.AttemptStateRunning,
		LeaseExpiresAt:    now,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := store.PutExecutionJob(context.Background(), job); err != nil {
		t.Fatalf("put job: %v", err)
	}
	if err := store.PutAttempt(context.Background(), attempt); err != nil {
		t.Fatalf("put attempt: %v", err)
	}

	retryer := &fakeRetryer{job: job}
	svc := New(store, retryer, time.Second)
	svc.now = func() time.Time { return time.Now().UTC() }

	if err := svc.recoverExpiredAttempts(context.Background()); err != nil {
		t.Fatalf("recover expired attempts: %v", err)
	}
	if len(retryer.calls) != 1 || retryer.calls[0] != job.ID {
		t.Fatalf("expected one retry for job %s, got %#v", job.ID, retryer.calls)
	}
}
