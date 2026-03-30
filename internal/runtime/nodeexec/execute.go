package nodeexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/theg1239/lecrev/internal/artifact"
)

type Request struct {
	AttemptID      string
	JobID          string
	FunctionID     string
	Entrypoint     string
	ArtifactBundle []byte
	Payload        json.RawMessage
	Env            map[string]string
	Timeout        time.Duration
	Region         string
	HostID         string
	NodeBinary     string
}

type WorkspaceRequest struct {
	AttemptID  string
	JobID      string
	FunctionID string
	Workspace  string
	Entrypoint string
	Payload    json.RawMessage
	Env        map[string]string
	Timeout    time.Duration
	Region     string
	HostID     string
	NodeBinary string
}

type Result struct {
	ExitCode   int
	Logs       string
	Output     json.RawMessage
	StartedAt  time.Time
	FinishedAt time.Time
}

func PrepareWorkspace(artifactBundle []byte, workspace string) error {
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return err
	}
	return artifact.ExtractTarGz(artifactBundle, workspace)
}

func ExecuteBundle(ctx context.Context, req Request) (*Result, error) {
	workspace, err := os.MkdirTemp("", "lecrev-run-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(workspace)

	if err := PrepareWorkspace(req.ArtifactBundle, workspace); err != nil {
		return nil, err
	}

	return ExecuteWorkspace(ctx, WorkspaceRequest{
		AttemptID:  req.AttemptID,
		JobID:      req.JobID,
		FunctionID: req.FunctionID,
		Workspace:  workspace,
		Entrypoint: req.Entrypoint,
		Payload:    req.Payload,
		Env:        req.Env,
		Timeout:    req.Timeout,
		Region:     req.Region,
		HostID:     req.HostID,
		NodeBinary: req.NodeBinary,
	})
}

func ExecuteWorkspace(ctx context.Context, req WorkspaceRequest) (*Result, error) {
	startedAt := time.Now().UTC()

	scratchDir, err := os.MkdirTemp("", "lecrev-nodeexec-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratchDir)

	payloadPath := filepath.Join(scratchDir, "__lecrev_payload.json")
	resultPath := filepath.Join(scratchDir, "__lecrev_result.json")
	wrapperPath := filepath.Join(scratchDir, "__lecrev_invoke.mjs")

	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage(`null`)
	}
	if err := os.WriteFile(payloadPath, req.Payload, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(wrapperPath, []byte(wrapperScript), 0o644); err != nil {
		return nil, err
	}

	if req.NodeBinary == "" {
		req.NodeBinary = "node"
	}

	invokeCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	entrypoint := filepath.Join(req.Workspace, filepath.FromSlash(req.Entrypoint))
	if _, err := os.Stat(entrypoint); err != nil {
		return nil, fmt.Errorf("entrypoint %s: %w", req.Entrypoint, err)
	}
	contextJSON, err := invocationContextJSON(req)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(invokeCtx, req.NodeBinary, wrapperPath, payloadPath, entrypoint, resultPath)
	cmd.Dir = req.Workspace
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = commandEnv(req.Env, contextJSON)

	runErr := cmd.Run()
	logParts := make([]string, 0, 2)
	if stdout.Len() > 0 {
		logParts = append(logParts, strings.TrimSpace(stdout.String()))
	}
	if stderr.Len() > 0 {
		logParts = append(logParts, strings.TrimSpace(stderr.String()))
	}
	logs := strings.TrimSpace(strings.Join(logParts, "\n"))

	if invokeCtx.Err() == context.DeadlineExceeded {
		return &Result{
			ExitCode:   -1,
			Logs:       logs,
			StartedAt:  startedAt,
			FinishedAt: time.Now().UTC(),
		}, fmt.Errorf("execution timed out after %s", req.Timeout)
	}

	if runErr != nil {
		return &Result{
			ExitCode:   exitCode(runErr),
			Logs:       logs,
			StartedAt:  startedAt,
			FinishedAt: time.Now().UTC(),
		}, runErr
	}

	output, err := os.ReadFile(resultPath)
	if err != nil {
		return nil, fmt.Errorf("read result: %w", err)
	}

	return &Result{
		ExitCode:   0,
		Logs:       logs,
		Output:     output,
		StartedAt:  startedAt,
		FinishedAt: time.Now().UTC(),
	}, nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

func invocationContextJSON(req WorkspaceRequest) ([]byte, error) {
	return json.Marshal(map[string]any{
		"attemptId":  req.AttemptID,
		"jobId":      req.JobID,
		"functionId": req.FunctionID,
		"region":     req.Region,
		"hostId":     req.HostID,
	})
}

func commandEnv(env map[string]string, contextJSON []byte) []string {
	cmdEnv := append(os.Environ(), "LECREV_CONTEXT="+string(contextJSON))
	for key, value := range env {
		cmdEnv = append(cmdEnv, fmt.Sprintf("%s=%s", key, value))
	}
	return cmdEnv
}

const wrapperScript = `import fs from 'node:fs/promises';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const [payloadPath, entrypointPath, resultPath] = process.argv.slice(2);
const payload = JSON.parse(await fs.readFile(payloadPath, 'utf8'));
const context = JSON.parse(process.env.LECREV_CONTEXT ?? '{}');
const mod = await import(pathToFileURL(path.resolve(entrypointPath)).href);
if (typeof mod.handler !== 'function') {
  throw new Error('entrypoint must export an async handler(event, context)');
}
const result = await mod.handler(payload, context);
await fs.writeFile(resultPath, JSON.stringify(result ?? null));
`
