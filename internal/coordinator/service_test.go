package coordinator

import (
	"context"
	"testing"
	"time"

	regionv1 "github.com/theg1239/lecrev/lecrev/region/v1"

	"github.com/theg1239/lecrev/internal/domain"
	memstore "github.com/theg1239/lecrev/internal/store/memory"
)

func TestHandleAssignmentUpdateRecordsCost(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	now := time.Now().UTC()
	if _, err := meta.EnsureProject(context.Background(), "demo", "tenant-dev", "demo"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	if err := meta.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-1",
		ProjectID:      "demo",
		Name:           "echo",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		MemoryMB:       128,
		TimeoutSec:     10,
		NetworkPolicy:  domain.NetworkPolicyFull,
		Regions:        []string{"ap-south-1"},
		ArtifactDigest: "digest",
		State:          domain.FunctionStateReady,
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("put function version: %v", err)
	}
	job := &domain.ExecutionJob{
		ID:                "job-1",
		FunctionVersionID: "fn-1",
		ProjectID:         "demo",
		State:             domain.JobStateAssigned,
		Payload:           []byte(`{"hello":"world"}`),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := meta.PutExecutionJob(context.Background(), job); err != nil {
		t.Fatalf("put execution job: %v", err)
	}
	attempt := &domain.Attempt{
		ID:                "attempt-1",
		JobID:             job.ID,
		FunctionVersionID: "fn-1",
		HostID:            "host-ap-south-1-a",
		Region:            "ap-south-1",
		State:             domain.AttemptStateAssigned,
		StartMode:         domain.StartModeBlankWarm,
		LeaseExpiresAt:    now.Add(30 * time.Second),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := meta.PutAttempt(context.Background(), attempt); err != nil {
		t.Fatalf("put attempt: %v", err)
	}

	svc := New("ap-south-1", meta, nil)
	sendCh := make(chan *regionv1.CoordinatorMessage, 4)
	if err := svc.registerHost(&regionv1.RegisterHost{
		HostId:         "host-ap-south-1-a",
		Region:         "ap-south-1",
		Driver:         "local-node",
		AvailableSlots: 1,
		BlankWarm:      1,
	}, sendCh); err != nil {
		t.Fatalf("register host: %v", err)
	}

	if err := svc.handleAssignmentUpdate(context.Background(), &regionv1.AssignmentUpdate{
		HostId:    "host-ap-south-1-a",
		Region:    "ap-south-1",
		AttemptId: attempt.ID,
		JobId:     job.ID,
		State:     regionv1.AssignmentState_ASSIGNMENT_STATE_RUNNING,
	}); err != nil {
		t.Fatalf("running update: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := svc.handleAssignmentUpdate(context.Background(), &regionv1.AssignmentUpdate{
		HostId:     "host-ap-south-1-a",
		Region:     "ap-south-1",
		AttemptId:  attempt.ID,
		JobId:      job.ID,
		State:      regionv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED,
		ExitCode:   0,
		OutputJson: []byte(`{"ok":true}`),
		Logs:       "done",
	}); err != nil {
		t.Fatalf("success update: %v", err)
	}
	storedJob, err := meta.GetExecutionJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get execution job: %v", err)
	}
	if storedJob.Result == nil {
		t.Fatal("expected job result to be recorded")
	}
	if storedJob.Result.LogsKey == "" {
		t.Fatal("expected archived logs key on job result")
	}
	if storedJob.Result.OutputKey == "" {
		t.Fatal("expected archived output key on job result")
	}

	records, err := meta.ListCostRecordsByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("list cost records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 cost record, got %d", len(records))
	}
	record := records[0]
	if record.ID != attempt.ID {
		t.Fatalf("expected cost record id %s, got %s", attempt.ID, record.ID)
	}
	if record.StartMode != domain.StartModeBlankWarm {
		t.Fatalf("expected start mode %s, got %s", domain.StartModeBlankWarm, record.StartMode)
	}
	if record.CPUMs <= 0 {
		t.Fatalf("expected cpu cost > 0, got %d", record.CPUMs)
	}
	if record.MemoryMBMs != 128*record.CPUMs {
		t.Fatalf("expected memory cost %d, got %d", 128*record.CPUMs, record.MemoryMBMs)
	}
	if record.WarmInstanceMs != record.CPUMs {
		t.Fatalf("expected warm instance ms %d, got %d", record.CPUMs, record.WarmInstanceMs)
	}
}

func TestDrainHostClearsWarmPoolsAndPreventsSlotRecovery(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	svc := New("ap-south-1", meta, nil)
	sendCh := make(chan *regionv1.CoordinatorMessage, 4)
	if err := svc.registerHost(&regionv1.RegisterHost{
		HostId:         "host-ap-south-1-a",
		Region:         "ap-south-1",
		Driver:         "local-node",
		AvailableSlots: 1,
		BlankWarm:      1,
		FunctionWarm: []*regionv1.WarmPoolMetric{
			{FunctionVersionId: "fn-1", Available: 2},
		},
	}, sendCh); err != nil {
		t.Fatalf("register host: %v", err)
	}

	if err := svc.DrainHost(context.Background(), "host-ap-south-1-a", "maintenance"); err != nil {
		t.Fatalf("drain host: %v", err)
	}

	msg := <-sendCh
	drain, ok := msg.Body.(*regionv1.CoordinatorMessage_Drain)
	if !ok {
		t.Fatalf("expected drain message, got %T", msg.Body)
	}
	if drain.Drain.Reason != "maintenance" {
		t.Fatalf("expected drain reason maintenance, got %s", drain.Drain.Reason)
	}

	host, err := meta.GetHost(context.Background(), "host-ap-south-1-a")
	if err != nil {
		t.Fatalf("get host: %v", err)
	}
	if host.State != domain.HostStateDraining {
		t.Fatalf("expected draining host state, got %s", host.State)
	}
	if host.AvailableSlots != 0 {
		t.Fatalf("expected host available slots 0, got %d", host.AvailableSlots)
	}

	pools, err := meta.ListWarmPoolsByRegion(context.Background(), "ap-south-1")
	if err != nil {
		t.Fatalf("list warm pools: %v", err)
	}
	if len(pools) != 0 {
		t.Fatalf("expected warm pools to be cleared, got %d", len(pools))
	}

	svc.releaseHostSlot("host-ap-south-1-a")
	host, err = meta.GetHost(context.Background(), "host-ap-south-1-a")
	if err != nil {
		t.Fatalf("get host after release: %v", err)
	}
	if host.AvailableSlots != 0 {
		t.Fatalf("expected draining host to stay unschedulable, got %d slots", host.AvailableSlots)
	}
}

func TestPrepareFunctionWarmSendsSnapshotPrepCommand(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	now := time.Now().UTC()
	if err := meta.PutArtifact(context.Background(), &domain.Artifact{
		Digest:     "digest",
		SizeBytes:  128,
		BundleKey:  "artifacts/digest/bundle.tgz",
		StartupKey: "artifacts/digest/startup.json",
		Regions: map[string]time.Time{
			"ap-south-1": now,
		},
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	if err := meta.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-prepare",
		ProjectID:      "demo",
		Name:           "echo",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		MemoryMB:       128,
		TimeoutSec:     10,
		NetworkPolicy:  domain.NetworkPolicyNone,
		Regions:        []string{"ap-south-1"},
		ArtifactDigest: "digest",
		State:          domain.FunctionStateReady,
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("put function version: %v", err)
	}

	svc := New("ap-south-1", meta, nil)
	sendCh := make(chan *regionv1.CoordinatorMessage, 4)
	if err := svc.registerHost(&regionv1.RegisterHost{
		HostId:         "host-ap-south-1-a",
		Region:         "ap-south-1",
		Driver:         "firecracker",
		AvailableSlots: 2,
		BlankWarm:      1,
	}, sendCh); err != nil {
		t.Fatalf("register host: %v", err)
	}

	version, err := meta.GetFunctionVersion(context.Background(), "fn-prepare")
	if err != nil {
		t.Fatalf("get function version: %v", err)
	}
	if err := svc.PrepareFunctionWarm(context.Background(), version); err != nil {
		t.Fatalf("prepare function warm: %v", err)
	}

	msg := <-sendCh
	prepare, ok := msg.Body.(*regionv1.CoordinatorMessage_Prepare)
	if !ok {
		t.Fatalf("expected prepare message, got %T", msg.Body)
	}
	if prepare.Prepare.FunctionVersionId != version.ID {
		t.Fatalf("expected function version %s, got %s", version.ID, prepare.Prepare.FunctionVersionId)
	}
	if prepare.Prepare.SnapshotKind != regionv1.SnapshotKind_SNAPSHOT_KIND_FUNCTION {
		t.Fatalf("expected function snapshot kind, got %s", prepare.Prepare.SnapshotKind)
	}

	host, err := meta.GetHost(context.Background(), "host-ap-south-1-a")
	if err != nil {
		t.Fatalf("get host: %v", err)
	}
	if host.AvailableSlots != 1 {
		t.Fatalf("expected one slot reserved for prep, got %d", host.AvailableSlots)
	}
}

func TestReapStaleHostsMarksPersistedHostsDown(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	now := time.Now().UTC()
	host := &domain.Host{
		ID:             "host-ap-south-2-a",
		Region:         "ap-south-2",
		Driver:         "firecracker",
		State:          domain.HostStateActive,
		AvailableSlots: 1,
		BlankWarm:      1,
		FunctionWarm: map[string]int{
			"fn-stale": 1,
		},
		LastHeartbeat: now.Add(-time.Minute),
	}
	if err := meta.PutHost(context.Background(), host); err != nil {
		t.Fatalf("put host: %v", err)
	}
	if err := meta.ReplaceWarmPoolsForHost(context.Background(), host.Region, host.ID, []domain.WarmPool{
		{
			Region:    host.Region,
			HostID:    host.ID,
			BlankWarm: 1,
			UpdatedAt: now.Add(-time.Minute),
		},
		{
			Region:            host.Region,
			HostID:            host.ID,
			FunctionVersionID: "fn-stale",
			FunctionWarm:      1,
			UpdatedAt:         now.Add(-time.Minute),
		},
	}); err != nil {
		t.Fatalf("replace warm pools: %v", err)
	}

	svc := New("ap-south-2", meta, nil)
	svc.now = func() time.Time { return now }
	svc.staleHostAfter = 15 * time.Second

	if err := svc.reapStaleHosts(context.Background()); err != nil {
		t.Fatalf("reap stale hosts: %v", err)
	}

	storedHost, err := meta.GetHost(context.Background(), host.ID)
	if err != nil {
		t.Fatalf("get host: %v", err)
	}
	if storedHost.State != domain.HostStateDown {
		t.Fatalf("expected host state down, got %s", storedHost.State)
	}
	if storedHost.AvailableSlots != 0 {
		t.Fatalf("expected zero available slots, got %d", storedHost.AvailableSlots)
	}
	if storedHost.BlankWarm != 0 || len(storedHost.FunctionWarm) != 0 {
		t.Fatalf("expected warm inventory cleared, got blank=%d function=%v", storedHost.BlankWarm, storedHost.FunctionWarm)
	}

	pools, err := meta.ListWarmPoolsByRegion(context.Background(), host.Region)
	if err != nil {
		t.Fatalf("list warm pools: %v", err)
	}
	if len(pools) != 0 {
		t.Fatalf("expected stale warm pools to be removed, got %d", len(pools))
	}

	region, err := meta.GetRegion(context.Background(), host.Region)
	if err != nil {
		t.Fatalf("get region: %v", err)
	}
	if region.State != "degraded" {
		t.Fatalf("expected degraded region, got %s", region.State)
	}
	if region.AvailableHosts != 0 || region.BlankWarm != 0 || region.FunctionWarm != 0 {
		t.Fatalf("expected empty degraded region stats, got %+v", region)
	}
}
