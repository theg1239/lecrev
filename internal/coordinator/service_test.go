package coordinator

import (
	"context"
	"testing"
	"time"

	regionv1 "github.com/ishaan/eeeverc/eeeverc/region/v1"

	"github.com/ishaan/eeeverc/internal/domain"
	memstore "github.com/ishaan/eeeverc/internal/store/memory"
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
