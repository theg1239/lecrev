package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/theg1239/lecrev/internal/domain"
)

type HTTPWarmPreparer struct {
	baseURL      string
	apiKey       string
	client       *http.Client
	pollInterval time.Duration
	pollTimeout  time.Duration
}

func NewHTTPWarmPreparer(baseURL, apiKey string, client *http.Client) *HTTPWarmPreparer {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	apiKey = strings.TrimSpace(apiKey)
	if baseURL == "" || apiKey == "" {
		return nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPWarmPreparer{
		baseURL:      baseURL,
		apiKey:       apiKey,
		client:       client,
		pollInterval: 250 * time.Millisecond,
		pollTimeout:  8 * time.Second,
	}
}

func (p *HTTPWarmPreparer) PrepareFunctionVersion(ctx context.Context, version *domain.FunctionVersion) error {
	if p == nil {
		return nil
	}
	if version == nil || strings.TrimSpace(version.ID) == "" {
		return fmt.Errorf("function version is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/functions/"+version.ID+"/prepare", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", p.apiKey)
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if len(body) == 0 {
			return fmt.Errorf("prepare function warm returned status %d", resp.StatusCode)
		}
		return fmt.Errorf("prepare function warm returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return p.waitForWarmReady(ctx, version.ID)
}

type warmStatusResponse struct {
	FunctionVersionID string                 `json:"functionVersionId"`
	Ready             bool                   `json:"ready"`
	Regions           []warmRegionStatusItem `json:"regions"`
}

type warmRegionStatusItem struct {
	Region       string `json:"region"`
	State        string `json:"state"`
	FunctionWarm int    `json:"functionWarm"`
	Ready        bool   `json:"ready"`
}

func (p *HTTPWarmPreparer) waitForWarmReady(ctx context.Context, versionID string) error {
	if p == nil {
		return nil
	}
	timeout := p.pollTimeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	interval := p.pollInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastStatus *warmStatusResponse
	for {
		status, err := p.fetchWarmStatus(waitCtx, versionID)
		if err != nil {
			return err
		}
		lastStatus = status
		if status.Ready {
			return nil
		}

		activeRegions := 0
		for _, region := range status.Regions {
			if region.State == "active" {
				activeRegions += 1
			}
		}
		if activeRegions == 0 {
			return fmt.Errorf("no active target regions available for warm preparation")
		}

		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return warmWaitTimeoutError(lastStatus)
		case <-timer.C:
		}
	}
}

func (p *HTTPWarmPreparer) fetchWarmStatus(ctx context.Context, versionID string) (*warmStatusResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/v1/functions/"+versionID+"/warm-status", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", p.apiKey)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if len(body) == 0 {
			return nil, fmt.Errorf("warm status returned status %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("warm status returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var status warmStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decode warm status: %w", err)
	}
	return &status, nil
}

func warmWaitTimeoutError(status *warmStatusResponse) error {
	if status == nil || len(status.Regions) == 0 {
		return fmt.Errorf("warm preparation did not become ready before timeout")
	}
	parts := make([]string, 0, len(status.Regions))
	for _, region := range status.Regions {
		parts = append(parts, fmt.Sprintf("%s=%s(functionWarm=%d)", region.Region, region.State, region.FunctionWarm))
	}
	return fmt.Errorf("warm preparation did not become ready before timeout (%s)", strings.Join(parts, ", "))
}
