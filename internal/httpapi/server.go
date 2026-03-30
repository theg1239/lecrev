package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/theg1239/lecrev/internal/apikey"
	"github.com/theg1239/lecrev/internal/artifact"
	"github.com/theg1239/lecrev/internal/build"
	"github.com/theg1239/lecrev/internal/domain"
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
	r.Post("/v1/triggers/webhook/{token}", srv.invokeWebhook)
	r.Route("/v1", func(r chi.Router) {
		r.Use(srv.authMiddleware)
		r.Options("/*", srv.handlePreflight)
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
	defaultProjectListLimit  = 50
	defaultOverviewItemLimit = 8
	maxListLimit             = 200
)

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawKey := strings.TrimSpace(r.Header.Get("X-API-Key"))
		if rawKey == "" {
			http.Error(w, "missing X-API-Key", http.StatusUnauthorized)
			return
		}
		keyHash := apikey.Hash(rawKey)
		record, err := s.store.GetAPIKeyByHash(r.Context(), keyHash)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "invalid X-API-Key", http.StatusUnauthorized)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if record.Disabled || subtle.ConstantTimeCompare([]byte(record.KeyHash), []byte(keyHash)) != 1 {
			http.Error(w, "invalid X-API-Key", http.StatusUnauthorized)
			return
		}
		_ = s.store.TouchAPIKeyLastUsed(r.Context(), keyHash, time.Now().UTC())
		next.ServeHTTP(w, r.WithContext(withAuthContext(r.Context(), authContext{
			TenantID: record.TenantID,
			IsAdmin:  record.IsAdmin,
		})))
	})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"status": "healthy",
	})
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
	if latestJob != nil && latestJob.Result != nil && strings.TrimSpace(latestJob.Result.LogsKey) != "" {
		data, err := s.objects.Get(r.Context(), latestJob.Result.LogsKey)
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
	key, err := jobResultKey(latestJob, "output")
	if err != nil {
		writeServiceError(w, err)
		return
	}
	data, err := s.objects.Get(r.Context(), key)
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
	writeJSON(w, http.StatusAccepted, job)
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
	writeJSON(w, http.StatusOK, job)
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
	writeJSON(w, http.StatusOK, attempts)
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
	key, err := jobResultKey(job, "logs")
	if err != nil {
		writeServiceError(w, err)
		return
	}
	data, err := s.objects.Get(r.Context(), key)
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
	key, err := jobResultKey(job, "output")
	if err != nil {
		writeServiceError(w, err)
		return
	}
	data, err := s.objects.Get(r.Context(), key)
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
	writeJSON(w, http.StatusAccepted, job)
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

func generateWebhookToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", buf), nil
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
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Idempotency-Key")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Type")
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
