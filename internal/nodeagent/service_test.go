package nodeagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	regionv1 "github.com/theg1239/lecrev/lecrev/region/v1"

	"github.com/theg1239/lecrev/internal/artifact"
	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/firecracker"
	"github.com/theg1239/lecrev/internal/secrets"
	memstore "github.com/theg1239/lecrev/internal/store/memory"
)

func TestExecuteAssignmentFailsWhenLogsExceedLimit(t *testing.T) {
	t.Parallel()

	result := &firecracker.ExecuteResult{
		ExitCode:         0,
		Logs:             strings.Repeat("l", maxExecutionLogBytes+128),
		Output:           json.RawMessage(`{"ok":true}`),
		SnapshotEligible: true,
		StartedAt:        time.Now().UTC(),
		FinishedAt:       time.Now().UTC(),
	}
	svc, objects := newTestService(t, stubDriver{result: result, err: nil})

	var updates []*regionv1.AssignmentUpdate
	svc.executeAssignment(context.Background(), testAssignment(), func(update *regionv1.AssignmentUpdate) {
		updates = append(updates, update)
	}, func() {})

	last := updates[len(updates)-1]
	if last.State != regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED {
		t.Fatalf("expected failed terminal update, got %s", last.State)
	}
	if last.ExitCode != 1 {
		t.Fatalf("expected coerced failure exit code 1, got %d", last.ExitCode)
	}
	if !strings.Contains(last.ErrorMessage, "execution logs exceeded limit") {
		t.Fatalf("expected log-limit error, got %q", last.ErrorMessage)
	}
	if len(last.Logs) != maxExecutionLogBytes {
		t.Fatalf("expected truncated logs length %d, got %d", maxExecutionLogBytes, len(last.Logs))
	}
	if string(last.OutputJson) != `{"ok":true}` {
		t.Fatalf("expected output to remain intact, got %s", string(last.OutputJson))
	}

	archivedLogs, err := objects.Get(context.Background(), artifact.ExecutionLogsKey("job-1", "attempt-1"))
	if err != nil {
		t.Fatalf("get archived logs: %v", err)
	}
	if len(archivedLogs) != maxExecutionLogBytes {
		t.Fatalf("expected archived truncated logs length %d, got %d", maxExecutionLogBytes, len(archivedLogs))
	}
}

func TestExecuteAssignmentFailsWhenOutputExceedsLimit(t *testing.T) {
	t.Parallel()

	largeOutput, err := json.Marshal(strings.Repeat("o", maxExecutionOutputBytes+128))
	if err != nil {
		t.Fatalf("marshal oversized output: %v", err)
	}
	result := &firecracker.ExecuteResult{
		ExitCode:         0,
		Logs:             "ok",
		Output:           largeOutput,
		SnapshotEligible: true,
		StartedAt:        time.Now().UTC(),
		FinishedAt:       time.Now().UTC(),
	}
	svc, objects := newTestService(t, stubDriver{result: result, err: nil})

	var updates []*regionv1.AssignmentUpdate
	svc.executeAssignment(context.Background(), testAssignment(), func(update *regionv1.AssignmentUpdate) {
		updates = append(updates, update)
	}, func() {})

	last := updates[len(updates)-1]
	if last.State != regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED {
		t.Fatalf("expected failed terminal update, got %s", last.State)
	}
	if last.ExitCode != 1 {
		t.Fatalf("expected coerced failure exit code 1, got %d", last.ExitCode)
	}
	if !strings.Contains(last.ErrorMessage, "execution output exceeded limit") {
		t.Fatalf("expected output-limit error, got %q", last.ErrorMessage)
	}
	if len(last.OutputJson) != 0 {
		t.Fatalf("expected oversized output to be dropped, got %d bytes", len(last.OutputJson))
	}

	archivedOutput, err := objects.Get(context.Background(), artifact.ExecutionOutputKey("job-1", "attempt-1"))
	if err != nil {
		t.Fatalf("get archived output: %v", err)
	}
	if string(archivedOutput) != "null" {
		t.Fatalf("expected archived null output, got %s", string(archivedOutput))
	}
}

type stubDriver struct {
	result *firecracker.ExecuteResult
	err    error
}

func (d stubDriver) Name() string {
	return "stub-driver"
}

func (d stubDriver) Execute(context.Context, firecracker.ExecuteRequest) (*firecracker.ExecuteResult, error) {
	return d.result, d.err
}

func newTestService(t *testing.T, driver firecracker.Driver) (*Service, artifact.Store) {
	t.Helper()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	bundle, err := artifact.BundleFromFiles(map[string][]byte{
		"index.mjs": []byte(`export async function handler() { return { ok: true }; }`),
	})
	if err != nil {
		t.Fatalf("bundle fixture: %v", err)
	}
	const digest = "digest"
	if err := meta.PutArtifact(context.Background(), &domain.Artifact{
		Digest:     digest,
		SizeBytes:  int64(len(bundle)),
		BundleKey:  "artifacts/digest/bundle.tgz",
		StartupKey: "artifacts/digest/startup.json",
		Regions: map[string]time.Time{
			"ap-south-1": time.Now().UTC(),
		},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	if err := objects.Put(context.Background(), "artifacts/digest/bundle.tgz", bundle); err != nil {
		t.Fatalf("put bundle object: %v", err)
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
		ArtifactDigest: digest,
		SourceType:     domain.SourceTypeBundle,
		State:          domain.FunctionStateReady,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("put function version: %v", err)
	}
	svc := New("host-ap-south-1-a", "ap-south-1", "", driver, objects, meta, stubResolver{})
	return svc, objects
}

func testAssignment() *regionv1.ExecutionAssignment {
	return &regionv1.ExecutionAssignment{
		AttemptId:         "attempt-1",
		JobId:             "job-1",
		FunctionVersionId: "fn-1",
		ArtifactDigest:    "digest",
		Entrypoint:        "index.mjs",
		TimeoutSec:        10,
	}
}

type stubResolver struct{}

func (stubResolver) ResolveExecution(context.Context, secrets.ExecutionRequest) (map[string]string, error) {
	return map[string]string{}, nil
}
