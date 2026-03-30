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
	now     func() time.Time
}

func New(store store.Store, objects artifact.Store) *Service {
	return &Service{
		store:   store,
		objects: objects,
		now:     func() time.Time { return time.Now().UTC() },
	}
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
	if req.ProjectID == "" {
		return nil, fmt.Errorf("projectId is required")
	}
	if req.Runtime == "" {
		req.Runtime = "node22"
	}
	if req.TimeoutSec <= 0 {
		req.TimeoutSec = 30
	}
	if req.MemoryMB <= 0 {
		req.MemoryMB = 128
	}
	if req.MaxRetries < 0 {
		req.MaxRetries = 0
	}
	normalizedRegions, err := regions.NormalizeExecutionRegions(req.Regions)
	if err != nil {
		return nil, err
	}
	req.Regions = normalizedRegions

	versionID := uuid.NewString()
	buildJob := &domain.BuildJob{
		ID:                uuid.NewString(),
		FunctionVersionID: versionID,
		State:             "running",
		CreatedAt:         s.now(),
		UpdatedAt:         s.now(),
	}
	if err := s.store.PutBuildJob(ctx, buildJob); err != nil {
		return nil, err
	}

	bundle, startup, err := s.prepareBundle(ctx, req)
	if err != nil {
		buildJob.State = "failed"
		buildJob.Error = err.Error()
		buildJob.UpdatedAt = s.now()
		_ = s.store.PutBuildJob(ctx, buildJob)
		return nil, err
	}

	sum := sha256.Sum256(bundle)
	digest := hex.EncodeToString(sum[:])
	bundleKey := fmt.Sprintf("artifacts/%s/bundle.tgz", digest)
	startupKey := fmt.Sprintf("artifacts/%s/startup.json", digest)

	if err := s.objects.Put(ctx, bundleKey, bundle); err != nil {
		return nil, err
	}
	if err := s.objects.Put(ctx, startupKey, startup); err != nil {
		return nil, err
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
		return nil, err
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
		SourceType:     req.Source.Type,
		ArtifactDigest: digest,
		State:          domain.FunctionStateReady,
		CreatedAt:      now,
	}
	if err := s.store.PutFunctionVersion(ctx, version); err != nil {
		return nil, err
	}

	buildJob.State = "succeeded"
	buildJob.UpdatedAt = s.now()
	if err := s.store.PutBuildJob(ctx, buildJob); err != nil {
		return nil, err
	}

	return version, nil
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
		tmpDir, err := os.MkdirTemp("", "lecrev-git-*")
		if err != nil {
			return nil, nil, err
		}
		defer os.RemoveAll(tmpDir)
		if req.Source.GitURL == "" {
			return nil, nil, fmt.Errorf("gitUrl is required for git source")
		}
		cloneArgs := []string{"clone", "--depth", "1"}
		if req.Source.GitRef != "" {
			cloneArgs = append(cloneArgs, "--branch", req.Source.GitRef)
		}
		cloneArgs = append(cloneArgs, req.Source.GitURL, tmpDir)
		cmd := exec.CommandContext(ctx, "git", cloneArgs...)
		if output, err := cmd.CombinedOutput(); err != nil {
			return nil, nil, fmt.Errorf("git clone failed: %w: %s", err, string(output))
		}
		root := tmpDir
		if req.Source.SubPath != "" {
			root = filepath.Join(tmpDir, req.Source.SubPath)
		}
		bundle, err := artifact.BundleFromDirectory(root, map[string][]byte{
			"function.json": functionManifest,
			"startup.json":  startup,
		})
		return bundle, startup, err
	default:
		return nil, nil, fmt.Errorf("unsupported source type %q", req.Source.Type)
	}
}
