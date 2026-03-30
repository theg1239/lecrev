package secrets

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/theg1239/lecrev/internal/store"
)

const ProxyPath = "/v1/internal/secrets/resolve"

type resolveExecutionResponse struct {
	Secrets map[string]string `json:"secrets"`
}

type ProxyClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewProxyClient(baseURL, token string, client *http.Client) *ProxyClient {
	if client == nil {
		client = http.DefaultClient
	}
	return &ProxyClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		client:  client,
	}
}

func (c *ProxyClient) ResolveExecution(ctx context.Context, req ExecutionRequest) (map[string]string, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+ProxyPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("secrets proxy returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var result resolveExecutionResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if result.Secrets == nil {
		return map[string]string{}, nil
	}
	return result.Secrets, nil
}

type ProxyHandler struct {
	resolver ExecutionResolver
	token    string
}

func NewProxyHandler(resolver ExecutionResolver, token string) *ProxyHandler {
	return &ProxyHandler{
		resolver: resolver,
		token:    strings.TrimSpace(token),
	}
}

func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !authorizedProxyToken(r.Header.Get("Authorization"), h.token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req ExecutionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	secrets, err := h.resolver.ResolveExecution(r.Context(), req)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrAccessDenied) {
			status = http.StatusForbidden
		} else if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resolveExecutionResponse{Secrets: secrets})
}

func authorizedProxyToken(headerValue, expectedToken string) bool {
	expectedToken = strings.TrimSpace(expectedToken)
	if expectedToken == "" {
		return false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(headerValue, prefix) {
		return false
	}
	actual := strings.TrimSpace(strings.TrimPrefix(headerValue, prefix))
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expectedToken)) == 1
}

var _ ExecutionResolver = (*ProxyClient)(nil)
