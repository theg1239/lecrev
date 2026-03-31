package build

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/theg1239/lecrev/internal/domain"
)

func TestHTTPWarmPreparerInvokesControlPlane(t *testing.T) {
	t.Parallel()

	var seenPaths []string
	var seenKey string
	statusChecks := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPaths = append(seenPaths, r.URL.Path)
		seenKey = r.Header.Get("X-API-Key")
		switch r.URL.Path {
		case "/v1/functions/fn-123/prepare":
			w.WriteHeader(http.StatusAccepted)
		case "/v1/functions/fn-123/warm-status":
			statusChecks += 1
			w.Header().Set("Content-Type", "application/json")
			payload := map[string]any{
				"functionVersionId": "fn-123",
				"ready":             statusChecks >= 2,
				"regions": []map[string]any{
					{
						"region":       "ap-south-1",
						"state":        "active",
						"functionWarm": map[bool]int{true: 1, false: 0}[statusChecks >= 2],
						"ready":        statusChecks >= 2,
					},
				},
			}
			_ = json.NewEncoder(w).Encode(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	preparer := NewHTTPWarmPreparer(server.URL, "admin-key", server.Client())
	if preparer == nil {
		t.Fatal("expected warm preparer")
	}
	preparer.pollInterval = 5 * time.Millisecond
	preparer.pollTimeout = 250 * time.Millisecond
	if err := preparer.PrepareFunctionVersion(context.Background(), &domain.FunctionVersion{ID: "fn-123"}); err != nil {
		t.Fatalf("prepare function version: %v", err)
	}
	if len(seenPaths) < 3 {
		t.Fatalf("expected prepare plus warm-status polling, got %+v", seenPaths)
	}
	if seenPaths[0] != "/v1/functions/fn-123/prepare" {
		t.Fatalf("expected first path to be prepare, got %+v", seenPaths)
	}
	if seenKey != "admin-key" {
		t.Fatalf("expected api key header, got %q", seenKey)
	}
}
