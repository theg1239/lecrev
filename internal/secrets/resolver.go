package secrets

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/theg1239/lecrev/internal/store"
)

type ExecutionRequest struct {
	HostID            string   `json:"hostId"`
	Region            string   `json:"region"`
	FunctionVersionID string   `json:"functionVersionId"`
	SecretRefs        []string `json:"secretRefs"`
}

type ExecutionResolver interface {
	ResolveExecution(ctx context.Context, req ExecutionRequest) (map[string]string, error)
}

type ScopedResolver struct {
	store    store.Store
	provider Provider
}

func NewScopedResolver(store store.Store, provider Provider) *ScopedResolver {
	return &ScopedResolver{
		store:    store,
		provider: provider,
	}
}

func (r *ScopedResolver) ResolveExecution(ctx context.Context, req ExecutionRequest) (map[string]string, error) {
	if strings.TrimSpace(req.HostID) == "" {
		return nil, fmt.Errorf("hostId is required")
	}
	if strings.TrimSpace(req.Region) == "" {
		return nil, fmt.Errorf("region is required")
	}
	if strings.TrimSpace(req.FunctionVersionID) == "" {
		return nil, fmt.Errorf("functionVersionId is required")
	}

	refs := normalizeSecretRefs(req.SecretRefs)
	if len(refs) == 0 {
		return map[string]string{}, nil
	}

	host, err := r.store.GetHost(ctx, req.HostID)
	if err != nil {
		return nil, err
	}
	if host.Region != req.Region {
		return nil, store.ErrAccessDenied
	}

	version, err := r.store.GetFunctionVersion(ctx, req.FunctionVersionID)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(version.Regions, req.Region) {
		return nil, store.ErrAccessDenied
	}
	for _, ref := range refs {
		if !slices.Contains(version.EnvRefs, ref) {
			return nil, store.ErrAccessDenied
		}
	}

	return r.provider.Resolve(ctx, refs)
}

func normalizeSecretRefs(refs []string) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		trimmed := strings.TrimSpace(ref)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}
