package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/theg1239/lecrev/internal/apikey"
	"github.com/theg1239/lecrev/internal/artifact"
	"github.com/theg1239/lecrev/internal/build"
	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/scheduler"
	memstore "github.com/theg1239/lecrev/internal/store/memory"
)

type testDispatcher struct {
	region string
}

func (d testDispatcher) Region() string { return d.region }
func (d testDispatcher) Stats() domain.RegionStats {
	return domain.RegionStats{AvailableHosts: 1, BlankWarm: 1}
}
func (d testDispatcher) EnqueueExecution(context.Context, domain.Assignment) error { return nil }

type testAdmin struct {
	region string
	hostID string
	reason string
}

func (a *testAdmin) Region() string { return a.region }

func (a *testAdmin) DrainHost(_ context.Context, hostID, reason string) error {
	a.hostID = hostID
	a.reason = reason
	return nil
}

func TestWebhookTriggerLifecycleAndIdempotentInvoke(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	builder := build.New(meta, objects)
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "dev-root-key", "tenant-dev", false, false)
	if _, err := meta.EnsureProject(context.Background(), "demo", "tenant-dev", "demo"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	if err := meta.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-webhook",
		ProjectID:      "demo",
		Name:           "echo",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		TimeoutSec:     5,
		NetworkPolicy:  domain.NetworkPolicyFull,
		Regions:        []string{"ap-south-1"},
		ArtifactDigest: "digest",
		State:          domain.FunctionStateReady,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("put function version: %v", err)
	}

	handler := New(meta, objects, builder, sched)

	createReq := httptest.NewRequest(http.MethodPost, "/v1/functions/fn-webhook/triggers/webhook", bytes.NewBufferString(`{"description":"github push"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("X-API-Key", "dev-root-key")
	createResp := httptest.NewRecorder()
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected create trigger status %d, got %d: %s", http.StatusCreated, createResp.Code, createResp.Body.String())
	}
	var trigger domain.WebhookTrigger
	if err := json.Unmarshal(createResp.Body.Bytes(), &trigger); err != nil {
		t.Fatalf("decode trigger: %v", err)
	}
	if trigger.Token == "" {
		t.Fatal("expected webhook token to be generated")
	}

	invokeBody := []byte(`{"repository":"lecrev","event":"push"}`)
	invokeReq1 := httptest.NewRequest(http.MethodPost, "/v1/triggers/webhook/"+trigger.Token, bytes.NewReader(invokeBody))
	invokeReq1.Header.Set("Content-Type", "application/json")
	invokeReq1.Header.Set("Idempotency-Key", "webhook-delivery-1")
	invokeResp1 := httptest.NewRecorder()
	handler.ServeHTTP(invokeResp1, invokeReq1)
	if invokeResp1.Code != http.StatusAccepted {
		t.Fatalf("expected first webhook invoke status %d, got %d: %s", http.StatusAccepted, invokeResp1.Code, invokeResp1.Body.String())
	}
	var firstJob domain.ExecutionJob
	if err := json.Unmarshal(invokeResp1.Body.Bytes(), &firstJob); err != nil {
		t.Fatalf("decode first job: %v", err)
	}
	if firstJob.State != domain.JobStateQueued {
		t.Fatalf("expected queued webhook job, got %s", firstJob.State)
	}
	if firstJob.TargetRegion != "" {
		t.Fatalf("expected no target region before scheduling, got %s", firstJob.TargetRegion)
	}

	invokeReq2 := httptest.NewRequest(http.MethodPost, "/v1/triggers/webhook/"+trigger.Token, bytes.NewReader(invokeBody))
	invokeReq2.Header.Set("Content-Type", "application/json")
	invokeReq2.Header.Set("Idempotency-Key", "webhook-delivery-1")
	invokeResp2 := httptest.NewRecorder()
	handler.ServeHTTP(invokeResp2, invokeReq2)
	if invokeResp2.Code != http.StatusAccepted {
		t.Fatalf("expected second webhook invoke status %d, got %d: %s", http.StatusAccepted, invokeResp2.Code, invokeResp2.Body.String())
	}
	var secondJob domain.ExecutionJob
	if err := json.Unmarshal(invokeResp2.Body.Bytes(), &secondJob); err != nil {
		t.Fatalf("decode second job: %v", err)
	}
	if firstJob.ID != secondJob.ID {
		t.Fatalf("expected replayed job id %s, got %s", firstJob.ID, secondJob.ID)
	}
}

func TestJobInspectionAndDrainEndpoints(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	builder := build.New(meta, objects)
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "dev-root-key", "tenant-dev", false, true)
	if _, err := meta.EnsureProject(context.Background(), "demo", "tenant-dev", "demo"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	now := time.Now().UTC()
	if err := meta.PutExecutionJob(context.Background(), &domain.ExecutionJob{
		ID:                "job-observe",
		FunctionVersionID: "fn-observe",
		ProjectID:         "demo",
		State:             domain.JobStateSucceeded,
		Payload:           []byte(`{"hello":"world"}`),
		Result: &domain.JobResult{
			ExitCode:   0,
			Logs:       "hello from host\n",
			LogsKey:    "jobs/job-observe/logs.txt",
			Output:     json.RawMessage(`{"ok":true}`),
			OutputKey:  "jobs/job-observe/output.json",
			HostID:     "host-ap-south-1-a",
			Region:     "ap-south-1",
			StartedAt:  now,
			FinishedAt: now,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("put job: %v", err)
	}
	if err := objects.Put(context.Background(), "jobs/job-observe/logs.txt", []byte("hello from host\n")); err != nil {
		t.Fatalf("put archived logs: %v", err)
	}
	if err := objects.Put(context.Background(), "jobs/job-observe/output.json", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("put archived output: %v", err)
	}
	if err := meta.PutAttempt(context.Background(), &domain.Attempt{
		ID:                "attempt-observe",
		JobID:             "job-observe",
		FunctionVersionID: "fn-observe",
		HostID:            "host-ap-south-1-a",
		Region:            "ap-south-1",
		State:             domain.AttemptStateSucceeded,
		StartMode:         domain.StartModeFunctionWarm,
		StartedAt:         now,
		LeaseExpiresAt:    now,
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("put attempt: %v", err)
	}
	if err := meta.PutCostRecord(context.Background(), &domain.CostRecord{
		ID:              "attempt-observe",
		TenantID:        "tenant-dev",
		ProjectID:       "demo",
		JobID:           "job-observe",
		AttemptID:       "attempt-observe",
		HostID:          "host-ap-south-1-a",
		Region:          "ap-south-1",
		StartMode:       domain.StartModeFunctionWarm,
		CPUMs:           25,
		MemoryMBMs:      3200,
		WarmInstanceMs:  25,
		DataEgressBytes: 128,
		CreatedAt:       now,
	}); err != nil {
		t.Fatalf("put cost record: %v", err)
	}
	if err := meta.ReplaceWarmPoolsForHost(context.Background(), "ap-south-1", "host-ap-south-1-a", []domain.WarmPool{
		{
			Region:    "ap-south-1",
			HostID:    "host-ap-south-1-a",
			BlankWarm: 1,
			UpdatedAt: now,
		},
		{
			Region:            "ap-south-1",
			HostID:            "host-ap-south-1-a",
			FunctionVersionID: "fn-observe",
			FunctionWarm:      2,
			UpdatedAt:         now,
		},
	}); err != nil {
		t.Fatalf("replace warm pools: %v", err)
	}

	admin := &testAdmin{region: "ap-south-1"}
	handler := New(meta, objects, builder, sched, admin)

	attemptsReq := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-observe/attempts", nil)
	attemptsReq.Header.Set("X-API-Key", "dev-root-key")
	attemptsResp := httptest.NewRecorder()
	handler.ServeHTTP(attemptsResp, attemptsReq)
	if attemptsResp.Code != http.StatusOK {
		t.Fatalf("expected attempts status %d, got %d: %s", http.StatusOK, attemptsResp.Code, attemptsResp.Body.String())
	}
	var attempts []domain.Attempt
	if err := json.Unmarshal(attemptsResp.Body.Bytes(), &attempts); err != nil {
		t.Fatalf("decode attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].ID != "attempt-observe" {
		t.Fatalf("unexpected attempts payload: %+v", attempts)
	}

	logsReq := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-observe/logs", nil)
	logsReq.Header.Set("X-API-Key", "dev-root-key")
	logsResp := httptest.NewRecorder()
	handler.ServeHTTP(logsResp, logsReq)
	if logsResp.Code != http.StatusOK {
		t.Fatalf("expected logs status %d, got %d: %s", http.StatusOK, logsResp.Code, logsResp.Body.String())
	}
	if got := logsResp.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("expected logs content type text/plain; charset=utf-8, got %s", got)
	}
	if body := logsResp.Body.String(); body != "hello from host\n" {
		t.Fatalf("unexpected archived logs body %q", body)
	}

	outputReq := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-observe/output", nil)
	outputReq.Header.Set("X-API-Key", "dev-root-key")
	outputResp := httptest.NewRecorder()
	handler.ServeHTTP(outputResp, outputReq)
	if outputResp.Code != http.StatusOK {
		t.Fatalf("expected output status %d, got %d: %s", http.StatusOK, outputResp.Code, outputResp.Body.String())
	}
	if got := outputResp.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected output content type application/json, got %s", got)
	}
	if body := outputResp.Body.String(); body != `{"ok":true}` {
		t.Fatalf("unexpected archived output body %q", body)
	}

	costsReq := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-observe/costs", nil)
	costsReq.Header.Set("X-API-Key", "dev-root-key")
	costsResp := httptest.NewRecorder()
	handler.ServeHTTP(costsResp, costsReq)
	if costsResp.Code != http.StatusOK {
		t.Fatalf("expected costs status %d, got %d: %s", http.StatusOK, costsResp.Code, costsResp.Body.String())
	}
	var records []domain.CostRecord
	if err := json.Unmarshal(costsResp.Body.Bytes(), &records); err != nil {
		t.Fatalf("decode costs: %v", err)
	}
	if len(records) != 1 || records[0].AttemptID != "attempt-observe" {
		t.Fatalf("unexpected cost payload: %+v", records)
	}

	warmReq := httptest.NewRequest(http.MethodGet, "/v1/regions/ap-south-1/warm-pools", nil)
	warmReq.Header.Set("X-API-Key", "dev-root-key")
	warmResp := httptest.NewRecorder()
	handler.ServeHTTP(warmResp, warmReq)
	if warmResp.Code != http.StatusOK {
		t.Fatalf("expected warm pools status %d, got %d: %s", http.StatusOK, warmResp.Code, warmResp.Body.String())
	}
	var pools []domain.WarmPool
	if err := json.Unmarshal(warmResp.Body.Bytes(), &pools); err != nil {
		t.Fatalf("decode warm pools: %v", err)
	}
	if len(pools) != 2 {
		t.Fatalf("expected 2 warm pools, got %d", len(pools))
	}

	drainReq := httptest.NewRequest(http.MethodPost, "/v1/regions/ap-south-1/hosts/host-ap-south-1-a/drain", bytes.NewBufferString(`{"reason":"maintenance"}`))
	drainReq.Header.Set("Content-Type", "application/json")
	drainReq.Header.Set("X-API-Key", "dev-root-key")
	drainResp := httptest.NewRecorder()
	handler.ServeHTTP(drainResp, drainReq)
	if drainResp.Code != http.StatusAccepted {
		t.Fatalf("expected drain status %d, got %d: %s", http.StatusAccepted, drainResp.Code, drainResp.Body.String())
	}
	if admin.hostID != "host-ap-south-1-a" || admin.reason != "maintenance" {
		t.Fatalf("expected drain to be forwarded, got host=%s reason=%s", admin.hostID, admin.reason)
	}
}

func TestAuthRejectsCrossTenantProjectAccess(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	builder := build.New(meta, objects)
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "tenant-a-key", "tenant-a", false, false)
	if _, err := meta.EnsureProject(context.Background(), "project-b", "tenant-b", "project-b"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	handler := New(meta, objects, builder, sched)

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/project-b/functions", bytes.NewBufferString(`{
		"name":"echo",
		"runtime":"node22",
		"entrypoint":"index.mjs",
		"source":{"type":"bundle","inlineFiles":{"index.mjs":"export async function handler(){ return { ok: true }; }"}}
	}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "tenant-a-key")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("expected status %d, got %d: %s", http.StatusForbidden, resp.Code, resp.Body.String())
	}
}

func TestCreateFunctionRejectsInvalidAdmissionRequest(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	builder := build.New(meta, objects)
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "dev-root-key", "tenant-dev", false, false)
	if _, err := meta.EnsureProject(context.Background(), "demo", "tenant-dev", "demo"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	handler := New(meta, objects, builder, sched)

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/demo/functions", bytes.NewBufferString(`{
		"name":"echo",
		"runtime":"python3.12",
		"entrypoint":"index.py",
		"networkPolicy":"allowlist",
		"source":{"type":"bundle","inlineFiles":{"index.py":"def handler(event, context): return {'ok': True}"}}
	}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "dev-root-key")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, resp.Code, resp.Body.String())
	}
	if !bytes.Contains(resp.Body.Bytes(), []byte("unsupported runtime")) {
		t.Fatalf("expected runtime validation error, got %s", resp.Body.String())
	}
}

func TestBuildJobInspectionAndInvokeBlockedUntilReady(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	builder := build.New(meta, objects)
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "dev-root-key", "tenant-dev", false, false)
	if _, err := meta.EnsureProject(context.Background(), "demo", "tenant-dev", "demo"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	now := time.Now().UTC()
	if err := meta.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:            "fn-building",
		ProjectID:     "demo",
		Name:          "echo",
		Runtime:       "node22",
		Entrypoint:    "index.mjs",
		Regions:       []string{"ap-south-1"},
		BuildJobID:    "build-1",
		SourceType:    domain.SourceTypeBundle,
		State:         domain.FunctionStateBuilding,
		CreatedAt:     now,
		NetworkPolicy: domain.NetworkPolicyFull,
	}); err != nil {
		t.Fatalf("put function version: %v", err)
	}
	if err := meta.PutBuildJob(context.Background(), &domain.BuildJob{
		ID:                "build-1",
		FunctionVersionID: "fn-building",
		TargetRegion:      "ap-south-1",
		State:             "running",
		LogsKey:           "builds/build-1/logs.txt",
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("put build job: %v", err)
	}
	if err := objects.Put(context.Background(), "builds/build-1/logs.txt", []byte("build started\n")); err != nil {
		t.Fatalf("put build logs: %v", err)
	}

	handler := New(meta, objects, builder, sched)

	buildReq := httptest.NewRequest(http.MethodGet, "/v1/build-jobs/build-1", nil)
	buildReq.Header.Set("X-API-Key", "dev-root-key")
	buildResp := httptest.NewRecorder()
	handler.ServeHTTP(buildResp, buildReq)
	if buildResp.Code != http.StatusOK {
		t.Fatalf("expected build job status %d, got %d: %s", http.StatusOK, buildResp.Code, buildResp.Body.String())
	}
	var buildJob domain.BuildJob
	if err := json.Unmarshal(buildResp.Body.Bytes(), &buildJob); err != nil {
		t.Fatalf("decode build job: %v", err)
	}
	if buildJob.State != "running" {
		t.Fatalf("expected running build job state, got %s", buildJob.State)
	}
	if buildJob.LogsKey != "builds/build-1/logs.txt" {
		t.Fatalf("expected build logs key to round-trip, got %s", buildJob.LogsKey)
	}

	buildLogsReq := httptest.NewRequest(http.MethodGet, "/v1/build-jobs/build-1/logs", nil)
	buildLogsReq.Header.Set("X-API-Key", "dev-root-key")
	buildLogsResp := httptest.NewRecorder()
	handler.ServeHTTP(buildLogsResp, buildLogsReq)
	if buildLogsResp.Code != http.StatusOK {
		t.Fatalf("expected build logs status %d, got %d: %s", http.StatusOK, buildLogsResp.Code, buildLogsResp.Body.String())
	}
	if got := buildLogsResp.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("expected build logs content type text/plain; charset=utf-8, got %s", got)
	}
	if body := buildLogsResp.Body.String(); body != "build started\n" {
		t.Fatalf("unexpected build logs body %q", body)
	}

	invokeReq := httptest.NewRequest(http.MethodPost, "/v1/functions/fn-building/invoke", bytes.NewBufferString(`{"payload":{"hello":"world"}}`))
	invokeReq.Header.Set("Content-Type", "application/json")
	invokeReq.Header.Set("X-API-Key", "dev-root-key")
	invokeResp := httptest.NewRecorder()
	handler.ServeHTTP(invokeResp, invokeReq)
	if invokeResp.Code != http.StatusConflict {
		t.Fatalf("expected invoke conflict status %d, got %d: %s", http.StatusConflict, invokeResp.Code, invokeResp.Body.String())
	}
}

func TestAuthRejectsInvalidAndDisabledAPIKeys(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	builder := build.New(meta, objects)
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "disabled-key", "tenant-dev", true, false)

	handler := New(meta, objects, builder, sched)

	invalidReq := httptest.NewRequest(http.MethodGet, "/v1/regions", nil)
	invalidReq.Header.Set("X-API-Key", "missing-key")
	invalidResp := httptest.NewRecorder()
	handler.ServeHTTP(invalidResp, invalidReq)
	if invalidResp.Code != http.StatusUnauthorized {
		t.Fatalf("expected invalid key status %d, got %d: %s", http.StatusUnauthorized, invalidResp.Code, invalidResp.Body.String())
	}

	disabledReq := httptest.NewRequest(http.MethodGet, "/v1/regions", nil)
	disabledReq.Header.Set("X-API-Key", "disabled-key")
	disabledResp := httptest.NewRecorder()
	handler.ServeHTTP(disabledResp, disabledReq)
	if disabledResp.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled key status %d, got %d: %s", http.StatusUnauthorized, disabledResp.Code, disabledResp.Body.String())
	}
}

func TestRegionsEndpointSupportsTenantFrontendClients(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	builder := build.New(meta, objects)
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "tenant-key", "tenant-dev", false, false)
	now := time.Now().UTC()
	if err := meta.PutRegion(context.Background(), &domain.Region{
		Name:            "ap-south-1",
		State:           "healthy",
		AvailableHosts:  2,
		BlankWarm:       1,
		FunctionWarm:    3,
		LastHeartbeatAt: now,
	}); err != nil {
		t.Fatalf("put region: %v", err)
	}

	handler := New(meta, objects, builder, sched)

	req := httptest.NewRequest(http.MethodGet, "/v1/regions", nil)
	req.Header.Set("X-API-Key", "tenant-key")
	req.Header.Set("Origin", "http://localhost:3000")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("expected reflected CORS origin, got %q", got)
	}
	var regions []domain.Region
	if err := json.Unmarshal(resp.Body.Bytes(), &regions); err != nil {
		t.Fatalf("decode regions: %v", err)
	}
	if len(regions) != 1 || regions[0].Name != "ap-south-1" {
		t.Fatalf("unexpected regions payload: %+v", regions)
	}

	preflightReq := httptest.NewRequest(http.MethodOptions, "/v1/projects/demo/functions", nil)
	preflightReq.Header.Set("Origin", "http://localhost:3000")
	preflightReq.Header.Set("Access-Control-Request-Method", http.MethodPost)
	preflightReq.Header.Set("Access-Control-Request-Headers", "Content-Type, X-API-Key")
	preflightResp := httptest.NewRecorder()
	handler.ServeHTTP(preflightResp, preflightReq)
	if preflightResp.Code != http.StatusNoContent {
		t.Fatalf("expected preflight status %d, got %d: %s", http.StatusNoContent, preflightResp.Code, preflightResp.Body.String())
	}
	if got := preflightResp.Header().Get("Access-Control-Allow-Headers"); got != "Content-Type, X-API-Key, Idempotency-Key" {
		t.Fatalf("unexpected allow headers value %q", got)
	}
}

func TestInfraEndpointsRequireAdminAPIKey(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	builder := build.New(meta, objects)
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "tenant-key", "tenant-dev", false, false)

	handler := New(meta, objects, builder, sched)

	req := httptest.NewRequest(http.MethodGet, "/v1/regions/ap-south-1/hosts", nil)
	req.Header.Set("X-API-Key", "tenant-key")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("expected admin-only status %d, got %d: %s", http.StatusForbidden, resp.Code, resp.Body.String())
	}
}

func TestProjectOverviewEndpoints(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	builder := build.New(meta, objects)
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "tenant-key", "tenant-dev", false, false)
	project, err := meta.EnsureProject(context.Background(), "demo", "tenant-dev", "Demo Project")
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	now := time.Now().UTC()
	if err := meta.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-older",
		ProjectID:      project.ID,
		Name:           "echo",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		MemoryMB:       128,
		TimeoutSec:     30,
		NetworkPolicy:  domain.NetworkPolicyFull,
		Regions:        []string{"ap-south-1"},
		BuildJobID:     "build-older",
		ArtifactDigest: "digest-older",
		State:          domain.FunctionStateReady,
		CreatedAt:      now.Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("put older function version: %v", err)
	}
	if err := meta.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-newer",
		ProjectID:      project.ID,
		Name:           "worker",
		Runtime:        "node22",
		Entrypoint:     "handler.mjs",
		MemoryMB:       256,
		TimeoutSec:     45,
		NetworkPolicy:  domain.NetworkPolicyNone,
		Regions:        []string{"ap-south-2", "ap-south-1"},
		BuildJobID:     "build-newer",
		ArtifactDigest: "",
		State:          domain.FunctionStateBuilding,
		CreatedAt:      now.Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("put newer function version: %v", err)
	}
	if err := meta.PutBuildJob(context.Background(), &domain.BuildJob{
		ID:                "build-older",
		FunctionVersionID: "fn-older",
		TargetRegion:      "ap-south-1",
		State:             "succeeded",
		LogsKey:           "builds/build-older/logs.txt",
		CreatedAt:         now.Add(-2 * time.Minute),
		UpdatedAt:         now.Add(-90 * time.Second),
	}); err != nil {
		t.Fatalf("put older build job: %v", err)
	}
	if err := meta.PutBuildJob(context.Background(), &domain.BuildJob{
		ID:                "build-newer",
		FunctionVersionID: "fn-newer",
		TargetRegion:      "ap-south-2",
		State:             "running",
		CreatedAt:         now.Add(-1 * time.Minute),
		UpdatedAt:         now.Add(-30 * time.Second),
	}); err != nil {
		t.Fatalf("put newer build job: %v", err)
	}
	if err := meta.PutExecutionJob(context.Background(), &domain.ExecutionJob{
		ID:                "job-latest",
		FunctionVersionID: "fn-older",
		ProjectID:         project.ID,
		TargetRegion:      "ap-south-1",
		State:             domain.JobStateSucceeded,
		MaxRetries:        2,
		AttemptCount:      1,
		LastAttemptID:     "attempt-latest",
		Result: &domain.JobResult{
			ExitCode:   0,
			LogsKey:    "jobs/job-latest/logs.txt",
			OutputKey:  "jobs/job-latest/output.json",
			HostID:     "host-ap-south-1-a",
			Region:     "ap-south-1",
			StartedAt:  now.Add(-20 * time.Second),
			FinishedAt: now.Add(-10 * time.Second),
		},
		CreatedAt: now.Add(-20 * time.Second),
		UpdatedAt: now.Add(-10 * time.Second),
	}); err != nil {
		t.Fatalf("put execution job: %v", err)
	}

	handler := New(meta, objects, builder, sched)

	projectsReq := httptest.NewRequest(http.MethodGet, "/v1/projects?limit=10", nil)
	projectsReq.Header.Set("X-API-Key", "tenant-key")
	projectsResp := httptest.NewRecorder()
	handler.ServeHTTP(projectsResp, projectsReq)
	if projectsResp.Code != http.StatusOK {
		t.Fatalf("expected projects status %d, got %d: %s", http.StatusOK, projectsResp.Code, projectsResp.Body.String())
	}
	var projects []domain.Project
	if err := json.Unmarshal(projectsResp.Body.Bytes(), &projects); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != "demo" {
		t.Fatalf("unexpected projects payload: %+v", projects)
	}

	projectReq := httptest.NewRequest(http.MethodGet, "/v1/projects/demo", nil)
	projectReq.Header.Set("X-API-Key", "tenant-key")
	projectResp := httptest.NewRecorder()
	handler.ServeHTTP(projectResp, projectReq)
	if projectResp.Code != http.StatusOK {
		t.Fatalf("expected project status %d, got %d: %s", http.StatusOK, projectResp.Code, projectResp.Body.String())
	}

	functionsReq := httptest.NewRequest(http.MethodGet, "/v1/projects/demo/functions?limit=5", nil)
	functionsReq.Header.Set("X-API-Key", "tenant-key")
	functionsResp := httptest.NewRecorder()
	handler.ServeHTTP(functionsResp, functionsReq)
	if functionsResp.Code != http.StatusOK {
		t.Fatalf("expected functions status %d, got %d: %s", http.StatusOK, functionsResp.Code, functionsResp.Body.String())
	}
	var functions []functionVersionSummary
	if err := json.Unmarshal(functionsResp.Body.Bytes(), &functions); err != nil {
		t.Fatalf("decode function summaries: %v", err)
	}
	if len(functions) != 2 || functions[0].ID != "fn-newer" || functions[1].ID != "fn-older" {
		t.Fatalf("unexpected function summary ordering: %+v", functions)
	}

	buildsReq := httptest.NewRequest(http.MethodGet, "/v1/projects/demo/build-jobs?limit=5", nil)
	buildsReq.Header.Set("X-API-Key", "tenant-key")
	buildsResp := httptest.NewRecorder()
	handler.ServeHTTP(buildsResp, buildsReq)
	if buildsResp.Code != http.StatusOK {
		t.Fatalf("expected build jobs status %d, got %d: %s", http.StatusOK, buildsResp.Code, buildsResp.Body.String())
	}
	var builds []buildJobSummary
	if err := json.Unmarshal(buildsResp.Body.Bytes(), &builds); err != nil {
		t.Fatalf("decode build summaries: %v", err)
	}
	if len(builds) != 2 || builds[0].ID != "build-newer" || builds[1].ID != "build-older" {
		t.Fatalf("unexpected build summary ordering: %+v", builds)
	}

	jobsReq := httptest.NewRequest(http.MethodGet, "/v1/projects/demo/jobs?limit=5", nil)
	jobsReq.Header.Set("X-API-Key", "tenant-key")
	jobsResp := httptest.NewRecorder()
	handler.ServeHTTP(jobsResp, jobsReq)
	if jobsResp.Code != http.StatusOK {
		t.Fatalf("expected jobs status %d, got %d: %s", http.StatusOK, jobsResp.Code, jobsResp.Body.String())
	}
	var jobs []executionJobSummary
	if err := json.Unmarshal(jobsResp.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("decode job summaries: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "job-latest" || jobs[0].Result == nil || !jobs[0].Result.LogsReady || !jobs[0].Result.OutputReady {
		t.Fatalf("unexpected job summaries payload: %+v", jobs)
	}

	overviewReq := httptest.NewRequest(http.MethodGet, "/v1/projects/demo/overview?limit=1", nil)
	overviewReq.Header.Set("X-API-Key", "tenant-key")
	overviewResp := httptest.NewRecorder()
	handler.ServeHTTP(overviewResp, overviewReq)
	if overviewResp.Code != http.StatusOK {
		t.Fatalf("expected overview status %d, got %d: %s", http.StatusOK, overviewResp.Code, overviewResp.Body.String())
	}
	var overview projectOverviewResponse
	if err := json.Unmarshal(overviewResp.Body.Bytes(), &overview); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if overview.Project.ID != "demo" {
		t.Fatalf("unexpected overview project: %+v", overview.Project)
	}
	if overview.Totals.Functions != 2 {
		t.Fatalf("expected 2 functions in totals, got %d", overview.Totals.Functions)
	}
	if overview.Totals.BuildsByState["running"] != 1 || overview.Totals.BuildsByState["succeeded"] != 1 {
		t.Fatalf("unexpected build totals: %+v", overview.Totals.BuildsByState)
	}
	if overview.Totals.JobsByState[string(domain.JobStateSucceeded)] != 1 {
		t.Fatalf("unexpected job totals: %+v", overview.Totals.JobsByState)
	}
	if len(overview.RecentFunctions) != 1 || overview.RecentFunctions[0].ID != "fn-newer" {
		t.Fatalf("unexpected recent functions payload: %+v", overview.RecentFunctions)
	}
	if len(overview.RecentBuildJobs) != 1 || overview.RecentBuildJobs[0].ID != "build-newer" {
		t.Fatalf("unexpected recent build jobs payload: %+v", overview.RecentBuildJobs)
	}
	if len(overview.RecentJobs) != 1 || overview.RecentJobs[0].ID != "job-latest" {
		t.Fatalf("unexpected recent jobs payload: %+v", overview.RecentJobs)
	}
}

func TestWriteServiceErrorMapsQuotaErrorsToTooManyRequests(t *testing.T) {
	t.Parallel()

	for _, err := range []error{
		domain.ErrProjectBuildQuota,
		domain.ErrProjectExecutionQuota,
	} {
		resp := httptest.NewRecorder()
		writeServiceError(resp, err)
		if resp.Code != http.StatusTooManyRequests {
			t.Fatalf("expected quota error %v to map to %d, got %d", err, http.StatusTooManyRequests, resp.Code)
		}
	}
}

func mustSeedAPIKey(t *testing.T, meta *memstore.Store, rawKey, tenantID string, disabled, isAdmin bool) {
	t.Helper()
	if err := meta.PutAPIKey(context.Background(), &domain.APIKey{
		KeyHash:    apikey.Hash(rawKey),
		TenantID:   tenantID,
		IsAdmin:    isAdmin,
		Disabled:   disabled,
		CreatedAt:  time.Now().UTC(),
		LastUsedAt: time.Time{},
	}); err != nil {
		t.Fatalf("put api key: %v", err)
	}
}
