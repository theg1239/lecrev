package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/regions"
	"github.com/theg1239/lecrev/internal/transport"
)

type Config struct {
	BaseURL      string
	APIKey       string
	ProjectID    string
	Regions      []string
	Timeout      time.Duration
	PollInterval time.Duration
	Client       *http.Client
}

type Result struct {
	Version   domain.FunctionVersion `json:"version"`
	Job       domain.ExecutionJob    `json:"job"`
	Attempts  []domain.Attempt       `json:"attempts"`
	Costs     []domain.CostRecord    `json:"costs"`
	Regions   []domain.Region        `json:"regions"`
	Hosts     []domain.Host          `json:"hosts"`
	WarmPools []domain.WarmPool      `json:"warmPools"`
}

func Run(ctx context.Context, cfg Config) (*Result, error) {
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = "http://embedded.lecrev"
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		cfg.APIKey = "dev-root-key"
	}
	if strings.TrimSpace(cfg.ProjectID) == "" {
		cfg.ProjectID = "demo"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 100 * time.Millisecond
	}
	normalizedRegions, err := regions.NormalizeExecutionRegions(cfg.Regions)
	if err != nil {
		return nil, err
	}
	cfg.Regions = normalizedRegions

	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	var regionInventory []domain.Region
	if err := doJSON(runCtx, cfg.Client, cfg.BaseURL, cfg.APIKey, http.MethodGet, "/v1/regions", nil, http.StatusOK, &regionInventory); err != nil {
		return nil, fmt.Errorf("list regions: %w", err)
	}

	deployReq := map[string]any{
		"name":           "smoke-echo",
		"runtime":        "node22",
		"entrypoint":     "index.mjs",
		"memoryMb":       128,
		"timeoutSec":     10,
		"networkPolicy":  "full",
		"regions":        cfg.Regions,
		"maxRetries":     1,
		"idempotencyKey": fmt.Sprintf("deploy-%s", uuid.NewString()),
		"source": map[string]any{
			"type": "bundle",
			"inlineFiles": map[string]string{
				"index.mjs": "export async function handler(event, context) { return { ok: true, echo: event, region: context.region, hostId: context.hostId }; }",
			},
		},
	}
	var version domain.FunctionVersion
	if err := doJSON(runCtx, cfg.Client, cfg.BaseURL, cfg.APIKey, http.MethodPost, "/v1/projects/"+cfg.ProjectID+"/functions", deployReq, http.StatusCreated, &version); err != nil {
		return nil, fmt.Errorf("deploy function: %w", err)
	}

	payload := json.RawMessage(`{"hello":"world","smoke":true}`)
	invokeReq := map[string]any{
		"payload":        payload,
		"idempotencyKey": fmt.Sprintf("invoke-%s", uuid.NewString()),
	}
	var job domain.ExecutionJob
	if err := doJSON(runCtx, cfg.Client, cfg.BaseURL, cfg.APIKey, http.MethodPost, "/v1/functions/"+version.ID+"/invoke", invokeReq, http.StatusAccepted, &job); err != nil {
		return nil, fmt.Errorf("invoke function: %w", err)
	}

	job, err = waitForJob(runCtx, cfg, job.ID)
	if err != nil {
		return nil, err
	}
	if job.State != domain.JobStateSucceeded {
		return nil, fmt.Errorf("job %s ended in state %s: %s", job.ID, job.State, job.Error)
	}
	if job.Result == nil {
		return nil, fmt.Errorf("job %s succeeded without a result", job.ID)
	}
	if err := validateOutput(job.Result.Output, payload); err != nil {
		return nil, err
	}

	var attempts []domain.Attempt
	if err := doJSON(runCtx, cfg.Client, cfg.BaseURL, cfg.APIKey, http.MethodGet, "/v1/jobs/"+job.ID+"/attempts", nil, http.StatusOK, &attempts); err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	if len(attempts) == 0 {
		return nil, fmt.Errorf("job %s returned no attempts", job.ID)
	}

	var costs []domain.CostRecord
	if err := doJSON(runCtx, cfg.Client, cfg.BaseURL, cfg.APIKey, http.MethodGet, "/v1/jobs/"+job.ID+"/costs", nil, http.StatusOK, &costs); err != nil {
		return nil, fmt.Errorf("list costs: %w", err)
	}
	if len(costs) == 0 {
		return nil, fmt.Errorf("job %s returned no cost records", job.ID)
	}

	resultRegion := job.TargetRegion
	if job.Result.Region != "" {
		resultRegion = job.Result.Region
	}
	if resultRegion == "" {
		return nil, fmt.Errorf("job %s completed without a target region", job.ID)
	}

	var hosts []domain.Host
	if err := doJSON(runCtx, cfg.Client, cfg.BaseURL, cfg.APIKey, http.MethodGet, "/v1/regions/"+resultRegion+"/hosts", nil, http.StatusOK, &hosts); err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}

	warmPools, err := waitForWarmPool(runCtx, cfg, resultRegion, version.ID)
	if err != nil {
		return nil, err
	}

	return &Result{
		Version:   version,
		Job:       job,
		Attempts:  attempts,
		Costs:     costs,
		Regions:   regionInventory,
		Hosts:     hosts,
		WarmPools: warmPools,
	}, nil
}

func waitForJob(ctx context.Context, cfg Config, jobID string) (domain.ExecutionJob, error) {
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for {
		var job domain.ExecutionJob
		if err := doJSON(ctx, cfg.Client, cfg.BaseURL, cfg.APIKey, http.MethodGet, "/v1/jobs/"+jobID, nil, http.StatusOK, &job); err != nil {
			return domain.ExecutionJob{}, fmt.Errorf("get job: %w", err)
		}
		if job.State == domain.JobStateSucceeded || job.State == domain.JobStateFailed {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return domain.ExecutionJob{}, fmt.Errorf("wait for job %s: %w", jobID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForWarmPool(ctx context.Context, cfg Config, region, functionVersionID string) ([]domain.WarmPool, error) {
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for {
		var warmPools []domain.WarmPool
		if err := doJSON(ctx, cfg.Client, cfg.BaseURL, cfg.APIKey, http.MethodGet, "/v1/regions/"+region+"/warm-pools", nil, http.StatusOK, &warmPools); err != nil {
			return nil, fmt.Errorf("list warm pools: %w", err)
		}
		for _, pool := range warmPools {
			if pool.FunctionVersionID == functionVersionID && pool.FunctionWarm > 0 {
				return warmPools, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for warm pool for %s in %s: %w", functionVersionID, region, ctx.Err())
		case <-ticker.C:
		}
	}
}

func validateOutput(output json.RawMessage, payload json.RawMessage) error {
	var body struct {
		OK     bool            `json:"ok"`
		Echo   json.RawMessage `json:"echo"`
		Region string          `json:"region"`
		HostID string          `json:"hostId"`
	}
	if err := json.Unmarshal(output, &body); err != nil {
		return fmt.Errorf("decode job output: %w", err)
	}
	if !body.OK {
		return fmt.Errorf("expected ok=true output, got %s", string(output))
	}
	if string(body.Echo) != string(payload) {
		return fmt.Errorf("expected echoed payload %s, got %s", string(payload), string(body.Echo))
	}
	if strings.TrimSpace(body.Region) == "" {
		return fmt.Errorf("expected non-empty result region")
	}
	if strings.TrimSpace(body.HostID) == "" {
		return fmt.Errorf("expected non-empty result hostId")
	}
	return nil
}

func doJSON(ctx context.Context, client *http.Client, baseURL, apiKey, method, path string, requestBody any, expectedStatus int, target any) error {
	var body io.Reader
	if requestBody != nil {
		payload, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", apiKey)
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != expectedStatus {
		return fmt.Errorf("unexpected status %d for %s %s: %s", resp.StatusCode, method, path, strings.TrimSpace(string(raw)))
	}
	if target == nil {
		return nil
	}
	return json.Unmarshal(raw, target)
}

func NewHandlerClient(handler http.Handler) *http.Client {
	return transport.NewHandlerClient(handler)
}
