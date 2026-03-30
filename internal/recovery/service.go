package recovery

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/store"
)

type Retryer interface {
	RetryExecution(ctx context.Context, jobID string) (*domain.ExecutionJob, error)
}

type Service struct {
	store    store.Store
	retryer  Retryer
	interval time.Duration
	now      func() time.Time
}

func New(store store.Store, retryer Retryer, interval time.Duration) *Service {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Service{
		store:    store,
		retryer:  retryer,
		interval: interval,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.recoverExpiredAttempts(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("lease recovery failed", "err", err)
			}
		}
	}
}

func (s *Service) recoverExpiredAttempts(ctx context.Context) error {
	expired, err := s.store.ListExpiredAttempts(ctx, s.now())
	if err != nil {
		return err
	}
	for _, attempt := range expired {
		if attempt.State.Terminal() {
			continue
		}
		if err := s.recoverAttempt(ctx, attempt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) recoverAttempt(ctx context.Context, attempt domain.Attempt) error {
	job, err := s.store.GetExecutionJob(ctx, attempt.JobID)
	if err != nil {
		return err
	}

	latestAttempt, err := s.store.GetAttempt(ctx, attempt.ID)
	if err != nil {
		return err
	}
	if latestAttempt.State.Terminal() || latestAttempt.LeaseExpiresAt.After(s.now()) {
		return nil
	}

	now := s.now()
	latestAttempt.State = domain.AttemptStateFailed
	latestAttempt.Error = "lease expired before completion"
	latestAttempt.LeaseExpiresAt = now
	latestAttempt.UpdatedAt = now
	if err := s.store.UpdateAttempt(ctx, latestAttempt); err != nil {
		return err
	}

	if job.AttemptCount <= job.MaxRetries {
		job.State = domain.JobStateRetrying
		job.Error = latestAttempt.Error
		job.UpdatedAt = now
		if err := s.store.UpdateExecutionJob(ctx, job); err != nil {
			return err
		}
		if _, err := s.retryer.RetryExecution(ctx, job.ID); err != nil {
			job.State = domain.JobStateFailed
			job.Error = err.Error()
			job.UpdatedAt = s.now()
			return s.store.UpdateExecutionJob(ctx, job)
		}
		return nil
	}

	job.State = domain.JobStateFailed
	job.Error = latestAttempt.Error
	job.UpdatedAt = now
	return s.store.UpdateExecutionJob(ctx, job)
}
