package nodeexec

import (
	"bufio"
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
	"sync"
	"time"

	"github.com/theg1239/lecrev/internal/firecracker"
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
	Type            string                 `json:"type"`
	Payload         json.RawMessage        `json:"payload,omitempty"`
	Context         map[string]any         `json:"context,omitempty"`
	Env             map[string]string      `json:"env,omitempty"`
	EnableStreaming bool                   `json:"enableStreaming,omitempty"`
	StreamKind      firecracker.StreamKind `json:"streamKind,omitempty"`
}

type preparedWorkerResponse struct {
	Type      string                       `json:"type,omitempty"`
	Ready     bool                         `json:"ready,omitempty"`
	Logs      string                       `json:"logs,omitempty"`
	Output    json.RawMessage              `json:"output,omitempty"`
	Error     string                       `json:"error,omitempty"`
	HTTPEvent *firecracker.HTTPStreamEvent `json:"httpEvent,omitempty"`
}

type preparedWorkerController struct {
	functionID string
	socketPath string
	cmd        *exec.Cmd
	conn       *net.UnixConn
	reader     *bufio.Reader
	waitCh     chan error

	mu       sync.Mutex
	stderrMu sync.Mutex
	stderr   bytes.Buffer
}

const (
	preparedWorkerResponseTypeReady     = "ready"
	preparedWorkerResponseTypeResult    = "result"
	preparedWorkerResponseTypeHTTPEvent = "http_event"
)

var preparedWorkers sync.Map

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
	_ = ShutdownPreparedWorker(context.Background(), req.FunctionID)

	compileCacheDir, err := ensureCompileCacheDir(req.Workspace, filepath.Join(os.TempDir(), "lecrev-worker-cache", sanitizeID(req.FunctionID)))
	if err != nil {
		return err
	}

	cmd := exec.Command(req.NodeBinary, workerScriptPath, socketPath, entrypoint)
	cmd.Dir = os.TempDir()
	cmd.Env = preparedWorkerEnv(req.Env, compileCacheDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	controller := &preparedWorkerController{
		functionID: req.FunctionID,
		socketPath: socketPath,
		cmd:        cmd,
		waitCh:     make(chan error, 1),
	}
	go controller.captureOutput(stdout, &controller.stderr)
	go controller.captureOutput(stderr, &controller.stderr)
	go controller.wait()

	readyCtx, cancel := context.WithTimeout(ctx, req.StartupTimeout)
	defer cancel()
	if err := connectPreparedWorker(readyCtx, controller); err != nil {
		controller.terminate()
		logs := strings.TrimSpace(controller.stderrLogs())
		if logs != "" {
			return fmt.Errorf("start prepared worker: %w; logs: %s", err, logs)
		}
		return fmt.Errorf("start prepared worker: %w", err)
	}
	preparedWorkers.Store(req.FunctionID, controller)
	return nil
}

func ExecutePreparedWorker(ctx context.Context, req WorkspaceRequest) (*Result, error) {
	if req.HTTPStream != nil {
		return ExecutePreparedWorkerStream(ctx, req)
	}
	startedAt := time.Now().UTC()
	trace := timetrace.New()

	contextStarted := time.Now()
	contextValue := invocationContext(req)
	trace.Step("build_context", contextStarted)

	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage(`null`)
	}

	workerInvokeStarted := time.Now()
	response, err := preparedWorkerRequest(ctx, req.FunctionID, preparedWorkerMessage{
		Type:            "invoke",
		Payload:         req.Payload,
		Context:         contextValue,
		Env:             req.Env,
		EnableStreaming: req.HTTPStream != nil,
		StreamKind:      firecracker.StreamKindHTTP,
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
	if controller, ok := loadPreparedWorker(functionID); ok {
		controller.close()
		preparedWorkers.Delete(functionID)
	}
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

func pingPreparedWorker(ctx context.Context, functionID string) (bool, error) {
	response, err := preparedWorkerRequest(ctx, functionID, preparedWorkerMessage{Type: "ping"})
	if err != nil {
		return false, err
	}
	return response.Ready, nil
}

func ExecutePreparedWorkerStream(ctx context.Context, req WorkspaceRequest) (*Result, error) {
	startedAt := time.Now().UTC()
	trace := timetrace.New()

	contextStarted := time.Now()
	contextValue := invocationContext(req)
	trace.Step("build_context", contextStarted)

	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage(`null`)
	}

	workerInvokeStarted := time.Now()
	response, err := preparedWorkerStreamingRequest(ctx, req.FunctionID, preparedWorkerMessage{
		Type:            "invoke",
		Payload:         req.Payload,
		Context:         contextValue,
		Env:             req.Env,
		EnableStreaming: true,
		StreamKind:      firecracker.StreamKindHTTP,
	}, req.HTTPStream)
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

func ReleasePreparedWorkerConnection(functionID string) {
	controller, ok := loadPreparedWorker(functionID)
	if !ok {
		return
	}
	controller.closeConnection()
}

func preparedWorkerRequest(ctx context.Context, functionID string, request preparedWorkerMessage) (*preparedWorkerResponse, error) {
	if strings.TrimSpace(functionID) == "" {
		return nil, fmt.Errorf("%w: functionID is required", ErrPreparedWorkerUnavailable)
	}
	controller, ok := loadPreparedWorker(functionID)
	if !ok {
		return nil, fmt.Errorf("%w: missing prepared worker controller", ErrPreparedWorkerUnavailable)
	}
	response, err := controller.request(ctx, request)
	if err == nil {
		return response, nil
	}
	controller.closeConnection()
	if reconnectErr := controller.openConnection(ctx); reconnectErr == nil {
		response, retryErr := controller.request(ctx, request)
		if retryErr == nil {
			return response, nil
		}
		err = retryErr
	}
	preparedWorkers.Delete(functionID)
	controller.close()
	return nil, err
}

func preparedWorkerStreamingRequest(ctx context.Context, functionID string, request preparedWorkerMessage, sink func(firecracker.HTTPStreamEvent) error) (*preparedWorkerResponse, error) {
	if strings.TrimSpace(functionID) == "" {
		return nil, fmt.Errorf("%w: functionID is required", ErrPreparedWorkerUnavailable)
	}
	controller, ok := loadPreparedWorker(functionID)
	if !ok {
		return nil, fmt.Errorf("%w: missing prepared worker controller", ErrPreparedWorkerUnavailable)
	}
	response, err := controller.requestStream(ctx, request, sink)
	if err == nil {
		return response, nil
	}
	controller.closeConnection()
	if reconnectErr := controller.openConnection(ctx); reconnectErr == nil {
		response, retryErr := controller.requestStream(ctx, request, sink)
		if retryErr == nil {
			return response, nil
		}
		err = retryErr
	}
	preparedWorkers.Delete(functionID)
	controller.close()
	return nil, err
}

func loadPreparedWorker(functionID string) (*preparedWorkerController, bool) {
	value, ok := preparedWorkers.Load(functionID)
	if !ok {
		return nil, false
	}
	controller, ok := value.(*preparedWorkerController)
	return controller, ok
}

func connectPreparedWorker(ctx context.Context, controller *preparedWorkerController) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		if err := controller.openConnection(ctx); err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			response, pingErr := controller.request(pingCtx, preparedWorkerMessage{Type: "ping"})
			cancel()
			if pingErr == nil && response.Ready {
				return nil
			}
			controller.closeConnection()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case procErr := <-controller.waitCh:
			if procErr == nil {
				return fmt.Errorf("%w: prepared worker exited before becoming ready", ErrPreparedWorkerUnavailable)
			}
			return fmt.Errorf("%w: prepared worker exited: %v", ErrPreparedWorkerUnavailable, procErr)
		case <-ticker.C:
		}
	}
}

func (c *preparedWorkerController) openConnection(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return nil
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("%w: dial prepared worker: %v", ErrPreparedWorkerUnavailable, err)
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return fmt.Errorf("%w: prepared worker did not return a unix connection", ErrPreparedWorkerUnavailable)
	}
	c.conn = unixConn
	c.reader = bufio.NewReader(unixConn)
	return nil
}

func (c *preparedWorkerController) request(ctx context.Context, request preparedWorkerMessage) (*preparedWorkerResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return nil, fmt.Errorf("%w: prepared worker control connection is not ready", ErrPreparedWorkerUnavailable)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(deadline)
	} else {
		_ = c.conn.SetDeadline(time.Time{})
	}
	if err := json.NewEncoder(c.conn).Encode(request); err != nil {
		c.closeConnectionLocked()
		return nil, fmt.Errorf("%w: send prepared worker request: %v", ErrPreparedWorkerUnavailable, err)
	}

	var response preparedWorkerResponse
	if err := json.NewDecoder(c.reader).Decode(&response); err != nil {
		c.closeConnectionLocked()
		return nil, fmt.Errorf("%w: read prepared worker response: %v", ErrPreparedWorkerUnavailable, err)
	}
	return &response, nil
}

func (c *preparedWorkerController) requestStream(ctx context.Context, request preparedWorkerMessage, sink func(firecracker.HTTPStreamEvent) error) (*preparedWorkerResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return nil, fmt.Errorf("%w: prepared worker control connection is not ready", ErrPreparedWorkerUnavailable)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(deadline)
	} else {
		_ = c.conn.SetDeadline(time.Time{})
	}
	if err := json.NewEncoder(c.conn).Encode(request); err != nil {
		c.closeConnectionLocked()
		return nil, fmt.Errorf("%w: send prepared worker request: %v", ErrPreparedWorkerUnavailable, err)
	}

	decoder := json.NewDecoder(c.reader)
	for {
		var response preparedWorkerResponse
		if err := decoder.Decode(&response); err != nil {
			c.closeConnectionLocked()
			return nil, fmt.Errorf("%w: read prepared worker response: %v", ErrPreparedWorkerUnavailable, err)
		}
		if response.Type == preparedWorkerResponseTypeHTTPEvent {
			if sink != nil && response.HTTPEvent != nil {
				if err := sink(*response.HTTPEvent); err != nil {
					return nil, err
				}
			}
			continue
		}
		return &response, nil
	}
}

func (c *preparedWorkerController) closeConnection() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeConnectionLocked()
}

func (c *preparedWorkerController) closeConnectionLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
		c.reader = nil
	}
}

func (c *preparedWorkerController) close() {
	c.closeConnection()
}

func (c *preparedWorkerController) terminate() {
	c.close()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

func (c *preparedWorkerController) wait() {
	err := c.cmd.Wait()
	c.waitCh <- err
}

func (c *preparedWorkerController) captureOutput(stream interface{ Read([]byte) (int, error) }, dest *bytes.Buffer) {
	var buffer [4096]byte
	for {
		n, err := stream.Read(buffer[:])
		if n > 0 {
			c.stderrMu.Lock()
			dest.Write(buffer[:n])
			c.stderrMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (c *preparedWorkerController) stderrLogs() string {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	return c.stderr.String()
}

func preparedWorkerSocketPath(functionID string) string {
	sum := sha256.Sum256([]byte(functionID))
	return filepath.Join(os.TempDir(), "lecrev-worker-"+hex.EncodeToString(sum[:8])+".sock")
}

func preparedWorkerEnv(env map[string]string, compileCacheDir string) []string {
	cmdEnv := append([]string(nil), os.Environ()...)
	if strings.TrimSpace(compileCacheDir) != "" {
		cmdEnv = append(cmdEnv, "NODE_COMPILE_CACHE="+compileCacheDir)
	}
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

let activeLogs = null;

function appendLog(text) {
  if (!activeLogs) {
    return;
  }
  activeLogs.push(text);
}

for (const method of ['log', 'info', 'warn', 'error', 'debug']) {
  console[method] = (...args) => {
    appendLog(args.map(formatValue).join(' ') + '\n');
  };
}

for (const stream of [process.stdout, process.stderr]) {
  stream.write = ((original) => (chunk, encoding, callback) => {
    const text = Buffer.isBuffer(chunk)
      ? chunk.toString(typeof encoding === 'string' ? encoding : undefined)
      : String(chunk ?? '');
    appendLog(text);
    if (typeof encoding === 'function') {
      encoding();
    }
    if (typeof callback === 'function') {
      callback();
    }
    return true;
  })(stream.write.bind(stream));
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

function handleRequest(socket, protocolWrite, request) {
  const emit = (value) => {
    protocolWrite(JSON.stringify(value) + '\n');
  };
  const createHttpStream = () => {
    if (!request.enableStreaming || request.streamKind !== 'http') {
      return null;
    }
    let started = false;
    let ended = false;
    const normalizeChunk = (chunk) => {
      if (chunk == null) {
        return Buffer.alloc(0);
      }
      if (typeof chunk === 'string') {
        return Buffer.from(chunk);
      }
      if (chunk instanceof Uint8Array) {
        return Buffer.from(chunk);
      }
      if (ArrayBuffer.isView(chunk)) {
        return Buffer.from(chunk.buffer, chunk.byteOffset, chunk.byteLength);
      }
      if (chunk instanceof ArrayBuffer) {
        return Buffer.from(chunk);
      }
      return Buffer.from(String(chunk));
    };
    return {
      get active() {
        return started;
      },
      get closed() {
        return ended;
      },
      async start(init = {}) {
        if (ended) {
          throw new Error('http stream is already closed');
        }
        if (started) {
          return;
        }
        started = true;
        emit({
          type: 'http_event',
          httpEvent: {
            type: 'http_start',
            statusCode: Number.isInteger(init?.statusCode) ? init.statusCode : 200,
            headers: init?.headers ?? {},
          },
        });
      },
      async write(chunk) {
        if (!started) {
          await this.start();
        }
        if (ended) {
          throw new Error('http stream is already closed');
        }
        const buffer = normalizeChunk(chunk);
        if (buffer.length === 0) {
          return;
        }
        emit({
          type: 'http_event',
          httpEvent: {
            type: 'http_chunk',
            chunk: buffer.toString('base64'),
          },
        });
      },
      async end(chunk) {
        if (ended) {
          return;
        }
        if (chunk !== undefined) {
          await this.write(chunk);
        } else if (!started) {
          await this.start();
        }
        emit({
          type: 'http_event',
          httpEvent: {
            type: 'http_end',
          },
        });
        ended = true;
      },
    };
  };

  if (request.type === 'ping') {
    emit({ type: 'ready', ready: true });
    return false;
  }
  if (request.type === 'shutdown') {
    emit({ type: 'ready', ready: false });
    server.close(() => process.exit(0));
    return true;
  }
  if (request.type !== 'invoke') {
    emit({ type: 'result', error: 'unsupported prepared worker request' });
    return false;
  }

  const logs = [];
  const previousLogs = activeLogs;
  activeLogs = logs;
  const hasEnvOverrides = request.env && Object.keys(request.env).length > 0;
  const restoreEnv = hasEnvOverrides ? applyEnv(request.env) : null;
  const stream = createHttpStream();
  const context = stream ? { ...(request.context ?? {}), stream } : (request.context ?? {});

  return Promise.resolve()
    .then(() => mod.handler(request.payload ?? null, context))
    .then((output) => {
      if (stream?.active && !stream.closed) {
        return stream.end().then(() => output);
      }
      return output;
    })
    .then((output) => {
      emit({ type: 'result', output: output ?? null, logs: logs.join('') });
      return false;
    })
    .catch((error) => {
      emit({ type: 'result', error: error?.stack ?? String(error), logs: logs.join('') });
      return false;
    })
    .finally(() => {
      activeLogs = previousLogs;
      if (restoreEnv) {
        restoreEnv();
      }
    });
}

const server = net.createServer({ allowHalfOpen: true }, (socket) => {
  let raw = '';
  let processing = Promise.resolve(false);
  const protocolWrite = socket.write.bind(socket);
  socket.setEncoding('utf8');

  function enqueue(rawRequest) {
    processing = processing.then(async (shouldClose) => {
      if (shouldClose) {
        return true;
      }
      let request;
      try {
        request = rawRequest.trim() === '' ? { type: 'ping' } : JSON.parse(rawRequest);
      } catch (error) {
        protocolWrite(JSON.stringify({ error: error?.stack ?? String(error) }) + '\n');
        return false;
      }
      return await handleRequest(socket, protocolWrite, request);
    });
  }

  socket.on('data', (chunk) => {
    raw += chunk;
    for (;;) {
      const newlineIndex = raw.indexOf('\n');
      if (newlineIndex === -1) {
        return;
      }
      const requestRaw = raw.slice(0, newlineIndex);
      raw = raw.slice(newlineIndex + 1);
      enqueue(requestRaw);
    }
  });

  socket.on('end', () => {
    if (raw.trim() !== '') {
      enqueue(raw);
      raw = '';
    }
  });
});

server.listen(socketPath);
`
