package build

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/theg1239/lecrev/internal/artifact"
	"github.com/theg1239/lecrev/internal/domain"
	memstore "github.com/theg1239/lecrev/internal/store/memory"
)

func TestCreateFunctionVersionDefaultsToAPACRegions(t *testing.T) {
	t.Parallel()

	svc := New(memstore.New(), artifact.NewMemoryStore())
	version, err := svc.CreateFunctionVersion(context.Background(), domain.DeployRequest{
		ProjectID:  "demo",
		Name:       "echo",
		Runtime:    "node22",
		Entrypoint: "index.mjs",
		Source: domain.DeploySource{
			Type: domain.SourceTypeBundle,
			InlineFiles: map[string]string{
				"index.mjs": "export async function handler() { return { ok: true }; }",
			},
		},
	})
	if err != nil {
		t.Fatalf("create function version: %v", err)
	}

	want := []string{"ap-south-1", "ap-south-2", "ap-southeast-1"}
	if len(version.Regions) != len(want) {
		t.Fatalf("expected %d regions, got %d", len(want), len(version.Regions))
	}
	for i := range want {
		if version.Regions[i] != want[i] {
			t.Fatalf("expected region[%d]=%s, got %s", i, want[i], version.Regions[i])
		}
	}
}

func TestCreateFunctionVersionRejectsUnsupportedRegion(t *testing.T) {
	t.Parallel()

	svc := New(memstore.New(), artifact.NewMemoryStore())
	_, err := svc.CreateFunctionVersion(context.Background(), domain.DeployRequest{
		ProjectID:  "demo",
		Name:       "echo",
		Runtime:    "node22",
		Entrypoint: "index.mjs",
		Regions:    []string{"us-east-1"},
		Source: domain.DeploySource{
			Type: domain.SourceTypeBundle,
			InlineFiles: map[string]string{
				"index.mjs": "export async function handler() { return { ok: true }; }",
			},
		},
	})
	if err == nil {
		t.Fatal("expected unsupported region error")
	}
}

func TestCreateFunctionVersionReplaysIdempotentDeploy(t *testing.T) {
	t.Parallel()

	svc := New(memstore.New(), artifact.NewMemoryStore())
	req := domain.DeployRequest{
		ProjectID:      "demo",
		Name:           "echo",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		IdempotencyKey: "deploy-key-1",
		Source: domain.DeploySource{
			Type: domain.SourceTypeBundle,
			InlineFiles: map[string]string{
				"index.mjs": "export async function handler() { return { ok: true }; }",
			},
		},
	}

	first, err := svc.CreateFunctionVersion(context.Background(), req)
	if err != nil {
		t.Fatalf("first create function version: %v", err)
	}
	second, err := svc.CreateFunctionVersion(context.Background(), req)
	if err != nil {
		t.Fatalf("second create function version: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected replayed version id %s, got %s", first.ID, second.ID)
	}
}

func TestCreateFunctionVersionRejectsIdempotencyKeyReuseWithDifferentPayload(t *testing.T) {
	t.Parallel()

	svc := New(memstore.New(), artifact.NewMemoryStore())
	req := domain.DeployRequest{
		ProjectID:      "demo",
		Name:           "echo",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		IdempotencyKey: "deploy-key-2",
		Source: domain.DeploySource{
			Type: domain.SourceTypeBundle,
			InlineFiles: map[string]string{
				"index.mjs": "export async function handler() { return { ok: true }; }",
			},
		},
	}

	if _, err := svc.CreateFunctionVersion(context.Background(), req); err != nil {
		t.Fatalf("first create function version: %v", err)
	}
	req.Entrypoint = "different.mjs"
	_, err := svc.CreateFunctionVersion(context.Background(), req)
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
}

func TestCreateFunctionVersionQueuesAsyncBuildAndMarksReady(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	bus := NewMemoryBus(16)
	svc := New(meta, objects)
	svc.SetBuildBus(bus)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		_ = svc.RunBuildConsumer(ctx, "ap-south-1", "builder-ap-south-1")
	}()

	version, err := svc.CreateFunctionVersion(ctx, domain.DeployRequest{
		ProjectID:  "demo",
		Name:       "echo",
		Runtime:    "node22",
		Entrypoint: "index.mjs",
		Regions:    []string{"ap-south-1"},
		Source: domain.DeploySource{
			Type: domain.SourceTypeBundle,
			InlineFiles: map[string]string{
				"index.mjs": "export async function handler() { return { ok: true }; }",
			},
		},
	})
	if err != nil {
		t.Fatalf("create function version: %v", err)
	}
	if version.State != domain.FunctionStateBuilding {
		t.Fatalf("expected initial state %s, got %s", domain.FunctionStateBuilding, version.State)
	}
	if version.BuildJobID == "" {
		t.Fatal("expected build job id to be set")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		latest, err := meta.GetFunctionVersion(context.Background(), version.ID)
		if err != nil {
			t.Fatalf("get function version: %v", err)
		}
		if latest.State == domain.FunctionStateReady {
			job, err := meta.GetBuildJob(context.Background(), version.BuildJobID)
			if err != nil {
				t.Fatalf("get build job: %v", err)
			}
			if job.State != "succeeded" {
				t.Fatalf("expected succeeded build job, got %s", job.State)
			}
			if latest.ArtifactDigest == "" {
				t.Fatal("expected artifact digest after async build")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for async build to complete")
}

func TestCreateFunctionVersionBuildsGitSourceRepository(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	svc := New(meta, objects)

	repoURL := createGitRepo(t, map[string]string{
		"package.json": `{
  "name": "git-echo",
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

	version, err := svc.CreateFunctionVersion(context.Background(), domain.DeployRequest{
		ProjectID:  "demo",
		Name:       "git-echo",
		Runtime:    "node22",
		Entrypoint: "dist/index.mjs",
		Regions:    []string{"ap-south-1"},
		Source: domain.DeploySource{
			Type:   domain.SourceTypeGit,
			GitURL: repoURL,
		},
	})
	if err != nil {
		t.Fatalf("create function version from git source: %v", err)
	}
	if version.State != domain.FunctionStateReady {
		t.Fatalf("expected ready function version, got %s", version.State)
	}
	if version.ArtifactDigest == "" {
		t.Fatal("expected artifact digest for git source build")
	}

	artifactMeta, err := meta.GetArtifact(context.Background(), version.ArtifactDigest)
	if err != nil {
		t.Fatalf("get artifact metadata: %v", err)
	}
	bundle, err := objects.Get(context.Background(), artifactMeta.BundleKey)
	if err != nil {
		t.Fatalf("get bundle: %v", err)
	}
	dest := t.TempDir()
	if err := artifact.ExtractTarGz(bundle, dest); err != nil {
		t.Fatalf("extract bundle: %v", err)
	}

	for _, required := range []string{"dist/index.mjs", "function.json", "startup.json"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(required))); err != nil {
			t.Fatalf("expected bundled file %s: %v", required, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected .git directory to be excluded from bundle, got %v", err)
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
