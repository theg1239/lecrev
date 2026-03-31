package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/firecracker"
	memstore "github.com/theg1239/lecrev/internal/store/memory"
)

type fakeDispatcher struct {
	region      string
	stats       domain.RegionStats
	err         error
	warmErr     error
	assignments []domain.Assignment
	warmups     []string
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

type directFakeDispatcher struct {
	*fakeDispatcher
	directErr    error
	directResult *domain.DirectExecutionResult
}

func (f *directFakeDispatcher) ExecuteDirect(_ context.Context, assignment domain.Assignment) (*domain.DirectExecutionResult, error) {
	if f.directErr != nil {
		return nil, f.directErr
	}
	f.assignments = append(f.assignments, assignment)
	if f.directResult != nil {
		return f.directResult, nil
	}
	return &domain.DirectExecutionResult{
		JobID:     assignment.JobID,
		AttemptID: assignment.AttemptID,
		State:     domain.JobStateSucceeded,
		Result: &domain.JobResult{
			ExitCode:   0,
			Output:     json.RawMessage(`{"ok":true}`),
			HostID:     "host-" + f.region,
			Region:     f.region,
			StartedAt:  time.Now().UTC(),
			FinishedAt: time.Now().UTC(),
			LatencyMs:  1,
		},
	}, nil
}

func (f *directFakeDispatcher) ExecuteDirectStream(ctx context.Context, assignment domain.Assignment, onMeta func(string, string, domain.StartMode), _ func(firecracker.HTTPStreamEvent) error) (*domain.DirectExecutionResult, error) {
	if onMeta != nil {
		onMeta(assignment.JobID, assignment.AttemptID, domain.StartModeFunctionWarm)
	}
	return f.ExecuteDirect(ctx, assignment)
}

func (f *fakeDispatcher) PrepareFunctionWarm(_ context.Context, version *domain.FunctionVersion) error {
	if f.warmErr != nil {
		return f.warmErr
	}
	f.warmups = append(f.warmups, version.ID)
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

func TestExecuteDirectPrefersWarmRegion(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	seedFunctionVersion(t, store, "fn-direct", []string{"ap-south-1", "ap-southeast-1"})

	coldRegion := &directFakeDispatcher{fakeDispatcher: &fakeDispatcher{
		region: "ap-south-1",
		stats:  domain.RegionStats{AvailableHosts: 1, BlankWarm: 1},
	}}
	warmRegion := &directFakeDispatcher{fakeDispatcher: &fakeDispatcher{
		region: "ap-southeast-1",
		stats:  domain.RegionStats{AvailableHosts: 1, FunctionWarm: 2},
	}}
	svc := New(store, []RegionDispatcher{coldRegion, warmRegion})

	result, err := svc.ExecuteDirect(context.Background(), "fn-direct", json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("execute direct: %v", err)
	}
	if result == nil || result.Result == nil {
		t.Fatalf("expected direct execution result, got %+v", result)
	}
	if result.Result.Region != "ap-southeast-1" {
		t.Fatalf("expected warm region result, got %s", result.Result.Region)
	}
	if len(warmRegion.assignments) != 1 {
		t.Fatalf("expected warm region to receive direct assignment, got %d", len(warmRegion.assignments))
	}
	if len(coldRegion.assignments) != 0 {
		t.Fatalf("expected cold region to remain unused, got %d", len(coldRegion.assignments))
	}
}

func TestDispatchExecutionEnforcesActiveExecutionQuota(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	seedFunctionVersion(t, store, "fn-quota", []string{"ap-south-1"})

	svc := New(store, []RegionDispatcher{
		&fakeDispatcher{region: "ap-south-1", stats: domain.RegionStats{AvailableHosts: 1, BlankWarm: 1}},
	})
	svc.maxActiveExecutionJobsPerProject = 1

	now := time.Now().UTC()
	if err := store.PutExecutionJob(context.Background(), &domain.ExecutionJob{
		ID:                "job-existing",
		FunctionVersionID: "fn-quota",
		ProjectID:         "demo",
		State:             domain.JobStateQueued,
		Payload:           json.RawMessage(`{"existing":true}`),
		MaxRetries:        1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("put existing execution job: %v", err)
	}

	_, err := svc.DispatchExecution(context.Background(), "fn-quota", json.RawMessage(`{"msg":"hi"}`))
	if !errors.Is(err, domain.ErrProjectExecutionQuota) {
		t.Fatalf("expected execution quota error, got %v", err)
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

func TestScheduleNextStopsWhenCapacityIsTemporarilyExhausted(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	seedFunctionVersion(t, store, "fn-capacity", []string{"ap-south-1"})

	dispatcher := &fakeDispatcher{
		region: "ap-south-1",
		stats:  domain.RegionStats{AvailableHosts: 1, BlankWarm: 1},
		err:    domain.ErrNoExecutionCapacity,
	}
	svc := New(store, []RegionDispatcher{dispatcher})

	job, err := svc.DispatchExecution(context.Background(), "fn-capacity", json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("queue execution: %v", err)
	}

	dispatched, err := svc.scheduleNext(context.Background())
	if err != nil {
		t.Fatalf("schedule next: %v", err)
	}
	if dispatched {
		t.Fatal("expected scheduler to stop when capacity is temporarily unavailable")
	}

	job, err = store.GetExecutionJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State != domain.JobStateQueued {
		t.Fatalf("expected queued job after capacity miss, got %s", job.State)
	}
	if job.AttemptCount != 0 {
		t.Fatalf("expected no counted attempts after capacity miss, got %d", job.AttemptCount)
	}
}

func TestScheduleNextPrefersHealthyRegionOverStaleWarmerRegion(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	seedFunctionVersion(t, store, "fn-healthy", []string{"ap-south-1", "ap-southeast-1"})
	now := time.Now().UTC()
	seedRegion(t, store, &domain.Region{
		Name:            "ap-south-1",
		State:           "active",
		AvailableHosts:  1,
		BlankWarm:       2,
		FunctionWarm:    10,
		LastHeartbeatAt: now.Add(-time.Minute),
	})
	seedRegion(t, store, &domain.Region{
		Name:            "ap-southeast-1",
		State:           "active",
		AvailableHosts:  1,
		BlankWarm:       0,
		FunctionWarm:    0,
		LastHeartbeatAt: now,
	})

	staleWarm := &fakeDispatcher{
		region: "ap-south-1",
		stats:  domain.RegionStats{AvailableHosts: 1, BlankWarm: 2, FunctionWarm: 10},
	}
	healthy := &fakeDispatcher{
		region: "ap-southeast-1",
		stats:  domain.RegionStats{AvailableHosts: 1},
	}
	svc := New(store, []RegionDispatcher{staleWarm, healthy})
	svc.now = func() time.Time { return now }
	svc.healthyWithin = 15 * time.Second

	job, err := svc.DispatchExecution(context.Background(), "fn-healthy", json.RawMessage(`{"msg":"hi"}`))
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
		t.Fatalf("expected healthy region, got %s", job.TargetRegion)
	}
}

func TestScheduleNextPrefersArtifactLocalRegion(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	seedFunctionVersion(t, store, "fn-artifact", []string{"ap-south-1", "ap-southeast-1"})
	seedArtifact(t, store, &domain.Artifact{
		Digest:     "digest",
		SizeBytes:  128,
		BundleKey:  "artifacts/digest/bundle.tgz",
		StartupKey: "artifacts/digest/startup.json",
		Regions: map[string]time.Time{
			"ap-south-1": time.Now().UTC(),
		},
		CreatedAt: time.Now().UTC(),
	})

	remote := &fakeDispatcher{
		region: "ap-southeast-1",
		stats:  domain.RegionStats{AvailableHosts: 1, BlankWarm: 1},
	}
	local := &fakeDispatcher{
		region: "ap-south-1",
		stats:  domain.RegionStats{AvailableHosts: 1, BlankWarm: 1},
	}
	svc := New(store, []RegionDispatcher{remote, local})

	job, err := svc.DispatchExecution(context.Background(), "fn-artifact", json.RawMessage(`{"msg":"hi"}`))
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
	if job.TargetRegion != "ap-south-1" {
		t.Fatalf("expected artifact-local region, got %s", job.TargetRegion)
	}
}

func TestPrepareFunctionVersionTargetsConfiguredRegions(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	version := seedFunctionVersion(t, store, "fn-warm", []string{"ap-south-1", "ap-southeast-1"})
	version.NetworkPolicy = domain.NetworkPolicyNone
	if err := store.PutFunctionVersion(context.Background(), version); err != nil {
		t.Fatalf("update function version: %v", err)
	}
	first := &fakeDispatcher{region: "ap-south-1"}
	second := &fakeDispatcher{region: "ap-southeast-1"}
	svc := New(store, []RegionDispatcher{first, second})

	if err := svc.PrepareFunctionVersion(context.Background(), version); err != nil {
		t.Fatalf("prepare function version: %v", err)
	}
	if len(first.warmups) != 1 || first.warmups[0] != version.ID {
		t.Fatalf("expected first region warm prep for %s, got %+v", version.ID, first.warmups)
	}
	if len(second.warmups) != 1 || second.warmups[0] != version.ID {
		t.Fatalf("expected second region warm prep for %s, got %+v", version.ID, second.warmups)
	}
}

func TestPrepareFunctionVersionIncludesFullNetworkWarmPrep(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	version := seedFunctionVersion(t, store, "fn-warm-full", []string{"ap-south-1"})
	dispatcher := &fakeDispatcher{region: "ap-south-1"}
	svc := New(store, []RegionDispatcher{dispatcher})

	if err := svc.PrepareFunctionVersion(context.Background(), version); err != nil {
		t.Fatalf("prepare function version: %v", err)
	}
	if len(dispatcher.warmups) != 1 || dispatcher.warmups[0] != version.ID {
		t.Fatalf("expected warm prep for full-network version %s, got %+v", version.ID, dispatcher.warmups)
	}
}

func TestPrepareFunctionVersionSucceedsWhenOneRegionWarms(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	version := seedFunctionVersion(t, store, "fn-warm-partial", []string{"ap-south-1", "ap-south-2"})
	first := &fakeDispatcher{region: "ap-south-1"}
	second := &fakeDispatcher{region: "ap-south-2", warmErr: domain.ErrNoExecutionCapacity}
	svc := New(store, []RegionDispatcher{first, second})

	if err := svc.PrepareFunctionVersion(context.Background(), version); err != nil {
		t.Fatalf("prepare function version: %v", err)
	}
	if len(first.warmups) != 1 || first.warmups[0] != version.ID {
		t.Fatalf("expected successful warm prep in first region, got %+v", first.warmups)
	}
	if len(second.warmups) != 0 {
		t.Fatalf("expected no warm prep success in second region, got %+v", second.warmups)
	}
}

func TestRecoverStaleSchedulingRequeuesJob(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	seedFunctionVersion(t, store, "fn-stale", []string{"ap-south-1"})
	now := time.Now().UTC()
	job := &domain.ExecutionJob{
		ID:                "job-stale",
		FunctionVersionID: "fn-stale",
		ProjectID:         "demo",
		State:             domain.JobStateScheduling,
		Payload:           json.RawMessage(`{"msg":"hi"}`),
		MaxRetries:        2,
		CreatedAt:         now.Add(-time.Minute),
		UpdatedAt:         now.Add(-time.Minute),
	}
	if err := store.PutExecutionJob(context.Background(), job); err != nil {
		t.Fatalf("put job: %v", err)
	}

	dispatcher := &fakeDispatcher{region: "ap-south-1", stats: domain.RegionStats{AvailableHosts: 1}}
	svc := New(store, []RegionDispatcher{dispatcher})
	svc.now = func() time.Time { return now }
	svc.schedulingTimeout = 10 * time.Second

	recovered, err := svc.recoverStaleScheduling(context.Background())
	if err != nil {
		t.Fatalf("recover stale scheduling: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected one recovered job, got %d", recovered)
	}

	requeued, err := store.GetExecutionJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get requeued job: %v", err)
	}
	if requeued.State != domain.JobStateQueued {
		t.Fatalf("expected queued state after recovery, got %s", requeued.State)
	}
	if requeued.TargetRegion != "" {
		t.Fatalf("expected cleared target region, got %s", requeued.TargetRegion)
	}
	if requeued.Error != "scheduling claim expired before dispatch" {
		t.Fatalf("unexpected recovery error message: %s", requeued.Error)
	}
}

func TestScheduleNextPrefersLowerBacklogRegion(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	seedFunctionVersion(t, store, "fn-backlog", []string{"ap-south-1", "ap-southeast-1"})
	now := time.Now().UTC()
	if err := store.PutExecutionJob(context.Background(), &domain.ExecutionJob{
		ID:                "job-existing-1",
		FunctionVersionID: "fn-backlog",
		ProjectID:         "demo",
		TargetRegion:      "ap-south-1",
		State:             domain.JobStateAssigned,
		Payload:           json.RawMessage(`{"msg":"existing"}`),
		MaxRetries:        1,
		CreatedAt:         now.Add(-time.Minute),
		UpdatedAt:         now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("put existing job: %v", err)
	}

	busy := &fakeDispatcher{
		region: "ap-south-1",
		stats:  domain.RegionStats{AvailableHosts: 1, BlankWarm: 1},
	}
	idle := &fakeDispatcher{
		region: "ap-southeast-1",
		stats:  domain.RegionStats{AvailableHosts: 1, BlankWarm: 1},
	}
	svc := New(store, []RegionDispatcher{busy, idle})

	job, err := svc.DispatchExecution(context.Background(), "fn-backlog", json.RawMessage(`{"msg":"hi"}`))
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
		t.Fatalf("expected lower backlog region, got %s", job.TargetRegion)
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

func seedFunctionVersion(t *testing.T, store *memstore.Store, versionID string, regions []string) *domain.FunctionVersion {
	t.Helper()

	now := time.Now().UTC()
	artifactRegions := make(map[string]time.Time, len(regions))
	for _, region := range regions {
		artifactRegions[region] = now
	}
	seedArtifact(t, store, &domain.Artifact{
		Digest:     "digest",
		SizeBytes:  128,
		BundleKey:  "artifacts/digest/bundle.tgz",
		StartupKey: "artifacts/digest/startup.json",
		Regions:    artifactRegions,
		CreatedAt:  now,
	})
	version := &domain.FunctionVersion{
		ID:             versionID,
		ProjectID:      "demo",
		Name:           "echo",
		Runtime:        "node22",
		MemoryMB:       128,
		Entrypoint:     "index.mjs",
		TimeoutSec:     5,
		NetworkPolicy:  domain.NetworkPolicyFull,
		Regions:        append([]string(nil), regions...),
		ArtifactDigest: "digest",
		State:          domain.FunctionStateReady,
		CreatedAt:      now,
	}
	if err := store.PutFunctionVersion(context.Background(), version); err != nil {
		t.Fatalf("put function version: %v", err)
	}
	return version
}

func seedRegion(t *testing.T, store *memstore.Store, region *domain.Region) {
	t.Helper()
	if err := store.PutRegion(context.Background(), region); err != nil {
		t.Fatalf("put region: %v", err)
	}
}

func seedArtifact(t *testing.T, store *memstore.Store, artifact *domain.Artifact) {
	t.Helper()
	if err := store.PutArtifact(context.Background(), artifact); err != nil {
		t.Fatalf("put artifact: %v", err)
	}
}
