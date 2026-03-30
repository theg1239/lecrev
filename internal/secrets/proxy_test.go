package secrets

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/theg1239/lecrev/internal/domain"
	memstore "github.com/theg1239/lecrev/internal/store/memory"
	"github.com/theg1239/lecrev/internal/transport"
)

func TestProxyClientResolvesScopedSecrets(t *testing.T) {
	t.Parallel()

	meta := memstore.New()
	now := time.Now().UTC()
	if _, err := meta.EnsureProject(context.Background(), "demo", "tenant-dev", "demo"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	if err := meta.PutHost(context.Background(), &domain.Host{
		ID:             "host-ap-south-1-a",
		Region:         "ap-south-1",
		Driver:         "firecracker",
		State:          domain.HostStateActive,
		AvailableSlots: 1,
		LastHeartbeat:  now,
	}); err != nil {
		t.Fatalf("put host: %v", err)
	}
	if err := meta.PutFunctionVersion(context.Background(), &domain.FunctionVersion{
		ID:             "fn-1",
		ProjectID:      "demo",
		Name:           "echo",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		TimeoutSec:     10,
		NetworkPolicy:  domain.NetworkPolicyFull,
		Regions:        []string{"ap-south-1"},
		EnvRefs:        []string{"DEMO_SECRET"},
		ArtifactDigest: "digest",
		State:          domain.FunctionStateReady,
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("put function version: %v", err)
	}

	handler := NewProxyHandler(NewScopedResolver(meta, NewMemoryProvider(map[string]string{
		"DEMO_SECRET": "shh-local-dev",
	})), "proxy-token")
	client := NewProxyClient("http://embedded.lecrev", "proxy-token", transport.NewHandlerClient(handler))
	secrets, err := client.ResolveExecution(context.Background(), ExecutionRequest{
		HostID:            "host-ap-south-1-a",
		Region:            "ap-south-1",
		FunctionVersionID: "fn-1",
		SecretRefs:        []string{"DEMO_SECRET"},
	})
	if err != nil {
		t.Fatalf("resolve execution via proxy: %v", err)
	}
	if secrets["DEMO_SECRET"] != "shh-local-dev" {
		t.Fatalf("unexpected secret payload: %+v", secrets)
	}
}

func TestProxyClientRejectsBadToken(t *testing.T) {
	t.Parallel()

	handler := NewProxyHandler(staticResolver(map[string]string{"DEMO_SECRET": "shh"}), "proxy-token")
	client := NewProxyClient("http://embedded.lecrev", "wrong-token", transport.NewHandlerClient(handler))
	_, err := client.ResolveExecution(context.Background(), ExecutionRequest{
		HostID:            "host-ap-south-1-a",
		Region:            "ap-south-1",
		FunctionVersionID: "fn-1",
		SecretRefs:        []string{"DEMO_SECRET"},
	})
	if err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("expected 401 error, got %v", err)
	}
}

type staticResolver map[string]string

func (s staticResolver) ResolveExecution(context.Context, ExecutionRequest) (map[string]string, error) {
	return map[string]string(s), nil
}
