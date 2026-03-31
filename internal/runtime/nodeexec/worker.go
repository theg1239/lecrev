package nodeexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/theg1239/lecrev/internal/timetrace"
)

var ErrPreparedWorkerUnavailable = errors.New("prepared worker unavailable")

type PrepareWorkerRequest struct {
	FunctionID     string
	Workspace      string
	Entrypoint     string
	Env            map[string]string
	NodeBinary     string
	StartupTimeout time.Duration
}

type preparedWorkerMessage struct {
	Type    string            `json:"type"`
	Payload json.RawMessage   `json:"payload,omitempty"`
	Context map[string]any    `json:"context,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type preparedWorkerResponse struct {
	Ready  bool            `json:"ready,omitempty"`
	Logs   string          `json:"logs,omitempty"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func StartPreparedWorker(ctx context.Context, req PrepareWorkerRequest) error {
	if strings.TrimSpace(req.FunctionID) == "" {
		return fmt.Errorf("functionID is required")
	}
	if strings.TrimSpace(req.Workspace) == "" {
		return fmt.Errorf("workspace is required")
	}
	if strings.TrimSpace(req.Entrypoint) == "" {
		return fmt.Errorf("entrypoint is required")
	}
	if req.NodeBinary == "" {
		req.NodeBinary = "node"
	}
	if req.StartupTimeout <= 0 {
		req.StartupTimeout = 10 * time.Second
	}

	entrypoint := filepath.Join(req.Workspace, filepath.FromSlash(req.Entrypoint))
	if _, err := os.Stat(entrypoint); err != nil {
		return fmt.Errorf("entrypoint %s: %w", req.Entrypoint, err)
	}

	workerScriptPath := filepath.Join(req.Workspace, "__lecrev_worker.mjs")
	if err := os.WriteFile(workerScriptPath, []byte(preparedWorkerScript), 0o644); err != nil {
		return err
	}

	socketPath := preparedWorkerSocketPath(req.FunctionID)
	_ = os.Remove(socketPath)

	cmd := exec.Command(req.NodeBinary, workerScriptPath, socketPath, entrypoint)
	cmd.Dir = os.TempDir()
	cmd.Env = preparedWorkerEnv(req.Env)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	readyCtx, cancel := context.WithTimeout(ctx, req.StartupTimeout)
	defer cancel()
	if err := waitForPreparedWorker(readyCtx, req.FunctionID, waitCh); err != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-waitCh:
		default:
		}
		logs := strings.TrimSpace(strings.Join([]string{
			strings.TrimSpace(stdout.String()),
			strings.TrimSpace(stderr.String()),
		}, "\n"))
		if logs != "" {
			return fmt.Errorf("start prepared worker: %w; logs: %s", err, logs)
		}
		return fmt.Errorf("start prepared worker: %w", err)
	}
	return nil
}

func ExecutePreparedWorker(ctx context.Context, req WorkspaceRequest) (*Result, error) {
	startedAt := time.Now().UTC()
	trace := timetrace.New()

	contextStarted := time.Now()
	contextJSON, err := invocationContextJSON(req)
	if err != nil {
		return nil, err
	}
	var contextValue map[string]any
	if err := json.Unmarshal(contextJSON, &contextValue); err != nil {
		return nil, err
	}
	trace.Step("build_context", contextStarted)

	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage(`null`)
	}

	workerInvokeStarted := time.Now()
	response, err := preparedWorkerRequest(ctx, req.FunctionID, preparedWorkerMessage{
		Type:    "invoke",
		Payload: req.Payload,
		Context: contextValue,
		Env:     req.Env,
	})
	trace.Step("prepared_worker_invoke", workerInvokeStarted)
	finishedAt := time.Now().UTC()
	if err != nil {
		return nil, err
	}

	result := &Result{
		ExitCode:      0,
		Logs:          strings.TrimSpace(response.Logs),
		Output:        normalizedWorkerOutput(response.Output),
		PlatformTrace: trace.String(),
		StartedAt:     startedAt,
		FinishedAt:    finishedAt,
	}
	if response.Error != "" {
		result.ExitCode = 1
		return result, errors.New(response.Error)
	}
	return result, nil
}

func ShutdownPreparedWorker(ctx context.Context, functionID string) error {
	response, err := preparedWorkerRequest(ctx, functionID, preparedWorkerMessage{Type: "shutdown"})
	if err != nil {
		if errors.Is(err, ErrPreparedWorkerUnavailable) {
			return nil
		}
		return err
	}
	if response != nil && response.Error != "" {
		return errors.New(response.Error)
	}
	return nil
}

func waitForPreparedWorker(ctx context.Context, functionID string, waitCh <-chan error) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		pingCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		ready, err := pingPreparedWorker(pingCtx, functionID)
		cancel()
		if err == nil && ready {
			return nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return err
			}
			return ctx.Err()
		case procErr := <-waitCh:
			if procErr == nil {
				return fmt.Errorf("%w: prepared worker exited before becoming ready", ErrPreparedWorkerUnavailable)
			}
			return fmt.Errorf("%w: prepared worker exited: %v", ErrPreparedWorkerUnavailable, procErr)
		case <-ticker.C:
		}
	}
}

func pingPreparedWorker(ctx context.Context, functionID string) (bool, error) {
	response, err := preparedWorkerRequest(ctx, functionID, preparedWorkerMessage{Type: "ping"})
	if err != nil {
		return false, err
	}
	return response.Ready, nil
}

func preparedWorkerRequest(ctx context.Context, functionID string, request preparedWorkerMessage) (*preparedWorkerResponse, error) {
	if strings.TrimSpace(functionID) == "" {
		return nil, fmt.Errorf("%w: functionID is required", ErrPreparedWorkerUnavailable)
	}
	socketPath := preparedWorkerSocketPath(functionID)
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("%w: dial prepared worker: %v", ErrPreparedWorkerUnavailable, err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return nil, fmt.Errorf("%w: send prepared worker request: %v", ErrPreparedWorkerUnavailable, err)
	}
	if unixConn, ok := conn.(*net.UnixConn); ok {
		_ = unixConn.CloseWrite()
	}

	var response preparedWorkerResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return nil, fmt.Errorf("%w: read prepared worker response: %v", ErrPreparedWorkerUnavailable, err)
	}
	return &response, nil
}

func preparedWorkerSocketPath(functionID string) string {
	sum := sha256.Sum256([]byte(functionID))
	return filepath.Join(os.TempDir(), "lecrev-worker-"+hex.EncodeToString(sum[:8])+".sock")
}

func preparedWorkerEnv(env map[string]string) []string {
	cmdEnv := append([]string(nil), os.Environ()...)
	for key, value := range env {
		cmdEnv = append(cmdEnv, fmt.Sprintf("%s=%s", key, value))
	}
	return cmdEnv
}

func normalizedWorkerOutput(output json.RawMessage) json.RawMessage {
	if len(output) == 0 {
		return json.RawMessage("null")
	}
	return output
}

func sanitizeID(input string) string {
	if input == "" {
		return "unknown"
	}
	var builder strings.Builder
	for _, r := range input {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	return builder.String()
}

const preparedWorkerScript = `import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import util from 'node:util';
import { pathToFileURL } from 'node:url';

const [socketPath, entrypointPath] = process.argv.slice(2);
try {
  fs.rmSync(socketPath, { force: true });
} catch {}

const mod = await import(pathToFileURL(path.resolve(entrypointPath)).href);
if (typeof mod.handler !== 'function') {
  throw new Error('entrypoint must export an async handler(event, context)');
}

function formatValue(value) {
  return typeof value === 'string'
    ? value
    : util.inspect(value, { depth: 6, colors: false, breakLength: Infinity });
}

function captureConsole(logs) {
  const originals = {};
  for (const method of ['log', 'info', 'warn', 'error', 'debug']) {
    originals[method] = console[method];
    console[method] = (...args) => {
      logs.push(args.map(formatValue).join(' ') + '\n');
    };
  }
  return () => {
    for (const method of Object.keys(originals)) {
      console[method] = originals[method];
    }
  };
}

function captureStream(stream, logs) {
  const original = stream.write.bind(stream);
  stream.write = (chunk, encoding, callback) => {
    const text = Buffer.isBuffer(chunk)
      ? chunk.toString(typeof encoding === 'string' ? encoding : undefined)
      : String(chunk ?? '');
    logs.push(text);
    if (typeof encoding === 'function') {
      encoding();
    }
    if (typeof callback === 'function') {
      callback();
    }
    return true;
  };
  return () => {
    stream.write = original;
  };
}

function applyEnv(overrides) {
  const previous = new Map();
  for (const [key, value] of Object.entries(overrides ?? {})) {
    previous.set(key, Object.prototype.hasOwnProperty.call(process.env, key) ? process.env[key] : undefined);
    process.env[key] = String(value);
  }
  return () => {
    for (const [key, value] of previous.entries()) {
      if (value === undefined) {
        delete process.env[key];
      } else {
        process.env[key] = value;
      }
    }
  };
}

const server = net.createServer({ allowHalfOpen: true }, (socket) => {
  let raw = '';
  socket.setEncoding('utf8');
  socket.on('data', (chunk) => {
    raw += chunk;
  });
  socket.on('end', async () => {
    let request;
    try {
      request = raw.trim() === '' ? { type: 'ping' } : JSON.parse(raw);
    } catch (error) {
      socket.end(JSON.stringify({ error: error?.stack ?? String(error) }));
      return;
    }

    if (request.type === 'ping') {
      socket.end(JSON.stringify({ ready: true }));
      return;
    }
    if (request.type === 'shutdown') {
      socket.end(JSON.stringify({ ready: false }));
      server.close(() => process.exit(0));
      return;
    }
    if (request.type !== 'invoke') {
      socket.end(JSON.stringify({ error: 'unsupported prepared worker request' }));
      return;
    }

    const logs = [];
    const restoreEnv = applyEnv(request.env);
    const restoreStdout = captureStream(process.stdout, logs);
    const restoreStderr = captureStream(process.stderr, logs);
    const restoreConsole = captureConsole(logs);

    try {
      const output = await mod.handler(request.payload ?? null, request.context ?? {});
      socket.end(JSON.stringify({ output: output ?? null, logs: logs.join('') }));
    } catch (error) {
      socket.end(JSON.stringify({ error: error?.stack ?? String(error), logs: logs.join('') }));
    } finally {
      restoreConsole();
      restoreStdout();
      restoreStderr();
      restoreEnv();
    }
  });
});

server.listen(socketPath);
`
