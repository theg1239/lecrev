package build

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/theg1239/lecrev/internal/domain"
)

const nextSiteHandlerEntrypoint = "__lecrev_next_site_handler.mjs"

type preparedBuildOutput struct {
	Bundle     []byte
	Startup    []byte
	Metadata   map[string]string
	Entrypoint string
	Cleanup    func()
	Site       *preparedNextSite
}

type preparedNextSite struct {
	Framework    string
	BundleRoot   string
	Entrypoint   string
	StaticAssets []nextStaticAsset
}

type nextStaticAsset struct {
	Path string
	Data []byte
}

func isNextWorkspace(pkg packageManifest) bool {
	for _, deps := range []map[string]string{pkg.Dependencies, pkg.DevDependencies} {
		if deps == nil {
			continue
		}
		if strings.TrimSpace(deps["next"]) != "" {
			return true
		}
	}
	return false
}

func maybePrepareNextSite(root string, recorder *buildRecorder) (*preparedNextSite, error) {
	serverRoot := filepath.Join(root, ".next", "standalone")
	serverEntrypoint := filepath.Join(serverRoot, "server.js")
	if _, statErr := os.Stat(serverEntrypoint); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat next standalone server: %w", statErr)
	}

	staticAssets, err := collectNextStaticAssets(root)
	if err != nil {
		return nil, fmt.Errorf("collect next static assets: %w", err)
	}
	if err := stageNextRuntimeAssets(root, serverRoot); err != nil {
		return nil, fmt.Errorf("stage next runtime assets: %w", err)
	}
	handlerPath := filepath.Join(serverRoot, nextSiteHandlerEntrypoint)
	if err := os.WriteFile(handlerPath, []byte(nextSiteHandlerScript), 0o644); err != nil {
		return nil, fmt.Errorf("write next site handler: %w", err)
	}
	if recorder != nil {
		recorder.Printf("detected next.js standalone build output at %s", serverRoot)
		recorder.Printf("staged next.js runtime assets into standalone bundle root")
		recorder.Printf("collected %d next.js static assets for site serving", len(staticAssets))
	}
	return &preparedNextSite{
		Framework:    "nextjs",
		BundleRoot:   serverRoot,
		Entrypoint:   nextSiteHandlerEntrypoint,
		StaticAssets: staticAssets,
	}, nil
}

func stageNextRuntimeAssets(root, serverRoot string) error {
	type stagedDir struct {
		source string
		target string
	}
	dirs := []stagedDir{
		{
			source: filepath.Join(root, ".next", "static"),
			target: filepath.Join(serverRoot, ".next", "static"),
		},
		{
			source: filepath.Join(root, "public"),
			target: filepath.Join(serverRoot, "public"),
		},
	}
	for _, dir := range dirs {
		if err := copyDirIfExists(dir.source, dir.target); err != nil {
			return err
		}
	}
	return nil
}

func copyDirIfExists(sourceDir, targetDir string) error {
	info, err := os.Stat(sourceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(targetDir, rel)
		if info.IsDir() {
			return os.MkdirAll(targetPath, info.Mode().Perm())
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return err
		}
		return copyFile(path, targetPath, info.Mode().Perm())
	})
}

func copyFile(sourcePath, targetPath string, mode os.FileMode) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()

	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer target.Close()

	if _, err := io.Copy(target, source); err != nil {
		return err
	}
	return nil
}

func collectNextStaticAssets(root string) ([]nextStaticAsset, error) {
	assets := make([]nextStaticAsset, 0, 128)
	appendDir := func(sourceDir, requestPrefix string) error {
		info, err := os.Stat(sourceDir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			return nil
		}
		return filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(sourceDir, path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			key := filepath.ToSlash(filepath.Join(requestPrefix, rel))
			if !strings.HasPrefix(key, "/") {
				key = "/" + key
			}
			assets = append(assets, nextStaticAsset{
				Path: key,
				Data: data,
			})
			return nil
		})
	}

	if err := appendDir(filepath.Join(root, ".next", "static"), "/_next/static"); err != nil {
		return nil, err
	}
	if err := appendDir(filepath.Join(root, "public"), "/"); err != nil {
		return nil, err
	}
	for _, faviconPath := range []string{
		filepath.Join(root, "app", "favicon.ico"),
		filepath.Join(root, "src", "app", "favicon.ico"),
	} {
		if data, err := os.ReadFile(faviconPath); err == nil {
			assets = append(assets, nextStaticAsset{Path: "/favicon.ico", Data: data})
			break
		}
	}
	return assets, nil
}

func encodeWebsiteManifest(versionID, staticPrefix, entrypoint string, createdAt time.Time) ([]byte, error) {
	manifest := domain.WebsiteManifest{
		Framework:         "nextjs",
		FunctionVersionID: versionID,
		StaticPrefix:      staticPrefix,
		DynamicEntrypoint: entrypoint,
		CreatedAt:         createdAt,
	}
	return json.MarshalIndent(manifest, "", "  ")
}

const nextSiteHandlerScript = `import fs from 'node:fs';
import { spawn } from 'node:child_process';
import http from 'node:http';
import net from 'node:net';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const workspaceRoot = path.dirname(fileURLToPath(import.meta.url));
const serverEntrypoint = path.join(workspaceRoot, 'server.js');
const configuredReadyTimeoutMs = Number(process.env.LECREV_NEXT_READY_TIMEOUT_MS || 30000);
const readyTimeoutMs = Number.isFinite(configuredReadyTimeoutMs) && configuredReadyTimeoutMs > 0
  ? configuredReadyTimeoutMs
  : 30000;
const logDir = path.join(workspaceRoot, '.lecrev-next-runtime');
const stdoutLogPath = path.join(logDir, 'server.stdout.log');
const stderrLogPath = path.join(logDir, 'server.stderr.log');
let bootPromise = null;
let serverState = null;

function childAlive(child) {
  return Boolean(child) && child.exitCode == null && !child.killed;
}

function normalizeHeaders(headers = {}) {
  const result = {};
  for (const [key, rawValue] of Object.entries(headers)) {
    if (rawValue == null) {
      continue;
    }
    const name = String(key).toLowerCase();
    if (['connection', 'transfer-encoding', 'keep-alive', 'upgrade', 'proxy-connection'].includes(name)) {
      continue;
    }
    result[name] = Array.isArray(rawValue) ? rawValue.join(', ') : String(rawValue);
  }
  return result;
}

function responseHeaders(headers = {}) {
  const result = {};
  for (const [key, rawValue] of Object.entries(headers)) {
    if (rawValue == null) {
      continue;
    }
    result[key] = Array.isArray(rawValue) ? rawValue.join(', ') : String(rawValue);
  }
  return result;
}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function ensureLogFile(filePath) {
  fs.mkdirSync(path.dirname(filePath), { recursive: true });
  fs.writeFileSync(filePath, '', { encoding: 'utf8' });
}

function readLogTail(filePath) {
  try {
    const content = fs.readFileSync(filePath, 'utf8');
    if (content.length <= 0) {
      return '';
    }
    const maxLength = 8192;
    if (content.length <= maxLength) {
      return content.trim();
    }
    return ('…' + content.slice(-maxLength)).trim();
  } catch {
    return '';
  }
}

function formatStartupLogs(stderrRef, stdoutRef) {
  const stderr = stderrRef();
  const stdout = stdoutRef();
  const parts = [];
  if (stderr !== '') {
    parts.push('stderr:\n' + stderr);
  }
  if (stdout !== '') {
    parts.push('stdout:\n' + stdout);
  }
  if (parts.length === 0) {
    return 'no stdout/stderr captured';
  }
  return parts.join('\n\n');
}

async function findAvailablePort() {
  return await new Promise((resolve, reject) => {
    const server = net.createServer();
    server.unref();
    server.on('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      const port = typeof address === 'object' && address ? address.port : 0;
      server.close((closeErr) => {
        if (closeErr) {
          reject(closeErr);
          return;
        }
        resolve(port);
      });
    });
  });
}

async function waitUntilReady(port, child, stderrRef, stdoutRef) {
  const startedAt = Date.now();
  while (Date.now() - startedAt < readyTimeoutMs) {
    if (!childAlive(child)) {
      throw new Error('next standalone server exited before becoming ready: ' + formatStartupLogs(stderrRef, stdoutRef));
    }
    try {
      await new Promise((resolve, reject) => {
        const socket = net.createConnection({ host: '127.0.0.1', port }, () => {
          socket.end();
          resolve();
        });
        socket.setTimeout(250);
        socket.on('timeout', () => socket.destroy(new Error('connect timeout')));
        socket.on('error', reject);
      });
      return;
    } catch {}
    await delay(50);
  }
  throw new Error('timed out waiting for next standalone server to listen: ' + formatStartupLogs(stderrRef, stdoutRef));
}

async function ensureServer() {
  if (serverState && childAlive(serverState.child)) {
    return serverState;
  }
  if (bootPromise) {
    return await bootPromise;
  }
  bootPromise = (async () => {
    const port = await findAvailablePort();
    ensureLogFile(stdoutLogPath);
    ensureLogFile(stderrLogPath);
    const stdoutFd = fs.openSync(stdoutLogPath, 'a');
    const stderrFd = fs.openSync(stderrLogPath, 'a');
    const child = spawn(process.execPath, [serverEntrypoint], {
      cwd: workspaceRoot,
      env: {
        ...process.env,
        PORT: String(port),
        HOSTNAME: '127.0.0.1',
        NODE_ENV: process.env.NODE_ENV || 'production',
      },
      stdio: ['ignore', stdoutFd, stderrFd],
    });
    try {
      fs.closeSync(stdoutFd);
    } catch {}
    try {
      fs.closeSync(stderrFd);
    } catch {}
    child.on('exit', () => {
      if (serverState?.child === child) {
        serverState = null;
      }
    });
    child.unref();
    await waitUntilReady(port, child, () => readLogTail(stderrLogPath), () => readLogTail(stdoutLogPath));
    serverState = {
      port,
      child,
      stderr: () => readLogTail(stderrLogPath),
      stdout: () => readLogTail(stdoutLogPath),
    };
    return serverState;
  })();
  try {
    return await bootPromise;
  } finally {
    bootPromise = null;
  }
}

function requestBody(request) {
  if (typeof request?.bodyBase64 === 'string' && request.bodyBase64 !== '') {
    return Buffer.from(request.bodyBase64, 'base64');
  }
  if (typeof request?.bodyText === 'string' && request.bodyText !== '') {
    return Buffer.from(request.bodyText);
  }
  return null;
}

function isTextResponse(headers) {
  const contentType = String(headers['content-type'] || headers['Content-Type'] || '').toLowerCase();
  if (contentType === '') {
    return true;
  }
  return (
    contentType.startsWith('text/') ||
    contentType.includes('json') ||
    contentType.includes('javascript') ||
    contentType.includes('xml') ||
    contentType.includes('svg') ||
    contentType.includes('x-www-form-urlencoded')
  );
}

async function proxyThroughNext(request, stream) {
  let attempt = 0;
  while (attempt < 2) {
    attempt += 1;
    const state = await ensureServer();
    try {
      return await proxyOnce(state, request, stream);
    } catch (error) {
      if (attempt >= 2) {
        throw error;
      }
      if (serverState?.child === state.child) {
        try {
          state.child.kill('SIGKILL');
        } catch {}
        serverState = null;
      }
    }
  }
  throw new Error('next proxy failed unexpectedly');
}

async function proxyOnce(state, request, stream) {
  const upstreamPath = (request?.path || '/') + (request?.rawQuery ? '?' + request.rawQuery : '');
  const headers = normalizeHeaders(request?.headers || {});
  headers.host = request?.host || request?.headers?.host || request?.headers?.Host || '127.0.0.1:' + state.port;
  headers['x-forwarded-host'] = request?.host || headers.host;
  headers['x-forwarded-proto'] = request?.scheme || 'https';
  headers['x-forwarded-port'] = request?.scheme === 'http' ? '80' : '443';
  const body = requestBody(request);
  if (body && !('content-length' in headers)) {
    headers['content-length'] = String(body.length);
  } else if (!body) {
    delete headers['content-length'];
  }

  return await new Promise((resolve, reject) => {
    const upstream = http.request({
      hostname: '127.0.0.1',
      port: state.port,
      method: request?.method || 'GET',
      path: upstreamPath,
      headers,
    }, async (response) => {
      try {
        const statusCode = response.statusCode || 200;
        const headerMap = responseHeaders(response.headers);
        if (stream) {
          await stream.start({ statusCode, headers: headerMap });
          for await (const chunk of response) {
            await stream.write(chunk);
          }
          await stream.end();
          resolve({
            statusCode,
            headers: headerMap,
            body: '',
          });
          return;
        }

        const chunks = [];
        for await (const chunk of response) {
          chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
        }
        const payload = Buffer.concat(chunks);
        if (isTextResponse(headerMap)) {
          resolve({
            statusCode,
            headers: headerMap,
            body: payload.toString('utf8'),
          });
          return;
        }
        resolve({
          statusCode,
          headers: headerMap,
          body: payload.toString('base64'),
          isBase64Encoded: true,
        });
      } catch (error) {
        reject(error);
      }
    });
    upstream.on('error', reject);
    if (body && body.length > 0) {
      upstream.write(body);
    }
    upstream.end();
  });
}

export async function handler(event, context) {
  const request = event?.request ?? {};
  const stream = context?.stream ?? null;
  return await proxyThroughNext(request, stream);
}
`
