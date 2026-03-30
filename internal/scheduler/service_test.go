package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ishaan/eeeverc/internal/domain"
	memstore "github.com/ishaan/eeeverc/internal/store/memory"
)

type fakeDispatcher struct {
	region string
	stats  domain.RegionStats
	err    error
}

func (f fakeDispatcher) Region() string                                            { return f.region }
func (f fakeDispatcher) Stats() domain.RegionStats                                 { return f.stats }
func (f fakeDispatcher) EnqueueExecution(context.Context, domain.Assignment) error { return f.err }

func TestDispatchPrefersWarmRegion(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	now := time.Now().UTC()
	if err := store.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-1",
		ProjectID:      "demo",
		Name:           "echo",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		TimeoutSec:     5,
		NetworkPolicy:  domain.NetworkPolicyFull,
		Regions:        []string{"ap-south-1", "ap-southeast-1"},
		ArtifactDigest: "digest",
		State:          domain.FunctionStateReady,
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("put function: %v", err)
	}

	svc := New(store, []RegionDispatcher{
		fakeDispatcher{region: "ap-south-1", stats: domain.RegionStats{AvailableHosts: 1, BlankWarm: 1, FunctionWarm: 0}},
		fakeDispatcher{region: "ap-southeast-1", stats: domain.RegionStats{AvailableHosts: 1, BlankWarm: 0, FunctionWarm: 3}},
	})
	job, err := svc.DispatchExecution(context.Background(), "fn-1", json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("dispatch execution: %v", err)
	}
	if job.TargetRegion != "ap-southeast-1" {
		t.Fatalf("expected warm region, got %s", job.TargetRegion)
	}
}

func TestDispatchFallsBackWhenTopRegionCannotAccept(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	now := time.Now().UTC()
	if err := store.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-2",
		ProjectID:      "demo",
		Name:           "echo",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		TimeoutSec:     5,
		NetworkPolicy:  domain.NetworkPolicyFull,
		Regions:        []string{"ap-south-1", "ap-southeast-1"},
		ArtifactDigest: "digest",
		State:          domain.FunctionStateReady,
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("put function: %v", err)
	}

	svc := New(store, []RegionDispatcher{
		fakeDispatcher{
			region: "ap-south-1",
			stats:  domain.RegionStats{AvailableHosts: 1, BlankWarm: 1, FunctionWarm: 10},
			err:    errors.New("no active hosts available in region ap-south-1"),
		},
		fakeDispatcher{
			region: "ap-southeast-1",
			stats:  domain.RegionStats{AvailableHosts: 1, BlankWarm: 1, FunctionWarm: 0},
		},
	})
	job, err := svc.DispatchExecution(context.Background(), "fn-2", json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("dispatch execution: %v", err)
	}
	if job.TargetRegion != "ap-southeast-1" {
		t.Fatalf("expected fallback region, got %s", job.TargetRegion)
	}
	if job.AttemptCount != 1 {
		t.Fatalf("expected one successful dispatched attempt, got %d", job.AttemptCount)
	}
}

func TestDispatchExecutionIdempotentReplaysExistingJob(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	now := time.Now().UTC()
	if err := store.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-3",
		ProjectID:      "demo",
		Name:           "echo",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		TimeoutSec:     5,
		NetworkPolicy:  domain.NetworkPolicyFull,
		Regions:        []string{"ap-south-1"},
		ArtifactDigest: "digest",
		State:          domain.FunctionStateReady,
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("put function: %v", err)
	}

	svc := New(store, []RegionDispatcher{
		fakeDispatcher{region: "ap-south-1", stats: domain.RegionStats{AvailableHosts: 1, BlankWarm: 1}},
	})
	first, err := svc.DispatchExecutionIdempotent(context.Background(), "fn-3", json.RawMessage(`{"msg":"hi"}`), "invoke-key-1")
	if err != nil {
		t.Fatalf("dispatch execution: %v", err)
	}
	second, err := svc.DispatchExecutionIdempotent(context.Background(), "fn-3", json.RawMessage(`{"msg":"hi"}`), "invoke-key-1")
	if err != nil {
		t.Fatalf("replay execution: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected replayed job id %s, got %s", first.ID, second.ID)
	}
}

func TestDispatchExecutionIdempotentRejectsDifferentPayload(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	now := time.Now().UTC()
	if err := store.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-4",
		ProjectID:      "demo",
		Name:           "echo",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		TimeoutSec:     5,
		NetworkPolicy:  domain.NetworkPolicyFull,
		Regions:        []string{"ap-south-1"},
		ArtifactDigest: "digest",
		State:          domain.FunctionStateReady,
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("put function: %v", err)
	}

	svc := New(store, []RegionDispatcher{
		fakeDispatcher{region: "ap-south-1", stats: domain.RegionStats{AvailableHosts: 1, BlankWarm: 1}},
	})
	if _, err := svc.DispatchExecutionIdempotent(context.Background(), "fn-4", json.RawMessage(`{"msg":"hi"}`), "invoke-key-2"); err != nil {
		t.Fatalf("dispatch execution: %v", err)
	}
	_, err := svc.DispatchExecutionIdempotent(context.Background(), "fn-4", json.RawMessage(`{"msg":"different"}`), "invoke-key-2")
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
}
