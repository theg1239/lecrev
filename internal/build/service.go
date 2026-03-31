package build

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
	warmer  WarmPreparer
	replica ArtifactReplicator
	now     func() time.Time

	maxActiveBuildJobsPerProject int
	gitCloneTimeout              time.Duration
	npmInstallTimeout            time.Duration
	npmBuildTimeout              time.Duration
	npmPruneTimeout              time.Duration
}

type WarmPreparer interface {
	PrepareFunctionVersion(ctx context.Context, version *domain.FunctionVersion) error
}

type ArtifactReplicator interface {
	Replicate(ctx context.Context, req ArtifactReplicationRequest) (map[string]time.Time, error)
}

type ArtifactReplicationRequest struct {
	BundleKey  string
	Bundle     []byte
	StartupKey string
	Startup    []byte
	Regions    []string
}

const (
	defaultRuntime       = "node22"
	defaultMemoryMB      = 128
	defaultTimeoutSec    = 30
	defaultNetworkPolicy = domain.NetworkPolicyFull
	websiteMemoryMB      = 1024
	websiteTimeoutSec    = 120
	minMemoryMB          = 64
	maxMemoryMB          = 4096
	maxTimeoutSec        = 900
	maxRetries           = 10
	maxEnvVars           = 128
	maxEnvRefs           = 128
	maxArtifactSizeBytes = 64 << 20
	maxActiveBuildJobs   = 20
)

func New(store store.Store, objects artifact.Store) *Service {
	return &Service{
		store:                        store,
		objects:                      objects,
		now:                          func() time.Time { return time.Now().UTC() },
		maxActiveBuildJobsPerProject: maxActiveBuildJobs,
		gitCloneTimeout:              3 * time.Minute,
		npmInstallTimeout:            8 * time.Minute,
		npmBuildTimeout:              10 * time.Minute,
		npmPruneTimeout:              3 * time.Minute,
	}
}

func (s *Service) SetBuildBus(bus BuildBus) {
	s.bus = bus
}

func (s *Service) SetWarmPreparer(warmer WarmPreparer) {
	s.warmer = warmer
}

func (s *Service) SetArtifactReplicator(replica ArtifactReplicator) {
	s.replica = replica
}

func (s *Service) SetCommandTimeouts(gitClone, npmInstall, npmBuild, npmPrune time.Duration) {
	if gitClone > 0 {
		s.gitCloneTimeout = gitClone
	}
	if npmInstall > 0 {
		s.npmInstallTimeout = npmInstall
	}
	if npmBuild > 0 {
		s.npmBuildTimeout = npmBuild
	}
	if npmPrune > 0 {
		s.npmPruneTimeout = npmPrune
	}
}

func (s *Service) RunBuildConsumer(ctx context.Context, region, consumer string) error {
	return s.RunBuildConsumerWithConcurrency(ctx, region, consumer, 1)
}

func (s *Service) RunBuildConsumerWithConcurrency(ctx context.Context, region, consumer string, concurrency int) error {
	if s.bus == nil {
		return nil
	}
	return s.bus.ConsumeBuild(ctx, region, consumer, concurrency, func(ctx context.Context, assignment BuildAssignment) error {
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
	if err := s.enforceBuildQuota(ctx, req.ProjectID); err != nil {
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
	if isWebsiteDeploy(req.Source) {
		if req.MemoryMB < websiteMemoryMB {
			req.MemoryMB = websiteMemoryMB
		}
		if req.TimeoutSec < websiteTimeoutSec {
			req.TimeoutSec = websiteTimeoutSec
		}
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

func isWebsiteDeploy(source domain.DeploySource) bool {
	if source.Metadata == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(source.Metadata["deliveryKind"]), "website") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(source.Metadata["framework"]), "nextjs") {
		return true
	}
	return false
}

func validateDeployRequest(req domain.DeployRequest) error {
	if strings.TrimSpace(req.Entrypoint) == "" && req.Source.Type != domain.SourceTypeGit {
		return fmt.Errorf("entrypoint is required")
	}
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
	if len(req.EnvVars) > maxEnvVars {
		return fmt.Errorf("envVars cannot exceed %d entries", maxEnvVars)
	}
	for key, value := range req.EnvVars {
		if err := validateEnvVar(key, value); err != nil {
			return err
		}
	}
	for _, ref := range req.EnvRefs {
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("envRefs cannot contain empty values")
		}
		if _, exists := req.EnvVars[ref]; exists {
			return fmt.Errorf("envRefs cannot overlap envVars key %q", ref)
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
	logsKey := artifact.BuildLogsKey(buildJobID)
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
		EnvVars:        cloneEnvVars(req.EnvVars),
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
		Metadata:          buildMetadataForRequest(req),
		LogsKey:           logsKey,
		State:             "queued",
		Request:           requestPayload,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := s.store.PutBuildJob(ctx, buildJob); err != nil {
		return nil, nil, err
	}
	if s.objects != nil {
		queuedLog := fmt.Sprintf("[%s] build job %s queued for function version %s in region %s\n",
			now.Format(time.RFC3339), buildJob.ID, version.ID, buildJob.TargetRegion)
		if err := s.objects.Put(ctx, logsKey, []byte(queuedLog)); err != nil {
			return nil, nil, err
		}
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
	if sanitizedPayload, marshalErr := json.Marshal(sanitizeDeployRequestForStorage(req)); marshalErr == nil {
		buildJob.Request = sanitizedPayload
	}

	buildJob.State = "running"
	buildJob.Error = ""
	if strings.TrimSpace(buildJob.LogsKey) == "" {
		buildJob.LogsKey = artifact.BuildLogsKey(buildJob.ID)
	}
	buildJob.UpdatedAt = s.now()
	if err := s.store.PutBuildJob(ctx, buildJob); err != nil {
		return err
	}
	recorder := newBuildRecorder(s.now)
	if s.objects != nil && strings.TrimSpace(buildJob.LogsKey) != "" {
		logsKey := buildJob.LogsKey
		recorder.onUpdate = func(data []byte) {
			_ = s.objects.Put(ctx, logsKey, data)
		}
	}
	recorder.Printf("build job %s started for function version %s in region %s", buildJob.ID, version.ID, buildJob.TargetRegion)
	recorder.Printf("source type=%s runtime=%s entrypoint=%s", req.Source.Type, req.Runtime, req.Entrypoint)

	prepared, err := s.prepareBuildOutput(ctx, req, recorder)
	if err != nil {
		return s.markBuildFailedWithLogs(ctx, version, buildJob, recorder, err)
	}
	defer func() {
		if prepared != nil && prepared.Cleanup != nil {
			prepared.Cleanup()
		}
	}()
	bundle := prepared.Bundle
	startup := prepared.Startup
	buildJob.Metadata = mergeBuildMetadata(buildJob.Metadata, prepared.Metadata)
	if len(bundle) > maxArtifactSizeBytes {
		return s.markBuildFailedWithLogs(ctx, version, buildJob, recorder, fmt.Errorf("artifact size %d exceeds limit of %d bytes", len(bundle), maxArtifactSizeBytes))
	}

	sum := sha256.Sum256(bundle)
	digest := hex.EncodeToString(sum[:])
	bundleKey := fmt.Sprintf("artifacts/%s/bundle.tgz", digest)
	startupKey := fmt.Sprintf("artifacts/%s/startup.json", digest)
	recorder.Printf("prepared immutable bundle digest=%s sizeBytes=%d", digest, len(bundle))

	if err := s.objects.Put(ctx, bundleKey, bundle); err != nil {
		return s.markBuildFailedWithLogs(ctx, version, buildJob, recorder, err)
	}
	if err := s.objects.Put(ctx, startupKey, startup); err != nil {
		return s.markBuildFailedWithLogs(ctx, version, buildJob, recorder, err)
	}
	recorder.Printf("uploaded bundle to %s and startup metadata to %s", bundleKey, startupKey)

	now := s.now()
	artifactMeta := &domain.Artifact{
		Digest:     digest,
		SizeBytes:  int64(len(bundle)),
		BundleKey:  bundleKey,
		StartupKey: startupKey,
		CreatedAt:  now,
		Regions:    make(map[string]time.Time, len(req.Regions)),
	}
	if s.replica != nil {
		regions, err := s.replica.Replicate(ctx, ArtifactReplicationRequest{
			BundleKey:  bundleKey,
			Bundle:     bundle,
			StartupKey: startupKey,
			Startup:    startup,
			Regions:    req.Regions,
		})
		if err != nil {
			return s.markBuildFailedWithLogs(ctx, version, buildJob, recorder, err)
		}
		artifactMeta.Regions = regions
		for _, region := range req.Regions {
			recorder.Printf("replicated artifact into region %s", region)
		}
	} else {
		for _, region := range req.Regions {
			artifactMeta.Regions[region] = now
			recorder.Printf("marked artifact available in region %s", region)
		}
	}
	if prepared.Site != nil {
		staticPrefix := artifact.SiteAssetsPrefix(digest)
		for _, assetFile := range prepared.Site.StaticAssets {
			if err := s.objects.Put(ctx, artifact.SiteAssetObjectKey(digest, assetFile.Path), assetFile.Data); err != nil {
				return s.markBuildFailedWithLogs(ctx, version, buildJob, recorder, err)
			}
		}
		siteManifest, err := encodeWebsiteManifest(version.ID, staticPrefix, prepared.Entrypoint, now)
		if err != nil {
			return s.markBuildFailedWithLogs(ctx, version, buildJob, recorder, err)
		}
		if err := s.objects.Put(ctx, artifact.SiteManifestKey(digest), siteManifest); err != nil {
			return s.markBuildFailedWithLogs(ctx, version, buildJob, recorder, err)
		}
		recorder.Printf("uploaded next.js site manifest to %s", artifact.SiteManifestKey(digest))
		recorder.Printf("uploaded %d next.js static assets to %s", len(prepared.Site.StaticAssets), staticPrefix)
	}
	if err := s.store.PutArtifact(ctx, artifactMeta); err != nil {
		return s.markBuildFailedWithLogs(ctx, version, buildJob, recorder, err)
	}

	version.ArtifactDigest = digest
	version.Entrypoint = prepared.Entrypoint
	version.State = domain.FunctionStateReady
	if err := s.store.PutFunctionVersion(ctx, version); err != nil {
		return s.markBuildFailedWithLogs(ctx, version, buildJob, recorder, err)
	}
	if err := s.ensureDefaultHTTPTrigger(ctx, version, recorder); err != nil {
		return s.markBuildFailedWithLogs(ctx, version, buildJob, recorder, err)
	}
	if s.warmer != nil && len(version.EnvRefs) == 0 {
		if err := s.warmer.PrepareFunctionVersion(ctx, version); err != nil {
			recorder.Printf("warm preparation deferred for function version %s: %v", version.ID, err)
		} else {
			recorder.Printf("function warm preparation ready for function version %s", version.ID)
		}
	} else if s.warmer != nil {
		recorder.Printf("warm preparation skipped for function version %s because envRefs are configured", version.ID)
	}

	recorder.Printf("build job %s completed successfully", buildJob.ID)
	if err := s.archiveBuildLogs(ctx, buildJob, recorder); err != nil {
		recorder.Printf("build log archival failed: %v", err)
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

func (s *Service) markBuildFailedWithLogs(ctx context.Context, version *domain.FunctionVersion, buildJob *domain.BuildJob, recorder *buildRecorder, err error) error {
	if recorder != nil {
		recorder.Printf("build job %s failed: %v", buildJob.ID, err)
		if archiveErr := s.archiveBuildLogs(ctx, buildJob, recorder); archiveErr != nil {
			err = fmt.Errorf("%w (build log archival failed: %v)", err, archiveErr)
		}
	}
	return s.markBuildFailed(ctx, version, buildJob, err)
}

func pickBuildRegion(targetRegions []string) string {
	if len(targetRegions) == 0 {
		return regions.DefaultPrimary()
	}
	return targetRegions[0]
}

func (s *Service) ensureDefaultHTTPTrigger(ctx context.Context, version *domain.FunctionVersion, recorder *buildRecorder) error {
	triggers, err := s.store.ListHTTPTriggersByFunctionVersion(ctx, version.ID)
	if err != nil {
		return fmt.Errorf("list http triggers for function version %s: %w", version.ID, err)
	}
	if len(triggers) > 0 {
		recorder.Printf("default public http trigger already exists for function version %s", version.ID)
		return nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		token, err := generateHTTPTriggerToken()
		if err != nil {
			return fmt.Errorf("generate default http trigger token: %w", err)
		}
		trigger := &domain.HTTPTrigger{
			Token:             token,
			ProjectID:         version.ProjectID,
			FunctionVersionID: version.ID,
			Description:       "default public function url",
			AuthMode:          domain.HTTPTriggerAuthModeNone,
			Enabled:           true,
			CreatedAt:         s.now(),
		}
		if err := s.store.PutHTTPTrigger(ctx, trigger); err != nil {
			if errors.Is(err, store.ErrAlreadyExists) {
				continue
			}
			return fmt.Errorf("create default http trigger for function version %s: %w", version.ID, err)
		}
		recorder.Printf("created default public http trigger for function version %s", version.ID)
		return nil
	}
	return fmt.Errorf("create default http trigger for function version %s: exhausted token retries", version.ID)
}

func generateHTTPTriggerToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func buildMetadataForRequest(req domain.DeployRequest) map[string]string {
	metadata := make(map[string]string)
	if environment := deploymentEnvironment(req.Source); environment != "" {
		metadata["environment"] = environment
	}
	if branch := strings.TrimSpace(req.Source.GitRef); branch != "" {
		metadata["branch"] = branch
	}
	if gitURL := strings.TrimSpace(req.Source.GitURL); gitURL != "" {
		metadata["gitUrl"] = sanitizeGitURL(gitURL)
	}
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

func deploymentEnvironment(source domain.DeploySource) string {
	if source.Metadata != nil {
		if environment := strings.TrimSpace(source.Metadata["environment"]); environment != "" {
			return environment
		}
	}
	if source.Labels != nil {
		if environment := strings.TrimSpace(source.Labels["environment"]); environment != "" {
			return environment
		}
	}
	return ""
}

func mergeBuildMetadata(base map[string]string, extra map[string]string) map[string]string {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	merged := make(map[string]string, len(base)+len(extra))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range extra {
		if strings.TrimSpace(value) == "" {
			continue
		}
		merged[key] = value
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func deployRequestHash(req domain.DeployRequest) (string, error) {
	sanitized := sanitizeDeployRequestForStorage(req)
	return idempotency.Hash(struct {
		ProjectID     string               `json:"projectId"`
		Name          string               `json:"name"`
		Runtime       string               `json:"runtime"`
		Entrypoint    string               `json:"entrypoint"`
		MemoryMB      int                  `json:"memoryMb"`
		TimeoutSec    int                  `json:"timeoutSec"`
		NetworkPolicy domain.NetworkPolicy `json:"networkPolicy"`
		Regions       []string             `json:"regions"`
		EnvVars       map[string]string    `json:"envVars,omitempty"`
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
		EnvVars:       cloneEnvVars(req.EnvVars),
		EnvRefs:       append([]string(nil), req.EnvRefs...),
		MaxRetries:    req.MaxRetries,
		Source:        sanitized.Source,
	})
}

func (s *Service) prepareBuildOutput(ctx context.Context, req domain.DeployRequest, recorder *buildRecorder) (*preparedBuildOutput, error) {
	switch req.Source.Type {
	case domain.SourceTypeBundle:
		entrypoint := req.Entrypoint
		functionManifest, err := json.MarshalIndent(map[string]any{
			"name":          req.Name,
			"runtime":       req.Runtime,
			"entrypoint":    entrypoint,
			"memoryMb":      req.MemoryMB,
			"timeoutSec":    req.TimeoutSec,
			"networkPolicy": req.NetworkPolicy,
			"regions":       req.Regions,
			"envVars":       req.EnvVars,
			"envRefs":       req.EnvRefs,
			"maxRetries":    req.MaxRetries,
		}, "", "  ")
		if err != nil {
			return nil, err
		}
		startup, err := json.MarshalIndent(map[string]any{
			"entrypoint": entrypoint,
			"runtime":    req.Runtime,
			"preparedAt": s.now(),
		}, "", "  ")
		if err != nil {
			return nil, err
		}
		recorder.Printf("validating bundle source")
		files := make(map[string][]byte)
		for name, content := range req.Source.InlineFiles {
			files[name] = []byte(content)
		}
		if req.Source.BundleBase64 != "" {
			recorder.Printf("decoding bundleBase64 payload")
			decoded, err := base64.StdEncoding.DecodeString(req.Source.BundleBase64)
			if err != nil {
				return nil, err
			}
			tmpDir, err := os.MkdirTemp("", "lecrev-bundle-*")
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(tmpDir)
			if err := artifact.ExtractTarGz(decoded, tmpDir); err != nil {
				return nil, err
			}
			recorder.Printf("extracted uploaded bundle archive for smoke validation")
			bundle, err := artifact.BundleFromDirectory(tmpDir, map[string][]byte{
				"function.json": functionManifest,
				"startup.json":  startup,
			})
			if err != nil {
				return nil, err
			}
			return &preparedBuildOutput{
				Bundle:     bundle,
				Startup:    startup,
				Entrypoint: entrypoint,
			}, nil
		}
		files["function.json"] = functionManifest
		files["startup.json"] = startup
		recorder.Printf("packaging inline source files into immutable bundle")
		bundle, err := artifact.BundleFromFiles(files)
		if err != nil {
			return nil, err
		}
		return &preparedBuildOutput{
			Bundle:     bundle,
			Startup:    startup,
			Entrypoint: entrypoint,
		}, nil
	case domain.SourceTypeGit:
		root, metadata, cleanup, site, err := s.prepareGitWorkspace(ctx, req, recorder)
		if err != nil {
			return nil, err
		}
		entrypoint := req.Entrypoint
		bundleRoot := root
		if site != nil {
			entrypoint = site.Entrypoint
			bundleRoot = site.BundleRoot
			if metadata == nil {
				metadata = make(map[string]string)
			}
			metadata["framework"] = "nextjs"
			metadata["deliveryKind"] = "website"
		}
		functionManifest, err := json.MarshalIndent(map[string]any{
			"name":          req.Name,
			"runtime":       req.Runtime,
			"entrypoint":    entrypoint,
			"memoryMb":      req.MemoryMB,
			"timeoutSec":    req.TimeoutSec,
			"networkPolicy": req.NetworkPolicy,
			"regions":       req.Regions,
			"envVars":       req.EnvVars,
			"envRefs":       req.EnvRefs,
			"maxRetries":    req.MaxRetries,
		}, "", "  ")
		if err != nil {
			cleanup()
			return nil, err
		}
		startup, err := json.MarshalIndent(map[string]any{
			"entrypoint": entrypoint,
			"runtime":    req.Runtime,
			"preparedAt": s.now(),
		}, "", "  ")
		if err != nil {
			cleanup()
			return nil, err
		}
		recorder.Printf("packaging prepared git workspace from %s", bundleRoot)
		bundle, err := artifact.BundleFromDirectory(bundleRoot, map[string][]byte{
			"function.json": functionManifest,
			"startup.json":  startup,
		})
		if err != nil {
			cleanup()
			return nil, err
		}
		return &preparedBuildOutput{
			Bundle:     bundle,
			Startup:    startup,
			Metadata:   metadata,
			Entrypoint: entrypoint,
			Cleanup:    cleanup,
			Site:       site,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported source type %q", req.Source.Type)
	}
}

type packageManifest struct {
	PackageManager  string            `json:"packageManager"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

type nodePackageManager string

const (
	nodePackageManagerNPM  nodePackageManager = "npm"
	nodePackageManagerYarn nodePackageManager = "yarn"
	nodePackageManagerPNPM nodePackageManager = "pnpm"
)

func (s *Service) prepareGitWorkspace(ctx context.Context, req domain.DeployRequest, recorder *buildRecorder) (string, map[string]string, func(), *preparedNextSite, error) {
	if strings.TrimSpace(req.Source.GitURL) == "" {
		return "", nil, nil, nil, fmt.Errorf("gitUrl is required for git source")
	}

	tmpDir, err := os.MkdirTemp("", "lecrev-git-*")
	if err != nil {
		return "", nil, nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	cloneArgs := []string{"clone", "--depth", "1"}
	if req.Source.GitRef != "" {
		cloneArgs = append(cloneArgs, "--branch", req.Source.GitRef)
	}
	cloneArgs = append(cloneArgs, req.Source.GitURL, tmpDir)
	if err := runGitCloneWithTimeout(ctx, s.gitCloneTimeout, recorder, cloneArgs...); err != nil {
		cleanup()
		return "", nil, nil, nil, fmt.Errorf("git clone failed: %w", err)
	}
	metadata := buildMetadataForRequest(req)
	metadata = mergeBuildMetadata(metadata, captureGitMetadata(ctx, recorder, tmpDir))

	root, err := resolveBuildRoot(tmpDir, req.Source.SubPath)
	if err != nil {
		cleanup()
		return "", nil, nil, nil, err
	}
	recorder.Printf("resolved build root to %s", root)
	if err := prepareNodeWorkspace(ctx, recorder, root, req.EnvVars, s.npmInstallTimeout, s.npmBuildTimeout, s.npmPruneTimeout); err != nil {
		cleanup()
		return "", nil, nil, nil, err
	}
	site, err := maybePrepareNextSite(root, recorder)
	if err != nil {
		cleanup()
		return "", nil, nil, nil, err
	}
	if site == nil {
		if err := verifyEntrypoint(root, req.Entrypoint); err != nil {
			cleanup()
			return "", nil, nil, nil, err
		}
	}
	return root, metadata, cleanup, site, nil
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

func prepareNodeWorkspace(ctx context.Context, recorder *buildRecorder, root string, envVars map[string]string, installTimeout, buildTimeout, pruneTimeout time.Duration) error {
	pkgPath := filepath.Join(root, "package.json")
	rawPkg, err := os.ReadFile(pkgPath)
	if errors.Is(err, os.ErrNotExist) {
		recorder.Printf("package.json not present; skipping dependency install/build")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read package.json: %w", err)
	}

	var pkg packageManifest
	if err := json.Unmarshal(rawPkg, &pkg); err != nil {
		return fmt.Errorf("decode package.json: %w", err)
	}

	manager, err := detectPackageManager(root, pkg.PackageManager)
	if err != nil {
		return err
	}
	hasBuildScript := strings.TrimSpace(pkg.Scripts["build"]) != ""
	buildEnv := envVarList(envVars)
	if manager != nodePackageManagerNPM {
		buildEnv = append(buildEnv, "COREPACK_ENABLE_DOWNLOAD_PROMPT=0")
	}
	installName, installArgs := nodeInstallCommand(root, manager, pkg.PackageManager, hasBuildScript)
	if err := runCommandWithTimeoutAndEnv(ctx, installTimeout, recorder, root, buildEnv, installName, installArgs...); err != nil {
		return fmt.Errorf("install %s dependencies: %w", manager, err)
	}
	if !hasBuildScript {
		recorder.Printf("no build script detected; skipping package build")
		return nil
	}
	if isNextWorkspace(pkg) {
		if err := ensureNextStandaloneConfig(root, recorder); err != nil {
			return fmt.Errorf("prepare next.js standalone config: %w", err)
		}
		buildEnv = append(buildEnv, "NEXT_PRIVATE_STANDALONE=true")
		recorder.Printf("detected next.js workspace; enabling standalone build output")
	}
	buildName, buildArgs := nodeBuildCommand(manager)
	if err := runCommandWithTimeoutAndEnv(ctx, buildTimeout, recorder, root, buildEnv, buildName, buildArgs...); err != nil {
		return fmt.Errorf("run %s build: %w", manager, err)
	}
	pruneName, pruneArgs, shouldPrune := nodePruneCommand(root, manager, pkg.PackageManager)
	if shouldPrune {
		if err := runCommandWithTimeoutAndEnv(ctx, pruneTimeout, recorder, root, buildEnv, pruneName, pruneArgs...); err != nil {
			return fmt.Errorf("prune %s devDependencies: %w", manager, err)
		}
	}
	return nil
}

func detectPackageManager(root, packageManager string) (nodePackageManager, error) {
	trimmed := strings.TrimSpace(packageManager)
	switch {
	case strings.HasPrefix(trimmed, "npm@"), trimmed == "npm":
		return nodePackageManagerNPM, nil
	case strings.HasPrefix(trimmed, "yarn@"), trimmed == "yarn":
		return nodePackageManagerYarn, nil
	case strings.HasPrefix(trimmed, "pnpm@"), trimmed == "pnpm":
		return nodePackageManagerPNPM, nil
	case strings.HasPrefix(trimmed, "bun@"), trimmed == "bun":
		return "", fmt.Errorf("unsupported package manager %q", packageManager)
	}
	if hasNpmLockfile(root) {
		return nodePackageManagerNPM, nil
	}
	if hasLockfile(root, "yarn.lock") {
		return nodePackageManagerYarn, nil
	}
	if hasLockfile(root, "pnpm-lock.yaml") {
		return nodePackageManagerPNPM, nil
	}
	if hasLockfile(root, "bun.lock") || hasLockfile(root, "bun.lockb") {
		return "", fmt.Errorf("unsupported lockfile detected: bun is not currently supported")
	}
	return nodePackageManagerNPM, nil
}

func hasNpmLockfile(root string) bool {
	for _, name := range []string{"package-lock.json", "npm-shrinkwrap.json"} {
		if hasLockfile(root, name) {
			return true
		}
	}
	return false
}

func hasLockfile(root, name string) bool {
	_, err := os.Stat(filepath.Join(root, name))
	return err == nil
}

func nodeInstallCommand(root string, manager nodePackageManager, packageManager string, hasBuildScript bool) (string, []string) {
	switch manager {
	case nodePackageManagerYarn:
		args := []string{"yarn", "install"}
		if usesModernYarn(root, packageManager) {
			args = append(args, "--immutable")
		} else {
			args = append(args, "--frozen-lockfile")
		}
		return "corepack", args
	case nodePackageManagerPNPM:
		return "corepack", []string{"pnpm", "install", "--frozen-lockfile"}
	default:
		args := []string{"install"}
		if hasNpmLockfile(root) {
			args[0] = "ci"
		}
		if !hasBuildScript {
			args = append(args, "--omit=dev")
		}
		return "npm", args
	}
}

func nodeBuildCommand(manager nodePackageManager) (string, []string) {
	switch manager {
	case nodePackageManagerYarn:
		return "corepack", []string{"yarn", "build"}
	case nodePackageManagerPNPM:
		return "corepack", []string{"pnpm", "build"}
	default:
		return "npm", []string{"run", "build", "--if-present"}
	}
}

func nodePruneCommand(root string, manager nodePackageManager, packageManager string) (string, []string, bool) {
	switch manager {
	case nodePackageManagerPNPM:
		return "corepack", []string{"pnpm", "prune", "--prod"}, true
	case nodePackageManagerYarn:
		if usesModernYarn(root, packageManager) {
			return "", nil, false
		}
		return "corepack", []string{"yarn", "install", "--frozen-lockfile", "--production=true", "--ignore-scripts"}, true
	default:
		return "npm", []string{"prune", "--omit=dev"}, true
	}
}

func usesModernYarn(root, packageManager string) bool {
	if hasLockfile(root, ".yarnrc.yml") {
		return true
	}
	trimmed := strings.TrimSpace(packageManager)
	if !strings.HasPrefix(trimmed, "yarn@") {
		return false
	}
	version := strings.TrimPrefix(trimmed, "yarn@")
	major := version
	if idx := strings.IndexAny(major, ".+-"); idx >= 0 {
		major = major[:idx]
	}
	return major != "" && major != "0" && major != "1"
}

func ensureNextStandaloneConfig(root string, recorder *buildRecorder) error {
	candidates := []string{
		"next.config.ts",
		"next.config.mjs",
		"next.config.js",
		"next.config.cjs",
	}
	for _, name := range candidates {
		configPath := filepath.Join(root, name)
		if _, err := os.Stat(configPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		backupName := strings.Replace(name, "next.config.", "next.config.lecrev.orig.", 1)
		backupPath := filepath.Join(root, backupName)
		if _, err := os.Stat(backupPath); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			if err := os.Rename(configPath, backupPath); err != nil {
				return fmt.Errorf("backup %s: %w", name, err)
			}
		}
		wrapper := nextStandaloneConfigWrapper(name, backupName)
		if err := os.WriteFile(configPath, []byte(wrapper), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
		if recorder != nil {
			recorder.Printf("wrapped %s to force next.js standalone output", name)
		}
		return nil
	}

	configPath := filepath.Join(root, "next.config.mjs")
	if err := os.WriteFile(configPath, []byte("export default { output: 'standalone' };\n"), 0o644); err != nil {
		return fmt.Errorf("write next.config.mjs: %w", err)
	}
	if recorder != nil {
		recorder.Printf("generated next.config.mjs to force next.js standalone output")
	}
	return nil
}

func nextStandaloneConfigWrapper(configName, backupName string) string {
	switch filepath.Ext(configName) {
	case ".mjs":
		return fmt.Sprintf(`import originalConfig from './%s';

const withStandalone = (config) => ({ ...(config ?? {}), output: 'standalone' });

export default async function lecrevNextConfig(...args) {
  const resolved = typeof originalConfig === 'function' ? await originalConfig(...args) : await originalConfig;
  return withStandalone(resolved);
}
`, backupName)
	case ".ts":
		return fmt.Sprintf(`import originalConfig from './%s';

const withStandalone = (config: Record<string, unknown> | undefined | null) => ({ ...(config ?? {}), output: 'standalone' });

export default async function lecrevNextConfig(...args: any[]) {
  const resolved = typeof originalConfig === 'function' ? await originalConfig(...args) : await originalConfig;
  return withStandalone((resolved ?? undefined) as Record<string, unknown> | undefined);
}
`, backupName)
	default:
		return fmt.Sprintf(`const originalModule = require('./%s');
const originalConfig = originalModule && typeof originalModule === 'object' && 'default' in originalModule
  ? originalModule.default
  : originalModule;

const withStandalone = (config) => ({ ...(config ?? {}), output: 'standalone' });

module.exports = async (...args) => {
  const resolved = typeof originalConfig === 'function' ? await originalConfig(...args) : await originalConfig;
  return withStandalone(resolved);
};
`, backupName)
	}
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

func (s *Service) archiveBuildLogs(ctx context.Context, buildJob *domain.BuildJob, recorder *buildRecorder) error {
	if recorder == nil {
		return nil
	}
	key := artifact.BuildLogsKey(buildJob.ID)
	if err := s.objects.Put(ctx, key, recorder.Bytes()); err != nil {
		return err
	}
	buildJob.LogsKey = key
	return nil
}

func (s *Service) enforceBuildQuota(ctx context.Context, projectID string) error {
	if s.maxActiveBuildJobsPerProject <= 0 {
		return nil
	}
	count, err := s.store.CountBuildJobsByProjectStates(ctx, projectID, []string{"queued", "running"})
	if err != nil {
		return err
	}
	if count >= s.maxActiveBuildJobsPerProject {
		return fmt.Errorf("%w: project %s already has %d active build jobs (limit %d)", domain.ErrProjectBuildQuota, projectID, count, s.maxActiveBuildJobsPerProject)
	}
	return nil
}

type buildRecorder struct {
	now      func() time.Time
	builder  strings.Builder
	onUpdate func([]byte)
}

func newBuildRecorder(now func() time.Time) *buildRecorder {
	return &buildRecorder{now: now}
}

func (r *buildRecorder) Printf(format string, args ...any) {
	if r == nil {
		return
	}
	timestamp := time.Now().UTC()
	if r.now != nil {
		timestamp = r.now()
	}
	r.builder.WriteString("[")
	r.builder.WriteString(timestamp.Format(time.RFC3339))
	r.builder.WriteString("] ")
	r.builder.WriteString(fmt.Sprintf(format, args...))
	if !strings.HasSuffix(format, "\n") {
		r.builder.WriteString("\n")
	}
	if r.onUpdate != nil {
		r.onUpdate(r.Bytes())
	}
}

func (r *buildRecorder) Bytes() []byte {
	if r == nil {
		return nil
	}
	return []byte(r.builder.String())
}

func captureGitMetadata(ctx context.Context, recorder *buildRecorder, repoRoot string) map[string]string {
	metadata := make(map[string]string)

	commitSHA, err := runCommandOutput(ctx, recorder, repoRoot, "git", "rev-parse", "HEAD")
	if err != nil {
		if recorder != nil {
			recorder.Printf("warning: unable to resolve git commit sha: %v", err)
		}
	} else if commitSHA != "" {
		metadata["commitSha"] = commitSHA
	}

	branch, err := runCommandOutput(ctx, recorder, repoRoot, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		if recorder != nil {
			recorder.Printf("warning: unable to resolve git branch: %v", err)
		}
	} else if branch != "" && branch != "HEAD" {
		metadata["branch"] = branch
	}

	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

func runCommand(ctx context.Context, recorder *buildRecorder, dir, name string, args ...string) error {
	_, err := runCommandOutput(ctx, recorder, dir, name, args...)
	return err
}

func runCommandWithTimeout(ctx context.Context, timeout time.Duration, recorder *buildRecorder, dir, name string, args ...string) error {
	return runCommandWithTimeoutAndEnv(ctx, timeout, recorder, dir, nil, name, args...)
}

func runCommandWithTimeoutAndEnv(ctx context.Context, timeout time.Duration, recorder *buildRecorder, dir string, env []string, name string, args ...string) error {
	commandCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		commandCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	_, err := runCommandOutputWithEnv(commandCtx, recorder, dir, env, name, args...)
	return err
}

func runGitCloneWithTimeout(ctx context.Context, timeout time.Duration, recorder *buildRecorder, args ...string) error {
	commandCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		commandCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	cmd := exec.CommandContext(commandCtx, "git", args...)
	if recorder != nil {
		recorder.Printf("$ git %s", strings.Join(redactGitCloneArgs(args), " "))
	}
	output, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(output))
	if recorder != nil && len(output) > 0 {
		recorder.Printf("%s", trimmed)
	}
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(redactGitCloneArgs(args), " "), err, trimmed)
	}
	return nil
}

func runCommandOutput(ctx context.Context, recorder *buildRecorder, dir, name string, args ...string) (string, error) {
	return runCommandOutputWithEnv(ctx, recorder, dir, nil, name, args...)
}

func runCommandOutputWithEnv(ctx context.Context, recorder *buildRecorder, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if recorder != nil {
		if dir != "" {
			recorder.Printf("$ (cd %s && %s %s)", dir, name, strings.Join(args, " "))
		} else {
			recorder.Printf("$ %s %s", name, strings.Join(args, " "))
		}
	}
	output, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(output))
	if recorder != nil && len(output) > 0 {
		recorder.Printf("%s", trimmed)
	}
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, trimmed)
	}
	return trimmed, nil
}

func redactGitCloneArgs(args []string) []string {
	redacted := make([]string, len(args))
	for i, arg := range args {
		redacted[i] = arg
	}
	if len(redacted) >= 2 {
		redacted[len(redacted)-2] = sanitizeGitURL(redacted[len(redacted)-2])
	}
	return redacted
}

func sanitizeGitURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return trimmed
	}
	parsed.User = nil
	return parsed.String()
}

func sanitizeDeployRequestForStorage(req domain.DeployRequest) domain.DeployRequest {
	sanitized := req
	sanitized.Source = req.Source
	sanitized.Source.GitURL = sanitizeGitURL(req.Source.GitURL)
	return sanitized
}

func validateEnvVar(key, value string) error {
	name := strings.TrimSpace(key)
	if name == "" {
		return fmt.Errorf("envVars cannot contain empty keys")
	}
	for i, r := range name {
		isLetter := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		if i == 0 {
			if !isLetter && r != '_' {
				return fmt.Errorf("envVars key %q must start with a letter or underscore", key)
			}
			continue
		}
		if !isLetter && !isDigit && r != '_' {
			return fmt.Errorf("envVars key %q contains unsupported character %q", key, string(r))
		}
	}
	if len(value) > 16<<10 {
		return fmt.Errorf("envVars value for %q exceeds 16384 bytes", key)
	}
	return nil
}

func cloneEnvVars(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func envVarList(envVars map[string]string) []string {
	if len(envVars) == 0 {
		return nil
	}
	env := make([]string, 0, len(envVars))
	for key, value := range envVars {
		env = append(env, key+"="+value)
	}
	return env
}
