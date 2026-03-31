package microvm

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theg1239/lecrev/internal/firecracker"
)

func TestConfigValidateRequiresAssets(t *testing.T) {
	t.Parallel()

	_, err := New(Config{})
	if err == nil {
		t.Fatal("expected config validation error")
	}
}

func TestBootArgsIncludeGuestRunnerAndNetwork(t *testing.T) {
	t.Parallel()

	driver, err := New(Config{
		FirecrackerBinary: "/usr/bin/firecracker",
		KernelImagePath:   "/var/lib/lecrev/vmlinux",
		RootFSPath:        "/var/lib/lecrev/rootfs.ext4",
		TapDevice:         "tap0",
		GuestIP:           "172.16.0.2",
		GatewayIP:         "172.16.0.1",
		Netmask:           "255.255.255.252",
	})
	if err != nil {
		t.Fatalf("new driver: %v", err)
	}
	args, err := driver.bootArgs(firecracker.ExecuteRequest{NetworkPolicy: "full"})
	if err != nil {
		t.Fatalf("build boot args: %v", err)
	}
	for _, want := range []string{
		"init=/usr/local/bin/lecrev-guest-runner",
		"lecrev.vsock_port=5005",
		"ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("expected boot args to contain %q, got %q", want, args)
		}
	}
}

func TestInvokeGuestUsesFirecrackerVsockHandshake(t *testing.T) {
	t.Parallel()

	requestCh := make(chan firecracker.GuestRequest, 1)
	serverConn, clientConn := net.Pipe()
	go func() {
		defer serverConn.Close()

		reader := bufio.NewReader(serverConn)
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if strings.TrimSpace(line) != "CONNECT 5005" {
			return
		}
		_, _ = serverConn.Write([]byte("OK 5005\n"))

		var request firecracker.GuestRequest
		if err := json.NewDecoder(reader).Decode(&request); err != nil {
			return
		}
		requestCh <- request
		_ = json.NewEncoder(serverConn).Encode(&firecracker.GuestResponse{
			Invocation: &firecracker.GuestInvocationResponse{
				ExitCode:   0,
				Logs:       "guest log",
				Output:     json.RawMessage(`{"ok":true}`),
				StartedAt:  time.Now().UTC(),
				FinishedAt: time.Now().UTC(),
			},
		})
	}()

	response, err := guestRequestWithDialer(context.Background(), func(context.Context) (net.Conn, error) {
		return clientConn, nil
	}, 5005, firecracker.GuestRequest{
		Action: firecracker.GuestActionExecute,
		Invocation: &firecracker.GuestInvocationRequest{
			AttemptID:      "attempt-1",
			JobID:          "job-1",
			FunctionID:     "fn-1",
			Entrypoint:     "index.mjs",
			ArtifactBundle: []byte("bundle"),
			Payload:        json.RawMessage(`{"hello":"world"}`),
			Env:            map[string]string{"A": "B"},
			TimeoutMillis:  1000,
			Region:         "ap-south-1",
			HostID:         "host-ap-south-1-a",
		},
	})
	if err != nil {
		t.Fatalf("invoke guest: %v", err)
	}
	if response.Invocation == nil || response.Invocation.ExitCode != 0 || string(response.Invocation.Output) != `{"ok":true}` {
		t.Fatalf("unexpected guest response: %+v", response)
	}

	select {
	case request := <-requestCh:
		if request.Invocation == nil || request.Invocation.JobID != "job-1" {
			t.Fatalf("expected guest request to include job id, got %+v", request)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for guest request")
	}
}

func TestStageKernelFallsBackToCopy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	target := filepath.Join(dir, "nested", "target")
	if err := os.WriteFile(source, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir target dir: %v", err)
	}
	if err := stageKernel(target, source); err != nil {
		t.Fatalf("stage kernel: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(raw) != "kernel" {
		t.Fatalf("expected copied kernel contents, got %q", string(raw))
	}
}

func TestWarmInventoryReturnsPreparedSnapshots(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	driver, err := New(Config{
		FirecrackerBinary: "/usr/bin/firecracker",
		KernelImagePath:   filepath.Join(dir, "vmlinux"),
		RootFSPath:        filepath.Join(dir, "rootfs.ext4"),
		WorkspaceDir:      dir,
		SnapshotDir:       filepath.Join(dir, "snapshots"),
	})
	if err != nil {
		t.Fatalf("new driver: %v", err)
	}

	asset := driver.functionSnapshotPath("fn-1")
	if err := os.MkdirAll(asset.dir, 0o755); err != nil {
		t.Fatalf("mkdir asset: %v", err)
	}
	for _, path := range []string{asset.statePath, asset.memoryPath, asset.rootFSPath} {
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatalf("write asset file %s: %v", path, err)
		}
	}
	if err := os.WriteFile(asset.metadataPath, []byte(`{"kind":"function","functionId":"fn-1"}`), 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	blank := driver.blankSnapshotAsset()
	if err := os.MkdirAll(blank.dir, 0o755); err != nil {
		t.Fatalf("mkdir blank asset: %v", err)
	}
	for _, path := range []string{blank.statePath, blank.memoryPath, blank.rootFSPath} {
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatalf("write blank asset file %s: %v", path, err)
		}
	}
	if err := os.WriteFile(blank.metadataPath, []byte(`{"kind":"blank","networkPolicy":"none","memoryMb":128}`), 0o644); err != nil {
		t.Fatalf("write blank metadata: %v", err)
	}

	inventory := driver.WarmInventory()
	if inventory.BlankWarm != 1 {
		t.Fatalf("expected blank warm inventory to be 1, got %d", inventory.BlankWarm)
	}
	if inventory.FunctionWarm["fn-1"] != 1 {
		t.Fatalf("expected function warm inventory for fn-1, got %+v", inventory.FunctionWarm)
	}
}

func TestBlankSnapshotAssetForRequestRequiresMatchingShape(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	driver, err := New(Config{
		FirecrackerBinary: "/usr/bin/firecracker",
		KernelImagePath:   filepath.Join(dir, "vmlinux"),
		RootFSPath:        filepath.Join(dir, "rootfs.ext4"),
		WorkspaceDir:      dir,
		SnapshotDir:       filepath.Join(dir, "snapshots"),
	})
	if err != nil {
		t.Fatalf("new driver: %v", err)
	}

	blank := driver.blankSnapshotAsset()
	if err := os.MkdirAll(blank.dir, 0o755); err != nil {
		t.Fatalf("mkdir blank asset: %v", err)
	}
	for _, path := range []string{blank.statePath, blank.memoryPath, blank.rootFSPath} {
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatalf("write blank asset file %s: %v", path, err)
		}
	}
	if err := os.WriteFile(blank.metadataPath, []byte(`{"kind":"blank","networkPolicy":"none","memoryMb":128}`), 0o644); err != nil {
		t.Fatalf("write blank metadata: %v", err)
	}

	if _, ok := driver.blankSnapshotAssetForRequest(firecracker.ExecuteRequest{NetworkPolicy: "none", MemoryMB: 128}); !ok {
		t.Fatal("expected matching blank snapshot to be usable")
	}
	if _, ok := driver.blankSnapshotAssetForRequest(firecracker.ExecuteRequest{NetworkPolicy: "full", MemoryMB: 128}); ok {
		t.Fatal("expected network mismatch to reject blank snapshot")
	}
	if _, ok := driver.blankSnapshotAssetForRequest(firecracker.ExecuteRequest{NetworkPolicy: "none", MemoryMB: 256}); ok {
		t.Fatal("expected memory mismatch to reject blank snapshot")
	}
}

func TestGuestInvocationRequestOmitsArtifactBundleForPreparedRoot(t *testing.T) {
	t.Parallel()

	req := firecracker.ExecuteRequest{
		AttemptID:      "attempt-1",
		JobID:          "job-1",
		FunctionID:     "fn-1",
		Entrypoint:     "index.mjs",
		ArtifactBundle: []byte("bundle-bytes"),
		Payload:        json.RawMessage(`{"ok":true}`),
		Env:            map[string]string{"A": "B"},
		Timeout:        5 * time.Second,
		Region:         "ap-south-1",
		HostID:         "host-ap-south-1-a",
	}

	prepared := guestInvocationRequest(req, true)
	if len(prepared.ArtifactBundle) != 0 {
		t.Fatalf("expected prepared-root invocation to omit artifact bundle, got %d bytes", len(prepared.ArtifactBundle))
	}
	if !prepared.UsePreparedRoot {
		t.Fatal("expected prepared-root invocation flag to be set")
	}

	cold := guestInvocationRequest(req, false)
	if string(cold.ArtifactBundle) != "bundle-bytes" {
		t.Fatalf("expected cold invocation to keep artifact bundle, got %q", string(cold.ArtifactBundle))
	}
	if cold.UsePreparedRoot {
		t.Fatal("expected cold invocation flag to be unset")
	}
}
