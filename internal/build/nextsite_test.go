package build

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theg1239/lecrev/internal/artifact"
	"github.com/theg1239/lecrev/internal/runtime/nodeexec"
)

func TestMaybePrepareNextSiteProducesRunnableHandler(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	serverRoot := filepath.Join(root, ".next", "standalone")
	staticRoot := filepath.Join(root, ".next", "static")
	publicRoot := filepath.Join(root, "public")

	for _, dir := range []string{serverRoot, staticRoot, publicRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(serverRoot, "server.js"), []byte(`
const http = require('http');
const host = process.env.HOSTNAME || '127.0.0.1';
const port = Number(process.env.PORT || '3000');
http.createServer((req, res) => {
  res.statusCode = 200;
  res.setHeader('Content-Type', 'text/html; charset=utf-8');
  res.setHeader('X-Upstream-Path', req.url);
  res.end('<html><body>site:' + req.url + '</body></html>');
}).listen(port, host);
`), 0o644); err != nil {
		t.Fatalf("write server.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staticRoot, "chunk.js"), []byte("console.log('chunk');"), 0o644); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(publicRoot, "robots.txt"), []byte("User-agent: *"), 0o644); err != nil {
		t.Fatalf("write robots.txt: %v", err)
	}

	site, err := maybePrepareNextSite(root, nil)
	if err != nil {
		t.Fatalf("maybePrepareNextSite: %v", err)
	}
	if site == nil {
		t.Fatal("expected next site to be detected")
	}
	if site.Entrypoint != nextSiteHandlerEntrypoint {
		t.Fatalf("expected generated entrypoint %q, got %q", nextSiteHandlerEntrypoint, site.Entrypoint)
	}

	bundle, err := artifact.BundleFromDirectory(site.BundleRoot, map[string][]byte{
		"function.json": []byte(`{"entrypoint":"` + site.Entrypoint + `"}`),
		"startup.json":  []byte(`{"entrypoint":"` + site.Entrypoint + `"}`),
	})
	if err != nil {
		t.Fatalf("bundle next site: %v", err)
	}

	payload, err := json.Marshal(map[string]any{
		"request": map[string]any{
			"method":     "GET",
			"scheme":     "https",
			"host":       "preview.lecrev.test",
			"path":       "/hello",
			"rawQuery":   "q=1",
			"headers":    map[string]string{"accept": "text/html"},
			"bodyText":   "",
			"bodyBase64": "",
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	result, err := nodeexec.ExecuteBundle(context.Background(), nodeexec.Request{
		AttemptID:      "attempt-next",
		JobID:          "job-next",
		FunctionID:     "fn-next",
		Entrypoint:     site.Entrypoint,
		ArtifactBundle: bundle,
		Payload:        payload,
		Env:            map[string]string{"LECREV_NEXT_READY_TIMEOUT_MS": "5000"},
		Timeout:        10 * time.Second,
		Region:         "ap-south-1",
		HostID:         "host-test",
	})
	if err != nil {
		t.Fatalf("execute next site bundle: %v", err)
	}

	var response struct {
		StatusCode int               `json:"statusCode"`
		Headers    map[string]string `json:"headers"`
		Body       string            `json:"body"`
	}
	if err := json.Unmarshal(result.Output, &response); err != nil {
		t.Fatalf("decode result output: %v", err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", response.StatusCode)
	}
	if got := response.Headers["x-upstream-path"]; got != "/hello?q=1" {
		t.Fatalf("expected upstream path /hello?q=1, got %q", got)
	}
	if !strings.Contains(response.Body, "site:/hello?q=1") {
		t.Fatalf("unexpected body %q", response.Body)
	}
}

func TestMaybePrepareNextSiteSurfacesStartupLogs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	serverRoot := filepath.Join(root, ".next", "standalone")
	if err := os.MkdirAll(serverRoot, 0o755); err != nil {
		t.Fatalf("mkdir standalone root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(serverRoot, "server.js"), []byte(`
console.error('next boot failed: missing runtime input');
process.exit(1);
`), 0o644); err != nil {
		t.Fatalf("write failing server.js: %v", err)
	}

	site, err := maybePrepareNextSite(root, nil)
	if err != nil {
		t.Fatalf("maybePrepareNextSite: %v", err)
	}
	if site == nil {
		t.Fatal("expected next site to be detected")
	}

	bundle, err := artifact.BundleFromDirectory(site.BundleRoot, map[string][]byte{
		"function.json": []byte(`{"entrypoint":"` + site.Entrypoint + `"}`),
		"startup.json":  []byte(`{"entrypoint":"` + site.Entrypoint + `"}`),
	})
	if err != nil {
		t.Fatalf("bundle next site: %v", err)
	}

	payload, err := json.Marshal(map[string]any{
		"request": map[string]any{
			"method":   "GET",
			"scheme":   "https",
			"host":     "preview.lecrev.test",
			"path":     "/",
			"rawQuery": "",
			"headers":  map[string]string{"accept": "text/html"},
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	result, execErr := nodeexec.ExecuteBundle(context.Background(), nodeexec.Request{
		AttemptID:      "attempt-next-fail",
		JobID:          "job-next-fail",
		FunctionID:     "fn-next-fail",
		Entrypoint:     site.Entrypoint,
		ArtifactBundle: bundle,
		Payload:        payload,
		Env:            map[string]string{"LECREV_NEXT_READY_TIMEOUT_MS": "5000"},
		Timeout:        10 * time.Second,
		Region:         "ap-south-1",
		HostID:         "host-test",
	})
	if execErr == nil {
		t.Fatal("expected execute next site bundle to fail")
	}
	if result == nil {
		t.Fatal("expected result on startup failure")
	}
	if !strings.Contains(result.Logs, "next boot failed: missing runtime input") {
		t.Fatalf("expected startup logs in result, got %q", result.Logs)
	}
}
