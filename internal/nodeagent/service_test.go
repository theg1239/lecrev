package nodeagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
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

	archivedLogs := waitForStoredObject(t, objects, artifact.ExecutionLogsKey("job-1", "attempt-1"))
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

	archivedOutput := waitForStoredObject(t, objects, artifact.ExecutionOutputKey("job-1", "attempt-1"))
	if string(archivedOutput) != "null" {
		t.Fatalf("expected archived null output, got %s", string(archivedOutput))
	}
}

func TestExecuteAssignmentDoesNotBlockOnArtifactArchival(t *testing.T) {
	t.Parallel()

	objects := &slowExecutionArtifactStore{
		Store: artifact.NewMemoryStore(),
		delay: 250 * time.Millisecond,
		puts:  make(chan string, 4),
	}
	bundle, err := artifact.BundleFromFiles(map[string][]byte{
		"index.mjs": []byte(`export async function handler() { return { ok: true }; }`),
	})
	if err != nil {
		t.Fatalf("bundle fixture: %v", err)
	}
	if err := objects.Store.Put(context.Background(), "artifacts/digest/bundle.tgz", bundle); err != nil {
		t.Fatalf("put bundle object: %v", err)
	}

	svc := NewWithConfig(Config{}, "host-ap-south-1-a", "ap-south-1", "", stubDriver{
		result: &firecracker.ExecuteResult{
			ExitCode:   0,
			Output:     json.RawMessage(`{"ok":true}`),
			StartedAt:  time.Now().UTC(),
			FinishedAt: time.Now().UTC(),
		},
	}, objects, stubResolver{})

	var terminal *regionv1.AssignmentUpdate
	started := time.Now()
	svc.executeAssignment(context.Background(), testAssignment(), func(update *regionv1.AssignmentUpdate) {
		if update.State == regionv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED || update.State == regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED {
			terminal = update
		}
	}, func() {})
	if elapsed := time.Since(started); elapsed >= 150*time.Millisecond {
		t.Fatalf("expected terminal update before archival delay, took %s", elapsed)
	}
	if terminal == nil || terminal.State != regionv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED {
		t.Fatalf("expected succeeded terminal update, got %+v", terminal)
	}

	wantKeys := map[string]bool{
		artifact.ExecutionLogsKey("job-1", "attempt-1"):   false,
		artifact.ExecutionOutputKey("job-1", "attempt-1"): false,
	}
	waitForArtifactPuts(t, objects.puts, wantKeys)
}

func TestExecuteAssignmentDoesNotBlockOnDeferredCleanup(t *testing.T) {
	t.Parallel()

	driver := &deferredCleanupDriver{
		result: &firecracker.ExecuteResult{
			ExitCode:   0,
			Output:     json.RawMessage(`{"ok":true}`),
			StartedAt:  time.Now().UTC(),
			FinishedAt: time.Now().UTC(),
		},
		cleanupDelay: 250 * time.Millisecond,
	}
	svc, _ := newTestService(t, driver)

	terminalCh := make(chan *regionv1.AssignmentUpdate, 1)
	done := make(chan struct{})
	go func() {
		svc.executeAssignment(context.Background(), testAssignment(), func(update *regionv1.AssignmentUpdate) {
			if update.State == regionv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED || update.State == regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED {
				select {
				case terminalCh <- update:
				default:
				}
			}
		}, func() {})
		close(done)
	}()

	started := time.Now()
	var terminal *regionv1.AssignmentUpdate
	select {
	case terminal = <-terminalCh:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("timed out waiting for terminal update before deferred cleanup finished")
	}
	if elapsed := time.Since(started); elapsed >= 150*time.Millisecond {
		t.Fatalf("expected terminal update before deferred cleanup delay, took %s", elapsed)
	}
	if terminal == nil || terminal.State != regionv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED {
		t.Fatalf("expected succeeded terminal update, got %+v", terminal)
	}
	<-done
	if driver.cleanupCount() != 1 {
		t.Fatalf("expected deferred cleanup to run once, got %d", driver.cleanupCount())
	}
}

func TestRegistrationAndWarmupUseDriverInventory(t *testing.T) {
	t.Parallel()

	driver := &inventoryDriver{
		result: &firecracker.ExecuteResult{
			ExitCode:   0,
			Output:     json.RawMessage(`{"ok":true}`),
			StartedAt:  time.Now().UTC(),
			FinishedAt: time.Now().UTC(),
		},
		inventory: firecracker.WarmInventory{
			BlankWarm:    0,
			FunctionWarm: map[string]int{},
		},
	}
	svc, _ := newConfiguredTestService(t, driver, Config{MaxConcurrentAssignments: 3})

	if err := svc.prepareDriver(context.Background()); err != nil {
		t.Fatalf("prepare driver: %v", err)
	}

	register := svc.RegistrationMessage()
	if register.AvailableSlots != 3 {
		t.Fatalf("expected configured host slots 3, got %d", register.AvailableSlots)
	}
	if register.BlankWarm != 3 {
		t.Fatalf("expected blank warm inventory from driver, got %d", register.BlankWarm)
	}

	svc.executeAssignment(context.Background(), testAssignment(), func(*regionv1.AssignmentUpdate) {}, func() {})

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		heartbeat := svc.heartbeatMessage()
		if len(heartbeat.FunctionWarm) == 1 && heartbeat.FunctionWarm[0].FunctionVersionId == "fn-1" && heartbeat.FunctionWarm[0].Available == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected prepared function warm inventory, got %+v", heartbeat.FunctionWarm)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPrepareSnapshotUsesDriverWarmPreparation(t *testing.T) {
	t.Parallel()

	driver := &inventoryDriver{
		result: &firecracker.ExecuteResult{
			ExitCode:   0,
			Output:     json.RawMessage(`{"ok":true}`),
			StartedAt:  time.Now().UTC(),
			FinishedAt: time.Now().UTC(),
		},
		inventory: firecracker.WarmInventory{
			BlankWarm:    1,
			FunctionWarm: map[string]int{},
		},
	}
	svc, _ := newConfiguredTestService(t, driver, Config{MaxConcurrentAssignments: 2})

	svc.prepareSnapshot(context.Background(), &regionv1.PrepareSnapshot{
		HostId:            "host-ap-south-1-a",
		SnapshotKind:      regionv1.SnapshotKind_SNAPSHOT_KIND_FUNCTION,
		FunctionVersionId: "fn-1",
		ArtifactBundleKey: "artifacts/digest/bundle.tgz",
		Entrypoint:        "index.mjs",
		NetworkPolicy:     string(domain.NetworkPolicyFull),
		TimeoutSec:        10,
		MemoryMb:          128,
	}, func() {})

	heartbeat := svc.heartbeatMessage()
	if len(heartbeat.FunctionWarm) != 1 || heartbeat.FunctionWarm[0].FunctionVersionId != "fn-1" || heartbeat.FunctionWarm[0].Available != 2 {
		t.Fatalf("expected function warm capacity 2 after prepare, got %+v", heartbeat.FunctionWarm)
	}
}

func TestExecuteAssignmentIncludesPlatformTraceInLogs(t *testing.T) {
	t.Parallel()

	svc, _ := newTestService(t, stubDriver{result: &firecracker.ExecuteResult{
		ExitCode:   0,
		Output:     json.RawMessage(`{"ok":true}`),
		StartedAt:  time.Now().UTC(),
		FinishedAt: time.Now().UTC(),
	}})

	var terminal *regionv1.AssignmentUpdate
	svc.executeAssignment(context.Background(), testAssignment(), func(update *regionv1.AssignmentUpdate) {
		if update.State == regionv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED || update.State == regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED {
			terminal = update
		}
	}, func() {})
	if terminal == nil {
		t.Fatal("expected terminal assignment update")
	}
	for _, want := range []string{
		"[platform] step=load_bundle",
		"[platform] step=resolve_secrets",
		"[platform] step=driver_execute",
	} {
		if !strings.Contains(terminal.Logs, want) {
			t.Fatalf("expected terminal logs to contain %q, got %q", want, terminal.Logs)
		}
	}
}

func TestLoadBundleCachesImmutableArtifact(t *testing.T) {
	t.Parallel()

	objects := &countingArtifactStore{Store: artifact.NewMemoryStore()}
	if err := objects.Put(context.Background(), "artifacts/digest/bundle.tgz", []byte("bundle")); err != nil {
		t.Fatalf("put bundle: %v", err)
	}

	svc := NewWithConfig(Config{}, "host-ap-south-1-a", "ap-south-1", "", stubDriver{}, objects, stubResolver{})

	first, err := svc.loadBundle(context.Background(), "artifacts/digest/bundle.tgz")
	if err != nil {
		t.Fatalf("load bundle first: %v", err)
	}
	second, err := svc.loadBundle(context.Background(), "artifacts/digest/bundle.tgz")
	if err != nil {
		t.Fatalf("load bundle second: %v", err)
	}
	if string(first) != "bundle" || string(second) != "bundle" {
		t.Fatalf("unexpected bundle contents: %q / %q", string(first), string(second))
	}
	if objects.getCount() != 1 {
		t.Fatalf("expected a single backing-store read, got %d", objects.getCount())
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

type inventoryDriver struct {
	result    *firecracker.ExecuteResult
	inventory firecracker.WarmInventory
}

func (d *inventoryDriver) Name() string {
	return "inventory-driver"
}

func (d *inventoryDriver) Execute(context.Context, firecracker.ExecuteRequest) (*firecracker.ExecuteResult, error) {
	return d.result, nil
}

func (d *inventoryDriver) WarmInventory() firecracker.WarmInventory {
	functionWarm := make(map[string]int, len(d.inventory.FunctionWarm))
	for functionID, count := range d.inventory.FunctionWarm {
		functionWarm[functionID] = count
	}
	return firecracker.WarmInventory{
		BlankWarm:    d.inventory.BlankWarm,
		FunctionWarm: functionWarm,
	}
}

func (d *inventoryDriver) WarmInventoryForSlots(freeSlots int) firecracker.WarmInventory {
	base := d.WarmInventory()
	if freeSlots <= 0 {
		base.BlankWarm = 0
		for functionID := range base.FunctionWarm {
			base.FunctionWarm[functionID] = 0
		}
		return base
	}
	if base.BlankWarm > 0 {
		base.BlankWarm = freeSlots
	}
	for functionID, count := range base.FunctionWarm {
		if count > 0 {
			base.FunctionWarm[functionID] = freeSlots
		}
	}
	return base
}

func (d *inventoryDriver) EnsureBlankWarm(context.Context) error {
	d.inventory.BlankWarm = 1
	return nil
}

func (d *inventoryDriver) PrepareFunctionWarm(_ context.Context, req firecracker.ExecuteRequest) error {
	if d.inventory.FunctionWarm == nil {
		d.inventory.FunctionWarm = map[string]int{}
	}
	d.inventory.FunctionWarm[req.FunctionID] = 1
	return nil
}

type deferredCleanupDriver struct {
	result       *firecracker.ExecuteResult
	cleanupDelay time.Duration
	mu           sync.Mutex
	cleanups     int
}

func (d *deferredCleanupDriver) Name() string {
	return "deferred-cleanup-driver"
}

func (d *deferredCleanupDriver) Execute(context.Context, firecracker.ExecuteRequest) (*firecracker.ExecuteResult, error) {
	return d.result, nil
}

func (d *deferredCleanupDriver) ExecuteDeferred(context.Context, firecracker.ExecuteRequest) (*firecracker.ExecuteResult, func(), error) {
	return d.result, func() {
		time.Sleep(d.cleanupDelay)
		d.mu.Lock()
		d.cleanups++
		d.mu.Unlock()
	}, nil
}

func (d *deferredCleanupDriver) cleanupCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cleanups
}

func newTestService(t *testing.T, driver firecracker.Driver) (*Service, artifact.Store) {
	return newConfiguredTestService(t, driver, Config{})
}

func newConfiguredTestService(t *testing.T, driver firecracker.Driver, cfg Config) (*Service, artifact.Store) {
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
	svc := NewWithConfig(cfg, "host-ap-south-1-a", "ap-south-1", "", driver, objects, stubResolver{})
	return svc, objects
}

func testAssignment() *regionv1.ExecutionAssignment {
	return &regionv1.ExecutionAssignment{
		AttemptId:         "attempt-1",
		JobId:             "job-1",
		FunctionVersionId: "fn-1",
		ArtifactDigest:    "digest",
		ArtifactBundleKey: "artifacts/digest/bundle.tgz",
		Entrypoint:        "index.mjs",
		TimeoutSec:        10,
		MemoryMb:          128,
	}
}

type stubResolver struct{}

func (stubResolver) ResolveExecution(context.Context, secrets.ExecutionRequest) (map[string]string, error) {
	return map[string]string{}, nil
}

type countingArtifactStore struct {
	artifact.Store
	mu   sync.Mutex
	gets int
}

func (s *countingArtifactStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.Store.Get(ctx, key)
}

func (s *countingArtifactStore) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

type slowExecutionArtifactStore struct {
	artifact.Store
	delay time.Duration
	puts  chan string
}

func (s *slowExecutionArtifactStore) Put(ctx context.Context, key string, data []byte) error {
	if strings.HasPrefix(key, "executions/") && s.delay > 0 {
		time.Sleep(s.delay)
	}
	if err := s.Store.Put(ctx, key, data); err != nil {
		return err
	}
	if strings.HasPrefix(key, "executions/") && s.puts != nil {
		select {
		case s.puts <- key:
		default:
		}
	}
	return nil
}

func waitForStoredObject(t *testing.T, objects artifact.Store, key string) []byte {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := objects.Get(context.Background(), key)
		if err == nil {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for stored object %s", key)
	return nil
}

func waitForArtifactPuts(t *testing.T, puts <-chan string, want map[string]bool) {
	t.Helper()

	deadline := time.After(2 * time.Second)
	for {
		allSeen := true
		for _, seen := range want {
			if !seen {
				allSeen = false
				break
			}
		}
		if allSeen {
			return
		}
		select {
		case key := <-puts:
			if _, ok := want[key]; ok {
				want[key] = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for archival of %+v", want)
		}
	}
}
