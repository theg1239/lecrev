package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theg1239/lecrev/internal/artifact"
	"github.com/theg1239/lecrev/internal/devstack"
	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/regions"
)

func TestRunEmbeddedStackEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stack, err := devstack.StartEmbedded(ctx, devstack.Config{
		LoadEnv:          false,
		ExecutionRegions: append([]string(nil), regions.DefaultExecutionRegions...),
		SecretsBackend:   "memory",
	})
	if err != nil {
		t.Fatalf("start embedded stack: %v", err)
	}
	defer stack.Close()

	result, err := Run(ctx, Config{
		BaseURL:   "http://embedded.lecrev",
		APIKey:    stack.APIKey,
		ProjectID: "smoke-test",
		Regions:   append([]string(nil), stack.Regions...),
		Client:    NewHandlerClient(stack.Handler),
		Timeout:   45 * time.Second,
	})
	if err != nil {
		t.Fatalf("run smoke: %v", err)
	}
	if result.Job.Result == nil {
		t.Fatal("expected terminal job result")
	}
	if result.Job.Result.Region == "" {
		t.Fatal("expected result region to be recorded")
	}
	if strings.TrimSpace(result.Job.Result.LogsKey) == "" {
		t.Fatal("expected archived logs key to be recorded")
	}
	if strings.TrimSpace(result.Job.Result.OutputKey) == "" {
		t.Fatal("expected archived output key to be recorded")
	}
	if len(result.Attempts) == 0 {
		t.Fatal("expected at least one attempt")
	}
	logs, err := stack.Objects.Get(ctx, result.Job.Result.LogsKey)
	if err != nil {
		t.Fatalf("get archived logs: %v", err)
	}
	output, err := stack.Objects.Get(ctx, result.Job.Result.OutputKey)
	if err != nil {
		t.Fatalf("get archived output: %v", err)
	}
	if len(output) == 0 {
		t.Fatal("expected archived output content")
	}
	if string(logs) != result.Job.Result.Logs {
		t.Fatalf("expected archived logs %q, got %q", result.Job.Result.Logs, string(logs))
	}
	if string(output) != string(result.Job.Result.Output) {
		t.Fatalf("expected archived output %s, got %s", string(result.Job.Result.Output), string(output))
	}
	lastAttempt := result.Attempts[len(result.Attempts)-1]
	if got := artifact.ExecutionLogsKey(result.Job.ID, lastAttempt.ID); result.Job.Result.LogsKey != got {
		t.Fatalf("expected logs key %s, got %s", got, result.Job.Result.LogsKey)
	}
	if len(result.Costs) == 0 {
		t.Fatal("expected at least one cost record")
	}

	select {
	case err := <-stack.Errors():
		t.Fatalf("embedded stack returned async error: %v", err)
	default:
	}
}

func TestRunEmbeddedGitSourceEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	stack, err := devstack.StartEmbedded(ctx, devstack.Config{
		LoadEnv:          false,
		ExecutionRegions: append([]string(nil), regions.DefaultExecutionRegions...),
		SecretsBackend:   "memory",
	})
	if err != nil {
		t.Fatalf("start embedded stack: %v", err)
	}
	defer stack.Close()

	repoURL := createGitRepo(t, map[string]string{
		"package.json": `{
  "name": "smoke-git-echo",
  "type": "module",
  "scripts": {
    "build": "node scripts/build.mjs"
  }
}
`,
		"scripts/build.mjs": `import fs from 'node:fs/promises';
import path from 'node:path';

const root = process.cwd();
await fs.mkdir(path.join(root, 'dist'), { recursive: true });
await fs.copyFile(path.join(root, 'src/index.mjs'), path.join(root, 'dist/index.mjs'));
`,
		"src/index.mjs": `export async function handler(event, context) {
  return { ok: true, echo: event, region: context.region, hostId: context.hostId };
}
`,
	})

	client := NewHandlerClient(stack.Handler)
	deployReq := map[string]any{
		"name":           "smoke-git-echo",
		"runtime":        "node22",
		"entrypoint":     "dist/index.mjs",
		"memoryMb":       128,
		"timeoutSec":     10,
		"networkPolicy":  "full",
		"regions":        append([]string(nil), stack.Regions...),
		"maxRetries":     1,
		"idempotencyKey": "deploy-git-smoke",
		"source": map[string]any{
			"type":   "git",
			"gitUrl": repoURL,
		},
	}
	var version domain.FunctionVersion
	if err := doJSON(ctx, client, "http://embedded.lecrev", stack.APIKey, http.MethodPost, "/v1/projects/smoke-git/functions", deployReq, http.StatusCreated, &version); err != nil {
		t.Fatalf("deploy git function: %v", err)
	}
	if version.BuildJobID == "" {
		t.Fatal("expected build job id for git deploy")
	}
	_, version, err = waitForBuildReady(ctx, Config{
		BaseURL:      "http://embedded.lecrev",
		APIKey:       stack.APIKey,
		Client:       client,
		PollInterval: 100 * time.Millisecond,
	}, version.ID, version.BuildJobID)
	if err != nil {
		t.Fatalf("wait for git build ready: %v", err)
	}

	payload := json.RawMessage(`{"hello":"git"}`)
	var job domain.ExecutionJob
	if err := doJSON(ctx, client, "http://embedded.lecrev", stack.APIKey, http.MethodPost, "/v1/functions/"+version.ID+"/invoke", map[string]any{
		"payload": payload,
	}, http.StatusAccepted, &job); err != nil {
		t.Fatalf("invoke git function: %v", err)
	}
	job, err = waitForJob(ctx, Config{
		BaseURL:      "http://embedded.lecrev",
		APIKey:       stack.APIKey,
		Client:       client,
		PollInterval: 100 * time.Millisecond,
	}, job.ID)
	if err != nil {
		t.Fatalf("wait for git job: %v", err)
	}
	if job.Result == nil {
		t.Fatal("expected terminal git job result")
	}
	if err := validateOutput(job.Result.Output, payload); err != nil {
		t.Fatalf("validate git output: %v", err)
	}
	archivedOutput, err := stack.Objects.Get(ctx, job.Result.OutputKey)
	if err != nil {
		t.Fatalf("get archived git output: %v", err)
	}
	if !bytes.Equal(archivedOutput, job.Result.Output) {
		t.Fatalf("expected archived output %s, got %s", string(job.Result.Output), string(archivedOutput))
	}

	select {
	case err := <-stack.Errors():
		t.Fatalf("embedded stack returned async error: %v", err)
	default:
	}
}

func createGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()

	repoDir := t.TempDir()
	for name, content := range files {
		target := filepath.Join(repoDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", target, err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", target, err)
		}
	}

	runTestCommand(t, "", "git", "init", "--initial-branch=main", repoDir)
	runTestCommand(t, repoDir, "git", "add", ".")
	runTestCommand(t, repoDir, "git", "-c", "user.name=lecrev-test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	return (&url.URL{Scheme: "file", Path: repoDir}).String()
}

func runTestCommand(t *testing.T, dir, name string, args ...string) {
	t.Helper()

	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %v: %s", name, args, err, string(output))
	}
}
