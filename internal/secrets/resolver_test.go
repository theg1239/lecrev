package secrets

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/store"
	memstore "github.com/theg1239/lecrev/internal/store/memory"
)

func TestScopedResolverAllowsDeclaredSecrets(t *testing.T) {
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

	resolver := NewScopedResolver(meta, NewMemoryProvider(map[string]string{
		"DEMO_SECRET": "shh-local-dev",
	}))
	values, err := resolver.ResolveExecution(context.Background(), ExecutionRequest{
		HostID:            "host-ap-south-1-a",
		Region:            "ap-south-1",
		FunctionVersionID: "fn-1",
		SecretRefs:        []string{"DEMO_SECRET"},
	})
	if err != nil {
		t.Fatalf("resolve execution secrets: %v", err)
	}
	if values["DEMO_SECRET"] != "shh-local-dev" {
		t.Fatalf("unexpected secret value: %+v", values)
	}
}

func TestScopedResolverRejectsUndeclaredSecret(t *testing.T) {
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

	resolver := NewScopedResolver(meta, NewMemoryProvider(map[string]string{
		"DEMO_SECRET": "shh-local-dev",
		"OTHER":       "nope",
	}))
	_, err := resolver.ResolveExecution(context.Background(), ExecutionRequest{
		HostID:            "host-ap-south-1-a",
		Region:            "ap-south-1",
		FunctionVersionID: "fn-1",
		SecretRefs:        []string{"OTHER"},
	})
	if !errors.Is(err, store.ErrAccessDenied) {
		t.Fatalf("expected access denied, got %v", err)
	}
}
