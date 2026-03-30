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
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/theg1239/lecrev/internal/apikey"
	"github.com/theg1239/lecrev/internal/build"
	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/scheduler"
	"github.com/theg1239/lecrev/internal/store"
)

type Server struct {
	store     store.Store
	builder   *build.Service
	scheduler *scheduler.Service
	admins    map[string]RegionAdmin
}

type RegionAdmin interface {
	Region() string
	DrainHost(ctx context.Context, hostID, reason string) error
}

func New(store store.Store, builder *build.Service, scheduler *scheduler.Service, admins ...RegionAdmin) http.Handler {
	adminIndex := make(map[string]RegionAdmin, len(admins))
	for _, admin := range admins {
		adminIndex[admin.Region()] = admin
	}
	srv := &Server{
		store:     store,
		builder:   builder,
		scheduler: scheduler,
		admins:    adminIndex,
	}
	r := chi.NewRouter()
	r.Post("/v1/triggers/webhook/{token}", srv.invokeWebhook)
	r.Route("/v1", func(r chi.Router) {
		r.Use(srv.authMiddleware)
		r.Post("/projects/{projectID}/functions", srv.createFunction)
		r.Get("/build-jobs/{jobID}", srv.getBuildJob)
		r.Post("/functions/{versionID}/invoke", srv.invokeFunction)
		r.Get("/jobs/{jobID}", srv.getJob)
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
	jobID := chi.URLParam(r, "jobID")
	job, err := s.store.GetBuildJob(r.Context(), jobID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	version, err := s.store.GetFunctionVersion(r.Context(), job.FunctionVersionID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := s.authorizeProject(r.Context(), version.ProjectID); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobID")
	job, err := s.store.GetExecutionJob(r.Context(), jobID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := s.authorizeProject(r.Context(), job.ProjectID); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) listJobAttempts(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobID")
	job, err := s.store.GetExecutionJob(r.Context(), jobID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := s.authorizeProject(r.Context(), job.ProjectID); err != nil {
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
	job, err := s.store.GetExecutionJob(r.Context(), jobID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := s.authorizeProject(r.Context(), job.ProjectID); err != nil {
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
	if err := requireAdmin(r.Context()); err != nil {
		writeServiceError(w, err)
		return
	}
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

func writeServiceError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.Is(err, store.ErrAccessDenied):
		status = http.StatusForbidden
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, domain.ErrFunctionVersionNotReady):
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

func (s *Server) authorizeProject(ctx context.Context, projectID string) error {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if project.TenantID != tenantIDFromContext(ctx) {
		return store.ErrAccessDenied
	}
	return nil
}

func requireAdmin(ctx context.Context) error {
	if !authFromContext(ctx).IsAdmin {
		return store.ErrAccessDenied
	}
	return nil
}
