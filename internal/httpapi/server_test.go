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
	builder := build.New(meta, artifact.NewMemoryStore())
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

	handler := New(meta, builder, sched)

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
	builder := build.New(meta, artifact.NewMemoryStore())
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
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("put job: %v", err)
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
	handler := New(meta, builder, sched, admin)

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
	builder := build.New(meta, artifact.NewMemoryStore())
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "tenant-a-key", "tenant-a", false, false)
	if _, err := meta.EnsureProject(context.Background(), "project-b", "tenant-b", "project-b"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	handler := New(meta, builder, sched)

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

func TestBuildJobInspectionAndInvokeBlockedUntilReady(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	builder := build.New(meta, artifact.NewMemoryStore())
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
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("put build job: %v", err)
	}

	handler := New(meta, builder, sched)

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
	builder := build.New(meta, artifact.NewMemoryStore())
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "disabled-key", "tenant-dev", true, false)

	handler := New(meta, builder, sched)

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

func TestInfraEndpointsRequireAdminAPIKey(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	builder := build.New(meta, artifact.NewMemoryStore())
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "tenant-key", "tenant-dev", false, false)

	handler := New(meta, builder, sched)

	req := httptest.NewRequest(http.MethodGet, "/v1/regions", nil)
	req.Header.Set("X-API-Key", "tenant-key")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("expected admin-only status %d, got %d: %s", http.StatusForbidden, resp.Code, resp.Body.String())
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
