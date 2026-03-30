package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/theg1239/lecrev/internal/domain"
	memstore "github.com/theg1239/lecrev/internal/store/memory"
)

type fakeDispatcher struct {
	region      string
	stats       domain.RegionStats
	err         error
	assignments []domain.Assignment
}

func (f *fakeDispatcher) Region() string            { return f.region }
func (f *fakeDispatcher) Stats() domain.RegionStats { return f.stats }
func (f *fakeDispatcher) EnqueueExecution(_ context.Context, assignment domain.Assignment) error {
	if f.err != nil {
		return f.err
	}
	f.assignments = append(f.assignments, assignment)
	return nil
}

func TestDispatchExecutionQueuesJob(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	seedFunctionVersion(t, store, "fn-1", []string{"ap-south-1"})

	svc := New(store, []RegionDispatcher{
		&fakeDispatcher{region: "ap-south-1", stats: domain.RegionStats{AvailableHosts: 1, BlankWarm: 1}},
	})

	job, err := svc.DispatchExecution(context.Background(), "fn-1", json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("dispatch execution: %v", err)
	}
	if job.State != domain.JobStateQueued {
		t.Fatalf("expected queued job, got %s", job.State)
	}
	if job.TargetRegion != "" {
		t.Fatalf("expected no region before scheduling, got %s", job.TargetRegion)
	}
	if job.AttemptCount != 0 {
		t.Fatalf("expected no attempts before scheduling, got %d", job.AttemptCount)
	}
}

func TestScheduleNextPrefersWarmRegion(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	seedFunctionVersion(t, store, "fn-2", []string{"ap-south-1", "ap-southeast-1"})

	coldRegion := &fakeDispatcher{
		region: "ap-south-1",
		stats:  domain.RegionStats{AvailableHosts: 1, BlankWarm: 1},
	}
	warmRegion := &fakeDispatcher{
		region: "ap-southeast-1",
		stats:  domain.RegionStats{AvailableHosts: 1, FunctionWarm: 3},
	}
	svc := New(store, []RegionDispatcher{coldRegion, warmRegion})

	job, err := svc.DispatchExecution(context.Background(), "fn-2", json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("queue execution: %v", err)
	}

	dispatched, err := svc.scheduleNext(context.Background())
	if err != nil {
		t.Fatalf("schedule next: %v", err)
	}
	if !dispatched {
		t.Fatal("expected one queued job to be dispatched")
	}

	job, err = store.GetExecutionJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.TargetRegion != "ap-southeast-1" {
		t.Fatalf("expected warm region, got %s", job.TargetRegion)
	}
	if job.State != domain.JobStateAssigned {
		t.Fatalf("expected assigned state after scheduling, got %s", job.State)
	}
	if job.AttemptCount != 1 {
		t.Fatalf("expected one dispatched attempt, got %d", job.AttemptCount)
	}
	if len(warmRegion.assignments) != 1 {
		t.Fatalf("expected warm region assignment, got %d", len(warmRegion.assignments))
	}
	if len(coldRegion.assignments) != 0 {
		t.Fatalf("expected cold region to remain unused, got %d assignments", len(coldRegion.assignments))
	}
}

func TestScheduleNextFallsBackWhenTopRegionCannotAccept(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	seedFunctionVersion(t, store, "fn-3", []string{"ap-south-1", "ap-southeast-1"})

	unavailable := &fakeDispatcher{
		region: "ap-south-1",
		stats:  domain.RegionStats{AvailableHosts: 1, BlankWarm: 1, FunctionWarm: 10},
		err:    errors.New("no active hosts available in region ap-south-1"),
	}
	healthy := &fakeDispatcher{
		region: "ap-southeast-1",
		stats:  domain.RegionStats{AvailableHosts: 1, BlankWarm: 1},
	}
	svc := New(store, []RegionDispatcher{unavailable, healthy})

	job, err := svc.DispatchExecution(context.Background(), "fn-3", json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("queue execution: %v", err)
	}

	dispatched, err := svc.scheduleNext(context.Background())
	if err != nil {
		t.Fatalf("schedule next: %v", err)
	}
	if !dispatched {
		t.Fatal("expected fallback scheduling to dispatch one job")
	}

	job, err = store.GetExecutionJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.TargetRegion != "ap-southeast-1" {
		t.Fatalf("expected fallback region, got %s", job.TargetRegion)
	}
	if job.AttemptCount != 1 {
		t.Fatalf("expected one successful dispatched attempt, got %d", job.AttemptCount)
	}
	if len(healthy.assignments) != 1 {
		t.Fatalf("expected healthy region to receive one assignment, got %d", len(healthy.assignments))
	}
}

func TestDispatchExecutionIdempotentReplaysExistingJob(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	seedFunctionVersion(t, store, "fn-4", []string{"ap-south-1"})

	svc := New(store, []RegionDispatcher{
		&fakeDispatcher{region: "ap-south-1", stats: domain.RegionStats{AvailableHosts: 1, BlankWarm: 1}},
	})

	first, err := svc.DispatchExecutionIdempotent(context.Background(), "fn-4", json.RawMessage(`{"msg":"hi"}`), "invoke-key-1")
	if err != nil {
		t.Fatalf("dispatch execution: %v", err)
	}
	second, err := svc.DispatchExecutionIdempotent(context.Background(), "fn-4", json.RawMessage(`{"msg":"hi"}`), "invoke-key-1")
	if err != nil {
		t.Fatalf("replay execution: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected replayed job id %s, got %s", first.ID, second.ID)
	}
	if second.State != domain.JobStateQueued {
		t.Fatalf("expected replayed queued job, got %s", second.State)
	}
}

func TestDispatchExecutionIdempotentRejectsDifferentPayload(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	seedFunctionVersion(t, store, "fn-5", []string{"ap-south-1"})

	svc := New(store, []RegionDispatcher{
		&fakeDispatcher{region: "ap-south-1", stats: domain.RegionStats{AvailableHosts: 1, BlankWarm: 1}},
	})

	if _, err := svc.DispatchExecutionIdempotent(context.Background(), "fn-5", json.RawMessage(`{"msg":"hi"}`), "invoke-key-2"); err != nil {
		t.Fatalf("dispatch execution: %v", err)
	}
	_, err := svc.DispatchExecutionIdempotent(context.Background(), "fn-5", json.RawMessage(`{"msg":"different"}`), "invoke-key-2")
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
}

func seedFunctionVersion(t *testing.T, store *memstore.Store, versionID string, regions []string) {
	t.Helper()

	now := time.Now().UTC()
	if err := store.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             versionID,
		ProjectID:      "demo",
		Name:           "echo",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		TimeoutSec:     5,
		NetworkPolicy:  domain.NetworkPolicyFull,
		Regions:        append([]string(nil), regions...),
		ArtifactDigest: "digest",
		State:          domain.FunctionStateReady,
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("put function version: %v", err)
	}
}
