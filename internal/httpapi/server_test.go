package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestHealthz(t *testing.T) {
	t.Parallel()

	handler := New(memstore.New(), artifact.NewMemoryStore(), build.New(memstore.New(), artifact.NewMemoryStore()), scheduler.New(memstore.New(), nil))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected healthz status %d, got %d", http.StatusOK, resp.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode healthz payload: %v", err)
	}
	if payload["status"] != "healthy" || payload["ok"] != true {
		t.Fatalf("unexpected healthz payload: %+v", payload)
	}
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

func TestHTTPTriggerLifecycle(t *testing.T) {
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
		ID:             "fn-http-trigger",
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

	createReq := httptest.NewRequest(http.MethodPost, "/v1/functions/fn-http-trigger/triggers/http", bytes.NewBufferString(`{"description":"public echo","authMode":"none"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("X-API-Key", "dev-root-key")
	createResp := httptest.NewRecorder()
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected create trigger status %d, got %d: %s", http.StatusCreated, createResp.Code, createResp.Body.String())
	}
	var trigger httpTriggerResponse
	if err := json.Unmarshal(createResp.Body.Bytes(), &trigger); err != nil {
		t.Fatalf("decode trigger: %v", err)
	}
	if trigger.Token == "" {
		t.Fatal("expected http trigger token to be generated")
	}
	if trigger.AuthMode != domain.HTTPTriggerAuthModeNone {
		t.Fatalf("expected auth mode none, got %s", trigger.AuthMode)
	}
	if !strings.HasSuffix(trigger.URL, "/f/"+trigger.Token) {
		t.Fatalf("expected function url to end with token, got %s", trigger.URL)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v1/functions/fn-http-trigger/triggers/http", nil)
	listReq.Header.Set("X-API-Key", "dev-root-key")
	listResp := httptest.NewRecorder()
	handler.ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("expected list trigger status %d, got %d: %s", http.StatusOK, listResp.Code, listResp.Body.String())
	}
	var triggers []httpTriggerResponse
	if err := json.Unmarshal(listResp.Body.Bytes(), &triggers); err != nil {
		t.Fatalf("decode trigger list: %v", err)
	}
	if len(triggers) != 1 || triggers[0].Token != trigger.Token {
		t.Fatalf("unexpected http trigger list payload: %+v", triggers)
	}
}

func TestHTTPTriggerLifecycleUsesConfiguredPublicBaseURL(t *testing.T) {
	t.Setenv("LECREV_PUBLIC_BASE_URL", "https://functions.demo.lecrev.app")

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
		ID:             "fn-http-public-base",
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

	createReq := httptest.NewRequest(http.MethodPost, "/v1/functions/fn-http-public-base/triggers/http", bytes.NewBufferString(`{"description":"public echo","authMode":"none"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("X-API-Key", "dev-root-key")
	createResp := httptest.NewRecorder()
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected create trigger status %d, got %d: %s", http.StatusCreated, createResp.Code, createResp.Body.String())
	}
	var trigger httpTriggerResponse
	if err := json.Unmarshal(createResp.Body.Bytes(), &trigger); err != nil {
		t.Fatalf("decode trigger: %v", err)
	}
	if want := "https://functions.demo.lecrev.app/f/" + trigger.Token; trigger.URL != want {
		t.Fatalf("expected configured public base URL %s, got %s", want, trigger.URL)
	}
}

func TestHTTPTriggerCreationRejectsNonReadyVersion(t *testing.T) {
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
		ID:            "fn-http-building",
		ProjectID:     "demo",
		Name:          "echo",
		Runtime:       "node22",
		Entrypoint:    "index.mjs",
		TimeoutSec:    5,
		NetworkPolicy: domain.NetworkPolicyFull,
		Regions:       []string{"ap-south-1"},
		State:         domain.FunctionStateBuilding,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("put function version: %v", err)
	}

	handler := New(meta, objects, builder, sched)

	req := httptest.NewRequest(http.MethodPost, "/v1/functions/fn-http-building/triggers/http", bytes.NewBufferString(`{"description":"public echo","authMode":"none"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "dev-root-key")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, resp.Code, resp.Body.String())
	}
}

func TestHTTPTriggerInvokeReturnsStructuredHTTPResponse(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	builder := build.New(meta, objects)
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	if _, err := meta.EnsureProject(context.Background(), "demo", "tenant-dev", "demo"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	if err := meta.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-http-invoke",
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
	if err := meta.PutHTTPTrigger(context.Background(), &domain.HTTPTrigger{
		Token:             "public-http-trigger",
		ProjectID:         "demo",
		FunctionVersionID: "fn-http-invoke",
		AuthMode:          domain.HTTPTriggerAuthModeNone,
		Enabled:           true,
		CreatedAt:         time.Now().UTC(),
	}); err != nil {
		t.Fatalf("put http trigger: %v", err)
	}

	handler := New(meta, objects, builder, sched)

	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			jobs, err := meta.ListExecutionJobsByProject(context.Background(), "demo")
			if err == nil && len(jobs) > 0 {
				job := jobs[0]
				now := time.Now().UTC()
				finishedAt := now.Add(25 * time.Millisecond)
				job.State = domain.JobStateSucceeded
				job.TargetRegion = "ap-south-1"
				job.AttemptCount = 1
				job.Result = &domain.JobResult{
					ExitCode:   0,
					Output:     json.RawMessage(`{"statusCode":201,"headers":{"Content-Type":"application/json","X-Demo":"ok"},"body":{"ok":true,"hello":"world"}}`),
					HostID:     "host-ap-south-1-a",
					Region:     "ap-south-1",
					StartedAt:  now,
					FinishedAt: finishedAt,
				}
				_ = meta.UpdateExecutionJob(context.Background(), &job)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	req := httptest.NewRequest(http.MethodPost, "/f/public-http-trigger/echo?name=ishaan", bytes.NewBufferString(`{"hello":"world"}`))
	req.Host = "lecrev.test"
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	<-done

	if resp.Code != http.StatusCreated {
		t.Fatalf("expected function url status %d, got %d: %s", http.StatusCreated, resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected content type application/json, got %s", got)
	}
	if got := resp.Header().Get("X-Demo"); got != "ok" {
		t.Fatalf("expected X-Demo header to round-trip, got %s", got)
	}
	if resp.Header().Get("X-Lecrev-Job-Id") == "" {
		t.Fatal("expected X-Lecrev-Job-Id header to be set")
	}
	if got := resp.Header().Get("X-Lecrev-Latency-Ms"); got != "25" {
		t.Fatalf("expected X-Lecrev-Latency-Ms header 25, got %s", got)
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if payload["ok"] != true || payload["hello"] != "world" {
		t.Fatalf("unexpected function url payload: %+v", payload)
	}
	if payload["latencyMs"] != float64(25) {
		t.Fatalf("expected latencyMs in response body, got %+v", payload)
	}

	jobs, err := meta.ListExecutionJobsByProject(context.Background(), "demo")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("expected one execution job, got jobs=%d err=%v", len(jobs), err)
	}
	var event map[string]any
	if err := json.Unmarshal(jobs[0].Payload, &event); err != nil {
		t.Fatalf("decode queued payload: %v", err)
	}
	if event["trigger"] != "http" {
		t.Fatalf("expected http trigger payload, got %+v", event)
	}
	request, ok := event["request"].(map[string]any)
	if !ok {
		t.Fatalf("expected request envelope in payload, got %+v", event["request"])
	}
	if request["method"] != http.MethodPost {
		t.Fatalf("expected POST method in payload, got %+v", request["method"])
	}
	if request["path"] != "/echo" {
		t.Fatalf("expected function path /echo, got %+v", request["path"])
	}
}

func TestHTTPTriggerInvokeAPIKeyModeRequiresAuthorizedKey(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	builder := build.New(meta, objects)
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "tenant-key", "tenant-dev", false, false)
	mustSeedAPIKey(t, meta, "other-tenant-key", "tenant-other", false, false)
	if _, err := meta.EnsureProject(context.Background(), "demo", "tenant-dev", "demo"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	if err := meta.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-http-auth",
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
	if err := meta.PutHTTPTrigger(context.Background(), &domain.HTTPTrigger{
		Token:             "private-http-trigger",
		ProjectID:         "demo",
		FunctionVersionID: "fn-http-auth",
		AuthMode:          domain.HTTPTriggerAuthModeAPIKey,
		Enabled:           true,
		CreatedAt:         time.Now().UTC(),
	}); err != nil {
		t.Fatalf("put http trigger: %v", err)
	}

	handler := New(meta, objects, builder, sched)

	unauthReq := httptest.NewRequest(http.MethodGet, "/f/private-http-trigger", nil)
	unauthResp := httptest.NewRecorder()
	handler.ServeHTTP(unauthResp, unauthReq)
	if unauthResp.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized status %d, got %d", http.StatusUnauthorized, unauthResp.Code)
	}

	wrongTenantReq := httptest.NewRequest(http.MethodGet, "/f/private-http-trigger", nil)
	wrongTenantReq.Header.Set("Authorization", "Bearer other-tenant-key")
	wrongTenantResp := httptest.NewRecorder()
	handler.ServeHTTP(wrongTenantResp, wrongTenantReq)
	if wrongTenantResp.Code != http.StatusForbidden {
		t.Fatalf("expected forbidden status %d, got %d: %s", http.StatusForbidden, wrongTenantResp.Code, wrongTenantResp.Body.String())
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			jobs, err := meta.ListExecutionJobsByProject(context.Background(), "demo")
			if err == nil && len(jobs) > 0 {
				job := jobs[0]
				now := time.Now().UTC()
				job.State = domain.JobStateSucceeded
				job.TargetRegion = "ap-south-1"
				job.AttemptCount = 1
				job.Result = &domain.JobResult{
					ExitCode:   0,
					Output:     json.RawMessage(`{"statusCode":200,"headers":{"Content-Type":"application/json"},"body":{"ok":true}}`),
					HostID:     "host-ap-south-1-a",
					Region:     "ap-south-1",
					StartedAt:  now,
					FinishedAt: now,
				}
				_ = meta.UpdateExecutionJob(context.Background(), &job)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	authReq := httptest.NewRequest(http.MethodHead, "/f/private-http-trigger/secure", bytes.NewBufferString(`{"hello":"world"}`))
	authReq.Header.Set("X-API-Key", "tenant-key")
	authReq.Header.Set("Content-Type", "application/json")
	authResp := httptest.NewRecorder()
	handler.ServeHTTP(authResp, authReq)
	<-done

	if authResp.Code != http.StatusOK {
		t.Fatalf("expected authenticated function url status %d, got %d: %s", http.StatusOK, authResp.Code, authResp.Body.String())
	}
	if authResp.Body.Len() != 0 {
		t.Fatalf("expected HEAD response body to be empty, got %q", authResp.Body.String())
	}

	jobs, err := meta.ListExecutionJobsByProject(context.Background(), "demo")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("expected one execution job, got jobs=%d err=%v", len(jobs), err)
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
	finishedAt := now.Add(25 * time.Millisecond)
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
			FinishedAt: finishedAt,
		},
		CreatedAt: now,
		UpdatedAt: finishedAt,
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
		UpdatedAt:         finishedAt,
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

	jobReq := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-observe", nil)
	jobReq.Header.Set("X-API-Key", "dev-root-key")
	jobResp := httptest.NewRecorder()
	handler.ServeHTTP(jobResp, jobReq)
	if jobResp.Code != http.StatusOK {
		t.Fatalf("expected job status %d, got %d: %s", http.StatusOK, jobResp.Code, jobResp.Body.String())
	}
	var job domain.ExecutionJob
	if err := json.Unmarshal(jobResp.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	if job.Result == nil || job.Result.LatencyMs != 25 {
		t.Fatalf("expected job result latencyMs 25, got %+v", job.Result)
	}

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
	if attempts[0].LatencyMs != 25 {
		t.Fatalf("expected attempt latencyMs 25, got %+v", attempts[0])
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

func TestJobArtifactEndpointsFallbackToInlineResultWhenArchivePending(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	builder := build.New(meta, objects)
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "tenant-key", "tenant-dev", false, false)
	if _, err := meta.EnsureProject(context.Background(), "demo", "tenant-dev", "demo"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	now := time.Now().UTC()
	if err := meta.PutExecutionJob(context.Background(), &domain.ExecutionJob{
		ID:                "job-inline",
		FunctionVersionID: "fn-inline",
		ProjectID:         "demo",
		State:             domain.JobStateSucceeded,
		Result: &domain.JobResult{
			ExitCode:   0,
			Logs:       "inline logs\n",
			LogsKey:    "jobs/job-inline/logs.txt",
			Output:     json.RawMessage(`{"inline":true}`),
			OutputKey:  "jobs/job-inline/output.json",
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

	handler := New(meta, objects, builder, sched)

	logsReq := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-inline/logs", nil)
	logsReq.Header.Set("X-API-Key", "tenant-key")
	logsResp := httptest.NewRecorder()
	handler.ServeHTTP(logsResp, logsReq)
	if logsResp.Code != http.StatusOK {
		t.Fatalf("expected logs status %d, got %d: %s", http.StatusOK, logsResp.Code, logsResp.Body.String())
	}
	if body := logsResp.Body.String(); body != "inline logs\n" {
		t.Fatalf("unexpected inline logs body %q", body)
	}

	outputReq := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-inline/output", nil)
	outputReq.Header.Set("X-API-Key", "tenant-key")
	outputResp := httptest.NewRecorder()
	handler.ServeHTTP(outputResp, outputReq)
	if outputResp.Code != http.StatusOK {
		t.Fatalf("expected output status %d, got %d: %s", http.StatusOK, outputResp.Code, outputResp.Body.String())
	}
	if body := strings.TrimSpace(outputResp.Body.String()); body != `{"inline":true}` {
		t.Fatalf("unexpected inline output body %q", body)
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
	if got := preflightResp.Header().Get("Access-Control-Allow-Headers"); got != "Content-Type, X-API-Key, Idempotency-Key, Authorization" {
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

func TestDeploymentSummaryEndpoints(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	builder := build.New(meta, objects)
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "tenant-key", "tenant-dev", false, false)

	handler := New(meta, objects, builder, sched)

	createReq := func(projectID string, body string) domain.FunctionVersion {
		req := httptest.NewRequest(http.MethodPost, "/v1/projects/"+projectID+"/functions", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", "tenant-key")
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusCreated {
			t.Fatalf("expected create function status %d, got %d: %s", http.StatusCreated, resp.Code, resp.Body.String())
		}
		var version domain.FunctionVersion
		if err := json.Unmarshal(resp.Body.Bytes(), &version); err != nil {
			t.Fatalf("decode function version: %v", err)
		}
		return version
	}

	demoVersion := createReq("demo", `{
		"name":"demo-echo",
		"environment":"staging",
		"runtime":"node22",
		"entrypoint":"index.mjs",
		"regions":["ap-south-1"],
		"source":{"type":"bundle","inlineFiles":{"index.mjs":"export async function handler(){ return { ok: true }; }"}}
	}`)
	opsVersion := createReq("ops", `{
		"name":"ops-echo",
		"environment":"production",
		"runtime":"node22",
		"entrypoint":"index.mjs",
		"regions":["ap-south-2"],
		"source":{"type":"bundle","inlineFiles":{"index.mjs":"export async function handler(){ return { ok: true }; }"}}
	}`)

	now := time.Now().UTC()
	if err := meta.PutExecutionJob(context.Background(), &domain.ExecutionJob{
		ID:                "job-demo-active",
		FunctionVersionID: demoVersion.ID,
		ProjectID:         "demo",
		TargetRegion:      "ap-south-1",
		State:             domain.JobStateSucceeded,
		MaxRetries:        1,
		AttemptCount:      1,
		LastAttemptID:     "attempt-demo-active",
		Result: &domain.JobResult{
			ExitCode:   0,
			LogsKey:    "jobs/job-demo-active/logs.txt",
			OutputKey:  "jobs/job-demo-active/output.json",
			HostID:     "host-ap-south-1-a",
			Region:     "ap-south-1",
			StartedAt:  now.Add(-5 * time.Second),
			FinishedAt: now,
		},
		CreatedAt: now.Add(-5 * time.Second),
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("put demo execution job: %v", err)
	}

	projectReq := httptest.NewRequest(http.MethodGet, "/v1/projects/demo/deployments?status=active&environment=staging", nil)
	projectReq.Header.Set("X-API-Key", "tenant-key")
	projectResp := httptest.NewRecorder()
	handler.ServeHTTP(projectResp, projectReq)
	if projectResp.Code != http.StatusOK {
		t.Fatalf("expected project deployments status %d, got %d: %s", http.StatusOK, projectResp.Code, projectResp.Body.String())
	}
	var projectDeployments []deploymentSummary
	if err := json.Unmarshal(projectResp.Body.Bytes(), &projectDeployments); err != nil {
		t.Fatalf("decode project deployments: %v", err)
	}
	if len(projectDeployments) != 1 {
		t.Fatalf("expected 1 project deployment, got %d: %+v", len(projectDeployments), projectDeployments)
	}
	if projectDeployments[0].FunctionVersionID != demoVersion.ID ||
		projectDeployments[0].Environment != "staging" ||
		projectDeployments[0].Status != "active" ||
		projectDeployments[0].Build == nil ||
		projectDeployments[0].Build.State != "succeeded" ||
		projectDeployments[0].LastJob == nil ||
		projectDeployments[0].LastJob.State != domain.JobStateSucceeded {
		t.Fatalf("unexpected project deployment payload: %+v", projectDeployments[0])
	}

	allReq := httptest.NewRequest(http.MethodGet, "/v1/deployments?status=ready&environment=production", nil)
	allReq.Header.Set("X-API-Key", "tenant-key")
	allResp := httptest.NewRecorder()
	handler.ServeHTTP(allResp, allReq)
	if allResp.Code != http.StatusOK {
		t.Fatalf("expected deployments status %d, got %d: %s", http.StatusOK, allResp.Code, allResp.Body.String())
	}
	var deployments []deploymentSummary
	if err := json.Unmarshal(allResp.Body.Bytes(), &deployments); err != nil {
		t.Fatalf("decode deployments: %v", err)
	}
	if len(deployments) != 1 {
		t.Fatalf("expected 1 tenant deployment, got %d: %+v", len(deployments), deployments)
	}
	if deployments[0].FunctionVersionID != opsVersion.ID ||
		deployments[0].ProjectID != "ops" ||
		deployments[0].Environment != "production" ||
		deployments[0].Status != "ready" ||
		deployments[0].Build == nil ||
		deployments[0].Build.State != "succeeded" ||
		deployments[0].LastJob != nil {
		t.Fatalf("unexpected tenant deployment payload: %+v", deployments[0])
	}
}

func TestDeploymentDetailAndArtifactEndpoints(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	objects := artifact.NewMemoryStore()
	builder := build.New(meta, objects)
	sched := scheduler.New(meta, []scheduler.RegionDispatcher{
		testDispatcher{region: "ap-south-1"},
	})
	mustSeedAPIKey(t, meta, "tenant-key", "tenant-dev", false, false)
	if _, err := meta.EnsureProject(context.Background(), "demo", "tenant-dev", "Demo"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	now := time.Now().UTC()
	if err := meta.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-active",
		ProjectID:      "demo",
		Name:           "active",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		MemoryMB:       128,
		TimeoutSec:     30,
		NetworkPolicy:  domain.NetworkPolicyFull,
		Regions:        []string{"ap-south-1"},
		BuildJobID:     "build-active",
		ArtifactDigest: "digest-active",
		SourceType:     domain.SourceTypeBundle,
		State:          domain.FunctionStateReady,
		CreatedAt:      now.Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("put active function version: %v", err)
	}
	if err := meta.PutBuildJob(context.Background(), &domain.BuildJob{
		ID:                "build-active",
		FunctionVersionID: "fn-active",
		TargetRegion:      "ap-south-1",
		State:             "succeeded",
		LogsKey:           "builds/build-active/logs.txt",
		CreatedAt:         now.Add(-2 * time.Minute),
		UpdatedAt:         now.Add(-90 * time.Second),
	}); err != nil {
		t.Fatalf("put active build job: %v", err)
	}
	if err := objects.Put(context.Background(), "builds/build-active/logs.txt", []byte("build active\n")); err != nil {
		t.Fatalf("put active build logs: %v", err)
	}
	if err := meta.PutExecutionJob(context.Background(), &domain.ExecutionJob{
		ID:                "job-active",
		FunctionVersionID: "fn-active",
		ProjectID:         "demo",
		TargetRegion:      "ap-south-1",
		State:             domain.JobStateSucceeded,
		MaxRetries:        1,
		AttemptCount:      1,
		LastAttemptID:     "attempt-active",
		Result: &domain.JobResult{
			ExitCode:   0,
			LogsKey:    "jobs/job-active/logs.txt",
			OutputKey:  "jobs/job-active/output.json",
			HostID:     "host-ap-south-1-a",
			Region:     "ap-south-1",
			StartedAt:  now.Add(-30 * time.Second),
			FinishedAt: now.Add(-20 * time.Second),
		},
		CreatedAt: now.Add(-30 * time.Second),
		UpdatedAt: now.Add(-20 * time.Second),
	}); err != nil {
		t.Fatalf("put active execution job: %v", err)
	}
	if err := objects.Put(context.Background(), "jobs/job-active/logs.txt", []byte("job active\n")); err != nil {
		t.Fatalf("put active execution logs: %v", err)
	}
	if err := objects.Put(context.Background(), "jobs/job-active/output.json", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("put active execution output: %v", err)
	}

	if err := meta.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-build-only",
		ProjectID:      "demo",
		Name:           "build-only",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		MemoryMB:       128,
		TimeoutSec:     30,
		NetworkPolicy:  domain.NetworkPolicyFull,
		Regions:        []string{"ap-south-1"},
		BuildJobID:     "build-only",
		ArtifactDigest: "digest-build-only",
		SourceType:     domain.SourceTypeBundle,
		State:          domain.FunctionStateReady,
		CreatedAt:      now.Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("put build-only function version: %v", err)
	}
	if err := meta.PutBuildJob(context.Background(), &domain.BuildJob{
		ID:                "build-only",
		FunctionVersionID: "fn-build-only",
		TargetRegion:      "ap-south-1",
		State:             "succeeded",
		LogsKey:           "builds/build-only/logs.txt",
		CreatedAt:         now.Add(-1 * time.Minute),
		UpdatedAt:         now.Add(-50 * time.Second),
	}); err != nil {
		t.Fatalf("put build-only build job: %v", err)
	}
	if err := objects.Put(context.Background(), "builds/build-only/logs.txt", []byte("build only\n")); err != nil {
		t.Fatalf("put build-only build logs: %v", err)
	}

	handler := New(meta, objects, builder, sched)

	detailReq := httptest.NewRequest(http.MethodGet, "/v1/deployments/fn-active", nil)
	detailReq.Header.Set("X-API-Key", "tenant-key")
	detailResp := httptest.NewRecorder()
	handler.ServeHTTP(detailResp, detailReq)
	if detailResp.Code != http.StatusOK {
		t.Fatalf("expected deployment detail status %d, got %d: %s", http.StatusOK, detailResp.Code, detailResp.Body.String())
	}
	var deployment deploymentSummary
	if err := json.Unmarshal(detailResp.Body.Bytes(), &deployment); err != nil {
		t.Fatalf("decode deployment summary: %v", err)
	}
	if deployment.ID != "fn-active" || deployment.Status != "active" || deployment.LastJob == nil {
		t.Fatalf("unexpected deployment detail payload: %+v", deployment)
	}

	logsReq := httptest.NewRequest(http.MethodGet, "/v1/deployments/fn-active/logs", nil)
	logsReq.Header.Set("X-API-Key", "tenant-key")
	logsResp := httptest.NewRecorder()
	handler.ServeHTTP(logsResp, logsReq)
	if logsResp.Code != http.StatusOK {
		t.Fatalf("expected deployment logs status %d, got %d: %s", http.StatusOK, logsResp.Code, logsResp.Body.String())
	}
	if body := logsResp.Body.String(); body != "job active\n" {
		t.Fatalf("expected execution logs to win for active deployment, got %q", body)
	}

	outputReq := httptest.NewRequest(http.MethodGet, "/v1/deployments/fn-active/output", nil)
	outputReq.Header.Set("X-API-Key", "tenant-key")
	outputResp := httptest.NewRecorder()
	handler.ServeHTTP(outputResp, outputReq)
	if outputResp.Code != http.StatusOK {
		t.Fatalf("expected deployment output status %d, got %d: %s", http.StatusOK, outputResp.Code, outputResp.Body.String())
	}
	if body := outputResp.Body.String(); body != `{"ok":true}` {
		t.Fatalf("unexpected deployment output body %q", body)
	}

	buildOnlyLogsReq := httptest.NewRequest(http.MethodGet, "/v1/deployments/fn-build-only/logs", nil)
	buildOnlyLogsReq.Header.Set("X-API-Key", "tenant-key")
	buildOnlyLogsResp := httptest.NewRecorder()
	handler.ServeHTTP(buildOnlyLogsResp, buildOnlyLogsReq)
	if buildOnlyLogsResp.Code != http.StatusOK {
		t.Fatalf("expected build-only deployment logs status %d, got %d: %s", http.StatusOK, buildOnlyLogsResp.Code, buildOnlyLogsResp.Body.String())
	}
	if body := buildOnlyLogsResp.Body.String(); body != "build only\n" {
		t.Fatalf("expected build logs fallback for build-only deployment, got %q", body)
	}

	buildOnlyOutputReq := httptest.NewRequest(http.MethodGet, "/v1/deployments/fn-build-only/output", nil)
	buildOnlyOutputReq.Header.Set("X-API-Key", "tenant-key")
	buildOnlyOutputResp := httptest.NewRecorder()
	handler.ServeHTTP(buildOnlyOutputResp, buildOnlyOutputReq)
	if buildOnlyOutputResp.Code != http.StatusConflict {
		t.Fatalf("expected build-only deployment output status %d, got %d: %s", http.StatusConflict, buildOnlyOutputResp.Code, buildOnlyOutputResp.Body.String())
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
