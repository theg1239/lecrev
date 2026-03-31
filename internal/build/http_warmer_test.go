package build

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/theg1239/lecrev/internal/domain"
)

func TestHTTPWarmPreparerInvokesControlPlane(t *testing.T) {
	t.Parallel()

	var seenPath string
	var seenKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenKey = r.Header.Get("X-API-Key")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	preparer := NewHTTPWarmPreparer(server.URL, "admin-key", server.Client())
	if preparer == nil {
		t.Fatal("expected warm preparer")
	}
	if err := preparer.PrepareFunctionVersion(context.Background(), &domain.FunctionVersion{ID: "fn-123"}); err != nil {
		t.Fatalf("prepare function version: %v", err)
	}
	if seenPath != "/v1/functions/fn-123/prepare" {
		t.Fatalf("expected prepare path, got %s", seenPath)
	}
	if seenKey != "admin-key" {
		t.Fatalf("expected api key header, got %q", seenKey)
	}
}
