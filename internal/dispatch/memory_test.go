package dispatch

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ishaan/eeeverc/internal/domain"
)

func TestMemoryBusPublishAndConsume(t *testing.T) {
	t.Parallel()

	bus := NewMemoryBus(8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = bus.ConsumeExecution(ctx, "ap-south-1", "consumer-a", func(_ context.Context, assignment domain.Assignment) error {
			if assignment.JobID != "job-1" {
				t.Errorf("unexpected job id: %s", assignment.JobID)
			}
			close(done)
			return nil
		})
	}()

	if err := bus.PublishExecution(ctx, "ap-south-1", domain.Assignment{JobID: "job-1"}); err != nil {
		t.Fatalf("publish execution: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for assignment delivery")
	}
}

func TestMemoryBusRequeuesOnHandlerError(t *testing.T) {
	t.Parallel()

	bus := NewMemoryBus(8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts atomic.Int32
	done := make(chan struct{})
	go func() {
		_ = bus.ConsumeExecution(ctx, "ap-south-1", "consumer-a", func(_ context.Context, assignment domain.Assignment) error {
			if assignment.JobID != "job-1" {
				t.Errorf("unexpected job id: %s", assignment.JobID)
			}
			if attempts.Add(1) == 1 {
				return context.DeadlineExceeded
			}
			close(done)
			return nil
		})
	}()

	if err := bus.PublishExecution(ctx, "ap-south-1", domain.Assignment{JobID: "job-1"}); err != nil {
		t.Fatalf("publish execution: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for requeued assignment")
	}
}
