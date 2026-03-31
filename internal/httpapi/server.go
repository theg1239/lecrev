package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/theg1239/lecrev/internal/apikey"
	"github.com/theg1239/lecrev/internal/artifact"
	"github.com/theg1239/lecrev/internal/build"
	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/firecracker"
	"github.com/theg1239/lecrev/internal/metrics"
	"github.com/theg1239/lecrev/internal/scheduler"
	"github.com/theg1239/lecrev/internal/store"
)

type Server struct {
	store     store.Store
	objects   artifact.Store
	builder   *build.Service
	scheduler *scheduler.Service
	admins    map[string]RegionAdmin
}

type RegionAdmin interface {
	Region() string
	DrainHost(ctx context.Context, hostID, reason string) error
}

func New(store store.Store, objects artifact.Store, builder *build.Service, scheduler *scheduler.Service, admins ...RegionAdmin) http.Handler {
	adminIndex := make(map[string]RegionAdmin, len(admins))
	for _, admin := range admins {
		adminIndex[admin.Region()] = admin
	}
	srv := &Server{
		store:     store,
		objects:   objects,
		builder:   builder,
		scheduler: scheduler,
		admins:    adminIndex,
	}
	r := chi.NewRouter()
	r.Use(srv.corsMiddleware)
	r.Options("/*", srv.handlePreflight)
	r.Get("/healthz", srv.healthz)
	r.Handle("/metrics", metrics.NewHandler(store))
	r.HandleFunc("/f/{token}", srv.invokeHTTPTrigger)
	r.HandleFunc("/f/{token}/*", srv.invokeHTTPTrigger)
	r.HandleFunc("/w/{versionID}", srv.serveWebsite)
	r.HandleFunc("/w/{versionID}/*", srv.serveWebsite)
	r.Post("/v1/triggers/webhook/{token}", srv.invokeWebhook)
	r.Route("/v1", func(r chi.Router) {
		r.Use(srv.authMiddleware)
		r.Options("/*", srv.handlePreflight)
		r.Post("/projects", srv.createProject)
		r.Get("/deployments", srv.listDeployments)
		r.Get("/deployments/{deploymentID}", srv.getDeployment)
		r.Get("/deployments/{deploymentID}/logs", srv.getDeploymentLogs)
		r.Get("/deployments/{deploymentID}/output", srv.getDeploymentOutput)
		r.Get("/projects", srv.listProjects)
		r.Get("/projects/{projectID}", srv.getProject)
		r.Get("/projects/{projectID}/overview", srv.getProjectOverview)
		r.Get("/projects/{projectID}/deployments", srv.listProjectDeployments)
		r.Get("/projects/{projectID}/functions", srv.listProjectFunctions)
		r.Get("/projects/{projectID}/build-jobs", srv.listProjectBuildJobs)
		r.Get("/projects/{projectID}/jobs", srv.listProjectJobs)
		r.Post("/projects/{projectID}/functions", srv.createFunction)
		r.Get("/build-jobs/{jobID}", srv.getBuildJob)
		r.Get("/build-jobs/{jobID}/logs", srv.getBuildJobLogs)
		r.Post("/functions/{versionID}/invoke", srv.invokeFunction)
		r.Get("/jobs/{jobID}", srv.getJob)
		r.Get("/jobs/{jobID}/logs", srv.getJobLogs)
		r.Get("/jobs/{jobID}/output", srv.getJobOutput)
		r.Get("/functions/{versionID}", srv.getFunction)
		r.Get("/functions/{versionID}/site", srv.getFunctionSite)
		r.Get("/functions/{versionID}/warm-status", srv.getFunctionWarmStatus)
		r.Post("/functions/{versionID}/prepare", srv.prepareFunction)
		r.Post("/functions/{versionID}/triggers/http", srv.createHTTPTrigger)
		r.Get("/functions/{versionID}/triggers/http", srv.listHTTPTriggers)
		r.Post("/functions/{versionID}/triggers/webhook", srv.createWebhookTrigger)
		r.Get("/functions/{versionID}/triggers/webhook", srv.listWebhookTriggers)
		r.Get("/regions", srv.listRegions)
		r.Get("/regions/{region}/warm-pools", srv.listWarmPools)
		r.Get("/regions/{region}/hosts", srv.listRegionHosts)
		r.Get("/jobs/{jobID}/attempts", srv.listJobAttempts)
		r.Get("/jobs/{jobID}/costs", srv.listJobCosts)
		r.Post("/regions/{region}/hosts/{hostID}/drain", srv.drainHost)
	})
	return r
}

const (
	defaultProjectListLimit    = 50
	defaultOverviewItemLimit   = 8
	maxListLimit               = 200
	httpTriggerJobPollInterval = 25 * time.Millisecond
)

var (
	errMissingAPIKey = errors.New("missing api key")
	errInvalidAPIKey = errors.New("invalid api key")
)

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, err := s.authenticateAPIKey(r.Context(), apiKeyFromRequest(r))
		if err != nil {
			switch {
			case errors.Is(err, errMissingAPIKey):
				http.Error(w, "missing X-API-Key", http.StatusUnauthorized)
			case errors.Is(err, errInvalidAPIKey):
				http.Error(w, "invalid X-API-Key", http.StatusUnauthorized)
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		next.ServeHTTP(w, r.WithContext(withAuthContext(r.Context(), auth)))
	})
}

func (s *Server) authenticateAPIKey(ctx context.Context, rawKey string) (authContext, error) {
	rawKey = strings.TrimSpace(rawKey)
	if rawKey == "" {
		return authContext{}, errMissingAPIKey
	}
	keyHash := apikey.Hash(rawKey)
	record, err := s.store.GetAPIKeyByHash(ctx, keyHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return authContext{}, errInvalidAPIKey
		}
		return authContext{}, err
	}
	if record.Disabled || subtle.ConstantTimeCompare([]byte(record.KeyHash), []byte(keyHash)) != 1 {
		return authContext{}, errInvalidAPIKey
	}
	_ = s.store.TouchAPIKeyLastUsed(ctx, keyHash, time.Now().UTC())
	return authContext{
		TenantID: record.TenantID,
		IsAdmin:  record.IsAdmin,
	}, nil
}

func apiKeyFromRequest(r *http.Request) string {
	if rawKey := strings.TrimSpace(r.Header.Get("X-API-Key")); rawKey != "" {
		return rawKey
	}
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authz) >= len("Bearer ") && strings.EqualFold(authz[:len("Bearer ")], "Bearer ") {
		return strings.TrimSpace(authz[len("Bearer "):])
	}
	return ""
}

func (s *Server) authContextForHTTPTrigger(ctx context.Context, r *http.Request, trigger *domain.HTTPTrigger) (context.Context, error) {
	if trigger == nil {
		return ctx, fmt.Errorf("http trigger is required")
	}
	switch trigger.AuthMode {
	case domain.HTTPTriggerAuthModeNone:
		return ctx, nil
	case domain.HTTPTriggerAuthModeAPIKey:
		auth, err := s.authenticateAPIKey(ctx, apiKeyFromRequest(r))
		if err != nil {
			return nil, err
		}
		return withAuthContext(ctx, auth), nil
	default:
		return nil, fmt.Errorf("unsupported http trigger authMode %q", trigger.AuthMode)
	}
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"status": "healthy",
	})
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var body createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	projectID := normalizeProjectID(strings.TrimSpace(body.ID), name)
	if projectID == "" {
		http.Error(w, "project id is required", http.StatusBadRequest)
		return
	}

	project, err := s.store.EnsureProject(ctx, projectID, tenantIDFromContext(r.Context()), name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, project)
}

func (s *Server) createFunction(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if projectID == "" {
		http.Error(w, "missing projectID", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if _, err := s.store.EnsureProject(ctx, projectID, tenantIDFromContext(r.Context()), projectID); err != nil {
		writeServiceError(w, err)
		return
	}

	var body createFunctionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	var source domain.DeploySource
	if err := json.Unmarshal(body.Source, &source); err != nil {
		http.Error(w, fmt.Sprintf("decode source: %v", err), http.StatusBadRequest)
		return
	}
	if environment := strings.ToLower(strings.TrimSpace(body.Environment)); environment != "" {
		if source.Metadata == nil {
			source.Metadata = make(map[string]string)
		}
		source.Metadata["environment"] = environment
	}
	version, err := s.builder.CreateFunctionVersion(ctx, domain.DeployRequest{
		ProjectID:      projectID,
		Name:           body.Name,
		Runtime:        body.Runtime,
		Entrypoint:     body.Entrypoint,
		MemoryMB:       body.MemoryMB,
		TimeoutSec:     body.TimeoutSec,
		NetworkPolicy:  domain.NetworkPolicy(body.NetworkPolicy),
		Regions:        body.Regions,
		EnvVars:        cloneStringMap(body.EnvVars),
		EnvRefs:        body.EnvRefs,
		MaxRetries:     body.MaxRetries,
		IdempotencyKey: body.IdempotencyKey,
		Source:         source,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, version)
}

func normalizeProjectID(rawID, name string) string {
	source := strings.TrimSpace(rawID)
	if source == "" {
		source = name
	}
	source = strings.ToLower(source)
	var b strings.Builder
	lastDash := false
	for _, r := range source {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == ' ':
			if b.Len() == 0 || lastDash {
				continue
			}
			b.WriteByte('-')
			lastDash = true
		default:
			if b.Len() == 0 || lastDash {
				continue
			}
			b.WriteByte('-')
			lastDash = true
		}
	}
	normalized := strings.Trim(b.String(), "-")
	if normalized == "" {
		return ""
	}
	return normalized
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjectsByTenant(r.Context(), tenantIDFromContext(r.Context()))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	s.listDeploymentsForProjects(w, r, projects)
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	limit, err := queryLimit(r, defaultProjectListLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	projects, err := s.store.ListProjectsByTenant(r.Context(), tenantIDFromContext(r.Context()))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if len(projects) > limit {
		projects = projects[:limit]
	}
	writeJSON(w, http.StatusOK, projects)
}

func (s *Server) listDeploymentsForProjects(w http.ResponseWriter, r *http.Request, projects []domain.Project) {
	limit, err := queryLimit(r, defaultProjectListLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	statusFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	environmentFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("environment")))

	deployments := make([]deploymentSummary, 0)
	for _, project := range projects {
		versions, err := s.store.ListFunctionVersionsByProject(r.Context(), project.ID)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		buildJobs, err := s.store.ListBuildJobsByProject(r.Context(), project.ID)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		jobs, err := s.store.ListExecutionJobsByProject(r.Context(), project.ID)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		deployments = append(deployments, summarizeDeployments(project, versions, buildJobs, jobs)...)
	}

	deployments = filterDeploymentSummaries(deployments, statusFilter, environmentFilter)
	sortDeployments(deployments)
	if len(deployments) > limit {
		deployments = deployments[:limit]
	}
	writeJSON(w, http.StatusOK, deployments)
}

func (s *Server) listProjectDeployments(w http.ResponseWriter, r *http.Request) {
	project, err := s.authorizedProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	s.listDeploymentsForProjects(w, r, []domain.Project{*project})
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	deployment, _, _, err := s.loadAuthorizedDeployment(r.Context(), chi.URLParam(r, "deploymentID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deployment)
}

func (s *Server) getDeploymentLogs(w http.ResponseWriter, r *http.Request) {
	_, buildJob, latestJob, err := s.loadAuthorizedDeployment(r.Context(), chi.URLParam(r, "deploymentID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if latestJob != nil && latestJob.Result != nil {
		data, err := s.executionLogsData(r.Context(), latestJob)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeRaw(w, http.StatusOK, "text/plain; charset=utf-8", data)
		return
	}
	if buildJob != nil && strings.TrimSpace(buildJob.LogsKey) != "" {
		data, err := s.objects.Get(r.Context(), buildJob.LogsKey)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeRaw(w, http.StatusOK, "text/plain; charset=utf-8", data)
		return
	}
	if latestJob != nil {
		writeServiceError(w, domain.ErrExecutionResultNotReady)
		return
	}
	writeServiceError(w, domain.ErrBuildLogsNotReady)
}

func (s *Server) getDeploymentOutput(w http.ResponseWriter, r *http.Request) {
	_, _, latestJob, err := s.loadAuthorizedDeployment(r.Context(), chi.URLParam(r, "deploymentID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if latestJob == nil {
		writeServiceError(w, domain.ErrExecutionResultNotReady)
		return
	}
	data, err := s.executionOutputData(r.Context(), latestJob)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeRaw(w, http.StatusOK, "application/json", data)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.authorizedProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) getProjectOverview(w http.ResponseWriter, r *http.Request) {
	project, err := s.authorizedProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	limit, err := queryLimit(r, defaultOverviewItemLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	versions, err := s.store.ListFunctionVersionsByProject(r.Context(), project.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	buildJobs, err := s.store.ListBuildJobsByProject(r.Context(), project.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	jobs, err := s.store.ListExecutionJobsByProject(r.Context(), project.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, projectOverviewResponse{
		Project: *project,
		Totals: projectOverviewTotals{
			Functions:     len(versions),
			BuildsByState: countBuildJobsByState(buildJobs),
			JobsByState:   countExecutionJobsByState(jobs),
		},
		RecentFunctions: summarizeFunctionVersions(limitFunctionVersions(versions, limit)),
		RecentBuildJobs: summarizeBuildJobs(limitBuildJobs(buildJobs, limit)),
		RecentJobs:      summarizeExecutionJobs(limitExecutionJobs(jobs, limit)),
	})
}

func (s *Server) listProjectFunctions(w http.ResponseWriter, r *http.Request) {
	project, err := s.authorizedProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	limit, err := queryLimit(r, defaultProjectListLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	versions, err := s.store.ListFunctionVersionsByProject(r.Context(), project.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summarizeFunctionVersions(limitFunctionVersions(versions, limit)))
}

func (s *Server) listProjectBuildJobs(w http.ResponseWriter, r *http.Request) {
	project, err := s.authorizedProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	limit, err := queryLimit(r, defaultProjectListLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	buildJobs, err := s.store.ListBuildJobsByProject(r.Context(), project.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summarizeBuildJobs(limitBuildJobs(buildJobs, limit)))
}

func (s *Server) listProjectJobs(w http.ResponseWriter, r *http.Request) {
	project, err := s.authorizedProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	limit, err := queryLimit(r, defaultProjectListLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	jobs, err := s.store.ListExecutionJobsByProject(r.Context(), project.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summarizeExecutionJobs(limitExecutionJobs(jobs, limit)))
}

func (s *Server) invokeFunction(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	if versionID == "" {
		http.Error(w, "missing versionID", http.StatusBadRequest)
		return
	}
	version, err := s.store.GetFunctionVersion(r.Context(), versionID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if version.State != domain.FunctionStateReady {
		writeServiceError(w, domain.ErrFunctionVersionNotReady)
		return
	}
	if err := s.authorizeProject(r.Context(), version.ProjectID); err != nil {
		writeServiceError(w, err)
		return
	}
	var body invokeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	job, err := s.scheduler.DispatchExecutionIdempotent(ctx, versionID, body.Payload, idempotencyKey(r.Header.Get("Idempotency-Key"), body.IdempotencyKey))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, withJobLatency(job))
}

func (s *Server) prepareFunction(w http.ResponseWriter, r *http.Request) {
	if err := requireAdmin(r.Context()); err != nil {
		writeServiceError(w, err)
		return
	}
	versionID := chi.URLParam(r, "versionID")
	if versionID == "" {
		http.Error(w, "missing versionID", http.StatusBadRequest)
		return
	}
	version, err := s.store.GetFunctionVersion(r.Context(), versionID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if version.State != domain.FunctionStateReady {
		writeServiceError(w, domain.ErrFunctionVersionNotReady)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.scheduler.PrepareFunctionVersion(ctx, version); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":                true,
		"functionVersionId": version.ID,
		"state":             "queued",
	})
}

func (s *Server) getBuildJob(w http.ResponseWriter, r *http.Request) {
	job, _, err := s.authorizedBuildJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) getBuildJobLogs(w http.ResponseWriter, r *http.Request) {
	job, _, err := s.authorizedBuildJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if strings.TrimSpace(job.LogsKey) == "" {
		writeServiceError(w, domain.ErrBuildLogsNotReady)
		return
	}
	data, err := s.objects.Get(r.Context(), job.LogsKey)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeRaw(w, http.StatusOK, "text/plain; charset=utf-8", data)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.authorizedJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, withJobLatency(job))
}

func (s *Server) listJobAttempts(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobID")
	_, err := s.authorizedJob(r.Context(), jobID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	attempts, err := s.store.ListAttemptsByJob(r.Context(), jobID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, withAttemptLatency(attempts))
}

func (s *Server) listJobCosts(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobID")
	_, err := s.authorizedJob(r.Context(), jobID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	records, err := s.store.ListCostRecordsByJob(r.Context(), jobID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, records)
}

func (s *Server) getJobLogs(w http.ResponseWriter, r *http.Request) {
	job, err := s.authorizedJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	data, err := s.executionLogsData(r.Context(), job)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeRaw(w, http.StatusOK, "text/plain; charset=utf-8", data)
}

func (s *Server) getJobOutput(w http.ResponseWriter, r *http.Request) {
	job, err := s.authorizedJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	data, err := s.executionOutputData(r.Context(), job)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeRaw(w, http.StatusOK, "application/json", data)
}

func (s *Server) getFunction(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	version, err := s.store.GetFunctionVersion(r.Context(), versionID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := s.authorizeProject(r.Context(), version.ProjectID); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, version)
}

func (s *Server) getFunctionSite(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	version, err := s.store.GetFunctionVersion(r.Context(), versionID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := s.authorizeProject(r.Context(), version.ProjectID); err != nil {
		writeServiceError(w, err)
		return
	}
	manifest, err := s.websiteManifest(r.Context(), version)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	var functionURL string
	if triggers, err := s.store.ListHTTPTriggersByFunctionVersion(r.Context(), version.ID); err == nil {
		for _, trigger := range triggers {
			if trigger.Enabled {
				functionURL = publicFunctionURL(r, trigger.Token)
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, functionSiteResponse{
		FunctionVersionID: version.ID,
		Framework:         manifest.Framework,
		DynamicEntrypoint: manifest.DynamicEntrypoint,
		StaticPrefix:      manifest.StaticPrefix,
		PreviewURL:        publicWebsiteURL(r, version.ID),
		FunctionURL:       functionURL,
		CreatedAt:         manifest.CreatedAt,
	})
}

func (s *Server) serveWebsite(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	if versionID == "" {
		http.Error(w, "missing versionID", http.StatusBadRequest)
		return
	}
	version, err := s.store.GetFunctionVersion(r.Context(), versionID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if version.State != domain.FunctionStateReady {
		http.Error(w, "website preview not ready", http.StatusServiceUnavailable)
		return
	}
	manifest, err := s.websiteManifest(r.Context(), version)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	sitePath := websiteRequestPath(r)
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && s.serveWebsiteStaticAsset(w, r, version.ArtifactDigest, sitePath) {
		return
	}

	payload, err := buildHTTPPayloadForPath(r, "", sitePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	streamWriter := newHTTPDirectStreamWriter(w, r.Method, "", version.ID)
	directCtx, cancelDirect := context.WithTimeout(r.Context(), httpTriggerTimeout(version.TimeoutSec))
	directResult, directErr := s.scheduler.ExecuteDirectStream(
		directCtx,
		version.ID,
		payload,
		func(jobID, _ string, startMode domain.StartMode) {
			streamWriter.SetMeta(jobID, startMode)
		},
		streamWriter.HandleEvent,
	)
	cancelDirect()
	if directErr == nil {
		returnDirectHTTPResult(streamWriter, directResult)
		return
	}
	if !errors.Is(directErr, domain.ErrDirectInvokeUnavailable) && !errors.Is(directErr, domain.ErrNoExecutionCapacity) {
		if errors.Is(directErr, context.DeadlineExceeded) || errors.Is(directErr, context.Canceled) {
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{
				"functionVersionId": version.ID,
				"state":             "timeout",
				"message":           "website preview did not complete before the response deadline",
				"framework":         manifest.Framework,
			})
			return
		}
		writeServiceError(w, directErr)
		return
	}

	dispatchCtx, cancelDispatch := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancelDispatch()
	job, err := s.scheduler.DispatchExecutionIdempotent(dispatchCtx, version.ID, payload, idempotencyKey(r.Header.Get("Idempotency-Key"), ""))
	if err != nil {
		writeServiceError(w, err)
		return
	}

	waitCtx, cancelWait := context.WithTimeout(r.Context(), httpTriggerTimeout(version.TimeoutSec))
	defer cancelWait()
	job, err = s.waitForExecutionJob(waitCtx, job.ID, httpTriggerJobPollInterval)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{
				"jobId":             job.ID,
				"functionVersionId": version.ID,
				"state":             "timeout",
				"message":           "website preview did not complete before the response deadline",
			})
			return
		}
		writeServiceError(w, err)
		return
	}

	job = withJobLatency(job)
	if job.State != domain.JobStateSucceeded || job.Result == nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"jobId":             job.ID,
			"functionVersionId": version.ID,
			"state":             job.State,
			"error":             job.Error,
		})
		return
	}
	if err := writeHTTPFunctionResult(w, r.Method, job.Result.Output, jobLatencyMs(job)); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"jobId":             job.ID,
			"functionVersionId": version.ID,
			"state":             domain.JobStateFailed,
			"error":             err.Error(),
		})
	}
}

func (s *Server) listRegions(w http.ResponseWriter, r *http.Request) {
	regions, err := s.store.ListRegions(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, regions)
}

func (s *Server) listRegionHosts(w http.ResponseWriter, r *http.Request) {
	if err := requireAdmin(r.Context()); err != nil {
		writeServiceError(w, err)
		return
	}
	region := chi.URLParam(r, "region")
	hosts, err := s.store.ListHostsByRegion(r.Context(), region)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hosts)
}

func (s *Server) listWarmPools(w http.ResponseWriter, r *http.Request) {
	if err := requireAdmin(r.Context()); err != nil {
		writeServiceError(w, err)
		return
	}
	region := chi.URLParam(r, "region")
	pools, err := s.store.ListWarmPoolsByRegion(r.Context(), region)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pools)
}

func (s *Server) getFunctionWarmStatus(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	version, err := s.store.GetFunctionVersion(r.Context(), versionID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := s.authorizeProject(r.Context(), version.ProjectID); err != nil {
		writeServiceError(w, err)
		return
	}

	regions, err := s.store.ListRegions(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	regionIndex := make(map[string]domain.Region, len(regions))
	for _, region := range regions {
		regionIndex[region.Name] = region
	}

	response := functionWarmStatusResponse{
		FunctionVersionID: version.ID,
		Regions:           make([]functionWarmRegionStatus, 0, len(version.Regions)),
	}

	for _, regionName := range version.Regions {
		regionRecord, ok := regionIndex[regionName]
		regionStatus := functionWarmRegionStatus{
			Region: regionName,
			State:  "degraded",
		}
		if ok {
			regionStatus.State = regionRecord.State
			regionStatus.AvailableHosts = regionRecord.AvailableHosts
			regionStatus.AvailableFullNetworkSlots = regionRecord.AvailableFullNetworkSlots
		}

		pools, err := s.store.ListWarmPoolsByRegion(r.Context(), regionName)
		if err != nil {
			writeServiceError(w, err)
			return
		}

		for _, pool := range pools {
			regionStatus.BlankWarm += pool.BlankWarm
			if pool.FunctionVersionID != version.ID {
				continue
			}
			regionStatus.FunctionWarm += pool.FunctionWarm
			if pool.UpdatedAt.After(regionStatus.UpdatedAt) {
				regionStatus.UpdatedAt = pool.UpdatedAt
			}
		}

		regionStatus.Ready = regionStatus.State == "active" && regionStatus.FunctionWarm > 0
		if regionStatus.Ready {
			response.Ready = true
		}
		response.Regions = append(response.Regions, regionStatus)
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *Server) drainHost(w http.ResponseWriter, r *http.Request) {
	if err := requireAdmin(r.Context()); err != nil {
		writeServiceError(w, err)
		return
	}
	region := chi.URLParam(r, "region")
	hostID := chi.URLParam(r, "hostID")
	admin, ok := s.admins[region]
	if !ok {
		http.Error(w, fmt.Sprintf("no coordinator admin available for region %s", region), http.StatusNotFound)
		return
	}
	var body drainHostRequest
	if err := decodeOptionalJSON(r, &body); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	if err := admin.DrainHost(r.Context(), hostID, strings.TrimSpace(body.Reason)); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"region": region,
		"hostId": hostID,
		"state":  "draining",
		"reason": strings.TrimSpace(body.Reason),
	})
}

func (s *Server) createWebhookTrigger(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	version, err := s.store.GetFunctionVersion(r.Context(), versionID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := s.authorizeProject(r.Context(), version.ProjectID); err != nil {
		writeServiceError(w, err)
		return
	}

	var body createWebhookTriggerRequest
	if err := decodeOptionalJSON(r, &body); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(body.Token)
	if token == "" {
		token, err = generateWebhookToken()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	trigger := &domain.WebhookTrigger{
		Token:             token,
		ProjectID:         version.ProjectID,
		FunctionVersionID: version.ID,
		Description:       strings.TrimSpace(body.Description),
		Enabled:           true,
		CreatedAt:         time.Now().UTC(),
	}
	if err := s.store.PutWebhookTrigger(r.Context(), trigger); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, trigger)
}

func (s *Server) createHTTPTrigger(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	version, err := s.store.GetFunctionVersion(r.Context(), versionID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := s.authorizeProject(r.Context(), version.ProjectID); err != nil {
		writeServiceError(w, err)
		return
	}
	if version.State != domain.FunctionStateReady {
		writeServiceError(w, domain.ErrFunctionVersionNotReady)
		return
	}

	var body createHTTPTriggerRequest
	if err := decodeOptionalJSON(r, &body); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(body.Token)
	if token == "" {
		token, err = generateTriggerToken()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	authMode, err := normalizeHTTPTriggerAuthMode(body.AuthMode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	trigger := &domain.HTTPTrigger{
		Token:             token,
		ProjectID:         version.ProjectID,
		FunctionVersionID: version.ID,
		Description:       strings.TrimSpace(body.Description),
		AuthMode:          authMode,
		Enabled:           true,
		CreatedAt:         time.Now().UTC(),
	}
	if err := s.store.PutHTTPTrigger(r.Context(), trigger); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, httpTriggerToResponse(*trigger, publicFunctionURL(r, token)))
}

func (s *Server) listHTTPTriggers(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	version, err := s.store.GetFunctionVersion(r.Context(), versionID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := s.authorizeProject(r.Context(), version.ProjectID); err != nil {
		writeServiceError(w, err)
		return
	}
	triggers, err := s.store.ListHTTPTriggersByFunctionVersion(r.Context(), versionID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	response := make([]httpTriggerResponse, 0, len(triggers))
	for _, trigger := range triggers {
		response = append(response, httpTriggerToResponse(trigger, publicFunctionURL(r, trigger.Token)))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) listWebhookTriggers(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	version, err := s.store.GetFunctionVersion(r.Context(), versionID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := s.authorizeProject(r.Context(), version.ProjectID); err != nil {
		writeServiceError(w, err)
		return
	}
	triggers, err := s.store.ListWebhookTriggersByFunctionVersion(r.Context(), versionID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, triggers)
}

func (s *Server) invokeHTTPTrigger(w http.ResponseWriter, r *http.Request) {
	requestStartedAt := time.Now().UTC()
	token := chi.URLParam(r, "token")
	var (
		triggerLookupMs int64
		versionLoadMs   int64
		buildPayloadMs  int64
		directInvokeMs  int64
		dispatchMs      int64
		waitMs          int64
		versionID       string
		jobID           string
		state           = "started"
		errMsg          string
	)
	defer func() {
		slog.Info("http trigger invoke timing",
			"token", token,
			"versionID", versionID,
			"jobID", jobID,
			"state", state,
			"error", errMsg,
			"triggerLookupMs", triggerLookupMs,
			"versionLoadMs", versionLoadMs,
			"buildPayloadMs", buildPayloadMs,
			"directInvokeMs", directInvokeMs,
			"dispatchMs", dispatchMs,
			"waitMs", waitMs,
			"totalMs", time.Since(requestStartedAt).Milliseconds(),
		)
	}()
	triggerLookupStarted := time.Now()
	trigger, err := s.store.GetHTTPTrigger(r.Context(), token)
	triggerLookupMs = time.Since(triggerLookupStarted).Milliseconds()
	if err != nil || !trigger.Enabled {
		state = "not_found"
		http.Error(w, "http trigger not found", http.StatusNotFound)
		return
	}
	requestCtx, err := s.authContextForHTTPTrigger(r.Context(), r, trigger)
	if err != nil {
		state = "unauthorized"
		errMsg = err.Error()
		switch {
		case errors.Is(err, errMissingAPIKey):
			http.Error(w, "missing X-API-Key", http.StatusUnauthorized)
		case errors.Is(err, errInvalidAPIKey):
			http.Error(w, "invalid X-API-Key", http.StatusUnauthorized)
		default:
			http.Error(w, err.Error(), http.StatusUnauthorized)
		}
		return
	}
	versionLoadStarted := time.Now()
	version, err := s.store.GetFunctionVersion(r.Context(), trigger.FunctionVersionID)
	versionLoadMs = time.Since(versionLoadStarted).Milliseconds()
	if err != nil {
		state = "version_lookup_failed"
		errMsg = err.Error()
		writeServiceError(w, err)
		return
	}
	versionID = version.ID
	if trigger.AuthMode == domain.HTTPTriggerAuthModeAPIKey {
		if err := s.authorizeProject(requestCtx, version.ProjectID); err != nil {
			state = "forbidden"
			errMsg = err.Error()
			writeServiceError(w, err)
			return
		}
	}
	if version.State != domain.FunctionStateReady {
		state = "not_ready"
		http.Error(w, "function url not ready", http.StatusServiceUnavailable)
		return
	}
	buildPayloadStarted := time.Now()
	payload, err := buildHTTPPayload(r, token)
	buildPayloadMs = time.Since(buildPayloadStarted).Milliseconds()
	if err != nil {
		state = "payload_failed"
		errMsg = err.Error()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	streamWriter := newHTTPDirectStreamWriter(w, r.Method, token, version.ID)
	directCtx, cancelDirect := context.WithTimeout(requestCtx, httpTriggerTimeout(version.TimeoutSec))
	directStarted := time.Now()
	directResult, directErr := s.scheduler.ExecuteDirectStream(
		directCtx,
		trigger.FunctionVersionID,
		payload,
		func(jobID, _ string, startMode domain.StartMode) {
			streamWriter.SetMeta(jobID, startMode)
		},
		streamWriter.HandleEvent,
	)
	directInvokeMs = time.Since(directStarted).Milliseconds()
	cancelDirect()
	if directErr == nil {
		jobID = directResult.JobID
		if directResult.State == domain.JobStateSucceeded {
			state = "succeeded_direct"
		} else {
			state = string(directResult.State)
			errMsg = directResult.Error
		}
		returnDirectHTTPResult(streamWriter, directResult)
		return
	}
	switch {
	case errors.Is(directErr, domain.ErrNoExecutionCapacity):
		latencyMs := computeLatencyMs(requestStartedAt, time.Now().UTC())
		state = "overloaded"
		errMsg = directErr.Error()
		w.Header().Set("Retry-After", "1")
		w.Header().Set("X-Lecrev-Latency-Ms", strconv.FormatInt(latencyMs, 10))
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"functionVersionId": version.ID,
			"state":             "overloaded",
			"message":           "function url execution capacity is unavailable; retry later",
			"latencyMs":         latencyMs,
		})
		return
	case errors.Is(directErr, domain.ErrDirectInvokeUnavailable):
		state = "direct_fallback"
	case errors.Is(directErr, context.DeadlineExceeded), errors.Is(directErr, context.Canceled):
		latencyMs := computeLatencyMs(requestStartedAt, time.Now().UTC())
		state = "timeout_direct"
		errMsg = directErr.Error()
		w.Header().Set("X-Lecrev-Latency-Ms", strconv.FormatInt(latencyMs, 10))
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{
			"functionVersionId": version.ID,
			"state":             "timeout",
			"message":           "function url execution did not complete before the response deadline",
			"latencyMs":         latencyMs,
		})
		return
	default:
		state = "direct_failed"
		errMsg = directErr.Error()
		writeServiceError(w, directErr)
		return
	}

	dispatchCtx, cancelDispatch := context.WithTimeout(requestCtx, 15*time.Second)
	defer cancelDispatch()

	dispatchStarted := time.Now()
	job, err := s.scheduler.DispatchExecutionIdempotent(dispatchCtx, trigger.FunctionVersionID, payload, idempotencyKey(r.Header.Get("Idempotency-Key"), ""))
	dispatchMs = time.Since(dispatchStarted).Milliseconds()
	if err != nil {
		state = "dispatch_failed"
		errMsg = err.Error()
		writeServiceError(w, err)
		return
	}
	jobID = job.ID

	waitCtx, cancelWait := context.WithTimeout(requestCtx, httpTriggerTimeout(version.TimeoutSec))
	defer cancelWait()

	waitStarted := time.Now()
	job, err = s.waitForExecutionJob(waitCtx, job.ID, httpTriggerJobPollInterval)
	waitMs = time.Since(waitStarted).Milliseconds()
	if err != nil {
		latencyMs := computeLatencyMs(requestStartedAt, time.Now().UTC())
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			state = "timeout"
			errMsg = err.Error()
			w.Header().Set("X-Lecrev-Latency-Ms", strconv.FormatInt(latencyMs, 10))
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{
				"jobId":             job.ID,
				"functionVersionId": version.ID,
				"state":             "timeout",
				"message":           "function url execution did not complete before the response deadline",
				"latencyMs":         latencyMs,
			})
			return
		}
		state = "wait_failed"
		errMsg = err.Error()
		writeServiceError(w, err)
		return
	}

	job = withJobLatency(job)
	latencyMs := jobLatencyMs(job)
	if latencyMs == 0 {
		latencyMs = computeLatencyMs(requestStartedAt, time.Now().UTC())
	}

	w.Header().Set("X-Lecrev-Job-Id", job.ID)
	w.Header().Set("X-Lecrev-Function-Version-Id", version.ID)
	w.Header().Set("X-Lecrev-Function-Url-Token", token)
	w.Header().Set("X-Lecrev-Latency-Ms", strconv.FormatInt(latencyMs, 10))

	if job.State != domain.JobStateSucceeded || job.Result == nil {
		state = string(job.State)
		errMsg = job.Error
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"jobId":             job.ID,
			"functionVersionId": version.ID,
			"state":             job.State,
			"error":             job.Error,
			"latencyMs":         latencyMs,
		})
		return
	}
	state = "succeeded"

	if err := writeHTTPFunctionResult(w, r.Method, job.Result.Output, latencyMs); err != nil {
		state = "response_write_failed"
		errMsg = err.Error()
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"jobId":             job.ID,
			"functionVersionId": version.ID,
			"state":             job.State,
			"error":             err.Error(),
			"latencyMs":         latencyMs,
		})
	}
}

func returnDirectHTTPResult(streamWriter *httpDirectStreamWriter, result *domain.DirectExecutionResult) {
	if result == nil {
		writeJSON(streamWriter.writer, http.StatusBadGateway, map[string]any{
			"functionVersionId": streamWriter.versionID,
			"state":             domain.JobStateFailed,
			"error":             "direct execution returned no result",
		})
		return
	}
	latencyMs := int64(0)
	if result.Result != nil {
		latencyMs = result.Result.LatencyMs
	}
	if latencyMs <= 0 && !result.FinishedAt.IsZero() {
		latencyMs = computeLatencyMs(result.StartedAt, result.FinishedAt)
	}
	if latencyMs <= 0 {
		latencyMs = 1
	}

	streamWriter.SetMeta(result.JobID, result.StartMode)
	streamWriter.SetLatency(latencyMs)

	if result.State != domain.JobStateSucceeded || result.Result == nil {
		if streamWriter.Started() {
			return
		}
		writeJSON(streamWriter.writer, http.StatusBadGateway, map[string]any{
			"jobId":             result.JobID,
			"functionVersionId": streamWriter.versionID,
			"state":             result.State,
			"error":             result.Error,
			"latencyMs":         latencyMs,
		})
		return
	}
	if streamWriter.Started() {
		streamWriter.Close()
		return
	}
	if err := writeHTTPFunctionResult(streamWriter.writer, streamWriter.method, result.Result.Output, latencyMs); err != nil {
		writeJSON(streamWriter.writer, http.StatusBadGateway, map[string]any{
			"jobId":             result.JobID,
			"functionVersionId": streamWriter.versionID,
			"state":             domain.JobStateFailed,
			"error":             err.Error(),
			"latencyMs":         latencyMs,
		})
	}
}

type httpDirectStreamWriter struct {
	writer    http.ResponseWriter
	method    string
	token     string
	versionID string
	flusher   http.Flusher

	mu        sync.Mutex
	started   bool
	closed    bool
	status    int
	jobID     string
	startMode domain.StartMode
}

const latencyTrailerHeader = "X-Lecrev-Latency-Ms"

func newHTTPDirectStreamWriter(w http.ResponseWriter, method, token, versionID string) *httpDirectStreamWriter {
	sw := &httpDirectStreamWriter{
		writer:    w,
		method:    method,
		token:     token,
		versionID: versionID,
		status:    http.StatusOK,
	}
	if flusher, ok := w.(http.Flusher); ok {
		sw.flusher = flusher
	}
	return sw
}

func (w *httpDirectStreamWriter) declareLatencyTrailer() {
	headers := w.writer.Header()
	const trailerKey = "Trailer"
	current := headers.Values(trailerKey)
	for _, value := range current {
		for _, name := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(name), latencyTrailerHeader) {
				return
			}
		}
	}
	headers.Add(trailerKey, latencyTrailerHeader)
}

func (w *httpDirectStreamWriter) SetMeta(jobID string, startMode domain.StartMode) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return
	}
	if strings.TrimSpace(jobID) != "" {
		w.jobID = jobID
		w.writer.Header().Set("X-Lecrev-Job-Id", jobID)
	}
	w.writer.Header().Set("X-Lecrev-Function-Version-Id", w.versionID)
	if strings.TrimSpace(w.token) != "" {
		w.writer.Header().Set("X-Lecrev-Function-Url-Token", w.token)
	}
	if startMode != "" {
		w.startMode = startMode
		w.writer.Header().Set("X-Lecrev-Start-Mode", string(startMode))
	}
}

func (w *httpDirectStreamWriter) SetLatency(latencyMs int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if latencyMs <= 0 {
		return
	}
	if w.started {
		w.writer.Header().Set(latencyTrailerHeader, strconv.FormatInt(latencyMs, 10))
		return
	}
	w.writer.Header().Set(latencyTrailerHeader, strconv.FormatInt(latencyMs, 10))
}

func (w *httpDirectStreamWriter) Started() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.started
}

func (w *httpDirectStreamWriter) HandleEvent(event firecracker.HTTPStreamEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	switch event.Type {
	case firecracker.HTTPStreamEventStart:
		if w.started {
			return nil
		}
		w.declareLatencyTrailer()
		if event.StatusCode > 0 {
			w.status = event.StatusCode
		}
		for key, value := range event.Headers {
			if key == "" {
				continue
			}
			w.writer.Header().Set(key, value)
		}
		w.writer.WriteHeader(w.status)
		w.started = true
		if w.flusher != nil {
			w.flusher.Flush()
		}
	case firecracker.HTTPStreamEventChunk:
		if !w.started {
			w.declareLatencyTrailer()
			w.writer.WriteHeader(w.status)
			w.started = true
		}
		if w.method != http.MethodHead && len(event.Chunk) > 0 {
			if _, err := w.writer.Write(event.Chunk); err != nil {
				return err
			}
		}
		if w.flusher != nil {
			w.flusher.Flush()
		}
	case firecracker.HTTPStreamEventEnd:
		if !w.started {
			w.declareLatencyTrailer()
			w.writer.WriteHeader(w.status)
			w.started = true
		}
		if w.flusher != nil {
			w.flusher.Flush()
		}
		w.closed = true
	}
	return nil
}

func (w *httpDirectStreamWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

func (s *Server) invokeWebhook(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	trigger, err := s.store.GetWebhookTrigger(r.Context(), token)
	if err != nil || !trigger.Enabled {
		http.Error(w, "webhook trigger not found", http.StatusNotFound)
		return
	}
	payload, err := buildWebhookPayload(r, token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	job, err := s.scheduler.DispatchExecutionIdempotent(ctx, trigger.FunctionVersionID, payload, idempotencyKey(r.Header.Get("Idempotency-Key"), ""))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, withJobLatency(job))
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeRaw(w http.ResponseWriter, status int, contentType string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeServiceError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.Is(err, artifact.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrAccessDenied):
		status = http.StatusForbidden
	case errors.Is(err, domain.ErrProjectBuildQuota), errors.Is(err, domain.ErrProjectExecutionQuota):
		status = http.StatusTooManyRequests
	case errors.Is(err, domain.ErrNoExecutionCapacity), errors.Is(err, domain.ErrDirectInvokeUnavailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, domain.ErrFunctionVersionNotReady), errors.Is(err, domain.ErrBuildLogsNotReady), errors.Is(err, domain.ErrExecutionResultNotReady):
		status = http.StatusConflict
	case errors.Is(err, domain.ErrIdempotencyConflict), errors.Is(err, domain.ErrIdempotencyInProgress), errors.Is(err, store.ErrAlreadyExists):
		status = http.StatusConflict
	default:
		status = http.StatusBadRequest
	}
	http.Error(w, err.Error(), status)
}

func idempotencyKey(headerValue, bodyValue string) string {
	if strings.TrimSpace(headerValue) != "" {
		return strings.TrimSpace(headerValue)
	}
	return strings.TrimSpace(bodyValue)
}

func decodeOptionalJSON(r *http.Request, target any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(target); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func generateTriggerToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", buf), nil
}

func generateWebhookToken() (string, error) {
	return generateTriggerToken()
}

func buildWebhookPayload(r *http.Request, token string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	payloadBody := json.RawMessage("null")
	trimmed := strings.TrimSpace(string(body))
	if trimmed != "" {
		if json.Valid(body) {
			payloadBody = append(json.RawMessage(nil), body...)
		} else {
			encoded, err := json.Marshal(trimmed)
			if err != nil {
				return nil, err
			}
			payloadBody = encoded
		}
	}

	headers := make(map[string][]string, len(r.Header))
	for key, values := range r.Header {
		headers[key] = append([]string(nil), values...)
	}
	query := make(map[string][]string, len(r.URL.Query()))
	for key, values := range r.URL.Query() {
		query[key] = append([]string(nil), values...)
	}

	return json.Marshal(map[string]any{
		"trigger": "webhook",
		"token":   token,
		"method":  r.Method,
		"path":    r.URL.Path,
		"query":   query,
		"headers": headers,
		"body":    payloadBody,
	})
}

func buildHTTPPayload(r *http.Request, token string) ([]byte, error) {
	return buildHTTPPayloadForPath(r, token, triggerRequestPath(r))
}

func buildHTTPPayloadForPath(r *http.Request, token, functionPath string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	payloadBody, rawBodyText, base64Body, isBase64, err := decodeHTTPPayloadBody(body)
	if err != nil {
		return nil, err
	}

	headers := make(map[string]string, len(r.Header))
	multiValueHeaders := make(map[string][]string, len(r.Header))
	for key, values := range r.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
		multiValueHeaders[key] = append([]string(nil), values...)
	}

	queryValues := r.URL.Query()
	query := make(map[string]string, len(queryValues))
	multiValueQuery := make(map[string][]string, len(queryValues))
	for key, values := range queryValues {
		if len(values) > 0 {
			query[key] = values[0]
		}
		multiValueQuery[key] = append([]string(nil), values...)
	}

	return json.Marshal(map[string]any{
		"trigger": "http",
		"token":   token,
		"request": map[string]any{
			"method":            r.Method,
			"scheme":            requestScheme(r),
			"host":              requestHost(r),
			"url":               fullRequestURL(r),
			"path":              functionPath,
			"rawPath":           r.URL.Path,
			"rawQuery":          r.URL.RawQuery,
			"headers":           headers,
			"multiValueHeaders": multiValueHeaders,
			"query":             query,
			"multiValueQuery":   multiValueQuery,
			"body":              payloadBody,
			"bodyText":          rawBodyText,
			"bodyBase64":        base64Body,
			"isBase64Encoded":   isBase64,
			"remoteAddr":        r.RemoteAddr,
		},
	})
}

func triggerRequestPath(r *http.Request) string {
	pathValue := strings.TrimSpace(chi.URLParam(r, "*"))
	if pathValue == "" {
		return "/"
	}
	return "/" + strings.TrimPrefix(pathValue, "/")
}

func websiteRequestPath(r *http.Request) string {
	pathValue := strings.TrimSpace(chi.URLParam(r, "*"))
	if pathValue == "" {
		return "/"
	}
	return "/" + strings.TrimPrefix(pathValue, "/")
}

func (s *Server) websiteManifest(ctx context.Context, version *domain.FunctionVersion) (*domain.WebsiteManifest, error) {
	if version == nil {
		return nil, fmt.Errorf("function version is required")
	}
	data, err := s.objects.Get(ctx, artifact.SiteManifestKey(version.ArtifactDigest))
	if err != nil {
		return nil, err
	}
	var manifest domain.WebsiteManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode site manifest: %w", err)
	}
	return &manifest, nil
}

func (s *Server) serveWebsiteStaticAsset(w http.ResponseWriter, r *http.Request, digest, requestPath string) bool {
	if strings.TrimSpace(digest) == "" || strings.TrimSpace(requestPath) == "" || requestPath == "/" {
		return false
	}
	key := artifact.SiteAssetObjectKey(digest, requestPath)
	data, err := s.objects.Get(r.Context(), key)
	if err != nil {
		return false
	}
	contentType := mime.TypeByExtension(path.Ext(requestPath))
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	if strings.HasPrefix(requestPath, "/_next/static/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
	return true
}

func decodeHTTPPayloadBody(body []byte) (any, string, string, bool, error) {
	if len(body) == 0 {
		return nil, "", "", false, nil
	}
	rawText := string(body)
	if json.Valid(body) {
		var payload any
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, "", "", false, err
		}
		return payload, rawText, base64.StdEncoding.EncodeToString(body), false, nil
	}
	return rawText, rawText, base64.StdEncoding.EncodeToString(body), false, nil
}

func normalizeHTTPTriggerAuthMode(raw string) (domain.HTTPTriggerAuthMode, error) {
	mode := domain.HTTPTriggerAuthMode(strings.ToLower(strings.TrimSpace(raw)))
	if mode == "" {
		return domain.HTTPTriggerAuthModeNone, nil
	}
	switch mode {
	case domain.HTTPTriggerAuthModeNone, domain.HTTPTriggerAuthModeAPIKey:
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported http trigger authMode %q", raw)
	}
}

func publicFunctionURL(r *http.Request, token string) string {
	if configured := strings.TrimSpace(os.Getenv("LECREV_PUBLIC_BASE_URL")); configured != "" {
		return strings.TrimRight(configured, "/") + "/f/" + token
	}
	return strings.TrimRight(publicBaseURL(r), "/") + "/f/" + token
}

func publicWebsiteURL(r *http.Request, versionID string) string {
	if configured := strings.TrimSpace(os.Getenv("LECREV_PUBLIC_WEBSITE_BASE_URL")); configured != "" {
		return strings.TrimRight(configured, "/") + "/w/" + versionID
	}
	return strings.TrimRight(publicBaseURL(r), "/") + "/w/" + versionID
}

func publicBaseURL(r *http.Request) string {
	return requestScheme(r) + "://" + requestHost(r)
}

func requestScheme(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		return forwarded
	}
	if r.URL != nil && strings.TrimSpace(r.URL.Scheme) != "" {
		return r.URL.Scheme
	}
	return "http"
}

func requestHost(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		return forwarded
	}
	if strings.TrimSpace(r.Host) != "" {
		return r.Host
	}
	if r.URL != nil && strings.TrimSpace(r.URL.Host) != "" {
		return r.URL.Host
	}
	return "localhost"
}

func fullRequestURL(r *http.Request) string {
	path := "/"
	if r.URL != nil {
		path = r.URL.RequestURI()
	}
	return publicBaseURL(r) + path
}

func httpTriggerTimeout(timeoutSec int) time.Duration {
	if timeoutSec <= 0 {
		return 30 * time.Second
	}
	timeout := time.Duration(timeoutSec+10) * time.Second
	if timeout < 10*time.Second {
		timeout = 10 * time.Second
	}
	if timeout > 180*time.Second {
		timeout = 180 * time.Second
	}
	return timeout
}

func (s *Server) waitForExecutionJob(ctx context.Context, jobID string, pollInterval time.Duration) (*domain.ExecutionJob, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		job, err := s.store.GetExecutionJob(ctx, jobID)
		if err != nil {
			return nil, err
		}
		if job.State == domain.JobStateSucceeded || job.State == domain.JobStateFailed {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return job, ctx.Err()
		case <-ticker.C:
		}
	}
}

func writeHTTPFunctionResult(w http.ResponseWriter, method string, output json.RawMessage, latencyMs int64) error {
	statusCode, headers, body, structured, err := decodeHTTPFunctionResult(output, latencyMs)
	if err != nil {
		return err
	}
	if !structured {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if method != http.MethodHead {
			_ = writeHTTPResponseBody(w, withLatencyJSON(output, latencyMs))
		}
		return nil
	}
	for key, values := range headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(statusCode)
	if method != http.MethodHead && len(body) > 0 {
		_ = writeHTTPResponseBody(w, body)
	}
	return nil
}

func writeHTTPResponseBody(w http.ResponseWriter, body []byte) error {
	if len(body) == 0 {
		return nil
	}

	const streamThreshold = 64 << 10
	const streamChunkSize = 256 << 10

	flusher, ok := w.(http.Flusher)
	if !ok || len(body) < streamThreshold {
		_, err := w.Write(body)
		return err
	}

	for offset := 0; offset < len(body); offset += streamChunkSize {
		end := offset + streamChunkSize
		if end > len(body) {
			end = len(body)
		}
		if _, err := w.Write(body[offset:end]); err != nil {
			return err
		}
		if offset == 0 {
			flusher.Flush()
		}
	}
	return nil
}

func decodeHTTPFunctionResult(output json.RawMessage, latencyMs int64) (int, http.Header, []byte, bool, error) {
	var payload map[string]any
	if err := json.Unmarshal(output, &payload); err != nil {
		return 0, nil, nil, false, nil
	}
	rawStatus, ok := payload["statusCode"]
	if !ok {
		return 0, nil, nil, false, nil
	}
	statusCode, err := toInt(rawStatus)
	if err != nil || statusCode < 100 || statusCode > 999 {
		return 0, nil, nil, true, fmt.Errorf("invalid statusCode in http function output")
	}

	headers := make(http.Header)
	if rawHeaders, ok := payload["headers"]; ok {
		headerMap, err := toHTTPHeader(rawHeaders)
		if err != nil {
			return 0, nil, nil, true, err
		}
		headers = headerMap
	}

	payload["body"] = withLatencyValue(payload["body"], latencyMs)
	bodyBytes, err := encodeHTTPResponseBody(payload["body"], headers, payload["isBase64Encoded"])
	if err != nil {
		return 0, nil, nil, true, err
	}
	return statusCode, headers, bodyBytes, true, nil
}

func toHTTPHeader(value any) (http.Header, error) {
	headers := make(http.Header)
	if value == nil {
		return headers, nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid headers in http function output")
	}
	for key, raw := range m {
		switch typed := raw.(type) {
		case string:
			headers.Add(key, typed)
		case []any:
			for _, item := range typed {
				headers.Add(key, fmt.Sprint(item))
			}
		default:
			headers.Add(key, fmt.Sprint(typed))
		}
	}
	return headers, nil
}

func encodeHTTPResponseBody(value any, headers http.Header, rawBase64Flag any) ([]byte, error) {
	isBase64Encoded, err := toBool(rawBase64Flag)
	if err != nil {
		return nil, err
	}
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case string:
		if isBase64Encoded {
			return base64.StdEncoding.DecodeString(typed)
		}
		if headers.Get("Content-Type") == "" {
			headers.Set("Content-Type", "text/plain; charset=utf-8")
		}
		return []byte(typed), nil
	default:
		if headers.Get("Content-Type") == "" {
			headers.Set("Content-Type", "application/json")
		}
		return json.Marshal(typed)
	}
}

func toInt(value any) (int, error) {
	switch typed := value.(type) {
	case float64:
		return int(typed), nil
	case float32:
		return int(typed), nil
	case int:
		return typed, nil
	case int64:
		return int(typed), nil
	case json.Number:
		v, err := typed.Int64()
		return int(v), err
	default:
		return 0, fmt.Errorf("invalid integer value %T", value)
	}
}

func withJobLatency(job *domain.ExecutionJob) *domain.ExecutionJob {
	if job == nil || job.Result == nil {
		return job
	}
	jobCopy := *job
	result := *job.Result
	result.LatencyMs = computeLatencyMs(result.StartedAt, result.FinishedAt)
	jobCopy.Result = &result
	return &jobCopy
}

func jobLatencyMs(job *domain.ExecutionJob) int64 {
	if job == nil || job.Result == nil {
		return 0
	}
	return computeLatencyMs(job.Result.StartedAt, job.Result.FinishedAt)
}

func withAttemptLatency(attempts []domain.Attempt) []domain.Attempt {
	decorated := make([]domain.Attempt, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.State.Terminal() {
			attempt.LatencyMs = computeLatencyMs(attempt.StartedAt, attempt.UpdatedAt)
		}
		decorated = append(decorated, attempt)
	}
	return decorated
}

func computeLatencyMs(startedAt, finishedAt time.Time) int64 {
	if startedAt.IsZero() || finishedAt.IsZero() || finishedAt.Before(startedAt) {
		return 0
	}
	latencyMs := finishedAt.Sub(startedAt).Milliseconds()
	if latencyMs == 0 && finishedAt.After(startedAt) {
		return 1
	}
	return latencyMs
}

func withLatencyJSON(output json.RawMessage, latencyMs int64) []byte {
	var value any
	if err := json.Unmarshal(output, &value); err != nil {
		return output
	}
	value = withLatencyValue(value, latencyMs)
	encoded, err := json.Marshal(value)
	if err != nil {
		return output
	}
	return encoded
}

func withLatencyValue(value any, latencyMs int64) any {
	bodyMap, ok := value.(map[string]any)
	if !ok {
		return value
	}
	bodyMap["latencyMs"] = latencyMs
	return bodyMap
}

func toBool(value any) (bool, error) {
	switch typed := value.(type) {
	case nil:
		return false, nil
	case bool:
		return typed, nil
	default:
		return false, fmt.Errorf("invalid boolean value %T", value)
	}
}

func httpTriggerToResponse(trigger domain.HTTPTrigger, url string) httpTriggerResponse {
	return httpTriggerResponse{
		Token:             trigger.Token,
		ProjectID:         trigger.ProjectID,
		FunctionVersionID: trigger.FunctionVersionID,
		Description:       trigger.Description,
		AuthMode:          trigger.AuthMode,
		Enabled:           trigger.Enabled,
		URL:               url,
		CreatedAt:         trigger.CreatedAt,
	}
}

func queryLimit(r *http.Request, fallback int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return fallback, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid limit %q", raw)
	}
	if limit < 1 || limit > maxListLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", maxListLimit)
	}
	return limit, nil
}

func limitFunctionVersions(items []domain.FunctionVersion, limit int) []domain.FunctionVersion {
	if len(items) <= limit {
		return items
	}
	return items[:limit]
}

func limitBuildJobs(items []domain.BuildJob, limit int) []domain.BuildJob {
	if len(items) <= limit {
		return items
	}
	return items[:limit]
}

func limitExecutionJobs(items []domain.ExecutionJob, limit int) []domain.ExecutionJob {
	if len(items) <= limit {
		return items
	}
	return items[:limit]
}

func summarizeFunctionVersions(items []domain.FunctionVersion) []functionVersionSummary {
	summaries := make([]functionVersionSummary, 0, len(items))
	for _, item := range items {
		summaries = append(summaries, functionVersionSummary{
			ID:             item.ID,
			Name:           item.Name,
			Runtime:        item.Runtime,
			State:          item.State,
			Entrypoint:     item.Entrypoint,
			MemoryMB:       item.MemoryMB,
			TimeoutSec:     item.TimeoutSec,
			NetworkPolicy:  item.NetworkPolicy,
			Regions:        append([]string(nil), item.Regions...),
			BuildJobID:     item.BuildJobID,
			ArtifactDigest: item.ArtifactDigest,
			CreatedAt:      item.CreatedAt,
		})
	}
	return summaries
}

func summarizeBuildJobs(items []domain.BuildJob) []buildJobSummary {
	summaries := make([]buildJobSummary, 0, len(items))
	for _, item := range items {
		summaries = append(summaries, buildJobSummary{
			ID:                item.ID,
			FunctionVersionID: item.FunctionVersionID,
			TargetRegion:      item.TargetRegion,
			State:             item.State,
			Error:             item.Error,
			LogsReady:         strings.TrimSpace(item.LogsKey) != "",
			CreatedAt:         item.CreatedAt,
			UpdatedAt:         item.UpdatedAt,
		})
	}
	return summaries
}

func summarizeExecutionJobs(items []domain.ExecutionJob) []executionJobSummary {
	summaries := make([]executionJobSummary, 0, len(items))
	for _, item := range items {
		summary := executionJobSummary{
			ID:                item.ID,
			FunctionVersionID: item.FunctionVersionID,
			TargetRegion:      item.TargetRegion,
			State:             item.State,
			MaxRetries:        item.MaxRetries,
			AttemptCount:      item.AttemptCount,
			LastAttemptID:     item.LastAttemptID,
			Error:             item.Error,
			CreatedAt:         item.CreatedAt,
			UpdatedAt:         item.UpdatedAt,
		}
		if item.Result != nil {
			summary.Result = &executionResultSummary{
				ExitCode:    item.Result.ExitCode,
				HostID:      item.Result.HostID,
				Region:      item.Result.Region,
				StartedAt:   item.Result.StartedAt,
				FinishedAt:  item.Result.FinishedAt,
				LatencyMs:   computeLatencyMs(item.Result.StartedAt, item.Result.FinishedAt),
				LogsReady:   strings.TrimSpace(item.Result.LogsKey) != "",
				OutputReady: strings.TrimSpace(item.Result.OutputKey) != "",
			}
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

func summarizeDeployments(project domain.Project, versions []domain.FunctionVersion, buildJobs []domain.BuildJob, jobs []domain.ExecutionJob) []deploymentSummary {
	buildByVersionID := make(map[string]domain.BuildJob, len(buildJobs))
	for _, job := range buildJobs {
		if _, exists := buildByVersionID[job.FunctionVersionID]; exists {
			continue
		}
		buildByVersionID[job.FunctionVersionID] = job
	}
	jobByVersionID := make(map[string]domain.ExecutionJob, len(jobs))
	for _, job := range jobs {
		if _, exists := jobByVersionID[job.FunctionVersionID]; exists {
			continue
		}
		jobByVersionID[job.FunctionVersionID] = job
	}

	summaries := make([]deploymentSummary, 0, len(versions))
	for _, version := range versions {
		var (
			buildSummary *buildJobSummary
			jobSummary   *executionJobSummary
			environment  string
			branch       string
			commitSHA    string
			gitURL       string
			updatedAt    = version.CreatedAt
		)

		if buildJob, ok := buildByVersionID[version.ID]; ok {
			summary := summarizeBuildJobs([]domain.BuildJob{buildJob})[0]
			buildSummary = &summary
			if buildJob.UpdatedAt.After(updatedAt) {
				updatedAt = buildJob.UpdatedAt
			}
			environment = strings.TrimSpace(buildJob.Metadata["environment"])
			branch = strings.TrimSpace(buildJob.Metadata["branch"])
			commitSHA = strings.TrimSpace(buildJob.Metadata["commitSha"])
			gitURL = strings.TrimSpace(buildJob.Metadata["gitUrl"])
			if request, ok := decodeDeploymentRequest(buildJob.Request); ok {
				if environment == "" {
					environment = deploymentEnvironment(request.Source)
				}
				if branch == "" {
					branch = strings.TrimSpace(request.Source.GitRef)
				}
				if gitURL == "" {
					gitURL = strings.TrimSpace(request.Source.GitURL)
				}
			}
		}

		if job, ok := jobByVersionID[version.ID]; ok {
			summary := summarizeExecutionJobs([]domain.ExecutionJob{job})[0]
			jobSummary = &summary
			if job.UpdatedAt.After(updatedAt) {
				updatedAt = job.UpdatedAt
			}
		}

		summaries = append(summaries, deploymentSummary{
			ID:                version.ID,
			ProjectID:         project.ID,
			ProjectName:       project.Name,
			FunctionVersionID: version.ID,
			Name:              version.Name,
			Runtime:           version.Runtime,
			SourceType:        version.SourceType,
			Environment:       environment,
			Branch:            branch,
			CommitSHA:         commitSHA,
			GitURL:            gitURL,
			Status:            deploymentStatus(version, buildSummary, jobSummary),
			FunctionState:     version.State,
			Regions:           append([]string(nil), version.Regions...),
			Build:             buildSummary,
			LastJob:           jobSummary,
			CreatedAt:         version.CreatedAt,
			UpdatedAt:         updatedAt,
		})
	}
	return summaries
}

func countBuildJobsByState(items []domain.BuildJob) map[string]int {
	counts := make(map[string]int)
	for _, item := range items {
		counts[item.State]++
	}
	return counts
}

func countExecutionJobsByState(items []domain.ExecutionJob) map[string]int {
	counts := make(map[string]int)
	for _, item := range items {
		counts[string(item.State)]++
	}
	return counts
}

func deploymentStatus(version domain.FunctionVersion, build *buildJobSummary, job *executionJobSummary) string {
	switch {
	case version.State == domain.FunctionStateFailed:
		return "failed"
	case build != nil && (build.State == "failed" || strings.TrimSpace(build.Error) != ""):
		return "failed"
	case job != nil && (job.State == domain.JobStateFailed || strings.TrimSpace(job.Error) != ""):
		return "failed"
	case job != nil && job.State == domain.JobStateSucceeded:
		return "active"
	case version.State == domain.FunctionStateReady:
		return "ready"
	default:
		return "building"
	}
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

func decodeDeploymentRequest(raw []byte) (domain.DeployRequest, bool) {
	if len(raw) == 0 {
		return domain.DeployRequest{}, false
	}
	var request domain.DeployRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return domain.DeployRequest{}, false
	}
	return request, true
}

func filterDeploymentSummaries(items []deploymentSummary, statusFilter, environmentFilter string) []deploymentSummary {
	if statusFilter == "" && environmentFilter == "" {
		return items
	}
	filtered := make([]deploymentSummary, 0, len(items))
	for _, item := range items {
		if statusFilter != "" && strings.ToLower(item.Status) != statusFilter {
			continue
		}
		if environmentFilter != "" && strings.ToLower(strings.TrimSpace(item.Environment)) != environmentFilter {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func sortDeployments(items []deploymentSummary) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID > items[j].ID
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setCORSHeaders(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		origin = "*"
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Idempotency-Key, Authorization")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Type, Trailer, X-Lecrev-Job-Id, X-Lecrev-Function-Version-Id, X-Lecrev-Function-Url-Token, X-Lecrev-Latency-Ms")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.Header().Add("Vary", "Origin")
}

type authContext struct {
	TenantID string
	IsAdmin  bool
}

type authContextKey struct{}

func withAuthContext(ctx context.Context, auth authContext) context.Context {
	return context.WithValue(ctx, authContextKey{}, auth)
}

func authFromContext(ctx context.Context) authContext {
	auth, _ := ctx.Value(authContextKey{}).(authContext)
	return auth
}

func tenantIDFromContext(ctx context.Context) string {
	return authFromContext(ctx).TenantID
}

func (s *Server) authorizedJob(ctx context.Context, jobID string) (*domain.ExecutionJob, error) {
	job, err := s.store.GetExecutionJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeProject(ctx, job.ProjectID); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Server) authorizedBuildJob(ctx context.Context, jobID string) (*domain.BuildJob, *domain.FunctionVersion, error) {
	job, err := s.store.GetBuildJob(ctx, jobID)
	if err != nil {
		return nil, nil, err
	}
	version, err := s.store.GetFunctionVersion(ctx, job.FunctionVersionID)
	if err != nil {
		return nil, nil, err
	}
	if err := s.authorizeProject(ctx, version.ProjectID); err != nil {
		return nil, nil, err
	}
	return job, version, nil
}

func (s *Server) loadAuthorizedDeployment(ctx context.Context, deploymentID string) (*deploymentSummary, *domain.BuildJob, *domain.ExecutionJob, error) {
	version, err := s.store.GetFunctionVersion(ctx, deploymentID)
	if err != nil {
		return nil, nil, nil, err
	}
	project, err := s.authorizedProject(ctx, version.ProjectID)
	if err != nil {
		return nil, nil, nil, err
	}

	var buildJob *domain.BuildJob
	if strings.TrimSpace(version.BuildJobID) != "" {
		job, err := s.store.GetBuildJob(ctx, version.BuildJobID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, nil, nil, err
		}
		if err == nil {
			buildJob = job
		}
	}

	jobs, err := s.store.ListExecutionJobsByProject(ctx, version.ProjectID)
	if err != nil {
		return nil, nil, nil, err
	}
	latestJob := latestExecutionJobForVersion(version.ID, jobs)

	buildJobs := make([]domain.BuildJob, 0, 1)
	if buildJob != nil {
		buildJobs = append(buildJobs, *buildJob)
	}
	deploymentJobs := make([]domain.ExecutionJob, 0, 1)
	if latestJob != nil {
		deploymentJobs = append(deploymentJobs, *latestJob)
	}

	summaries := summarizeDeployments(*project, []domain.FunctionVersion{*version}, buildJobs, deploymentJobs)
	if len(summaries) == 0 {
		return nil, nil, nil, store.ErrNotFound
	}
	return &summaries[0], buildJob, latestJob, nil
}

func (s *Server) authorizedProject(ctx context.Context, projectID string) (*domain.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	auth := authFromContext(ctx)
	if !auth.IsAdmin && project.TenantID != auth.TenantID {
		return nil, store.ErrAccessDenied
	}
	return project, nil
}

func latestExecutionJobForVersion(versionID string, jobs []domain.ExecutionJob) *domain.ExecutionJob {
	for _, job := range jobs {
		if job.FunctionVersionID != versionID {
			continue
		}
		jobCopy := job
		return &jobCopy
	}
	return nil
}

func jobResultKey(job *domain.ExecutionJob, kind string) (string, error) {
	if job.Result == nil {
		return "", domain.ErrExecutionResultNotReady
	}
	switch kind {
	case "logs":
		if strings.TrimSpace(job.Result.LogsKey) == "" {
			return "", domain.ErrExecutionResultNotReady
		}
		return job.Result.LogsKey, nil
	case "output":
		if strings.TrimSpace(job.Result.OutputKey) == "" {
			return "", domain.ErrExecutionResultNotReady
		}
		return job.Result.OutputKey, nil
	default:
		return "", fmt.Errorf("unsupported execution artifact %q", kind)
	}
}

func (s *Server) executionLogsData(ctx context.Context, job *domain.ExecutionJob) ([]byte, error) {
	if job == nil || job.Result == nil {
		return nil, domain.ErrExecutionResultNotReady
	}
	if key := strings.TrimSpace(job.Result.LogsKey); key != "" {
		data, err := s.objects.Get(ctx, key)
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, artifact.ErrNotFound) {
			return nil, err
		}
	}
	return []byte(job.Result.Logs), nil
}

func (s *Server) executionOutputData(ctx context.Context, job *domain.ExecutionJob) ([]byte, error) {
	if job == nil || job.Result == nil {
		return nil, domain.ErrExecutionResultNotReady
	}
	if key := strings.TrimSpace(job.Result.OutputKey); key != "" {
		data, err := s.objects.Get(ctx, key)
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, artifact.ErrNotFound) {
			return nil, err
		}
	}
	if len(job.Result.Output) == 0 {
		return []byte("null"), nil
	}
	return append([]byte(nil), job.Result.Output...), nil
}

func (s *Server) authorizeProject(ctx context.Context, projectID string) error {
	_, err := s.authorizedProject(ctx, projectID)
	return err
}

func requireAdmin(ctx context.Context) error {
	if !authFromContext(ctx).IsAdmin {
		return store.ErrAccessDenied
	}
	return nil
}
