package localnode

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

	"github.com/ishaan/eeeverc/internal/artifact"
	"github.com/ishaan/eeeverc/internal/firecracker"
)

type Driver struct{}

func New() *Driver {
	return &Driver{}
}

func (d *Driver) Execute(ctx context.Context, req firecracker.ExecuteRequest) (*firecracker.ExecuteResult, error) {
	startedAt := time.Now().UTC()

	workspace, err := os.MkdirTemp("", "eeeverc-run-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(workspace)

	if err := artifact.ExtractTarGz(req.ArtifactBundle, workspace); err != nil {
		return nil, err
	}

	payloadPath := filepath.Join(workspace, "__eeeverc_payload.json")
	resultPath := filepath.Join(workspace, "__eeeverc_result.json")
	wrapperPath := filepath.Join(workspace, "__eeeverc_invoke.mjs")

	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage(`null`)
	}
	if err := os.WriteFile(payloadPath, req.Payload, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(wrapperPath, []byte(wrapperScript), 0o644); err != nil {
		return nil, err
	}

	invokeCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	entrypoint := filepath.Join(workspace, filepath.FromSlash(req.Entrypoint))
	contextJSON, err := json.Marshal(map[string]any{
		"attemptId":  req.AttemptID,
		"jobId":      req.JobID,
		"functionId": req.FunctionID,
		"region":     req.Region,
		"hostId":     req.HostID,
	})
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(invokeCtx, "node", wrapperPath, payloadPath, entrypoint, resultPath)
	cmd.Dir = workspace
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "EEEVERC_CONTEXT="+string(contextJSON))
	for key, value := range req.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", key, value))
	}

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
		return &firecracker.ExecuteResult{
			ExitCode:         -1,
			Logs:             logs,
			SnapshotEligible: false,
			StartedAt:        startedAt,
			FinishedAt:       time.Now().UTC(),
		}, fmt.Errorf("execution timed out after %s", req.Timeout)
	}

	if runErr != nil {
		return &firecracker.ExecuteResult{
			ExitCode:         exitCode(runErr),
			Logs:             logs,
			SnapshotEligible: false,
			StartedAt:        startedAt,
			FinishedAt:       time.Now().UTC(),
		}, runErr
	}

	output, err := os.ReadFile(resultPath)
	if err != nil {
		return nil, fmt.Errorf("read result: %w", err)
	}

	return &firecracker.ExecuteResult{
		ExitCode:         0,
		Logs:             logs,
		Output:           output,
		SnapshotEligible: true,
		StartedAt:        startedAt,
		FinishedAt:       time.Now().UTC(),
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

const wrapperScript = `import fs from 'node:fs/promises';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const [payloadPath, entrypointPath, resultPath] = process.argv.slice(2);
const payload = JSON.parse(await fs.readFile(payloadPath, 'utf8'));
const context = JSON.parse(process.env.EEEVERC_CONTEXT ?? '{}');
const mod = await import(pathToFileURL(path.resolve(entrypointPath)).href);
if (typeof mod.handler !== 'function') {
  throw new Error('entrypoint must export an async handler(event, context)');
}
const result = await mod.handler(payload, context);
await fs.writeFile(resultPath, JSON.stringify(result ?? null));
`
