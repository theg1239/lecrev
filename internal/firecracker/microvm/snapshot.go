package microvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/theg1239/lecrev/internal/firecracker"
	"github.com/theg1239/lecrev/internal/timetrace"
)

type snapshotAsset struct {
	dir          string
	statePath    string
	memoryPath   string
	rootFSPath   string
	metadataPath string
}

type snapshotMetadata struct {
	Kind           string    `json:"kind"`
	FunctionID     string    `json:"functionId,omitempty"`
	Entrypoint     string    `json:"entrypoint,omitempty"`
	NetworkPolicy  string    `json:"networkPolicy,omitempty"`
	MemoryMB       int       `json:"memoryMb,omitempty"`
	WorkerPrepared bool      `json:"workerPrepared,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

type vmInstance struct {
	layout      *vmLayout
	proc        *vmProcess
	api         *apiClient
	cleanupOnce sync.Once
}

func (d *Driver) validateHost() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("firecracker driver requires linux hosts")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("firecracker driver requires /dev/kvm: %w", err)
	}
	return nil
}

func (d *Driver) ExecuteDeferred(ctx context.Context, req firecracker.ExecuteRequest) (*firecracker.ExecuteResult, func(), error) {
	trace := timetrace.New()
	if err := d.validateHost(); err != nil {
		return nil, nil, err
	}
	if req.MemoryMB <= 0 {
		req.MemoryMB = int(d.config.DefaultMemoryMB)
	}
	if asset, ok := d.functionSnapshotAsset(req.FunctionID); ok {
		trace.Note("execution_path", "startMode=function-warm")
		return d.executeFromSnapshotDeferred(ctx, req, asset, true, trace)
	}
	if asset, ok := d.blankSnapshotAssetForRequest(req); ok {
		trace.Note("execution_path", "startMode=blank-warm")
		return d.executeFromSnapshotDeferred(ctx, req, asset, false, trace)
	}
	trace.Note("execution_path", "startMode=cold")
	return d.executeColdDeferred(ctx, req, trace)
}

func (d *Driver) executeCold(ctx context.Context, req firecracker.ExecuteRequest, trace *timetrace.Recorder) (*firecracker.ExecuteResult, error) {
	result, cleanup, err := d.executeColdDeferred(ctx, req, trace)
	if cleanup != nil {
		cleanup()
	}
	return result, err
}

func (d *Driver) executeColdDeferred(ctx context.Context, req firecracker.ExecuteRequest, trace *timetrace.Recorder) (*firecracker.ExecuteResult, func(), error) {
	instance, err := d.bootInstance(ctx, req, trace)
	if err != nil {
		return nil, nil, err
	}
	result, invokeErr := d.runInvocation(ctx, instance, req, false, trace)
	result = combineExecutionTrace(result, trace.String())
	return result, d.cleanupFunc(instance, result), invokeErr
}

func (d *Driver) executeFromSnapshot(ctx context.Context, req firecracker.ExecuteRequest, asset snapshotAsset, usePreparedRoot bool, trace *timetrace.Recorder) (*firecracker.ExecuteResult, error) {
	result, cleanup, err := d.executeFromSnapshotDeferred(ctx, req, asset, usePreparedRoot, trace)
	if cleanup != nil {
		cleanup()
	}
	return result, err
}

func (d *Driver) executeFromSnapshotDeferred(ctx context.Context, req firecracker.ExecuteRequest, asset snapshotAsset, usePreparedRoot bool, trace *timetrace.Recorder) (*firecracker.ExecuteResult, func(), error) {
	instance, err := d.restoreInstance(ctx, req, asset, trace)
	if err != nil {
		return nil, nil, err
	}
	result, invokeErr := d.runInvocation(ctx, instance, req, usePreparedRoot, trace)
	result = combineExecutionTrace(result, trace.String())
	return result, d.cleanupFunc(instance, result), invokeErr
}

func (d *Driver) cleanupFunc(instance *vmInstance, result *firecracker.ExecuteResult) func() {
	if instance == nil {
		return nil
	}
	return func() {
		cleanupStarted := time.Now()
		instance.cleanupWithTimeout(d.config.InvokeShutdownTimeout)
		cleanupTrace := timetrace.New()
		cleanupTrace.Step("cleanup_vm", cleanupStarted)
		if result != nil {
			result.PlatformTrace = timetrace.Combine(result.PlatformTrace, cleanupTrace.String())
		}
	}
}

func combineExecutionTrace(result *firecracker.ExecuteResult, outerTrace string) *firecracker.ExecuteResult {
	if result == nil {
		return nil
	}
	result.PlatformTrace = timetrace.Combine(outerTrace, result.PlatformTrace)
	return result
}

func (d *Driver) runInvocation(ctx context.Context, instance *vmInstance, req firecracker.ExecuteRequest, usePreparedRoot bool, trace *timetrace.Recorder) (*firecracker.ExecuteResult, error) {
	invokeCtx, cancel := context.WithTimeout(ctx, d.config.ConnectTimeout+req.Timeout)
	defer cancel()

	invokeStarted := time.Now()
	response, err := invokeGuest(invokeCtx, instance.layout.vsockSocketHost, d.config.GuestVSockPort, guestInvocationRequest(req, usePreparedRoot))
	trace.Step("invoke_guest", invokeStarted)

	if response == nil {
		return nil, fmt.Errorf("guest execution failed: %w; firecracker logs: %s", err, instance.proc.logs())
	}
	result := &firecracker.ExecuteResult{
		ExitCode:         response.ExitCode,
		Logs:             response.Logs,
		Output:           response.Output,
		SnapshotEligible: false,
		StartedAt:        response.StartedAt,
		FinishedAt:       response.FinishedAt,
	}
	if err != nil {
		result.Logs = combineLogs(response.Logs, instance.proc.logs())
		return result, err
	}
	if response.Error != "" {
		result.Logs = combineLogs(response.Logs, instance.proc.logs())
		return result, errors.New(response.Error)
	}
	return result, nil
}

func guestInvocationRequest(req firecracker.ExecuteRequest, usePreparedRoot bool) firecracker.GuestInvocationRequest {
	artifactBundle := req.ArtifactBundle
	if usePreparedRoot {
		artifactBundle = nil
	}
	return firecracker.GuestInvocationRequest{
		AttemptID:       req.AttemptID,
		JobID:           req.JobID,
		FunctionID:      req.FunctionID,
		Entrypoint:      req.Entrypoint,
		ArtifactBundle:  artifactBundle,
		UsePreparedRoot: usePreparedRoot,
		Payload:         req.Payload,
		Env:             req.Env,
		TimeoutMillis:   req.Timeout.Milliseconds(),
		Region:          req.Region,
		HostID:          req.HostID,
	}
}

func (d *Driver) bootInstance(ctx context.Context, req firecracker.ExecuteRequest, trace *timetrace.Recorder) (*vmInstance, error) {
	prepareLayoutStarted := time.Now()
	layout, vmID, err := d.prepareLayout(req.AttemptID)
	if err != nil {
		return nil, err
	}
	trace.Step("prepare_layout", prepareLayoutStarted)
	stageKernelStarted := time.Now()
	if err := stageKernel(layout.kernelPath, d.config.KernelImagePath); err != nil {
		layout.cleanup()
		return nil, err
	}
	trace.Step("stage_kernel", stageKernelStarted)
	stageRootFSStarted := time.Now()
	if err := copyFile(d.config.RootFSPath, layout.rootFSPath); err != nil {
		layout.cleanup()
		return nil, err
	}
	trace.Step("stage_rootfs", stageRootFSStarted)
	ownershipStarted := time.Now()
	if err := d.prepareOwnership(layout, layout.kernelPath, layout.rootFSPath); err != nil {
		layout.cleanup()
		return nil, err
	}
	trace.Step("prepare_ownership", ownershipStarted)

	startVMStarted := time.Now()
	proc, err := d.startVM(ctx, vmID, layout)
	if err != nil {
		layout.cleanup()
		return nil, err
	}
	trace.Step("start_firecracker", startVMStarted)
	waitSocketStarted := time.Now()
	if err := waitForFile(ctx, layout.apiSocketHost, d.config.StartTimeout, proc); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("wait for firecracker api socket: %w; logs: %s", err, proc.logs())
	}
	trace.Step("wait_api_socket", waitSocketStarted)

	api := newAPIClient(layout.apiSocketHost)
	guestCID := d.allocCID()
	bootArgs, err := d.bootArgs(req)
	if err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, err
	}
	configMachineStarted := time.Now()
	if err := api.put(ctx, "/machine-config", machineConfig{
		VCPUCount:  d.config.VCPUCount,
		MemSizeMib: int64(req.MemoryMB),
		Smt:        false,
	}); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("configure machine: %w; logs: %s", err, proc.logs())
	}
	trace.Step("configure_machine", configMachineStarted)
	bootSourceStarted := time.Now()
	if err := api.put(ctx, "/boot-source", bootSource{
		KernelImagePath: layout.guestKernelPath,
		BootArgs:        bootArgs,
	}); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("configure boot source: %w; logs: %s", err, proc.logs())
	}
	trace.Step("configure_boot_source", bootSourceStarted)
	rootfsDriveStarted := time.Now()
	if err := api.put(ctx, "/drives/rootfs", drive{
		DriveID:      "rootfs",
		PathOnHost:   layout.guestRootFSPath,
		IsRootDevice: true,
		IsReadOnly:   false,
	}); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("configure rootfs: %w; logs: %s", err, proc.logs())
	}
	trace.Step("configure_rootfs_drive", rootfsDriveStarted)
	if strings.EqualFold(req.NetworkPolicy, "full") {
		networkStarted := time.Now()
		if err := api.put(ctx, "/network-interfaces/eth0", networkInterface{
			IfaceID:     "eth0",
			HostDevName: d.config.TapDevice,
			GuestMAC:    d.config.GuestMAC,
		}); err != nil {
			layout.cleanup()
			_ = proc.close()
			return nil, fmt.Errorf("configure network interface: %w; logs: %s", err, proc.logs())
		}
		trace.Step("configure_network", networkStarted)
	}
	vsockStarted := time.Now()
	if err := api.put(ctx, "/vsock", vsockDevice{
		VsockID:  "vsock0",
		GuestCID: guestCID,
		UDSPath:  layout.guestVSockPath,
	}); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("configure vsock: %w; logs: %s", err, proc.logs())
	}
	trace.Step("configure_vsock", vsockStarted)
	instanceStartStarted := time.Now()
	if err := api.put(ctx, "/actions", action{ActionType: "InstanceStart"}); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("start vm: %w; logs: %s", err, proc.logs())
	}
	trace.Step("instance_start", instanceStartStarted)

	return &vmInstance{layout: layout, proc: proc, api: api}, nil
}

func (d *Driver) restoreInstance(ctx context.Context, req firecracker.ExecuteRequest, asset snapshotAsset, trace *timetrace.Recorder) (*vmInstance, error) {
	prepareLayoutStarted := time.Now()
	layout, vmID, err := d.prepareLayout(req.AttemptID)
	if err != nil {
		return nil, err
	}
	trace.Step("prepare_layout", prepareLayoutStarted)

	instanceStatePath := filepath.Join(layout.rootDir, "snapshot.state")
	instanceMemoryPath := filepath.Join(layout.rootDir, "snapshot.mem")
	stageStateStarted := time.Now()
	if err := copyFile(asset.statePath, instanceStatePath); err != nil {
		layout.cleanup()
		return nil, err
	}
	trace.Step("stage_snapshot_state", stageStateStarted)
	stageMemoryStarted := time.Now()
	if err := copyFile(asset.memoryPath, instanceMemoryPath); err != nil {
		layout.cleanup()
		return nil, err
	}
	trace.Step("stage_snapshot_memory", stageMemoryStarted)
	stageRootFSStarted := time.Now()
	if err := copyFile(asset.rootFSPath, layout.rootFSPath); err != nil {
		layout.cleanup()
		return nil, err
	}
	trace.Step("stage_snapshot_rootfs", stageRootFSStarted)
	ownershipStarted := time.Now()
	if err := d.prepareOwnership(layout, instanceStatePath, instanceMemoryPath, layout.rootFSPath); err != nil {
		layout.cleanup()
		return nil, err
	}
	trace.Step("prepare_ownership", ownershipStarted)

	startVMStarted := time.Now()
	proc, err := d.startVM(ctx, vmID, layout)
	if err != nil {
		layout.cleanup()
		return nil, err
	}
	trace.Step("start_firecracker", startVMStarted)
	waitSocketStarted := time.Now()
	if err := waitForFile(ctx, layout.apiSocketHost, d.config.StartTimeout, proc); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("wait for firecracker api socket: %w; logs: %s", err, proc.logs())
	}
	trace.Step("wait_api_socket", waitSocketStarted)

	api := newAPIClient(layout.apiSocketHost)
	loadSnapshotStarted := time.Now()
	if err := api.put(ctx, "/snapshot/load", snapshotLoadRequest{
		SnapshotPath: "snapshot.state",
		MemBackend: &snapshotMemoryBackend{
			BackendPath: "snapshot.mem",
			BackendType: "File",
		},
		ResumeVM: false,
	}); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("load snapshot: %w; logs: %s", err, proc.logs())
	}
	trace.Step("load_snapshot", loadSnapshotStarted)
	resumeStarted := time.Now()
	if err := api.patch(ctx, "/vm", vmState{State: "Resumed"}); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("resume snapshot vm: %w; logs: %s", err, proc.logs())
	}
	trace.Step("resume_snapshot", resumeStarted)
	return &vmInstance{layout: layout, proc: proc, api: api}, nil
}

func (v *vmInstance) cleanup() {
	v.cleanupWithTimeout(2 * time.Second)
}

func (v *vmInstance) cleanupWithTimeout(timeout time.Duration) {
	if v == nil {
		return
	}
	v.cleanupOnce.Do(func() {
		if v.proc != nil {
			_ = v.proc.terminate(timeout)
		}
		if v.layout != nil && v.layout.cleanup != nil {
			v.layout.cleanup()
		}
	})
}

func (d *Driver) ensureBlankSnapshotLocked(ctx context.Context) error {
	asset := d.blankSnapshotAsset()
	if asset.complete() {
		return nil
	}

	req := firecracker.ExecuteRequest{
		AttemptID:     "blank-snapshot",
		FunctionID:    "blank-snapshot",
		Timeout:       d.config.PrepareTimeout,
		MemoryMB:      int(d.config.DefaultMemoryMB),
		NetworkPolicy: "none",
	}
	instance, err := d.bootInstance(ctx, req, nil)
	if err != nil {
		return err
	}
	defer instance.cleanup()

	pingCtx, cancel := context.WithTimeout(ctx, d.config.ConnectTimeout)
	defer cancel()
	if err := pingGuest(pingCtx, instance.layout.vsockSocketHost, d.config.GuestVSockPort); err != nil {
		return fmt.Errorf("ping guest for blank snapshot: %w; firecracker logs: %s", err, instance.proc.logs())
	}

	return d.createSnapshotAsset(ctx, instance, asset, snapshotMetadata{
		Kind:          "blank",
		NetworkPolicy: "none",
		MemoryMB:      int(d.config.DefaultMemoryMB),
		CreatedAt:     time.Now().UTC(),
	})
}

func (d *Driver) ensureFunctionSnapshotLocked(ctx context.Context, req firecracker.ExecuteRequest) error {
	asset := d.functionSnapshotPath(req.FunctionID)
	if asset.complete() {
		return nil
	}

	var (
		instance *vmInstance
		err      error
	)
	if strings.EqualFold(req.NetworkPolicy, "none") {
		if err := d.ensureBlankSnapshotLocked(ctx); err == nil {
			if blankAsset, ok := d.blankSnapshotAssetForRequest(req); ok {
				instance, err = d.restoreInstance(ctx, req, blankAsset, nil)
			}
		}
	}
	if instance == nil {
		instance, err = d.bootInstance(ctx, req, nil)
	}
	if err != nil {
		return err
	}
	defer instance.cleanup()

	prepareCtx, cancel := context.WithTimeout(ctx, d.config.PrepareTimeout)
	defer cancel()
	prepareResp, err := prepareGuest(prepareCtx, instance.layout.vsockSocketHost, d.config.GuestVSockPort, firecracker.GuestPrepareRequest{
		FunctionID:     req.FunctionID,
		Entrypoint:     req.Entrypoint,
		ArtifactBundle: req.ArtifactBundle,
		Env:            req.Env,
	})
	if err != nil {
		return fmt.Errorf("prepare function snapshot for %s: %w; firecracker logs: %s", req.FunctionID, err, instance.proc.logs())
	}

	return d.createSnapshotAsset(ctx, instance, asset, snapshotMetadata{
		Kind:           "function",
		FunctionID:     req.FunctionID,
		Entrypoint:     req.Entrypoint,
		NetworkPolicy:  req.NetworkPolicy,
		MemoryMB:       req.MemoryMB,
		WorkerPrepared: prepareResp != nil && prepareResp.WorkerPrepared,
		CreatedAt:      time.Now().UTC(),
	})
}

func (d *Driver) createSnapshotAsset(ctx context.Context, instance *vmInstance, asset snapshotAsset, metadata snapshotMetadata) error {
	if err := os.MkdirAll(filepath.Dir(asset.dir), 0o755); err != nil {
		return err
	}
	tempDir, err := os.MkdirTemp(filepath.Dir(asset.dir), filepath.Base(asset.dir)+"-tmp-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	snapshotStatePath := filepath.Join(instance.layout.rootDir, "snapshot.state")
	snapshotMemoryPath := filepath.Join(instance.layout.rootDir, "snapshot.mem")
	if err := instance.api.patch(ctx, "/vm", vmState{State: "Paused"}); err != nil {
		return err
	}
	if err := instance.api.put(ctx, "/snapshot/create", snapshotCreateRequest{
		SnapshotType: "Full",
		SnapshotPath: "snapshot.state",
		MemFilePath:  "snapshot.mem",
	}); err != nil {
		return err
	}
	if err := copyFile(snapshotStatePath, filepath.Join(tempDir, "snapshot.state")); err != nil {
		return err
	}
	if err := copyFile(snapshotMemoryPath, filepath.Join(tempDir, "snapshot.mem")); err != nil {
		return err
	}
	if err := copyFile(instance.layout.rootFSPath, filepath.Join(tempDir, "rootfs.ext4")); err != nil {
		return err
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tempDir, "metadata.json"), raw, 0o644); err != nil {
		return err
	}

	if instance.proc != nil && instance.proc.cmd != nil && instance.proc.cmd.Process != nil {
		_ = instance.proc.cmd.Process.Kill()
	}
	_ = instance.proc.wait(d.config.ShutdownTimeout)

	_ = os.RemoveAll(asset.dir)
	return os.Rename(tempDir, asset.dir)
}

func (d *Driver) functionWarmInventory() map[string]int {
	root := filepath.Join(d.config.SnapshotDir, "functions")
	entries, err := os.ReadDir(root)
	if err != nil {
		return map[string]int{}
	}
	out := make(map[string]int, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		asset := snapshotAsset{
			dir:          filepath.Join(root, entry.Name()),
			statePath:    filepath.Join(root, entry.Name(), "snapshot.state"),
			memoryPath:   filepath.Join(root, entry.Name(), "snapshot.mem"),
			rootFSPath:   filepath.Join(root, entry.Name(), "rootfs.ext4"),
			metadataPath: filepath.Join(root, entry.Name(), "metadata.json"),
		}
		if !asset.complete() {
			continue
		}
		meta, err := asset.metadata()
		if err != nil || meta.Kind != "function" || strings.TrimSpace(meta.FunctionID) == "" {
			continue
		}
		out[meta.FunctionID] = 1
	}
	return out
}

func (d *Driver) functionSnapshotAsset(functionID string) (snapshotAsset, bool) {
	asset := d.functionSnapshotPath(functionID)
	return asset, asset.complete()
}

func (d *Driver) blankWarmInventory() int {
	_, ok := d.blankSnapshotAssetForRequest(firecracker.ExecuteRequest{
		MemoryMB:      int(d.config.DefaultMemoryMB),
		NetworkPolicy: "none",
	})
	if !ok {
		return 0
	}
	return 1
}

func (d *Driver) blankSnapshotAsset() snapshotAsset {
	dir := filepath.Join(d.config.SnapshotDir, "blank")
	return snapshotAsset{
		dir:          dir,
		statePath:    filepath.Join(dir, "snapshot.state"),
		memoryPath:   filepath.Join(dir, "snapshot.mem"),
		rootFSPath:   filepath.Join(dir, "rootfs.ext4"),
		metadataPath: filepath.Join(dir, "metadata.json"),
	}
}

func (d *Driver) blankSnapshotAssetForRequest(req firecracker.ExecuteRequest) (snapshotAsset, bool) {
	asset := d.blankSnapshotAsset()
	if !asset.complete() {
		return snapshotAsset{}, false
	}
	meta, err := asset.metadata()
	if err != nil {
		return snapshotAsset{}, false
	}
	if meta.Kind != "blank" {
		return snapshotAsset{}, false
	}
	if !strings.EqualFold(meta.NetworkPolicy, "none") || !strings.EqualFold(req.NetworkPolicy, "none") {
		return snapshotAsset{}, false
	}
	if meta.MemoryMB > 0 && req.MemoryMB > 0 && meta.MemoryMB != req.MemoryMB {
		return snapshotAsset{}, false
	}
	return asset, true
}

func (d *Driver) functionSnapshotPath(functionID string) snapshotAsset {
	dir := filepath.Join(d.config.SnapshotDir, "functions", sanitizeID(functionID))
	return snapshotAsset{
		dir:          dir,
		statePath:    filepath.Join(dir, "snapshot.state"),
		memoryPath:   filepath.Join(dir, "snapshot.mem"),
		rootFSPath:   filepath.Join(dir, "rootfs.ext4"),
		metadataPath: filepath.Join(dir, "metadata.json"),
	}
}

func (d *Driver) prepareOwnership(layout *vmLayout, paths ...string) error {
	if !d.config.UseJailer {
		return nil
	}
	if err := os.Chown(layout.rootDir, d.config.JailerUID, d.config.JailerGID); err != nil && !os.IsPermission(err) {
		return err
	}
	for _, path := range paths {
		if err := os.Chown(path, d.config.JailerUID, d.config.JailerGID); err != nil && !os.IsPermission(err) {
			return err
		}
	}
	return nil
}

func (a snapshotAsset) complete() bool {
	for _, path := range []string{a.statePath, a.memoryPath, a.rootFSPath, a.metadataPath} {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	return true
}

func (a snapshotAsset) metadata() (*snapshotMetadata, error) {
	raw, err := os.ReadFile(a.metadataPath)
	if err != nil {
		return nil, err
	}
	var meta snapshotMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

type snapshotCreateRequest struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

type snapshotLoadRequest struct {
	SnapshotPath    string                 `json:"snapshot_path"`
	MemBackend      *snapshotMemoryBackend `json:"mem_backend,omitempty"`
	ResumeVM        bool                   `json:"resume_vm"`
	TrackDirtyPages bool                   `json:"track_dirty_pages,omitempty"`
}

type snapshotMemoryBackend struct {
	BackendPath string `json:"backend_path"`
	BackendType string `json:"backend_type"`
}

type vmState struct {
	State string `json:"state"`
}
