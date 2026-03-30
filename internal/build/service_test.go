package build

import (
	"context"
	"errors"
	"testing"

	"github.com/ishaan/eeeverc/internal/artifact"
	"github.com/ishaan/eeeverc/internal/domain"
	memstore "github.com/ishaan/eeeverc/internal/store/memory"
)

func TestCreateFunctionVersionDefaultsToAPACRegions(t *testing.T) {
	t.Parallel()

	svc := New(memstore.New(), artifact.NewMemoryStore())
	version, err := svc.CreateFunctionVersion(context.Background(), domain.DeployRequest{
		ProjectID:  "demo",
		Name:       "echo",
		Runtime:    "node22",
		Entrypoint: "index.mjs",
		Source: domain.DeploySource{
			Type: domain.SourceTypeBundle,
			InlineFiles: map[string]string{
				"index.mjs": "export async function handler() { return { ok: true }; }",
			},
		},
	})
	if err != nil {
		t.Fatalf("create function version: %v", err)
	}

	want := []string{"ap-south-1", "ap-south-2", "ap-southeast-1"}
	if len(version.Regions) != len(want) {
		t.Fatalf("expected %d regions, got %d", len(want), len(version.Regions))
	}
	for i := range want {
		if version.Regions[i] != want[i] {
			t.Fatalf("expected region[%d]=%s, got %s", i, want[i], version.Regions[i])
		}
	}
}

func TestCreateFunctionVersionRejectsUnsupportedRegion(t *testing.T) {
	t.Parallel()

	svc := New(memstore.New(), artifact.NewMemoryStore())
	_, err := svc.CreateFunctionVersion(context.Background(), domain.DeployRequest{
		ProjectID:  "demo",
		Name:       "echo",
		Runtime:    "node22",
		Entrypoint: "index.mjs",
		Regions:    []string{"us-east-1"},
		Source: domain.DeploySource{
			Type: domain.SourceTypeBundle,
			InlineFiles: map[string]string{
				"index.mjs": "export async function handler() { return { ok: true }; }",
			},
		},
	})
	if err == nil {
		t.Fatal("expected unsupported region error")
	}
}

func TestCreateFunctionVersionReplaysIdempotentDeploy(t *testing.T) {
	t.Parallel()

	svc := New(memstore.New(), artifact.NewMemoryStore())
	req := domain.DeployRequest{
		ProjectID:      "demo",
		Name:           "echo",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		IdempotencyKey: "deploy-key-1",
		Source: domain.DeploySource{
			Type: domain.SourceTypeBundle,
			InlineFiles: map[string]string{
				"index.mjs": "export async function handler() { return { ok: true }; }",
			},
		},
	}

	first, err := svc.CreateFunctionVersion(context.Background(), req)
	if err != nil {
		t.Fatalf("first create function version: %v", err)
	}
	second, err := svc.CreateFunctionVersion(context.Background(), req)
	if err != nil {
		t.Fatalf("second create function version: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected replayed version id %s, got %s", first.ID, second.ID)
	}
}

func TestCreateFunctionVersionRejectsIdempotencyKeyReuseWithDifferentPayload(t *testing.T) {
	t.Parallel()

	svc := New(memstore.New(), artifact.NewMemoryStore())
	req := domain.DeployRequest{
		ProjectID:      "demo",
		Name:           "echo",
		Runtime:        "node22",
		Entrypoint:     "index.mjs",
		IdempotencyKey: "deploy-key-2",
		Source: domain.DeploySource{
			Type: domain.SourceTypeBundle,
			InlineFiles: map[string]string{
				"index.mjs": "export async function handler() { return { ok: true }; }",
			},
		},
	}

	if _, err := svc.CreateFunctionVersion(context.Background(), req); err != nil {
		t.Fatalf("first create function version: %v", err)
	}
	req.Entrypoint = "different.mjs"
	_, err := svc.CreateFunctionVersion(context.Background(), req)
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
}
