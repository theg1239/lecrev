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

	requestCh := make(chan firecracker.GuestInvocationRequest, 1)
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

		var request firecracker.GuestInvocationRequest
		if err := json.NewDecoder(reader).Decode(&request); err != nil {
			return
		}
		requestCh <- request
		_ = json.NewEncoder(serverConn).Encode(&firecracker.GuestInvocationResponse{
			ExitCode:   0,
			Logs:       "guest log",
			Output:     json.RawMessage(`{"ok":true}`),
			StartedAt:  time.Now().UTC(),
			FinishedAt: time.Now().UTC(),
		})
	}()

	response, err := invokeGuestWithDialer(context.Background(), func(context.Context) (net.Conn, error) {
		return clientConn, nil
	}, 5005, firecracker.GuestInvocationRequest{
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
	})
	if err != nil {
		t.Fatalf("invoke guest: %v", err)
	}
	if response.ExitCode != 0 || string(response.Output) != `{"ok":true}` {
		t.Fatalf("unexpected guest response: %+v", response)
	}

	select {
	case request := <-requestCh:
		if request.JobID != "job-1" {
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
