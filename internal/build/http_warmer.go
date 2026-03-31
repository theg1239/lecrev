package build

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/theg1239/lecrev/internal/domain"
)

type HTTPWarmPreparer struct {
	baseURL string
	apiKey  string
	client  *http.Client
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
		baseURL: baseURL,
		apiKey:  apiKey,
		client:  client,
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
	return nil
}
