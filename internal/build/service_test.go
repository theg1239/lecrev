package build

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	mrand "math/rand"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestCreateFunctionVersionRejectsInvalidAdmissionRequests(t *testing.T) {
	t.Parallel()

	svc := New(memstore.New(), artifact.NewMemoryStore())
	base := domain.DeployRequest{
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
	}

	tests := []struct {
		name string
		edit func(*domain.DeployRequest)
		want string
	}{
		{
			name: "unsupported runtime",
			edit: func(req *domain.DeployRequest) { req.Runtime = "python3.12" },
			want: "unsupported runtime",
		},
		{
			name: "allowlist network policy rejected",
			edit: func(req *domain.DeployRequest) { req.NetworkPolicy = domain.NetworkPolicyAllowlist },
			want: "not yet supported",
		},
		{
			name: "memory below minimum",
			edit: func(req *domain.DeployRequest) { req.MemoryMB = 32 },
			want: "memoryMb must be between",
		},
		{
			name: "timeout above maximum",
			edit: func(req *domain.DeployRequest) { req.TimeoutSec = maxTimeoutSec + 1 },
			want: "timeoutSec must be between",
		},
		{
			name: "too many retries",
			edit: func(req *domain.DeployRequest) { req.MaxRetries = maxRetries + 1 },
			want: "maxRetries must be between",
		},
		{
			name: "empty env ref",
			edit: func(req *domain.DeployRequest) { req.EnvRefs = []string{"DEMO_SECRET", ""} },
			want: "envRefs cannot contain empty values",
		},
		{
			name: "too many env refs",
			edit: func(req *domain.DeployRequest) {
				req.EnvRefs = make([]string, maxEnvRefs+1)
				for i := range req.EnvRefs {
					req.EnvRefs[i] = "SECRET_" + strings.Repeat("X", 1)
				}
			},
			want: "envRefs cannot exceed",
		},
		{
			name: "invalid env var key",
			edit: func(req *domain.DeployRequest) { req.EnvVars = map[string]string{"1INVALID": "value"} },
			want: "must start with a letter or underscore",
		},
		{
			name: "env var overlaps env ref",
			edit: func(req *domain.DeployRequest) {
				req.EnvVars = map[string]string{"PUBLIC_API_URL": "https://example.com"}
				req.EnvRefs = []string{"PUBLIC_API_URL"}
			},
			want: "cannot overlap envVars key",
		},
		{
			name: "too many env vars",
			edit: func(req *domain.DeployRequest) {
				req.EnvVars = make(map[string]string, maxEnvVars+1)
				for i := 0; i < maxEnvVars+1; i++ {
					req.EnvVars[fmt.Sprintf("KEY_%d", i)] = "value"
				}
			},
			want: "envVars cannot exceed",
		},
		{
			name: "bundle source without payload",
			edit: func(req *domain.DeployRequest) {
				req.Source = domain.DeploySource{Type: domain.SourceTypeBundle}
			},
			want: "bundle source requires",
		},
		{
			name: "git source without url",
			edit: func(req *domain.DeployRequest) {
				req.Source = domain.DeploySource{Type: domain.SourceTypeGit}
			},
			want: "gitUrl is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			req.Source = base.Source
			req.Source.InlineFiles = map[string]string{
				"index.mjs": "export async function handler() { return { ok: true }; }",
			}
			tc.edit(&req)

			_, err := svc.CreateFunctionVersion(context.Background(), req)
			if err == nil {
				t.Fatalf("expected admission error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
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

func TestDeployRequestHashIgnoresGitCredentials(t *testing.T) {
	t.Parallel()

	req := domain.DeployRequest{
		ProjectID:     "demo",
		Name:          "git-echo",
		Runtime:       "node22",
		Entrypoint:    "dist/index.mjs",
		MemoryMB:      256,
		TimeoutSec:    30,
		NetworkPolicy: domain.NetworkPolicyNone,
		Regions:       []string{"ap-south-1"},
		MaxRetries:    2,
		Source: domain.DeploySource{
			Type:   domain.SourceTypeGit,
			GitURL: "https://x-access-token:first-token@github.com/acme/widgets.git",
			GitRef: "main",
		},
	}

	withRotatedToken := req
	withRotatedToken.Source = req.Source
	withRotatedToken.Source.GitURL = "https://x-access-token:second-token@github.com/acme/widgets.git"

	firstHash, err := deployRequestHash(req)
	if err != nil {
		t.Fatalf("first deploy request hash: %v", err)
	}
	secondHash, err := deployRequestHash(withRotatedToken)
	if err != nil {
		t.Fatalf("second deploy request hash: %v", err)
	}
	if firstHash != secondHash {
		t.Fatalf("expected matching hashes for token-only git url changes, got %q and %q", firstHash, secondHash)
	}
}

func TestDeployRequestHashStillDiffersForDifferentGitRepos(t *testing.T) {
	t.Parallel()

	req := domain.DeployRequest{
		ProjectID:     "demo",
		Name:          "git-echo",
		Runtime:       "node22",
		Entrypoint:    "dist/index.mjs",
		MemoryMB:      256,
		TimeoutSec:    30,
		NetworkPolicy: domain.NetworkPolicyNone,
		Regions:       []string{"ap-south-1"},
		MaxRetries:    2,
		Source: domain.DeploySource{
			Type:   domain.SourceTypeGit,
			GitURL: "https://x-access-token:first-token@github.com/acme/widgets.git",
			GitRef: "main",
		},
	}

	differentRepo := req
	differentRepo.Source = req.Source
	differentRepo.Source.GitURL = "https://x-access-token:first-token@github.com/acme/other-widgets.git"

	firstHash, err := deployRequestHash(req)
	if err != nil {
		t.Fatalf("first deploy request hash: %v", err)
	}
	secondHash, err := deployRequestHash(differentRepo)
	if err != nil {
		t.Fatalf("second deploy request hash: %v", err)
	}
	if firstHash == secondHash {
		t.Fatalf("expected different hashes for different git repositories")
	}
}

func TestCreateFunctionVersionPersistsEnvVars(t *testing.T) {
	t.Parallel()

	svc := New(memstore.New(), artifact.NewMemoryStore())
	version, err := svc.CreateFunctionVersion(context.Background(), domain.DeployRequest{
		ProjectID:  "demo",
		Name:       "env-demo",
		Runtime:    "node22",
		Entrypoint: "index.mjs",
		EnvVars: map[string]string{
			"NEXT_PUBLIC_SITE_URL": "https://demo.example.com",
			"MEDIUM_FEED_URL":      "https://medium.com/gdg-vit?format=json",
		},
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
	if got := version.EnvVars["NEXT_PUBLIC_SITE_URL"]; got != "https://demo.example.com" {
		t.Fatalf("expected env var to round-trip, got %q", got)
	}
	if got := version.EnvVars["MEDIUM_FEED_URL"]; got == "" {
		t.Fatal("expected secondary env var to round-trip")
	}
}

func TestDetectPackageManagerRecognizesCommonLockfiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		packageManager string
		lockfiles      []string
		want           nodePackageManager
		wantErr        string
	}{
		{
			name:      "npm lockfile",
			lockfiles: []string{"package-lock.json"},
			want:      nodePackageManagerNPM,
		},
		{
			name:      "yarn lockfile",
			lockfiles: []string{"yarn.lock"},
			want:      nodePackageManagerYarn,
		},
		{
			name:      "pnpm lockfile",
			lockfiles: []string{"pnpm-lock.yaml"},
			want:      nodePackageManagerPNPM,
		},
		{
			name:           "explicit yarn package manager",
			packageManager: "yarn@4.2.0",
			want:           nodePackageManagerYarn,
		},
		{
			name:      "bun unsupported",
			lockfiles: []string{"bun.lockb"},
			wantErr:   "unsupported lockfile",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range tc.lockfiles {
				if err := os.WriteFile(filepath.Join(root, name), []byte("lock"), 0o644); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}
			got, err := detectPackageManager(root, tc.packageManager)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("detect package manager: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected package manager %q, got %q", tc.want, got)
			}
		})
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

func TestCreateFunctionVersionEnforcesActiveBuildQuota(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	svc := New(meta, artifact.NewMemoryStore())
	svc.maxActiveBuildJobsPerProject = 1

	now := time.Now().UTC()
	if err := meta.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-existing-build",
		ProjectID:      "demo",
		Name:           "existing",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		MemoryMB:       128,
		TimeoutSec:     30,
		NetworkPolicy:  domain.NetworkPolicyFull,
		Regions:        []string{"ap-south-1"},
		SourceType:     domain.SourceTypeBundle,
		ArtifactDigest: "",
		State:          domain.FunctionStateBuilding,
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("put function version: %v", err)
	}
	if err := meta.PutBuildJob(context.Background(), &domain.BuildJob{
		ID:                "build-existing",
		FunctionVersionID: "fn-existing-build",
		TargetRegion:      "ap-south-1",
		State:             "queued",
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("put build job: %v", err)
	}

	_, err := svc.CreateFunctionVersion(context.Background(), domain.DeployRequest{
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
	if !errors.Is(err, domain.ErrProjectBuildQuota) {
		t.Fatalf("expected build quota error, got %v", err)
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
			if strings.TrimSpace(job.LogsKey) == "" {
				t.Fatal("expected build logs key after async build")
			}
			logs, err := objects.Get(context.Background(), job.LogsKey)
			if err != nil {
				t.Fatalf("get build logs: %v", err)
			}
			if !strings.Contains(string(logs), "build job") {
				t.Fatalf("expected build logs to contain lifecycle entries, got %q", string(logs))
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

func TestCreateFunctionVersionQueuesInitialBuildLogsImmediately(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	bus := NewMemoryBus(16)
	svc := New(meta, objects)
	svc.SetBuildBus(bus)

	version, err := svc.CreateFunctionVersion(context.Background(), domain.DeployRequest{
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

	job, err := meta.GetBuildJob(context.Background(), version.BuildJobID)
	if err != nil {
		t.Fatalf("get build job: %v", err)
	}
	if strings.TrimSpace(job.LogsKey) == "" {
		t.Fatal("expected queued build job to have logs key")
	}
	logs, err := objects.Get(context.Background(), job.LogsKey)
	if err != nil {
		t.Fatalf("get queued build logs: %v", err)
	}
	if !strings.Contains(string(logs), "queued for function version") {
		t.Fatalf("expected queued build logs, got %q", string(logs))
	}
}

func TestAsyncBuildCreatesDefaultHTTPTrigger(t *testing.T) {
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

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		triggers, err := meta.ListHTTPTriggersByFunctionVersion(context.Background(), version.ID)
		if err != nil {
			t.Fatalf("list http triggers: %v", err)
		}
		if len(triggers) == 1 {
			if triggers[0].AuthMode != domain.HTTPTriggerAuthModeNone {
				t.Fatalf("expected public default auth mode, got %s", triggers[0].AuthMode)
			}
			if !triggers[0].Enabled {
				t.Fatal("expected default http trigger to be enabled")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out waiting for default http trigger")
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
	buildJob, err := meta.GetBuildJob(context.Background(), version.BuildJobID)
	if err != nil {
		t.Fatalf("get build job: %v", err)
	}
	repoParsed, err := url.Parse(repoURL)
	if err != nil {
		t.Fatalf("parse repo url: %v", err)
	}
	repoDir := repoParsed.Path
	revisionCmd := exec.Command("git", "rev-parse", "HEAD")
	revisionCmd.Dir = repoDir
	revisionBytes, err := revisionCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve git revision: %v: %s", err, string(revisionBytes))
	}
	commitSHA := strings.TrimSpace(string(revisionBytes))
	if buildJob.Metadata["gitUrl"] != repoURL {
		t.Fatalf("expected build metadata gitUrl=%s, got %+v", repoURL, buildJob.Metadata)
	}
	if buildJob.Metadata["branch"] != "main" {
		t.Fatalf("expected build metadata branch=main, got %+v", buildJob.Metadata)
	}
	if buildJob.Metadata["commitSha"] != commitSHA {
		t.Fatalf("expected build metadata commitSha=%s, got %+v", commitSHA, buildJob.Metadata)
	}
	if strings.TrimSpace(buildJob.LogsKey) == "" {
		t.Fatal("expected build logs key for git source build")
	}
	buildLogs, err := objects.Get(context.Background(), buildJob.LogsKey)
	if err != nil {
		t.Fatalf("get git build logs: %v", err)
	}
	if !strings.Contains(string(buildLogs), "git clone") {
		t.Fatalf("expected git build logs to include clone step, got %q", string(buildLogs))
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

func TestCreateFunctionVersionQueuesWarmPreparationWhenReady(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	svc := New(meta, objects)
	warmer := &recordingWarmPreparer{}
	svc.SetWarmPreparer(warmer)

	version, err := svc.CreateFunctionVersion(context.Background(), domain.DeployRequest{
		ProjectID:     "demo",
		Name:          "warm-echo",
		Runtime:       "node22",
		Entrypoint:    "index.mjs",
		NetworkPolicy: domain.NetworkPolicyNone,
		Regions:       []string{"ap-south-1", "ap-southeast-1"},
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
	if version.State != domain.FunctionStateReady {
		t.Fatalf("expected ready function version, got %s", version.State)
	}
	if len(warmer.preparedVersionIDs) != 1 || warmer.preparedVersionIDs[0] != version.ID {
		t.Fatalf("expected warm preparation for %s, got %+v", version.ID, warmer.preparedVersionIDs)
	}
}

func TestCreateFunctionVersionQueuesWarmPreparationForFullNetworkPolicy(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	svc := New(meta, objects)
	warmer := &recordingWarmPreparer{}
	svc.SetWarmPreparer(warmer)

	version, err := svc.CreateFunctionVersion(context.Background(), domain.DeployRequest{
		ProjectID:     "demo",
		Name:          "full-network",
		Runtime:       "node22",
		Entrypoint:    "index.mjs",
		NetworkPolicy: domain.NetworkPolicyFull,
		Regions:       []string{"ap-south-1"},
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
	if version.State != domain.FunctionStateReady {
		t.Fatalf("expected ready function version, got %s", version.State)
	}
	if len(warmer.preparedVersionIDs) != 1 || warmer.preparedVersionIDs[0] != version.ID {
		t.Fatalf("expected warm preparation for %s, got %+v", version.ID, warmer.preparedVersionIDs)
	}
}

func TestCreateFunctionVersionSkipsWarmPreparationWhenEnvRefsConfigured(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	svc := New(meta, objects)
	warmer := &recordingWarmPreparer{}
	svc.SetWarmPreparer(warmer)

	version, err := svc.CreateFunctionVersion(context.Background(), domain.DeployRequest{
		ProjectID:     "demo",
		Name:          "envful",
		Runtime:       "node22",
		Entrypoint:    "index.mjs",
		NetworkPolicy: domain.NetworkPolicyNone,
		Regions:       []string{"ap-south-1"},
		EnvRefs:       []string{"API_TOKEN"},
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
	if version.State != domain.FunctionStateReady {
		t.Fatalf("expected ready function version, got %s", version.State)
	}
	if len(warmer.preparedVersionIDs) != 0 {
		t.Fatalf("expected warm preparation to be skipped when env refs are configured, got %+v", warmer.preparedVersionIDs)
	}
}

func TestAsyncBuildFailsWhenArtifactExceedsLimit(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	bus := NewMemoryBus(16)
	svc := New(meta, objects)
	svc.SetBuildBus(bus)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	go func() {
		_ = svc.RunBuildConsumer(ctx, "ap-south-1", "builder-ap-south-1")
	}()

	version, err := svc.CreateFunctionVersion(ctx, domain.DeployRequest{
		ProjectID:  "demo",
		Name:       "too-big",
		Runtime:    "node22",
		Entrypoint: "index.mjs",
		Regions:    []string{"ap-south-1"},
		Source: domain.DeploySource{
			Type:         domain.SourceTypeBundle,
			BundleBase64: oversizeBundleBase64(t),
		},
	})
	if err != nil {
		t.Fatalf("queue oversized build: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		latest, err := meta.GetFunctionVersion(context.Background(), version.ID)
		if err != nil {
			t.Fatalf("get function version: %v", err)
		}
		if latest.State == domain.FunctionStateFailed {
			if latest.ArtifactDigest != "" {
				t.Fatal("expected no artifact digest on failed oversized build")
			}
			job, err := meta.GetBuildJob(context.Background(), version.BuildJobID)
			if err != nil {
				t.Fatalf("get build job: %v", err)
			}
			if job.State != "failed" {
				t.Fatalf("expected failed build job, got %s", job.State)
			}
			if !strings.Contains(job.Error, "exceeds limit") {
				t.Fatalf("expected size-limit error, got %q", job.Error)
			}
			if strings.TrimSpace(job.LogsKey) == "" {
				t.Fatal("expected build logs key for failed oversized build")
			}
			logs, err := objects.Get(context.Background(), job.LogsKey)
			if err != nil {
				t.Fatalf("get oversized build logs: %v", err)
			}
			if !strings.Contains(string(logs), "exceeds limit") {
				t.Fatalf("expected oversized build logs to contain failure, got %q", string(logs))
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for oversized build to fail")
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

func oversizeBundleBase64(t *testing.T) string {
	t.Helper()

	blob := make([]byte, maxArtifactSizeBytes+1024)
	if _, err := mrand.New(mrand.NewSource(1)).Read(blob); err != nil {
		t.Fatalf("generate oversized blob: %v", err)
	}
	bundle, err := artifact.BundleFromFiles(map[string][]byte{
		"index.mjs": []byte("export async function handler() { return { ok: true }; }"),
		"blob.bin":  blob,
	})
	if err != nil {
		t.Fatalf("build oversized bundle: %v", err)
	}
	return base64.StdEncoding.EncodeToString(bundle)
}

type recordingWarmPreparer struct {
	preparedVersionIDs []string
}

func (r *recordingWarmPreparer) PrepareFunctionVersion(_ context.Context, version *domain.FunctionVersion) error {
	r.preparedVersionIDs = append(r.preparedVersionIDs, version.ID)
	return nil
}
