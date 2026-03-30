package nodeexec

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestExecuteWorkspaceUsesScratchOutsideWorkspace(t *testing.T) {
	t.Parallel()

	nodeBinary, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available")
	}

	workspace := t.TempDir()
	entrypoint := filepath.Join(workspace, "index.mjs")
	if err := os.WriteFile(entrypoint, []byte(`export async function handler() { return { ok: true }; }`), 0o444); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}
	defer func() {
		_ = os.Chmod(workspace, 0o755)
	}()
	if err := os.Chmod(workspace, 0o555); err != nil {
		t.Fatalf("chmod workspace: %v", err)
	}

	result, err := ExecuteWorkspace(context.Background(), WorkspaceRequest{
		AttemptID:  "attempt-1",
		JobID:      "job-1",
		FunctionID: "fn-1",
		Workspace:  workspace,
		Entrypoint: "index.mjs",
		Timeout:    5 * time.Second,
		NodeBinary: nodeBinary,
	})
	if err != nil {
		t.Fatalf("execute workspace: %v", err)
	}
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("unexpected output: %s", string(result.Output))
	}
}

func TestPreparedWorkerReusesPreloadedHandler(t *testing.T) {
	t.Parallel()

	nodeBinary, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available")
	}

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "index.mjs"), []byte(`
const loadedAt = Date.now();
const topLevelEnv = process.env.TOP_LEVEL_ENV ?? null;

export async function handler(event, context) {
  console.log("hello", event?.name ?? "unknown");
  return {
    loadedAt,
    topLevelEnv,
    runtimeEnv: process.env.RUNTIME_ENV ?? null,
    hostId: context.hostId ?? null,
    name: event?.name ?? null
  };
}
`), 0o644); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}

	functionID := sanitizeID(t.Name())
	if err := StartPreparedWorker(context.Background(), PrepareWorkerRequest{
		FunctionID:     functionID,
		Workspace:      workspace,
		Entrypoint:     "index.mjs",
		Env:            map[string]string{"TOP_LEVEL_ENV": "booted"},
		NodeBinary:     nodeBinary,
		StartupTimeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("start prepared worker: %v", err)
	}
	defer func() {
		_ = ShutdownPreparedWorker(context.Background(), functionID)
	}()

	first, err := ExecutePreparedWorker(context.Background(), WorkspaceRequest{
		AttemptID:  "attempt-1",
		JobID:      "job-1",
		FunctionID: functionID,
		Workspace:  workspace,
		Entrypoint: "index.mjs",
		Payload:    json.RawMessage(`{"name":"first"}`),
		Env:        map[string]string{"RUNTIME_ENV": "run-1"},
		Timeout:    5 * time.Second,
		Region:     "ap-south-1",
		HostID:     "host-ap-south-1-a",
		NodeBinary: nodeBinary,
	})
	if err != nil {
		t.Fatalf("execute prepared worker first: %v", err)
	}
	if first.Logs == "" {
		t.Fatalf("expected captured logs from prepared worker")
	}

	second, err := ExecutePreparedWorker(context.Background(), WorkspaceRequest{
		AttemptID:  "attempt-2",
		JobID:      "job-2",
		FunctionID: functionID,
		Workspace:  workspace,
		Entrypoint: "index.mjs",
		Payload:    json.RawMessage(`{"name":"second"}`),
		Env:        map[string]string{"RUNTIME_ENV": "run-2"},
		Timeout:    5 * time.Second,
		Region:     "ap-south-1",
		HostID:     "host-ap-south-1-a",
		NodeBinary: nodeBinary,
	})
	if err != nil {
		t.Fatalf("execute prepared worker second: %v", err)
	}

	firstOutput := decodeResultMap(t, first.Output)
	secondOutput := decodeResultMap(t, second.Output)

	if firstOutput["topLevelEnv"] != "booted" || secondOutput["topLevelEnv"] != "booted" {
		t.Fatalf("expected top-level env to be captured at worker boot, got %#v / %#v", firstOutput["topLevelEnv"], secondOutput["topLevelEnv"])
	}
	if firstOutput["runtimeEnv"] != "run-1" || secondOutput["runtimeEnv"] != "run-2" {
		t.Fatalf("expected runtime env overrides per invocation, got %#v / %#v", firstOutput["runtimeEnv"], secondOutput["runtimeEnv"])
	}
	if firstOutput["loadedAt"] != secondOutput["loadedAt"] {
		t.Fatalf("expected worker to reuse the preloaded module, got %#v / %#v", firstOutput["loadedAt"], secondOutput["loadedAt"])
	}
}

func decodeResultMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return decoded
}
