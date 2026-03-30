package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ishaan/eeeverc/internal/artifact"
	"github.com/ishaan/eeeverc/internal/build"
	"github.com/ishaan/eeeverc/internal/domain"
	"github.com/ishaan/eeeverc/internal/scheduler"
	memstore "github.com/ishaan/eeeverc/internal/store/memory"
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

	handler := New(meta, builder, sched, map[string]string{"dev-root-key": "demo"}, "tenant-dev", "demo")

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

	invokeBody := []byte(`{"repository":"eeeverc","event":"push"}`)
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
	handler := New(meta, builder, sched, map[string]string{"dev-root-key": "demo"}, "tenant-dev", "demo", admin)

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
