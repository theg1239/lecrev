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
	"time"

	"github.com/theg1239/lecrev/internal/firecracker"
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
	layout *vmLayout
	proc   *vmProcess
	api    *apiClient
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

func (d *Driver) executeCold(ctx context.Context, req firecracker.ExecuteRequest) (*firecracker.ExecuteResult, error) {
	instance, err := d.bootInstance(ctx, req)
	if err != nil {
		return nil, err
	}
	defer instance.cleanup()
	return d.runInvocation(ctx, instance, req, false)
}

func (d *Driver) executeFromSnapshot(ctx context.Context, req firecracker.ExecuteRequest, asset snapshotAsset, usePreparedRoot bool) (*firecracker.ExecuteResult, error) {
	instance, err := d.restoreInstance(ctx, req, asset)
	if err != nil {
		return nil, err
	}
	defer instance.cleanup()
	return d.runInvocation(ctx, instance, req, usePreparedRoot)
}

func (d *Driver) runInvocation(ctx context.Context, instance *vmInstance, req firecracker.ExecuteRequest, usePreparedRoot bool) (*firecracker.ExecuteResult, error) {
	invokeCtx, cancel := context.WithTimeout(ctx, d.config.ConnectTimeout+req.Timeout)
	defer cancel()

	response, err := invokeGuest(invokeCtx, instance.layout.vsockSocketHost, d.config.GuestVSockPort, firecracker.GuestInvocationRequest{
		AttemptID:       req.AttemptID,
		JobID:           req.JobID,
		FunctionID:      req.FunctionID,
		Entrypoint:      req.Entrypoint,
		ArtifactBundle:  req.ArtifactBundle,
		UsePreparedRoot: usePreparedRoot,
		Payload:         req.Payload,
		Env:             req.Env,
		TimeoutMillis:   req.Timeout.Milliseconds(),
		Region:          req.Region,
		HostID:          req.HostID,
	})

	_ = instance.proc.wait(d.config.ShutdownTimeout)

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

func (d *Driver) bootInstance(ctx context.Context, req firecracker.ExecuteRequest) (*vmInstance, error) {
	layout, vmID, err := d.prepareLayout(req.AttemptID)
	if err != nil {
		return nil, err
	}
	if err := stageKernel(layout.kernelPath, d.config.KernelImagePath); err != nil {
		layout.cleanup()
		return nil, err
	}
	if err := copyFile(d.config.RootFSPath, layout.rootFSPath); err != nil {
		layout.cleanup()
		return nil, err
	}
	if err := d.prepareOwnership(layout, layout.kernelPath, layout.rootFSPath); err != nil {
		layout.cleanup()
		return nil, err
	}

	proc, err := d.startVM(ctx, vmID, layout)
	if err != nil {
		layout.cleanup()
		return nil, err
	}
	if err := waitForFile(ctx, layout.apiSocketHost, d.config.StartTimeout, proc); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("wait for firecracker api socket: %w; logs: %s", err, proc.logs())
	}

	api := newAPIClient(layout.apiSocketHost)
	guestCID := d.allocCID()
	bootArgs, err := d.bootArgs(req)
	if err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, err
	}
	if err := api.put(ctx, "/machine-config", machineConfig{
		VCPUCount:  d.config.VCPUCount,
		MemSizeMib: int64(req.MemoryMB),
		Smt:        false,
	}); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("configure machine: %w; logs: %s", err, proc.logs())
	}
	if err := api.put(ctx, "/boot-source", bootSource{
		KernelImagePath: layout.guestKernelPath,
		BootArgs:        bootArgs,
	}); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("configure boot source: %w; logs: %s", err, proc.logs())
	}
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
	if strings.EqualFold(req.NetworkPolicy, "full") {
		if err := api.put(ctx, "/network-interfaces/eth0", networkInterface{
			IfaceID:     "eth0",
			HostDevName: d.config.TapDevice,
			GuestMAC:    d.config.GuestMAC,
		}); err != nil {
			layout.cleanup()
			_ = proc.close()
			return nil, fmt.Errorf("configure network interface: %w; logs: %s", err, proc.logs())
		}
	}
	if err := api.put(ctx, "/vsock", vsockDevice{
		VsockID:  "vsock0",
		GuestCID: guestCID,
		UDSPath:  layout.guestVSockPath,
	}); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("configure vsock: %w; logs: %s", err, proc.logs())
	}
	if err := api.put(ctx, "/actions", action{ActionType: "InstanceStart"}); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("start vm: %w; logs: %s", err, proc.logs())
	}

	return &vmInstance{layout: layout, proc: proc, api: api}, nil
}

func (d *Driver) restoreInstance(ctx context.Context, req firecracker.ExecuteRequest, asset snapshotAsset) (*vmInstance, error) {
	layout, vmID, err := d.prepareLayout(req.AttemptID)
	if err != nil {
		return nil, err
	}

	instanceStatePath := filepath.Join(layout.rootDir, "snapshot.state")
	instanceMemoryPath := filepath.Join(layout.rootDir, "snapshot.mem")
	if err := copyFile(asset.statePath, instanceStatePath); err != nil {
		layout.cleanup()
		return nil, err
	}
	if err := copyFile(asset.memoryPath, instanceMemoryPath); err != nil {
		layout.cleanup()
		return nil, err
	}
	if err := copyFile(asset.rootFSPath, layout.rootFSPath); err != nil {
		layout.cleanup()
		return nil, err
	}
	if err := d.prepareOwnership(layout, instanceStatePath, instanceMemoryPath, layout.rootFSPath); err != nil {
		layout.cleanup()
		return nil, err
	}

	proc, err := d.startVM(ctx, vmID, layout)
	if err != nil {
		layout.cleanup()
		return nil, err
	}
	if err := waitForFile(ctx, layout.apiSocketHost, d.config.StartTimeout, proc); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("wait for firecracker api socket: %w; logs: %s", err, proc.logs())
	}

	api := newAPIClient(layout.apiSocketHost)
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
	if err := api.patch(ctx, "/vm", vmState{State: "Resumed"}); err != nil {
		layout.cleanup()
		_ = proc.close()
		return nil, fmt.Errorf("resume snapshot vm: %w; logs: %s", err, proc.logs())
	}
	return &vmInstance{layout: layout, proc: proc, api: api}, nil
}

func (v *vmInstance) cleanup() {
	if v == nil {
		return
	}
	if v.proc != nil {
		_ = v.proc.close()
	}
	if v.layout != nil && v.layout.cleanup != nil {
		v.layout.cleanup()
	}
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
	instance, err := d.bootInstance(ctx, req)
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
				instance, err = d.restoreInstance(ctx, req, blankAsset)
			}
		}
	}
	if instance == nil {
		instance, err = d.bootInstance(ctx, req)
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
