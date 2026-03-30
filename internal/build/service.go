package build

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/theg1239/lecrev/internal/artifact"
	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/idempotency"
	"github.com/theg1239/lecrev/internal/regions"
	"github.com/theg1239/lecrev/internal/store"
)

type Service struct {
	store   store.Store
	objects artifact.Store
	bus     BuildBus
	now     func() time.Time
}

const (
	defaultRuntime       = "node22"
	defaultMemoryMB      = 128
	defaultTimeoutSec    = 30
	defaultNetworkPolicy = domain.NetworkPolicyFull
	minMemoryMB          = 64
	maxMemoryMB          = 1024
	maxTimeoutSec        = 300
	maxRetries           = 5
	maxEnvRefs           = 64
	maxArtifactSizeBytes = 10 << 20
)

func New(store store.Store, objects artifact.Store) *Service {
	return &Service{
		store:   store,
		objects: objects,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) SetBuildBus(bus BuildBus) {
	s.bus = bus
}

func (s *Service) RunBuildConsumer(ctx context.Context, region, consumer string) error {
	if s.bus == nil {
		return nil
	}
	return s.bus.ConsumeBuild(ctx, region, consumer, func(ctx context.Context, assignment BuildAssignment) error {
		return s.ProcessBuildJob(ctx, assignment.BuildJobID)
	})
}

func (s *Service) CreateFunctionVersion(ctx context.Context, req domain.DeployRequest) (*domain.FunctionVersion, error) {
	if req.IdempotencyKey != "" {
		return s.createFunctionVersionIdempotent(ctx, req)
	}
	return s.createFunctionVersion(ctx, req)
}

func (s *Service) createFunctionVersionIdempotent(ctx context.Context, req domain.DeployRequest) (*domain.FunctionVersion, error) {
	requestHash, err := deployRequestHash(req)
	if err != nil {
		return nil, err
	}
	now := s.now()
	record := &domain.IdempotencyRecord{
		Scope:       "deploy",
		ProjectID:   req.ProjectID,
		Key:         req.IdempotencyKey,
		RequestHash: requestHash,
		Resource:    "function_version",
		Status:      domain.IdempotencyStatusPending,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.store.CreateIdempotencyRecord(ctx, record); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return s.replayFunctionVersion(ctx, req.ProjectID, req.IdempotencyKey, requestHash)
		}
		return nil, err
	}

	version, err := s.createFunctionVersion(ctx, req)
	if err != nil {
		_ = s.store.DeleteIdempotencyRecord(ctx, record.Scope, record.ProjectID, record.Key)
		return nil, err
	}

	record.ResourceID = version.ID
	record.Status = domain.IdempotencyStatusCompleted
	record.UpdatedAt = s.now()
	if err := s.store.UpdateIdempotencyRecord(ctx, record); err != nil {
		return nil, err
	}
	return version, nil
}

func (s *Service) replayFunctionVersion(ctx context.Context, projectID, key, requestHash string) (*domain.FunctionVersion, error) {
	record, err := s.store.GetIdempotencyRecord(ctx, "deploy", projectID, key)
	if err != nil {
		return nil, err
	}
	if record.RequestHash != requestHash {
		return nil, domain.ErrIdempotencyConflict
	}
	if record.Status != domain.IdempotencyStatusCompleted || record.ResourceID == "" {
		return nil, domain.ErrIdempotencyInProgress
	}
	return s.store.GetFunctionVersion(ctx, record.ResourceID)
}

func (s *Service) createFunctionVersion(ctx context.Context, req domain.DeployRequest) (*domain.FunctionVersion, error) {
	req, err := normalizeDeployRequest(req)
	if err != nil {
		return nil, err
	}
	version, buildJob, err := s.initializeBuild(ctx, req)
	if err != nil {
		return nil, err
	}
	if s.bus != nil {
		if err := s.bus.PublishBuild(ctx, buildJob.TargetRegion, BuildAssignment{BuildJobID: buildJob.ID}); err != nil {
			_ = s.markBuildFailed(ctx, version, buildJob, fmt.Errorf("publish build job: %w", err))
			return nil, err
		}
		return version, nil
	}
	if err := s.ProcessBuildJob(ctx, buildJob.ID); err != nil {
		return nil, err
	}
	return s.store.GetFunctionVersion(ctx, version.ID)
}

func normalizeDeployRequest(req domain.DeployRequest) (domain.DeployRequest, error) {
	if req.ProjectID == "" {
		return domain.DeployRequest{}, fmt.Errorf("projectId is required")
	}
	if strings.TrimSpace(req.Name) == "" {
		return domain.DeployRequest{}, fmt.Errorf("name is required")
	}
	if strings.TrimSpace(req.Entrypoint) == "" {
		return domain.DeployRequest{}, fmt.Errorf("entrypoint is required")
	}
	if req.Runtime == "" {
		req.Runtime = defaultRuntime
	}
	if req.TimeoutSec == 0 {
		req.TimeoutSec = defaultTimeoutSec
	} else if req.TimeoutSec < 0 {
		return domain.DeployRequest{}, fmt.Errorf("timeoutSec must be positive")
	}
	if req.MemoryMB == 0 {
		req.MemoryMB = defaultMemoryMB
	} else if req.MemoryMB < 0 {
		return domain.DeployRequest{}, fmt.Errorf("memoryMb must be positive")
	}
	if req.NetworkPolicy == "" {
		req.NetworkPolicy = defaultNetworkPolicy
	}
	if req.MaxRetries < 0 {
		return domain.DeployRequest{}, fmt.Errorf("maxRetries must be zero or greater")
	}
	normalizedRegions, err := regions.NormalizeExecutionRegions(req.Regions)
	if err != nil {
		return domain.DeployRequest{}, err
	}
	req.Regions = normalizedRegions
	if err := validateDeployRequest(req); err != nil {
		return domain.DeployRequest{}, err
	}
	return req, nil
}

func validateDeployRequest(req domain.DeployRequest) error {
	if req.Runtime != defaultRuntime {
		return fmt.Errorf("unsupported runtime %q: only %s is supported", req.Runtime, defaultRuntime)
	}
	if req.MemoryMB < minMemoryMB || req.MemoryMB > maxMemoryMB {
		return fmt.Errorf("memoryMb must be between %d and %d", minMemoryMB, maxMemoryMB)
	}
	if req.TimeoutSec < 1 || req.TimeoutSec > maxTimeoutSec {
		return fmt.Errorf("timeoutSec must be between 1 and %d", maxTimeoutSec)
	}
	if req.MaxRetries > maxRetries {
		return fmt.Errorf("maxRetries must be between 0 and %d", maxRetries)
	}
	if len(req.EnvRefs) > maxEnvRefs {
		return fmt.Errorf("envRefs cannot exceed %d entries", maxEnvRefs)
	}
	for _, ref := range req.EnvRefs {
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("envRefs cannot contain empty values")
		}
	}
	switch req.NetworkPolicy {
	case domain.NetworkPolicyNone, domain.NetworkPolicyFull:
	case domain.NetworkPolicyAllowlist:
		return fmt.Errorf("network policy %q is not yet supported", req.NetworkPolicy)
	default:
		return fmt.Errorf("unsupported network policy %q", req.NetworkPolicy)
	}
	switch req.Source.Type {
	case domain.SourceTypeBundle:
		if req.Source.BundleBase64 == "" && len(req.Source.InlineFiles) == 0 {
			return fmt.Errorf("bundle source requires bundleBase64 or inlineFiles")
		}
	case domain.SourceTypeGit:
		if strings.TrimSpace(req.Source.GitURL) == "" {
			return fmt.Errorf("gitUrl is required for git source")
		}
	default:
		return fmt.Errorf("unsupported source type %q", req.Source.Type)
	}
	return nil
}

func (s *Service) initializeBuild(ctx context.Context, req domain.DeployRequest) (*domain.FunctionVersion, *domain.BuildJob, error) {
	now := s.now()
	versionID := uuid.NewString()
	buildJobID := uuid.NewString()
	requestPayload, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	version := &domain.FunctionVersion{
		ID:             versionID,
		ProjectID:      req.ProjectID,
		Name:           req.Name,
		Runtime:        req.Runtime,
		Entrypoint:     req.Entrypoint,
		MemoryMB:       req.MemoryMB,
		TimeoutSec:     req.TimeoutSec,
		NetworkPolicy:  req.NetworkPolicy,
		Regions:        append([]string(nil), req.Regions...),
		EnvRefs:        append([]string(nil), req.EnvRefs...),
		MaxRetries:     req.MaxRetries,
		BuildJobID:     buildJobID,
		SourceType:     req.Source.Type,
		ArtifactDigest: "",
		State:          domain.FunctionStateBuilding,
		CreatedAt:      now,
	}
	if err := s.store.PutFunctionVersion(ctx, version); err != nil {
		return nil, nil, err
	}

	buildJob := &domain.BuildJob{
		ID:                buildJobID,
		FunctionVersionID: versionID,
		TargetRegion:      pickBuildRegion(req.Regions),
		State:             "queued",
		Request:           requestPayload,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := s.store.PutBuildJob(ctx, buildJob); err != nil {
		return nil, nil, err
	}
	return version, buildJob, nil
}

func (s *Service) ProcessBuildJob(ctx context.Context, buildJobID string) error {
	buildJob, err := s.store.GetBuildJob(ctx, buildJobID)
	if err != nil {
		return err
	}
	if buildJob.State == "succeeded" {
		return nil
	}
	version, err := s.store.GetFunctionVersion(ctx, buildJob.FunctionVersionID)
	if err != nil {
		return err
	}

	var req domain.DeployRequest
	if err := json.Unmarshal(buildJob.Request, &req); err != nil {
		return s.markBuildFailed(ctx, version, buildJob, fmt.Errorf("decode build request: %w", err))
	}
	req, err = normalizeDeployRequest(req)
	if err != nil {
		return s.markBuildFailed(ctx, version, buildJob, err)
	}

	buildJob.State = "running"
	buildJob.Error = ""
	buildJob.UpdatedAt = s.now()
	if err := s.store.PutBuildJob(ctx, buildJob); err != nil {
		return err
	}

	bundle, startup, err := s.prepareBundle(ctx, req)
	if err != nil {
		return s.markBuildFailed(ctx, version, buildJob, err)
	}
	if len(bundle) > maxArtifactSizeBytes {
		return s.markBuildFailed(ctx, version, buildJob, fmt.Errorf("artifact size %d exceeds limit of %d bytes", len(bundle), maxArtifactSizeBytes))
	}

	sum := sha256.Sum256(bundle)
	digest := hex.EncodeToString(sum[:])
	bundleKey := fmt.Sprintf("artifacts/%s/bundle.tgz", digest)
	startupKey := fmt.Sprintf("artifacts/%s/startup.json", digest)

	if err := s.objects.Put(ctx, bundleKey, bundle); err != nil {
		return s.markBuildFailed(ctx, version, buildJob, err)
	}
	if err := s.objects.Put(ctx, startupKey, startup); err != nil {
		return s.markBuildFailed(ctx, version, buildJob, err)
	}

	now := s.now()
	artifactMeta := &domain.Artifact{
		Digest:     digest,
		SizeBytes:  int64(len(bundle)),
		BundleKey:  bundleKey,
		StartupKey: startupKey,
		CreatedAt:  now,
		Regions:    make(map[string]time.Time, len(req.Regions)),
	}
	for _, region := range req.Regions {
		artifactMeta.Regions[region] = now
	}
	if err := s.store.PutArtifact(ctx, artifactMeta); err != nil {
		return s.markBuildFailed(ctx, version, buildJob, err)
	}

	version.ArtifactDigest = digest
	version.State = domain.FunctionStateReady
	if err := s.store.PutFunctionVersion(ctx, version); err != nil {
		return s.markBuildFailed(ctx, version, buildJob, err)
	}

	buildJob.State = "succeeded"
	buildJob.Error = ""
	buildJob.UpdatedAt = s.now()
	return s.store.PutBuildJob(ctx, buildJob)
}

func (s *Service) markBuildFailed(ctx context.Context, version *domain.FunctionVersion, buildJob *domain.BuildJob, err error) error {
	buildJob.State = "failed"
	buildJob.Error = err.Error()
	buildJob.UpdatedAt = s.now()
	version.State = domain.FunctionStateFailed
	if putErr := s.store.PutFunctionVersion(ctx, version); putErr != nil {
		return putErr
	}
	if putErr := s.store.PutBuildJob(ctx, buildJob); putErr != nil {
		return putErr
	}
	return err
}

func pickBuildRegion(targetRegions []string) string {
	if len(targetRegions) == 0 {
		return regions.DefaultPrimary()
	}
	return targetRegions[0]
}

func deployRequestHash(req domain.DeployRequest) (string, error) {
	return idempotency.Hash(struct {
		ProjectID     string               `json:"projectId"`
		Name          string               `json:"name"`
		Runtime       string               `json:"runtime"`
		Entrypoint    string               `json:"entrypoint"`
		MemoryMB      int                  `json:"memoryMb"`
		TimeoutSec    int                  `json:"timeoutSec"`
		NetworkPolicy domain.NetworkPolicy `json:"networkPolicy"`
		Regions       []string             `json:"regions"`
		EnvRefs       []string             `json:"envRefs"`
		MaxRetries    int                  `json:"maxRetries"`
		Source        domain.DeploySource  `json:"source"`
	}{
		ProjectID:     req.ProjectID,
		Name:          req.Name,
		Runtime:       req.Runtime,
		Entrypoint:    req.Entrypoint,
		MemoryMB:      req.MemoryMB,
		TimeoutSec:    req.TimeoutSec,
		NetworkPolicy: req.NetworkPolicy,
		Regions:       append([]string(nil), req.Regions...),
		EnvRefs:       append([]string(nil), req.EnvRefs...),
		MaxRetries:    req.MaxRetries,
		Source:        req.Source,
	})
}

func (s *Service) prepareBundle(ctx context.Context, req domain.DeployRequest) ([]byte, []byte, error) {
	functionManifest, err := json.MarshalIndent(map[string]any{
		"name":          req.Name,
		"runtime":       req.Runtime,
		"entrypoint":    req.Entrypoint,
		"memoryMb":      req.MemoryMB,
		"timeoutSec":    req.TimeoutSec,
		"networkPolicy": req.NetworkPolicy,
		"regions":       req.Regions,
		"envRefs":       req.EnvRefs,
		"maxRetries":    req.MaxRetries,
	}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	startup, err := json.MarshalIndent(map[string]any{
		"entrypoint": req.Entrypoint,
		"runtime":    req.Runtime,
		"preparedAt": s.now(),
	}, "", "  ")
	if err != nil {
		return nil, nil, err
	}

	switch req.Source.Type {
	case domain.SourceTypeBundle:
		files := make(map[string][]byte)
		for name, content := range req.Source.InlineFiles {
			files[name] = []byte(content)
		}
		if req.Source.BundleBase64 != "" {
			decoded, err := base64.StdEncoding.DecodeString(req.Source.BundleBase64)
			if err != nil {
				return nil, nil, err
			}
			tmpDir, err := os.MkdirTemp("", "lecrev-bundle-*")
			if err != nil {
				return nil, nil, err
			}
			defer os.RemoveAll(tmpDir)
			if err := artifact.ExtractTarGz(decoded, tmpDir); err != nil {
				return nil, nil, err
			}
			bundle, err := artifact.BundleFromDirectory(tmpDir, map[string][]byte{
				"function.json": functionManifest,
				"startup.json":  startup,
			})
			return bundle, startup, err
		}
		files["function.json"] = functionManifest
		files["startup.json"] = startup
		bundle, err := artifact.BundleFromFiles(files)
		return bundle, startup, err
	case domain.SourceTypeGit:
		root, cleanup, err := s.prepareGitWorkspace(ctx, req)
		if err != nil {
			return nil, nil, err
		}
		defer cleanup()
		bundle, err := artifact.BundleFromDirectory(root, map[string][]byte{
			"function.json": functionManifest,
			"startup.json":  startup,
		})
		return bundle, startup, err
	default:
		return nil, nil, fmt.Errorf("unsupported source type %q", req.Source.Type)
	}
}

type packageManifest struct {
	PackageManager string            `json:"packageManager"`
	Scripts        map[string]string `json:"scripts"`
}

func (s *Service) prepareGitWorkspace(ctx context.Context, req domain.DeployRequest) (string, func(), error) {
	if strings.TrimSpace(req.Source.GitURL) == "" {
		return "", nil, fmt.Errorf("gitUrl is required for git source")
	}

	tmpDir, err := os.MkdirTemp("", "lecrev-git-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	cloneArgs := []string{"clone", "--depth", "1"}
	if req.Source.GitRef != "" {
		cloneArgs = append(cloneArgs, "--branch", req.Source.GitRef)
	}
	cloneArgs = append(cloneArgs, req.Source.GitURL, tmpDir)
	if err := runCommand(ctx, "", "git", cloneArgs...); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git clone failed: %w", err)
	}

	root, err := resolveBuildRoot(tmpDir, req.Source.SubPath)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if err := prepareNodeWorkspace(ctx, root); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := verifyEntrypoint(root, req.Entrypoint); err != nil {
		cleanup()
		return "", nil, err
	}
	return root, cleanup, nil
}

func resolveBuildRoot(repoRoot, subPath string) (string, error) {
	root := repoRoot
	if strings.TrimSpace(subPath) != "" {
		repoAbs, err := filepath.Abs(repoRoot)
		if err != nil {
			return "", err
		}
		candidate, err := filepath.Abs(filepath.Join(repoRoot, filepath.FromSlash(subPath)))
		if err != nil {
			return "", err
		}
		if candidate != repoAbs && !strings.HasPrefix(candidate, repoAbs+string(os.PathSeparator)) {
			return "", fmt.Errorf("subPath %q escapes repository root", subPath)
		}
		root = candidate
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("resolve build root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("build root %q is not a directory", root)
	}
	return root, nil
}

func prepareNodeWorkspace(ctx context.Context, root string) error {
	pkgPath := filepath.Join(root, "package.json")
	rawPkg, err := os.ReadFile(pkgPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read package.json: %w", err)
	}

	var pkg packageManifest
	if err := json.Unmarshal(rawPkg, &pkg); err != nil {
		return fmt.Errorf("decode package.json: %w", err)
	}

	if err := validatePackageManager(root, pkg.PackageManager); err != nil {
		return err
	}

	hasBuildScript := strings.TrimSpace(pkg.Scripts["build"]) != ""
	installArgs := []string{"install"}
	if hasNpmLockfile(root) {
		installArgs[0] = "ci"
	}
	if !hasBuildScript {
		installArgs = append(installArgs, "--omit=dev")
	}
	if err := runCommand(ctx, root, "npm", installArgs...); err != nil {
		return fmt.Errorf("install npm dependencies: %w", err)
	}
	if !hasBuildScript {
		return nil
	}
	if err := runCommand(ctx, root, "npm", "run", "build", "--if-present"); err != nil {
		return fmt.Errorf("run npm build: %w", err)
	}
	if err := runCommand(ctx, root, "npm", "prune", "--omit=dev"); err != nil {
		return fmt.Errorf("prune npm devDependencies: %w", err)
	}
	return nil
}

func validatePackageManager(root, packageManager string) error {
	if strings.TrimSpace(packageManager) != "" && !strings.HasPrefix(packageManager, "npm@") && packageManager != "npm" {
		return fmt.Errorf("unsupported package manager %q: only npm-based git builds are currently supported", packageManager)
	}
	for _, unsupported := range []string{"pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb"} {
		if _, err := os.Stat(filepath.Join(root, unsupported)); err == nil {
			return fmt.Errorf("unsupported lockfile %q: only npm-based git builds are currently supported", unsupported)
		}
	}
	return nil
}

func hasNpmLockfile(root string) bool {
	for _, name := range []string{"package-lock.json", "npm-shrinkwrap.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			return true
		}
	}
	return false
}

func verifyEntrypoint(root, entrypoint string) error {
	if strings.TrimSpace(entrypoint) == "" {
		return fmt.Errorf("entrypoint is required")
	}
	target := filepath.Join(root, filepath.FromSlash(entrypoint))
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("verify entrypoint %q: %w", entrypoint, err)
	}
	if info.IsDir() {
		return fmt.Errorf("entrypoint %q resolved to a directory", entrypoint)
	}
	return nil
}

func runCommand(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}
